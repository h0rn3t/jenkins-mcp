// Package analysis turns the raw results of many Jenkins builds into one
// summary: how much failed, how old the failures are, what kind they are,
// which services they name, and which suites failed together.
//
// It answers the question a single build cannot — whether a night is worse
// than the one before it, and whether the difference is new breakage or the
// same failures still unfixed.
package analysis

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
)

// A Controller is the part of the Jenkins API a summary reads.
// [jenkins.Client] satisfies it.
type Controller interface {
	Jobs(ctx context.Context, folder string) ([]jenkins.Job, error)
	Builds(ctx context.Context, job string, limit int) ([]jenkins.Build, error)
	TestReport(ctx context.Context, job string, number, maxFailures int) (jenkins.TestReport, error)
}

// Defaults and bounds. The controller is shared with everyone else using it,
// so the fan-out stays deliberately small.
const (
	defaultSince    = 24 * time.Hour
	defaultDepth    = 10
	maxDepth        = 50
	defaultCluster  = 3
	maxFailuresRead = 200
	maxClusters     = 20
	sampleLen       = 200
	workers         = 6
)

// Options bound a summary. The zero value summarises every job over the last
// 24 hours.
type Options struct {
	// JobFilter keeps only jobs whose name contains it, case-insensitively.
	// Empty means every job.
	JobFilter string

	// Since is how far back the window reaches; optional, default 24h.
	Since time.Duration

	// MinCluster is how many fresh failures a suite needs to count as a
	// cluster; optional, default 3.
	MinCluster int

	// Depth is how many builds per job to look at before filtering by the
	// window; optional, default 10, capped at 50.
	Depth int

	// Now is the end of the window; optional, default time.Now().
	Now time.Time
}

// A Summary is one window of Jenkins history.
type Summary struct {
	Window   Window     `json:"window"`
	Totals   Totals     `json:"totals"`
	Age      Age        `json:"age"`
	Nature   []Count    `json:"nature"`
	Services []Evidence `json:"services,omitempty"`
	Clusters []Cluster  `json:"clusters,omitempty"`
	Runs     []Run      `json:"runs"`

	// NoTestReport lists builds that published none — a broken publish step,
	// not failing tests.
	NoTestReport []string `json:"noTestReport,omitempty"`

	// Errors lists jobs that could not be read at all. A summary with entries
	// here is partial.
	Errors []string `json:"errors,omitempty"`
}

// A Window is the span the summary covers.
type Window struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Jobs   int       `json:"jobs"`
	Builds int       `json:"builds"`
}

// Totals are the test counts across every run in the window.
type Totals struct {
	Tests   int `json:"tests"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// Age splits failures by how many builds in a row they have been failing.
// Debt counts age 20 and over and is a subset of Standing.
type Age struct {
	Fresh    int `json:"fresh"`
	Recent   int `json:"recent"`
	Standing int `json:"standing"`
	Debt     int `json:"debt"`
}

// A Count is how many fresh failures share one kind.
type Count struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// An Evidence is a service or database table named outright in the text of a
// fresh failure, which is what makes it level A rather than a guess.
type Evidence struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Fresh int    `json:"fresh"`
	Level string `json:"level"`
}

// A Cluster is one suite with several fresh failures — the shape a real
// regression takes, as opposed to scattered single failures.
type Cluster struct {
	Suite       string   `json:"suite"`
	Job         string   `json:"job"`
	Fresh       int      `json:"fresh"`
	Nature      string   `json:"nature"`
	Services    []string `json:"services,omitempty"`
	FailedSince int      `json:"failedSince,omitempty"`
	Sample      string   `json:"sample,omitempty"`
}

// A Run is one build inside the window. Date is its local calendar date,
// which is what groups a night that crosses midnight UTC.
type Run struct {
	Job       string    `json:"job"`
	Build     int       `json:"build"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	Date      string    `json:"date"`
	Tests     int       `json:"tests"`
	Failed    int       `json:"failed"`
	Fresh     int       `json:"fresh"`
}

// Summarize reads every job matching opts.JobFilter and aggregates the builds
// that started inside the window. A job whose build published no test report
// is listed in NoTestReport; a job that could not be read is listed in Errors,
// and only when no job at all produced a run does Summarize report an error.
func Summarize(ctx context.Context, c Controller, opts Options) (Summary, error) {
	opts.Since = cmp.Or(opts.Since, defaultSince)
	opts.MinCluster = cmp.Or(opts.MinCluster, defaultCluster)
	opts.Depth = min(cmp.Or(opts.Depth, defaultDepth), maxDepth)
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	window := Window{From: opts.Now.Add(-opts.Since), To: opts.Now}

	jobs, err := selectJobs(ctx, c, opts.JobFilter)
	if err != nil {
		return Summary{}, err
	}
	window.Jobs = len(jobs)

	// One worker per job, bounded: the controller serves the whole team.
	var (
		mu    sync.Mutex
		total = tally{suites: map[string]*Cluster{}}
		wg    sync.WaitGroup
		sem   = make(chan struct{}, workers)
	)
	for _, job := range jobs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			got, gone, err := collect(ctx, c, job, window, opts.Depth)

			mu.Lock()
			defer mu.Unlock()
			total.missing = append(total.missing, gone...)
			if err != nil {
				total.failed = append(total.failed, fmt.Sprintf("%s: %v", job, err))
				return
			}
			for _, r := range got {
				total.add(r)
			}
		})
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if len(total.runs) == 0 {
		if len(total.failed) > 0 {
			return Summary{}, fmt.Errorf("no job could be read: %s", strings.Join(total.failed, "; "))
		}
		if len(total.missing) == 0 {
			return Summary{}, fmt.Errorf("no build of %d job(s) started between %s and %s",
				window.Jobs, window.From.Format(time.RFC3339), window.To.Format(time.RFC3339))
		}
	}

	return total.summary(window, opts.MinCluster), nil
}

// selectJobs lists the controller's jobs and keeps the ones whose name carries
// filter. Folders hold no builds, so they never survive.
func selectJobs(ctx context.Context, c Controller, filter string) ([]string, error) {
	all, err := c.Jobs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	filter = strings.ToLower(filter)
	jobs := make([]string, 0, len(all))
	for _, job := range all {
		name := cmp.Or(job.FullName, job.Name)
		if !job.Folder && strings.Contains(strings.ToLower(name), filter) {
			jobs = append(jobs, name)
		}
	}
	return jobs, nil
}

// summary assembles what was tallied, newest run first and biggest cluster
// first.
func (t *tally) summary(window Window, minCluster int) Summary {
	window.Builds = len(t.runs)
	slices.SortFunc(t.runs, func(a, b Run) int {
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(a.Job, b.Job))
	})
	summary := Summary{
		Window: window, Totals: t.totals, Age: t.age, Runs: t.runs,
		Nature: natures(t.fresh), Services: services(t.fresh),
		NoTestReport: t.missing, Errors: t.failed,
	}
	for _, cluster := range t.suites {
		if cluster.Fresh >= minCluster {
			summary.Clusters = append(summary.Clusters, *cluster)
		}
	}
	slices.SortFunc(summary.Clusters, func(a, b Cluster) int {
		return cmp.Or(cmp.Compare(b.Fresh, a.Fresh), cmp.Compare(a.Suite, b.Suite))
	})
	summary.Clusters = summary.Clusters[:min(len(summary.Clusters), maxClusters)]
	return summary
}

// A tally accumulates what one summary reports. Callers serialise access to
// it; it does no locking of its own.
type tally struct {
	runs    []Run
	totals  Totals
	age     Age
	fresh   []jenkins.TestCase
	suites  map[string]*Cluster
	missing []string
	failed  []string
}

// add folds one build's report into the tally.
func (t *tally) add(r result) {
	t.runs = append(t.runs, r.run)
	t.totals.Tests += r.report.Total
	t.totals.Passed += r.report.Passed
	t.totals.Failed += r.report.Failed
	t.totals.Skipped += r.report.Skipped
	for _, tc := range r.report.Failures {
		switch {
		case tc.Age <= 1:
			t.age.Fresh++
		case tc.Age <= 5:
			t.age.Recent++
		default:
			t.age.Standing++
			if tc.Age >= 20 {
				t.age.Debt++
			}
		}
		if tc.Age != 1 {
			continue
		}
		t.fresh = append(t.fresh, tc)
		cluster, ok := t.suites[tc.Suite]
		if !ok {
			cluster = &Cluster{
				Suite: tc.Suite, Job: r.run.Job, Nature: natureOf(tc.Error),
				FailedSince: tc.FailedSince, Sample: sample(tc.Error),
			}
			t.suites[tc.Suite] = cluster
		}
		cluster.Fresh++
		cluster.Services = union(cluster.Services, namesIn(tc.Error))
	}
}

// A result pairs a run with the report it was built from, so the caller can
// read the failures without asking again.
type result struct {
	run    Run
	report jenkins.TestReport
}

// collect reads one job's builds inside the window. It returns the builds that
// reported tests and, separately, those that published no report at all.
func collect(ctx context.Context, c Controller, job string, window Window, depth int) ([]result, []string, error) {
	builds, err := c.Builds(ctx, job, depth)
	if err != nil {
		return nil, nil, fmt.Errorf("list builds: %w", err)
	}
	var (
		results []result
		missing []string
	)
	for _, b := range builds {
		if b.StartedAt.Before(window.From) || b.StartedAt.After(window.To) {
			continue
		}
		report, err := c.TestReport(ctx, job, b.Number, maxFailuresRead)
		if errors.Is(err, jenkins.ErrNotFound) {
			missing = append(missing, fmt.Sprintf("%s#%d", job, b.Number))
			continue
		}
		if err != nil {
			return nil, missing, fmt.Errorf("build %d: %w", b.Number, err)
		}
		run := Run{
			Job: job, Build: b.Number, Status: b.Status, StartedAt: b.StartedAt,
			Date: b.StartedAt.Local().Format(time.DateOnly), Tests: report.Total, Failed: report.Failed,
		}
		for _, tc := range report.Failures {
			if tc.Age == 1 {
				run.Fresh++
			}
		}
		results = append(results, result{run: run, report: report})
	}
	return results, missing, nil
}

// natureOf classifies a failure by how its error text begins. The prefixes are
// the ones qa-test-ui actually produces; anything else stays "other" rather
// than being forced into a bucket.
func natureOf(text string) string {
	switch {
	case strings.TrimSpace(text) == "":
		return "unknown"
	case strings.HasPrefix(text, "Locator:"):
		return "ui-locator"
	case strings.HasPrefix(text, "locator.") && strings.Contains(text, "Timeout"):
		return "ui-timeout"
	case strings.HasPrefix(text, "UI API response"):
		return "api-endpoint"
	case strings.HasPrefix(text, "Response Body"):
		return "api-body"
	case strings.HasPrefix(text, "SQL:"):
		return "db"
	case strings.Contains(text, "not found in current browser session"):
		return "file-export"
	case strings.Contains(text, "function timed out"):
		return "timeout"
	}
	return "other"
}

// natures counts the kinds of the fresh failures, commonest first.
func natures(fresh []jenkins.TestCase) []Count {
	seen := map[string]int{}
	for _, tc := range fresh {
		seen[natureOf(tc.Error)]++
	}
	counts := make([]Count, 0, len(seen))
	for kind, n := range seen {
		counts = append(counts, Count{Kind: kind, Count: n})
	}
	slices.SortFunc(counts, func(a, b Count) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Kind, b.Kind))
	})
	return counts
}

var (
	servicePattern = regexp.MustCompile(`ms-[a-z0-9]+(?:-[a-z0-9]+)*`)
	tablePattern   = regexp.MustCompile(`\bms_[a-z0-9]+(?:_[a-z0-9]+)+\b`)
)

// namesIn returns the services and tables the error text names outright.
func namesIn(text string) []string {
	return append(servicePattern.FindAllString(text, -1), tablePattern.FindAllString(text, -1)...)
}

// services counts, over the fresh failures only, every service and table named
// in an error. Being named in the text is what level A means.
func services(fresh []jenkins.TestCase) []Evidence {
	seen := map[string]int{}
	for _, tc := range fresh {
		for _, name := range unique(namesIn(tc.Error)) {
			seen[name]++
		}
	}
	found := make([]Evidence, 0, len(seen))
	for name, n := range seen {
		kind := "service"
		if strings.Contains(name, "_") {
			kind = "table"
		}
		found = append(found, Evidence{Name: name, Kind: kind, Fresh: n, Level: "A"})
	}
	slices.SortFunc(found, func(a, b Evidence) int {
		return cmp.Or(cmp.Compare(b.Fresh, a.Fresh), cmp.Compare(a.Name, b.Name))
	})
	return found
}

// sample is the opening of an error, enough to recognise it in a report.
func sample(text string) string {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' })
	joined := strings.Join(lines, " | ")
	if runes := []rune(joined); len(runes) > sampleLen {
		return string(runes[:sampleLen]) + "…"
	}
	return joined
}

// union appends the names of b that a does not already carry.
func union(a, b []string) []string {
	for _, name := range b {
		if !slices.Contains(a, name) {
			a = append(a, name)
		}
	}
	return a
}

// unique drops repeats so one error naming a service twice counts once.
func unique(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}
