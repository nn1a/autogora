package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

func scanTerminalRequest(row scanner) (model.TerminalRequest, error) {
	var value model.TerminalRequest
	var summary, result, metadataJSON, blockKind, reason, finalizedAt sql.NullString
	var artifactsJSON string
	err := row.Scan(&value.RunID, &value.Kind, &summary, &result, &metadataJSON, &artifactsJSON,
		&blockKind, &reason, &value.RequestedAt, &finalizedAt)
	value.Summary = stringPointer(summary)
	value.Result = stringPointer(result)
	value.Reason = stringPointer(reason)
	value.FinalizedAt = stringPointer(finalizedAt)
	if blockKind.Valid {
		kind := model.BlockKind(blockKind.String)
		value.BlockKind = &kind
	}
	if metadataJSON.Valid && metadataJSON.String != "" {
		if decodeErr := json.Unmarshal([]byte(metadataJSON.String), &value.Metadata); err == nil && decodeErr != nil {
			err = decodeErr
		}
	}
	if artifactsJSON != "" {
		if decodeErr := json.Unmarshal([]byte(artifactsJSON), &value.Artifacts); err == nil && decodeErr != nil {
			err = decodeErr
		}
	}
	if value.Artifacts == nil {
		value.Artifacts = []string{}
	}
	return value, err
}

func terminalRequestQuery() string {
	return `SELECT run_id, kind, summary, result, metadata_json, artifacts_json, block_kind, reason,
		requested_at, finalized_at FROM run_terminal_requests`
}

func getTerminalRequest(ctx context.Context, q querier, runID string) (*model.TerminalRequest, error) {
	value, err := scanTerminalRequest(q.QueryRowContext(ctx, terminalRequestQuery()+" WHERE run_id = ?", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) GetRunTerminalRequest(ctx context.Context, runID string) (*model.TerminalRequest, error) {
	return getTerminalRequest(ctx, s.db, runID)
}

func (s *Store) listTerminalRequests(ctx context.Context, taskID string) ([]model.TerminalRequest, error) {
	rows, err := s.db.QueryContext(ctx, terminalRequestQuery()+` WHERE run_id IN
		(SELECT id FROM task_runs WHERE task_id = ?) ORDER BY requested_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.TerminalRequest, 0)
	for rows.Next() {
		value, err := scanTerminalRequest(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func terminalJSON(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

func ensureNoTerminalRequest(ctx context.Context, q querier, runID string) error {
	request, err := getTerminalRequest(ctx, q, runID)
	if err != nil {
		return err
	}
	if request != nil {
		return fmt.Errorf("run already requested terminal outcome: %s", request.Kind)
	}
	return nil
}

func (s *Store) MarkRunManaged(ctx context.Context, scope RunScope) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO managed_runs(run_id, registered_at) VALUES (?, ?)", run.ID, now()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "run_managed", nil, &run.ID)
	})
}

func (s *Store) requestRunCompletion(ctx context.Context, scope RunScope, completion CompletionInput) (model.TaskDetail, error) {
	summary, result := strings.TrimSpace(completion.Summary), strings.TrimSpace(completion.Result)
	if summary == "" {
		summary = result
	}
	if summary == "" {
		return model.TaskDetail{}, errors.New("completion requires a summary or result")
	}
	metadata, err := terminalJSON(completion.Metadata)
	if err != nil {
		return model.TaskDetail{}, err
	}
	artifacts := normalizeSkills(completion.Artifacts)
	artifactsJSON, err := terminalJSON(artifacts)
	if err != nil {
		return model.TaskDetail{}, err
	}
	taskID, finalizeNow := "", false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := ensureNoTerminalRequest(ctx, tx, run.ID); err != nil {
			return err
		}
		open, err := hasOpenParents(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if open {
			return errors.New("task prerequisites changed while the run was active; terminate or requeue the run before completing")
		}
		taskID = task.ID
		var managedCount int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM managed_runs WHERE run_id = ?", run.ID).Scan(&managedCount); err != nil {
			return err
		}
		finalizeNow = run.PID == nil && managedCount == 0
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_terminal_requests(run_id, kind, summary, result,
			metadata_json, artifacts_json, requested_at) VALUES (?, 'complete', ?, ?, ?, ?, ?)`, run.ID,
			summary, nullableStringValue(result), metadata, artifactsJSON, timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "completion_requested", map[string]any{
			"summary": truncate(summary, 400), "resultLength": len(result), "artifactCount": len(artifacts),
		}, &run.ID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	if finalizeNow {
		finalized, finalizeErr := s.FinalizeRunTerminal(ctx, scope.RunID, 0)
		if finalizeErr != nil {
			_ = s.DiscardRunTerminalRequest(ctx, scope, "immediate finalization failed")
		}
		return finalized, finalizeErr
	}
	return s.GetTask(ctx, taskID)
}

func validBlockKind(kind model.BlockKind) bool {
	return kind == "" || kind == model.BlockKindDependency || kind == model.BlockKindNeedsInput || kind == model.BlockKindCapability || kind == model.BlockKindTransient
}

func (s *Store) requestRunBlock(ctx context.Context, scope RunScope, input BlockInput) (model.TaskDetail, error) {
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		return model.TaskDetail{}, errors.New("block reason cannot be empty")
	}
	if !validBlockKind(input.Kind) {
		return model.TaskDetail{}, fmt.Errorf("invalid block kind: %s", input.Kind)
	}
	taskID, finalizeNow := "", false
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := ensureNoTerminalRequest(ctx, tx, run.ID); err != nil {
			return err
		}
		taskID = task.ID
		var managedCount int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM managed_runs WHERE run_id = ?", run.ID).Scan(&managedCount); err != nil {
			return err
		}
		finalizeNow = run.PID == nil && managedCount == 0
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_terminal_requests(run_id, kind, block_kind, reason, requested_at)
			VALUES (?, 'block', ?, ?, ?)`, run.ID, nullableStringValue(string(input.Kind)), reason, timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "block_requested", map[string]any{"reason": reason, "kind": input.Kind}, &run.ID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	if finalizeNow {
		finalized, finalizeErr := s.FinalizeRunTerminal(ctx, scope.RunID, 0)
		if finalizeErr != nil {
			_ = s.DiscardRunTerminalRequest(ctx, scope, "immediate finalization failed")
		}
		return finalized, finalizeErr
	}
	return s.GetTask(ctx, taskID)
}

func nullableStringValue(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func (s *Store) DiscardRunTerminalRequest(ctx context.Context, scope RunScope, reason string) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM run_terminal_requests WHERE run_id = ? AND finalized_at IS NULL", run.ID)
		if err != nil {
			return err
		}
		removed, _ := result.RowsAffected()
		if removed == 0 {
			return nil
		}
		return appendEvent(ctx, tx, task.ID, "terminal_request_discarded", map[string]any{"reason": strings.TrimSpace(reason)}, &run.ID)
	})
}

func (s *Store) FinalizeRunTerminal(ctx context.Context, runID string, exitCode int) (model.TaskDetail, error) {
	request, err := s.GetRunTerminalRequest(ctx, runID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if request == nil {
		return model.TaskDetail{}, fmt.Errorf("run has no terminal request: %s", runID)
	}
	run, err := getRun(ctx, s.db, runID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if run.Status != model.RunStatusRunning {
		if request.FinalizedAt != nil {
			return s.GetTask(ctx, run.TaskID)
		}
		return model.TaskDetail{}, fmt.Errorf("cannot finalize terminal run: %s", run.Status)
	}
	if exitCode != 0 && request.Kind != "block" {
		return model.TaskDetail{}, fmt.Errorf("cannot finalize terminal request after exit code %d", exitCode)
	}
	task, err := requireTask(ctx, s.db, run.TaskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	metadata := request.Metadata
	if request.Kind == "complete" && len(request.Artifacts) > 0 {
		workspacePath := ""
		if workspace, err := s.GetRunWorkspace(ctx, runID); err != nil {
			return model.TaskDetail{}, err
		} else if workspace != nil {
			workspacePath = workspace.Path
		}
		if workspacePath == "" && task.Workspace != nil {
			workspacePath = strings.TrimPrefix(strings.TrimPrefix(*task.Workspace, "dir:"), "worktree:")
		}
		captured, err := s.captureArtifactsAt(ctx, task, workspacePath, request.Artifacts)
		if err != nil {
			return model.TaskDetail{}, err
		}
		if metadata == nil {
			metadata = map[string]any{}
		}
		artifacts := make([]map[string]any, 0, len(captured))
		for _, attachment := range captured {
			artifacts = append(artifacts, map[string]any{"id": attachment.ID, "name": attachment.Name, "path": attachment.Path})
		}
		metadata["artifacts"] = artifacts
	}
	metadataJSON, err := terminalJSON(metadata)
	if err != nil {
		return model.TaskDetail{}, err
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		currentRequest, err := getTerminalRequest(ctx, tx, runID)
		if err != nil {
			return err
		}
		if currentRequest == nil {
			return fmt.Errorf("run has no terminal request: %s", runID)
		}
		currentRun, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if currentRun.Status != model.RunStatusRunning {
			if currentRequest.FinalizedAt != nil {
				return nil
			}
			return fmt.Errorf("cannot finalize terminal run: %s", currentRun.Status)
		}
		currentTask, err := requireTask(ctx, tx, currentRun.TaskID)
		if err != nil {
			return err
		}
		if currentTask.CurrentRunID == nil || *currentTask.CurrentRunID != runID || currentTask.Status != model.TaskStatusRunning {
			return errors.New("run no longer owns this task")
		}
		timestamp := now()
		switch currentRequest.Kind {
		case "complete":
			open, err := hasOpenParents(ctx, tx, currentTask.ID)
			if err != nil {
				return err
			}
			if open {
				return errors.New("task prerequisites changed while completion was pending")
			}
			var workspaceKind string
			workspaceErr := tx.QueryRowContext(ctx, `SELECT rw.kind FROM run_workspaces rw
				JOIN managed_runs mr ON mr.run_id = rw.run_id WHERE rw.run_id = ?`, runID).Scan(&workspaceKind)
			if workspaceErr != nil && !errors.Is(workspaceErr, sql.ErrNoRows) {
				return workspaceErr
			}
			if model.WorkspaceKind(workspaceKind) == model.WorkspaceWorktree {
				var state string
				if err := tx.QueryRowContext(ctx, "SELECT state FROM task_change_sets WHERE run_id = ?", runID).Scan(&state); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return errors.New("managed worktree completion requires a durable change set")
					}
					return err
				}
				if !validChangeSetState(state) {
					return fmt.Errorf("change set is not ready: %s", state)
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'completed', ended_at = ?, heartbeat_at = ?,
				exit_code = ?, summary = ?, metadata_json = ? WHERE id = ?`, timestamp, timestamp, exitCode,
				currentRequest.Summary, metadataJSON, runID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'done', current_run_id = NULL, result = ?, failure_count = 0,
				block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ? WHERE id = ?`, currentRequest.Result, timestamp, currentTask.ID); err != nil {
				return err
			}
			summary := ""
			if currentRequest.Summary != nil {
				summary = *currentRequest.Summary
			}
			resultLength := 0
			if currentRequest.Result != nil {
				resultLength = len(*currentRequest.Result)
			}
			if err := appendEvent(ctx, tx, currentTask.ID, "completed", map[string]any{"summary": truncate(summary, 400), "resultLength": resultLength}, &runID); err != nil {
				return err
			}
			if err := satisfyOutgoingDependencies(ctx, tx, currentTask.ID, timestamp); err != nil {
				return err
			}
		case "block":
			reason := ""
			if currentRequest.Reason != nil {
				reason = *currentRequest.Reason
			}
			kind := model.BlockKind("")
			if currentRequest.BlockKind != nil {
				kind = *currentRequest.BlockKind
			}
			if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET status = 'blocked', ended_at = ?, heartbeat_at = ?, exit_code = ?, error = ? WHERE id = ?", timestamp, timestamp, exitCode, reason, runID); err != nil {
				return err
			}
			if err := blockTaskRecord(ctx, tx, currentTask, BlockInput{Reason: reason, Kind: kind}, &runID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid terminal request kind: %s", currentRequest.Kind)
		}
		_, err = tx.ExecContext(ctx, "UPDATE run_terminal_requests SET finalized_at = ? WHERE run_id = ?", timestamp, runID)
		return err
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, run.TaskID)
}
