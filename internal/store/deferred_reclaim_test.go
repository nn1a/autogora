package store

import (
	"context"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestDeferredReclaimSurvivesLaterEventsAndPreservesOutcome(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "durable termination", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	if _, err := opened.DeferReclaim(ctx, claim.Run.ID, 30, "stale heartbeat"); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.AddComment(ctx, task.Task.ID, "operator", "termination is still pending"); err != nil {
		t.Fatal(err)
	}
	intent, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent == nil || intent.Reason != "stale heartbeat" || intent.Outcome != model.RunStatusReclaimed || intent.CountFailure {
		t.Fatalf("reclaim intent = %#v", intent)
	}
	if expires, err := time.Parse(time.RFC3339Nano, intent.ExpiresAt); err != nil || !expires.After(time.Now()) {
		t.Fatalf("reclaim expiry = %q, %v", intent.ExpiresAt, err)
	}
	if _, err := opened.DeferTimedOutRun(ctx, claim.Run.ID, 45, "runtime limit"); err != nil {
		t.Fatal(err)
	}
	intent, err = opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || intent == nil || intent.Reason != "runtime limit" || intent.Outcome != model.RunStatusTimedOut || !intent.CountFailure {
		t.Fatalf("timeout intent = %#v, %v", intent, err)
	}
}

func TestFailRunDiscardsPendingTerminalRequest(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "failed completion", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteRun(ctx, scope, CompletionInput{Summary: "premature"}); err != nil {
		t.Fatal(err)
	}
	failed, err := opened.FailRun(ctx, scope, "worker exited nonzero", FailRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.TerminalRequests) != 0 || failed.Task.Status == model.TaskStatusRunning || failed.Task.CurrentRunID != nil {
		t.Fatalf("failed run retained terminal request: %#v", failed)
	}
}
