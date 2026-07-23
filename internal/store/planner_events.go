package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type RecordOrchestrationAgentSelectionInput struct {
	Kind         string
	Role         string
	Profile      string
	Runtime      model.Runtime
	Model        string
	Provider     string
	Source       string
	FallbackFrom *string
	Attempt      int
}

func normalizeOrchestrationAgentSelection(input RecordOrchestrationAgentSelectionInput) RecordOrchestrationAgentSelectionInput {
	input.Kind = strings.TrimSpace(input.Kind)
	input.Role = strings.TrimSpace(input.Role)
	input.Profile = strings.TrimSpace(input.Profile)
	input.Model = strings.TrimSpace(input.Model)
	input.Provider = strings.TrimSpace(input.Provider)
	input.Source = strings.TrimSpace(input.Source)
	input.FallbackFrom = normalizedPointer(input.FallbackFrom)
	return input
}

// RecordOrchestrationAgentSelection appends an immutable audit event for a
// planner or judge call. It deliberately records CLI-default model/provider as
// empty strings rather than guessing a model the external CLI selected.
func (s *Store) RecordOrchestrationAgentSelection(ctx context.Context, taskID string, raw RecordOrchestrationAgentSelectionInput) error {
	taskID = strings.TrimSpace(taskID)
	input := normalizeOrchestrationAgentSelection(raw)
	if taskID == "" || input.Kind == "" || input.Role == "" || input.Profile == "" || input.Source == "" {
		return errors.New("orchestration agent selection requires task, kind, role, profile, and source")
	}
	if !model.ValidRuntime(input.Runtime) {
		return fmt.Errorf("invalid orchestration agent runtime: %s", input.Runtime)
	}
	if input.Attempt < 1 {
		return errors.New("orchestration agent selection attempt must be at least 1")
	}
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := requireTask(ctx, tx, taskID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "orchestration_agent_selected", map[string]any{
			"kind": input.Kind, "role": input.Role, "profile": input.Profile,
			"runtime": input.Runtime, "model": input.Model, "provider": input.Provider,
			"source": input.Source, "fallbackFrom": input.FallbackFrom, "attempt": input.Attempt,
		}, nil)
	})
}
