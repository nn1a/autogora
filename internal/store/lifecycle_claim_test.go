package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestClaimTaskSearchesPastIneligibleDependencyPage(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "Unfinished prerequisite", Priority: 200})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < claimCandidatePageSize+5; index++ {
		_, err := opened.CreateTask(ctx, CreateTaskInput{
			Title:    fmt.Sprintf("Blocked candidate %03d", index),
			Assignee: stringValue("blocked-worker"),
			Runtime:  model.RuntimeCodex,
			Priority: 100,
			Parents:  []string{parent.Task.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE priority = 100"); err != nil {
		t.Fatal(err)
	}
	eligible, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "Eligible after blocked page", Assignee: stringValue("free-worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := opened.ClaimTask(ctx, ClaimOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Task.Task.ID != eligible.Task.ID {
		t.Fatalf("claim = %+v, want task %s after the first ineligible page", claim, eligible.Task.ID)
	}
	var waiting int
	if err := opened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE priority = 100 AND status = 'todo'").Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != claimCandidatePageSize+5 {
		t.Fatalf("dependency cleanup moved %d tasks to todo, want %d", waiting, claimCandidatePageSize+5)
	}
}

func TestClaimTaskSearchesAcrossMoreThanTwoAssigneeCapPages(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	busyAssignee := "busy-worker"
	anchor, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "Existing busy run", Assignee: &busyAssignee, Runtime: model.RuntimeCodex, Priority: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: anchor.Task.ID}); err != nil || claim == nil {
		t.Fatalf("claim busy anchor: %+v, %v", claim, err)
	}
	for index := 0; index < claimCandidatePageSize*2+25; index++ {
		_, err := opened.CreateTask(ctx, CreateTaskInput{
			Title: fmt.Sprintf("Assignee-capped candidate %03d", index), Assignee: &busyAssignee,
			Runtime: model.RuntimeCodex, Priority: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	eligible, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "Eligible after three pages", Assignee: stringValue("available-worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := opened.ClaimTask(ctx, ClaimOptions{MaxInProgressPerAssignee: 1})
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Task.Task.ID != eligible.Task.ID {
		t.Fatalf("claim = %+v, want task %s after more than 100 capped candidates", claim, eligible.Task.ID)
	}
}

func TestClaimTaskUsesIDAsDeterministicKeysetTieBreaker(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	ids := []string{"t_z_tie", "t_a_tie", "t_m_tie"}
	createdAt := "2026-07-23T00:00:00.000Z"
	for index, id := range ids {
		task, err := opened.CreateTask(ctx, CreateTaskInput{
			Title: fmt.Sprintf("Tied candidate %d", index), Assignee: stringValue("worker"),
			Runtime: model.RuntimeCodex, Priority: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := opened.db.ExecContext(ctx, "DELETE FROM task_events WHERE task_id = ?", task.Task.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET id = ?, created_at = ?, updated_at = ? WHERE id = ?", id, createdAt, createdAt, task.Task.ID); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"t_a_tie", "t_m_tie", "t_z_tie"}
	for index, id := range want {
		claim, err := opened.ClaimTask(ctx, ClaimOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if claim == nil || claim.Task.Task.ID != id {
			t.Fatalf("claim %d = %+v, want %s", index, claim, id)
		}
	}
}

func TestClaimTaskDoesNotInferActivePullRequestFromComments(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "ordinary URL", body: "Background: https://example.com/design-notes"},
		{name: "GitHub pull request URL", body: "Related discussion: https://github.com/acme/widgets/pull/42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			opened, err := Open(filepath.Join(t.TempDir(), "comments.db"), "default", "")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			task, err := opened.CreateTask(ctx, CreateTaskInput{
				Title: "Commented task", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := opened.AddComment(ctx, task.Task.ID, "human", test.body); err != nil {
				t.Fatal(err)
			}
			claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
			if err != nil {
				t.Fatal(err)
			}
			if claim == nil || claim.Task.Task.ID != task.Task.ID {
				t.Fatalf("comment unexpectedly guarded task claim: %+v", claim)
			}
		})
	}
}

func TestClaimTaskAllowsExplicitRerunAfterRecentSuccess(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "rerun explicitly", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	first, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v, %v", first, err)
	}
	if _, err := opened.CompleteRun(ctx, RunScope{RunID: first.Run.ID, ClaimToken: first.ClaimToken}, CompletionInput{Summary: "first success"}); err != nil {
		t.Fatal(err)
	}
	ready := model.TaskStatusReady
	if _, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{Status: &ready}); err != nil {
		t.Fatal(err)
	}
	second, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || second == nil {
		t.Fatalf("explicit rerun was guarded: %+v, %v", second, err)
	}
}

func TestClaimTaskStillGuardsRecentSuccessWithoutExplicitRerun(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "guard duplicate", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	first, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v, %v", first, err)
	}
	if _, err := opened.CompleteRun(ctx, RunScope{RunID: first.Run.ID, ClaimToken: first.ClaimToken}, CompletionInput{Summary: "first success"}); err != nil {
		t.Fatal(err)
	}
	// Simulate an inconsistent status repair without a deliberate lifecycle
	// event. The guard should still prevent an immediate duplicate run.
	if _, err := opened.db.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE id = ?", task.Task.ID); err != nil {
		t.Fatal(err)
	}
	duplicate, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate != nil {
		t.Fatalf("recent success was claimed without an explicit rerun: %+v", duplicate)
	}
}
