package runcontrol

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

func TestTerminateRunPersistsIntentAndReclaimsMissingProcess(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "taskcircuit.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "terminate", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	termination, err := TerminateTaskRun(ctx, opened, task.Task.ID, "administrative edit")
	if err != nil {
		t.Fatal(err)
	}
	if termination.RunID != claim.Run.ID || termination.Signaled || termination.Pending || termination.Task.Task.Status != model.TaskStatusReady {
		t.Fatalf("unexpected termination: %+v", termination)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != model.RunStatusReclaimed || inspection.Run.Error == nil || *inspection.Run.Error != "administrative edit" {
		t.Fatalf("termination intent was not preserved: %+v", inspection.Run)
	}
}
