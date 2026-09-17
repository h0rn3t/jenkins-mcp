// Package jenkins reads jobs, builds and console output from a Jenkins
// controller through its remote API, and triggers or stops builds.
//
// Every job argument is a slash-separated path of job names as they appear in
// a Jenkins URL — "nightly" at the top level, "team/nightly" inside a folder —
// never a raw URL path. Client rejects a path that could escape the
// controller's job tree.
package jenkins

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"encoding/json/v2"
)

// Errors reported by Client. Match them with errors.Is.
var (
	// ErrInvalidJob reports a job path that is empty or contains a segment
	// that could leave the controller's job tree.
	ErrInvalidJob = errors.New("invalid job path")
	// ErrNotFound reports a job, build or log that the controller does not have.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized reports credentials the controller rejected or that lack
	// permission for the request.
	ErrUnauthorized = errors.New("unauthorized")
)

// Response and window limits. Console windows are bounded so a log cannot
// exhaust the context of the model reading it.
const (
	defaultBuilds = 10
	maxBuilds     = 100

	defaultConsoleBytes = 64 << 10
	maxConsoleBytes     = 1 << 20
	maxJSONBytes        = 4 << 20

	maxTestFailures = 200
	maxTestError    = 500
)

// A Config describes the controller to talk to.
type Config struct {
	// BaseURL is the controller's root URL, http or https, with no credentials
	// in it. A path prefix ("https://ci.example.com/jenkins") is preserved.
	BaseURL string

	// Authentication, sent as HTTP Basic credentials:
	User string // optional; empty with Password for anonymous access
	// Password is the account's API token or its password — Jenkins accepts
	// either in this position. Prefer a token: it is revocable on its own and
	// does not grant a web session.
	Password string

	// HTTPClient is used for every request; optional, default: a client with a
	// 30 second timeout.
	HTTPClient *http.Client
}

// A Client calls the Jenkins remote API. It is safe for concurrent use.
type Client struct {
	base     *url.URL
	user     string
	password string
	http     *http.Client
}

// A Job is a Jenkins job or folder. Fields beyond Name are populated only when
// the controller reports them; [Client.Jobs] fills fewer of them than
// [Client.Job].
type Job struct {
	Name        string      `json:"name"`
	FullName    string      `json:"fullName,omitempty"`
	URL         string      `json:"url,omitempty"`
	Status      string      `json:"status"`
	Building    bool        `json:"building"`
	Folder      bool        `json:"folder,omitempty"`
	Buildable   bool        `json:"buildable,omitempty"`
	InQueue     bool        `json:"inQueue,omitempty"`
	Description string      `json:"description,omitempty"`
	LastBuild   *Build      `json:"lastBuild,omitempty"`
	Parameters  []Parameter `json:"parameters,omitempty"`
}

// A Parameter is a build parameter a job accepts.
type Parameter struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// A Build is one run of a job. Duration is a human-readable span such as
// "3m21s" and is empty while the build is running.
type Build struct {
	Number      int       `json:"number"`
	Status      string    `json:"status"`
	Building    bool      `json:"building"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	Duration    string    `json:"duration,omitempty"`
	DisplayName string    `json:"displayName,omitempty"`
	URL         string    `json:"url,omitempty"`
}

// A Console is one window of a build's console log. End is the offset to pass
// as the next start; Truncated reports that output past End is already
// available, and Running that the build is still writing.
type Console struct {
	Text      string `json:"text"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Running   bool   `json:"running"`
}

// A TestReport summarises a build's JUnit results. The counts cover the whole
// build; Failures is the capped sample of failing cases the caller asked for.
type TestReport struct {
	Total    int        `json:"total"`
	Passed   int        `json:"passed"`
	Failed   int        `json:"failed"`
	Skipped  int        `json:"skipped"`
	Failures []TestCase `json:"failures,omitempty"`
}

// A TestCase is one failing test. Age is how many builds in a row it has been
// failing, so age 1 is new in this build and a large age is a standing
// failure; FailedSince is the build the run of failures started in.
type TestCase struct {
	Suite       string `json:"suite,omitempty"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Age         int    `json:"age"`
	FailedSince int    `json:"failedSince,omitempty"`
	Error       string `json:"error,omitempty"`
}

// A Queued is the queue item a triggered build waits in. The build number is
// assigned only once an executor picks the item up.
type Queued struct {
	Job      string `json:"job"`
	QueueID  int    `json:"queueId,omitempty"`
	QueueURL string `json:"queueUrl,omitempty"`
}

// New returns a Client for the controller described by cfg. It reports an
// error for a base URL that is missing, is not http or https, or carries
// credentials, and for a user without a password or a password without a user.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("jenkins: base URL is required")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("jenkins: parse base URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("jenkins: base URL scheme is %q, want http or https", base.Scheme)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("jenkins: base URL %q has no host", cfg.BaseURL)
	}
	if base.User != nil {
		return nil, errors.New("jenkins: base URL must not carry credentials; use User and Password")
	}
	if (cfg.User == "") != (cfg.Password == "") {
		return nil, errors.New("jenkins: User and Password must be set together, or both empty for anonymous access")
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawQuery, base.Fragment = "", ""
	return &Client{
		base:     base,
		user:     cfg.User,
		password: cfg.Password,
		http:     cmp.Or(cfg.HTTPClient, &http.Client{Timeout: 30 * time.Second}),
	}, nil
}

// Jobs lists the jobs and folders directly inside folder, or at the root of
// the controller when folder is empty. It does not recurse.
func (c *Client) Jobs(ctx context.Context, folder string) ([]Job, error) {
	u, err := c.folderURL(folder, "api", "json")
	if err != nil {
		return nil, err
	}
	setTree(u, "jobs[name,fullName,url,color,buildable]")
	var out struct {
		Jobs []wireJob `json:"jobs"`
	}
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(out.Jobs))
	for _, w := range out.Jobs {
		jobs = append(jobs, w.job())
	}
	return jobs, nil
}

// Job returns one job with its last build and the parameters it accepts.
func (c *Client) Job(ctx context.Context, job string) (Job, error) {
	u, err := c.jobURL(job, "api", "json")
	if err != nil {
		return Job{}, err
	}
	setTree(u, "name,fullName,url,color,buildable,inQueue,description,"+
		"lastBuild["+buildFields+"],"+
		"property[parameterDefinitions[name,type,description,defaultParameterValue[value]]]")
	var w wireJob
	if err := c.getJSON(ctx, u, &w); err != nil {
		return Job{}, err
	}
	return w.job(), nil
}

// Builds lists a job's most recent builds, newest first. A limit below one
// selects 10; a limit above 100 is capped there.
func (c *Client) Builds(ctx context.Context, job string, limit int) ([]Build, error) {
	if limit < 1 {
		limit = defaultBuilds
	}
	limit = min(limit, maxBuilds)
	u, err := c.jobURL(job, "api", "json")
	if err != nil {
		return nil, err
	}
	setTree(u, fmt.Sprintf("builds[%s]{0,%d}", buildFields, limit))
	var out struct {
		Builds []wireBuild `json:"builds"`
	}
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	builds := make([]Build, 0, len(out.Builds))
	for _, w := range out.Builds {
		builds = append(builds, w.build())
	}
	return builds, nil
}

// Build returns one build of a job. A number below one selects the last build.
func (c *Client) Build(ctx context.Context, job string, number int) (Build, error) {
	u, err := c.jobURL(job, buildRef(number), "api", "json")
	if err != nil {
		return Build{}, err
	}
	setTree(u, buildFields)
	var w wireBuild
	if err := c.getJSON(ctx, u, &w); err != nil {
		return Build{}, err
	}
	return w.build(), nil
}

// Console returns at most maxBytes of a build's console log starting at byte
// offset start; a negative start returns the tail, which is where a failure
// usually is. A number below one selects the last build. A maxBytes below one
// selects 64 KiB and anything above 1 MiB is capped there. The text is always
// valid UTF-8: bytes the log holds that are not are replaced with U+FFFD.
func (c *Client) Console(ctx context.Context, job string, number int, start int64, maxBytes int) (Console, error) {
	if maxBytes < 1 {
		maxBytes = defaultConsoleBytes
	}
	maxBytes = min(maxBytes, maxConsoleBytes)

	if start < 0 {
		// Jenkins answers a start past the end with an empty body and the
		// current length in X-Text-Size, which is the only way to seek back.
		probe, err := c.logText(ctx, job, number, math.MaxInt64)
		if err != nil {
			return Console{}, err
		}
		size := textSize(probe.Header)
		_ = probe.Body.Close() // nothing was read; the window request follows
		start = max(0, size-int64(maxBytes))
	}

	resp, err := c.logText(ctx, job, number, start)
	if err != nil {
		return Console{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return Console{}, fmt.Errorf("read console of %q: %w", job, err)
	}
	end := start + int64(len(body))
	size := textSize(resp.Header)
	if size == 0 {
		size = end // the controller advertised no length
	}
	return Console{
		Text:      strings.ToValidUTF8(string(body), "�"),
		Start:     start,
		End:       end,
		Size:      size,
		Truncated: end < size,
		Running:   resp.Header.Get("X-More-Data") == "true",
	}, nil
}

// TestReport returns a build's JUnit results, with at most maxFailures failing
// cases attached; a maxFailures below one fetches the counts alone, and
// anything above 200 is capped there. A number below one selects the last
// build. A build that published no test report reports [ErrNotFound].
func (c *Client) TestReport(ctx context.Context, job string, number, maxFailures int) (TestReport, error) {
	u, err := c.jobURL(job, buildRef(number), "testReport", "api", "json")
	if err != nil {
		return TestReport{}, err
	}
	maxFailures = min(maxFailures, maxTestFailures)
	tree := "failCount,passCount,skipCount"
	if maxFailures > 0 {
		tree += ",suites[cases[className,name,status,age,failedSince,errorDetails]]"
	}
	setTree(u, tree)

	var w struct {
		FailCount int `json:"failCount"`
		PassCount int `json:"passCount"`
		SkipCount int `json:"skipCount"`
		Suites    []struct {
			Cases []struct {
				ClassName    string `json:"className"`
				Name         string `json:"name"`
				Status       string `json:"status"`
				Age          int    `json:"age"`
				FailedSince  int    `json:"failedSince"`
				ErrorDetails string `json:"errorDetails"`
			} `json:"cases"`
		} `json:"suites"`
	}
	if err := c.getJSON(ctx, u, &w); err != nil {
		return TestReport{}, err
	}

	report := TestReport{
		Total:   w.FailCount + w.PassCount + w.SkipCount,
		Passed:  w.PassCount,
		Failed:  w.FailCount,
		Skipped: w.SkipCount,
	}
	for _, suite := range w.Suites {
		for _, tc := range suite.Cases {
			if len(report.Failures) >= maxFailures {
				return report, nil
			}
			if tc.Status != "FAILED" && tc.Status != "REGRESSION" {
				continue
			}
			report.Failures = append(report.Failures, TestCase{
				Suite:       tc.ClassName,
				Name:        tc.Name,
				Status:      strings.ToLower(tc.Status),
				Age:         tc.Age,
				FailedSince: tc.FailedSince,
				Error:       truncate(tc.ErrorDetails, maxTestError),
			})
		}
	}
	return report, nil
}

// truncate cuts s to at most n runes, never mid-rune, marking what it dropped.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "… (truncated)"
}

// Trigger starts a build of a job and returns the queue item it waits in.
// Passing parameters selects the buildWithParameters endpoint; a job that
// declares no parameters rejects it.
func (c *Client) Trigger(ctx context.Context, job string, params map[string]string) (Queued, error) {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	endpoint := "build"
	var body io.Reader
	if len(form) > 0 {
		endpoint, body = "buildWithParameters", strings.NewReader(form.Encode())
	}
	u, err := c.jobURL(job, endpoint)
	if err != nil {
		return Queued{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return Queued{}, fmt.Errorf("trigger %q: %w", job, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c.setCrumb(ctx, req)
	resp, err := c.send(req)
	if err != nil {
		return Queued{}, err
	}
	defer resp.Body.Close()

	queued := Queued{Job: job, QueueURL: resp.Header.Get("Location")}
	if _, last, ok := strings.CutLast(strings.TrimSuffix(queued.QueueURL, "/"), "/"); ok {
		queued.QueueID, _ = strconv.Atoi(last)
	}
	return queued, nil
}

// Stop aborts a running build. Unlike the read methods, it requires an
// explicit build number: an accidental "last build" is a build nobody meant
// to stop.
func (c *Client) Stop(ctx context.Context, job string, number int) error {
	if number < 1 {
		return fmt.Errorf("stop %q: build number is required, got %d", job, number)
	}
	u, err := c.jobURL(job, strconv.Itoa(number), "stop")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return fmt.Errorf("stop %q #%d: %w", job, number, err)
	}
	c.setCrumb(ctx, req)
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// buildFields is the tree selector for the fields a Build carries.
const buildFields = "number,result,building,timestamp,duration,url,displayName"

// folderURL builds a URL under the controller from a job path, which may be
// empty for the root, plus literal tail segments.
func (c *Client) folderURL(job string, tail ...string) (*url.URL, error) {
	elems := []string{c.base.Path}
	if job = strings.Trim(job, "/"); job != "" {
		for seg := range strings.SplitSeq(job, "/") {
			if seg == "" || seg == "." || seg == ".." || !utf8.ValidString(seg) ||
				strings.ContainsFunc(seg, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				return nil, fmt.Errorf("%w: %q", ErrInvalidJob, job)
			}
			elems = append(elems, "job", seg)
		}
	}
	u := c.base.Clone()
	u.Path = path.Join(append(elems, tail...)...)
	return u, nil
}

// jobURL is folderURL for the paths where a job is required.
func (c *Client) jobURL(job string, tail ...string) (*url.URL, error) {
	if strings.Trim(job, "/") == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidJob, job)
	}
	return c.folderURL(job, tail...)
}

// logText requests the console window beginning at start, returning the
// response so the caller reads X-Text-Size before the body.
func (c *Client) logText(ctx context.Context, job string, number int, start int64) (*http.Response, error) {
	u, err := c.jobURL(job, buildRef(number), "logText", "progressiveText")
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"start": {strconv.FormatInt(start, 10)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("console of %q: %w", job, err)
	}
	return c.send(req)
}

// getJSON decodes a bounded JSON response into dst.
func (c *Client) getJSON(ctx context.Context, u *url.URL, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("request %s: %w", u.Path, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", u.Path, err)
	}
	if len(body) > maxJSONBytes {
		return fmt.Errorf("read %s: response exceeds %d bytes", u.Path, maxJSONBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s: %w", u.Path, err)
	}
	return nil
}

// send attaches credentials, performs req, and maps a failing status to an
// error that carries no response body — a Jenkins error page is HTML meant for
// an operator, not for the model reading this.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, ErrNotFound)
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, ErrUnauthorized)
		}
		return nil, fmt.Errorf("%s %s: unexpected status %s", req.Method, req.URL.Path, resp.Status)
	}
	return resp, nil
}

// setCrumb adds the CSRF header for a state-changing request. A controller
// with CSRF protection disabled has no crumb issuer, and a failure to reach it
// surfaces on the request that follows.
func (c *Client) setCrumb(ctx context.Context, req *http.Request) {
	u := c.base.Clone()
	u.Path = path.Join(c.base.Path, "crumbIssuer", "api", "json")
	var out struct {
		Crumb string `json:"crumb"`
		Field string `json:"crumbRequestField"`
	}
	if err := c.getJSON(ctx, u, &out); err == nil && out.Field != "" {
		req.Header.Set(out.Field, out.Crumb)
	}
}

// buildRef is the URL segment addressing a build: an explicit number, or the
// controller's alias for the most recent one.
func buildRef(number int) string {
	if number < 1 {
		return "lastBuild"
	}
	return strconv.Itoa(number)
}

// setTree limits a response to the fields the caller decodes.
func setTree(u *url.URL, tree string) {
	u.RawQuery = url.Values{"tree": {tree}}.Encode()
}

// textSize reports the console length Jenkins advertises, or zero.
func textSize(h http.Header) int64 {
	n, err := strconv.ParseInt(h.Get("X-Text-Size"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// wireJob and wireBuild are the controller's JSON. They exist because Jenkins
// reports state as a ball color and time as milliseconds, which no caller
// should have to decode.
type wireJob struct {
	Class       string     `json:"_class"`
	Name        string     `json:"name"`
	FullName    string     `json:"fullName"`
	URL         string     `json:"url"`
	Color       string     `json:"color"`
	Buildable   bool       `json:"buildable"`
	InQueue     bool       `json:"inQueue"`
	Description string     `json:"description"`
	LastBuild   *wireBuild `json:"lastBuild"`
	Property    []struct {
		ParameterDefinitions []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Description string `json:"description"`
			Default     struct {
				Value any `json:"value"`
			} `json:"defaultParameterValue"`
		} `json:"parameterDefinitions"`
	} `json:"property"`
}

type wireBuild struct {
	Number      int    `json:"number"`
	Result      string `json:"result"`
	Building    bool   `json:"building"`
	Timestamp   int64  `json:"timestamp"`
	Duration    int64  `json:"duration"`
	URL         string `json:"url"`
	DisplayName string `json:"displayName"`
}

func (w wireJob) job() Job {
	job := Job{
		Name:        w.Name,
		FullName:    w.FullName,
		URL:         w.URL,
		Buildable:   w.Buildable,
		InQueue:     w.InQueue,
		Description: w.Description,
		Folder:      strings.Contains(w.Class, "Folder") || strings.HasSuffix(w.Class, "MultiBranchProject"),
	}
	color, building := strings.CutSuffix(w.Color, "_anime")
	job.Building = building
	switch color {
	case "blue":
		job.Status = "success"
	case "red":
		job.Status = "failure"
	case "yellow":
		job.Status = "unstable"
	case "aborted":
		job.Status = "aborted"
	case "grey", "notbuilt":
		job.Status = "not built"
	case "disabled":
		job.Status = "disabled"
	case "":
		job.Status = "unknown"
	default:
		job.Status = color
	}
	if w.LastBuild != nil {
		last := w.LastBuild.build()
		job.LastBuild = &last
	}
	for _, p := range w.Property {
		for _, def := range p.ParameterDefinitions {
			param := Parameter{Name: def.Name, Type: def.Type, Description: def.Description}
			if def.Default.Value != nil {
				param.Default = fmt.Sprint(def.Default.Value)
			}
			job.Parameters = append(job.Parameters, param)
		}
	}
	return job
}

func (w wireBuild) build() Build {
	build := Build{
		Number:      w.Number,
		Building:    w.Building,
		DisplayName: w.DisplayName,
		URL:         w.URL,
	}
	switch {
	case w.Building:
		build.Status = "building"
	case w.Result == "":
		build.Status = "unknown"
	default:
		build.Status = strings.ToLower(strings.ReplaceAll(w.Result, "_", " "))
	}
	if w.Timestamp > 0 {
		build.StartedAt = time.UnixMilli(w.Timestamp).UTC()
	}
	if w.Duration > 0 {
		build.Duration = (time.Duration(w.Duration) * time.Millisecond).String()
	}
	return build
}
