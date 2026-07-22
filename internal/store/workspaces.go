package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type BindRunWorkspaceInput struct {
	Path           string
	Kind           model.WorkspaceKind
	RepositoryPath *string
	BaseCommit     *string
	Generated      bool
}

func scanRunWorkspace(row scanner) (model.RunWorkspace, error) {
	var value model.RunWorkspace
	var repository, base sql.NullString
	var kind string
	var generated int
	err := row.Scan(&value.RunID, &value.TaskID, &value.Path, &kind, &repository, &base, &generated, &value.PreparedAt)
	value.Kind = model.WorkspaceKind(kind)
	value.RepositoryPath = stringPointer(repository)
	value.BaseCommit = stringPointer(base)
	value.Generated = generated != 0
	return value, err
}

func (s *Store) listRunWorkspaces(ctx context.Context, taskID string) ([]model.RunWorkspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, path, kind, repository_path,
		base_commit, generated, prepared_at FROM run_workspaces WHERE task_id = ? ORDER BY prepared_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.RunWorkspace, 0)
	for rows.Next() {
		value, err := scanRunWorkspace(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) GetRunWorkspace(ctx context.Context, runID string) (*model.RunWorkspace, error) {
	value, err := scanRunWorkspace(s.db.QueryRowContext(ctx, `SELECT run_id, task_id, path, kind, repository_path,
		base_commit, generated, prepared_at FROM run_workspaces WHERE run_id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) BindRunWorkspace(ctx context.Context, scope RunScope, input BindRunWorkspaceInput) (model.RunWorkspace, error) {
	resolved, err := filepath.Abs(strings.TrimSpace(input.Path))
	if err != nil || strings.TrimSpace(input.Path) == "" {
		return model.RunWorkspace{}, errors.New("run workspace path cannot be empty")
	}
	if input.Kind != model.WorkspaceScratch && input.Kind != model.WorkspaceDir && input.Kind != model.WorkspaceWorktree {
		return model.RunWorkspace{}, fmt.Errorf("invalid run workspace kind: %s", input.Kind)
	}
	var repository any
	if input.RepositoryPath != nil && strings.TrimSpace(*input.RepositoryPath) != "" {
		value, err := filepath.Abs(strings.TrimSpace(*input.RepositoryPath))
		if err != nil {
			return model.RunWorkspace{}, err
		}
		repository = value
	}
	var base any
	if input.BaseCommit != nil && strings.TrimSpace(*input.BaseCommit) != "" {
		base = strings.TrimSpace(*input.BaseCommit)
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, "SELECT path FROM run_workspaces WHERE run_id = ?", run.ID).Scan(&existing)
		if err == nil {
			if existing != resolved {
				return fmt.Errorf("run workspace is already bound to %s", existing)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_workspaces(run_id, task_id, path, kind, repository_path,
			base_commit, generated, prepared_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, task.ID, resolved,
			input.Kind, repository, base, input.Generated, timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "workspace_prepared", map[string]any{
			"path": resolved, "kind": input.Kind, "repositoryPath": repository, "baseCommit": base,
		}, &run.ID)
	})
	if err != nil {
		return model.RunWorkspace{}, err
	}
	value, err := s.GetRunWorkspace(ctx, scope.RunID)
	if err != nil {
		return model.RunWorkspace{}, err
	}
	if value == nil {
		return model.RunWorkspace{}, errors.New("run workspace binding was not persisted")
	}
	return *value, nil
}
