package main_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// binary is the command under test, built once for the whole package. The MCP
// client below starts it as a child process, so it has to exist on disk.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "jenkins-mcp-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "jenkins-mcp")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// The fake controller listens on a real address: the command runs in its own
// process and cannot be handed an in-memory transport.
func fakeJenkins(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestServesJenkinsToolsOverStdio(t *testing.T) {
	t.Parallel()
	url := fakeJenkins(t, func(w http.ResponseWriter, r *http.Request) {
		if user, token, ok := r.BasicAuth(); !ok || user != "ci" || token != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"jobs":[{"name":"nightly","fullName":"nightly","color":"blue"}]}`)
	})

	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = append(os.Environ(), "JENKINS_URL="+url, "JENKINS_USER=ci", "JENKINS_TOKEN=tok")
	cmd.Stderr = os.Stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to %s error = %v, want nil", binary, err)
	}
	t.Cleanup(func() { session.Close() })

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if len(tools.Tools) != 9 {
		t.Errorf("ListTools() returned %d tools, want 9", len(tools.Tools))
	}

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "jenkins_list_jobs"})
	if err != nil {
		t.Fatalf("CallTool(jenkins_list_jobs) error = %v, want nil", err)
	}
	if res.IsError {
		t.Fatalf("CallTool(jenkins_list_jobs) is a tool error: %+v", res.Content)
	}
	got := fmt.Sprint(res.StructuredContent)
	if !strings.Contains(got, "nightly") || !strings.Contains(got, "success") {
		t.Errorf("CallTool(jenkins_list_jobs) = %s, want the nightly job with status success", got)
	}
}

func TestRefusesToStartWithoutConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"no url", nil, "JENKINS_URL"},
		{"bad url scheme", []string{"JENKINS_URL=ftp://ci.example.com"}, "http"},
		{"user without secret", []string{"JENKINS_URL=https://ci.example.com", "JENKINS_USER=ci"}, "JENKINS_PASSWORD"},
		{"password without user", []string{"JENKINS_URL=https://ci.example.com", "JENKINS_PASSWORD=pw"}, "JENKINS_USER"},
		{"bad timeout", []string{"JENKINS_URL=https://ci.example.com", "JENKINS_TIMEOUT=soon"}, "JENKINS_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), binary)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tt.env...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s with %v exited 0, want a non-zero exit", binary, tt.env)
			}
			if !strings.Contains(string(out), tt.want) {
				t.Errorf("%s with %v reported %q, want it to mention %q", binary, tt.env, out, tt.want)
			}
		})
	}
}

func TestAuthenticatesWithTokenOrPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
		want string // the Basic-auth secret the controller should receive
	}{
		{"token", []string{"JENKINS_USER=ci", "JENKINS_TOKEN=11aabb"}, "11aabb"},
		{"password", []string{"JENKINS_USER=ci", "JENKINS_PASSWORD=hunter2"}, "hunter2"},
		{"token wins over password", []string{"JENKINS_USER=ci", "JENKINS_TOKEN=11aabb", "JENKINS_PASSWORD=hunter2"}, "11aabb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := make(chan string, 1)
			url := fakeJenkins(t, func(w http.ResponseWriter, r *http.Request) {
				_, secret, _ := r.BasicAuth()
				select {
				case got <- secret:
				default:
				}
				fmt.Fprint(w, `{"jobs":[]}`)
			})

			cmd := exec.CommandContext(t.Context(), binary)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "JENKINS_URL=" + url}, tt.env...)
			session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
				Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
			if err != nil {
				t.Fatalf("connecting with %v error = %v, want nil", tt.env, err)
			}
			t.Cleanup(func() { session.Close() })
			if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "jenkins_list_jobs"}); err != nil {
				t.Fatalf("CallTool(jenkins_list_jobs) with %v error = %v, want nil", tt.env, err)
			}
			if sent := <-got; sent != tt.want {
				t.Errorf("with %v the controller received secret %q, want %q", tt.env, sent, tt.want)
			}
		})
	}
}

func TestKeepsSecretsOutOfDiagnostics(t *testing.T) {
	t.Parallel()
	url := fakeJenkins(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	})
	// The child's stderr goes to a file: reading a buffer it is still writing
	// to would be a race.
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v, want nil", err)
	}
	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")},
		"JENKINS_URL="+url, "JENKINS_USER=ci", "JENKINS_TOKEN=super-secret-token")
	cmd.Stderr = stderr

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).
		Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to %s error = %v, want nil", binary, err)
	}
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "jenkins_get_job",
		Arguments: map[string]any{"job": "x"},
	})
	if err != nil {
		t.Fatalf("CallTool(jenkins_get_job) error = %v, want a tool error, not a protocol error", err)
	}
	if !res.IsError {
		t.Fatalf("CallTool(jenkins_get_job) against a 401 controller isError = false, want true")
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	reported := b.String()
	if !strings.Contains(reported, "unauthorized") {
		t.Errorf("CallTool(jenkins_get_job) reported %q, want it to say unauthorized", reported)
	}
	session.Close()

	logged, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatalf("reading the command's stderr error = %v, want nil", err)
	}
	for _, out := range []string{reported, string(logged)} {
		if strings.Contains(out, "super-secret-token") {
			t.Errorf("%s produced %q, want no API token in it", binary, out)
		}
	}
}
