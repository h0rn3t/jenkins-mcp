package mcptools_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eugeneshershen/jenkins-mcp/internal/analysis"
	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
	"github.com/eugeneshershen/jenkins-mcp/internal/mcptools"
)

// connect serves h as the Jenkins controller and returns a client session
// talking to an MCP server with the Jenkins tools registered.
func connect(t *testing.T, h http.HandlerFunc) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	hc := srv.Client()
	jc, err := jenkins.New(jenkins.Config{BaseURL: srv.URL, HTTPClient: hc})
	if err != nil {
		t.Fatalf("jenkins.New(%q) error = %v, want nil", srv.URL, err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "jenkins-mcp", Version: "test"}, nil)
	mcptools.Register(server, jc)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(t.Context(), serverTransport, nil); err != nil {
		t.Fatalf("Server.Connect() error = %v, want nil", err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("Client.Connect() error = %v, want nil", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call invokes a tool and decodes its structured output into dst.
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, dst any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q, %v) error = %v, want nil", name, args, err)
	}
	if dst != nil {
		if res.IsError {
			t.Fatalf("CallTool(%q, %v) is a tool error: %s", name, args, text(res))
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("CallTool(%q, %v) structured content is not JSON: %v", name, args, err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("CallTool(%q, %v) structured content %s into %T: %v", name, args, raw, dst, err)
		}
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsRegistered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		readOnly bool
		required []string
	}{
		{"jenkins_list_jobs", true, nil},
		{"jenkins_get_job", true, []string{"job"}},
		{"jenkins_list_builds", true, []string{"job"}},
		{"jenkins_get_build", true, []string{"job"}},
		{"jenkins_get_console_log", true, []string{"job"}},
		{"jenkins_get_test_results", true, []string{"job"}},
		{"jenkins_summarize_failures", true, nil},
		{"jenkins_trigger_build", false, []string{"job"}},
		{"jenkins_stop_build", false, []string{"job", "build"}},
	}

	session := connect(t, func(w http.ResponseWriter, r *http.Request) {})
	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	if len(byName) != len(tests) {
		t.Errorf("ListTools() returned %d tools, want %d", len(byName), len(tests))
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := byName[tt.name]
			if !ok {
				t.Fatalf("ListTools() has no tool %q", tt.name)
			}
			if tool.Description == "" {
				t.Errorf("tool %q description = %q, want a description for the model", tt.name, tool.Description)
			}
			readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
			if readOnly != tt.readOnly {
				t.Errorf("tool %q readOnlyHint = %t, want %t", tt.name, readOnly, tt.readOnly)
			}
			schema, ok := tool.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("tool %q input schema = %T, want a JSON object", tt.name, tool.InputSchema)
			}
			var required []string
			// "required" is absent when the tool has no required argument.
			if list, ok := schema["required"].([]any); ok {
				for _, r := range list {
					required = append(required, r.(string))
				}
			}
			slices.Sort(required)
			want := slices.Clone(tt.required)
			slices.Sort(want)
			if !slices.Equal(required, want) {
				t.Errorf("tool %q required = %v, want %v", tt.name, required, want)
			}
			// MCP structured content is an object: a client rejects a tool
			// whose output schema is an array or a string.
			out, ok := tool.OutputSchema.(map[string]any)
			if !ok {
				t.Fatalf("tool %q output schema = %T, want a JSON object", tt.name, tool.OutputSchema)
			}
			if out["type"] != "object" {
				t.Errorf("tool %q output schema type = %v, want %q", tt.name, out["type"], "object")
			}
		})
	}
}

func TestListJobsTool(t *testing.T) {
	t.Parallel()
	var path string
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, `{"jobs":[{"name":"nightly","fullName":"team/nightly","color":"red"}]}`)
	})

	var got struct {
		Jobs []jenkins.Job `json:"jobs"`
	}
	call(t, session, "jenkins_list_jobs", map[string]any{"folder": "team"}, &got)
	if path != "/job/team/api/json" {
		t.Errorf("jenkins_list_jobs requested %q, want %q", path, "/job/team/api/json")
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Status != "failure" {
		t.Errorf("jenkins_list_jobs = %+v, want one job with status %q", got, "failure")
	}

	call(t, session, "jenkins_list_jobs", map[string]any{}, &got)
	if path != "/api/json" {
		t.Errorf("jenkins_list_jobs with no folder requested %q, want %q", path, "/api/json")
	}
}

func TestConsoleToolDefaultsToTail(t *testing.T) {
	t.Parallel()
	const log = "line one\nline two\nBUILD FAILED\n"
	var starts []string
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("start"))
		w.Header().Set("X-Text-Size", strconv.Itoa(len(log)))
		var start int
		fmt.Sscanf(r.URL.Query().Get("start"), "%d", &start)
		if start < len(log) {
			fmt.Fprint(w, log[start:])
		}
	})

	var got jenkins.Console
	call(t, session, "jenkins_get_console_log", map[string]any{"job": "b", "maxBytes": 13}, &got)
	if got.Text != "BUILD FAILED\n" {
		t.Errorf("jenkins_get_console_log without start = %q, want the tail %q", got.Text, "BUILD FAILED\n")
	}
	if len(starts) != 2 {
		t.Errorf("jenkins_get_console_log made %d requests (%v), want a size probe then the window", len(starts), starts)
	}

	call(t, session, "jenkins_get_console_log", map[string]any{"job": "b", "start": 0, "maxBytes": 8}, &got)
	if got.Text != "line one" || got.End != 8 || !got.Truncated {
		t.Errorf("jenkins_get_console_log with start 0 = %+v, want the first 8 bytes marked truncated", got)
	}
}

func TestConsoleToolUsesLastBuild(t *testing.T) {
	t.Parallel()
	var path string
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("X-Text-Size", "2")
		fmt.Fprint(w, "hi")
	})
	var got jenkins.Console
	call(t, session, "jenkins_get_console_log", map[string]any{"job": "b", "start": 0}, &got)
	if want := "/job/b/lastBuild/logText/progressiveText"; path != want {
		t.Errorf("jenkins_get_console_log without a build requested %q, want %q", path, want)
	}
}

func TestTriggerAndStopTools(t *testing.T) {
	t.Parallel()
	var method, path string
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/crumbIssuer") {
			http.Error(w, "disabled", http.StatusNotFound)
			return
		}
		method, path = r.Method, r.URL.Path
		w.Header().Set("Location", "https://ci/queue/item/5/")
		w.WriteHeader(http.StatusCreated)
	})

	var queued jenkins.Queued
	call(t, session, "jenkins_trigger_build", map[string]any{
		"job":        "team/nightly",
		"parameters": map[string]any{"BRANCH": "main"},
	}, &queued)
	if method != http.MethodPost || path != "/job/team/job/nightly/buildWithParameters" {
		t.Errorf("jenkins_trigger_build sent %s %s, want POST /job/team/job/nightly/buildWithParameters", method, path)
	}
	if queued.QueueID != 5 {
		t.Errorf("jenkins_trigger_build = %+v, want queue item 5", queued)
	}

	var stopped struct {
		Job     string `json:"job"`
		Build   int    `json:"build"`
		Stopped bool   `json:"stopped"`
	}
	call(t, session, "jenkins_stop_build", map[string]any{"job": "b", "build": 9}, &stopped)
	if !stopped.Stopped || stopped.Build != 9 {
		t.Errorf("jenkins_stop_build = %+v, want build 9 reported stopped", stopped)
	}
	if path != "/job/b/9/stop" {
		t.Errorf("jenkins_stop_build sent %s, want /job/b/9/stop", path)
	}
}

func TestJenkinsFailureIsToolError(t *testing.T) {
	t.Parallel()
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>stack trace</html>", http.StatusNotFound)
	})
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"jenkins_get_job", map[string]any{"job": "missing"}, "not found"},
		{"jenkins_get_console_log", map[string]any{"job": "missing", "start": 0}, "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, session, tt.name, tt.args, nil)
			if !res.IsError {
				t.Fatalf("%s on a missing job isError = false, want true", tt.name)
			}
			if got := text(res); !strings.Contains(got, tt.want) {
				t.Errorf("%s error = %q, want it to mention %q", tt.name, got, tt.want)
			}
			if strings.Contains(text(res), "stack trace") {
				t.Errorf("%s error = %q, want no controller HTML in the message", tt.name, text(res))
			}
		})
	}
}

func TestInvalidJobIsToolError(t *testing.T) {
	t.Parallel()
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an invalid job path reached the controller: %s", r.URL.Path)
	})
	res := call(t, session, "jenkins_get_job", map[string]any{"job": "../../manage"}, nil)
	if !res.IsError {
		t.Errorf("jenkins_get_job(%q) isError = false, want true", "../../manage")
	}
}

func TestMissingRequiredArgumentIsRejected(t *testing.T) {
	t.Parallel()
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a call with no job reached the controller: %s", r.URL.Path)
	})
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "jenkins_stop_build",
		Arguments: map[string]any{"job": "b"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Errorf("jenkins_stop_build without build = %+v, %v, want a rejected call", res, err)
	}
}

func TestTestResultsTool(t *testing.T) {
	t.Parallel()
	const body = `{"failCount":2,"passCount":10,"skipCount":0,"suites":[{"cases":[
		{"className":"Процес А","name":"крок 1","status":"FAILED","age":1,"failedSince":86},
		{"className":"Процес Б","name":"крок 2","status":"FAILED","age":37,"failedSince":49},
		{"className":"Процес В","name":"крок 3","status":"PASSED","age":0}
	]}]}`
	var tree string
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		tree = r.URL.Query().Get("tree")
		fmt.Fprint(w, body)
	})

	var got jenkins.TestReport
	call(t, session, "jenkins_get_test_results", map[string]any{"job": "b", "build": 86}, &got)
	if got.Total != 12 || got.Failed != 2 || got.Passed != 10 {
		t.Errorf("jenkins_get_test_results = %+v, want 2 failed of 12", got)
	}
	if len(got.Failures) != 2 {
		t.Fatalf("jenkins_get_test_results returned %d failures, want 2 by default", len(got.Failures))
	}
	if got.Failures[0].Age != 1 || got.Failures[1].Age != 37 {
		t.Errorf("jenkins_get_test_results failure ages = %d, %d, want 1 and 37", got.Failures[0].Age, got.Failures[1].Age)
	}

	var counts jenkins.TestReport // a fresh value: "failures" is omitted when empty
	call(t, session, "jenkins_get_test_results", map[string]any{"job": "b", "build": 86, "maxFailures": 0}, &counts)
	if counts.Failures != nil {
		t.Errorf("jenkins_get_test_results with maxFailures 0 returned %d failures, want none", len(counts.Failures))
	}
	if counts.Failed != 2 {
		t.Errorf("jenkins_get_test_results with maxFailures 0 .Failed = %d, want the count 2", counts.Failed)
	}
	if strings.Contains(tree, "suites") {
		t.Errorf("jenkins_get_test_results with maxFailures 0 requested tree %q, want no suites", tree)
	}
}

func TestSummarizeFailuresTool(t *testing.T) {
	t.Parallel()
	session := connect(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/testReport/api/json"):
			fmt.Fprint(w, `{"failCount":3,"passCount":7,"skipCount":0,"suites":[{"cases":[
				{"className":"Фіча","name":"a","status":"FAILED","age":1,"failedSince":5,"errorDetails":"UI API response GET https://t/ms-ts/api/v1/x: fail"},
				{"className":"Фіча","name":"b","status":"FAILED","age":1,"failedSince":5,"errorDetails":"UI API response GET https://t/ms-ts/api/v1/y: fail"},
				{"className":"Фіча","name":"c","status":"FAILED","age":1,"failedSince":5,"errorDetails":"UI API response GET https://t/ms-ts/api/v1/z: fail"}
			]}]}`)
		case strings.HasSuffix(r.URL.Path, "/api/json") && strings.Contains(r.URL.Query().Get("tree"), "builds["):
			fmt.Fprintf(w, `{"builds":[{"number":9,"result":"UNSTABLE","building":false,"timestamp":%d,"duration":1000}]}`,
				time.Now().Add(-time.Hour).UnixMilli())
		default:
			fmt.Fprint(w, `{"jobs":[{"name":"DATAHUB_A","fullName":"DATAHUB_A","color":"yellow"},{"name":"OTHER","fullName":"OTHER","color":"blue"}]}`)
		}
	})

	var got analysis.Summary
	call(t, session, "jenkins_summarize_failures", map[string]any{"jobFilter": "DATAHUB", "since": "24h"}, &got)
	if got.Window.Jobs != 1 || len(got.Runs) != 1 {
		t.Fatalf("jenkins_summarize_failures = %d jobs / %d runs, want 1 and 1", got.Window.Jobs, len(got.Runs))
	}
	if got.Totals.Tests != 10 || got.Age.Fresh != 3 {
		t.Errorf("jenkins_summarize_failures totals = %+v, age = %+v, want 10 tests and 3 fresh", got.Totals, got.Age)
	}
	if len(got.Clusters) != 1 || got.Clusters[0].Fresh != 3 || got.Clusters[0].Nature != "api-endpoint" {
		t.Errorf("jenkins_summarize_failures clusters = %+v, want one api-endpoint cluster of 3", got.Clusters)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "ms-ts" {
		t.Errorf("jenkins_summarize_failures services = %+v, want ms-ts", got.Services)
	}

	res := call(t, session, "jenkins_summarize_failures", map[string]any{"since": "not-a-duration"}, nil)
	if !res.IsError || !strings.Contains(text(res), "duration") {
		t.Errorf("jenkins_summarize_failures with a bad duration = %+v, want a tool error naming the problem", res)
	}
}
