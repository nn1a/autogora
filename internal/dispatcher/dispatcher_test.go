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

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func boolValue(value bool) *bool                       { return &value }
func durationValue(value time.Duration) *time.Duration { return &value }

func testManager(t *testing.T) (*boards.Manager, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
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

func buildAutogora(t *testing.T) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	repository := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	path := filepath.Join(t.TempDir(), "autogora")
	command := exec.Command("go", "build", "-o", path, "./cmd/autogora")
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Autogora: %v\n%s", err, output)
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
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true, MaxWorkers: 1,
		RateLimitCooldown: durationValue(0), AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CODEX_BIN" {
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
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "rough operational idea", Status: model.TaskStatusTriage})
	opened.Close()
	kinds := []orchestration.PlannerKind{}
	planner := func(_ context.Context, request orchestration.PlannerRequest) (any, error) {
		kinds = append(kinds, request.Kind)
		return map[string]any{"fanout": false, "rootTitle": "Audit backups", "rootBody": "Acceptance: record restore evidence.", "reason": "one specialist", "tasks": []any{}, "dependencies": []any{}}, nil
	}
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "automatic specification completed" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	profile := orchestration.ProfileRoute{Name: "operator", Runtime: model.RuntimeCline}
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(true), DecompositionPlanner: planner,
		DefaultProfile: &profile, Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusDone || detail.Task.Assignee == nil || *detail.Task.Assignee != "operator" || !strings.Contains(detail.Task.Body, "Acceptance") || len(kinds) != 1 || kinds[0] != orchestration.PlannerDecompose {
		t.Fatalf("unexpected auto specification: %#v, kinds=%v", detail, kinds)
	}
}

func TestOneShotDispatcherReclaimsHungWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	manager, dbPath := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "hung-worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "hung", Assignee: &assignee, Runtime: model.RuntimeCodex})
	opened.Close()
	fixture := executableFixture(t, `while :; do sleep 1; done`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true, MaxWorkers: 1,
		Interval: 250 * time.Millisecond, ClaimTTLSeconds: 1, AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CODEX_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(context.Background(), "default")
	defer check.Close()
	detail, _ := check.GetTask(context.Background(), task.Task.ID)
	if detail.Task.Status == model.TaskStatusRunning || detail.Task.CurrentRunID != nil || len(detail.Runs) != 1 || detail.Runs[0].Status == model.RunStatusRunning {
		t.Fatalf("one-shot watchdog left a stranded run: %#v", detail)
	}
}

func TestTargetedDispatcherRunsOnlyRequestedTask(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	first, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first", Assignee: &assignee, Runtime: model.RuntimeCline, Priority: 100})
	second, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "target complete" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: second.Task.ID, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CLINE_BIN" {
			return fixture
		}
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	firstDetail, _ := check.GetTask(ctx, first.Task.ID)
	secondDetail, _ := check.GetTask(ctx, second.Task.ID)
	if firstDetail.Task.Status != model.TaskStatusReady || secondDetail.Task.Status != model.TaskStatusDone {
		t.Fatalf("target dispatch changed the wrong task: first=%s second=%s", firstDetail.Task.Status, secondDetail.Task.Status)
	}
}

func TestWritableSharedDirectoryRunsAreSerializedWithoutRetryPenalty(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "writer"
	directory := t.TempDir()
	first, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first writer", Assignee: &assignee, Runtime: model.RuntimeCline, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	second, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second writer", Assignee: &assignee, Runtime: model.RuntimeCline, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	opened.Close()
	fixture := executableFixture(t, `
sleep 1
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "serialized writer completed" >/dev/null`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, MaxWorkers: 2, Interval: 250 * time.Millisecond,
		AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	firstDetail, _ := check.GetTask(ctx, first.Task.ID)
	secondDetail, _ := check.GetTask(ctx, second.Task.ID)
	done, waiting := firstDetail, secondDetail
	if secondDetail.Task.Status == model.TaskStatusDone {
		done, waiting = secondDetail, firstDetail
	}
	if done.Task.Status != model.TaskStatusDone || (waiting.Task.Status != model.TaskStatusReady && waiting.Task.Status != model.TaskStatusScheduled) || waiting.Task.FailureCount != 0 {
		t.Fatalf("shared writers were not serialized cleanly: first=%#v second=%#v", firstDetail, secondDetail)
	}
}

func TestDispatcherRunsClineThroughGoCLIBridge(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "cline-worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Cline bridge", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" show "$AUTOGORA_TASK_ID" >/dev/null
"$AUTOGORA_CLI" heartbeat "$AUTOGORA_TASK_ID" --note "running" >/dev/null
"$AUTOGORA_CLI" comment "$AUTOGORA_TASK_ID" "Cline used the Go CLI bridge" --author cline >/dev/null
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "completed through Go CLI" --metadata '{"verification":["go-cli"]}' >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CLINE_BIN" {
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
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	workspacePath := t.TempDir()
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "goal", Body: "Acceptance: finish second turn", Assignee: &assignee, Runtime: model.RuntimeCodex, Workspace: &workspacePath, GoalMode: true, GoalMaxTurns: 3})
	opened.Close()
	fixture := executableFixture(t, `
marker="$AUTOGORA_WORKSPACE/.goal-turn"
if [ ! -f "$marker" ]; then
  touch "$marker"
  printf '%s\n' '{"thread_id":"session-1"}'
else
  "$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "goal complete" --metadata '{"turns":2}' >/dev/null
fi`)
	judged := 0
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CODEX_BIN" {
			return fixture
		}
		return ""
	}, GoalJudge: func(_ context.Context, _ model.TaskDetail, turn int, _ string) (orchestration.GoalJudgment, error) {
		judged++
		if judged == 2 {
			return orchestration.GoalJudgment{Complete: true, Reason: "acceptance verified"}, nil
		}
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
	if detail.Task.Status != model.TaskStatusDone || judged != 2 || spawned != 2 {
		t.Fatalf("unexpected goal result: status=%s judged=%d spawned=%d detail=%#v", detail.Task.Status, judged, spawned, detail)
	}
}
