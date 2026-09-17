// The jenkins-mcp command serves a Jenkins controller's jobs, builds and
// console logs to an MCP client over stdio.
//
// It is configured entirely from the environment, the way an MCP client
// launches a server:
//
//	JENKINS_URL       the controller's root URL (required)
//	JENKINS_USER      the account to authenticate as
//	JENKINS_TOKEN     that account's API token
//	JENKINS_PASSWORD  that account's password, when no token is used
//	JENKINS_TIMEOUT   per-request timeout (optional; default 30s)
//
// Jenkins accepts an API token or a password in the same HTTP Basic field, so
// either works; a token is preferable because it can be revoked on its own and
// does not grant a web session. JENKINS_TOKEN wins when both are set.
//
// The secret is read from the environment and never from a flag, so it does
// not appear in the process table. Leave the user and both secrets unset to
// read a controller that allows anonymous access.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eugeneshershen/jenkins-mcp/internal/jenkins"
	"github.com/eugeneshershen/jenkins-mcp/internal/mcptools"
)

func main() {
	if err := run(); err != nil {
		// stdout carries the MCP protocol; diagnostics go to stderr only.
		fmt.Fprintln(os.Stderr, "jenkins-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	base := os.Getenv("JENKINS_URL")
	if base == "" {
		return errors.New("JENKINS_URL is not set; export the controller URL, such as https://ci.example.com")
	}
	// Jenkins takes an API token or a password in the same Basic-auth field.
	secret := cmp.Or(os.Getenv("JENKINS_TOKEN"), os.Getenv("JENKINS_PASSWORD"))
	user := os.Getenv("JENKINS_USER")
	if user != "" && secret == "" {
		return errors.New("JENKINS_USER is set without JENKINS_TOKEN or JENKINS_PASSWORD")
	}
	if secret != "" && user == "" {
		return errors.New("JENKINS_TOKEN or JENKINS_PASSWORD is set without JENKINS_USER")
	}
	timeout := 30 * time.Second
	if raw := os.Getenv("JENKINS_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("JENKINS_TIMEOUT %q is not a duration such as 45s: %w", raw, err)
		}
		timeout = parsed
	}

	client, err := jenkins.New(jenkins.Config{
		BaseURL:    base,
		User:       user,
		Password:   secret,
		HTTPClient: &http.Client{Timeout: timeout},
	})
	if err != nil {
		return err
	}

	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jenkins-mcp",
		Title:   "Jenkins",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Read-and-trigger access to a Jenkins controller. Start from jenkins_list_jobs " +
			"to find a job, then jenkins_list_builds to see how it has been doing, then " +
			"jenkins_get_console_log for why a build failed — it returns the tail of the log by " +
			"default. Job arguments are slash-separated paths as they appear in a Jenkins URL, " +
			"such as team/nightly.",
		// Stderr usually lands in the client's log: report trouble, not every
		// session that opens and closes.
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	mcptools.Register(server, client)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, &mcp.StdioTransport{})
}
