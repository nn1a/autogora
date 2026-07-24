package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

const automaticMutationCapabilityCode = "automatic_mutation_containment_unavailable"

type automaticMutationCapability struct {
	Available         bool
	UnsupportedReason string
}

func currentAutomaticMutationCapability() automaticMutationCapability {
	return automaticMutationCapability{
		Available:         processguard.AutomaticMutationContainmentAvailable(),
		UnsupportedReason: processguard.AutomaticMutationContainmentUnsupportedReason(),
	}
}

func (c automaticMutationCapability) reason() string {
	if value := strings.TrimSpace(c.UnsupportedReason); value != "" {
		return value
	}
	return "the platform cannot prove that every descendant process stopped"
}

type automaticMutationCapabilityError struct {
	Target string
	Reason string
}

func (e *automaticMutationCapabilityError) Error() string {
	if e == nil {
		return automaticMutationCapabilityCode
	}
	target := strings.TrimSpace(e.Target)
	if target == "" {
		target = "automatic mutation"
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "verifiable process-tree teardown is unavailable"
	}
	return fmt.Sprintf(
		"%s: %s was not started because %s",
		automaticMutationCapabilityCode,
		target,
		reason,
	)
}

func (e *automaticMutationCapabilityError) Unwrap() error {
	return processguard.ErrAutomaticMutationContainmentUnavailable
}

func automaticMutationCapabilityFailure(
	capability automaticMutationCapability,
	target string,
) error {
	if capability.Available {
		return nil
	}
	return &automaticMutationCapabilityError{
		Target: target,
		Reason: capability.reason(),
	}
}

func taskUsesAutomaticWorkspace(task model.Task) bool {
	if task.WorkspaceKind == model.WorkspaceWorktree {
		return false
	}
	if task.Workspace == nil {
		return true
	}
	value := strings.TrimSpace(*task.Workspace)
	return value == "" || value == "scratch"
}

// automaticWorkerMutationTarget classifies the mutation before workspace
// preparation. A worktree always requires host Git mutation containment,
// including a nominally read-only worker, because preparation and prerequisite
// integration are host-owned. A plain directory requires containment only
// when the worker can write. Scratch work remains isolated and is allowed.
//
// A read-only automatic workspace with a board default is resolved inside
// workspace.Manager. Its final add-worktree boundary performs a second,
// fail-closed capability check.
func automaticWorkerMutationTarget(
	task model.Task,
	existing *model.RunWorkspace,
	boardDefaultWorkdir bool,
	allowWrites bool,
) (string, bool) {
	if existing != nil {
		switch existing.Kind {
		case model.WorkspaceWorktree:
			return "automatic Git worktree preparation, integration, and checkpoint capture", true
		case model.WorkspaceDir:
			if allowWrites {
				return "automatic writable directory worker", true
			}
			return "", false
		case model.WorkspaceScratch:
			return "", false
		default:
			return "automatic worker with an unknown workspace kind", true
		}
	}

	value := ""
	if task.Workspace != nil {
		value = strings.TrimSpace(*task.Workspace)
	}
	if task.WorkspaceKind == model.WorkspaceWorktree ||
		value == "worktree" ||
		strings.HasPrefix(value, "worktree:") {
		return "automatic Git worktree preparation, integration, and checkpoint capture", true
	}
	if value != "" && value != "scratch" {
		if allowWrites {
			return "automatic writable directory worker", true
		}
		return "", false
	}
	if boardDefaultWorkdir && allowWrites {
		return "automatic writable board default workspace", true
	}
	return "", false
}

func capabilityBlockApplied(
	inspection store.RunInspection,
	reason string,
) bool {
	if inspection.Run.Status != model.RunStatusBlocked ||
		inspection.Task.CurrentRunID != nil ||
		inspection.Task.BlockKind == nil ||
		*inspection.Task.BlockKind != model.BlockKindCapability ||
		inspection.Task.BlockReason == nil {
		return false
	}
	return *inspection.Task.BlockReason == reason
}

// persistAutomaticMutationCapabilityBlock records an exact-run capability
// outcome. It handles both unmanaged claims, which usually finalize in
// BlockRun, and managed claims, which require explicit host finalization. The
// reconciliation also covers a committed write whose response was lost.
func persistAutomaticMutationCapabilityBlock(
	ctx context.Context,
	opened *store.Store,
	scope store.RunScope,
	reason string,
) error {
	_, requestErr := retryStoreOperation(ctx, func() (model.TaskDetail, error) {
		return opened.BlockRun(ctx, scope, store.BlockInput{
			Reason: reason,
			Kind:   model.BlockKindCapability,
		})
	})
	inspection, inspectErr := retryStoreOperation(
		ctx,
		func() (store.RunInspection, error) {
			return opened.GetRun(ctx, scope.RunID)
		},
	)
	if inspectErr == nil && capabilityBlockApplied(inspection, reason) {
		return nil
	}

	request, terminalErr := retryStoreOperation(
		ctx,
		func() (*model.TerminalRequest, error) {
			return opened.GetRunTerminalRequest(ctx, scope.RunID)
		},
	)
	if inspectErr == nil &&
		inspection.Run.Status == model.RunStatusRunning &&
		pendingBlockRequestMatches(
			request,
			reason,
			model.BlockKindCapability,
		) {
		_, finalizeErr := retryStoreOperation(
			ctx,
			func() (model.TaskDetail, error) {
				return opened.FinalizeRunTerminal(ctx, scope, 1)
			},
		)
		reconciled, reconcileErr := retryStoreOperation(
			ctx,
			func() (store.RunInspection, error) {
				return opened.GetRun(ctx, scope.RunID)
			},
		)
		if reconcileErr == nil &&
			capabilityBlockApplied(reconciled, reason) {
			return nil
		}
		return errors.Join(
			requestErr,
			finalizeErr,
			reconcileErr,
		)
	}
	return errors.Join(requestErr, inspectErr, terminalErr)
}

func blockUnsupportedAutomaticClaim(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	claim *model.ClaimedTask,
	options Options,
	capability automaticMutationCapability,
) (bool, error) {
	if capability.Available || claim == nil {
		return false, nil
	}
	existing, err := opened.GetRunWorkspace(ctx, claim.Run.ID)
	if err != nil {
		return false, err
	}
	boardDefaultWorkdir := false
	if taskUsesAutomaticWorkspace(claim.Task.Task) && existing == nil {
		metadata, metadataErr := manager.Read(claim.Task.Task.Board)
		if metadataErr != nil {
			failure := automaticMutationCapabilityFailure(
				capability,
				"automatic worker whose workspace mutation class could not be verified",
			)
			durable, cancel := durableContext()
			defer cancel()
			if persistErr := persistAutomaticMutationCapabilityBlock(
				durable,
				opened,
				store.RunScope{
					RunID:      claim.Run.ID,
					ClaimToken: claim.ClaimToken,
				},
				failure.Error(),
			); persistErr != nil {
				return true, errors.Join(persistErr, metadataErr)
			}
			options.log(
				"blocked %s because its workspace mutation class could not be verified: %v",
				claim.Task.Task.ID,
				metadataErr,
			)
			return true, nil
		}
		boardDefaultWorkdir = metadata.DefaultWorkdir != nil
	}
	target, requiresContainment := automaticWorkerMutationTarget(
		claim.Task.Task,
		existing,
		boardDefaultWorkdir,
		options.AllowWrites,
	)
	if !requiresContainment {
		return false, nil
	}
	failure := automaticMutationCapabilityFailure(capability, target)
	durable, cancel := durableContext()
	defer cancel()
	if err := persistAutomaticMutationCapabilityBlock(
		durable,
		opened,
		store.RunScope{
			RunID:      claim.Run.ID,
			ClaimToken: claim.ClaimToken,
		},
		failure.Error(),
	); err != nil {
		return true, err
	}
	options.log(
		"blocked %s before automatic mutation: %v",
		claim.Task.Task.ID,
		failure,
	)
	return true, nil
}

func automaticPublicationCapabilityFailure(
	publication model.Publication,
	capability automaticMutationCapability,
) error {
	if capability.Available {
		return nil
	}
	switch publication.Mode {
	case model.PublicationModeLocalFF, model.PublicationModePullRequest:
		return automaticMutationCapabilityFailure(
			capability,
			"automatic "+string(publication.Mode)+" publication",
		)
	default:
		return nil
	}
}

func automaticRecoveryCapabilityFailure(
	workspace *model.RunWorkspace,
	capability automaticMutationCapability,
) error {
	if workspace == nil || workspace.Kind != model.WorkspaceWorktree {
		return nil
	}
	return automaticMutationCapabilityFailure(
		capability,
		"automatic abandoned-run Git checkpoint and finalizer recovery",
	)
}
