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

type RecordChangeSetInput struct {
	RunID          string
	RepositoryPath string
	WorktreePath   string
	BaseCommit     string
	HeadCommit     string
	DurableRef     string
	State          string
	ChangedFiles   []string
}

func scanChangeSet(row scanner) (model.ChangeSet, error) {
	var value model.ChangeSet
	var changedFilesJSON string
	err := row.Scan(&value.ID, &value.RunID, &value.TaskID, &value.RepositoryPath, &value.WorktreePath,
		&value.BaseCommit, &value.HeadCommit, &value.DurableRef, &value.State, &changedFilesJSON, &value.CreatedAt)
	if changedFilesJSON != "" {
		if decodeErr := json.Unmarshal([]byte(changedFilesJSON), &value.ChangedFiles); err == nil && decodeErr != nil {
			err = decodeErr
		}
	}
	if value.ChangedFiles == nil {
		value.ChangedFiles = []string{}
	}
	return value, err
}

const changeSetColumns = `id, run_id, task_id, repository_path, worktree_path, base_commit,
	head_commit, durable_ref, state, changed_files_json, created_at`

func (s *Store) GetRunChangeSet(ctx context.Context, runID string) (*model.ChangeSet, error) {
	value, err := scanChangeSet(s.db.QueryRowContext(ctx, "SELECT "+changeSetColumns+" FROM task_change_sets WHERE run_id = ?", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) listChangeSets(ctx context.Context, taskID string) ([]model.ChangeSet, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+changeSetColumns+" FROM task_change_sets WHERE task_id = ? ORDER BY created_at", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.ChangeSet, 0)
	for rows.Next() {
		value, err := scanChangeSet(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func validChangeSetState(value string) bool { return value == "ready" || value == "no_change" }

func (s *Store) RecordRunChangeSet(ctx context.Context, scope RunScope, input RecordChangeSetInput) (model.ChangeSet, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	if input.RunID != scope.RunID {
		return model.ChangeSet{}, errors.New("change set run does not match active scope")
	}
	if strings.TrimSpace(input.RepositoryPath) == "" || strings.TrimSpace(input.WorktreePath) == "" ||
		strings.TrimSpace(input.BaseCommit) == "" || strings.TrimSpace(input.HeadCommit) == "" || strings.TrimSpace(input.DurableRef) == "" {
		return model.ChangeSet{}, errors.New("change set requires repository, worktree, base, head, and durable ref")
	}
	if !validChangeSetState(input.State) {
		return model.ChangeSet{}, fmt.Errorf("invalid change set state: %s", input.State)
	}
	filesJSON, err := json.Marshal(normalizeSkills(input.ChangedFiles))
	if err != nil {
		return model.ChangeSet{}, err
	}
	id := newID("cs")
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		request, err := getTerminalRequest(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if request == nil || request.Kind != "complete" || request.FinalizedAt != nil {
			return errors.New("change set requires a pending completion request")
		}
		existing, err := scanChangeSet(tx.QueryRowContext(ctx, "SELECT "+changeSetColumns+" FROM task_change_sets WHERE run_id = ?", run.ID))
		if err == nil {
			if existing.BaseCommit == input.BaseCommit && existing.HeadCommit == input.HeadCommit && existing.DurableRef == input.DurableRef {
				id = existing.ID
				return nil
			}
			return errors.New("run already has a different change set")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_change_sets(id, run_id, task_id, repository_path,
			worktree_path, base_commit, head_commit, durable_ref, state, changed_files_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, run.ID, task.ID, input.RepositoryPath,
			input.WorktreePath, input.BaseCommit, input.HeadCommit, input.DurableRef, input.State, string(filesJSON), timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "changeset_recorded", map[string]any{
			"changeSetId": id, "baseCommit": input.BaseCommit, "headCommit": input.HeadCommit,
			"durableRef": input.DurableRef, "state": input.State, "changedFileCount": len(input.ChangedFiles),
		}, &run.ID)
	})
	if err != nil {
		return model.ChangeSet{}, err
	}
	value, err := s.GetRunChangeSet(ctx, scope.RunID)
	if err != nil {
		return model.ChangeSet{}, err
	}
	if value == nil {
		return model.ChangeSet{}, errors.New("recorded change set was not found")
	}
	return *value, nil
}
