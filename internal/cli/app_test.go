package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
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
	dbPath := filepath.Join(directory, "taskcircuit.db")
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
	dbPath := filepath.Join(directory, "taskcircuit.db")
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

func TestScopedCLIBridgeCompletesClaimWithoutMCP(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "taskcircuit.db")
	opened, err := store.Open(dbPath, "default", filepath.Join(directory, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Cline CLI bridge", Assignee: stringPointer("cline-worker"), Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"TASKCIRCUIT_DB": dbPath, "TASKCIRCUIT_BOARD": "default", "TASKCIRCUIT_TASK_ID": task.Task.ID,
		"TASKCIRCUIT_RUN_ID": claim.Run.ID, "TASKCIRCUIT_CLAIM_TOKEN": claim.ClaimToken,
	}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(name string) string { return environment[name] }
	runApp(t, app, "heartbeat", task.Task.ID, "--note", "Cline worker running")
	runApp(t, app, "comment", task.Task.ID, "Cline communicated through the scoped CLI bridge.", "--author", "cline")
	runApp(t, app, "complete", task.Task.ID, "--summary", "completed without MCP", "--metadata", `{"verification":["cli-bridge"]}`)

	verified, err := store.Open(dbPath, "default", filepath.Join(directory, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	detail, err := verified.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusDone || len(detail.Comments) != 1 || len(detail.Runs) != 1 || detail.Runs[0].Summary == nil || *detail.Runs[0].Summary != "completed without MCP" {
		t.Fatalf("scoped CLI lifecycle did not reach shared kernel: %+v", detail)
	}
}

func TestCLIExplicitSpecificationAndDecomposition(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "taskcircuit.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	createdJSON := runApp(t, app, "create", "rough", "--triage", "--db", dbPath)
	var created struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	specified := runApp(t, app, "specify", created.Task.ID, "--db", dbPath, "--title", "Precise task", "--body", "Acceptance: tests pass")
	if !strings.Contains(specified, `"ok": true`) || !strings.Contains(specified, "Precise task") {
		t.Fatalf("unexpected specify output: %s", specified)
	}

	rootJSON := runApp(t, app, "create", "rough graph", "--triage", "--db", dbPath)
	var root struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(rootJSON), &root); err != nil {
		t.Fatal(err)
	}
	plan := `{"fanout":true,"rootTitle":"Coordinate","rootBody":"Verify output","reason":"parallel","tasks":[{"key":"one","title":"Child","body":"Implement","assignee":"worker","runtime":"codex","priority":1,"skills":[]}],"dependencies":[]}`
	decomposed := runApp(t, app, "decompose", root.Task.ID, "--db", dbPath, "--default-profile", "worker:codex", "--plan-json", plan)
	if !strings.Contains(decomposed, `"fanout": true`) || !strings.Contains(decomposed, `"childIds"`) {
		t.Fatalf("unexpected decompose output: %s", decomposed)
	}
}

func TestDispatchDryRunFindsEligibleTasksWithoutClaiming(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "taskcircuit.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)
	createdJSON := runApp(t, app, "create", "eligible", "--db", dbPath, "--assignee", "worker", "--runtime", "codex")
	var created struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	output := runApp(t, app, "dispatch", "--dry-run", "--max", "1", "--db", dbPath)
	if !strings.Contains(output, `"dryRun": true`) || !strings.Contains(output, created.Task.ID) {
		t.Fatalf("unexpected dry-run output: %s", output)
	}
	shown := runApp(t, app, "show", created.Task.ID, "--db", dbPath)
	if !strings.Contains(shown, `"status": "ready"`) {
		t.Fatalf("dry run changed task state: %s", shown)
	}
}

func TestBooleanFalseOptionRemainsExplicit(t *testing.T) {
	opts, err := parseOptions([]string{"--auto-decompose=false"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.present("auto-decompose") || opts.flags["auto-decompose"] {
		t.Fatalf("explicit false option was lost: %#v", opts)
	}
}
