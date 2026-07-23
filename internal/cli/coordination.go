package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const coordinationHelp = `autogora coordination <action> [options]

Actions:
  status                Show board policy, graph revision, and recent incidents
  list                  List incidents with optional filters
  show <incident-id>    Show an incident and its proposals
  proposal <id>         Show one proposal and its incident

Options:
  --board <slug>        Select a board
  --db <path>           Override the database path
  --status <status>     Filter list by incident status
  --trigger <trigger>   Filter list by trigger
  --task <task-id>      Filter list by focus task
  --root <task-id>      Filter list by root task
  --limit <number>      Return 1-500 incidents (default 100)
`

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
	default:
		return errors.New("coordination requires status, list, show, or proposal")
	}
}
