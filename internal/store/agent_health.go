package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

const maxAgentHealthErrorBytes = 4 * 1024

type SetAgentHealthInput struct {
	AgentID       string
	Status        model.AgentHealthStatus
	CooldownUntil *string
	LastError     *string
	LastRunID     *string
}

const agentHealthColumns = `agent_id, status, cooldown_until, last_error, last_run_id, updated_at`

func scanAgentHealth(row scanner) (model.AgentHealth, error) {
	var value model.AgentHealth
	var status string
	var cooldownUntil, lastError, lastRunID sql.NullString
	if err := row.Scan(
		&value.AgentID, &status, &cooldownUntil, &lastError, &lastRunID, &value.UpdatedAt,
	); err != nil {
		return model.AgentHealth{}, err
	}
	value.Status = model.AgentHealthStatus(status)
	value.CooldownUntil = stringPointer(cooldownUntil)
	value.LastError = stringPointer(lastError)
	value.LastRunID = stringPointer(lastRunID)
	return value, nil
}

func boundedAgentHealthError(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	if len(trimmed) <= maxAgentHealthErrorBytes {
		return &trimmed
	}
	bounded := trimmed[:maxAgentHealthErrorBytes]
	for !utf8.ValidString(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	return &bounded
}

func normalizeAgentHealthInput(input SetAgentHealthInput) (SetAgentHealthInput, error) {
	input.AgentID = strings.TrimSpace(input.AgentID)
	if input.AgentID == "" {
		return SetAgentHealthInput{}, errors.New("agent health requires an agent ID")
	}
	if !model.ValidAgentHealthStatus(input.Status) {
		return SetAgentHealthInput{}, fmt.Errorf("invalid agent health status: %s", input.Status)
	}
	cooldown, err := normalizeISO(input.CooldownUntil, "agent cooldown")
	if err != nil {
		return SetAgentHealthInput{}, err
	}
	input.CooldownUntil = cooldown
	input.LastError = boundedAgentHealthError(input.LastError)
	input.LastRunID = normalizedPointer(input.LastRunID)
	return input, nil
}

// SetAgentHealth records the latest observation for an agent. Repeated calls
// replace the previous observation while preserving the agent identity.
func (s *Store) SetAgentHealth(ctx context.Context, raw SetAgentHealthInput) (model.AgentHealth, error) {
	input, err := normalizeAgentHealthInput(raw)
	if err != nil {
		return model.AgentHealth{}, err
	}
	var value model.AgentHealth
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_health(
			agent_id, status, cooldown_until, last_error, last_run_id, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			status = excluded.status,
			cooldown_until = excluded.cooldown_until,
			last_error = excluded.last_error,
			last_run_id = excluded.last_run_id,
			updated_at = excluded.updated_at`, input.AgentID, input.Status,
			nullableString(input.CooldownUntil), nullableString(input.LastError),
			nullableString(input.LastRunID), timestamp); err != nil {
			return fmt.Errorf("set agent health: %w", err)
		}
		value, err = scanAgentHealth(tx.QueryRowContext(ctx, "SELECT "+agentHealthColumns+
			" FROM agent_health WHERE agent_id = ?", input.AgentID))
		return err
	})
	return value, err
}

// GetAgentHealth returns a synthesized unknown state when an agent has not
// been observed. UpdatedAt remains empty in that case.
func (s *Store) GetAgentHealth(ctx context.Context, agentID string) (model.AgentHealth, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return model.AgentHealth{}, errors.New("agent health requires an agent ID")
	}
	value, err := scanAgentHealth(s.db.QueryRowContext(ctx, "SELECT "+agentHealthColumns+
		" FROM agent_health WHERE agent_id = ?", agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AgentHealth{AgentID: agentID, Status: model.AgentHealthUnknown}, nil
	}
	return value, err
}

func (s *Store) ListAgentHealth(ctx context.Context) ([]model.AgentHealth, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+agentHealthColumns+" FROM agent_health ORDER BY agent_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.AgentHealth, 0)
	for rows.Next() {
		value, err := scanAgentHealth(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// ClearExpiredAgentCooldowns makes agents eligible for another availability
// check. It intentionally does not assume that an expired cooldown is ready.
func (s *Store) ClearExpiredAgentCooldowns(ctx context.Context, current time.Time) (int64, error) {
	timestamp := current.UTC().Format("2006-01-02T15:04:05.000Z")
	result, err := s.db.ExecContext(ctx, `UPDATE agent_health
		SET status = 'unknown', cooldown_until = NULL, updated_at = ?
		WHERE status IN ('missing', 'auth_required', 'rate_limited', 'unhealthy')
			AND cooldown_until IS NOT NULL AND cooldown_until <= ?`, timestamp, timestamp)
	if err != nil {
		return 0, fmt.Errorf("clear expired agent cooldowns: %w", err)
	}
	return result.RowsAffected()
}

// IsAgentUnavailable reports whether the latest observation should prevent a
// dispatch attempt. An expired rate-limit cooldown is eligible for a probe.
func IsAgentUnavailable(health model.AgentHealth, current time.Time) bool {
	switch health.Status {
	case model.AgentHealthMissing, model.AgentHealthAuthRequired, model.AgentHealthUnhealthy:
		if health.CooldownUntil == nil {
			return true
		}
		until, err := time.Parse(time.RFC3339Nano, *health.CooldownUntil)
		return err != nil || current.Before(until)
	case model.AgentHealthRateLimited:
		if health.CooldownUntil == nil {
			return true
		}
		until, err := time.Parse(time.RFC3339Nano, *health.CooldownUntil)
		return err != nil || current.Before(until)
	default:
		return false
	}
}
