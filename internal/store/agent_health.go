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

// AgentHealthObservation identifies one availability check in its causal
// start order. Call BeginAgentHealthObservation immediately before invoking an
// agent, then use ApplyAgentHealthObservation when that invocation finishes.
type AgentHealthObservation struct {
	AgentID    string
	Generation int64
}

// AgentHealthUpdate reports the authoritative state after an observed write.
// Applied is false when a newer observation already won the compare-and-swap.
type AgentHealthUpdate struct {
	Health      model.AgentHealth
	Observation AgentHealthObservation
	Applied     bool
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

// SetAgentHealth records an immediate observation atomically. Long-running
// checks must reserve their causal order with BeginAgentHealthObservation and
// finish with ApplyAgentHealthObservation instead.
func (s *Store) SetAgentHealth(ctx context.Context, raw SetAgentHealthInput) (model.AgentHealth, error) {
	input, err := normalizeAgentHealthInput(raw)
	if err != nil {
		return model.AgentHealth{}, err
	}
	var update AgentHealthUpdate
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		observation, err := reserveAgentHealthObservation(ctx, tx, input.AgentID)
		if err != nil {
			return err
		}
		update, err = applyAgentHealthObservation(ctx, tx, observation, input)
		return err
	})
	return update.Health, err
}

// BeginAgentHealthObservation reserves the next generation for an availability
// check without changing the visible health state.
func (s *Store) BeginAgentHealthObservation(ctx context.Context, agentID string) (AgentHealthObservation, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return AgentHealthObservation{}, errors.New("agent health observation requires an agent ID")
	}
	var observation AgentHealthObservation
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		var err error
		observation, err = reserveAgentHealthObservation(ctx, tx, agentID)
		return err
	})
	return observation, err
}

// ApplyAgentHealthObservation records an availability result unless a check
// that started later has already applied its result.
func (s *Store) ApplyAgentHealthObservation(
	ctx context.Context,
	observation AgentHealthObservation,
	raw SetAgentHealthInput,
) (AgentHealthUpdate, error) {
	input, err := normalizeAgentHealthInput(raw)
	if err != nil {
		return AgentHealthUpdate{}, err
	}
	observation.AgentID = strings.TrimSpace(observation.AgentID)
	if observation.AgentID == "" {
		return AgentHealthUpdate{}, errors.New("agent health observation requires an agent ID")
	}
	if observation.Generation <= 0 {
		return AgentHealthUpdate{}, errors.New("agent health observation requires a positive generation")
	}
	if observation.AgentID != input.AgentID {
		return AgentHealthUpdate{}, fmt.Errorf(
			"agent health observation belongs to %s, not %s",
			observation.AgentID,
			input.AgentID,
		)
	}
	var update AgentHealthUpdate
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		var err error
		update, err = applyAgentHealthObservation(ctx, tx, observation, input)
		return err
	})
	return update, err
}

func reserveAgentHealthObservation(ctx context.Context, tx *sql.Tx, agentID string) (AgentHealthObservation, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_health_observation_sequences(agent_id, next_generation, applied_generation)
		VALUES (?, 1, 0)
		ON CONFLICT(agent_id) DO UPDATE SET
			next_generation = agent_health_observation_sequences.next_generation + 1
	`, agentID); err != nil {
		return AgentHealthObservation{}, fmt.Errorf("reserve agent health observation: %w", err)
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `
		SELECT next_generation
		FROM agent_health_observation_sequences
		WHERE agent_id = ?
	`, agentID).Scan(&generation); err != nil {
		return AgentHealthObservation{}, fmt.Errorf("read agent health observation: %w", err)
	}
	return AgentHealthObservation{AgentID: agentID, Generation: generation}, nil
}

func applyAgentHealthObservation(
	ctx context.Context,
	tx *sql.Tx,
	observation AgentHealthObservation,
	input SetAgentHealthInput,
) (AgentHealthUpdate, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_health_observation_sequences(agent_id, next_generation, applied_generation)
		VALUES (?, ?, 0)
		ON CONFLICT(agent_id) DO UPDATE SET
			next_generation = MAX(
				agent_health_observation_sequences.next_generation,
				excluded.next_generation
			)
	`, observation.AgentID, observation.Generation); err != nil {
		return AgentHealthUpdate{}, fmt.Errorf("register agent health observation: %w", err)
	}
	var appliedGeneration int64
	if err := tx.QueryRowContext(ctx, `
		SELECT applied_generation
		FROM agent_health_observation_sequences
		WHERE agent_id = ?
	`, observation.AgentID).Scan(&appliedGeneration); err != nil {
		return AgentHealthUpdate{}, fmt.Errorf("read applied agent health observation: %w", err)
	}
	if observation.Generation <= appliedGeneration {
		health, err := scanAgentHealth(tx.QueryRowContext(ctx, "SELECT "+agentHealthColumns+
			" FROM agent_health WHERE agent_id = ?", input.AgentID))
		if err != nil {
			return AgentHealthUpdate{}, fmt.Errorf("read newer agent health observation: %w", err)
		}
		return AgentHealthUpdate{Health: health, Observation: observation}, nil
	}

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
		return AgentHealthUpdate{}, fmt.Errorf("set agent health: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE agent_health_observation_sequences
		SET applied_generation = ?
		WHERE agent_id = ? AND applied_generation < ?
	`, observation.Generation, observation.AgentID, observation.Generation)
	if err != nil {
		return AgentHealthUpdate{}, fmt.Errorf("apply agent health observation generation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AgentHealthUpdate{}, fmt.Errorf("inspect agent health observation generation: %w", err)
	}
	if affected != 1 {
		return AgentHealthUpdate{}, errors.New("agent health observation lost its transaction ordering")
	}
	health, err := scanAgentHealth(tx.QueryRowContext(ctx, "SELECT "+agentHealthColumns+
		" FROM agent_health WHERE agent_id = ?", input.AgentID))
	if err != nil {
		return AgentHealthUpdate{}, err
	}
	return AgentHealthUpdate{Health: health, Observation: observation, Applied: true}, nil
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
	var cleared int64
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE agent_health
			SET status = 'unknown', cooldown_until = NULL, updated_at = ?
			WHERE status IN ('missing', 'auth_required', 'rate_limited', 'unhealthy')
				AND cooldown_until IS NOT NULL AND cooldown_until <= ?`, timestamp, timestamp)
		if err != nil {
			return err
		}
		cleared, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("clear expired agent cooldowns: %w", err)
	}
	return cleared, nil
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
