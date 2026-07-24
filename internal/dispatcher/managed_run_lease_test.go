package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

func TestManagedRunLeaseGuardPreventsConcurrentRecovery(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "host setup remains owned", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 1,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(context.Background(), scope, true); err != nil {
		t.Fatal(err)
	}
	guard, err := startManagedRunLeaseGuard(
		context.Background(),
		opened,
		scope,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	originalExpiry, err := time.Parse(time.RFC3339Nano, claim.Run.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for !time.Now().After(originalExpiry.Add(150 * time.Millisecond)) {
		time.Sleep(20 * time.Millisecond)
	}
	crashGrace := time.Duration(0)
	options := Options{
		StaleTimeout:      24 * time.Hour,
		HeartbeatMaxStale: 24 * time.Hour,
		CrashGrace:        &crashGrace,
		TerminationGrace:  time.Second,
	}
	if err := recoverAbandonedRuns(
		context.Background(),
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	active, err := opened.GetTask(context.Background(), task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Task.Status != model.TaskStatusRunning ||
		active.Task.CurrentRunID == nil ||
		*active.Task.CurrentRunID != claim.Run.ID {
		t.Fatalf("live managed run was reclaimed: %#v", active.Task)
	}
	if err := guard.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRunLeaseGuardFencesUnconfirmedTeardownWithoutHostACK(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title:    "unconfirmed teardown remains operator fenced",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 30,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{
		RunID:      claim.Run.ID,
		ClaimToken: claim.ClaimToken,
	}
	if err := opened.MarkRunManagedWithPolicy(
		context.Background(),
		scope,
		true,
	); err != nil {
		t.Fatal(err)
	}
	guard, err := startManagedRunLeaseGuard(
		context.Background(),
		opened,
		scope,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	processguard.ReportTeardownFailure(
		guard.ctx,
		errors.Join(
			processguard.ErrTeardownUnconfirmed,
			errors.New("guard did not report descendant exit"),
		),
	)
	select {
	case <-guard.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("unconfirmed teardown did not cancel the managed run")
	}
	reclaim, err := opened.GetDeferredReclaim(
		context.Background(),
		claim.Run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaim == nil ||
		!reclaim.RequiresOperator ||
		reclaim.DiagnosticCode == nil ||
		*reclaim.DiagnosticCode != "process_teardown_unconfirmed" ||
		reclaim.HostAcknowledgedAt != nil {
		t.Fatalf("unconfirmed teardown fence = %#v", reclaim)
	}
	if err := guard.Stop(); !errors.Is(
		err,
		processguard.ErrTeardownUnconfirmed,
	) {
		t.Fatalf("guard stop error = %v, want teardown sentinel", err)
	}
	reclaim, err = opened.GetDeferredReclaim(
		context.Background(),
		claim.Run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaim == nil || reclaim.HostAcknowledgedAt != nil {
		t.Fatalf("unconfirmed teardown was acknowledged: %#v", reclaim)
	}
}

func TestRecoverySweepRefreshesWhenManagedLeaseRenewsAfterSnapshot(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title:    "renew between recovery observation and fence",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 1,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(context.Background(), scope, true); err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, claim.Run.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for !time.Now().After(expiry.Add(50 * time.Millisecond)) {
		time.Sleep(10 * time.Millisecond)
	}
	renewed := false
	options := Options{
		StaleTimeout:      24 * time.Hour,
		HeartbeatMaxStale: 24 * time.Hour,
		TerminationGrace:  time.Second,
		testHooks: &dispatcherTestHooks{
			recoveryObserved: func(ctx context.Context, item store.ActiveRun) error {
				if renewed || item.Run.ID != claim.Run.ID {
					return nil
				}
				renewed = true
				_, err := opened.RenewManagedRunLease(ctx, scope)
				return err
			},
		},
	}
	if err := recoverAbandonedRuns(
		context.Background(),
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	if !renewed {
		t.Fatal("test hook did not renew the observed lease")
	}
	current, err := opened.GetTask(context.Background(), task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Task.Status != model.TaskStatusRunning ||
		current.Task.CurrentRunID == nil ||
		*current.Task.CurrentRunID != claim.Run.ID {
		t.Fatalf("renewed run was recovered from stale snapshot: %#v", current.Task)
	}
	if reclaim, err := opened.GetDeferredReclaim(
		context.Background(),
		claim.Run.ID,
	); err != nil {
		t.Fatal(err)
	} else if reclaim != nil {
		t.Fatalf("stale recovery created a fence: %#v", reclaim)
	}
}

func TestManagedHostAcknowledgesRecoveryFenceOnlyAfterRunClaimUnwinds(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title:    "host quiescence acknowledgment",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 1,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runClaim(
			context.Background(),
			manager,
			opened,
			claim,
			Options{
				AllowWrites: true,
				Interval:    100 * time.Millisecond,
				testHooks: &dispatcherTestHooks{
					managedRunPhase: func(ctx context.Context, phase string) error {
						if phase != "lease-established" {
							return nil
						}
						close(entered)
						<-ctx.Done()
						return ctx.Err()
					},
				},
			},
			NewProcessSet(),
			t.TempDir(),
		)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("runClaim did not enter the blocked host phase")
	}
	inspection, err := opened.GetRun(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := opened.GetRunProcessIdentity(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observation := store.ObserveRunForRecovery(inspection.Run, identity)
	fencedRun, err := opened.FenceObservedRunRecovery(
		context.Background(),
		observation,
		1,
		"test recovery fence",
		model.RunStatusReclaimed,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	reclaim, err := opened.GetDeferredReclaim(context.Background(), claim.Run.ID)
	if err != nil || reclaim == nil {
		t.Fatalf("fence = %#v, %v", reclaim, err)
	}
	if reclaim.HostAcknowledgedAt != nil {
		t.Fatalf("host acknowledged before runClaim unwound: %#v", reclaim)
	}
	fencedObservation := store.ObserveRunForRecovery(
		fencedRun,
		identity,
		reclaim,
	)
	if _, err := opened.RecoverObservedAbandonedRun(
		context.Background(),
		fencedObservation,
		model.RunStatusReclaimed,
		"too early",
		false,
	); !errors.Is(err, store.ErrRunRecoveryFenceNotReady) {
		t.Fatalf("early recovery error = %v, want fence not ready", err)
	}
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("runClaim did not unwind after recovery fence")
	}
	reclaim, err = opened.GetDeferredReclaim(context.Background(), claim.Run.ID)
	if err != nil || reclaim == nil {
		t.Fatalf("acknowledged fence = %#v, %v", reclaim, err)
	}
	if reclaim.HostAcknowledgedAt == nil {
		t.Fatalf("runClaim exited without host quiescence acknowledgment: %#v", reclaim)
	}
}

func TestRunClaimRenewsLeaseWhileHostSetupIsBlocked(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "blocked host setup remains fenced", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 1,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{
		RunID:      claim.Run.ID,
		ClaimToken: claim.ClaimToken,
	}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runClaim(
			ctx,
			manager,
			opened,
			claim,
			Options{
				AllowWrites: true,
				Interval:    250 * time.Millisecond,
				testHooks: &dispatcherTestHooks{
					managedRunPhase: func(ctx context.Context, phase string) error {
						if phase != "lease-established" {
							return nil
						}
						close(entered)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				},
			},
			NewProcessSet(),
			"",
		)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runClaim did not reach blocked host setup")
	}
	originalExpiry, err := time.Parse(time.RFC3339Nano, claim.Run.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	for !time.Now().After(originalExpiry.Add(150 * time.Millisecond)) {
		time.Sleep(20 * time.Millisecond)
	}
	crashGrace := time.Duration(0)
	if err := recoverAbandonedRuns(
		context.Background(),
		opened,
		"default",
		Options{
			StaleTimeout:      24 * time.Hour,
			HeartbeatMaxStale: 24 * time.Hour,
			CrashGrace:        &crashGrace,
			TerminationGrace:  time.Second,
		},
	); err != nil {
		t.Fatal(err)
	}
	active, err := opened.GetTask(context.Background(), task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Task.Status != model.TaskStatusRunning ||
		active.Task.CurrentRunID == nil ||
		*active.Task.CurrentRunID != claim.Run.ID {
		t.Fatalf("maintenance reclaimed host-owned run: %#v", active.Task)
	}
	close(release)
	cancel()
	select {
	case runErr := <-result:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("canceled runClaim error = %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runClaim did not stop after releasing test phase")
	}
	terminal, err := opened.IsRunTerminal(context.Background(), claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal {
		countFailure := false
		if _, err := opened.FailRun(
			context.Background(),
			scope,
			"test cleanup",
			store.FailRunOptions{
				Outcome:      model.RunStatusReclaimed,
				CountFailure: &countFailure,
			},
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagedRunLeaseGuardFencesUnexpectedTerminalOwnershipLoss(t *testing.T) {
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "terminal ownership loss", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: 1,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(context.Background(), scope, true); err != nil {
		t.Fatal(err)
	}
	guard, err := startManagedRunLeaseGuard(
		context.Background(),
		opened,
		scope,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	countFailure := false
	if _, err := opened.FailRun(
		context.Background(),
		scope,
		"forced ownership loss",
		store.FailRunOptions{
			Outcome:      model.RunStatusReclaimed,
			CountFailure: &countFailure,
		},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-guard.ctx.Done():
		if !errors.Is(context.Cause(guard.ctx), errManagedRunLeaseLost) {
			t.Fatalf("guard cancellation cause = %v", context.Cause(guard.ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard did not fence a terminalized run")
	}
	if err := guard.Stop(); err != nil {
		t.Fatalf("terminal guard stop = %v", err)
	}
}
