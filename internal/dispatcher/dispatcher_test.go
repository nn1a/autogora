package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/orchestration"
	"github.com/nn1a/kanban/internal/store"
)

func boolValue(value bool) *bool                       { return &value }
func durationValue(value time.Duration) *time.Duration { return &value }

func testManager(t *testing.T) (*boards.Manager, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "taskcircuit.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "default", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	return manager, dbPath
}

func executableFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+content+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildTaskCircuit(t *testing.T) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	repository := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	path := filepath.Join(t.TempDir(), "taskcircuit")
	command := exec.Command("go", "build", "-o", path, "./cmd/taskcircuit")
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build TaskCircuit: %v\n%s", err, output)
	}
	return path
}

func TestDispatcherRateLimitDoesNotConsumeRetry(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "rate limited", Assignee: &assignee, Runtime: model.RuntimeCodex})
	opened.Close()
	fixture := executableFixture(t, "exit 75")
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/taskcircuit", Once: true, MaxWorkers: 1,
		RateLimitCooldown: durationValue(0), AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "TASKCIRCUIT_CODEX_BIN" {
				return fixture
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusReady || detail.Task.FailureCount != 0 || detail.Runs[0].Status != model.RunStatusRateLimited {
		t.Fatalf("unexpected retry state: %#v", detail)
	}
}

func TestRecoveryRequeuesRecordedDeadWorker(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	defer opened.Close()
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "crashed", Assignee: &assignee, Runtime: model.RuntimeCodex})
	claim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	deadPID := 99999999
	if _, err := opened.RecordSpawn(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, deadPID, filepath.Join(t.TempDir(), "dead.log")); err != nil {
		t.Fatal(err)
	}
	options := Options{CrashGrace: durationValue(0)}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, _ := opened.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusReady || detail.Task.FailureCount != 1 || detail.Runs[0].Status != model.RunStatusCrashed {
		t.Fatalf("dead worker was not recovered: %#v", detail)
	}
}

func TestDispatcherAutoSpecifiesTriageWithInjectedPlanner(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "rough operational idea", Status: model.TaskStatusTriage})
	opened.Close()
	kinds := []orchestration.PlannerKind{}
	planner := func(_ context.Context, request orchestration.PlannerRequest) (any, error) {
		kinds = append(kinds, request.Kind)
		return map[string]any{"fanout": false, "rootTitle": "Audit backups", "rootBody": "Acceptance: record restore evidence.", "reason": "one specialist", "tasks": []any{}, "dependencies": []any{}}, nil
	}
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/taskcircuit", Once: true, AutoDecompose: boolValue(true), DecompositionPlanner: planner}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusTodo || !strings.Contains(detail.Task.Body, "Acceptance") || len(kinds) != 1 || kinds[0] != orchestration.PlannerDecompose {
		t.Fatalf("unexpected auto specification: %#v, kinds=%v", detail, kinds)
	}
}

func TestDispatcherRunsClineThroughGoCLIBridge(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildTaskCircuit(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "cline-worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Cline bridge", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
"$TASKCIRCUIT_CLI" show "$TASKCIRCUIT_TASK_ID" >/dev/null
"$TASKCIRCUIT_CLI" heartbeat "$TASKCIRCUIT_TASK_ID" --note "running" >/dev/null
"$TASKCIRCUIT_CLI" comment "$TASKCIRCUIT_TASK_ID" "Cline used the Go CLI bridge" --author cline >/dev/null
"$TASKCIRCUIT_CLI" complete "$TASKCIRCUIT_TASK_ID" --summary "completed through Go CLI" --metadata '{"verification":["go-cli"]}' >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "TASKCIRCUIT_CLINE_BIN" {
			return fixture
		}
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusDone || len(detail.Comments) != 1 || detail.Runs[0].Summary == nil || *detail.Runs[0].Summary != "completed through Go CLI" {
		t.Fatalf("unexpected Cline result: %#v", detail)
	}
}

func TestDispatcherResumesGoalUntilTerminalCall(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildTaskCircuit(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	workspacePath := t.TempDir()
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "goal", Body: "Acceptance: finish second turn", Assignee: &assignee, Runtime: model.RuntimeCodex, Workspace: &workspacePath, GoalMode: true, GoalMaxTurns: 3})
	opened.Close()
	fixture := executableFixture(t, `
marker="$TASKCIRCUIT_WORKSPACE/.goal-turn"
if [ ! -f "$marker" ]; then
  touch "$marker"
  printf '%s\n' '{"thread_id":"session-1"}'
else
  "$TASKCIRCUIT_CLI" complete "$TASKCIRCUIT_TASK_ID" --summary "goal complete" --metadata '{"turns":2}' >/dev/null
fi`)
	judged := 0
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "TASKCIRCUIT_CODEX_BIN" {
			return fixture
		}
		return ""
	}, GoalJudge: func(_ context.Context, _ model.TaskDetail, turn int, _ string) (orchestration.GoalJudgment, error) {
		judged++
		return orchestration.GoalJudgment{Complete: false, Reason: "one gap", NextPrompt: "finish the gap"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	spawned := 0
	for _, event := range detail.Events {
		if event.Kind == "spawned" {
			spawned++
		}
	}
	if detail.Task.Status != model.TaskStatusDone || judged != 1 || spawned != 2 {
		t.Fatalf("unexpected goal result: status=%s judged=%d spawned=%d detail=%#v", detail.Task.Status, judged, spawned, detail)
	}
}
