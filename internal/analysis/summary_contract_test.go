package analysis_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eugeneshershen/jenkins-mcp/internal/analysis"
	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
)

// now is the fixed clock every case measures its window against.
var now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// fake is a Controller backed by fixtures. It is called from several
// goroutines at once, so every field it records is guarded.
type fake struct {
	jobs    []jenkins.Job
	builds  map[string][]jenkins.Build
	reports map[string]jenkins.TestReport
	errs    map[string]error

	mu    sync.Mutex
	calls int
}

func (f *fake) Jobs(context.Context, string) ([]jenkins.Job, error) { return f.jobs, nil }

func (f *fake) Builds(_ context.Context, job string, _ int) ([]jenkins.Build, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.builds[job], nil
}

func (f *fake) TestReport(_ context.Context, job string, number, _ int) (jenkins.TestReport, error) {
	key := fmt.Sprintf("%s#%d", job, number)
	if err, ok := f.errs[key]; ok {
		return jenkins.TestReport{}, err
	}
	return f.reports[key], nil
}

// build is a run started hoursAgo before the fixed clock.
func build(number int, hoursAgo float64) jenkins.Build {
	return jenkins.Build{
		Number:    number,
		Status:    "unstable",
		StartedAt: now.Add(-time.Duration(hoursAgo * float64(time.Hour))),
	}
}

func failure(suite, name string, age int, errText string) jenkins.TestCase {
	return jenkins.TestCase{Suite: suite, Name: name, Age: age, Status: "failed", Error: errText, FailedSince: 80}
}

func TestSummarizeWindowAndTotals(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs: []jenkins.Job{{Name: "DATAHUB_A"}, {Name: "DATAHUB_B"}, {Name: "OTHER_C"}},
		builds: map[string][]jenkins.Build{
			"DATAHUB_A": {build(10, 2), build(9, 26), build(8, 200)},
			"DATAHUB_B": {build(5, 3)},
			"OTHER_C":   {build(1, 1)},
		},
		reports: map[string]jenkins.TestReport{
			"DATAHUB_A#10": {Total: 100, Failed: 4, Passed: 94, Skipped: 2},
			"DATAHUB_A#9":  {Total: 100, Failed: 7, Passed: 93},
			"DATAHUB_B#5":  {Total: 50, Failed: 1, Passed: 49},
			"OTHER_C#1":    {Total: 999, Failed: 999},
		},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{
		JobFilter: "datahub", Since: 48 * time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if got.Window.Jobs != 2 {
		t.Errorf("Summarize() matched %d jobs, want 2 (OTHER_C filtered out, match is case-insensitive)", got.Window.Jobs)
	}
	if len(got.Runs) != 3 {
		t.Fatalf("Summarize() returned %d runs, want 3 (the 200h-old build is outside the window)", len(got.Runs))
	}
	want := analysis.Totals{Tests: 250, Passed: 236, Failed: 12, Skipped: 2}
	if got.Totals != want {
		t.Errorf("Summarize().Totals = %+v, want %+v", got.Totals, want)
	}
	if !got.Window.From.Equal(now.Add(-48*time.Hour)) || !got.Window.To.Equal(now) {
		t.Errorf("Summarize().Window = %v..%v, want the 48h window before the clock", got.Window.From, got.Window.To)
	}
}

func TestSummarizeAgeBuckets(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:   []jenkins.Job{{Name: "J"}},
		builds: map[string][]jenkins.Build{"J": {build(1, 1)}},
		reports: map[string]jenkins.TestReport{"J#1": {Total: 10, Failed: 5, Failures: []jenkins.TestCase{
			failure("S", "a", 1, "Locator:"),
			failure("S", "b", 2, "Locator:"),
			failure("S", "c", 5, "Locator:"),
			failure("S", "d", 6, "Locator:"),
			failure("S", "e", 44, "Locator:"),
		}}},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: time.Hour * 24, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	want := analysis.Age{Fresh: 1, Recent: 2, Standing: 2, Debt: 1}
	if got.Age != want {
		t.Errorf("Summarize().Age = %+v, want %+v (Debt counts age>=20 and is a subset of Standing)", got.Age, want)
	}
}

func TestSummarizeNature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  string
		want string
	}{
		{"locator", "Locator:\nlocator('//*[@x]')\nexpect failed", "ui-locator"},
		{"click timeout", "locator.click: Timeout 20000ms exceeded.", "ui-timeout"},
		{"named endpoint", `UI API response GET https://test/ms-ts/api/v1/x: параметр "data"`, "api-endpoint"},
		{"response body", `Response Body не содержит "BA02": {}`, "api-body"},
		{"response body no spaces", `Response Body (без пробелов) не содержит "<Point>"`, "api-body"},
		{"sql", `SQL: ожидалось "101", получено "104"`, "db"},
		{"file export", "File Експорт.xlsx not found in current browser session", "file-export"},
		{"scenario timeout", "function timed out, ensure the promise resolves within 600000", "timeout"},
		{"empty", "", "unknown"},
		{"anything else", "boom", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fake{
				jobs:   []jenkins.Job{{Name: "J"}},
				builds: map[string][]jenkins.Build{"J": {build(1, 1)}},
				reports: map[string]jenkins.TestReport{"J#1": {Total: 1, Failed: 1, Failures: []jenkins.TestCase{
					failure("S", "t", 1, tt.err),
				}}},
			}
			got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
			if err != nil {
				t.Fatalf("Summarize() error = %v, want nil", err)
			}
			if len(got.Nature) != 1 || got.Nature[0].Kind != tt.want {
				t.Errorf("Summarize().Nature for %q = %+v, want one entry %q", tt.err, got.Nature, tt.want)
			}
		})
	}
}

func TestSummarizeServices(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:   []jenkins.Job{{Name: "J"}},
		builds: map[string][]jenkins.Build{"J": {build(1, 1)}},
		reports: map[string]jenkins.TestReport{"J#1": {Total: 9, Failed: 4, Failures: []jenkins.TestCase{
			failure("S1", "a", 1, "UI API response GET https://test/ms-ts/api/v1/ts-certifications: fail"),
			failure("S2", "b", 1, "UI API response GET https://test/ms-ts/api/v1/other: fail"),
			failure("S3", "c", 1, `SQL: ожидалось "1" | Запрос: SELECT x FROM ms_ap_history_atomic WHERE id=1`),
			failure("S4", "d", 30, "UI API response GET https://test/ms-old/api: fail"),
		}}},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	byName := map[string]analysis.Evidence{}
	for _, e := range got.Services {
		byName[e.Name] = e
	}
	if e := byName["ms-ts"]; e.Fresh != 2 || e.Kind != "service" || e.Level != "A" {
		t.Errorf("Summarize().Services[ms-ts] = %+v, want 2 fresh, kind service, level A", e)
	}
	if e, ok := byName["ms_ap_history_atomic"]; !ok || e.Kind != "table" {
		t.Errorf("Summarize().Services has no table entry for the SQL identifier, got %+v", got.Services)
	}
	if _, ok := byName["ms-old"]; ok {
		t.Errorf("Summarize().Services counted %q from an age-30 failure, want fresh failures only", "ms-old")
	}
}

func TestSummarizeClusters(t *testing.T) {
	t.Parallel()
	cases := make([]jenkins.TestCase, 0, 12)
	for i := range 4 {
		cases = append(cases, failure("Велика фіча", fmt.Sprintf("t%d", i), 1, "Locator:\nlocator('//x')"))
	}
	for i := range 3 {
		cases = append(cases, failure("Середня фіча", fmt.Sprintf("m%d", i), 1, `SQL: ожидалось "1"`))
	}
	cases = append(cases,
		failure("Мала фіча", "s1", 1, "Locator:"),
		failure("Мала фіча", "s2", 1, "Locator:"),
		failure("Стара фіча", "o1", 40, "Locator:"),
		failure("Стара фіча", "o2", 40, "Locator:"),
		failure("Стара фіча", "o3", 40, "Locator:"),
	)
	f := &fake{
		jobs:    []jenkins.Job{{Name: "J"}},
		builds:  map[string][]jenkins.Build{"J": {build(1, 1)}},
		reports: map[string]jenkins.TestReport{"J#1": {Total: 100, Failed: len(cases), Failures: cases}},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("Summarize() found %d clusters, want 2 (below 3 fresh is not a cluster, and age 40 is not fresh): %+v", len(got.Clusters), got.Clusters)
	}
	if got.Clusters[0].Suite != "Велика фіча" || got.Clusters[0].Fresh != 4 {
		t.Errorf("Summarize().Clusters[0] = %+v, want the 4-failure suite first", got.Clusters[0])
	}
	if got.Clusters[1].Nature != "db" {
		t.Errorf("Summarize().Clusters[1].Nature = %q, want %q", got.Clusters[1].Nature, "db")
	}
	if got.Clusters[0].Job != "J" || got.Clusters[0].Sample == "" {
		t.Errorf("Summarize().Clusters[0] = %+v, want the job and a sample error", got.Clusters[0])
	}
}

func TestSummarizeMissingTestReport(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:    []jenkins.Job{{Name: "J"}, {Name: "K"}},
		builds:  map[string][]jenkins.Build{"J": {build(1, 1)}, "K": {build(2, 1)}},
		reports: map[string]jenkins.TestReport{"K#2": {Total: 5, Failed: 1}},
		errs:    map[string]error{"J#1": fmt.Errorf("get: %w", jenkins.ErrNotFound)},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil: a build without a test report is not a failure", err)
	}
	if !slices.Contains(got.NoTestReport, "J#1") {
		t.Errorf("Summarize().NoTestReport = %v, want it to name J#1", got.NoTestReport)
	}
	if len(got.Runs) != 1 || got.Totals.Tests != 5 {
		t.Errorf("Summarize() = %d runs / %d tests, want the one job that did report", len(got.Runs), got.Totals.Tests)
	}
	if len(got.Errors) != 0 {
		t.Errorf("Summarize().Errors = %v, want none for a missing test report", got.Errors)
	}
}

func TestSummarizePartialFailure(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:    []jenkins.Job{{Name: "J"}, {Name: "K"}},
		builds:  map[string][]jenkins.Build{"J": {build(1, 1)}, "K": {build(2, 1)}},
		reports: map[string]jenkins.TestReport{"K#2": {Total: 5, Failed: 1}},
		errs:    map[string]error{"J#1": jenkins.ErrUnauthorized},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want a partial summary while another job succeeded", err)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "J") {
		t.Errorf("Summarize().Errors = %v, want one entry naming J", got.Errors)
	}
	if got.Totals.Tests != 5 {
		t.Errorf("Summarize().Totals.Tests = %d, want the surviving job's 5", got.Totals.Tests)
	}
}

func TestSummarizeEverythingFailed(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:   []jenkins.Job{{Name: "J"}},
		builds: map[string][]jenkins.Build{"J": {build(1, 1)}},
		errs:   map[string]error{"J#1": jenkins.ErrUnauthorized},
	}
	if _, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now}); err == nil {
		t.Errorf("Summarize() error = nil, want an error when no job produced a run")
	}
}

func TestSummarizeDefaults(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:    []jenkins.Job{{Name: "J"}},
		builds:  map[string][]jenkins.Build{"J": {build(1, 1), build(0, 30)}},
		reports: map[string]jenkins.TestReport{"J#1": {Total: 1}},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if d := got.Window.To.Sub(got.Window.From); d != 24*time.Hour {
		t.Errorf("Summarize() with no Since used a %v window, want the 24h default", d)
	}
	if len(got.Runs) != 1 {
		t.Errorf("Summarize() returned %d runs, want the 30h-old build excluded by the default window", len(got.Runs))
	}
}

func TestSummarizeRunsCarryLocalDate(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:    []jenkins.Job{{Name: "J"}},
		builds:  map[string][]jenkins.Build{"J": {build(1, 1)}},
		reports: map[string]jenkins.TestReport{"J#1": {Total: 3, Failed: 1, Failures: []jenkins.TestCase{failure("S", "a", 1, "Locator:")}}},
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	run := got.Runs[0]
	if want := run.StartedAt.Local().Format(time.DateOnly); run.Date != want {
		t.Errorf("Summarize().Runs[0].Date = %q, want the local calendar date %q", run.Date, want)
	}
	if run.Fresh != 1 {
		t.Errorf("Summarize().Runs[0].Fresh = %d, want 1", run.Fresh)
	}
}

func TestSummarizeBoundsConcurrency(t *testing.T) {
	t.Parallel()
	f := &fake{builds: map[string][]jenkins.Build{}, reports: map[string]jenkins.TestReport{}}
	for i := range 40 {
		job := fmt.Sprintf("J%02d", i)
		f.jobs = append(f.jobs, jenkins.Job{Name: job})
		f.builds[job] = []jenkins.Build{build(1, 1)}
		f.reports[job+"#1"] = jenkins.TestReport{Total: 2, Failed: 1, Failures: []jenkins.TestCase{failure("S", "a", 1, "Locator:")}}
	}
	got, err := analysis.Summarize(t.Context(), f, analysis.Options{Since: 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatalf("Summarize() error = %v, want nil", err)
	}
	if len(got.Runs) != 40 || got.Totals.Failed != 40 {
		t.Errorf("Summarize() over 40 jobs = %d runs / %d failures, want 40 and 40", len(got.Runs), got.Totals.Failed)
	}
	if f.calls != 40 {
		t.Errorf("Summarize() made %d Builds calls, want one per job", f.calls)
	}
}

func TestSummarizeCancelledContext(t *testing.T) {
	t.Parallel()
	f := &fake{
		jobs:   []jenkins.Job{{Name: "J"}},
		builds: map[string][]jenkins.Build{"J": {build(1, 1)}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := analysis.Summarize(ctx, f, analysis.Options{Since: 24 * time.Hour, Now: now}); err == nil {
		t.Errorf("Summarize() with a cancelled context error = nil, want non-nil")
	}
}
