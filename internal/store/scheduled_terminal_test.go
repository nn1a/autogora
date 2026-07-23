package store

import (
	"context"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestAdministrativeTerminalTransitionsClearSchedule(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	tests := []struct {
		name   string
		apply  func(string) (model.TaskDetail, error)
		status model.TaskStatus
	}{
		{name: "complete", status: model.TaskStatusDone, apply: func(taskID string) (model.TaskDetail, error) {
			return opened.CompleteTask(ctx, taskID, CompletionInput{Summary: "done"})
		}},
		{name: "block", status: model.TaskStatusBlocked, apply: func(taskID string) (model.TaskDetail, error) {
			return opened.BlockTask(ctx, taskID, BlockInput{Reason: "needs review", Kind: model.BlockKindNeedsInput})
		}},
		{name: "archive", status: model.TaskStatusArchived, apply: func(taskID string) (model.TaskDetail, error) {
			return opened.ArchiveTask(ctx, taskID)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created, err := opened.CreateTask(ctx, CreateTaskInput{
				Title: "scheduled " + test.name, Status: model.TaskStatusScheduled, ScheduledAt: &future,
			})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := test.apply(created.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Task.Status != test.status || updated.Task.ScheduledAt != nil {
				t.Fatalf("terminal task retained schedule: %#v", updated.Task)
			}
		})
	}
}
