package dashboard

import (
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
