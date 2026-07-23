package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const coordinationHelp = `autogora coordination <action> [options]

Actions:
  status                Show board policy, graph revision, and recent incidents
  list                  List incidents with optional filters
  show <incident-id>    Show an incident and its proposals
  proposal <id>         Show one proposal and its incident
  approve <proposal-id> Approve and apply an awaiting proposal
  reject <proposal-id>  Reject a proposal awaiting approval
  dismiss <incident-id> Dismiss an open incident
  retry <proposal-id>   Supersede a pending proposal and reopen its incident

Options:
  --board <slug>        Select a board
  --db <path>           Override the database path
  --status <status>     Filter list by incident status
  --trigger <trigger>   Filter list by trigger
  --task <task-id>      Filter list by focus task
  --root <task-id>      Filter list by root task
  --limit <number>      Return 1-500 incidents (default 100)
  --updated-at <time>   Required proposal version for approve, reject, or retry
  --graph-revision <n>  Required graph version for approve, reject, or dismiss

Coordinator claim tokens are internal and are never accepted by these commands.
`

type coordinationMutationOutput struct {
	Action         string                      `json:"action"`
	Outcome        string                      `json:"outcome"`
	Proposal       *model.CoordinationProposal `json:"proposal,omitempty"`
	Incident       model.CoordinationIncident  `json:"incident"`
	GraphRevision  int64                       `json:"graphRevision"`
	CreatedTaskIDs map[string]string           `json:"createdTaskIds,omitempty"`
}

func coordinationMutationID(opts options, action, kind string) (string, error) {
	if len(opts.positionals) != 2 {
		return "", fmt.Errorf("coordination %s requires exactly one %s id", action, kind)
	}
	id := strings.TrimSpace(opts.positionals[1])
	if id == "" {
		return "", fmt.Errorf("coordination %s requires exactly one %s id", action, kind)
	}
	return id, nil
}

func coordinationUpdatedAtOption(opts options, action string) (string, error) {
	value := strings.TrimSpace(opts.value("updated-at"))
	if value == "" {
		return "", fmt.Errorf("coordination %s requires --updated-at", action)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return "", fmt.Errorf("coordination %s --updated-at must be RFC3339: %w", action, err)
	}
	return value, nil
}

func coordinationGraphRevisionOption(
	opts options,
	action string,
	required bool,
) (*int64, error) {
	value := strings.TrimSpace(opts.value("graph-revision"))
	if value == "" {
		if required {
			return nil, fmt.Errorf("coordination %s requires --graph-revision", action)
		}
		return nil, nil
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 {
		return nil, fmt.Errorf(
			"coordination %s --graph-revision must be a non-negative integer",
			action,
		)
	}
	return &revision, nil
}

func coordinationMutationError(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrCoordinationStateConflict) ||
		errors.Is(err, store.ErrGraphRevisionConflict) {
		return fmt.Errorf(
			"coordination %s conflict; refresh the proposal or incident and retry with its current versions: %w",
			action,
			err,
		)
	}
	return fmt.Errorf("coordination %s failed: %w", action, err)
}

func coordinationProposalForBoard(
	ctx context.Context,
	opened *store.Store,
	board, proposalID string,
) (model.CoordinationProposal, model.CoordinationIncident, error) {
	proposal, err := opened.GetCoordinationProposal(ctx, proposalID)
	if err != nil {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, err
	}
	incident, err := opened.GetCoordinationIncident(ctx, proposal.IncidentID)
	if err != nil {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, err
	}
	if incident.Board != board {
		return model.CoordinationProposal{}, model.CoordinationIncident{},
			errors.New("coordination proposal not found")
	}
	return proposal, incident, nil
}

func (a *App) approveCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	board string,
	opts options,
) error {
	proposalID, err := coordinationMutationID(opts, "approve", "proposal")
	if err != nil {
		return err
	}
	updatedAt, err := coordinationUpdatedAtOption(opts, "approve")
	if err != nil {
		return err
	}
	revision, err := coordinationGraphRevisionOption(opts, "approve", true)
	if err != nil {
		return err
	}
	if _, _, err := coordinationProposalForBoard(ctx, opened, board, proposalID); err != nil {
		return coordinationMutationError("approve", err)
	}
	approved, err := opened.ApproveCoordinationProposal(
		ctx,
		proposalID,
		store.ApproveCoordinationProposalInput{
			ExpectedUpdatedAt: updatedAt, ExpectedGraphRevision: revision,
		},
	)
	if err != nil {
		return coordinationMutationError("approve", err)
	}
	applied, err := opened.ApplyCoordinationProposal(
		ctx,
		proposalID,
		store.ApplyCoordinationProposalInput{
			Authorization:         store.CoordinationApplyApproved,
			ExpectedGraphRevision: revision,
			Current:               time.Now().UTC(),
		},
	)
	if err == nil {
		proposal := applied.Proposal
		return writeJSON(a.Stdout, coordinationMutationOutput{
			Action: "approve", Outcome: "applied", Proposal: &proposal,
			Incident: applied.Incident, GraphRevision: applied.GraphRevision,
			CreatedTaskIDs: applied.CreatedTaskIDs,
		})
	}
	if !errors.Is(err, store.ErrGraphRevisionConflict) {
		return coordinationMutationError("approve apply", err)
	}

	// Approval is already durable. If the graph changed before application,
	// retire the stale proposal and reopen the incident at the current graph
	// instead of leaving an Approved proposal that can no longer apply.
	state, stateErr := opened.GetBoardGraphState(ctx, board)
	if stateErr != nil {
		return coordinationMutationError("approve stale recovery", errors.Join(err, stateErr))
	}
	superseded, supersedeErr := opened.SupersedeCoordinationProposal(
		ctx,
		proposalID,
		store.SupersedeCoordinationProposalInput{
			ExpectedUpdatedAt:        approved.Proposal.UpdatedAt,
			ReplacementGraphRevision: &state.Revision,
			Current:                  time.Now().UTC(),
		},
	)
	if supersedeErr != nil {
		return coordinationMutationError(
			"approve stale recovery",
			errors.Join(err, supersedeErr),
		)
	}
	proposal := superseded.Proposal
	return writeJSON(a.Stdout, coordinationMutationOutput{
		Action: "approve", Outcome: "superseded", Proposal: &proposal,
		Incident: superseded.Incident, GraphRevision: superseded.Incident.GraphRevision,
	})
}

func (a *App) rejectCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	board string,
	opts options,
) error {
	proposalID, err := coordinationMutationID(opts, "reject", "proposal")
	if err != nil {
		return err
	}
	updatedAt, err := coordinationUpdatedAtOption(opts, "reject")
	if err != nil {
		return err
	}
	revision, err := coordinationGraphRevisionOption(opts, "reject", true)
	if err != nil {
		return err
	}
	if _, _, err := coordinationProposalForBoard(ctx, opened, board, proposalID); err != nil {
		return coordinationMutationError("reject", err)
	}
	rejected, err := opened.RejectCoordinationProposal(
		ctx,
		proposalID,
		store.RejectCoordinationProposalInput{
			ExpectedUpdatedAt: updatedAt, ExpectedGraphRevision: revision,
		},
	)
	if err != nil {
		return coordinationMutationError("reject", err)
	}
	proposal := rejected.Proposal
	return writeJSON(a.Stdout, coordinationMutationOutput{
		Action: "reject", Outcome: "rejected", Proposal: &proposal,
		Incident: rejected.Incident, GraphRevision: rejected.Incident.GraphRevision,
	})
}

func (a *App) dismissCoordinationIncident(
	ctx context.Context,
	opened *store.Store,
	board string,
	opts options,
) error {
	incidentID, err := coordinationMutationID(opts, "dismiss", "incident")
	if err != nil {
		return err
	}
	revision, err := coordinationGraphRevisionOption(opts, "dismiss", true)
	if err != nil {
		return err
	}
	incident, err := opened.GetCoordinationIncident(ctx, incidentID)
	if err != nil || incident.Board != board {
		if err == nil {
			err = errors.New("coordination incident not found")
		}
		return coordinationMutationError("dismiss", err)
	}
	dismissed, err := opened.TransitionCoordinationIncident(
		ctx,
		incident.ID,
		store.TransitionCoordinationIncidentInput{
			ExpectedStatus:        model.CoordinationIncidentOpen,
			Status:                model.CoordinationIncidentDismissed,
			ExpectedGraphRevision: revision,
		},
	)
	if err != nil {
		return coordinationMutationError("dismiss", err)
	}
	return writeJSON(a.Stdout, coordinationMutationOutput{
		Action: "dismiss", Outcome: "dismissed",
		Incident: dismissed, GraphRevision: dismissed.GraphRevision,
	})
}

func (a *App) retryCoordinationProposal(
	ctx context.Context,
	opened *store.Store,
	board string,
	opts options,
) error {
	proposalID, err := coordinationMutationID(opts, "retry", "proposal")
	if err != nil {
		return err
	}
	updatedAt, err := coordinationUpdatedAtOption(opts, "retry")
	if err != nil {
		return err
	}
	requestedRevision, err := coordinationGraphRevisionOption(opts, "retry", false)
	if err != nil {
		return err
	}
	proposal, incident, err := coordinationProposalForBoard(ctx, opened, board, proposalID)
	if err != nil {
		return coordinationMutationError("retry", err)
	}
	if incident.Status != model.CoordinationIncidentAwaitingApproval ||
		(proposal.Status != model.CoordinationProposalAwaitingApproval &&
			proposal.Status != model.CoordinationProposalApproved) {
		return fmt.Errorf(
			"coordination retry requires an awaiting-approval incident and an awaiting or approved proposal; current states are incident=%s proposal=%s",
			incident.Status,
			proposal.Status,
		)
	}
	state, err := opened.GetBoardGraphState(ctx, board)
	if err != nil {
		return coordinationMutationError("retry", err)
	}
	if requestedRevision != nil && *requestedRevision != state.Revision {
		return coordinationMutationError("retry", &store.GraphRevisionConflictError{
			Board: board, Expected: *requestedRevision, Actual: state.Revision,
		})
	}
	superseded, err := opened.SupersedeCoordinationProposal(
		ctx,
		proposalID,
		store.SupersedeCoordinationProposalInput{
			ExpectedUpdatedAt:        updatedAt,
			ReplacementGraphRevision: &state.Revision,
			Current:                  time.Now().UTC(),
		},
	)
	if err != nil {
		return coordinationMutationError("retry", err)
	}
	supersededProposal := superseded.Proposal
	return writeJSON(a.Stdout, coordinationMutationOutput{
		Action: "retry", Outcome: "reopened", Proposal: &supersededProposal,
		Incident: superseded.Incident, GraphRevision: superseded.Incident.GraphRevision,
	})
}

func coordinationListFilter(board string, opts options) (store.CoordinationIncidentFilter, error) {
	limit, err := numberOption(opts.value("limit"), 100)
	if err != nil {
		return store.CoordinationIncidentFilter{}, err
	}
	if limit < 1 || limit > 500 {
		return store.CoordinationIncidentFilter{}, errors.New("coordination limit must be between 1 and 500")
	}
	filter := store.CoordinationIncidentFilter{
		Board: board, RootTaskID: strings.TrimSpace(opts.value("root")),
		TaskID: strings.TrimSpace(opts.value("task")), Limit: limit,
	}
	if raw := strings.TrimSpace(opts.value("trigger")); raw != "" {
		filter.Trigger = model.CoordinationTrigger(raw)
		if !model.ValidCoordinationTrigger(filter.Trigger) {
			return store.CoordinationIncidentFilter{}, errors.New("invalid coordination trigger")
		}
	}
	if raw := strings.TrimSpace(opts.value("status")); raw != "" {
		filter.Status = model.CoordinationIncidentStatus(raw)
		if !model.ValidCoordinationIncidentStatus(filter.Status) {
			return store.CoordinationIncidentFilter{}, errors.New("invalid coordination incident status")
		}
	}
	return filter, nil
}

func (a *App) runCoordination(ctx context.Context, opts options) error {
	if opts.present("claim-token") {
		return errors.New("coordination commands do not accept claim tokens")
	}
	action := "status"
	if len(opts.positionals) > 0 {
		action = opts.positionals[0]
	}
	opened, manager, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()

	switch action {
	case "status":
		if len(opts.positionals) > 1 {
			return errors.New("coordination status does not accept arguments")
		}
		metadata, err := manager.Read(board)
		if err != nil {
			return err
		}
		state, err := opened.GetBoardGraphState(ctx, board)
		if err != nil {
			return err
		}
		incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{Board: board, Limit: 100})
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, map[string]any{
			"policy":     metadata.Orchestration.Autopilot.Coordination,
			"graphState": state, "incidents": incidents,
		})
	case "list", "ls":
		if len(opts.positionals) > 1 {
			return errors.New("coordination list does not accept arguments")
		}
		filter, err := coordinationListFilter(board, opts)
		if err != nil {
			return err
		}
		incidents, err := opened.ListCoordinationIncidents(ctx, filter)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, incidents)
	case "show":
		if len(opts.positionals) != 2 {
			return errors.New("coordination show requires exactly one incident id")
		}
		incident, err := opened.GetCoordinationIncident(ctx, opts.positionals[1])
		if err != nil {
			return err
		}
		if incident.Board != board {
			return errors.New("coordination incident not found")
		}
		proposals, err := opened.ListCoordinationProposals(ctx, store.CoordinationProposalFilter{
			IncidentID: incident.ID, Limit: 100,
		})
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, map[string]any{"incident": incident, "proposals": proposals})
	case "proposal":
		if len(opts.positionals) != 2 {
			return errors.New("coordination proposal requires exactly one proposal id")
		}
		proposal, err := opened.GetCoordinationProposal(ctx, opts.positionals[1])
		if err != nil {
			return err
		}
		incident, err := opened.GetCoordinationIncident(ctx, proposal.IncidentID)
		if err != nil {
			return err
		}
		if incident.Board != board {
			return errors.New("coordination proposal not found")
		}
		return writeJSON(a.Stdout, map[string]any{"proposal": proposal, "incident": incident})
	case "approve":
		return a.approveCoordinationProposal(ctx, opened, board, opts)
	case "reject":
		return a.rejectCoordinationProposal(ctx, opened, board, opts)
	case "dismiss":
		return a.dismissCoordinationIncident(ctx, opened, board, opts)
	case "retry":
		return a.retryCoordinationProposal(ctx, opened, board, opts)
	default:
		return errors.New(
			"coordination requires status, list, show, proposal, approve, reject, dismiss, or retry",
		)
	}
}
