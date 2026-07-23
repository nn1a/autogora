package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

// RecoverBlockedRunInput describes a terminal run whose task must remain
// unavailable to another worker until a user reviews the preserved work.
type RecoverBlockedRunInput struct {
	Outcome  model.RunStatus
	Reason   string
	Kind     model.BlockKind
	ExitCode *int
}

func validBlockedRecoveryOutcome(outcome model.RunStatus) bool {
	switch outcome {
	case model.RunStatusBlocked, model.RunStatusFailed, model.RunStatusReclaimed, model.RunStatusCrashed,
		model.RunStatusTimedOut, model.RunStatusRateLimited, model.RunStatusSpawnFailed, model.RunStatusProtocolViolation:
		return true
	default:
		return false
	}
}

func sameStringPointer(value *string, expected string) bool {
	return value != nil && *value == expected
}

func sameBlockKindPointer(value *model.BlockKind, expected model.BlockKind) bool {
	return value != nil && *value == expected
}

func sameOptionalInt(value, expected *int) bool {
	return (value == nil && expected == nil) || (value != nil && expected != nil && *value == *expected)
}

// RecoverRunBlocked atomically terminalizes a run and blocks its task. Keeping
// both changes in one transaction prevents another dispatcher from observing a
// transient Ready task after the terminal-run resource trigger releases its
// workspace lease. Repeating the same recovery is safe and emits no new events.
func (s *Store) RecoverRunBlocked(ctx context.Context, runID string, raw RecoverBlockedRunInput) (model.TaskDetail, error) {
	reason := strings.TrimSpace(raw.Reason)
	if reason == "" {
		return model.TaskDetail{}, errors.New("blocked run recovery requires a reason")
	}
	outcome := raw.Outcome
	if outcome == "" {
		outcome = model.RunStatusBlocked
	}
	if !validBlockedRecoveryOutcome(outcome) {
		return model.TaskDetail{}, fmt.Errorf("invalid blocked run recovery outcome: %s", outcome)
	}
	kind := raw.Kind
	if kind == "" {
		kind = model.BlockKindNeedsInput
	}
	if kind == model.BlockKindDependency || !validBlockKind(kind) {
		return model.TaskDetail{}, fmt.Errorf("invalid blocked run recovery kind: %s", kind)
	}
	var exitCode any
	if raw.ExitCode != nil {
		exitCode = *raw.ExitCode
	}

	taskID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		taskID = task.ID
		if run.Status != model.RunStatusRunning {
			if run.Status == outcome && task.Status == model.TaskStatusBlocked && task.CurrentRunID == nil &&
				sameStringPointer(run.Error, reason) && sameStringPointer(task.BlockReason, reason) &&
				sameBlockKindPointer(task.BlockKind, kind) && sameOptionalInt(run.ExitCode, raw.ExitCode) {
				return nil
			}
			return fmt.Errorf("cannot recover terminal run as blocked: %s", run.Status)
		}
		if task.Status != model.TaskStatusRunning || task.CurrentRunID == nil || *task.CurrentRunID != run.ID {
			return errors.New("run no longer owns this task")
		}

		timestamp := now()
		if _, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = ?, ended_at = ?, heartbeat_at = ?,
			exit_code = ?, error = ? WHERE id = ?`, outcome, timestamp, timestamp, exitCode, reason, run.ID); err != nil {
			return err
		}
		removedRequest := false
		result, err := tx.ExecContext(ctx, "DELETE FROM run_terminal_requests WHERE run_id = ? AND finalized_at IS NULL", run.ID)
		if err != nil {
			return err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if removed > 0 {
			removedRequest = true
		}

		recurrences := 1
		if sameStringPointer(task.BlockReason, reason) && sameBlockKindPointer(task.BlockKind, kind) {
			recurrences = task.BlockRecurrences + 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'blocked', current_run_id = NULL, scheduled_at = NULL,
			block_kind = ?, block_reason = ?, block_recurrences = ?, updated_at = ? WHERE id = ?`,
			kind, reason, recurrences, timestamp, task.ID); err != nil {
			return err
		}
		if removedRequest {
			if err := appendEvent(ctx, tx, task.ID, "terminal_request_discarded", map[string]any{
				"reason": "run recovered directly into a blocked state",
			}, &run.ID); err != nil {
				return err
			}
		}
		if outcome != model.RunStatusBlocked {
			if err := appendEvent(ctx, tx, task.ID, string(outcome), map[string]any{
				"error": reason, "outcome": outcome, "countFailure": false, "preservedWorkspace": true,
			}, &run.ID); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, task.ID, "blocked", map[string]any{
			"reason": reason, "kind": kind, "recurrences": recurrences, "outcome": outcome, "preservedWorkspace": true,
		}, &run.ID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}
