package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var ErrResourceBusy = errors.New("workspace resource is busy")

type ResourceBusyError struct {
	Path       string
	OwnerBoard string
	OwnerRunID string
}

func (e *ResourceBusyError) Error() string {
	if e.OwnerBoard != "" {
		return fmt.Sprintf("%s: %s (owned by board %s run %s)", ErrResourceBusy, e.Path, e.OwnerBoard, e.OwnerRunID)
	}
	return fmt.Sprintf("%s: %s (owned by run %s)", ErrResourceBusy, e.Path, e.OwnerRunID)
}

func (e *ResourceBusyError) Unwrap() error { return ErrResourceBusy }

type ResourceLease struct {
	ResourceKey string `json:"resourceKey"`
	RunID       string `json:"runId"`
	Path        string `json:"path"`
	AcquiredAt  string `json:"acquiredAt"`
}

type GlobalWorkspaceLease struct {
	ResourceKey string `json:"resourceKey"`
	Board       string `json:"board"`
	RunID       string `json:"runId"`
	Path        string `json:"path"`
	LeaseToken  string `json:"leaseToken"`
	AcquiredAt  string `json:"acquiredAt"`
}

func workspaceResourceKey(path string) string { return "workspace-write:" + filepath.ToSlash(path) }

func (s *Store) requireCoordinationStore() error {
	if s.board != "default" {
		return fmt.Errorf("coordination resources require the default store, got board %s", s.board)
	}
	return nil
}

func (s *Store) AcquireWorkspaceLease(ctx context.Context, scope RunScope, path string) (ResourceLease, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || !filepath.IsAbs(path) {
		return ResourceLease{}, errors.New("workspace lease requires an absolute path")
	}
	key := workspaceResourceKey(path)
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if err := ensureBoardNotRemoving(ctx, tx, s.board, boardRemovalScopeLocal); err != nil {
			return err
		}
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM resource_leases
			WHERE run_id IN (SELECT id FROM task_runs WHERE status <> 'running')`); err != nil {
			return err
		}
		var owner string
		err = tx.QueryRowContext(ctx, "SELECT run_id FROM resource_leases WHERE resource_key = ?", key).Scan(&owner)
		if err == nil {
			if owner == run.ID {
				return nil
			}
			return &ResourceBusyError{Path: path, OwnerRunID: owner}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, "INSERT INTO resource_leases(resource_key, run_id, path, acquired_at) VALUES (?, ?, ?, ?)", key, run.ID, path, timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "workspace_lease_acquired", map[string]any{"path": path, "resourceKey": key}, &run.ID)
	})
	if err != nil {
		return ResourceLease{}, err
	}
	var lease ResourceLease
	err = s.db.QueryRowContext(ctx, "SELECT resource_key, run_id, path, acquired_at FROM resource_leases WHERE resource_key = ?", key).
		Scan(&lease.ResourceKey, &lease.RunID, &lease.Path, &lease.AcquiredAt)
	return lease, err
}

func (s *Store) ListResourceLeases(ctx context.Context) ([]ResourceLease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, run_id, path, acquired_at FROM resource_leases ORDER BY acquired_at, resource_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ResourceLease, 0)
	for rows.Next() {
		var lease ResourceLease
		if err := rows.Scan(&lease.ResourceKey, &lease.RunID, &lease.Path, &lease.AcquiredAt); err != nil {
			return nil, err
		}
		result = append(result, lease)
	}
	return result, rows.Err()
}

// AcquireGlobalWorkspaceLease atomically records a cross-board writable
// workspace owner. The coordination store cannot validate the run because the
// run belongs to its board database; callers must first acquire the board-local
// lease, which validates the active run and claim token.
func (s *Store) AcquireGlobalWorkspaceLease(ctx context.Context, board, runID, path string) (GlobalWorkspaceLease, bool, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return GlobalWorkspaceLease{}, false, err
	}
	board = strings.TrimSpace(board)
	runID = strings.TrimSpace(runID)
	path = filepath.Clean(strings.TrimSpace(path))
	if board == "" || runID == "" {
		return GlobalWorkspaceLease{}, false, errors.New("global workspace lease requires a board and run ID")
	}
	if path == "" || !filepath.IsAbs(path) {
		return GlobalWorkspaceLease{}, false, errors.New("global workspace lease requires an absolute path")
	}
	key := workspaceResourceKey(path)
	var lease GlobalWorkspaceLease
	acquired := false
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if err := ensureBoardNotRemoving(ctx, tx, board, boardRemovalScopeCoordination); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT resource_key, board, run_id, path, lease_token, acquired_at
			FROM global_workspace_leases WHERE resource_key = ?`, key).Scan(
			&lease.ResourceKey, &lease.Board, &lease.RunID, &lease.Path, &lease.LeaseToken, &lease.AcquiredAt,
		)
		if err == nil {
			acquired = lease.Board == board && lease.RunID == runID && lease.Path == path
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		lease = GlobalWorkspaceLease{ResourceKey: key, Board: board, RunID: runID, Path: path, LeaseToken: newID("wl"), AcquiredAt: now()}
		if _, err := tx.ExecContext(ctx, `INSERT INTO global_workspace_leases(resource_key, board, run_id, path, lease_token, acquired_at)
			VALUES (?, ?, ?, ?, ?, ?)`, lease.ResourceKey, lease.Board, lease.RunID, lease.Path, lease.LeaseToken, lease.AcquiredAt); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return lease, acquired, err
}

// ReleaseGlobalWorkspaceLease removes only the exact lease observed by the
// caller. The random token prevents stale cleanup from deleting a lease that
// was released and reacquired by the same logical owner.
func (s *Store) ReleaseGlobalWorkspaceLease(ctx context.Context, expected GlobalWorkspaceLease) (bool, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return false, err
	}
	if strings.TrimSpace(expected.ResourceKey) == "" || strings.TrimSpace(expected.Board) == "" ||
		strings.TrimSpace(expected.RunID) == "" || strings.TrimSpace(expected.Path) == "" || strings.TrimSpace(expected.LeaseToken) == "" {
		return false, errors.New("exact global workspace lease owner is required")
	}
	var released bool
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM global_workspace_leases
			WHERE resource_key = ? AND board = ? AND run_id = ? AND path = ? AND lease_token = ?`,
			expected.ResourceKey, expected.Board, expected.RunID, expected.Path, expected.LeaseToken)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		released = err == nil && changed == 1
		return err
	})
	return released, err
}

func (s *Store) ListGlobalWorkspaceLeases(ctx context.Context) ([]GlobalWorkspaceLease, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT resource_key, board, run_id, path, lease_token, acquired_at
		FROM global_workspace_leases ORDER BY acquired_at, resource_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GlobalWorkspaceLease, 0)
	for rows.Next() {
		var lease GlobalWorkspaceLease
		if err := rows.Scan(&lease.ResourceKey, &lease.Board, &lease.RunID, &lease.Path, &lease.LeaseToken, &lease.AcquiredAt); err != nil {
			return nil, err
		}
		result = append(result, lease)
	}
	return result, rows.Err()
}

// IsRunTerminal intentionally treats a missing run as an error. Cross-board
// lease cleanup must remain conservative when the owner cannot be verified.
func (s *Store) IsRunTerminal(ctx context.Context, runID string) (bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false, errors.New("run ID is required")
	}
	var status string
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM task_runs WHERE id = ?", runID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("run not found: %s", runID)
		}
		return false, err
	}
	return status != "running", nil
}
