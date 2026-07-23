package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func createVersionedClaimTask(t *testing.T, opened *Store, title string) model.TaskDetail {
	t.Helper()
	task, err := opened.CreateTask(context.Background(), CreateTaskInput{
		Title: title, Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestClaimTaskRejectsStaleExpectedVersionWithoutCreatingRun(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "dispatch-race.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	task := createVersionedClaimTask(t, opened, "stale dispatch")
	stale := task.Task.UpdatedAt
	latestTitle := "latest dispatch"
	latest, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{
		ExpectedUpdatedAt: &stale,
		Title:             &latestTitle,
	})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := opened.ClaimTask(ctx, ClaimOptions{
		TaskID: task.Task.ID, ExpectedUpdatedAt: &stale,
	})
	if claim != nil || err == nil || !strings.Contains(err.Error(), "conflict") || !strings.Contains(err.Error(), "refresh") {
		t.Fatalf("stale claim = %#v, %v; want refresh conflict", claim, err)
	}
	unchanged, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Task.Status != model.TaskStatusReady || len(unchanged.Runs) != 0 {
		t.Fatalf("stale claim changed task or created a run: %#v", unchanged)
	}

	claim, err = opened.ClaimTask(ctx, ClaimOptions{
		TaskID: task.Task.ID, ExpectedUpdatedAt: &latest.Task.UpdatedAt,
	})
	if err != nil || claim == nil {
		t.Fatalf("fresh claim = %#v, %v", claim, err)
	}
}

func TestExpectedVersionSerializesTaskEditAgainstClaim(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "dispatch-race.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	for iteration := 0; iteration < 16; iteration++ {
		task := createVersionedClaimTask(t, opened, "dispatch race")
		expected := task.Task.UpdatedAt
		title := "edited before claim"
		start := make(chan struct{})
		var wait sync.WaitGroup
		var updateErr, claimErr error
		var claim *model.ClaimedTask

		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, updateErr = opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{
				ExpectedUpdatedAt: &expected,
				Title:             &title,
			})
		}()
		go func() {
			defer wait.Done()
			<-start
			claim, claimErr = opened.ClaimTask(ctx, ClaimOptions{
				TaskID: task.Task.ID, ExpectedUpdatedAt: &expected,
			})
		}()
		close(start)
		wait.Wait()

		updateWon := updateErr == nil
		claimWon := claimErr == nil && claim != nil
		if updateWon == claimWon {
			t.Fatalf("iteration %d winners: update=%v claim=%v claimValue=%#v; errors: %v / %v",
				iteration, updateWon, claimWon, claim, updateErr, claimErr)
		}
		loserErr := updateErr
		if updateWon {
			loserErr = claimErr
		}
		if loserErr == nil || !strings.Contains(loserErr.Error(), "conflict") {
			t.Fatalf("iteration %d losing operation error = %v, want conflict", iteration, loserErr)
		}
	}
}
