package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/kanban/internal/model"
)

func runApp(t *testing.T, app *App, args ...string) string {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app.Stdout, app.Stderr = stdout, stderr
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("run %v: %v (stderr=%s)", args, err, stderr.String())
	}
	return stdout.String()
}

func TestCoreCLIUsesOneBoardKernel(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "kanban.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }

	initialized := strings.TrimSpace(runApp(t, app, "init", "--db", dbPath))
	if initialized != dbPath {
		t.Fatalf("init path = %q, want %q", initialized, dbPath)
	}
	createdJSON := runApp(t, app, "create", "--db", dbPath, "--assignee", "worker", "--runtime", "codex", "--priority", "7", "Analyze", "repository")
	var created struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	if created.Task.Title != "Analyze repository" || created.Task.Status != model.TaskStatusReady || created.Task.Priority != 7 {
		t.Fatalf("unexpected create output: %+v", created.Task)
	}

	listedJSON := runApp(t, app, "list", "--db", dbPath, "--assignee", "worker")
	var listed []model.Task
	if err := json.Unmarshal([]byte(listedJSON), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.Task.ID {
		t.Fatalf("list did not share create kernel: %+v", listed)
	}
	shown := runApp(t, app, "show", "--db", dbPath, created.Task.ID)
	if !strings.Contains(shown, `"relationshipGraph"`) || !strings.Contains(shown, `"workerContext"`) {
		t.Fatalf("show omitted bounded execution context: %s", shown)
	}
	stats := runApp(t, app, "stats", "--db", dbPath)
	if !strings.Contains(stats, `"total": 1`) {
		t.Fatalf("stats output mismatch: %s", stats)
	}
}

func TestBoardsAndBulkCommandsPreserveIsolationAndPartialErrors(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "kanban.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)
	runApp(t, app, "boards", "create", "research", "--db", dbPath, "--name", "Research")
	createdJSON := runApp(t, app, "create", "Board task", "--db", dbPath, "--board", "research")
	var created struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	bulkJSON := runApp(t, app, "bulk", created.Task.ID, "t_missing", "--db", dbPath, "--board", "research", "--priority", "9")
	var bulk struct {
		OK     []any `json:"ok"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal([]byte(bulkJSON), &bulk); err != nil {
		t.Fatal(err)
	}
	if len(bulk.OK) != 1 || len(bulk.Errors) != 1 {
		t.Fatalf("bulk did not preserve per-task results: %s", bulkJSON)
	}
	defaultList := runApp(t, app, "list", "--db", dbPath)
	if strings.Contains(defaultList, created.Task.ID) {
		t.Fatalf("board task leaked into default board: %s", defaultList)
	}
}

func TestScopedWorkerCannotEscapeTaskOrCommandAllowlist(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(name string) string {
		if name == "TASKCIRCUIT_TASK_ID" {
			return "t_scoped"
		}
		return ""
	}
	if err := app.Run(context.Background(), []string{"list"}); err == nil || !strings.Contains(err.Error(), "dispatcher-scoped") {
		t.Fatalf("scoped worker escaped command allowlist: %v", err)
	}
	if _, err := app.scopedTaskID("t_other", "show"); err == nil || !strings.Contains(err.Error(), "scoped to task") {
		t.Fatalf("scoped worker escaped task boundary: %v", err)
	}
}
