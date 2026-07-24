package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

// ErrRunRecoveryObservationChanged means that a Supervisor recovery decision
// lost its compare-and-swap race with a newer owner update. Callers should
// refresh the run instead of treating this as a board-level failure.
var ErrRunRecoveryObservationChanged = errors.New("run recovery observation changed")

// ErrRunRecoveryFenceNotReady means recovery won its durable fence but the
// managed host has not yet proved that all host-owned Git work is quiescent.
var ErrRunRecoveryFenceNotReady = errors.New("run recovery fence is not ready")

// ErrRunRecoveryOwned means another Supervisor holds the unexpired recovery
// epoch. It is a benign lost race, not a board-level failure.
var ErrRunRecoveryOwned = errors.New("run recovery is owned by another supervisor")

// RunRecoveryObservation is the non-secret run state on which a Supervisor
// based its process-liveness and lease-expiry decision. The PID is included
// because RecordSpawn can change it without also advancing the lease.
type RunRecoveryObservation struct {
	RunID                          string
	HeartbeatAt                    string
	ClaimExpiresAt                 string
	PID                            *int
	ProcessIdentity                *string
	RecoveryFenceToken             string
	RecoveryFenceGeneration        int
	RecoveryRequiresOperator       bool
	HostAcknowledgedAt             *string
	OperatorQuiescedGeneration     *int
	ObservedRecoveryOwnerToken     *string
	ObservedRecoveryOwnerExpiresAt *string
	RecoveryOwnerClaimToken        string
}

// ObserveRunForRecovery captures the state that every automatic Supervisor
// terminalization must compare again inside its write transaction.
func ObserveRunForRecovery(
	run model.Run,
	processIdentity *string,
	reclaims ...*DeferredReclaim,
) RunRecoveryObservation {
	result := RunRecoveryObservation{
		RunID:           run.ID,
		HeartbeatAt:     run.HeartbeatAt,
		ClaimExpiresAt:  run.ClaimExpiresAt,
		PID:             run.PID,
		ProcessIdentity: processIdentity,
	}
	var reclaim *DeferredReclaim
	if len(reclaims) > 0 {
		reclaim = reclaims[0]
	}
	if reclaim != nil {
		result.RecoveryFenceToken = reclaim.FenceToken
		result.RecoveryFenceGeneration = reclaim.FenceGeneration
		result.RecoveryRequiresOperator = reclaim.RequiresOperator
		result.HostAcknowledgedAt = reclaim.HostAcknowledgedAt
		result.OperatorQuiescedGeneration = reclaim.OperatorQuiescedGeneration
		result.ObservedRecoveryOwnerToken = reclaim.RecoveryOwnerToken
		result.ObservedRecoveryOwnerExpiresAt = reclaim.RecoveryOwnerExpiresAt
	}
	return result
}

func requireRunRecoveryObservation(
	ctx context.Context,
	q querier,
	run model.Run,
	observation *RunRecoveryObservation,
) error {
	if observation == nil {
		return nil
	}
	if strings.TrimSpace(observation.RunID) == "" ||
		strings.TrimSpace(observation.HeartbeatAt) == "" ||
		strings.TrimSpace(observation.ClaimExpiresAt) == "" {
		return errors.New("run recovery observation is incomplete")
	}
	if observation.RunID != run.ID {
		return fmt.Errorf(
			"run recovery observation targets %s, not %s",
			observation.RunID,
			run.ID,
		)
	}
	var processIdentity string
	identityErr := q.QueryRowContext(
		ctx,
		"SELECT process_identity FROM run_process_identities WHERE run_id = ?",
		run.ID,
	).Scan(&processIdentity)
	switch {
	case errors.Is(identityErr, sql.ErrNoRows):
		if observation.ProcessIdentity != nil {
			return fmt.Errorf(
				"%w: run %s process identity advanced",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
	case identityErr != nil:
		return identityErr
	case observation.ProcessIdentity == nil ||
		*observation.ProcessIdentity != processIdentity:
		return fmt.Errorf(
			"%w: run %s process identity advanced",
			ErrRunRecoveryObservationChanged,
			run.ID,
		)
	}
	var fenceToken string
	var fenceGeneration int
	var requiresOperator bool
	var hostAcknowledgedAt, recoveryOwnerToken, recoveryOwnerExpiresAt sql.NullString
	var hostAcknowledgedFenceToken sql.NullString
	var operatorQuiescedGeneration sql.NullInt64
	fenceErr := q.QueryRowContext(
		ctx,
		`SELECT fence_token, fence_generation, requires_operator,
			host_acknowledged_at, host_acknowledged_fence_token,
			operator_quiesced_generation, recovery_owner_token,
			recovery_owner_expires_at
		 FROM run_reclaim_requests WHERE run_id = ?`,
		run.ID,
	).Scan(
		&fenceToken,
		&fenceGeneration,
		&requiresOperator,
		&hostAcknowledgedAt,
		&hostAcknowledgedFenceToken,
		&operatorQuiescedGeneration,
		&recoveryOwnerToken,
		&recoveryOwnerExpiresAt,
	)
	switch {
	case errors.Is(fenceErr, sql.ErrNoRows):
		if observation.RecoveryFenceToken != "" ||
			observation.RecoveryFenceGeneration != 0 ||
			observation.RecoveryRequiresOperator ||
			observation.HostAcknowledgedAt != nil ||
			observation.OperatorQuiescedGeneration != nil ||
			observation.ObservedRecoveryOwnerToken != nil ||
			observation.ObservedRecoveryOwnerExpiresAt != nil {
			return fmt.Errorf(
				"%w: run %s recovery fence changed",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
	case fenceErr != nil:
		return fenceErr
	case observation.RecoveryFenceToken != fenceToken ||
		observation.RecoveryFenceGeneration != fenceGeneration ||
		observation.RecoveryRequiresOperator != requiresOperator ||
		!sameNullableString(observation.HostAcknowledgedAt, hostAcknowledgedAt) ||
		!validObservedHostAcknowledgment(
			fenceToken,
			hostAcknowledgedAt,
			hostAcknowledgedFenceToken,
		) ||
		!sameNullableInt64(
			observation.OperatorQuiescedGeneration,
			operatorQuiescedGeneration,
		) ||
		!sameObservedRecoveryOwner(
			observation,
			recoveryOwnerToken,
			recoveryOwnerExpiresAt,
		):
		return fmt.Errorf(
			"%w: run %s recovery fence changed",
			ErrRunRecoveryObservationChanged,
			run.ID,
		)
	}
	if observation.HeartbeatAt != run.HeartbeatAt ||
		observation.ClaimExpiresAt != run.ClaimExpiresAt ||
		!sameOptionalInt(observation.PID, run.PID) {
		return fmt.Errorf(
			"%w: run %s lease or process state advanced",
			ErrRunRecoveryObservationChanged,
			run.ID,
		)
	}
	return nil
}

func validObservedHostAcknowledgment(
	fenceToken string,
	acknowledgedAt sql.NullString,
	acknowledgedFenceToken sql.NullString,
) bool {
	if !acknowledgedAt.Valid {
		return !acknowledgedFenceToken.Valid
	}
	return acknowledgedFenceToken.Valid &&
		acknowledgedFenceToken.String == fenceToken
}

func sameNullableInt64(expected *int, actual sql.NullInt64) bool {
	if !actual.Valid {
		return expected == nil
	}
	return expected != nil && int64(*expected) == actual.Int64
}

func sameObservedRecoveryOwner(
	observation *RunRecoveryObservation,
	ownerToken sql.NullString,
	ownerExpiresAt sql.NullString,
) bool {
	if strings.TrimSpace(observation.RecoveryOwnerClaimToken) != "" {
		return ownerToken.Valid &&
			ownerToken.String == observation.RecoveryOwnerClaimToken &&
			ownerExpiresAt.Valid
	}
	return sameNullableString(
		observation.ObservedRecoveryOwnerToken,
		ownerToken,
	) && sameNullableString(
		observation.ObservedRecoveryOwnerExpiresAt,
		ownerExpiresAt,
	)
}

func sameNullableString(expected *string, actual sql.NullString) bool {
	if !actual.Valid {
		return expected == nil
	}
	return expected != nil && *expected == actual.String
}

// RecoveryQuiescenceAttestationCurrent reports whether the operator confirmed
// both external-worker and host-write quiescence for this exact immutable
// fence generation and run observation. It never treats the attestation as
// proof against a newly observed live, matching process; the Supervisor must
// still perform that OS-level check before workspace access.
func RecoveryQuiescenceAttestationCurrent(
	run model.Run,
	processIdentity *string,
	reclaim *DeferredReclaim,
) bool {
	if reclaim == nil ||
		reclaim.RequiresOperator ||
		reclaim.OperatorQuiescedGeneration == nil ||
		*reclaim.OperatorQuiescedGeneration != reclaim.FenceGeneration ||
		!reclaim.OperatorConfirmedWorkerStopped ||
		!reclaim.OperatorConfirmedHostWritesStopped ||
		reclaim.OperatorObservedHeartbeatAt == nil ||
		reclaim.OperatorObservedClaimExpiresAt == nil ||
		*reclaim.OperatorObservedHeartbeatAt != run.HeartbeatAt ||
		*reclaim.OperatorObservedClaimExpiresAt != run.ClaimExpiresAt ||
		!sameOptionalInt(reclaim.OperatorObservedPID, run.PID) {
		return false
	}
	return sameRecoveryOptionalString(
		reclaim.OperatorObservedProcessIdentity,
		processIdentity,
	)
}

func sameRecoveryOptionalString(left, right *string) bool {
	return (left == nil && right == nil) ||
		(left != nil && right != nil && *left == *right)
}

func requireSupervisorRunRecoveryFenceReady(
	ctx context.Context,
	q querier,
	run model.Run,
	observation *RunRecoveryObservation,
) error {
	if observation == nil {
		return fmt.Errorf(
			"%w: run %s has no observed recovery fence",
			ErrRunRecoveryFenceNotReady,
			run.ID,
		)
	}
	if err := requireRunRecoveryObservation(
		ctx,
		q,
		run,
		observation,
	); err != nil {
		return err
	}
	if strings.TrimSpace(observation.RecoveryFenceToken) == "" {
		return fmt.Errorf(
			"%w: run %s has no observed recovery fence",
			ErrRunRecoveryFenceNotReady,
			run.ID,
		)
	}
	if observation.RecoveryRequiresOperator {
		return fmt.Errorf(
			"%w: run %s still requires operator intervention",
			ErrRunRecoveryFenceNotReady,
			run.ID,
		)
	}
	var operatorQuiescedGeneration sql.NullInt64
	var operatorConfirmedWorkerStopped, operatorConfirmedHostWritesStopped bool
	var operatorObservedHeartbeatAt, operatorObservedClaimExpiresAt sql.NullString
	var operatorObservedPID sql.NullInt64
	var operatorObservedProcessIdentity sql.NullString
	if err := q.QueryRowContext(
		ctx,
		`SELECT operator_quiesced_generation,
			operator_confirmed_worker_stopped,
			operator_confirmed_host_writes_stopped,
			operator_observed_heartbeat_at,
			operator_observed_claim_expires_at,
			operator_observed_pid,
			operator_observed_process_identity
		 FROM run_reclaim_requests WHERE run_id = ?`,
		run.ID,
	).Scan(
		&operatorQuiescedGeneration,
		&operatorConfirmedWorkerStopped,
		&operatorConfirmedHostWritesStopped,
		&operatorObservedHeartbeatAt,
		&operatorObservedClaimExpiresAt,
		&operatorObservedPID,
		&operatorObservedProcessIdentity,
	); err != nil {
		return err
	}
	operatorQuiesced := operatorQuiescedGeneration.Valid &&
		int(operatorQuiescedGeneration.Int64) == observation.RecoveryFenceGeneration &&
		operatorConfirmedWorkerStopped &&
		operatorConfirmedHostWritesStopped &&
		operatorObservedHeartbeatAt.Valid &&
		operatorObservedHeartbeatAt.String == run.HeartbeatAt &&
		operatorObservedClaimExpiresAt.Valid &&
		operatorObservedClaimExpiresAt.String == run.ClaimExpiresAt &&
		sameNullableInt(run.PID, operatorObservedPID) &&
		sameNullableString(observation.ProcessIdentity, operatorObservedProcessIdentity)
	var managed bool
	if err := q.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM managed_runs WHERE run_id = ?)",
		run.ID,
	).Scan(&managed); err != nil {
		return err
	}
	if !managed && !operatorQuiesced {
		return fmt.Errorf(
			"%w: external run %s has no current operator quiescence confirmation",
			ErrRunRecoveryFenceNotReady,
			run.ID,
		)
	}
	if managed && observation.HostAcknowledgedAt == nil && !operatorQuiesced {
		return fmt.Errorf(
			"%w: managed host has not acknowledged fence for run %s",
			ErrRunRecoveryFenceNotReady,
			run.ID,
		)
	}
	return nil
}

func sameNullableInt(expected *int, actual sql.NullInt64) bool {
	if !actual.Valid {
		return expected == nil
	}
	return expected != nil && int64(*expected) == actual.Int64
}

func requireSupervisorRunRecoveryFence(
	ctx context.Context,
	q querier,
	run model.Run,
	observation *RunRecoveryObservation,
) error {
	if err := requireSupervisorRunRecoveryFenceReady(
		ctx,
		q,
		run,
		observation,
	); err != nil {
		return err
	}
	if strings.TrimSpace(observation.RecoveryOwnerClaimToken) == "" {
		return fmt.Errorf(
			"%w: run %s has no matching recovery owner epoch",
			ErrRunRecoveryOwned,
			run.ID,
		)
	}
	var ownerToken, ownerExpiresAt string
	if err := q.QueryRowContext(
		ctx,
		`SELECT recovery_owner_token, recovery_owner_expires_at
		 FROM run_reclaim_requests WHERE run_id = ?`,
		run.ID,
	).Scan(&ownerToken, &ownerExpiresAt); err != nil {
		return err
	}
	if ownerToken != observation.RecoveryOwnerClaimToken {
		return fmt.Errorf(
			"%w: run %s recovery owner epoch changed",
			ErrRunRecoveryOwned,
			run.ID,
		)
	}
	expires, err := time.Parse(
		time.RFC3339Nano,
		ownerExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("parse run recovery owner expiry: %w", err)
	}
	if !expires.After(time.Now()) {
		return fmt.Errorf(
			"%w: run %s recovery owner epoch expired",
			ErrRunRecoveryOwned,
			run.ID,
		)
	}
	return nil
}

func requireOptionalSupervisorRunRecoveryFence(
	ctx context.Context,
	q querier,
	run model.Run,
	observation *RunRecoveryObservation,
) error {
	if observation == nil {
		return nil
	}
	return requireSupervisorRunRecoveryFence(ctx, q, run, observation)
}

// ClaimObservedRunRecovery elects one Supervisor for pre-terminal workspace
// inspection. A caller that merely read another owner's token never receives
// the local claim credential needed by terminal recovery APIs.
func (s *Store) ClaimObservedRunRecovery(
	ctx context.Context,
	observation RunRecoveryObservation,
	ttl time.Duration,
) (RunRecoveryObservation, bool, error) {
	if ttl <= 0 {
		return RunRecoveryObservation{}, false, errors.New(
			"run recovery ownership TTL must be positive",
		)
	}
	ownerToken, err := claimToken()
	if err != nil {
		return RunRecoveryObservation{}, false, err
	}
	result := observation
	acquired := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, observation.RunID)
		if err != nil {
			return err
		}
		if err := requireSupervisorRunRecoveryFenceReady(
			ctx,
			tx,
			run,
			&observation,
		); err != nil {
			return err
		}
		if observation.ObservedRecoveryOwnerToken != nil &&
			observation.ObservedRecoveryOwnerExpiresAt != nil {
			expires, err := time.Parse(
				time.RFC3339Nano,
				*observation.ObservedRecoveryOwnerExpiresAt,
			)
			if err != nil {
				return fmt.Errorf("parse run recovery owner expiry: %w", err)
			}
			if expires.After(time.Now()) {
				return nil
			}
		}
		expiresAt := time.Now().Add(ttl).UTC().
			Format("2006-01-02T15:04:05.000Z")
		update, err := tx.ExecContext(
			ctx,
			`UPDATE run_reclaim_requests
			 SET recovery_owner_token = ?, recovery_owner_expires_at = ?
			 WHERE run_id = ? AND fence_token = ?
			   AND fence_generation = ?`,
			ownerToken,
			expiresAt,
			run.ID,
			observation.RecoveryFenceToken,
			observation.RecoveryFenceGeneration,
		)
		if err != nil {
			return err
		}
		rows, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf(
				"%w: run %s fence changed before recovery ownership",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
		result.ObservedRecoveryOwnerToken = &ownerToken
		result.ObservedRecoveryOwnerExpiresAt = &expiresAt
		result.RecoveryOwnerClaimToken = ownerToken
		acquired = true
		var generation int
		if err := tx.QueryRowContext(
			ctx,
			`SELECT fence_generation FROM run_reclaim_requests
			 WHERE run_id = ?`,
			run.ID,
		).Scan(&generation); err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.TaskID, "recovery_owner_claimed", map[string]any{
			"expiresAt":       expiresAt,
			"fenceGeneration": generation,
		}, &run.ID)
	})
	if err != nil {
		return RunRecoveryObservation{}, false, err
	}
	return result, acquired, nil
}

// RenewObservedRunRecoveryOwnership extends only the exact local owner epoch.
func (s *Store) RenewObservedRunRecoveryOwnership(
	ctx context.Context,
	observation RunRecoveryObservation,
	ttl time.Duration,
) (RunRecoveryObservation, error) {
	if ttl <= 0 {
		return RunRecoveryObservation{}, errors.New(
			"run recovery ownership TTL must be positive",
		)
	}
	result := observation
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, observation.RunID)
		if err != nil {
			return err
		}
		if err := requireSupervisorRunRecoveryFence(
			ctx,
			tx,
			run,
			&observation,
		); err != nil {
			return err
		}
		expiresAt := time.Now().Add(ttl).UTC().
			Format("2006-01-02T15:04:05.000Z")
		update, err := tx.ExecContext(
			ctx,
			`UPDATE run_reclaim_requests
			 SET recovery_owner_expires_at = ?
			 WHERE run_id = ? AND fence_token = ?
			   AND fence_generation = ?
			   AND recovery_owner_token = ?`,
			expiresAt,
			run.ID,
			observation.RecoveryFenceToken,
			observation.RecoveryFenceGeneration,
			observation.RecoveryOwnerClaimToken,
		)
		if err != nil {
			return err
		}
		rows, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf(
				"%w: run %s recovery owner changed before renewal",
				ErrRunRecoveryOwned,
				run.ID,
			)
		}
		result.ObservedRecoveryOwnerExpiresAt = &expiresAt
		return nil
	})
	if err != nil {
		return RunRecoveryObservation{}, err
	}
	return result, nil
}

func (s *Store) ValidateObservedRunRecoveryOwnership(
	ctx context.Context,
	observation RunRecoveryObservation,
) error {
	run, err := getRun(ctx, s.db, observation.RunID)
	if err != nil {
		return err
	}
	return requireSupervisorRunRecoveryFence(
		ctx,
		s.db,
		run,
		&observation,
	)
}

// ConfirmRunRecoveryQuiescenceInput is the deliberately explicit operator
// authorization needed to release an unsafe recovery fence. Both confirmations
// are mandatory: the command attests that the original worker/process tree has
// stopped and that no host or external agent can still mutate the workspace.
type ConfirmRunRecoveryQuiescenceInput struct {
	RunID                    string
	FenceGeneration          int
	Actor                    string
	Reason                   string
	ConfirmWorkerStopped     bool
	ConfirmHostWritesStopped bool
}

// ConfirmRunRecoveryQuiescence rotates only the operator authorization
// generation. The immutable fence token and any token-scoped managed-host ACK
// remain unchanged. A current recovery owner must first quiesce or expire.
//
// The returned generation is not itself proof that a process is dead. The
// Supervisor must re-inspect liveness and process-tree ownership before it may
// claim a new recovery owner or touch the workspace.
func (s *Store) ConfirmRunRecoveryQuiescence(
	ctx context.Context,
	input ConfirmRunRecoveryQuiescenceInput,
) (DeferredReclaim, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	input.Actor = strings.TrimSpace(input.Actor)
	input.Reason = strings.TrimSpace(input.Reason)
	switch {
	case input.RunID == "":
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation requires a run ID",
		)
	case input.FenceGeneration < 1:
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation requires an exact fence generation",
		)
	case input.Actor == "":
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation requires an actor",
		)
	case len([]byte(input.Actor)) > 256:
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation actor exceeds 256 bytes",
		)
	case input.Reason == "":
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation requires a reason",
		)
	case len([]byte(input.Reason)) > 2048:
		return DeferredReclaim{}, errors.New(
			"operator recovery confirmation reason exceeds 2048 bytes",
		)
	case !input.ConfirmWorkerStopped:
		return DeferredReclaim{}, errors.New(
			"operator must explicitly confirm that the worker process tree stopped",
		)
	case !input.ConfirmHostWritesStopped:
		return DeferredReclaim{}, errors.New(
			"operator must explicitly confirm that host and external writes stopped",
		)
	}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, input.RunID)
		if err != nil {
			return err
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		if run.Status != model.RunStatusRunning ||
			task.Status != model.TaskStatusRunning ||
			task.CurrentRunID == nil ||
			*task.CurrentRunID != run.ID {
			return fmt.Errorf("active run not found: %s", run.ID)
		}
		var fenceToken string
		var fenceGeneration int
		var requiresOperator bool
		var diagnosticCode sql.NullString
		var ownerToken, ownerExpiresAt sql.NullString
		if err := tx.QueryRowContext(
			ctx,
			`SELECT fence_token, fence_generation, requires_operator,
				diagnostic_code, recovery_owner_token,
				recovery_owner_expires_at
			 FROM run_reclaim_requests WHERE run_id = ?`,
			run.ID,
		).Scan(
			&fenceToken,
			&fenceGeneration,
			&requiresOperator,
			&diagnosticCode,
			&ownerToken,
			&ownerExpiresAt,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf(
					"%w: run %s has no recovery fence",
					ErrRunRecoveryFenceNotReady,
					run.ID,
				)
			}
			return err
		}
		if fenceGeneration != input.FenceGeneration {
			return fmt.Errorf(
				"%w: run %s fence generation is %d, not %d",
				ErrRunRecoveryObservationChanged,
				run.ID,
				fenceGeneration,
				input.FenceGeneration,
			)
		}
		if !requiresOperator {
			return fmt.Errorf(
				"%w: run %s does not currently require operator confirmation",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
		if ownerToken.Valid != ownerExpiresAt.Valid {
			return errors.New("run recovery owner lease is malformed")
		}
		if ownerExpiresAt.Valid {
			expires, err := time.Parse(time.RFC3339Nano, ownerExpiresAt.String)
			if err != nil {
				return fmt.Errorf("parse run recovery owner expiry: %w", err)
			}
			if expires.After(time.Now()) {
				return fmt.Errorf(
					"%w: run %s recovery owner must quiesce before confirmation",
					ErrRunRecoveryOwned,
					run.ID,
				)
			}
		}
		var processIdentity sql.NullString
		err = tx.QueryRowContext(
			ctx,
			`SELECT process_identity FROM run_process_identities
			 WHERE run_id = ?`,
			run.ID,
		).Scan(&processIdentity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		timestamp := now()
		nextGeneration := fenceGeneration + 1
		var pid any
		if run.PID != nil {
			pid = *run.PID
		}
		var identity any
		if processIdentity.Valid {
			identity = processIdentity.String
		}
		result, err := tx.ExecContext(ctx, `UPDATE run_reclaim_requests SET
				requires_operator = 0,
				diagnostic_code = NULL,
				fence_generation = ?,
				recovery_owner_token = NULL,
				recovery_owner_expires_at = NULL,
				operator_quiesced_at = ?,
				operator_quiesced_by = ?,
				operator_quiescence_reason = ?,
				operator_quiesced_generation = ?,
				operator_confirmed_worker_stopped = 1,
				operator_confirmed_host_writes_stopped = 1,
				operator_observed_heartbeat_at = ?,
				operator_observed_claim_expires_at = ?,
				operator_observed_pid = ?,
				operator_observed_process_identity = ?
			 WHERE run_id = ? AND fence_token = ?
			   AND fence_generation = ? AND requires_operator = 1`,
			nextGeneration,
			timestamp,
			input.Actor,
			input.Reason,
			nextGeneration,
			run.HeartbeatAt,
			run.ClaimExpiresAt,
			pid,
			identity,
			run.ID,
			fenceToken,
			fenceGeneration,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf(
				"%w: run %s fence changed before operator confirmation",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
		var diagnostic any
		if diagnosticCode.Valid {
			diagnostic = diagnosticCode.String
		}
		return appendEvent(
			ctx,
			tx,
			run.TaskID,
			"recovery_quiescence_confirmed",
			map[string]any{
				"actor":                   input.Actor,
				"reason":                  input.Reason,
				"previousFenceGeneration": fenceGeneration,
				"fenceGeneration":         nextGeneration,
				"workerStopped":           true,
				"hostWritesStopped":       true,
				"diagnosticCode":          diagnostic,
			},
			&run.ID,
		)
	})
	if err != nil {
		return DeferredReclaim{}, err
	}
	reclaim, err := s.GetDeferredReclaim(ctx, input.RunID)
	if err != nil {
		return DeferredReclaim{}, err
	}
	if reclaim == nil {
		return DeferredReclaim{}, fmt.Errorf(
			"%w: run %s recovery fence disappeared after confirmation",
			ErrRunRecoveryObservationChanged,
			input.RunID,
		)
	}
	return *reclaim, nil
}

// AcknowledgeRunRecoveryFence is called only after runClaim has unwound all
// worker and host-owned Git operations. The claim token proves which managed
// host is acknowledging the immutable fence token.
func (s *Store) AcknowledgeRunRecoveryFence(
	ctx context.Context,
	scope RunScope,
	expectedFenceToken string,
	expectedFenceGeneration int,
) (DeferredReclaim, error) {
	expectedFenceToken = strings.TrimSpace(expectedFenceToken)
	if expectedFenceToken == "" || expectedFenceGeneration < 1 {
		return DeferredReclaim{}, errors.New(
			"recovery fence acknowledgment requires an exact token and generation",
		)
	}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRunForRecovery(ctx, tx, scope)
		if err != nil {
			return err
		}
		var fenceToken string
		var fenceGeneration int
		var acknowledgedAt, acknowledgedFenceToken sql.NullString
		if err := tx.QueryRowContext(
			ctx,
			`SELECT fence_token, fence_generation, host_acknowledged_at,
				host_acknowledged_fence_token
			 FROM run_reclaim_requests WHERE run_id = ?`,
			run.ID,
		).Scan(
			&fenceToken,
			&fenceGeneration,
			&acknowledgedAt,
			&acknowledgedFenceToken,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf(
					"%w: run %s has no recovery fence",
					ErrRunRecoveryFenceNotReady,
					run.ID,
				)
			}
			return err
		}
		if strings.TrimSpace(fenceToken) == "" {
			return errors.New("run recovery fence token is empty")
		}
		if fenceToken != expectedFenceToken ||
			fenceGeneration != expectedFenceGeneration {
			return fmt.Errorf(
				"%w: run %s fence generation changed before host acknowledgment",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
		if acknowledgedAt.Valid {
			if !acknowledgedFenceToken.Valid ||
				acknowledgedFenceToken.String != expectedFenceToken {
				return fmt.Errorf(
					"%w: run %s host acknowledgment belongs to another fence",
					ErrRunRecoveryObservationChanged,
					run.ID,
				)
			}
			return nil
		}
		timestamp := now()
		result, err := tx.ExecContext(
			ctx,
			`UPDATE run_reclaim_requests
			 SET host_acknowledged_at = ?,
			     host_acknowledged_fence_token = ?
			 WHERE run_id = ? AND fence_token = ? AND fence_generation = ?
			   AND host_acknowledged_at IS NULL
			   AND host_acknowledged_fence_token IS NULL`,
			timestamp,
			fenceToken,
			run.ID,
			fenceToken,
			fenceGeneration,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf(
				"%w: run %s fence changed before host acknowledgment",
				ErrRunRecoveryObservationChanged,
				run.ID,
			)
		}
		return appendEvent(
			ctx,
			tx,
			task.ID,
			"recovery_host_quiesced",
			map[string]any{
				"fenceGeneration": fenceGeneration,
				"scope":           "immutable_fence_token",
			},
			&run.ID,
		)
	})
	if err != nil {
		return DeferredReclaim{}, err
	}
	reclaim, err := s.GetDeferredReclaim(ctx, scope.RunID)
	if err != nil {
		return DeferredReclaim{}, err
	}
	if reclaim == nil {
		return DeferredReclaim{}, fmt.Errorf(
			"%w: run %s recovery fence disappeared",
			ErrRunRecoveryObservationChanged,
			scope.RunID,
		)
	}
	return *reclaim, nil
}
