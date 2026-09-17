// Package mcptools exposes a Jenkins controller to a model as MCP tools.
package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eugeneshershen/jenkins-mcp/internal/analysis"
	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
)

type (
	listJobsInput struct {
		Folder string `json:"folder,omitempty" jsonschema:"folder to list, such as team or team/subteam; omit for the top level"`
	}

	jobInput struct {
		Job string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
	}

	listBuildsInput struct {
		Job   string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Limit int    `json:"limit,omitempty" jsonschema:"how many builds to return, newest first; default 10, capped at 100"`
	}

	getBuildInput struct {
		Job   string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Build int    `json:"build,omitempty" jsonschema:"build number; omit for the most recent build"`
	}

	consoleInput struct {
		Job      string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Build    int    `json:"build,omitempty" jsonschema:"build number; omit for the most recent build"`
		Start    *int64 `json:"start,omitempty" jsonschema:"byte offset to read from, usually the end of a previous window; omit to read the tail of the log"`
		MaxBytes int    `json:"maxBytes,omitempty" jsonschema:"how many bytes to return at most; default 65536, capped at 1048576"`
	}

	triggerInput struct {
		Job        string            `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Parameters map[string]string `json:"parameters,omitempty" jsonschema:"build parameters by name; a job that declares none rejects them"`
	}

	stopInput struct {
		Job   string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Build int    `json:"build" jsonschema:"number of the build to abort"`
	}

	summaryInput struct {
		JobFilter  string `json:"jobFilter,omitempty" jsonschema:"keep only jobs whose name contains this, case-insensitively, such as DATAHUB; omit for every job"`
		Since      string `json:"since,omitempty" jsonschema:"how far back to look, as a Go duration such as 24h or 72h; omit for 24h"`
		MinCluster int    `json:"minCluster,omitempty" jsonschema:"how many fresh failures a suite needs to be reported as a cluster; omit for 3"`
		Depth      int    `json:"depth,omitempty" jsonschema:"how many builds per job to examine before filtering by the window; omit for 10, capped at 50"`
	}

	testReportInput struct {
		Job         string `json:"job" jsonschema:"job path, such as nightly or team/nightly"`
		Build       int    `json:"build,omitempty" jsonschema:"build number; omit for the most recent build"`
		MaxFailures *int   `json:"maxFailures,omitempty" jsonschema:"how many failing tests to list; omit for 20, 0 for counts only, capped at 200"`
	}
)

// Tool outputs. A result that is naturally a list or a word still travels as
// an object: MCP structured content is a JSON object, and a client rejects a
// tool whose output schema is an array or a string.
type (
	jobsOutput struct {
		Jobs []jenkins.Job `json:"jobs"`
	}

	buildsOutput struct {
		Builds []jenkins.Build `json:"builds"`
	}

	stopOutput struct {
		Job     string `json:"job"`
		Build   int    `json:"build"`
		Stopped bool   `json:"stopped"`
	}
)

// Register adds the Jenkins tools to s, each one calling c. The five read
// tools are annotated read-only; jenkins_trigger_build and jenkins_stop_build
// change the controller's state, so a client may ask the user before calling
// them.
func Register(s *mcp.Server, c *jenkins.Client) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_list_jobs",
		Description: "List the Jenkins jobs and folders directly inside a folder, or at the top level " +
			"when folder is omitted. This does not recurse: to descend, pass a returned job's " +
			"fullName as folder.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listJobsInput) (*mcp.CallToolResult, jobsOutput, error) {
		jobs, err := c.Jobs(ctx, in.Folder)
		return nil, jobsOutput{Jobs: jobs}, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_get_job",
		Description: "Get one Jenkins job: its current status, its last build, and the parameters it " +
			"accepts. Read the parameters here before triggering a parameterized build.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jobInput) (*mcp.CallToolResult, jenkins.Job, error) {
		job, err := c.Job(ctx, in.Job)
		return nil, job, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_list_builds",
		Description: "List a job's recent builds, newest first, with status, start time and duration. " +
			"Use it to see whether a failure is new or how long a job has been red.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listBuildsInput) (*mcp.CallToolResult, buildsOutput, error) {
		builds, err := c.Builds(ctx, in.Job, in.Limit)
		return nil, buildsOutput{Builds: builds}, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "jenkins_get_build",
		Description: "Get one build of a job: its status, when it started and how long it ran.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getBuildInput) (*mcp.CallToolResult, jenkins.Build, error) {
		build, err := c.Build(ctx, in.Job, in.Build)
		return nil, build, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_get_console_log",
		Description: "Read a window of a build's console log. With no start it returns the tail, where " +
			"a failure is usually reported. The result carries end, the offset to pass as the next " +
			"start, and size, the bytes available now; truncated means output past end is already " +
			"there, and running means the build is still writing.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in consoleInput) (*mcp.CallToolResult, jenkins.Console, error) {
		start := int64(-1)
		if in.Start != nil {
			start = *in.Start
		}
		console, err := c.Console(ctx, in.Job, in.Build, start, in.MaxBytes)
		return nil, console, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_summarize_failures",
		Description: "Aggregate a whole window of builds in one call — use this before drilling into " +
			"single jobs. Returns, for every job matching jobFilter: the runs with their local date, " +
			"test totals, failures split by age (fresh = new in that build, standing = long broken), " +
			"the kind of each fresh failure, the services and database tables named outright in the " +
			"error text, and the suites where several fresh failures landed together — the shape a " +
			"real regression takes. Builds that published no test report are listed apart from " +
			"failing tests.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in summaryInput) (*mcp.CallToolResult, analysis.Summary, error) {
		opts := analysis.Options{JobFilter: in.JobFilter, MinCluster: in.MinCluster, Depth: in.Depth}
		if in.Since != "" {
			since, err := time.ParseDuration(in.Since)
			if err != nil {
				return nil, analysis.Summary{}, fmt.Errorf("since %q is not a duration such as 24h or 72h: %w", in.Since, err)
			}
			opts.Since = since
		}
		summary, err := analysis.Summarize(ctx, c, opts)
		return nil, summary, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_get_test_results",
		Description: "Read a build's test results: how many passed, failed and were skipped, plus the " +
			"failing tests themselves. Each failure carries age — 1 means it started failing in this " +
			"build, a large age means a standing failure — so a fresh regression can be told from " +
			"long-broken tests. A build that published no test report reports not found.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in testReportInput) (*mcp.CallToolResult, jenkins.TestReport, error) {
		maxFailures := 20
		if in.MaxFailures != nil {
			maxFailures = *in.MaxFailures
		}
		report, err := c.TestReport(ctx, in.Job, in.Build, maxFailures)
		return nil, report, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_trigger_build",
		Description: "Start a build of a job and return the queue item it waits in. A build number is " +
			"assigned only once an executor picks the item up.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triggerInput) (*mcp.CallToolResult, jenkins.Queued, error) {
		queued, err := c.Trigger(ctx, in.Job, in.Parameters)
		return nil, queued, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "jenkins_stop_build",
		Description: "Abort a running build. The build number is required: there is no shorthand for " +
			"the most recent build here.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in stopInput) (*mcp.CallToolResult, stopOutput, error) {
		if err := c.Stop(ctx, in.Job, in.Build); err != nil {
			return nil, stopOutput{}, err
		}
		return nil, stopOutput{Job: in.Job, Build: in.Build, Stopped: true}, nil
	})
}
