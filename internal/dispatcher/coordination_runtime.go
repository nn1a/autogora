package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/coordinator"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

const (
	coordinationRetryBackoffBase  = 5 * time.Second
	coordinationRetryBackoffLimit = 5 * time.Minute
	coordinationClaimGrace        = 15 * time.Second
)

type coordinationCandidate struct {
	board    string
	metadata boards.Metadata
	incident model.CoordinationIncident
	mode     boards.CoordinationMode
}

func coordinationSeverityRank(value model.CoordinationSeverity) int {
	switch value {
	case model.CoordinationSeverityCritical:
		return 4
	case model.CoordinationSeverityError:
		return 3
	case model.CoordinationSeverityWarning:
		return 2
	default:
		return 1
	}
}

func sortCoordinationIncidents(values []model.CoordinationIncident) {
	sort.SliceStable(values, func(i, j int) bool {
		left, right := coordinationSeverityRank(values[i].Severity), coordinationSeverityRank(values[j].Severity)
		if left != right {
			return left > right
		}
		if values[i].CreatedAt != values[j].CreatedAt {
			return values[i].CreatedAt < values[j].CreatedAt
		}
		return values[i].ID < values[j].ID
	})
}

func coordinationClaimExpiredAt(incident model.CoordinationIncident, current time.Time) bool {
	if incident.Status != model.CoordinationIncidentCoordinating ||
		incident.ClaimExpiresAt == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, *incident.ClaimExpiresAt)
	return err == nil && !expires.After(current)
}

func coordinationIncidentCanRun(incident model.CoordinationIncident, current time.Time) bool {
	return incident.Status == model.CoordinationIncidentOpen ||
		coordinationClaimExpiredAt(incident, current)
}

func coordinationRetryDelay(failures int) time.Duration {
	delay := coordinationRetryBackoffBase
	for step := 1; step < failures && delay < coordinationRetryBackoffLimit; step++ {
		if delay > coordinationRetryBackoffLimit/2 {
			return coordinationRetryBackoffLimit
		}
		delay *= 2
	}
	return min(delay, coordinationRetryBackoffLimit)
}

func coordinationRetryReady(
	ctx context.Context,
	opened *store.Store,
	incidentID string,
	current time.Time,
) (bool, error) {
	attempts, err := opened.ListCoordinationAttempts(ctx, store.CoordinationAttemptFilter{
		IncidentID: incidentID, Limit: 32,
	})
	if err != nil {
		return false, err
	}
	failures := 0
	var lastEnded time.Time
	for _, attempt := range attempts {
		if attempt.Status != model.CoordinationAttemptFailed {
			break
		}
		failures++
		if failures == 1 && attempt.EndedAt != nil {
			lastEnded, _ = time.Parse(time.RFC3339Nano, *attempt.EndedAt)
		}
	}
	if failures == 0 || lastEnded.IsZero() {
		return true, nil
	}
	return !current.Before(lastEnded.Add(coordinationRetryDelay(failures))), nil
}

func activeCoordinationCandidates(
	ctx context.Context,
	opened *store.Store,
	metadata boards.Metadata,
	current time.Time,
) ([]model.CoordinationIncident, error) {
	incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		Board: metadata.Slug, Limit: 500,
	})
	if err != nil {
		return nil, err
	}
	result := make([]model.CoordinationIncident, 0, len(incidents))
	for _, incident := range incidents {
		if !coordinationIncidentCanRun(incident, current) {
			continue
		}
		eligible, err := coordinatorIncidentEligible(ctx, opened, incident)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		ready, err := coordinationRetryReady(ctx, opened, incident.ID, current)
		if err != nil {
			return nil, err
		}
		if ready {
			result = append(result, incident)
		}
	}
	sortCoordinationIncidents(result)
	return result, nil
}

func collectCoordinationCandidates(
	ctx context.Context,
	manager *boards.Manager,
	boardSlugs []string,
	options Options,
	state *coordinationRuntimeState,
	current time.Time,
) ([]coordinationCandidate, []string, error) {
	if !options.Autopilot {
		return nil, nil, nil
	}
	next := ""
	if state != nil {
		next = state.nextBoard
	}
	order := rotatedBoardSlugs(boardSlugs, next)
	candidates := make([]coordinationCandidate, 0, len(order))
	observedBoards := make([]string, 0, len(order))
	var observationErr error
	for _, board := range order {
		if err := ctx.Err(); err != nil {
			return nil, observedBoards, err
		}
		// Count every board in the rotation even when its metadata or store is
		// unavailable. A broken board must not pin the fairness cursor.
		observedBoards = append(observedBoards, board)
		metadata, err := manager.Read(board)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, observedBoards, ctxErr
			}
			observationErr = errors.Join(
				observationErr,
				fmt.Errorf("observe coordination board %q metadata: %w", board, err),
			)
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observedBoards, ctxErr
		}
		if metadata.Archived || !metadata.Orchestration.Autopilot.Enabled {
			continue
		}
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, observedBoards, ctxErr
			}
			observationErr = errors.Join(
				observationErr,
				fmt.Errorf("observe coordination board %q store: %w", board, err),
			)
			continue
		}
		reconcileErr := reconcilePendingCoordination(
			ctx, manager, opened, metadata, options, current,
		)
		if reconcileErr == nil {
			_, reconcileErr = reconcileCoordinatorIncidents(
				ctx, manager, opened, metadata, options, current,
			)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = opened.Close()
			return nil, observedBoards, ctxErr
		}
		if reconcileErr != nil {
			closeErr := opened.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, observedBoards, ctxErr
			}
			observationErr = errors.Join(
				observationErr,
				fmt.Errorf("observe coordination board %q reconciliation: %w", board, reconcileErr),
			)
			if closeErr != nil {
				observationErr = errors.Join(
					observationErr,
					fmt.Errorf("observe coordination board %q close: %w", board, closeErr),
				)
			}
			continue
		}
		if metadata.Orchestration.Autopilot.Coordination.Mode != boards.CoordinationModeObserve {
			incidents, candidateErr := activeCoordinationCandidates(ctx, opened, metadata, current)
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = opened.Close()
				return nil, observedBoards, ctxErr
			}
			if candidateErr != nil {
				closeErr := opened.Close()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, observedBoards, ctxErr
				}
				observationErr = errors.Join(
					observationErr,
					fmt.Errorf("observe coordination board %q candidates: %w", board, candidateErr),
				)
				if closeErr != nil {
					observationErr = errors.Join(
						observationErr,
						fmt.Errorf("observe coordination board %q close: %w", board, closeErr),
					)
				}
				continue
			}
			if len(incidents) > 0 {
				candidates = append(candidates, coordinationCandidate{
					board: board, metadata: metadata, incident: incidents[0],
					mode: metadata.Orchestration.Autopilot.Coordination.Mode,
				})
			}
		}
		closeErr := opened.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observedBoards, ctxErr
		}
		if closeErr != nil {
			observationErr = errors.Join(
				observationErr,
				fmt.Errorf("observe coordination board %q close: %w", board, closeErr),
			)
		}
	}
	return candidates, observedBoards, observationErr
}

func runCoordinationPass(
	ctx context.Context,
	manager *boards.Manager,
	boardSlugs []string,
	options Options,
	state *coordinationRuntimeState,
	current time.Time,
) error {
	if current.IsZero() {
		current = time.Now().UTC()
	} else {
		current = current.UTC()
	}
	candidates, observedBoards, err := collectCoordinationCandidates(
		ctx, manager, boardSlugs, options, state, current,
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if len(candidates) == 0 {
		if state != nil && len(observedBoards) > 0 {
			state.nextBoard = boardAfter(observedBoards, observedBoards[0])
		}
		return err
	}
	candidate := candidates[0]
	if state != nil {
		state.nextBoard = boardAfter(observedBoards, candidate.board)
	}
	coordinationErr := coordinateIncident(ctx, manager, candidate, options, current)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return errors.Join(err, coordinationErr)
}

func coordinatorIncidentEligible(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
) (bool, error) {
	taskID := optionalString(incident.TaskID)
	if taskID == "" {
		taskID = optionalString(incident.RootTaskID)
	}
	if strings.TrimSpace(taskID) == "" {
		return false, nil
	}
	detail, err := opened.GetTask(ctx, taskID)
	if err != nil {
		// A retained incident may outlive its task. It remains inspectable but
		// cannot be safely proposed against.
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return false, nil
		}
		return false, err
	}
	if detail.Task.Status == model.TaskStatusRunning ||
		detail.Task.Status == model.TaskStatusDone ||
		detail.Task.Status == model.TaskStatusArchived ||
		detail.Task.CurrentRunID != nil {
		return false, nil
	}
	return true, nil
}

func coordinationClaimTTL(options Options) time.Duration {
	ttl := options.PlannerTimeout + coordinationClaimGrace
	if ttl < store.MinCoordinationIncidentClaimTTL {
		return store.MinCoordinationIncidentClaimTTL
	}
	if ttl > store.MaxCoordinationIncidentClaimTTL {
		return store.MaxCoordinationIncidentClaimTTL
	}
	return ttl
}

func activeReusableProposal(
	ctx context.Context,
	opened *store.Store,
	incidentID string,
) (*model.CoordinationProposal, error) {
	proposals, err := opened.ListCoordinationProposals(ctx, store.CoordinationProposalFilter{
		IncidentID: incidentID, Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	for index := range proposals {
		switch proposals[index].Status {
		case model.CoordinationProposalDraft, model.CoordinationProposalValidating,
			model.CoordinationProposalValidated:
			return &proposals[index], nil
		}
	}
	return nil, nil
}

func recoverReusableCoordinationAttempt(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
	proposal model.CoordinationProposal,
	status model.CoordinationAttemptStatus,
	cause error,
	current time.Time,
) error {
	var attemptError *string
	if status == model.CoordinationAttemptFailed {
		message := "recovered Coordinator proposal could not be used"
		if cause != nil {
			message = cause.Error()
		}
		attemptError = &message
	}
	proposalRevision := proposal.ExpectedGraphRevision
	incidentRevision := incident.GraphRevision
	_, _, err := opened.RecoverCoordinationAttemptForProposal(
		ctx,
		store.RecoverCoordinationAttemptInput{
			Board:                         incident.Board,
			ProposalID:                    proposal.ID,
			ExpectedProposalStatus:        proposal.Status,
			ExpectedProposalGraphRevision: &proposalRevision,
			ExpectedIncidentGraphRevision: &incidentRevision,
			ClaimToken:                    incident.ClaimToken,
			Current:                       current,
			Status:                        status,
			Error:                         attemptError,
		},
	)
	return err
}

func coordinationGraphRevision(
	ctx context.Context,
	opened *store.Store,
	board string,
) (int64, error) {
	state, err := opened.GetBoardGraphState(ctx, board)
	if err != nil {
		return 0, err
	}
	return state.Revision, nil
}

func claimCoordinationForReuse(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
	revision int64,
	options Options,
	current time.Time,
) (model.CoordinationIncident, bool, error) {
	return opened.ClaimCoordinationIncident(ctx, incident.ID, store.ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: &revision,
		TTL:                   coordinationClaimTTL(options),
		Current:               current,
	})
}

func reopenClaimedCoordinationIncident(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
	current time.Time,
) error {
	revision := incident.GraphRevision
	_, err := opened.TransitionCoordinationIncident(ctx, incident.ID, store.TransitionCoordinationIncidentInput{
		ExpectedStatus:        model.CoordinationIncidentCoordinating,
		Status:                model.CoordinationIncidentOpen,
		ExpectedGraphRevision: &revision,
		ClaimToken:            incident.ClaimToken,
		Current:               current,
	})
	return err
}

func failCoordinationAttempt(
	ctx context.Context,
	opened *store.Store,
	attempt *model.CoordinationAttempt,
	incident model.CoordinationIncident,
	current time.Time,
	cause error,
) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	message := "coordination attempt failed"
	if cause != nil {
		message = cause.Error()
	}
	var finishErr error
	if attempt != nil {
		_, finishErr = opened.FinishCoordinationAttempt(cleanup, attempt.ID, store.FinishCoordinationAttemptInput{
			Board: incident.Board, Status: model.CoordinationAttemptFailed, Error: &message,
		})
	}
	reopenErr := reopenClaimedCoordinationIncident(cleanup, opened, incident, current)
	return errors.Join(finishErr, reopenErr)
}

func proposalFromRecord(value model.CoordinationProposal) (coordinator.Proposal, error) {
	var actions []coordinator.Action
	if err := json.Unmarshal(value.Actions, &actions); err != nil {
		return coordinator.Proposal{}, fmt.Errorf("decode coordination proposal %s actions: %w", value.ID, err)
	}
	return coordinator.Proposal{
		IncidentID: value.IncidentID, ExpectedGraphRevision: value.ExpectedGraphRevision,
		Summary: value.Summary, Rationale: value.Rationale, Actions: actions,
	}, nil
}

func latestCoordinatorIncidentSnapshot(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	board string,
	options Options,
	incident model.CoordinationIncident,
) (boards.Metadata, coordinator.IncidentSnapshot, int64, error) {
	var latest boards.Metadata
	var revision int64
	var lastErr error
	for range 3 {
		metadata, err := manager.Read(board)
		if err != nil {
			return boards.Metadata{}, coordinator.IncidentSnapshot{}, revision, err
		}
		state, err := opened.GetBoardGraphState(ctx, board)
		if err != nil {
			return metadata, coordinator.IncidentSnapshot{}, revision, err
		}
		latest, revision = metadata, state.Revision
		snapshotIncident := incident
		snapshotIncident.GraphRevision = revision
		snapshot, err := buildCoordinatorIncidentSnapshot(
			ctx, manager, opened, metadata, options, snapshotIncident,
		)
		if err == nil {
			// Policy can change independently of graph state. Read it again
			// after the bounded task/config/health snapshot so handoff uses the
			// newest mode and action limit rather than the paid-call input.
			latest, err = manager.Read(board)
			if err != nil {
				return latest, coordinator.IncidentSnapshot{}, revision, err
			}
			if metadata.Archived != latest.Archived ||
				!reflect.DeepEqual(metadata.Orchestration, latest.Orchestration) {
				lastErr = errors.New(
					"board orchestration policy changed while the Coordinator snapshot was being built",
				)
				continue
			}
			return latest, snapshot, revision, nil
		}
		lastErr = err
		if !errors.Is(err, store.ErrGraphRevisionConflict) {
			return metadata, coordinator.IncidentSnapshot{}, revision, err
		}
	}
	return latest, coordinator.IncidentSnapshot{}, revision, lastErr
}

func obsoleteCoordinatorSnapshot(err error) bool {
	if errors.Is(err, store.ErrGraphRevisionConflict) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "task not found") ||
		strings.Contains(message, "not found: task")
}

func supersedeClaimedCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	proposal model.CoordinationProposal,
	incident model.CoordinationIncident,
	replacementRevision int64,
	current time.Time,
) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_, err := opened.SupersedeCoordinationProposal(cleanup, proposal.ID, store.SupersedeCoordinationProposalInput{
		ExpectedUpdatedAt: proposal.UpdatedAt, ReplacementGraphRevision: &replacementRevision,
		ClaimToken: incident.ClaimToken, Current: current,
	})
	return err
}

func persistValidatedCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	incident model.CoordinationIncident,
	attemptID string,
	proposal coordinator.Proposal,
	selection orchestration.PlannerSelection,
	maxActions int,
	snapshot coordinator.IncidentSnapshot,
	current time.Time,
) (model.CoordinationProposal, coordinator.ValidationResult, error) {
	actions, err := json.Marshal(proposal.Actions)
	if err != nil {
		return model.CoordinationProposal{}, coordinator.ValidationResult{}, err
	}
	agent, modelName, provider := strings.TrimSpace(selection.Candidate.Profile),
		strings.TrimSpace(selection.Candidate.Model), strings.TrimSpace(selection.Candidate.Provider)
	if agent == "" {
		agent = "injected-coordinator"
	}
	revision := incident.GraphRevision
	record, _, err := opened.CreateCoordinationProposal(ctx, store.CreateCoordinationProposalInput{
		IncidentID: incident.ID, AttemptID: &attemptID, CoordinatorAgent: agent,
		CoordinatorModel: modelName, CoordinatorProvider: provider,
		Status: model.CoordinationProposalValidating, ExpectedGraphRevision: &revision,
		ClaimToken: incident.ClaimToken, Current: current,
		Summary: proposal.Summary, Rationale: proposal.Rationale, Actions: actions,
	})
	if err != nil {
		return model.CoordinationProposal{}, coordinator.ValidationResult{}, err
	}
	validation := coordinator.ValidateAgainstSnapshot(proposal, snapshot, maxActions)
	encodedIssues, err := json.Marshal(validation.Issues)
	if err != nil {
		return record, validation, err
	}
	issues := json.RawMessage(encodedIssues)
	target := model.CoordinationProposalValidated
	if !validation.Valid {
		target = model.CoordinationProposalFailed
	}
	record, err = opened.TransitionCoordinationProposal(ctx, record.ID, store.TransitionCoordinationProposalInput{
		ExpectedStatus: model.CoordinationProposalValidating, Status: target,
		ExpectedGraphRevision: &revision, ClaimToken: incident.ClaimToken, Current: current,
		ValidationErrors: &issues,
	})
	return record, validation, err
}

func revalidateCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	record model.CoordinationProposal,
	incident model.CoordinationIncident,
	snapshot coordinator.IncidentSnapshot,
	maxActions int,
	current time.Time,
) (model.CoordinationProposal, coordinator.ValidationResult, error) {
	proposal, err := proposalFromRecord(record)
	if err != nil {
		return record, coordinator.ValidationResult{}, err
	}
	validation := coordinator.ValidateAgainstSnapshot(proposal, snapshot, maxActions)
	encodedIssues, err := json.Marshal(validation.Issues)
	if err != nil {
		return record, validation, err
	}
	issues := json.RawMessage(encodedIssues)
	if record.Status == model.CoordinationProposalDraft {
		record, err = opened.TransitionCoordinationProposal(ctx, record.ID, store.TransitionCoordinationProposalInput{
			ExpectedStatus:        model.CoordinationProposalDraft,
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &incident.GraphRevision,
			ClaimToken:            incident.ClaimToken, Current: current,
		})
		if err != nil {
			return record, validation, err
		}
	}
	if record.Status == model.CoordinationProposalValidating {
		target := model.CoordinationProposalValidated
		if !validation.Valid {
			target = model.CoordinationProposalFailed
		}
		record, err = opened.TransitionCoordinationProposal(ctx, record.ID, store.TransitionCoordinationProposalInput{
			ExpectedStatus: model.CoordinationProposalValidating, Status: target,
			ExpectedGraphRevision: &incident.GraphRevision,
			ClaimToken:            incident.ClaimToken, Current: current, ValidationErrors: &issues,
		})
	}
	return record, validation, err
}

func coordinationActionsAreConditional(validation coordinator.ValidationResult) bool {
	if !validation.Valid || len(validation.Actions) == 0 {
		return false
	}
	for _, action := range validation.Actions {
		if action.Risk != coordinator.ActionRiskConditional {
			return false
		}
	}
	return true
}

func handoffCoordinationProposal(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	candidate coordinationCandidate,
	incident model.CoordinationIncident,
	proposal model.CoordinationProposal,
	options Options,
	current time.Time,
) error {
	latest, snapshot, revision, err := latestCoordinatorIncidentSnapshot(
		ctx, manager, opened, candidate.board, options, incident,
	)
	current = options.currentTime()
	if err != nil {
		if obsoleteCoordinatorSnapshot(err) {
			return supersedeClaimedCoordinationProposal(
				ctx, opened, proposal, incident, revision, current,
			)
		}
		return err
	}
	decoded, err := proposalFromRecord(proposal)
	if err != nil {
		return supersedeClaimedCoordinationProposal(
			ctx, opened, proposal, incident, revision, current,
		)
	}
	validation := coordinator.ValidateAgainstSnapshot(
		decoded, snapshot,
		latest.Orchestration.Autopilot.Coordination.MaxActionsPerIncident,
	)
	if !validation.Valid {
		return supersedeClaimedCoordinationProposal(
			ctx, opened, proposal, incident, revision, current,
		)
	}
	if latest.Archived || !latest.Orchestration.Autopilot.Enabled {
		// A disabled or archived board will not run another reconciliation
		// pass. Do not strand a proposal in AwaitingApproval where nobody can
		// safely advance it; retire it while the live claim can still do so.
		return supersedeClaimedCoordinationProposal(
			ctx, opened, proposal, incident, revision, current,
		)
	}
	auto := candidate.mode == boards.CoordinationModeAuto &&
		latest.Orchestration.Autopilot.Coordination.Mode == boards.CoordinationModeAuto &&
		coordinationActionsAreConditional(validation)
	if auto {
		_, err = opened.ApplyCoordinationProposal(ctx, proposal.ID, store.ApplyCoordinationProposalInput{
			Authorization:         store.CoordinationApplyValidatedAuto,
			ExpectedGraphRevision: &revision,
			ClaimToken:            incident.ClaimToken, Current: current,
		})
	} else {
		_, err = opened.RequestCoordinationApproval(ctx, proposal.ID, store.RequestCoordinationApprovalInput{
			ExpectedGraphRevision: &revision,
			ClaimToken:            incident.ClaimToken, Current: current,
		})
	}
	if err == nil {
		return nil
	}
	// Keep the validated proposal under its lease. A transient database error,
	// task race, or late policy change must not turn into another paid analysis
	// call. After lease expiry the next pass reclaims and revalidates this same
	// proposal before deciding whether it is stale.
	return err
}

func coordinatorPlanner(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	metadata boards.Metadata,
	options Options,
	cwd string,
	selected *orchestration.PlannerSelection,
) (orchestration.Planner, error) {
	if options.CoordinatorPlanner != nil {
		return options.CoordinatorPlanner, nil
	}
	configured, err := configuredProfiles(manager, metadata.Slug, options)
	if err != nil {
		return nil, err
	}
	return createRolePlannerWithSelection(
		manager, opened, metadata, configured, options, agentconfig.RoleCoordinator, cwd,
		func(_ context.Context, selection orchestration.PlannerSelection) error {
			*selected = selection
			return nil
		},
	)
}

func coordinateIncident(
	ctx context.Context,
	manager *boards.Manager,
	candidate coordinationCandidate,
	options Options,
	current time.Time,
) error {
	opened, err := manager.OpenStore(ctx, candidate.board)
	if err != nil {
		return err
	}
	defer opened.Close()
	revision, err := coordinationGraphRevision(ctx, opened, candidate.board)
	if err != nil {
		return err
	}
	reusable, err := activeReusableProposal(ctx, opened, candidate.incident.ID)
	if err != nil {
		return err
	}
	var incident model.CoordinationIncident
	var attempt *model.CoordinationAttempt
	if reusable != nil {
		var claimed bool
		incident, claimed, err = claimCoordinationForReuse(
			ctx, opened, candidate.incident, revision, options, current,
		)
		if err != nil || !claimed {
			return err
		}
		if reusable.ExpectedGraphRevision != incident.GraphRevision {
			recoveryStatus := model.CoordinationAttemptFailed
			var recoveryCause error = &store.GraphRevisionConflictError{
				Board:    incident.Board,
				Expected: reusable.ExpectedGraphRevision,
				Actual:   incident.GraphRevision,
			}
			if reusable.Status == model.CoordinationProposalValidated {
				recoveryStatus = model.CoordinationAttemptSucceeded
				recoveryCause = nil
			}
			if err := recoverReusableCoordinationAttempt(
				ctx,
				opened,
				incident,
				*reusable,
				recoveryStatus,
				recoveryCause,
				current,
			); err != nil {
				return err
			}
			return supersedeClaimedCoordinationProposal(
				ctx, opened, *reusable, incident, incident.GraphRevision, current,
			)
		}
		if reusable.Status == model.CoordinationProposalValidated {
			if err := recoverReusableCoordinationAttempt(
				ctx,
				opened,
				incident,
				*reusable,
				model.CoordinationAttemptSucceeded,
				nil,
				current,
			); err != nil {
				return err
			}
		}
	} else {
		since := current.Add(-time.Hour)
		reserved, reserveErr := opened.ReserveCoordinationAttempt(ctx, store.ReserveCoordinationAttemptInput{
			IncidentID: candidate.incident.ID, Board: candidate.board,
			ExpectedGraphRevision: &revision, Since: since, Current: current,
			MaxCalls: candidate.metadata.Orchestration.Autopilot.Coordination.MaxCallsPerHour,
			TTL:      coordinationClaimTTL(options),
		})
		if reserveErr != nil || !reserved.Reserved {
			return reserveErr
		}
		incident = reserved.Incident
		attempt = &reserved.Attempt
	}

	snapshot, err := buildCoordinatorIncidentSnapshot(
		ctx, manager, opened, candidate.metadata, options, incident,
	)
	if err != nil {
		current = options.currentTime()
		return errors.Join(err, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, err,
		))
	}
	current = options.currentTime()
	maxActions := candidate.metadata.Orchestration.Autopilot.Coordination.MaxActionsPerIncident
	if reusable != nil {
		record, validation, validationErr := revalidateCoordinationProposal(
			ctx, opened, *reusable, incident, snapshot, maxActions, current,
		)
		if validationErr != nil {
			recoveryErr := recoverReusableCoordinationAttempt(
				ctx,
				opened,
				incident,
				record,
				model.CoordinationAttemptFailed,
				validationErr,
				current,
			)
			if recoveryErr != nil {
				return errors.Join(validationErr, recoveryErr)
			}
			return errors.Join(validationErr, supersedeClaimedCoordinationProposal(
				ctx, opened, record, incident, incident.GraphRevision, current,
			))
		}
		if !validation.Valid {
			validationFailure := fmt.Errorf(
				"recovered Coordinator proposal %s failed deterministic validation",
				record.ID,
			)
			if err := recoverReusableCoordinationAttempt(
				ctx,
				opened,
				incident,
				record,
				model.CoordinationAttemptFailed,
				validationFailure,
				current,
			); err != nil {
				return err
			}
			if record.Status != model.CoordinationProposalFailed {
				return supersedeClaimedCoordinationProposal(
					ctx, opened, record, incident, incident.GraphRevision, current,
				)
			}
			return reopenClaimedCoordinationIncident(ctx, opened, incident, current)
		}
		if err := recoverReusableCoordinationAttempt(
			ctx,
			opened,
			incident,
			record,
			model.CoordinationAttemptSucceeded,
			nil,
			current,
		); err != nil {
			return err
		}
		return handoffCoordinationProposal(
			ctx, manager, opened, candidate, incident, record, options, current,
		)
	}

	cwd, err := os.MkdirTemp("", "autogora-coordinator-")
	if err != nil {
		current = options.currentTime()
		return errors.Join(err, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, err,
		))
	}
	defer os.RemoveAll(cwd)
	var selection orchestration.PlannerSelection
	planner, err := coordinatorPlanner(
		ctx, manager, opened, candidate.metadata, options, cwd, &selection,
	)
	if err != nil {
		current = options.currentTime()
		return errors.Join(err, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, err,
		))
	}
	analysisTimeout := options.PlannerTimeout
	if analysisTimeout <= 0 {
		analysisTimeout = 120 * time.Second
	}
	// PlannerTimeout is an end-to-end budget for the fallback chain. Individual
	// candidates also observe this context, so a slow primary cannot outlive the
	// incident lease and let another Supervisor duplicate the same paid analysis.
	analysisCtx, cancelAnalysis := context.WithTimeout(ctx, analysisTimeout)
	proposal, err := (coordinator.Analyzer{
		Planner: planner, MaxActions: maxActions,
	}).Analyze(analysisCtx, snapshot)
	cancelAnalysis()
	if err != nil {
		current = options.currentTime()
		return errors.Join(err, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, err,
		))
	}
	current = options.currentTime()
	record, validation, err := persistValidatedCoordinationProposal(
		ctx, opened, incident, attempt.ID, proposal, selection, maxActions, snapshot, current,
	)
	if err != nil {
		return errors.Join(err, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, err,
		))
	}
	if !validation.Valid {
		validationErr := fmt.Errorf("Coordinator proposal %s failed deterministic validation", record.ID)
		return errors.Join(validationErr, failCoordinationAttempt(
			ctx, opened, attempt, incident, current, validationErr,
		))
	}
	_, err = opened.FinishCoordinationAttempt(ctx, attempt.ID, store.FinishCoordinationAttemptInput{
		Board: incident.Board, Status: model.CoordinationAttemptSucceeded,
		SelectedAgent: record.CoordinatorAgent, SelectedRuntime: selection.Candidate.Runtime,
		SelectedModel: record.CoordinatorModel, SelectedProvider: record.CoordinatorProvider,
		SelectedSource: selection.Candidate.Source,
	})
	if err != nil {
		return err
	}
	current = options.currentTime()
	return handoffCoordinationProposal(
		ctx, manager, opened, candidate, incident, record, options, current,
	)
}

// reconcilePendingCoordination revalidates human-pending work without a paid
// Coordinator call. It runs before observation so a superseded incident can be
// resolved in the same pass when its triggering condition has disappeared.
func reconcilePendingCoordination(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	metadata boards.Metadata,
	options Options,
	current time.Time,
) error {
	state, err := opened.GetBoardGraphState(ctx, metadata.Slug)
	if err != nil {
		return err
	}
	incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		Board: metadata.Slug, Limit: 500,
	})
	if err != nil {
		return err
	}
	for _, incident := range incidents {
		switch incident.Status {
		case model.CoordinationIncidentOpen:
			if incident.GraphRevision == state.Revision {
				continue
			}
			revision := state.Revision
			if _, _, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
				ID: incident.ID, Board: incident.Board,
				RootTaskID: incident.RootTaskID, TaskID: incident.TaskID,
				Trigger: incident.Trigger, Severity: incident.Severity,
				ExpectedGraphRevision: &revision,
				Summary:               incident.Summary, Details: incident.Details,
			}); err != nil {
				return err
			}
		case model.CoordinationIncidentAwaitingApproval:
			proposals, err := opened.ListCoordinationProposals(
				ctx, store.CoordinationProposalFilter{IncidentID: incident.ID, Limit: 100},
			)
			if err != nil {
				return err
			}
			var pending *model.CoordinationProposal
			for index := range proposals {
				if proposals[index].Status == model.CoordinationProposalAwaitingApproval ||
					proposals[index].Status == model.CoordinationProposalApproved {
					pending = &proposals[index]
					break
				}
			}
			if pending == nil {
				_, err := opened.TransitionCoordinationIncident(
					ctx, incident.ID, store.TransitionCoordinationIncidentInput{
						ExpectedStatus: model.CoordinationIncidentAwaitingApproval,
						Status:         model.CoordinationIncidentFailed,
						Current:        current,
					},
				)
				if err != nil {
					return err
				}
				continue
			}
			latest, snapshot, revision, snapshotErr := latestCoordinatorIncidentSnapshot(
				ctx, manager, opened, metadata.Slug, options, incident,
			)
			stale := pending.ExpectedGraphRevision != revision ||
				incident.GraphRevision != revision
			if snapshotErr != nil {
				if !obsoleteCoordinatorSnapshot(snapshotErr) {
					return snapshotErr
				}
				stale = true
			} else if !stale {
				proposal, decodeErr := proposalFromRecord(*pending)
				if decodeErr != nil {
					stale = true
				} else {
					validation := coordinator.ValidateAgainstSnapshot(
						proposal, snapshot,
						latest.Orchestration.Autopilot.Coordination.MaxActionsPerIncident,
					)
					stale = !validation.Valid
				}
			}
			if !stale {
				continue
			}
			if _, err := opened.SupersedeCoordinationProposal(
				ctx, pending.ID, store.SupersedeCoordinationProposalInput{
					ExpectedUpdatedAt:        pending.UpdatedAt,
					ReplacementGraphRevision: &revision,
					Current:                  current,
				},
			); err != nil {
				return err
			}
		}
	}
	return nil
}
