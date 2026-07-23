package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestExecuteTurnRecordsSpawnLogAndSession(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(":memory:", "default", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "execute", Assignee: &assignee, Runtime: model.RuntimeCodex})
	claim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	logPath := filepath.Join(t.TempDir(), "run.log")
	result := ExecuteTurn(ctx, RunnerCommand{Command: "/bin/sh", Args: []string{"-c", `printf '%s\n' '{"thread_id":"session-1"}'`}, CWD: t.TempDir()}, opened,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(), logPath, nil)
	if result.Code != 0 || result.SessionID != "session-1" || !strings.Contains(result.Output, "session-1") {
		t.Fatalf("unexpected execution: %#v", result)
	}
	detail, _ := opened.GetTask(ctx, task.Task.ID)
	if len(detail.Runs) != 1 || detail.Runs[0].PID == nil || detail.Runs[0].LogPath == nil || *detail.Runs[0].LogPath != logPath {
		t.Fatalf("spawn was not recorded: %#v", detail.Runs)
	}
	contents, _ := os.ReadFile(logPath)
	if !strings.Contains(string(contents), "session-1") {
		t.Fatalf("log was not written: %s", contents)
	}
}

func TestExecuteTurnDoesNotStartAfterDeferredReclaim(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "cancel before spawn", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	if _, err := opened.DeferReclaim(ctx, claim.Run.ID, 30, "operator request"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	gateCalls := 0
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "/bin/sh", Args: []string{"-c", "touch \"$1\"", "sh", marker}, CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "run.log"), nil, func(context.Context) (TurnStartCompensation, error) {
			gateCalls++
			return nil, nil
		})
	if !errors.Is(result.SpawnError, store.ErrRunTerminationPending) {
		t.Fatalf("spawn error = %v, want ErrRunTerminationPending", result.SpawnError)
	}
	if gateCalls != 0 {
		t.Fatalf("start gate calls = %d, want 0 after deferred reclaim", gateCalls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker command started despite reclaim request: %v", err)
	}
}

func TestExecuteTurnDoesNotInvokeStartGateWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "cancel before start gate", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	gateCalls := 0
	cancel()
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "/bin/sh", Args: []string{"-c", "touch \"$1\"", "sh", marker}, CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "run.log"), nil, func(context.Context) (TurnStartCompensation, error) {
			gateCalls++
			return nil, nil
		})
	if !result.Canceled || !errors.Is(result.SpawnError, context.Canceled) {
		t.Fatalf("execution = %#v, want canceled before spawn", result)
	}
	if gateCalls != 0 {
		t.Fatalf("start gate calls = %d, want 0", gateCalls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker command started despite canceled context: %v", err)
	}
}

func TestExecuteTurnRejectsMissingExecutableBeforeStartGate(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "compensate failed spawn", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	gateCalls, compensationCalls := 0, 0
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: filepath.Join(t.TempDir(), "missing-worker"), CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "run.log"), nil, func(context.Context) (TurnStartCompensation, error) {
			gateCalls++
			return func(compensationCtx context.Context) error {
				compensationCalls++
				if err := compensationCtx.Err(); err != nil {
					t.Fatalf("compensation context is already done: %v", err)
				}
				if _, ok := compensationCtx.Deadline(); !ok {
					t.Fatal("compensation context has no deadline")
				}
				return nil
			}, nil
		})
	if result.SpawnError == nil || !errors.Is(result.SpawnError, os.ErrNotExist) {
		t.Fatalf("spawn error = %v, want executable not found", result.SpawnError)
	}
	if gateCalls != 0 || compensationCalls != 0 {
		t.Fatalf("gate calls = %d, compensation calls = %d; want 0 each", gateCalls, compensationCalls)
	}
}

func TestExecuteTurnCompensatesBeforeWorkerRelease(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "started worker", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	gateCalls, compensationCalls := 0, 0
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: "invalid-token"}, NewProcessSet(),
		filepath.Join(t.TempDir(), "run.log"), nil, func(context.Context) (TurnStartCompensation, error) {
			gateCalls++
			return func(context.Context) error {
				compensationCalls++
				return nil
			}, nil
		})
	if result.SpawnError == nil || !strings.Contains(result.SpawnError.Error(), "record worker spawn") {
		t.Fatalf("spawn error = %v, want spawn-record failure after process start", result.SpawnError)
	}
	if gateCalls != 1 || compensationCalls != 1 {
		t.Fatalf("gate calls = %d, compensation calls = %d; want 1 each", gateCalls, compensationCalls)
	}
}

func TestExecuteTurnEnforcesRuntimeLimit(t *testing.T) {
	ctx := context.Background()
	opened, _ := store.Open(":memory:", "default", t.TempDir())
	defer opened.Close()
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "hang", Assignee: &assignee, Runtime: model.RuntimeCodex})
	claim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	limit := 100 * time.Millisecond
	result := ExecuteTurn(ctx, RunnerCommand{Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, CWD: t.TempDir()}, opened,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(), filepath.Join(t.TempDir(), "run.log"), &limit)
	if !result.TimedOut {
		t.Fatalf("expected timeout: %#v", result)
	}
}

func TestScopedApprovalBrokerAllowsOnlyLifecycleBridge(t *testing.T) {
	directory := t.TempDir()
	prefix := "'/tmp/autogora'"
	broker := &approvalBroker{policy: ToolApproval{Directory: directory, CommandPrefix: prefix}, handled: map[string]bool{}}
	requests := map[string]map[string]any{
		"a.request.1.json": {"toolName": "read_file", "input": map[string]any{"path": "README.md"}},
		"b.request.2.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " complete $AUTOGORA_TASK_ID --summary ok"}},
		"c.request.3.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " delete t_other"}},
		"d.request.4.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " show $AUTOGORA_TASK_ID; rm -rf /tmp/x"}},
	}
	for name, request := range requests {
		value, _ := json.Marshal(request)
		if err := os.WriteFile(filepath.Join(directory, name), value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	broker.sweep()
	for name, want := range map[string]bool{"a.decision.1.json": true, "b.decision.2.json": true, "c.decision.3.json": false, "d.decision.4.json": false} {
		contents, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		var decision struct {
			Approved bool `json:"approved"`
		}
		if err := json.Unmarshal(contents, &decision); err != nil || decision.Approved != want {
			t.Fatalf("decision %s = %#v, %v; want %v", name, decision, err, want)
		}
	}
}
