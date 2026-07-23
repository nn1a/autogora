package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

// RecordRunAgentConfigInput describes the resolved profile and agent settings
// that will launch a claimed run.
type RecordRunAgentConfigInput struct {
	Profile      string
	Runtime      model.Runtime
	Model        string
	Provider     string
	Source       string
	FallbackFrom *string
}

const runAgentConfigColumns = `run_id, task_id, profile, runtime, model, provider, source,
	fallback_from, configured_at`

// GetRunAgentConfig returns the immutable execution configuration for a run.
// A run claimed before this feature, or one that has not launched yet, returns
// nil without an error.
func (s *Store) GetRunAgentConfig(ctx context.Context, runID string) (*model.RunAgentConfig, error) {
	value, err := scanRunAgentConfig(s.db.QueryRowContext(ctx, "SELECT "+runAgentConfigColumns+
		" FROM run_agent_configs WHERE run_id = ?", strings.TrimSpace(runID)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// ListTaskRunAgentConfigs returns execution configurations in launch order.
func (s *Store) ListTaskRunAgentConfigs(ctx context.Context, taskID string) ([]model.RunAgentConfig, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+runAgentConfigColumns+
		" FROM run_agent_configs WHERE task_id = ? ORDER BY configured_at, run_id", strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.RunAgentConfig, 0)
	for rows.Next() {
		value, err := scanRunAgentConfig(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func normalizeRunAgentConfigInput(input RecordRunAgentConfigInput) RecordRunAgentConfigInput {
	input.Profile = strings.TrimSpace(input.Profile)
	input.Model = strings.TrimSpace(input.Model)
	input.Provider = strings.TrimSpace(input.Provider)
	input.Source = strings.TrimSpace(input.Source)
	if input.FallbackFrom != nil {
		value := strings.TrimSpace(*input.FallbackFrom)
		if value == "" {
			input.FallbackFrom = nil
		} else {
			input.FallbackFrom = &value
		}
	}
	return input
}

func validateRunAgentConfigInput(input RecordRunAgentConfigInput) error {
	if input.Profile == "" || input.Source == "" {
		return errors.New("run agent config requires profile and source")
	}
	if !model.ValidRuntime(input.Runtime) {
		return fmt.Errorf("invalid run agent config runtime: %s", input.Runtime)
	}
	return nil
}

func sameRunAgentConfig(existing model.RunAgentConfig, input RecordRunAgentConfigInput) bool {
	if existing.Profile != input.Profile || existing.Runtime != input.Runtime || existing.Model != input.Model ||
		existing.Provider != input.Provider || existing.Source != input.Source {
		return false
	}
	if existing.FallbackFrom == nil || input.FallbackFrom == nil {
		return existing.FallbackFrom == nil && input.FallbackFrom == nil
	}
	return *existing.FallbackFrom == *input.FallbackFrom
}

// RecordRunAgentConfig binds one immutable execution snapshot to an active
// claimed run. Retrying the exact value is idempotent; changing any resolved
// setting is rejected so an audit record cannot drift after launch.
func (s *Store) RecordRunAgentConfig(ctx context.Context, scope RunScope, raw RecordRunAgentConfigInput) (model.RunAgentConfig, error) {
	input := normalizeRunAgentConfigInput(raw)
	if err := validateRunAgentConfigInput(input); err != nil {
		return model.RunAgentConfig{}, err
	}
	var recorded model.RunAgentConfig
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if input.Runtime != run.Runtime {
			return fmt.Errorf("run agent config runtime %s does not match active run runtime %s", input.Runtime, run.Runtime)
		}
		existing, err := scanRunAgentConfig(tx.QueryRowContext(ctx, "SELECT "+runAgentConfigColumns+
			" FROM run_agent_configs WHERE run_id = ?", run.ID))
		if err == nil {
			if !sameRunAgentConfig(existing, input) {
				return errors.New("run already has a different agent config")
			}
			recorded = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_agent_configs(
			run_id, task_id, profile, runtime, model, provider, source, fallback_from, configured_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, task.ID, input.Profile, input.Runtime,
			input.Model, input.Provider, input.Source, nullableString(input.FallbackFrom), timestamp); err != nil {
			return err
		}
		recorded = model.RunAgentConfig{
			RunID: run.ID, TaskID: task.ID, Profile: input.Profile, Runtime: input.Runtime,
			Model: input.Model, Provider: input.Provider, Source: input.Source,
			FallbackFrom: input.FallbackFrom, ConfiguredAt: timestamp,
		}
		return appendEvent(ctx, tx, task.ID, "run_agent_configured", map[string]any{
			"profile": input.Profile, "runtime": input.Runtime, "model": input.Model,
			"provider": input.Provider, "source": input.Source, "fallbackFrom": input.FallbackFrom,
		}, &run.ID)
	})
	if err != nil {
		return model.RunAgentConfig{}, err
	}
	return recorded, nil
}
