package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type recoverySetupError struct {
	cause        error
	terminalSafe bool
}

func (e *recoverySetupError) Error() string { return e.cause.Error() }
func (e *recoverySetupError) Unwrap() error { return e.cause }

func recoverySetupCanTerminalize(err error) bool {
	var setup *recoverySetupError
	return errors.As(err, &setup) && setup.terminalSafe
}

func workspaceRecoveryCheckpoint(value model.RecoveryCheckpoint) workspace.RecoveryCheckpoint {
	return workspace.RecoveryCheckpoint{
		RunID: value.SourceRunID, RepositoryPath: value.RepositoryPath,
		WorktreePath:      value.WorktreePath,
		OutputBaseCommit:  value.OutputBaseCommit,
		SourceStartCommit: value.StartCommit,
		HeadCommit:        value.HeadCommit, DurableRef: value.DurableRef,
		ChangedFiles: append([]string(nil), value.ChangedFiles...),
	}
}

func storeRecoveryCheckpointInput(
	snapshot workspace.RecoveryCheckpoint,
	taskFingerprint string,
	prerequisiteFingerprint string,
) store.RegisterRecoveryCheckpointInput {
	return store.RegisterRecoveryCheckpointInput{
		RepositoryPath:          snapshot.RepositoryPath,
		WorktreePath:            snapshot.WorktreePath,
		OutputBaseCommit:        snapshot.OutputBaseCommit,
		StartCommit:             snapshot.SourceStartCommit,
		HeadCommit:              snapshot.HeadCommit,
		DurableRef:              snapshot.DurableRef,
		ChangedFiles:            append([]string(nil), snapshot.ChangedFiles...),
		TaskSpecFingerprint:     taskFingerprint,
		PrerequisiteFingerprint: prerequisiteFingerprint,
	}
}

// reserveRecoveryCheckpoint fences compatible partial work before any
// prerequisite integration or conflict resolver can start. A stale semantic
// fingerprint is terminally superseded by Store and returns no reservation.
func reserveRecoveryCheckpoint(
	ctx context.Context,
	opened *store.Store,
	prepared *model.ClaimedTask,
) (*model.RecoveryCheckpoint, error) {
	if prepared == nil || prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree {
		return nil, nil
	}
	active, err := opened.GetActiveRecoveryCheckpoint(ctx, prepared.Task.Task.ID)
	if err != nil || active == nil {
		return nil, err
	}
	latest, err := opened.GetTask(ctx, prepared.Task.Task.ID)
	if err != nil {
		return nil, err
	}
	taskFingerprint, prerequisiteFingerprint := recoverySemanticFingerprints(latest)
	scope := store.RunScope{RunID: prepared.Run.ID, ClaimToken: prepared.ClaimToken}
	type reservationResult struct {
		checkpoint *model.RecoveryCheckpoint
		won        bool
	}
	reservation, err := retryStoreOperation(ctx, func() (reservationResult, error) {
		reserved, won, reserveErr := opened.ReserveRecoveryCheckpoint(
			ctx,
			scope,
			store.ReserveRecoveryCheckpointInput{
				CheckpointID:            active.ID,
				TaskSpecFingerprint:     taskFingerprint,
				PrerequisiteFingerprint: prerequisiteFingerprint,
			},
		)
		return reservationResult{checkpoint: reserved, won: won}, reserveErr
	})
	if err != nil {
		return nil, err
	}
	reserved, won := reservation.checkpoint, reservation.won
	if !won || reserved == nil {
		effectiveRuntime := prepared.Task.Task.Runtime
		prepared.Task = latest
		prepared.Task.Task.Runtime = effectiveRuntime
		return nil, nil
	}
	effectiveRuntime := prepared.Task.Task.Runtime
	prepared.Task = latest
	prepared.Task.Task.Runtime = effectiveRuntime
	return reserved, nil
}

// adoptReservedRecoveryCheckpoint changes only a clean, prerequisite-integrated
// worktree and confirms the exact resulting HEAD. Its rollback/release paths
// keep the previous immutable checkpoint retryable when setup fails.
func adoptReservedRecoveryCheckpoint(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
	reserved *model.RecoveryCheckpoint,
) (*model.RecoveryCheckpoint, error) {
	if reserved == nil {
		return nil, nil
	}
	scope := store.RunScope{RunID: prepared.Run.ID, ClaimToken: prepared.ClaimToken}
	adoption, err := workspaces.AdoptRecoveryCheckpoint(
		ctx,
		*prepared.Workspace,
		workspaceRecoveryCheckpoint(*reserved),
	)
	if err != nil {
		_, releaseErr := retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
			return opened.ReleaseRecoveryCheckpointReservation(
				ctx,
				scope,
				reserved.ID,
				reserved.ReservationToken,
			)
		})
		return nil, &recoverySetupError{
			cause: fmt.Errorf(
				"adopt recovery checkpoint %s: %w",
				reserved.ID,
				errors.Join(err, releaseErr),
			),
			terminalSafe: releaseErr == nil,
		}
	}

	confirmed, err := retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
		return opened.ConfirmRecoveryCheckpointAdoption(
			ctx,
			scope,
			reserved.ID,
			reserved.ReservationToken,
			adoption.OutputBaseCommit,
			adoption.AdoptedHeadCommit,
		)
	})
	if err != nil {
		// Reconcile an ambiguous database response before touching Git. If the
		// confirmation committed, its idempotent values are authoritative.
		current, inspectErr := opened.GetRunRecoveryCheckpoint(ctx, prepared.Run.ID)
		if inspectErr == nil && current != nil &&
			current.State == model.RecoveryCheckpointAdopted &&
			current.AdoptedOutputBaseCommit != nil &&
			*current.AdoptedOutputBaseCommit == adoption.OutputBaseCommit &&
			current.AdoptedHeadCommit != nil &&
			*current.AdoptedHeadCommit == adoption.AdoptedHeadCommit {
			confirmed = *current
			err = nil
		} else if inspectErr == nil && current != nil &&
			current.State == model.RecoveryCheckpointReserved {
			rollbackErr := workspaces.RollbackRecoveryCheckpointAdoption(ctx, adoption)
			var releaseErr error
			if rollbackErr == nil {
				_, releaseErr = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
					return opened.ReleaseRecoveryCheckpointReservation(
						ctx,
						scope,
						reserved.ID,
						reserved.ReservationToken,
					)
				})
			}
			return nil, &recoverySetupError{
				cause: fmt.Errorf(
					"confirm recovery checkpoint %s adoption: %w",
					reserved.ID,
					errors.Join(err, rollbackErr, releaseErr),
				),
				terminalSafe: rollbackErr == nil && releaseErr == nil,
			}
		} else {
			return nil, &recoverySetupError{
				cause: fmt.Errorf(
					"reconcile recovery checkpoint %s adoption: %w",
					reserved.ID,
					errors.Join(err, inspectErr),
				),
			}
		}
	}

	prepared.RecoveryCheckpoint = &confirmed
	return &confirmed, nil
}

// reserveAndAdoptRecoveryCheckpoint is retained for callers that already have
// a clean, prerequisite-integrated target. runClaim reserves earlier so a
// Finalizer conflict cannot silently bypass the checkpoint.
func reserveAndAdoptRecoveryCheckpoint(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
) (*model.RecoveryCheckpoint, error) {
	reserved, err := reserveRecoveryCheckpoint(ctx, opened, prepared)
	if err != nil || reserved == nil {
		return nil, err
	}
	return adoptReservedRecoveryCheckpoint(
		ctx,
		opened,
		workspaces,
		prepared,
		reserved,
	)
}

// adoptReservedRecoveryCheckpointAfterIntegration layers partial work on the
// clean resolver commit. Store then atomically promotes that commit to the run
// base, confirms checkpoint adoption, closes the resolution handoff, and
// discards the resolver's premature completion request. A fresh Finalizer turn
// must verify the combined result before Done.
func adoptReservedRecoveryCheckpointAfterIntegration(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
	reserved *model.RecoveryCheckpoint,
	resolvedOutputBase string,
) (*model.RecoveryCheckpoint, error) {
	if prepared == nil || prepared.Workspace == nil ||
		prepared.IntegrationResolution == nil || reserved == nil {
		return nil, errors.New(
			"post-resolution recovery adoption requires a resolver and reservation",
		)
	}
	target := *prepared.Workspace
	target.BaseCommit = &resolvedOutputBase
	adoption, err := workspaces.AdoptRecoveryCheckpoint(
		ctx,
		target,
		workspaceRecoveryCheckpoint(*reserved),
	)
	if err != nil {
		// Adoption rolls back its own Git mutations. Revalidate the clean
		// resolver result before allowing terminal block/release.
		_, validationErr := workspaces.ValidateIntegrationResolutionResult(
			ctx,
			opened,
			prepared,
		)
		return nil, &recoverySetupError{
			cause: fmt.Errorf(
				"compose recovery checkpoint after integration resolution: %w",
				errors.Join(err, validationErr),
			),
			terminalSafe: validationErr == nil,
		}
	}

	scope := store.RunScope{
		RunID:      prepared.Run.ID,
		ClaimToken: prepared.ClaimToken,
	}
	confirmed, err := retryStoreOperation(
		ctx,
		func() (model.RecoveryCheckpoint, error) {
			return opened.ConfirmRecoveryAfterIntegrationResolution(
				ctx,
				scope,
				reserved.ID,
				reserved.ReservationToken,
				resolvedOutputBase,
				adoption.AdoptedHeadCommit,
			)
		},
	)
	if err != nil {
		current, checkpointErr := opened.GetRunRecoveryCheckpoint(
			ctx,
			prepared.Run.ID,
		)
		resolutionOpen, resolutionErr := opened.HasRunIntegrationResolution(
			ctx,
			prepared.Run.ID,
		)
		request, requestErr := opened.GetRunTerminalRequest(
			ctx,
			prepared.Run.ID,
		)
		bound, workspaceErr := opened.GetRunWorkspace(
			ctx,
			prepared.Run.ID,
		)
		if checkpointErr == nil && resolutionErr == nil &&
			requestErr == nil && workspaceErr == nil &&
			current != nil &&
			current.State == model.RecoveryCheckpointAdopted &&
			current.AdoptedOutputBaseCommit != nil &&
			*current.AdoptedOutputBaseCommit == resolvedOutputBase &&
			current.AdoptedHeadCommit != nil &&
			*current.AdoptedHeadCommit == adoption.AdoptedHeadCommit &&
			!resolutionOpen && request == nil && bound != nil &&
			bound.BaseCommit != nil &&
			*bound.BaseCommit == resolvedOutputBase {
			confirmed = *current
			err = nil
		} else if checkpointErr == nil && resolutionErr == nil &&
			requestErr == nil && workspaceErr == nil &&
			current != nil &&
			current.State == model.RecoveryCheckpointReserved &&
			current.ReservedRunID != nil &&
			*current.ReservedRunID == prepared.Run.ID &&
			current.ReservationToken == reserved.ReservationToken &&
			resolutionOpen && request != nil &&
			request.Kind == "complete" &&
			request.FinalizedAt == nil {
			rollbackErr := workspaces.RollbackRecoveryCheckpointAdoption(
				ctx,
				adoption,
			)
			return nil, &recoverySetupError{
				cause: fmt.Errorf(
					"confirm post-resolution recovery checkpoint %s: %w",
					reserved.ID,
					errors.Join(err, rollbackErr),
				),
				terminalSafe: rollbackErr == nil,
			}
		} else {
			return nil, &recoverySetupError{
				cause: fmt.Errorf(
					"reconcile post-resolution recovery checkpoint %s: %w",
					reserved.ID,
					errors.Join(
						err,
						checkpointErr,
						resolutionErr,
						requestErr,
						workspaceErr,
					),
				),
			}
		}
	}

	prepared.Workspace.BaseCommit = &resolvedOutputBase
	prepared.RecoveryCheckpoint = &confirmed
	return &confirmed, nil
}

func captureRecoverySnapshot(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
) (workspace.RecoveryCheckpoint, store.RegisterRecoveryCheckpointInput, error) {
	if prepared == nil || prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree ||
		prepared.Workspace.BaseCommit == nil {
		return workspace.RecoveryCheckpoint{}, store.RegisterRecoveryCheckpointInput{},
			errors.New("partial-work recovery requires a prepared Git worktree")
	}
	startCommit := strings.TrimSpace(*prepared.Workspace.BaseCommit)
	if checkpoint := prepared.RecoveryCheckpoint; checkpoint != nil {
		if checkpoint.State != model.RecoveryCheckpointAdopted ||
			checkpoint.AdoptedOutputBaseCommit == nil ||
			strings.TrimSpace(*checkpoint.AdoptedOutputBaseCommit) == "" ||
			checkpoint.AdoptedHeadCommit == nil ||
			strings.TrimSpace(*checkpoint.AdoptedHeadCommit) == "" {
			return workspace.RecoveryCheckpoint{}, store.RegisterRecoveryCheckpointInput{},
				errors.New("recovery run has not durably adopted its checkpoint")
		}
		if !strings.EqualFold(
			strings.TrimSpace(*checkpoint.AdoptedOutputBaseCommit),
			strings.TrimSpace(*prepared.Workspace.BaseCommit),
		) {
			return workspace.RecoveryCheckpoint{}, store.RegisterRecoveryCheckpointInput{},
				errors.New("recovery run workspace base differs from its adopted output base")
		}
		startCommit = *checkpoint.AdoptedHeadCommit
	}
	latest, err := opened.GetTask(ctx, prepared.Task.Task.ID)
	if err != nil {
		return workspace.RecoveryCheckpoint{}, store.RegisterRecoveryCheckpointInput{}, err
	}
	taskFingerprint, prerequisiteFingerprint := recoverySemanticFingerprints(latest)
	snapshot, err := workspaces.CaptureRecoveryCheckpoint(
		ctx,
		*prepared.Workspace,
		startCommit,
		latest.Task.ID,
		latest.Task.Title,
	)
	if err != nil {
		return workspace.RecoveryCheckpoint{}, store.RegisterRecoveryCheckpointInput{}, err
	}
	return snapshot, storeRecoveryCheckpointInput(
		snapshot,
		taskFingerprint,
		prerequisiteFingerprint,
	), nil
}

// checkpointManagedRunFailure atomically publishes a hidden partial-work
// checkpoint and terminalizes the failed run. It returns false only when the
// worktree has no run-owned work and ordinary failure handling is safe.
func checkpointManagedRunFailure(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
	scope store.RunScope,
	runError string,
	failure store.FailRunOptions,
) (bool, error) {
	if prepared == nil || prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree {
		return false, nil
	}
	inspection, err := workspaces.InspectChanges(ctx, *prepared.Workspace)
	if err != nil {
		return false, err
	}
	if !inspection.Changed && prepared.RecoveryCheckpoint == nil {
		return false, nil
	}
	_, input, err := captureRecoverySnapshot(ctx, opened, workspaces, prepared)
	if err != nil {
		return false, err
	}
	if checkpoint := prepared.RecoveryCheckpoint; checkpoint != nil {
		_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
			replacement, _, callErr := opened.SupersedeRecoveryCheckpointAndFailRun(
				ctx,
				scope,
				checkpoint.ID,
				checkpoint.ReservationToken,
				input,
				runError,
				failure,
			)
			return replacement, callErr
		})
	} else {
		_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
			checkpoint, _, callErr := opened.RegisterRecoveryCheckpointAndFailRun(
				ctx,
				scope,
				input,
				runError,
				failure,
			)
			return checkpoint, callErr
		})
	}
	return err == nil, err
}

// checkpointManagedRunBlock is used only after the dispatcher knows no worker
// process can still write. It preserves partial work and blocks the task in one
// store transaction, including when a pending completion request must be
// replaced by a conservative recovery block.
func checkpointManagedRunBlock(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	prepared *model.ClaimedTask,
	scope store.RunScope,
	reason string,
	kind model.BlockKind,
	exitCode int,
) (bool, error) {
	if prepared == nil || prepared.IntegrationResolution != nil ||
		prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree {
		return false, nil
	}
	inspection, err := workspaces.InspectChanges(ctx, *prepared.Workspace)
	if err != nil {
		return false, err
	}
	if !inspection.Changed && prepared.RecoveryCheckpoint == nil {
		return false, nil
	}
	_, input, err := captureRecoverySnapshot(ctx, opened, workspaces, prepared)
	if err != nil {
		return false, err
	}
	blocked := store.RecoverBlockedRunInput{
		Outcome:  model.RunStatusBlocked,
		Reason:   reason,
		Kind:     kind,
		ExitCode: &exitCode,
	}
	if prepared.RecoveryCheckpoint != nil {
		_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
			checkpoint, _, callErr := opened.SupersedeRecoveryCheckpointAndRecoverRunBlocked(
				ctx,
				scope,
				input,
				blocked,
			)
			return checkpoint, callErr
		})
	} else {
		_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
			checkpoint, _, callErr := opened.RegisterRecoveryCheckpointAndRecoverRunBlocked(
				ctx,
				scope,
				input,
				blocked,
			)
			return checkpoint, callErr
		})
	}
	return err == nil, err
}

// checkpointAbandonedRunFailure is the tokenless Supervisor counterpart. The
// caller must first prove that the recorded process tree is no longer alive.
func checkpointAbandonedRunFailure(
	ctx context.Context,
	opened *store.Store,
	workspaces *workspace.Manager,
	runID string,
	taskID string,
	observation store.RunRecoveryObservation,
	bound *model.RunWorkspace,
	terminalRequest *model.TerminalRequest,
	outcome model.RunStatus,
	reason string,
	countFailure bool,
) (bool, error) {
	validateOwnership := func() error {
		return opened.ValidateObservedRunRecoveryOwnership(
			ctx,
			observation,
		)
	}
	if err := validateOwnership(); err != nil {
		return false, err
	}
	if bound == nil || bound.Kind != model.WorkspaceWorktree ||
		bound.BaseCommit == nil {
		return false, nil
	}
	recovery, err := opened.GetRunRecoveryCheckpoint(ctx, runID)
	if err != nil {
		return false, err
	}
	inspection, inspectErr := workspaces.InspectChanges(ctx, *bound)
	if err := validateOwnership(); err != nil {
		return false, err
	}
	startCommit := strings.TrimSpace(*bound.BaseCommit)
	if recovery != nil {
		if recovery.State != model.RecoveryCheckpointAdopted ||
			recovery.AdoptedOutputBaseCommit == nil ||
			recovery.AdoptedHeadCommit == nil {
			return false, errors.New(
				"abandoned run has an unadopted recovery reservation",
			)
		}
		startCommit = *recovery.AdoptedHeadCommit
	}
	var snapshot workspace.RecoveryCheckpoint
	if inspectErr != nil {
		_, statErr := os.Stat(bound.Path)
		if !errors.Is(statErr, os.ErrNotExist) {
			return false, inspectErr
		}
		published, loadErr := workspaces.LoadPublishedRecoveryCheckpoint(
			ctx,
			*bound,
			startCommit,
		)
		if err := validateOwnership(); err != nil {
			return false, err
		}
		if loadErr != nil {
			return false, errors.Join(inspectErr, loadErr)
		}
		switch {
		case published != nil:
			snapshot = *published
			reason += "; the recovery worktree was missing, so Autogora restored its already-published immutable checkpoint"
		case recovery != nil:
			snapshot, err = workspaces.ReissueAdoptedRecoveryCheckpoint(
				ctx,
				workspaceRecoveryCheckpoint(*recovery),
				runID,
				bound.Path,
				*recovery.AdoptedOutputBaseCommit,
				*recovery.AdoptedHeadCommit,
			)
			if ownershipErr := validateOwnership(); ownershipErr != nil {
				return false, ownershipErr
			}
			if err != nil {
				return false, errors.Join(inspectErr, err)
			}
			reason += "; the recovery worktree was missing, so Autogora preserved the last confirmed adopted checkpoint"
		default:
			return false, inspectErr
		}
	} else if !inspection.Changed && recovery == nil {
		return false, nil
	}
	latest, err := opened.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	taskFingerprint, prerequisiteFingerprint := recoverySemanticFingerprints(latest)
	if snapshot.HeadCommit == "" {
		if err := validateOwnership(); err != nil {
			return false, err
		}
		snapshot, err = workspaces.CaptureRecoveryCheckpoint(
			ctx,
			*bound,
			startCommit,
			latest.Task.ID,
			latest.Task.Title,
		)
		if err != nil {
			return false, err
		}
		if err := validateOwnership(); err != nil {
			return false, err
		}
	}
	input := storeRecoveryCheckpointInput(
		snapshot,
		taskFingerprint,
		prerequisiteFingerprint,
	)
	if terminalRequest != nil && terminalRequest.FinalizedAt == nil {
		if terminalRequest.Kind == "block" {
			if recovery != nil {
				_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
					checkpoint, _, callErr := opened.SupersedeRecoveryCheckpointAndFinalizeObservedStoppedBlock(
						ctx,
						observation,
						input,
						0,
					)
					return checkpoint, callErr
				})
			} else {
				_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
					checkpoint, _, callErr := opened.RegisterRecoveryCheckpointAndFinalizeObservedStoppedBlock(
						ctx,
						observation,
						input,
						0,
					)
					return checkpoint, callErr
				})
			}
			return err == nil, err
		}
		blockReason := reason
		if terminalRequest.Kind == "complete" {
			blockReason = "Worker requested completion, but Autogora could not verify finalization after the process stopped; partial work was checkpointed"
		}
		blocked := store.RecoverBlockedRunInput{
			Outcome: outcome,
			Reason:  blockReason,
			Kind:    model.BlockKindNeedsInput,
		}
		if recovery != nil {
			_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
				checkpoint, _, callErr := opened.SupersedeRecoveryCheckpointAndRecoverObservedRunBlocked(
					ctx,
					observation,
					input,
					blocked,
				)
				return checkpoint, callErr
			})
		} else {
			_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
				checkpoint, _, callErr := opened.RegisterRecoveryCheckpointAndRecoverObservedRunBlocked(
					ctx,
					observation,
					input,
					blocked,
				)
				return checkpoint, callErr
			})
		}
		return err == nil, err
	}
	_, err = retryStoreOperation(ctx, func() (model.RecoveryCheckpoint, error) {
		checkpoint, _, callErr := opened.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
			ctx,
			observation,
			input,
			outcome,
			reason,
			countFailure,
		)
		return checkpoint, callErr
	})
	return err == nil, err
}
