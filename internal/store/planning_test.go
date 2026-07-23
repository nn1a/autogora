package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

func openPlanningTestStore(t *testing.T, path string) *Store {
	t.Helper()
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	return opened
}

func createPlanningTask(t *testing.T, opened *Store, title string) model.TaskDetail {
	t.Helper()
	task, err := opened.CreateTask(context.Background(), CreateTaskInput{
		Title: title, Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func requirePlanningClaim(t *testing.T, decision AutoDecomposeDecision, attempt int) AutoDecomposeClaim {
	t.Helper()
	if decision.Eligibility != AutoDecomposeClaimed || decision.Claim == nil ||
		decision.Claim.Attempt != attempt {
		t.Fatalf("planning decision = %+v, want claimed attempt %d", decision, attempt)
	}
	return *decision.Claim
}

func TestNormalizeAutoDecomposeFailurePreservesUTF8AtLimit(t *testing.T) {
	failure, _, err := normalizeAutoDecomposeFailure(
		AutoDecomposeClaim{TaskID: "task-1", Token: "claim-1"},
		strings.Repeat("가", 1000),
		time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(failure) > 2000 {
		t.Fatalf("failure length = %d, want at most 2000 bytes", len(failure))
	}
	if !utf8.ValidString(failure) {
		t.Fatalf("failure is not valid UTF-8: %q", failure)
	}
}

func TestAutoDecomposeBackoffPersistsAcrossStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened := openPlanningTestStore(t, path)
	task := createPlanningTask(t, opened, "Planner retry survives restart")
	current := time.Date(2040, 1, 2, 3, 4, 5, 0, time.UTC)

	decision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current)
	if err != nil {
		t.Fatal(err)
	}
	claim := requirePlanningClaim(t, decision, 1)
	failure, err := opened.FailAutoDecompose(ctx, claim, "planner unavailable", current)
	if err != nil {
		t.Fatal(err)
	}
	if failure.Eligibility != AutoDecomposeBackoff || failure.RetryAt == nil {
		t.Fatalf("failure = %+v, want persisted backoff", failure)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openPlanningTestStore(t, path)
	beforeRetry, err := restarted.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if beforeRetry.Eligibility != AutoDecomposeBackoff || beforeRetry.Attempts != 1 ||
		beforeRetry.RetryAt == nil || *beforeRetry.RetryAt != *failure.RetryAt {
		t.Fatalf("restart decision = %+v, want original retry boundary %+v", beforeRetry, failure)
	}
	afterRetry, err := restarted.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, afterRetry, 2)
}

func TestAutoDecomposeClaimSerializesConcurrentSchedulers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	first := openPlanningTestStore(t, path)
	task := createPlanningTask(t, first, "Only one scheduler may plan")
	second := openPlanningTestStore(t, path)
	current := time.Date(2040, 2, 3, 4, 5, 6, 0, time.UTC)

	start := make(chan struct{})
	results := make(chan AutoDecomposeDecision, 2)
	errs := make(chan error, 2)
	var schedulers sync.WaitGroup
	for _, opened := range []*Store{first, second} {
		schedulers.Add(1)
		go func(opened *Store) {
			defer schedulers.Done()
			<-start
			result, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current)
			results <- result
			errs <- err
		}(opened)
	}
	close(start)
	schedulers.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed, busy := 0, 0
	for result := range results {
		switch result.Eligibility {
		case AutoDecomposeClaimed:
			claimed++
		case AutoDecomposeBusy:
			busy++
			if result.RetryAt == nil {
				t.Fatalf("busy decision lacks lease boundary: %+v", result)
			}
		default:
			t.Fatalf("unexpected concurrent decision: %+v", result)
		}
	}
	if claimed != 1 || busy != 1 {
		t.Fatalf("concurrent outcomes claimed=%d busy=%d, want 1 each", claimed, busy)
	}
	state, err := first.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Attempts != 1 || state.ClaimToken == nil {
		t.Fatalf("durable concurrent state = %+v", state)
	}
}

func TestAutoDecomposeAttemptQuotaExhaustsDeterministically(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	task := createPlanningTask(t, opened, "Bound planner retries")
	current := time.Date(2040, 3, 4, 5, 6, 7, 0, time.UTC)

	firstDecision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 2, time.Minute, current)
	if err != nil {
		t.Fatal(err)
	}
	first := requirePlanningClaim(t, firstDecision, 1)
	firstFailure, err := opened.FailAutoDecompose(ctx, first, "first failure", current)
	if err != nil {
		t.Fatal(err)
	}
	if firstFailure.Eligibility != AutoDecomposeBackoff {
		t.Fatalf("first failure = %+v", firstFailure)
	}

	secondDecision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, 2, time.Minute, current.Add(AutoDecomposeRetryDelay(1)),
	)
	if err != nil {
		t.Fatal(err)
	}
	second := requirePlanningClaim(t, secondDecision, 2)
	secondFailure, err := opened.FailAutoDecompose(ctx, second, "second failure", current.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if secondFailure.Eligibility != AutoDecomposeExhausted || secondFailure.RetryAt != nil {
		t.Fatalf("second failure = %+v, want exhaustion without retry", secondFailure)
	}

	exhausted, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 2, time.Minute, current.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Eligibility != AutoDecomposeExhausted || exhausted.Claim != nil ||
		exhausted.Attempts != 2 || exhausted.MaxAttempts != 2 {
		t.Fatalf("exhausted decision = %+v", exhausted)
	}
	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	failedEvents, exhaustedEvents := 0, 0
	for _, event := range detail.Events {
		switch event.Kind {
		case "auto_decompose_failed":
			failedEvents++
		case "auto_decompose_exhausted":
			exhaustedEvents++
		}
	}
	if failedEvents != 2 || exhaustedEvents != 1 || detail.Task.Status != model.TaskStatusTriage {
		t.Fatalf(
			"planner exhaustion audit failed=%d exhausted=%d task=%+v",
			failedEvents, exhaustedEvents, detail.Task,
		)
	}
}

func TestExpiredAutoDecomposeClaimConsumesFinalAttempt(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	task := createPlanningTask(t, opened, "Crashed planner stays bounded")
	current := time.Date(2040, 3, 5, 6, 7, 8, 0, time.UTC)

	decision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 1, time.Second, current)
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, decision, 1)

	exhausted, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, 1, time.Second, current.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Eligibility != AutoDecomposeExhausted || exhausted.Claim != nil {
		t.Fatalf("expired final claim decision = %+v", exhausted)
	}
	state, err := opened.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ClaimToken != nil || state.ClaimExpiresAt != nil ||
		state.LastError == nil || *state.LastError != "planner claim expired before completion" {
		t.Fatalf("expired claim state = %+v", state)
	}
	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	expiredEvents, exhaustedEvents := 0, 0
	for _, event := range detail.Events {
		switch event.Kind {
		case "auto_decompose_claim_expired":
			expiredEvents++
		case "auto_decompose_exhausted":
			exhaustedEvents++
		}
	}
	if expiredEvents != 1 || exhaustedEvents != 1 {
		t.Fatalf("expired claim audit events expired=%d exhausted=%d", expiredEvents, exhaustedEvents)
	}
}

func TestAutoDecomposeExhaustionRecoversAfterTaskEdit(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	task := createPlanningTask(t, opened, "Original rough idea")
	current := time.Date(2040, 4, 5, 6, 7, 8, 0, time.UTC)

	decision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 1, time.Minute, current)
	if err != nil {
		t.Fatal(err)
	}
	claim := requirePlanningClaim(t, decision, 1)
	if _, err := opened.FailAutoDecompose(ctx, claim, "bad initial prompt", current); err != nil {
		t.Fatal(err)
	}

	revisedTitle := "Revised rough idea"
	if _, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{Title: &revisedTitle}); err != nil {
		t.Fatal(err)
	}
	recovered, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 1, time.Minute, current.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	recoveredClaim := requirePlanningClaim(t, recovered, 1)
	if recoveredClaim.TaskUpdatedAt == claim.TaskUpdatedAt {
		t.Fatal("edited task did not start a fresh planning version")
	}

	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	resetEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "auto_decompose_retry_reset" {
			resetEvents++
		}
	}
	if resetEvents != 1 {
		t.Fatalf("retry reset events = %d, want 1", resetEvents)
	}
}

func TestAutoDecomposeLateFailureDoesNotPenalizeEditedTriageTask(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	task := createPlanningTask(t, opened, "Edit while Planner runs")
	current := time.Date(2040, 4, 6, 7, 8, 9, 0, time.UTC)

	decision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current)
	if err != nil {
		t.Fatal(err)
	}
	claim := requirePlanningClaim(t, decision, 1)
	revisedBody := "Use the revised acceptance criteria."
	if _, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{Body: &revisedBody}); err != nil {
		t.Fatal(err)
	}
	late, err := opened.FailAutoDecompose(ctx, claim, "stale planner failure", current.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if late.Eligibility != AutoDecomposeInvalidated {
		t.Fatalf("late planner failure = %+v, want invalidated", late)
	}
	state, err := opened.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("stale claim state remained after user edit: %+v", state)
	}
	recovered, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 3, time.Minute, current.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, recovered, 1)
}

func TestManualSpecifyBypassesExhaustedAutoDecomposeState(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	task := createPlanningTask(t, opened, "Manual recovery remains available")
	current := time.Date(2040, 5, 6, 7, 8, 9, 0, time.UTC)

	decision, err := opened.ClaimAutoDecompose(ctx, task.Task.ID, 1, time.Minute, current)
	if err != nil {
		t.Fatal(err)
	}
	claim := requirePlanningClaim(t, decision, 1)
	if _, err := opened.FailAutoDecompose(ctx, claim, "automatic planner failed", current); err != nil {
		t.Fatal(err)
	}
	specified, err := opened.SpecifyTask(
		ctx, task.Task.ID, "Human specification", "Acceptance: manual recovery works.", "human",
	)
	if err != nil {
		t.Fatal(err)
	}
	if specified.Task.Status == model.TaskStatusTriage ||
		specified.Task.Title != "Human specification" {
		t.Fatalf("manual specification was constrained by scheduler state: %+v", specified.Task)
	}
}
