# jenkins-mcp

An MCP server that gives a model read access to a Jenkins controller — jobs,
builds and console logs — plus the ability to start and abort builds. It speaks
MCP over stdio, so an MCP client launches it as a child process.

## Install

```sh
go install github.com/eugeneshershen/jenkins-mcp/cmd/jenkins-mcp@latest
```

Or build from a checkout:

```sh
go build -o jenkins-mcp ./cmd/jenkins-mcp
```

## Configure

Everything comes from the environment, which is how an MCP client passes
configuration to a server:

| Variable | Meaning |
| --- | --- |
| `JENKINS_URL` | The controller's root URL. Required. A path prefix (`https://ci.example.com/jenkins`) is kept. |
| `JENKINS_USER` | The account to authenticate as. |
| `JENKINS_TOKEN` | That account's API token. Create one under *Your name → Security → API Token*. |
| `JENKINS_PASSWORD` | That account's password, for a controller where you authenticate without a token. |
| `JENKINS_TIMEOUT` | Per-request timeout, e.g. `45s`. Optional, default `30s`. |

Jenkins accepts an API token or a password in the same HTTP Basic field, so
either works. `JENKINS_TOKEN` wins when both are set. A token is worth
preferring: it can be revoked on its own, and it does not grant a web session
to whoever reads it.

Set the user together with one of the two secrets, or neither for a controller
that allows anonymous access; the server refuses to start with only one of them.

The secret is read from the environment and never from a command-line flag, so
it does not show up in the process table. Over a plain `http://` controller it
still travels base64-encoded on every request — that is HTTP Basic auth, not
this server, but it is the reason a revocable token beats an account password.

### Claude Code

```sh
claude mcp add jenkins -- \
  env JENKINS_URL=https://ci.example.com \
      JENKINS_USER=you \
      JENKINS_TOKEN=11aabbcc... \
      jenkins-mcp
```

### Any client that reads a JSON config

```json
{
  "mcpServers": {
    "jenkins": {
      "command": "jenkins-mcp",
      "env": {
        "JENKINS_URL": "https://ci.example.com",
        "JENKINS_USER": "you",
        "JENKINS_TOKEN": "11aabbcc..."
      }
    }
  }
}
```

## Tools

Every `job` argument is a slash-separated path of job names exactly as it
appears in a Jenkins URL: `nightly`, or `team/nightly` for a job in a folder.
Where a build number may be omitted, the most recent build is used.

| Tool | Does |
| --- | --- |
| `jenkins_summarize_failures` | Aggregates a whole window of builds across many jobs in one call — see below. |
| `jenkins_list_jobs` | Lists jobs and folders inside a folder, or at the top level. Does not recurse — pass a returned `fullName` as `folder` to descend. |
| `jenkins_get_job` | One job: status, last build, and the parameters it accepts. |
| `jenkins_list_builds` | A job's recent builds, newest first (default 10, max 100). |
| `jenkins_get_build` | One build: status, start time, duration. |
| `jenkins_get_test_results` | A build's test counts plus the failing tests, each with the `age` that separates a new break from a long-standing one. |
| `jenkins_get_console_log` | A window of a build's console log. Returns the tail by default. |
| `jenkins_trigger_build` | Starts a build and returns the queue item. |
| `jenkins_stop_build` | Aborts a running build. The build number is required. |

The seven read tools are annotated `readOnlyHint`, so a client can let them run
without asking. `jenkins_trigger_build` and `jenkins_stop_build` are not, so a
client can require confirmation.

### Reading a whole night at once

Answering "what broke last night" one build at a time costs dozens of calls and
megabytes of test reports. `jenkins_summarize_failures` does it server-side:

```text
jenkins_summarize_failures  jobFilter: "DATAHUB"  since: "30h"
```

It walks every job whose name contains `jobFilter`, fetches their test reports
concurrently, and returns one compact document:

- `totals` — tests, passed, failed, skipped across the window.
- `age` — failures split into `fresh` (new in that build), `recent` (2–5 builds),
  `standing` (6+) and `debt` (20+). This is the difference between "a regression
  landed" and "the same tests are still red".
- `nature` — what kind each fresh failure is: a missing UI element, a click
  timeout, a response body, a SQL mismatch, a failed export.
- `services` — the services and database tables **named outright** in an error's
  text. Being named is the evidence; a table is reported under its own name
  rather than guessed into a service.
- `clusters` — suites where several fresh failures landed together, with a
  sample error and `failedSince`. A real regression has this shape; scattered
  single failures are environment noise.
- `runs` — each build with its **local** calendar date, which is what groups a
  nightly run that crosses midnight UTC.
- `noTestReport` — builds that published no report at all. A broken publish
  step, not failing tests, and never mixed in with them.

`errors` lists jobs that could not be read; a summary carrying it is partial and
says so rather than silently reporting smaller numbers.

### Reading a long log

`jenkins_get_console_log` never returns a whole log: it returns at most
`maxBytes` (default 64 KiB, hard cap 1 MiB) so one failed build cannot fill the
model's context. The result carries the offsets needed to read more:

- `end` — pass it back as `start` to continue from where the window stopped.
- `size` — how many bytes exist right now.
- `truncated` — output past `end` is already available.
- `running` — the build is still writing.

With no `start`, the window is the **tail** of the log, which is where a
failure is usually reported.

## What it will not do

- **Leave the job tree.** A job path is split on `/` and each segment is
  checked and escaped, so `../../manage`, an embedded newline, or a `?` in a
  job name cannot reach another endpoint or forge a request.
- **Repeat the controller to the model.** A failed request becomes a short
  error naming the method, path and condition. A Jenkins error page is HTML
  meant for an operator and never reaches the model.
- **Log secrets.** The token or password appears in the `Authorization` header
  and nowhere else — not in errors, not on stderr.
- **Send invalid UTF-8.** Console text is sanitised, since build logs carry
  arbitrary bytes.

The server needs only the permissions its account has. For read-only use, give
it an account with Overall/Read and Job/Read and nothing else — then
`jenkins_trigger_build` and `jenkins_stop_build` fail at the controller rather
than relying on the model to avoid them.

## Development

```sh
go test -race ./...     # includes an end-to-end test that builds the binary
                        # and speaks MCP to it over stdio
golangci-lint run ./...
govulncheck ./...
```

Layout: [cmd/jenkins-mcp/](cmd/jenkins-mcp/) reads the environment and serves;
[internal/jenkins/](internal/jenkins/) is the Jenkins client, with no MCP in it;
[internal/analysis/](internal/analysis/) aggregates many builds into one summary,
with no HTTP in it; [internal/mcptools/](internal/mcptools/) maps both onto MCP
tools.
