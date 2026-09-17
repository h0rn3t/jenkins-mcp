package jenkins_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
)

// newClient wires a Client to h over the in-memory test transport. srv.Client()
// is called before srv.URL is read: the server has no address until then.
func newClient(t *testing.T, h http.HandlerFunc) *jenkins.Client {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	hc := srv.Client()
	c, err := jenkins.New(jenkins.Config{BaseURL: srv.URL, User: "ci", Password: "s3cret", HTTPClient: hc})
	if err != nil {
		t.Fatalf("New(%q) error = %v, want nil", srv.URL, err)
	}
	return c
}

// recorder answers every request with body and records the last request line.
func recorder(t *testing.T, body string) (*jenkins.Client, *http.Request) {
	t.Helper()
	var last http.Request
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		last = *r
		fmt.Fprint(w, body)
	})
	return c, &last
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     jenkins.Config
		wantErr bool
	}{
		{"https url", jenkins.Config{BaseURL: "https://ci.example.com", User: "u", Password: "t"}, false},
		{"http url", jenkins.Config{BaseURL: "http://ci.example.com:8080", User: "u", Password: "t"}, false},
		{"trailing slash", jenkins.Config{BaseURL: "https://ci.example.com/jenkins/", User: "u", Password: "t"}, false},
		{"anonymous", jenkins.Config{BaseURL: "https://ci.example.com"}, false},
		{"empty url", jenkins.Config{User: "u", Password: "t"}, true},
		{"no scheme", jenkins.Config{BaseURL: "ci.example.com", User: "u", Password: "t"}, true},
		{"file scheme", jenkins.Config{BaseURL: "file:///etc/passwd"}, true},
		{"user without password", jenkins.Config{BaseURL: "https://ci.example.com", User: "u"}, true},
		{"password without user", jenkins.Config{BaseURL: "https://ci.example.com", Password: "t"}, true},
		{"credentials in url", jenkins.Config{BaseURL: "https://u:p@ci.example.com"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := jenkins.New(tt.cfg)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("New(%+v) error = %v, want error presence = %t", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestJobPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		job      string
		wantPath string
		wantErr  bool
	}{
		{"top level", "build", "/job/build/api/json", false},
		{"folder", "team/nightly", "/job/team/job/nightly/api/json", false},
		{"deep folder", "a/b/c", "/job/a/job/b/job/c/api/json", false},
		{"leading slash trimmed", "/build/", "/job/build/api/json", false},
		{"space escaped", "my job", "/job/my%20job/api/json", false},
		{"question mark escaped", "a?b", "/job/a%3fb/api/json", false},
		{"hash escaped", "a#b", "/job/a%23b/api/json", false},
		{"percent escaped", "a%2fb", "/job/a%252fb/api/json", false},
		{"empty", "", "", true},
		{"dot dot escape", "../manage", "", true},
		{"dot dot inside", "team/../../manage", "", true},
		{"single dot", "./build", "", true},
		{"empty segment", "team//nightly", "", true},
		{"newline", "build\nX-Evil: 1", "", true},
		{"null byte", "build\x00", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, last := recorder(t, `{"name":"x"}`)
			_, err := c.Job(t.Context(), tt.job)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("Job(%q) error = %v, want error presence = %t", tt.job, err, tt.wantErr)
			}
			if tt.wantErr {
				if !errors.Is(err, jenkins.ErrInvalidJob) {
					t.Errorf("Job(%q) error = %v, want errors.Is(err, ErrInvalidJob)", tt.job, err)
				}
				return
			}
			if got := last.URL.EscapedPath(); !strings.EqualFold(got, tt.wantPath) {
				t.Errorf("Job(%q) requested path = %q, want %q", tt.job, got, tt.wantPath)
			}
		})
	}
}

func TestRequestCarriesAuth(t *testing.T) {
	t.Parallel()
	c, last := recorder(t, `{"name":"x"}`)
	if _, err := c.Job(t.Context(), "build"); err != nil {
		t.Fatalf("Job(%q) error = %v, want nil", "build", err)
	}
	user, token, ok := last.BasicAuth()
	if !ok || user != "ci" || token != "s3cret" {
		t.Errorf("Job(%q) basic auth = (%q, %q, %t), want (%q, %q, true)", "build", user, token, ok, "ci", "s3cret")
	}
	if got := last.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Job(%q) Accept = %q, want %q", "build", got, "application/json")
	}
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"not found", http.StatusNotFound, jenkins.ErrNotFound},
		{"unauthorized", http.StatusUnauthorized, jenkins.ErrUnauthorized},
		{"forbidden", http.StatusForbidden, jenkins.ErrUnauthorized},
		{"server error", http.StatusInternalServerError, nil},
		{"bad gateway", http.StatusBadGateway, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "secret-internal-detail", tt.status)
			})
			_, err := c.Job(t.Context(), "build")
			if err == nil {
				t.Fatalf("Job(%q) on HTTP %d error = nil, want non-nil", "build", tt.status)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Job(%q) on HTTP %d error = %v, want errors.Is(err, %v)", "build", tt.status, err, tt.want)
			}
			if strings.Contains(err.Error(), "secret-internal-detail") {
				t.Errorf("Job(%q) on HTTP %d error = %v, want no response body in the message", "build", tt.status, err)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("Job(%q) on HTTP %d error = %v, want no credentials in the message", "build", tt.status, err)
			}
		})
	}
}

func TestJobsListsChildren(t *testing.T) {
	t.Parallel()
	const body = `{"jobs":[
		{"name":"nightly","fullName":"team/nightly","url":"https://ci/job/team/job/nightly/","color":"blue_anime","buildable":true,"_class":"hudson.model.FreeStyleProject"},
		{"name":"sub","fullName":"team/sub","url":"https://ci/job/team/job/sub/","_class":"com.cloudbees.hudson.plugins.folder.Folder"}
	]}`
	c, last := recorder(t, body)
	got, err := c.Jobs(t.Context(), "team")
	if err != nil {
		t.Fatalf("Jobs(%q) error = %v, want nil", "team", err)
	}
	if want := "/job/team/api/json"; last.URL.Path != want {
		t.Errorf("Jobs(%q) requested path = %q, want %q", "team", last.URL.Path, want)
	}
	if len(got) != 2 {
		t.Fatalf("Jobs(%q) = %d jobs, want 2", "team", len(got))
	}
	if got[0].Status != "success" || !got[0].Building {
		t.Errorf("Jobs(%q)[0] status = %q, building = %t, want %q, true", "team", got[0].Status, got[0].Building, "success")
	}
	if !got[1].Folder {
		t.Errorf("Jobs(%q)[1] folder = false, want true for a Folder _class", "team")
	}
}

func TestJobsAtRootUsesBasePath(t *testing.T) {
	t.Parallel()
	c, last := recorder(t, `{"jobs":[]}`)
	got, err := c.Jobs(t.Context(), "")
	if err != nil {
		t.Fatalf("Jobs(%q) error = %v, want nil", "", err)
	}
	if want := "/api/json"; last.URL.Path != want {
		t.Errorf("Jobs(%q) requested path = %q, want %q", "", last.URL.Path, want)
	}
	if got == nil {
		t.Errorf("Jobs(%q) = nil, want an empty non-nil slice so the result encodes as []", "")
	}
}

func TestJobStatusFromColor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		color        string
		wantStatus   string
		wantBuilding bool
	}{
		{"blue", "success", false},
		{"blue_anime", "success", true},
		{"red", "failure", false},
		{"red_anime", "failure", true},
		{"yellow", "unstable", false},
		{"aborted", "aborted", false},
		{"notbuilt", "not built", false},
		{"grey", "not built", false},
		{"disabled", "disabled", false},
		{"", "unknown", false},
		{"chartreuse", "chartreuse", false},
	}
	for _, tt := range tests {
		t.Run(tt.color, func(t *testing.T) {
			t.Parallel()
			c, _ := recorder(t, fmt.Sprintf(`{"jobs":[{"name":"j","color":%q}]}`, tt.color))
			got, err := c.Jobs(t.Context(), "")
			if err != nil {
				t.Fatalf("Jobs(%q) error = %v, want nil", "", err)
			}
			if got[0].Status != tt.wantStatus || got[0].Building != tt.wantBuilding {
				t.Errorf("color %q = (%q, %t), want (%q, %t)", tt.color, got[0].Status, got[0].Building, tt.wantStatus, tt.wantBuilding)
			}
		})
	}
}

func TestBuildsDecodeTimes(t *testing.T) {
	t.Parallel()
	const body = `{"builds":[
		{"number":42,"result":"FAILURE","building":false,"timestamp":1700000000000,"duration":201000,"url":"https://ci/job/b/42/"},
		{"number":43,"result":null,"building":true,"timestamp":1700000300000,"duration":0,"url":"https://ci/job/b/43/"}
	]}`
	c, last := recorder(t, body)
	got, err := c.Builds(t.Context(), "b", 5)
	if err != nil {
		t.Fatalf("Builds(%q, 5) error = %v, want nil", "b", err)
	}
	if want := "builds[number,result,building,timestamp,duration,url,displayName]{0,5}"; last.URL.Query().Get("tree") != want {
		t.Errorf("Builds(%q, 5) tree = %q, want %q", "b", last.URL.Query().Get("tree"), want)
	}
	if got[0].Status != "failure" {
		t.Errorf("Builds(%q, 5)[0].Status = %q, want %q", "b", got[0].Status, "failure")
	}
	if want := time.UnixMilli(1700000000000).UTC(); !got[0].StartedAt.Equal(want) {
		t.Errorf("Builds(%q, 5)[0].StartedAt = %v, want %v", "b", got[0].StartedAt, want)
	}
	if want := "3m21s"; got[0].Duration != want {
		t.Errorf("Builds(%q, 5)[0].Duration = %q, want %q", "b", got[0].Duration, want)
	}
	if got[1].Status != "building" {
		t.Errorf("Builds(%q, 5)[1].Status = %q, want %q for a null result while building", "b", got[1].Status, "building")
	}
	if got[1].Duration != "" {
		t.Errorf("Builds(%q, 5)[1].Duration = %q, want %q for a running build", "b", got[1].Duration, "")
	}
}

func TestBuildsLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		limit int
		want  string
	}{
		{"explicit", 3, "{0,3}"},
		{"zero defaults", 0, "{0,10}"},
		{"negative defaults", -1, "{0,10}"},
		{"over cap", 500, "{0,100}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, last := recorder(t, `{"builds":[]}`)
			if _, err := c.Builds(t.Context(), "b", tt.limit); err != nil {
				t.Fatalf("Builds(%q, %d) error = %v, want nil", "b", tt.limit, err)
			}
			if got := last.URL.Query().Get("tree"); !strings.HasSuffix(got, tt.want) {
				t.Errorf("Builds(%q, %d) tree = %q, want suffix %q", "b", tt.limit, got, tt.want)
			}
		})
	}
}

func TestBuildNumberSelectsPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		number int
		want   string
	}{
		{"explicit", 7, "/job/b/7/api/json"},
		{"zero is last build", 0, "/job/b/lastBuild/api/json"},
		{"negative is last build", -1, "/job/b/lastBuild/api/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, last := recorder(t, `{"number":7,"result":"SUCCESS"}`)
			if _, err := c.Build(t.Context(), "b", tt.number); err != nil {
				t.Fatalf("Build(%q, %d) error = %v, want nil", "b", tt.number, err)
			}
			if last.URL.Path != tt.want {
				t.Errorf("Build(%q, %d) requested path = %q, want %q", "b", tt.number, last.URL.Path, tt.want)
			}
		})
	}
}

func TestConsoleWindow(t *testing.T) {
	t.Parallel()
	const log = "0123456789abcdefghij"
	console := func(t *testing.T, more bool) (*jenkins.Client, *http.Request) {
		t.Helper()
		var last http.Request
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			last = *r
			start := int64(0)
			fmt.Sscanf(r.URL.Query().Get("start"), "%d", &start)
			w.Header().Set("X-Text-Size", strconv.Itoa(len(log)))
			if more {
				w.Header().Set("X-More-Data", "true")
			}
			if start < int64(len(log)) {
				fmt.Fprint(w, log[start:])
			}
		})
		return c, &last
	}

	t.Run("whole log fits", func(t *testing.T) {
		t.Parallel()
		c, last := console(t, false)
		got, err := c.Console(t.Context(), "b", 3, 0, 100)
		if err != nil {
			t.Fatalf("Console(%q, 3, 0, 100) error = %v, want nil", "b", err)
		}
		if want := "/job/b/3/logText/progressiveText"; last.URL.Path != want {
			t.Errorf("Console requested path = %q, want %q", last.URL.Path, want)
		}
		if got.Text != log || got.Start != 0 || got.End != 20 || got.Size != 20 {
			t.Errorf("Console(%q, 3, 0, 100) = %+v, want the whole log with End = Size = 20", "b", got)
		}
		if got.Truncated || got.Running {
			t.Errorf("Console(%q, 3, 0, 100) truncated = %t, running = %t, want false, false", "b", got.Truncated, got.Running)
		}
	})

	t.Run("window truncated to max", func(t *testing.T) {
		t.Parallel()
		c, _ := console(t, false)
		got, err := c.Console(t.Context(), "b", 3, 4, 6)
		if err != nil {
			t.Fatalf("Console(%q, 3, 4, 6) error = %v, want nil", "b", err)
		}
		if got.Text != "456789" || got.Start != 4 || got.End != 10 {
			t.Errorf("Console(%q, 3, 4, 6) = %+v, want text %q from 4 to 10", "b", got, "456789")
		}
		if !got.Truncated {
			t.Errorf("Console(%q, 3, 4, 6) truncated = false, want true while End < Size", "b")
		}
	})

	t.Run("negative start tails the log", func(t *testing.T) {
		t.Parallel()
		c, _ := console(t, false)
		got, err := c.Console(t.Context(), "b", 3, -1, 6)
		if err != nil {
			t.Fatalf("Console(%q, 3, -1, 6) error = %v, want nil", "b", err)
		}
		if got.Text != "efghij" || got.Start != 14 || got.End != 20 {
			t.Errorf("Console(%q, 3, -1, 6) = %+v, want the last 6 bytes", "b", got)
		}
		if got.Truncated {
			t.Errorf("Console(%q, 3, -1, 6) truncated = true, want false once the tail reaches Size", "b")
		}
	})

	t.Run("running build reports more data", func(t *testing.T) {
		t.Parallel()
		c, _ := console(t, true)
		got, err := c.Console(t.Context(), "b", 3, 0, 100)
		if err != nil {
			t.Fatalf("Console(%q, 3, 0, 100) error = %v, want nil", "b", err)
		}
		if !got.Running {
			t.Errorf("Console(%q, 3, 0, 100) running = false, want true when X-More-Data is set", "b")
		}
	})

	t.Run("last build when number is zero", func(t *testing.T) {
		t.Parallel()
		c, last := console(t, false)
		if _, err := c.Console(t.Context(), "b", 0, 0, 100); err != nil {
			t.Fatalf("Console(%q, 0, 0, 100) error = %v, want nil", "b", err)
		}
		if want := "/job/b/lastBuild/logText/progressiveText"; last.URL.Path != want {
			t.Errorf("Console requested path = %q, want %q", last.URL.Path, want)
		}
	})
}

func TestConsoleKeepsTextValidUTF8(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Text-Size", "6")
		fmt.Fprint(w, "ok\xff\xfe!\n")
	})
	got, err := c.Console(t.Context(), "b", 1, 0, 100)
	if err != nil {
		t.Fatalf("Console(%q, 1, 0, 100) error = %v, want nil", "b", err)
	}
	if !strings.ContainsRune(got.Text, '�') || !strings.HasPrefix(got.Text, "ok") {
		t.Errorf("Console(%q, 1, 0, 100).Text = %q, want invalid bytes replaced with U+FFFD", "b", got.Text)
	}
}

func TestConsoleMaxBytesBounds(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 2<<20)
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Text-Size", strconv.Itoa(len(big)))
		fmt.Fprint(w, big)
	})
	tests := []struct {
		name string
		max  int
		want int
	}{
		{"zero defaults", 0, 64 << 10},
		{"negative defaults", -5, 64 << 10},
		{"over cap", 8 << 20, 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := c.Console(t.Context(), "b", 1, 0, tt.max)
			if err != nil {
				t.Fatalf("Console(%q, 1, 0, %d) error = %v, want nil", "b", tt.max, err)
			}
			if len(got.Text) != tt.want {
				t.Errorf("Console(%q, 1, 0, %d) returned %d bytes, want %d", "b", tt.max, len(got.Text), tt.want)
			}
		})
	}
}

func TestOversizedJSONRejected(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"jobs":[`)
		filler := strings.Repeat(`{"name":"`+strings.Repeat("j", 1000)+`"},`, 5000)
		fmt.Fprint(w, filler)
		fmt.Fprint(w, `{"name":"last"}]}`)
	})
	if _, err := c.Jobs(t.Context(), ""); err == nil {
		t.Errorf("Jobs(%q) on an oversized body error = nil, want a size error", "")
	}
}

func TestTriggerBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		params   map[string]string
		wantPath string
	}{
		{"no parameters", nil, "/job/b/build"},
		{"empty parameters", map[string]string{}, "/job/b/build"},
		{"with parameters", map[string]string{"BRANCH": "main"}, "/job/b/buildWithParameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var last http.Request
			var body string
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/crumbIssuer/api/json" {
					fmt.Fprint(w, `{"crumb":"abc123","crumbRequestField":"Jenkins-Crumb"}`)
					return
				}
				last = *r
				b := make([]byte, 256)
				n, _ := r.Body.Read(b)
				body = string(b[:n])
				w.Header().Set("Location", "https://ci/queue/item/99/")
				w.WriteHeader(http.StatusCreated)
			})
			got, err := c.Trigger(t.Context(), "b", tt.params)
			if err != nil {
				t.Fatalf("Trigger(%q, %v) error = %v, want nil", "b", tt.params, err)
			}
			if last.Method != http.MethodPost {
				t.Errorf("Trigger(%q, %v) method = %q, want %q", "b", tt.params, last.Method, http.MethodPost)
			}
			if last.URL.Path != tt.wantPath {
				t.Errorf("Trigger(%q, %v) path = %q, want %q", "b", tt.params, last.URL.Path, tt.wantPath)
			}
			if got := last.Header.Get("Jenkins-Crumb"); got != "abc123" {
				t.Errorf("Trigger(%q, %v) Jenkins-Crumb = %q, want %q", "b", tt.params, got, "abc123")
			}
			for k, v := range tt.params {
				if !strings.Contains(body, k+"="+v) {
					t.Errorf("Trigger(%q, %v) body = %q, want it to carry %s=%s", "b", tt.params, body, k, v)
				}
			}
			if got.QueueURL != "https://ci/queue/item/99/" || got.QueueID != 99 {
				t.Errorf("Trigger(%q, %v) = %+v, want queue item 99 from the Location header", "b", tt.params, got)
			}
		})
	}
}

func TestTriggerWithoutCrumbIssuer(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			http.Error(w, "no crumb issuer", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Jenkins-Crumb"); got != "" {
			t.Errorf("Trigger sent Jenkins-Crumb = %q, want none when CSRF protection is off", got)
		}
		w.Header().Set("Location", "https://ci/queue/item/7/")
		w.WriteHeader(http.StatusCreated)
	})
	if _, err := c.Trigger(t.Context(), "b", nil); err != nil {
		t.Errorf("Trigger(%q, nil) error = %v, want nil when the crumb issuer is absent", "b", err)
	}
}

func TestStopBuild(t *testing.T) {
	t.Parallel()
	var last http.Request
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			fmt.Fprint(w, `{"crumb":"abc123","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		last = *r
	})
	if err := c.Stop(t.Context(), "b", 12); err != nil {
		t.Fatalf("Stop(%q, 12) error = %v, want nil", "b", err)
	}
	if last.Method != http.MethodPost || last.URL.Path != "/job/b/12/stop" {
		t.Errorf("Stop(%q, 12) sent %s %s, want POST /job/b/12/stop", "b", last.Method, last.URL.Path)
	}
}

func TestStopRequiresBuildNumber(t *testing.T) {
	t.Parallel()
	c, _ := recorder(t, "")
	if err := c.Stop(t.Context(), "b", 0); err == nil {
		t.Errorf("Stop(%q, 0) error = nil, want an error rather than stopping the last build", "b")
	}
}

func TestTestReportCountsOnly(t *testing.T) {
	t.Parallel()
	c, last := recorder(t, `{"failCount":28,"passCount":624,"skipCount":3}`)
	got, err := c.TestReport(t.Context(), "b", 86, 0)
	if err != nil {
		t.Fatalf("TestReport(%q, 86, 0) error = %v, want nil", "b", err)
	}
	if want := "/job/b/86/testReport/api/json"; last.URL.Path != want {
		t.Errorf("TestReport(%q, 86, 0) path = %q, want %q", "b", last.URL.Path, want)
	}
	if tree := last.URL.Query().Get("tree"); strings.Contains(tree, "suites") {
		t.Errorf("TestReport(%q, 86, 0) tree = %q, want no suites when no failures are asked for", "b", tree)
	}
	if got.Failed != 28 || got.Passed != 624 || got.Skipped != 3 || got.Total != 655 {
		t.Errorf("TestReport(%q, 86, 0) = %+v, want 28 failed, 624 passed, 3 skipped, 655 total", "b", got)
	}
	if got.Failures != nil {
		t.Errorf("TestReport(%q, 86, 0).Failures = %v, want nil", "b", got.Failures)
	}
}

func TestTestReportFailures(t *testing.T) {
	t.Parallel()
	body := `{"failCount":3,"passCount":1,"skipCount":0,"suites":[{"cases":[
		{"className":"Процес А","name":"крок 1","status":"PASSED","age":0},
		{"className":"Процес А","name":"крок 2","status":"FAILED","age":1,"failedSince":86,"errorDetails":"` + strings.Repeat("ю", 700) + `"},
		{"className":"Процес Б","name":"крок 3","status":"REGRESSION","age":1,"failedSince":86},
		{"className":"Процес В","name":"крок 4","status":"FAILED","age":37,"failedSince":49}
	]}]}`
	c, last := recorder(t, body)
	got, err := c.TestReport(t.Context(), "b", 86, 10)
	if err != nil {
		t.Fatalf("TestReport(%q, 86, 10) error = %v, want nil", "b", err)
	}
	if tree := last.URL.Query().Get("tree"); !strings.Contains(tree, "suites[cases[") {
		t.Errorf("TestReport(%q, 86, 10) tree = %q, want the case fields requested", "b", tree)
	}
	if len(got.Failures) != 3 {
		t.Fatalf("TestReport(%q, 86, 10) returned %d failures, want 3 (passed cases dropped)", "b", len(got.Failures))
	}
	if got.Failures[0].Suite != "Процес А" || got.Failures[0].Name != "крок 2" || got.Failures[0].Age != 1 {
		t.Errorf("TestReport(%q, 86, 10).Failures[0] = %+v, want the failing case of Процес А", "b", got.Failures[0])
	}
	if n := len([]rune(got.Failures[0].Error)); n > 520 {
		t.Errorf("TestReport(%q, 86, 10).Failures[0].Error is %d runes, want it truncated near 500", "b", n)
	}
	if !utf8.ValidString(got.Failures[0].Error) {
		t.Errorf("TestReport(%q, 86, 10).Failures[0].Error is not valid UTF-8 after truncation", "b")
	}
	if got.Failures[1].Status != "regression" {
		t.Errorf("TestReport(%q, 86, 10).Failures[1].Status = %q, want %q", "b", got.Failures[1].Status, "regression")
	}
}

func TestTestReportFailureCap(t *testing.T) {
	t.Parallel()
	cases := make([]string, 0, 300)
	for i := range 300 {
		cases = append(cases, fmt.Sprintf(`{"className":"S","name":"t%d","status":"FAILED","age":1}`, i))
	}
	body := `{"failCount":300,"passCount":0,"skipCount":0,"suites":[{"cases":[` + strings.Join(cases, ",") + `]}]}`
	tests := []struct {
		name string
		max  int
		want int
	}{
		{"under cap", 5, 5},
		{"over cap", 9999, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := recorder(t, body)
			got, err := c.TestReport(t.Context(), "b", 86, tt.max)
			if err != nil {
				t.Fatalf("TestReport(%q, 86, %d) error = %v, want nil", "b", tt.max, err)
			}
			if len(got.Failures) != tt.want {
				t.Errorf("TestReport(%q, 86, %d) returned %d failures, want %d", "b", tt.max, len(got.Failures), tt.want)
			}
			if got.Failed != 300 {
				t.Errorf("TestReport(%q, 86, %d).Failed = %d, want the full count 300 even when the list is capped", "b", tt.max, got.Failed)
			}
		})
	}
}

func TestTestReportMissingIsNotFound(t *testing.T) {
	t.Parallel()
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no test report", http.StatusNotFound)
	})
	_, err := c.TestReport(t.Context(), "b", 86, 0)
	if !errors.Is(err, jenkins.ErrNotFound) {
		t.Errorf("TestReport(%q, 86, 0) on a build with no report error = %v, want errors.Is(err, ErrNotFound)", "b", err)
	}
}

func TestTestReportLastBuild(t *testing.T) {
	t.Parallel()
	c, last := recorder(t, `{"failCount":0,"passCount":1,"skipCount":0}`)
	if _, err := c.TestReport(t.Context(), "b", 0, 0); err != nil {
		t.Fatalf("TestReport(%q, 0, 0) error = %v, want nil", "b", err)
	}
	if want := "/job/b/lastBuild/testReport/api/json"; last.URL.Path != want {
		t.Errorf("TestReport(%q, 0, 0) path = %q, want %q", "b", last.URL.Path, want)
	}
}
