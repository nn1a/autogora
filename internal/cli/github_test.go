package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/githubissues"
	setupcfg "github.com/nn1a/autogora/internal/setup"
)

type githubCLICommandResult struct {
	output setupcfg.CommandOutput
	err    error
}

type githubCLIRunner struct {
	calls   [][]string
	results []githubCLICommandResult
}

func (r *githubCLIRunner) LookPath(file string) (string, error) { return "/tools/" + file, nil }

func (r *githubCLIRunner) Run(_ context.Context, _ string, file string, args ...string) (setupcfg.CommandOutput, error) {
	r.calls = append(r.calls, append([]string{file}, args...))
	if len(r.results) > 0 {
		result := r.results[0]
		r.results = r.results[1:]
		return result.output, result.err
	}
	return setupcfg.CommandOutput{Stdout: `[{"id":"I_kwDOE_audit","number":9,"title":"Improve audit log","body":"Show what changed.","url":"https://github.corp.example/platform/control/issues/9","state":"OPEN","labels":[],"assignees":[],"author":{"login":"alex"},"createdAt":"2026-07-01T00:00:00Z","updatedAt":"2026-07-02T00:00:00Z"}]`}, nil
}

func TestGitHubCLIImportsEnterpriseIssuesIdempotently(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	runner := &githubCLIRunner{}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd, app.CommandRunner = directory, runner
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	dryRun := runApp(t, app, "github", "import", "--db", dbPath, "--host", "github.corp.example", "--repo", "platform/control", "--label", "automation", "--limit", "10", "--dry-run")
	if !strings.Contains(dryRun, `"status": "success"`) || !strings.Contains(dryRun, `"dryRun": true`) || !strings.Contains(dryRun, `"planned": 1`) || !strings.Contains(dryRun, `"action": "create"`) {
		t.Fatalf("dry-run output: %s", dryRun)
	}
	first := runApp(t, app, "github", "import", "--db", dbPath, "--host", "github.corp.example", "--repo", "platform/control", "--tenant", "platform")
	second := runApp(t, app, "github", "import", "--db", dbPath, "--repo", "github.corp.example/platform/control")
	if !strings.Contains(first, `"created": 1`) || !strings.Contains(second, `"existing": 1`) {
		t.Fatalf("idempotency output: first=%s second=%s", first, second)
	}
	listed := runApp(t, app, "list", "--db", dbPath, "--status", "triage")
	if strings.Count(listed, `"id":`) != 1 || !strings.Contains(listed, `"tenant": "platform"`) {
		t.Fatalf("imported task list: %s", listed)
	}
	if len(runner.calls) != 3 || !strings.Contains(strings.Join(runner.calls[0], " "), "--repo github.corp.example/platform/control") {
		t.Fatalf("gh calls: %#v", runner.calls)
	}
}

func TestGitHubCLIRejectsAmbiguousIssueFilters(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(string) string { return "" }
	err := app.Run(context.Background(), []string{"github", "import", "--issue", "4", "--label", "bug"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("ambiguous filters error = %v", err)
	}
}

func TestGitHubCLIRejectsUnknownAndEmptyOptionsBeforeFetch(t *testing.T) {
	tests := [][]string{
		{"github", "import", "--lable", "bug"},
		{"github", "import", "--all"},
		{"github", "import", "--label="},
		{"github", "import", "--search=   "},
		{"github", "import", "--state="},
		{"github", "import", "--repo="},
	}
	for _, args := range tests {
		runner := &githubCLIRunner{}
		app := New(&bytes.Buffer{}, &bytes.Buffer{})
		app.CommandRunner = runner
		app.Getenv = func(string) string { return "" }
		err := app.Run(context.Background(), args)
		if err == nil || (!strings.Contains(err.Error(), "unknown github import option") && !strings.Contains(err.Error(), "cannot be empty")) {
			t.Fatalf("args %v error = %v", args, err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("args %v unexpectedly fetched GitHub issues: %#v", args, runner.calls)
		}
	}
}

func TestGitHubCLIWritesPartialResultAndReturnsError(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	issue := `{"id":"I_kwDOE_partial","number":9,"title":"Keep partial results","body":"Import this issue.","url":"https://github.corp.example/platform/control/issues/9","state":"OPEN","labels":[],"assignees":[],"author":{"login":"alex"},"createdAt":"2026-07-01T00:00:00Z","updatedAt":"2026-07-02T00:00:00Z"}`
	runner := &githubCLIRunner{results: []githubCLICommandResult{
		{output: setupcfg.CommandOutput{Stdout: issue}},
		{output: setupcfg.CommandOutput{Stderr: "issue not found"}, err: errors.New("exit status 1")},
	}}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd, app.CommandRunner = directory, runner
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)
	stdout := &bytes.Buffer{}
	app.Stdout = stdout

	err := app.Run(context.Background(), []string{"github", "import", "--db", dbPath, "--repo", "github.corp.example/platform/control", "--issue", "9", "--issue", "10"})
	if !githubissues.IsPartialImportError(err) {
		t.Fatalf("partial CLI error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, `"status": "partial"`) || !strings.Contains(output, `"created": 1`) || !strings.Contains(output, `"failed": 1`) || !strings.Contains(output, `"number": 10`) {
		t.Fatalf("partial CLI JSON was not preserved: %s", output)
	}
}
