package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestObservedRecoveryRejectsLeaseRenewedAfterExpiredSnapshot(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "recovery-cas.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 60)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	pastHeartbeat := time.Now().Add(-2 * time.Minute).UTC().
		Format("2006-01-02T15:04:05.000Z")
	pastExpiry := time.Now().Add(-time.Minute).UTC().
		Format("2006-01-02T15:04:05.000Z")
	if _, err := opened.db.ExecContext(
		ctx,
		`UPDATE task_runs SET heartbeat_at = ?, claim_expires_at = ?
		 WHERE id = ?`,
		pastHeartbeat,
		pastExpiry,
		claim.Run.ID,
	); err != nil {
		t.Fatal(err)
	}
	expired, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observation := ObserveRunForRecovery(expired, nil)
	if _, err := opened.RenewManagedRunLease(ctx, scope); err != nil {
		t.Fatal(err)
	}

	_, err = opened.FenceObservedRunRecovery(
		ctx,
		observation,
		30,
		"stale snapshot",
		model.RunStatusReclaimed,
		false,
	)
	if !errors.Is(err, ErrRunRecoveryObservationChanged) {
		t.Fatalf("fence error = %v, want ErrRunRecoveryObservationChanged", err)
	}
	if _, err := opened.RecoverObservedAbandonedRun(
		ctx,
		observation,
		model.RunStatusReclaimed,
		"stale snapshot",
		false,
	); !errors.Is(err, ErrRunRecoveryObservationChanged) {
		t.Fatalf("recovery error = %v, want ErrRunRecoveryObservationChanged", err)
	}
	detail, err := opened.GetTask(ctx, claim.Task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusRunning ||
		detail.Task.CurrentRunID == nil ||
		*detail.Task.CurrentRunID != claim.Run.ID {
		t.Fatalf("renewed run was terminalized: %#v", detail.Task)
	}
	if reclaim, err := opened.GetDeferredReclaim(ctx, claim.Run.ID); err != nil {
		t.Fatal(err)
	} else if reclaim != nil {
		t.Fatalf("stale observer created a recovery fence: %#v", reclaim)
	}
}

func TestRunRecoveryFenceBlocksClaimScopedMutationUntilHostAcknowledges(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "recovery-fence.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 60)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	observation := ObserveRunForRecovery(claim.Run, nil)
	if _, err := opened.FenceObservedRunRecovery(
		ctx,
		observation,
		30,
		"supervisor fence",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}

	operations := []struct {
		name string
		run  func() error
	}{
		{
			name: "heartbeat",
			run: func() error {
				_, err := opened.Heartbeat(ctx, scope, "late")
				return err
			},
		},
		{
			name: "managed lease",
			run: func() error {
				_, err := opened.RenewManagedRunLease(ctx, scope)
				return err
			},
		},
		{
			name: "agent config",
			run: func() error {
				_, err := opened.RecordRunAgentConfig(ctx, scope, RecordRunAgentConfigInput{
					Profile: "default",
					Runtime: model.RuntimeCodex,
					Source:  "test",
				})
				return err
			},
		},
		{
			name: "workspace",
			run: func() error {
				_, err := opened.BindRunWorkspace(ctx, scope, BindRunWorkspaceInput{
					Path: filepath.Join(t.TempDir(), "work"),
					Kind: model.WorkspaceScratch,
				})
				return err
			},
		},
		{
			name: "completion",
			run: func() error {
				_, err := opened.RequestRunCompletion(
					ctx,
					scope,
					CompletionInput{Summary: "late completion"},
				)
				return err
			},
		},
		{
			name: "block",
			run: func() error {
				_, err := opened.BlockRun(
					ctx,
					scope,
					BlockInput{
						Reason: "late block",
						Kind:   model.BlockKindNeedsInput,
					},
				)
				return err
			},
		},
		{
			name: "fail",
			run: func() error {
				_, err := opened.FailRun(
					ctx,
					scope,
					"late failure",
					FailRunOptions{},
				)
				return err
			},
		},
		{
			name: "active-host recovery bridge",
			run: func() error {
				_, err := opened.RecoverRunBlocked(
					ctx,
					scope,
					RecoverBlockedRunInput{
						Outcome: model.RunStatusBlocked,
						Reason:  "late recovery bridge",
						Kind:    model.BlockKindNeedsInput,
					},
				)
				return err
			},
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, ErrRunTerminationPending) {
				t.Fatalf("error = %v, want ErrRunTerminationPending", err)
			}
		})
	}

	fence, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || fence == nil {
		t.Fatalf("load recovery fence before acknowledgment: %#v, %v", fence, err)
	}
	reclaim, err := opened.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaim.HostAcknowledgedAt == nil || reclaim.FenceToken == "" {
		t.Fatalf("acknowledged fence = %#v", reclaim)
	}
	current, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := ObserveRunForRecovery(current, nil, &reclaim)
	acknowledged, acquired, err := opened.ClaimObservedRunRecovery(
		ctx,
		acknowledged,
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("claim recovery ownership: acquired=%v, err=%v", acquired, err)
	}
	renewedOwner, err := opened.RenewObservedRunRecoveryOwnership(
		ctx,
		acknowledged,
		2*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if renewedOwner.ObservedRecoveryOwnerExpiresAt == nil ||
		acknowledged.ObservedRecoveryOwnerExpiresAt == nil ||
		*renewedOwner.ObservedRecoveryOwnerExpiresAt ==
			*acknowledged.ObservedRecoveryOwnerExpiresAt {
		t.Fatalf(
			"owner expiry did not advance: before=%v after=%v",
			acknowledged.ObservedRecoveryOwnerExpiresAt,
			renewedOwner.ObservedRecoveryOwnerExpiresAt,
		)
	}
	// Recovery APIs intentionally receive the immutable owner credential from
	// the original claim. A lease renewal changes only the DB expiry and must
	// not make that observation fail its own CAS.
	recovered, err := opened.RecoverObservedAbandonedRun(
		ctx,
		acknowledged,
		model.RunStatusReclaimed,
		"host quiesced",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Task.Status == model.TaskStatusRunning ||
		recovered.Task.CurrentRunID != nil {
		t.Fatalf("acknowledged recovery stayed active: %#v", recovered.Task)
	}
}

func TestOperatorQuiescenceConfirmationIsAuditedExactAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "operator-confirm.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 60)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	spawned, err := opened.RecordSpawnWithIdentity(
		ctx,
		scope,
		424242,
		filepath.Join(t.TempDir(), "worker.log"),
		"process-start-identity",
	)
	if err != nil {
		t.Fatal(err)
	}
	initialObservation := ObserveRunForRecovery(
		spawned,
		stringAddress("process-start-identity"),
	)
	_, err = opened.FenceObservedRunRecovery(
		ctx,
		initialObservation,
		30,
		"recovery needs quiescence",
		model.RunStatusReclaimed,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || fence == nil {
		t.Fatalf("fence = %#v, err=%v", fence, err)
	}
	acknowledged, err := opened.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledged.HostAcknowledgedAt == nil {
		t.Fatal("managed host acknowledgment was not stored")
	}
	current, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	operatorRun, err := opened.RequireObservedRunRecoveryIntervention(
		ctx,
		ObserveRunForRecovery(
			current,
			stringAddress("process-start-identity"),
			&acknowledged,
		),
		30,
		"process containment proof unavailable",
		model.RunStatusReclaimed,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	operatorFence, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || operatorFence == nil {
		t.Fatalf("operator fence = %#v, err=%v", operatorFence, err)
	}
	if operatorFence.FenceGeneration != acknowledged.FenceGeneration+1 ||
		operatorFence.FenceToken != acknowledged.FenceToken ||
		operatorFence.HostAcknowledgedAt == nil {
		t.Fatalf(
			"operator transition changed token-scoped ACK: before=%#v after=%#v",
			acknowledged,
			operatorFence,
		)
	}

	baseInput := ConfirmRunRecoveryQuiescenceInput{
		RunID:                    claim.Run.ID,
		FenceGeneration:          operatorFence.FenceGeneration,
		Actor:                    "operator@example.test",
		Reason:                   "verified guard and external host are stopped",
		ConfirmWorkerStopped:     true,
		ConfirmHostWritesStopped: true,
	}
	invalid := []struct {
		name   string
		mutate func(*ConfirmRunRecoveryQuiescenceInput)
	}{
		{
			name: "wrong generation",
			mutate: func(input *ConfirmRunRecoveryQuiescenceInput) {
				input.FenceGeneration--
			},
		},
		{
			name: "missing actor",
			mutate: func(input *ConfirmRunRecoveryQuiescenceInput) {
				input.Actor = ""
			},
		},
		{
			name: "missing reason",
			mutate: func(input *ConfirmRunRecoveryQuiescenceInput) {
				input.Reason = ""
			},
		},
		{
			name: "worker not confirmed",
			mutate: func(input *ConfirmRunRecoveryQuiescenceInput) {
				input.ConfirmWorkerStopped = false
			},
		},
		{
			name: "host not confirmed",
			mutate: func(input *ConfirmRunRecoveryQuiescenceInput) {
				input.ConfirmHostWritesStopped = false
			},
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			input := baseInput
			test.mutate(&input)
			if _, err := opened.ConfirmRunRecoveryQuiescence(
				ctx,
				input,
			); err == nil {
				t.Fatal("invalid operator confirmation succeeded")
			}
		})
	}

	activeOwnerExpiry := time.Now().Add(time.Minute).UTC().
		Format("2006-01-02T15:04:05.000Z")
	if _, err := opened.db.ExecContext(
		ctx,
		`UPDATE run_reclaim_requests
		 SET recovery_owner_token = 'other-supervisor',
		     recovery_owner_expires_at = ?
		 WHERE run_id = ?`,
		activeOwnerExpiry,
		claim.Run.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ConfirmRunRecoveryQuiescence(
		ctx,
		baseInput,
	); !errors.Is(err, ErrRunRecoveryOwned) {
		t.Fatalf("active-owner confirmation error = %v", err)
	}
	expiredOwner := time.Now().Add(-time.Minute).UTC().
		Format("2006-01-02T15:04:05.000Z")
	if _, err := opened.db.ExecContext(
		ctx,
		`UPDATE run_reclaim_requests SET recovery_owner_expires_at = ?
		 WHERE run_id = ?`,
		expiredOwner,
		claim.Run.ID,
	); err != nil {
		t.Fatal(err)
	}
	confirmed, err := opened.ConfirmRunRecoveryQuiescence(ctx, baseInput)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.FenceToken != operatorFence.FenceToken ||
		confirmed.FenceGeneration != operatorFence.FenceGeneration+1 ||
		confirmed.HostAcknowledgedAt == nil ||
		confirmed.RequiresOperator ||
		confirmed.OperatorQuiescedGeneration == nil ||
		*confirmed.OperatorQuiescedGeneration != confirmed.FenceGeneration ||
		confirmed.OperatorQuiescedBy == nil ||
		*confirmed.OperatorQuiescedBy != baseInput.Actor ||
		confirmed.RecoveryOwnerToken != nil ||
		confirmed.RecoveryOwnerExpiresAt != nil {
		t.Fatalf("confirmed fence = %#v", confirmed)
	}
	confirmedRun, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !RecoveryQuiescenceAttestationCurrent(
		confirmedRun,
		stringAddress("process-start-identity"),
		&confirmed,
	) {
		t.Fatalf("fresh confirmation did not match run: %#v", confirmed)
	}
	if _, err := opened.ConfirmRunRecoveryQuiescence(
		ctx,
		baseInput,
	); !errors.Is(err, ErrRunRecoveryObservationChanged) {
		t.Fatalf("replayed confirmation error = %v", err)
	}
	var events int
	if err := opened.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM task_events
		 WHERE run_id = ? AND kind = 'recovery_quiescence_confirmed'`,
		claim.Run.ID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("confirmation events = %d, want 1", events)
	}

	_, err = opened.RequireObservedRunRecoveryIntervention(
		ctx,
		ObserveRunForRecovery(
			operatorRun,
			stringAddress("process-start-identity"),
			&confirmed,
		),
		30,
		"matching process became live again",
		model.RunStatusReclaimed,
		false,
	)
	if err != nil {
		// operatorRun predates confirmation but the run lease fields are
		// unchanged, so only fence state comes from confirmed.
		t.Fatal(err)
	}
	reintervention, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reintervention == nil ||
		!reintervention.RequiresOperator ||
		reintervention.FenceGeneration != confirmed.FenceGeneration+1 ||
		reintervention.OperatorQuiescedGeneration != nil ||
		reintervention.OperatorQuiescedAt != nil ||
		reintervention.RecoveryOwnerToken != nil {
		t.Fatalf("re-intervention did not invalidate authorization: %#v", reintervention)
	}
}

func stringAddress(value string) *string {
	return &value
}

func TestRecoveryOperatorFenceIsDurableMonotonicAndOneShot(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "operator-fence.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 60)
	observation := ObserveRunForRecovery(claim.Run, nil)
	first, err := opened.RequireObservedRunRecoveryIntervention(
		ctx,
		observation,
		30,
		"process ownership is not verifiable",
		model.RunStatusReclaimed,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	reclaim, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || reclaim == nil {
		t.Fatalf("operator fence = %#v, %v", reclaim, err)
	}
	if !reclaim.RequiresOperator ||
		reclaim.DiagnosticCode == nil ||
		*reclaim.DiagnosticCode != "unverifiable_process_ownership" ||
		reclaim.FenceToken == "" {
		t.Fatalf("operator fence = %#v", reclaim)
	}

	updatedObservation := ObserveRunForRecovery(first, nil, reclaim)
	if _, err := opened.RequireObservedRunRecoveryIntervention(
		ctx,
		updatedObservation,
		30,
		reclaim.Reason,
		reclaim.Outcome,
		reclaim.CountFailure,
	); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := opened.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM task_events
		 WHERE run_id = ? AND kind = 'reclaim_requires_operator'`,
		claim.Run.ID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("operator transition events = %d, want 1", events)
	}

	current, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reclaim, err = opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FenceObservedRunRecovery(
		ctx,
		ObserveRunForRecovery(current, nil, reclaim),
		30,
		"ordinary refresh",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	reclaim, err = opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaim.RequiresOperator ||
		reclaim.DiagnosticCode == nil ||
		*reclaim.DiagnosticCode != "unverifiable_process_ownership" {
		t.Fatalf("ordinary refresh downgraded operator fence: %#v", reclaim)
	}
}

func TestConcurrentSupervisorsElectOneRecoveryOwner(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "recovery-owner-race.db")
	first, err := Open(databasePath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(databasePath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	claim := claimManagedLeaseFixture(t, first, 60)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := first.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	if _, err := first.FenceObservedRunRecovery(
		ctx,
		ObserveRunForRecovery(claim.Run, nil),
		30,
		"elect one recovery owner",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	fence, err := first.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || fence == nil {
		t.Fatalf("fence = %#v, err=%v", fence, err)
	}
	fence, err = func() (*DeferredReclaim, error) {
		acknowledged, ackErr := first.AcknowledgeRunRecoveryFence(
			ctx,
			scope,
			fence.FenceToken,
			fence.FenceGeneration,
		)
		return &acknowledged, ackErr
	}()
	if err != nil {
		t.Fatal(err)
	}
	current, err := getRun(ctx, first.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observation := ObserveRunForRecovery(current, nil, fence)
	type result struct {
		observation RunRecoveryObservation
		acquired    bool
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	claimOwner := func(opened *Store) {
		<-start
		owned, acquired, claimErr := opened.ClaimObservedRunRecovery(
			ctx,
			observation,
			time.Minute,
		)
		results <- result{
			observation: owned,
			acquired:    acquired,
			err:         claimErr,
		}
	}
	go claimOwner(first)
	go claimOwner(second)
	close(start)
	var winner *RunRecoveryObservation
	lost := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.acquired:
			if winner != nil {
				t.Fatal("two Supervisors acquired the same recovery epoch")
			}
			value := result.observation
			winner = &value
		case result.err == nil && !result.acquired:
			lost++
		case errors.Is(result.err, ErrRunRecoveryObservationChanged),
			errors.Is(result.err, ErrRunRecoveryOwned):
			lost++
		default:
			t.Fatalf(
				"unexpected owner claim result: acquired=%v err=%v",
				result.acquired,
				result.err,
			)
		}
	}
	if winner == nil || lost != 1 {
		t.Fatalf("owner election winner=%#v lost=%d", winner, lost)
	}
	if err := first.ValidateObservedRunRecoveryOwnership(
		ctx,
		*winner,
	); err != nil {
		t.Fatalf("winner does not own recovery: %v", err)
	}
	if err := second.ValidateObservedRunRecoveryOwnership(
		ctx,
		observation,
	); err == nil {
		t.Fatal("non-owner observation passed recovery ownership validation")
	}
}
