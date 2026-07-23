package dispatcher

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func openDispatcherPlanningStore(t *testing.T, path string) *store.Store {
	t.Helper()
	opened, err := store.Open(path, "default", "")
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

func claimDispatcherPlanningTask(
	t *testing.T,
	opened *store.Store,
	claimTTL time.Duration,
) (model.TaskDetail, store.AutoDecomposeClaim) {
	t.Helper()
	ctx := context.Background()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "Long-running Planner", Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, store.AutoDecomposeMaxAttempts, claimTTL, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Claim == nil {
		t.Fatalf("planning claim = %+v", decision)
	}
	return task, *decision.Claim
}

func TestAutoDecomposeHeartbeatRetainsOwnershipAndStopsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened := openDispatcherPlanningStore(t, path)
	competitor := openDispatcherPlanningStore(t, path)
	claimTTL := time.Second
	task, claim := claimDispatcherPlanningTask(t, opened, claimTTL)

	leaseGuard := startAutoDecomposeLeaseGuard(
		context.Background(),
		opened,
		claim,
		claimTTL,
		Options{Now: time.Now},
	)
	defer leaseGuard.Stop()
	plannerCtx := leaseGuard.ctx
	originalExpiry, err := time.Parse(time.RFC3339Nano, claim.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var state *store.AutoDecomposeState
	for time.Now().Before(deadline) {
		state, err = competitor.GetAutoDecomposeState(context.Background(), task.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state != nil && state.ClaimExpiresAt != nil {
			expiry, parseErr := time.Parse(time.RFC3339Nano, *state.ClaimExpiresAt)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if expiry.After(originalExpiry) && time.Now().After(originalExpiry) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state == nil || state.ClaimExpiresAt == nil ||
		!time.Now().After(originalExpiry) {
		t.Fatalf("heartbeat did not carry claim beyond original expiry: %+v", state)
	}
	select {
	case <-plannerCtx.Done():
		t.Fatalf("live heartbeat canceled Planner: %v", plannerCtx.Err())
	default:
	}
	busy, err := competitor.ClaimAutoDecompose(
		context.Background(),
		task.Task.ID,
		store.AutoDecomposeMaxAttempts,
		claimTTL,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if busy.Eligibility != store.AutoDecomposeBusy || busy.Attempts != 1 {
		t.Fatalf("competitor decision = %+v, want busy first attempt", busy)
	}

	leaseGuard.Stop()
	state, err = competitor.GetAutoDecomposeState(context.Background(), task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ClaimExpiresAt == nil {
		t.Fatalf("stopped heartbeat lost durable crash boundary: %+v", state)
	}
	expiry, err := time.Parse(time.RFC3339Nano, *state.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	reclaimed, err := competitor.ClaimAutoDecompose(
		context.Background(),
		task.Task.ID,
		store.AutoDecomposeMaxAttempts,
		claimTTL,
		expiry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Eligibility != store.AutoDecomposeClaimed ||
		reclaimed.Claim == nil || reclaimed.Claim.Attempt != 2 {
		t.Fatalf("stopped heartbeat prevented crash recovery: %+v", reclaimed)
	}
}

func TestAutoDecomposeHeartbeatCancelsPlannerAfterTaskEdit(t *testing.T) {
	opened := openDispatcherPlanningStore(t, ":memory:")
	claimTTL := time.Second
	task, claim := claimDispatcherPlanningTask(t, opened, claimTTL)
	var logMu sync.Mutex
	logs := make([]string, 0, 1)
	leaseGuard := startAutoDecomposeLeaseGuard(
		context.Background(),
		opened,
		claim,
		claimTTL,
		Options{
			Now: time.Now,
			OnLog: func(message string) {
				logMu.Lock()
				defer logMu.Unlock()
				logs = append(logs, message)
			},
		},
	)
	defer leaseGuard.Stop()
	plannerCtx := leaseGuard.ctx
	revised := "Use the edited task version."
	if _, err := opened.UpdateTask(
		context.Background(),
		task.Task.ID,
		store.UpdateTaskInput{Body: &revised},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-plannerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("task edit did not cancel stale Planner ownership")
	}
	if plannerCtx.Err() != context.Canceled {
		t.Fatalf("Planner context error = %v", plannerCtx.Err())
	}
	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(joined, "auto-decompose claim lost") {
		t.Fatalf("ownership loss was not observable: %q", joined)
	}

	recovered, err := opened.ClaimAutoDecompose(
		context.Background(),
		task.Task.ID,
		store.AutoDecomposeMaxAttempts,
		claimTTL,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Eligibility != store.AutoDecomposeClaimed ||
		recovered.Claim == nil || recovered.Claim.Attempt != 1 ||
		recovered.Claim.TaskUpdatedAt == claim.TaskUpdatedAt {
		t.Fatalf("edited task did not start a fresh planning budget: %+v", recovered)
	}
}

func TestAutoDecomposeHeartbeatCancelsAtConfirmedExpiryAfterStoreErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened := openDispatcherPlanningStore(t, path)
	competitor := openDispatcherPlanningStore(t, path)
	claimTTL := time.Second
	task, claim := claimDispatcherPlanningTask(t, opened, claimTTL)
	var logMu sync.Mutex
	logs := make([]string, 0, 2)
	started := time.Now()
	leaseGuard := startAutoDecomposeLeaseGuard(
		context.Background(),
		opened,
		claim,
		claimTTL,
		Options{
			Now: time.Now,
			OnLog: func(message string) {
				logMu.Lock()
				defer logMu.Unlock()
				logs = append(logs, message)
			},
		},
	)
	defer leaseGuard.Stop()
	// Closing only this connection deterministically makes renewal fail while
	// leaving the durable database available to the competing scheduler.
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-leaseGuard.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("renewal errors let Planner outlive its last confirmed expiry")
	}
	if elapsed := time.Since(started); elapsed < 750*time.Millisecond ||
		elapsed > 1500*time.Millisecond {
		t.Fatalf("lease watchdog canceled after %s, want the one-second claim boundary", elapsed)
	}
	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(joined, "heartbeat failed") ||
		!strings.Contains(joined, "claim lost") {
		t.Fatalf("transient failure and terminal lease loss were not observable: %q", joined)
	}
	reclaimed, err := competitor.ClaimAutoDecompose(
		context.Background(),
		task.Task.ID,
		store.AutoDecomposeMaxAttempts,
		claimTTL,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Eligibility != store.AutoDecomposeClaimed ||
		reclaimed.Claim == nil || reclaimed.Claim.Attempt != 2 {
		t.Fatalf("failed heartbeat did not preserve crash recovery: %+v", reclaimed)
	}
}

func TestAutoDecomposeHeartbeatDoesNotExtendFrozenClockLease(t *testing.T) {
	opened := openDispatcherPlanningStore(t, ":memory:")
	ctx := context.Background()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "Frozen scheduler clock", Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimTTL := time.Second
	current := time.Now().UTC()
	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, store.AutoDecomposeMaxAttempts, claimTTL, current,
	)
	if err != nil || decision.Claim == nil {
		t.Fatalf("claim: decision=%+v err=%v", decision, err)
	}
	claim := *decision.Claim
	started := time.Now()
	leaseGuard := startAutoDecomposeLeaseGuard(
		ctx,
		opened,
		claim,
		claimTTL,
		Options{Now: func() time.Time { return current }},
	)
	defer leaseGuard.Stop()
	select {
	case <-leaseGuard.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("successful no-op renewals extended a frozen durable expiry")
	}
	if elapsed := time.Since(started); elapsed < 750*time.Millisecond ||
		elapsed > 1500*time.Millisecond {
		t.Fatalf("frozen-clock lease canceled after %s, want its original boundary", elapsed)
	}
	state, err := opened.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.ClaimExpiresAt == nil ||
		*state.ClaimExpiresAt != claim.ExpiresAt {
		t.Fatalf("frozen renewal unexpectedly changed durable expiry: %+v", state)
	}
}

func TestExtendAutoDecomposeDeadlineUsesConservativeClockBound(t *testing.T) {
	monotonicNow := time.Now()
	wall := time.Date(2040, 7, 8, 9, 10, 11, 0, time.UTC)
	previousExpiry := wall.Add(100 * time.Second)
	previousDeadline := monotonicNow.Add(100 * time.Second)
	tests := []struct {
		name     string
		renewed  time.Time
		observed time.Time
		want     time.Time
	}{
		{
			name: "frozen clock", renewed: previousExpiry,
			observed: wall, want: previousDeadline,
		},
		{
			name: "backward clock", renewed: previousExpiry,
			observed: wall.Add(-20 * time.Second), want: previousDeadline,
		},
		{
			name: "forward clock", renewed: previousExpiry.Add(20 * time.Second),
			observed: wall.Add(20 * time.Second), want: monotonicNow.Add(100 * time.Second),
		},
		{
			name: "database delay", renewed: previousExpiry.Add(10 * time.Second),
			observed: wall.Add(30 * time.Second), want: monotonicNow.Add(80 * time.Second),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := extendAutoDecomposeDeadline(
				previousDeadline, previousExpiry, test.renewed,
				test.observed, monotonicNow,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("deadline = %s, want %s", got, test.want)
			}
		})
	}
	if _, err := extendAutoDecomposeDeadline(
		previousDeadline, previousExpiry, previousExpiry,
		previousExpiry, monotonicNow,
	); !errors.Is(err, store.ErrAutoDecomposeClaimExpired) {
		t.Fatalf("already-expired wall bound error = %v", err)
	}
}

func TestAutoDecomposeHeartbeatCoversPlannerAndAtomicResultApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened := openDispatcherPlanningStore(t, path)
	competitor := openDispatcherPlanningStore(t, path)
	claimTTL := time.Second
	task, claim := claimDispatcherPlanningTask(t, opened, claimTTL)
	originalExpiry, err := time.Parse(time.RFC3339Nano, claim.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	leaseGuard := startAutoDecomposeLeaseGuard(
		context.Background(),
		opened,
		claim,
		claimTTL,
		Options{Now: time.Now},
	)
	defer leaseGuard.Stop()
	planner := leaseGuard.planner(
		func(ctx context.Context, _ orchestration.PlannerRequest) (any, error) {
			for !time.Now().After(originalExpiry.Add(100 * time.Millisecond)) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
			}
			busy, err := competitor.ClaimAutoDecompose(
				ctx,
				task.Task.ID,
				store.AutoDecomposeMaxAttempts,
				claimTTL,
				time.Now(),
			)
			if err != nil {
				return nil, err
			}
			if busy.Eligibility != store.AutoDecomposeBusy || busy.Attempts != 1 {
				t.Fatalf("competitor decision during Planner = %+v", busy)
			}
			return map[string]any{
				"fanout": false, "rootTitle": "Specified after a long plan",
				"rootBody": "Acceptance: the renewed Planner result is committed.",
				"reason":   "one worker", "tasks": []any{}, "dependencies": []any{},
			}, nil
		},
	)
	profile := orchestration.ProfileRoute{
		Name: "worker", Runtime: model.RuntimeCodex,
	}
	result, err := orchestration.DecomposeTriageTask(
		leaseGuard.ctx,
		opened,
		task.Task.ID,
		orchestration.DecomposeOptions{
			Profiles:           []orchestration.ProfileRoute{profile},
			DefaultProfile:     profile,
			AutoDecomposeClaim: &claim,
			Planner:            planner,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.Task.Status != model.TaskStatusReady ||
		result.Task.Task.Title != "Specified after a long plan" ||
		result.Task.Task.Assignee == nil || *result.Task.Task.Assignee != profile.Name {
		t.Fatalf("post-Planner graph mutation was canceled or incomplete: %+v", result.Task.Task)
	}
	state, err := opened.GetAutoDecomposeState(leaseGuard.ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("atomic result application did not consume claim: %+v", state)
	}
}
