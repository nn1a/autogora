package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	setupcfg "github.com/nn1a/autogora/internal/setup"
)

type githubCLIRunner struct {
	calls [][]string
}

func (r *githubCLIRunner) LookPath(file string) (string, error) { return "/tools/" + file, nil }

func (r *githubCLIRunner) Run(_ context.Context, _ string, file string, args ...string) (setupcfg.CommandOutput, error) {
	r.calls = append(r.calls, append([]string{file}, args...))
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
	if !strings.Contains(dryRun, `"dryRun": true`) || !strings.Contains(dryRun, `"planned": 1`) || !strings.Contains(dryRun, `"action": "create"`) {
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
