package store

import (
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestRunningTaskKeepsItsDependencySnapshotStable(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	first, err := opened.CreateTask(ctx, CreateTaskInput{Title: "First prerequisite", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteTask(ctx, first.Task.ID, CompletionInput{Summary: "first handoff"}); err != nil {
		t.Fatal(err)
	}
	second, err := opened.CreateTask(ctx, CreateTaskInput{Title: "Second prerequisite", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteTask(ctx, second.Task.ID, CompletionInput{Summary: "second handoff"}); err != nil {
		t.Fatal(err)
	}
	dependent, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "Stable dependent", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Parents: []string{first.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: dependent.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim dependent: %#v %v", claim, err)
	}

	if _, err := opened.LinkTasks(ctx, first.Task.ID, dependent.Task.ID); err != nil {
		t.Fatalf("idempotent existing link failed: %v", err)
	}
	if _, err := opened.LinkTasks(ctx, second.Task.ID, dependent.Task.ID); err == nil || !strings.Contains(err.Error(), "dependent task is running") {
		t.Fatalf("new dependency changed a running task: %v", err)
	}
	if _, err := opened.UnlinkTasks(ctx, first.Task.ID, dependent.Task.ID); err == nil || !strings.Contains(err.Error(), "dependent task is running") {
		t.Fatalf("dependency was removed from a running task: %v", err)
	}
	if err := opened.DeleteTask(ctx, first.Task.ID); err == nil || !strings.Contains(err.Error(), "dependent task") {
		t.Fatalf("prerequisite of a running task was deleted: %v", err)
	}

	handoffs, err := opened.ListPrerequisiteHandoffs(ctx, dependent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].PrerequisiteID != first.Task.ID {
		t.Fatalf("running dependency snapshot changed: %#v", handoffs)
	}
	if _, err := opened.FailRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "test cleanup", FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
}
