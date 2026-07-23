package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type AutoDecomposeObservationKind string

const (
	AutoDecomposeObservationNoState          AutoDecomposeObservationKind = "no_state"
	AutoDecomposeObservationStaleVersion     AutoDecomposeObservationKind = "stale_version"
	AutoDecomposeObservationBackoff          AutoDecomposeObservationKind = "backoff"
	AutoDecomposeObservationDue              AutoDecomposeObservationKind = "due"
	AutoDecomposeObservationExpiredRetryable AutoDecomposeObservationKind = "expired_retryable"
	AutoDecomposeObservationLiveClaim        AutoDecomposeObservationKind = "live_claim"
	AutoDecomposeObservationExhausted        AutoDecomposeObservationKind = "exhausted"
)

// AutoDecomposeObservation classifies every Triage task from the scheduler's
// point of view. Only a matching-version task with no live lease and a fully
// consumed current attempt budget belongs to exceptional recovery.
type AutoDecomposeObservation struct {
	TaskID        string
	TaskUpdatedAt string
	Kind          AutoDecomposeObservationKind
	State         *AutoDecomposeState
}

func (o AutoDecomposeObservation) PlannerOwned() bool {
	switch o.Kind {
	case AutoDecomposeObservationNoState,
		AutoDecomposeObservationStaleVersion,
		AutoDecomposeObservationBackoff,
		AutoDecomposeObservationDue,
		AutoDecomposeObservationExpiredRetryable,
		AutoDecomposeObservationLiveClaim:
		return true
	default:
		return false
	}
}

func classifyAutoDecomposeObservation(
	taskID string,
	taskUpdatedAt string,
	state *AutoDecomposeState,
	current time.Time,
	maxAttempts int,
) (AutoDecomposeObservation, error) {
	observation := AutoDecomposeObservation{
		TaskID: taskID, TaskUpdatedAt: taskUpdatedAt, State: state,
	}
	if state == nil {
		observation.Kind = AutoDecomposeObservationNoState
		return observation, nil
	}
	if state.TaskUpdatedAt != taskUpdatedAt {
		observation.Kind = AutoDecomposeObservationStaleVersion
		return observation, nil
	}
	if (state.ClaimToken == nil) != (state.ClaimExpiresAt == nil) {
		return AutoDecomposeObservation{}, fmt.Errorf(
			"auto-decompose state for %s has an incomplete claim lease",
			taskID,
		)
	}
	if state.ClaimToken != nil {
		expiresAt, err := parseAutoDecomposeTimestamp("claim expiry", *state.ClaimExpiresAt)
		if err != nil {
			return AutoDecomposeObservation{}, err
		}
		if expiresAt.After(current) {
			// A live final attempt is still active Planner work. Exhaustion
			// begins only after that lease ends without a successful result.
			observation.Kind = AutoDecomposeObservationLiveClaim
			return observation, nil
		}
	}
	if state.Attempts >= maxAttempts {
		observation.Kind = AutoDecomposeObservationExhausted
		return observation, nil
	}
	if state.ClaimToken != nil {
		observation.Kind = AutoDecomposeObservationExpiredRetryable
		return observation, nil
	}
	if state.NextAttemptAt != nil {
		retryAt, err := parseAutoDecomposeTimestamp("retry time", *state.NextAttemptAt)
		if err != nil {
			return AutoDecomposeObservation{}, err
		}
		if retryAt.After(current) {
			observation.Kind = AutoDecomposeObservationBackoff
			return observation, nil
		}
	}
	observation.Kind = AutoDecomposeObservationDue
	return observation, nil
}

// ListAutoDecomposeObservations reads every Triage task with an optional
// scheduler row in one board-scoped snapshot. The caller supplies the current
// attempt policy so a state written under an older limit cannot be mistaken
// for either active planning or exhaustion.
func (s *Store) ListAutoDecomposeObservations(
	ctx context.Context,
	current time.Time,
	maxAttempts int,
) (map[string]AutoDecomposeObservation, error) {
	if _, err := autoDecomposeTimestamp(current); err != nil {
		return nil, err
	}
	if maxAttempts < 1 || maxAttempts > 32 {
		return nil, fmt.Errorf(
			"auto-decompose observation max attempts must be between 1 and 32",
		)
	}
	current = current.UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			task.id, task.updated_at,
			state.task_id, state.task_updated_at, state.attempts, state.max_attempts,
			state.next_attempt_at, state.claim_token, state.claim_expires_at,
			state.last_error, state.updated_at
		FROM tasks task
		LEFT JOIN auto_decompose_state state ON state.task_id = task.id
		WHERE task.board = ?
		  AND task.status = 'triage'
		ORDER BY task.id
	`, s.board)
	if err != nil {
		return nil, fmt.Errorf("list auto-decompose observations: %w", err)
	}
	defer rows.Close()

	observations := make(map[string]AutoDecomposeObservation)
	for rows.Next() {
		var taskID, taskUpdatedAt string
		var stateTaskID, stateTaskUpdatedAt, nextAttemptAt sql.NullString
		var claimToken, claimExpiresAt, lastError, stateUpdatedAt sql.NullString
		var attempts, stateMaxAttempts sql.NullInt64
		if err := rows.Scan(
			&taskID, &taskUpdatedAt,
			&stateTaskID, &stateTaskUpdatedAt, &attempts, &stateMaxAttempts,
			&nextAttemptAt, &claimToken, &claimExpiresAt, &lastError, &stateUpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan auto-decompose observation: %w", err)
		}
		var state *AutoDecomposeState
		if stateTaskID.Valid {
			state = &AutoDecomposeState{
				TaskID:         stateTaskID.String,
				TaskUpdatedAt:  stateTaskUpdatedAt.String,
				Attempts:       int(attempts.Int64),
				MaxAttempts:    int(stateMaxAttempts.Int64),
				NextAttemptAt:  stringPointer(nextAttemptAt),
				ClaimToken:     stringPointer(claimToken),
				ClaimExpiresAt: stringPointer(claimExpiresAt),
				LastError:      stringPointer(lastError),
				UpdatedAt:      stateUpdatedAt.String,
			}
		}
		observation, err := classifyAutoDecomposeObservation(
			taskID, taskUpdatedAt, state, current, maxAttempts,
		)
		if err != nil {
			return nil, err
		}
		observations[taskID] = observation
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auto-decompose observations: %w", err)
	}
	return observations, nil
}
