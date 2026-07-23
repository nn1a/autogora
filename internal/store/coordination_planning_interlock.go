package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

// liveAutoDecomposeClaimForTask is called only from a write transaction that
// is about to claim graph-stall coordination. Joining the current task version
// makes an edited task immediately stop honoring its obsolete Planner lease.
func liveAutoDecomposeClaimForTask(
	ctx context.Context,
	q querier,
	board string,
	taskID string,
	taskUpdatedAt string,
	current time.Time,
) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, nil
	}
	state, err := scanAutoDecomposeState(q.QueryRowContext(ctx, `
		SELECT
			state.task_id, state.task_updated_at, state.attempts, state.max_attempts,
			state.next_attempt_at, state.claim_token, state.claim_expires_at,
			state.last_error, state.updated_at
		FROM auto_decompose_state state
		JOIN tasks task ON task.id = state.task_id
		WHERE state.task_id = ?
		  AND task.board = ?
		  AND task.status = 'triage'
		  AND task.updated_at = state.task_updated_at
		  AND state.claim_token IS NOT NULL
	`, taskID, board))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Planner claim for task %s: %w", taskID, err)
	}
	if taskUpdatedAt != "" && state.TaskUpdatedAt != taskUpdatedAt {
		return false, nil
	}
	if state.ClaimExpiresAt == nil {
		return false, fmt.Errorf("Planner claim for task %s has no expiry", taskID)
	}
	expiresAt, err := parseAutoDecomposeTimestamp("claim expiry", *state.ClaimExpiresAt)
	if err != nil {
		return false, err
	}
	return expiresAt.After(current), nil
}

func graphStalledIncidentTaskVersion(
	incident model.CoordinationIncident,
) (string, error) {
	var details struct {
		TaskUpdatedAt string `json:"taskUpdatedAt"`
	}
	if err := json.Unmarshal(incident.Details, &details); err != nil {
		return "", fmt.Errorf(
			"decode graph-stall coordination incident %s details: %w",
			incident.ID,
			err,
		)
	}
	taskUpdatedAt := strings.TrimSpace(details.TaskUpdatedAt)
	if taskUpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, taskUpdatedAt); err != nil {
			return "", fmt.Errorf(
				"parse graph-stall coordination incident %s task version: %w",
				incident.ID,
				err,
			)
		}
	}
	return taskUpdatedAt, nil
}

func graphStalledClaimBlockedByAutoDecompose(
	ctx context.Context,
	q querier,
	incident model.CoordinationIncident,
	current time.Time,
) (bool, error) {
	if incident.Trigger != model.CoordinationTriggerGraphStalled ||
		incident.TaskID == nil {
		return false, nil
	}
	taskUpdatedAt, err := graphStalledIncidentTaskVersion(incident)
	if err != nil {
		return false, err
	}
	return liveAutoDecomposeClaimForTask(
		ctx, q, incident.Board, *incident.TaskID, taskUpdatedAt, current,
	)
}

// autoDecomposeClaimBlockedByGraphStall is the reciprocal check made from the
// same kind of SQLite IMMEDIATE write transaction as Coordinator claims. The
// first claimant wins; an expired Coordinator lease never blocks Planner.
func autoDecomposeClaimBlockedByGraphStall(
	ctx context.Context,
	q querier,
	board string,
	taskID string,
	taskUpdatedAt string,
	current time.Time,
) (*string, bool, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+incidentColumns+`
		FROM coordination_incidents
		WHERE board = ?
		  AND task_id = ?
		  AND trigger = 'graph_stalled'
		  AND status = 'coordinating'
		ORDER BY created_at DESC, id DESC
	`, board, strings.TrimSpace(taskID))
	if err != nil {
		return nil, false, fmt.Errorf(
			"inspect graph-stall coordination claim for task %s: %w",
			taskID,
			err,
		)
	}
	defer rows.Close()
	for rows.Next() {
		incident, err := scanCoordinationIncident(rows)
		if err != nil {
			return nil, false, fmt.Errorf(
				"scan graph-stall coordination claim for task %s: %w",
				taskID,
				err,
			)
		}
		incidentTaskUpdatedAt, err := graphStalledIncidentTaskVersion(incident)
		if err != nil {
			return nil, false, err
		}
		if incidentTaskUpdatedAt != "" &&
			incidentTaskUpdatedAt != taskUpdatedAt {
			continue
		}
		live, err := coordinationIncidentHasLiveClaim(incident, current)
		if err != nil {
			return nil, false, err
		}
		if live {
			return incident.ClaimExpiresAt, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf(
			"iterate graph-stall coordination claims for task %s: %w",
			taskID,
			err,
		)
	}
	return nil, false, nil
}
