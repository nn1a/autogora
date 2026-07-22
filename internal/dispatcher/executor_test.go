package dispatcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
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
	prefix := "'/tmp/taskcircuit'"
	broker := &approvalBroker{policy: ToolApproval{Directory: directory, CommandPrefix: prefix}, handled: map[string]bool{}}
	requests := map[string]map[string]any{
		"a.request.1.json": {"toolName": "read_file", "input": map[string]any{"path": "README.md"}},
		"b.request.2.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " complete $KANBAN_TASK_ID --summary ok"}},
		"c.request.3.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " delete t_other"}},
		"d.request.4.json": {"toolName": "execute_command", "input": map[string]any{"command": prefix + " show $KANBAN_TASK_ID; rm -rf /tmp/x"}},
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
