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

// RecoveryCheckpointContext is the credential-free subset that exceptional
// workflow observers may inspect. Repository/worktree paths, durable refs,
// reservation tokens, and claim tokens are deliberately absent.
type RecoveryCheckpointContext struct {
	ID              string
	State           model.RecoveryCheckpointState
	SourceRunID     string
	ChangedFiles    []string
	CreatedAt       string
	AdoptedAt       *string
	SupersedeReason *string
}

const recoveryCheckpointContextColumns = `id, state, source_run_id,
	changed_files_json, created_at, adopted_at, supersede_reason`

func scanRecoveryCheckpointContext(row scanner) (RecoveryCheckpointContext, error) {
	var value RecoveryCheckpointContext
	var state, changedFilesJSON string
	var adoptedAt, supersedeReason sql.NullString
	if err := row.Scan(
		&value.ID,
		&state,
		&value.SourceRunID,
		&changedFilesJSON,
		&value.CreatedAt,
		&adoptedAt,
		&supersedeReason,
	); err != nil {
		return RecoveryCheckpointContext{}, err
	}
	if err := json.Unmarshal([]byte(changedFilesJSON), &value.ChangedFiles); err != nil {
		return RecoveryCheckpointContext{}, fmt.Errorf(
			"decode recovery checkpoint context changed files: %w",
			err,
		)
	}
	if value.ChangedFiles == nil {
		value.ChangedFiles = []string{}
	}
	value.State = model.RecoveryCheckpointState(state)
	value.AdoptedAt = stringPointer(adoptedAt)
	value.SupersedeReason = stringPointer(supersedeReason)
	return value, nil
}

// ListRecoveryCheckpointContext returns the current handoff, if any, followed
// by a bounded newest-first terminal history. The two reads share one
// transaction so a concurrent adoption cannot duplicate or omit a checkpoint.
func (s *Store) ListRecoveryCheckpointContext(
	ctx context.Context,
	taskID string,
	recentTerminalLimit int,
) ([]RecoveryCheckpointContext, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task ID is required")
	}
	if recentTerminalLimit < 0 || recentTerminalLimit > 10 {
		return nil, errors.New("recent recovery checkpoint limit must be between 0 and 10")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	task, err := requireTask(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Board != s.board {
		return nil, errors.New("recovery checkpoint task belongs to another board")
	}

	result := make([]RecoveryCheckpointContext, 0, recentTerminalLimit+1)
	active, err := scanRecoveryCheckpointContext(tx.QueryRowContext(
		ctx,
		`SELECT `+recoveryCheckpointContextColumns+` FROM recovery_checkpoints
		 WHERE task_id = ? AND state IN ('pending', 'reserved', 'adopted')`,
		taskID,
	))
	switch {
	case err == nil:
		result = append(result, active)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}

	if recentTerminalLimit > 0 {
		rows, err := tx.QueryContext(
			ctx,
			`SELECT `+recoveryCheckpointContextColumns+` FROM recovery_checkpoints
			 WHERE task_id = ? AND state IN ('consumed', 'superseded')
			 ORDER BY created_at DESC, id DESC LIMIT ?`,
			taskID,
			recentTerminalLimit,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			value, err := scanRecoveryCheckpointContext(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, value)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			return nil, errors.Join(rowsErr, closeErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
