package store

import (
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestAdministrativeTerminalTransitionsRejectStaleTaskVersion(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	tests := []struct {
		name  string
		apply func(string, *string) error
	}{
		{name: "complete", apply: func(taskID string, expected *string) error {
			_, err := opened.CompleteTask(ctx, taskID, CompletionInput{Summary: "done", ExpectedUpdatedAt: expected})
			return err
		}},
		{name: "block", apply: func(taskID string, expected *string) error {
			_, err := opened.BlockTask(ctx, taskID, BlockInput{Reason: "needs review", Kind: model.BlockKindNeedsInput, ExpectedUpdatedAt: expected})
			return err
		}},
		{name: "archive", apply: func(taskID string, expected *string) error {
			_, err := opened.ArchiveTaskWithVersion(ctx, taskID, expected)
			return err
		}},
		{name: "delete", apply: func(taskID string, expected *string) error {
			return opened.DeleteTaskWithVersion(ctx, taskID, expected)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created, err := opened.CreateTask(ctx, CreateTaskInput{Title: "original " + test.name})
			if err != nil {
				t.Fatal(err)
			}
			stale := created.Task.UpdatedAt
			const latestVersion = "2099-01-01T00:00:00.000Z"
			if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET title = ?, updated_at = ? WHERE id = ?", "latest "+test.name, latestVersion, created.Task.ID); err != nil {
				t.Fatal(err)
			}

			if err := test.apply(created.Task.ID, &stale); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("stale %s error = %v, want conflict", test.name, err)
			}
			loaded, err := opened.GetTask(ctx, created.Task.ID)
			if err != nil {
				t.Fatalf("stale %s removed or hid the task: %v", test.name, err)
			}
			if loaded.Task.Title != "latest "+test.name || loaded.Task.Status != model.TaskStatusTodo {
				t.Fatalf("stale %s changed latest task: %#v", test.name, loaded.Task)
			}
		})
	}
}

func TestBulkMutationReturnsPartialOptimisticConflicts(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	fresh, err := opened.CreateTask(ctx, CreateTaskInput{Title: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := opened.CreateTask(ctx, CreateTaskInput{Title: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		fresh.Task.ID: fresh.Task.UpdatedAt,
		stale.Task.ID: stale.Task.UpdatedAt,
	}
	if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET title = ?, updated_at = ? WHERE id = ?", "newer stale", "2099-01-01T00:00:00.000Z", stale.Task.ID); err != nil {
		t.Fatal(err)
	}
	status := model.TaskStatusReview
	result := opened.BulkMutate(ctx, []string{fresh.Task.ID, stale.Task.ID}, BulkMutation{
		Status: &status, ExpectedUpdatedAt: expected,
	})

	if len(result.OK) != 1 || result.OK[0].ID != fresh.Task.ID {
		t.Fatalf("bulk successes = %#v, want only %s", result.OK, fresh.Task.ID)
	}
	if len(result.Errors) != 1 || result.Errors[0].ID != stale.Task.ID || !strings.Contains(result.Errors[0].Error, "conflict") {
		t.Fatalf("bulk errors = %#v, want stale conflict for %s", result.Errors, stale.Task.ID)
	}
	freshLoaded, err := opened.GetTask(ctx, fresh.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	staleLoaded, err := opened.GetTask(ctx, stale.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freshLoaded.Task.Status != model.TaskStatusReview || staleLoaded.Task.Status != model.TaskStatusTodo {
		t.Fatalf("partial bulk result not preserved: fresh=%s stale=%s", freshLoaded.Task.Status, staleLoaded.Task.Status)
	}
}

func TestApplyTaskGraphRejectsStaleRootVersionAtomically(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	root, err := opened.CreateTask(ctx, CreateTaskInput{Title: "rough graph", Status: model.TaskStatusTriage})
	if err != nil {
		t.Fatal(err)
	}
	stale := root.Task.UpdatedAt
	if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET title = ?, updated_at = ? WHERE id = ?", "latest graph", "2099-01-01T00:00:00.000Z", root.Task.ID); err != nil {
		t.Fatal(err)
	}
	worker := "worker"
	_, err = opened.ApplyTaskGraph(ctx, TaskGraphInput{
		RootTaskID: root.Task.ID, ExpectedUpdatedAt: &stale,
		RootTitle: "stale root", RootBody: "stale body",
		FinalizerAssignee: worker, FinalizerRuntime: model.RuntimeCodex,
		Nodes: []TaskGraphNode{{Key: "child", Title: "child", Body: "work", Assignee: worker, Runtime: model.RuntimeCodex}},
	})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("stale graph error = %v, want conflict", err)
	}
	latest, err := opened.GetTask(ctx, root.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Task.Title != "latest graph" || len(latest.Subtasks) != 0 {
		t.Fatalf("stale graph was not atomic: %#v", latest)
	}
}
