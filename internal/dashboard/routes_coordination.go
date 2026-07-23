package dashboard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func coordinationLimit(request *http.Request) (int, error) {
	raw := strings.TrimSpace(request.URL.Query().Get("limit"))
	if raw == "" {
		return 100, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		return 0, &coordinationQueryError{message: "coordination limit must be between 1 and 500"}
	}
	return limit, nil
}

type coordinationQueryError struct{ message string }

func (e *coordinationQueryError) Error() string { return e.message }

type coordinationMutationVersion struct {
	ExpectedUpdatedAt     string
	ExpectedGraphRevision int64
}

type coordinationMutationHTTPResult struct {
	Status int
	Body   any
}

func readCoordinationMutationVersion(
	request *http.Request,
	requireUpdatedAt bool,
) (coordinationMutationVersion, error) {
	body, err := readJSON(request)
	if err != nil {
		return coordinationMutationVersion{}, err
	}
	for key := range body {
		if key != "expectedGraphRevision" &&
			(key != "expectedUpdatedAt" || !requireUpdatedAt) {
			return coordinationMutationVersion{}, fmt.Errorf("invalid coordination mutation field: %s", key)
		}
	}
	updatedAt := strings.TrimSpace(stringValue(body["expectedUpdatedAt"]))
	if requireUpdatedAt && updatedAt == "" {
		return coordinationMutationVersion{}, errors.New(
			"coordination proposal mutation requires expectedUpdatedAt",
		)
	}
	rawRevision, exists := body["expectedGraphRevision"]
	if !exists {
		return coordinationMutationVersion{}, errors.New(
			"coordination mutation requires expectedGraphRevision",
		)
	}
	revision, ok := rawRevision.(float64)
	const maxExactJSONInteger = float64(1<<53 - 1)
	if !ok || revision < 0 || math.Trunc(revision) != revision || revision > maxExactJSONInteger {
		return coordinationMutationVersion{}, errors.New(
			"expectedGraphRevision must be a non-negative integer",
		)
	}
	return coordinationMutationVersion{
		ExpectedUpdatedAt: updatedAt, ExpectedGraphRevision: int64(revision),
	}, nil
}

func coordinationProposalOnBoard(
	ctx context.Context,
	opened *store.Store,
	proposalID, board string,
) (model.CoordinationProposal, model.CoordinationIncident, error) {
	proposal, err := opened.GetCoordinationProposal(ctx, strings.TrimSpace(proposalID))
	if err != nil {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, err
	}
	incident, err := opened.GetCoordinationIncident(ctx, proposal.IncidentID)
	if err != nil {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, err
	}
	if incident.Board != board {
		return model.CoordinationProposal{}, model.CoordinationIncident{},
			&coordinationQueryError{message: "coordination proposal not found"}
	}
	return proposal, incident, nil
}

func coordinationIncidentOnBoard(
	ctx context.Context,
	opened *store.Store,
	incidentID, board string,
) (model.CoordinationIncident, error) {
	incident, err := opened.GetCoordinationIncident(ctx, strings.TrimSpace(incidentID))
	if err != nil {
		return model.CoordinationIncident{}, err
	}
	if incident.Board != board {
		return model.CoordinationIncident{},
			&coordinationQueryError{message: "coordination incident not found"}
	}
	return incident, nil
}

func sendCoordinationMutation(
	response http.ResponseWriter,
	result coordinationMutationHTTPResult,
	err error,
) error {
	if err == nil {
		sendJSON(response, result.Status, result.Body)
	}
	return err
}

func (s *Server) approveCoordinationProposal(
	response http.ResponseWriter,
	request *http.Request,
	proposalID, board string,
) error {
	version, err := readCoordinationMutationVersion(request, true)
	if err != nil {
		return err
	}
	ctx := request.Context()
	result, err := usingStore(ctx, s, board, func(opened *store.Store) (coordinationMutationHTTPResult, error) {
		if _, _, err := coordinationProposalOnBoard(ctx, opened, proposalID, board); err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		approved, err := opened.ApproveCoordinationProposal(ctx, proposalID, store.ApproveCoordinationProposalInput{
			ExpectedUpdatedAt:     version.ExpectedUpdatedAt,
			ExpectedGraphRevision: &version.ExpectedGraphRevision,
		})
		if err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		applied, applyErr := opened.ApplyCoordinationProposal(ctx, proposalID, store.ApplyCoordinationProposalInput{
			Authorization:         store.CoordinationApplyApproved,
			ExpectedGraphRevision: &version.ExpectedGraphRevision,
		})
		if applyErr == nil {
			return coordinationMutationHTTPResult{Status: http.StatusOK, Body: applied}, nil
		}

		// Apply is transactional, so the proposal is still approved here. Move
		// it out of the approval lane before returning the conflict.
		graphState, graphErr := opened.GetBoardGraphState(ctx, board)
		if graphErr != nil {
			return coordinationMutationHTTPResult{}, fmt.Errorf(
				"coordination proposal apply conflict: %v; graph refresh failed: %w",
				applyErr, graphErr,
			)
		}
		superseded, supersedeErr := opened.SupersedeCoordinationProposal(
			ctx, proposalID, store.SupersedeCoordinationProposalInput{
				ExpectedUpdatedAt:        approved.Proposal.UpdatedAt,
				ReplacementGraphRevision: &graphState.Revision,
			},
		)
		if supersedeErr != nil {
			return coordinationMutationHTTPResult{}, fmt.Errorf(
				"coordination proposal apply conflict: %v; supersede recovery failed: %w",
				applyErr, supersedeErr,
			)
		}
		body := map[string]any{
			"error": "Coordination proposal could not be applied and was superseded; refresh before retrying",
			"code":  "coordination_apply_conflict", "cause": applyErr.Error(),
			"retryable": true, "proposal": superseded.Proposal,
			"incident": superseded.Incident, "graphState": graphState,
		}
		return coordinationMutationHTTPResult{Status: http.StatusConflict, Body: body}, nil
	})
	return sendCoordinationMutation(response, result, err)
}

func (s *Server) rejectCoordinationProposal(
	response http.ResponseWriter,
	request *http.Request,
	proposalID, board string,
) error {
	version, err := readCoordinationMutationVersion(request, true)
	if err != nil {
		return err
	}
	ctx := request.Context()
	result, err := usingStore(ctx, s, board, func(opened *store.Store) (coordinationMutationHTTPResult, error) {
		if _, _, err := coordinationProposalOnBoard(ctx, opened, proposalID, board); err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		rejected, err := opened.RejectCoordinationProposal(ctx, proposalID, store.RejectCoordinationProposalInput{
			ExpectedUpdatedAt:     version.ExpectedUpdatedAt,
			ExpectedGraphRevision: &version.ExpectedGraphRevision,
		})
		return coordinationMutationHTTPResult{Status: http.StatusOK, Body: rejected}, err
	})
	return sendCoordinationMutation(response, result, err)
}

func (s *Server) retryCoordinationProposal(
	response http.ResponseWriter,
	request *http.Request,
	proposalID, board string,
) error {
	version, err := readCoordinationMutationVersion(request, true)
	if err != nil {
		return err
	}
	ctx := request.Context()
	result, err := usingStore(ctx, s, board, func(opened *store.Store) (coordinationMutationHTTPResult, error) {
		proposal, incident, err := coordinationProposalOnBoard(ctx, opened, proposalID, board)
		if err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		if proposal.ExpectedGraphRevision != version.ExpectedGraphRevision {
			return coordinationMutationHTTPResult{}, &store.GraphRevisionConflictError{
				Board: incident.Board, Expected: version.ExpectedGraphRevision,
				Actual: proposal.ExpectedGraphRevision,
			}
		}
		if incident.Status != model.CoordinationIncidentAwaitingApproval ||
			(proposal.Status != model.CoordinationProposalAwaitingApproval &&
				proposal.Status != model.CoordinationProposalApproved) {
			return coordinationMutationHTTPResult{}, fmt.Errorf(
				"coordination retry conflict: proposal %s is %s and incident %s is %s; retry is only available while awaiting approval",
				proposal.ID, proposal.Status, incident.ID, incident.Status,
			)
		}
		state, err := opened.GetBoardGraphState(ctx, board)
		if err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		superseded, err := opened.SupersedeCoordinationProposal(
			ctx, proposalID, store.SupersedeCoordinationProposalInput{
				ExpectedUpdatedAt:        version.ExpectedUpdatedAt,
				ReplacementGraphRevision: &state.Revision,
			},
		)
		if err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		return coordinationMutationHTTPResult{Status: http.StatusOK, Body: map[string]any{
			"proposal": superseded.Proposal, "incident": superseded.Incident,
			"graphState": state, "retryScheduled": true,
		}}, nil
	})
	return sendCoordinationMutation(response, result, err)
}

func (s *Server) dismissCoordinationIncident(
	response http.ResponseWriter,
	request *http.Request,
	incidentID, board string,
) error {
	version, err := readCoordinationMutationVersion(request, false)
	if err != nil {
		return err
	}
	ctx := request.Context()
	result, err := usingStore(ctx, s, board, func(opened *store.Store) (coordinationMutationHTTPResult, error) {
		if _, err := coordinationIncidentOnBoard(ctx, opened, incidentID, board); err != nil {
			return coordinationMutationHTTPResult{}, err
		}
		incident, err := opened.TransitionCoordinationIncident(
			ctx, incidentID, store.TransitionCoordinationIncidentInput{
				ExpectedStatus:        model.CoordinationIncidentOpen,
				Status:                model.CoordinationIncidentDismissed,
				ExpectedGraphRevision: &version.ExpectedGraphRevision,
			},
		)
		return coordinationMutationHTTPResult{Status: http.StatusOK, Body: map[string]any{
			"incident": incident,
		}}, err
	})
	return sendCoordinationMutation(response, result, err)
}

func coordinationIncidentFilter(request *http.Request, board string) (store.CoordinationIncidentFilter, error) {
	limit, err := coordinationLimit(request)
	if err != nil {
		return store.CoordinationIncidentFilter{}, err
	}
	filter := store.CoordinationIncidentFilter{
		Board: board, RootTaskID: strings.TrimSpace(request.URL.Query().Get("rootTaskId")),
		TaskID: strings.TrimSpace(request.URL.Query().Get("taskId")), Limit: limit,
	}
	if raw := strings.TrimSpace(request.URL.Query().Get("trigger")); raw != "" {
		filter.Trigger = model.CoordinationTrigger(raw)
		if !model.ValidCoordinationTrigger(filter.Trigger) {
			return store.CoordinationIncidentFilter{}, &coordinationQueryError{message: "invalid coordination trigger"}
		}
	}
	if raw := strings.TrimSpace(request.URL.Query().Get("status")); raw != "" {
		filter.Status = model.CoordinationIncidentStatus(raw)
		if !model.ValidCoordinationIncidentStatus(filter.Status) {
			return store.CoordinationIncidentFilter{}, &coordinationQueryError{message: "invalid coordination incident status"}
		}
	}
	return filter, nil
}

func (s *Server) handleCoordination(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	if len(segments) == 5 && segments[2] == "proposals" {
		if request.Method != http.MethodPost {
			sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
				"error": "Coordination proposal actions require POST",
			})
			return nil
		}
		switch segments[4] {
		case "approve":
			return s.approveCoordinationProposal(response, request, segments[3], board)
		case "reject":
			return s.rejectCoordinationProposal(response, request, segments[3], board)
		case "retry":
			return s.retryCoordinationProposal(response, request, segments[3], board)
		}
	}
	if len(segments) == 5 && segments[2] == "incidents" && segments[4] == "dismiss" {
		if request.Method != http.MethodPost {
			sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
				"error": "Coordination incident dismissal requires POST",
			})
			return nil
		}
		return s.dismissCoordinationIncident(response, request, segments[3], board)
	}
	if request.Method != http.MethodGet {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "Coordination endpoint requires GET"})
		return nil
	}
	ctx := request.Context()
	if len(segments) == 2 {
		metadata, err := s.manager.Read(board)
		if err != nil {
			return err
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			state, err := opened.GetBoardGraphState(ctx, board)
			if err != nil {
				return nil, err
			}
			incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{Board: board, Limit: 100})
			if err != nil {
				return nil, err
			}
			active, awaitingApproval := 0, 0
			for _, incident := range incidents {
				switch incident.Status {
				case model.CoordinationIncidentOpen, model.CoordinationIncidentCoordinating,
					model.CoordinationIncidentAwaitingApproval, model.CoordinationIncidentApplying:
					active++
				}
				if incident.Status == model.CoordinationIncidentAwaitingApproval {
					awaitingApproval++
				}
			}
			return map[string]any{
				"policy": metadata.Orchestration.Autopilot.Coordination, "graphState": state,
				"activeCount": active, "awaitingApprovalCount": awaitingApproval, "incidents": incidents,
			}, nil
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 3 && segments[2] == "incidents" {
		filter, err := coordinationIncidentFilter(request, board)
		if err != nil {
			return err
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return opened.ListCoordinationIncidents(ctx, filter)
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 4 && segments[2] == "incidents" {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			incident, err := opened.GetCoordinationIncident(ctx, segments[3])
			if err != nil {
				return nil, err
			}
			if incident.Board != board {
				return nil, &coordinationQueryError{message: "coordination incident not found"}
			}
			proposals, err := opened.ListCoordinationProposals(ctx, store.CoordinationProposalFilter{
				IncidentID: incident.ID, Limit: 100,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"incident": incident, "proposals": proposals}, nil
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 4 && segments[2] == "proposals" {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			proposal, err := opened.GetCoordinationProposal(ctx, segments[3])
			if err != nil {
				return nil, err
			}
			incident, err := opened.GetCoordinationIncident(ctx, proposal.IncidentID)
			if err != nil {
				return nil, err
			}
			if incident.Board != board {
				return nil, &coordinationQueryError{message: "coordination proposal not found"}
			}
			return map[string]any{"proposal": proposal, "incident": incident}, nil
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}
