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
	OwnerRunID string
}

func (e *ResourceBusyError) Error() string {
	return fmt.Sprintf("%s: %s (owned by run %s)", ErrResourceBusy, e.Path, e.OwnerRunID)
}

func (e *ResourceBusyError) Unwrap() error { return ErrResourceBusy }

type ResourceLease struct {
	ResourceKey string `json:"resourceKey"`
	RunID       string `json:"runId"`
	Path        string `json:"path"`
	AcquiredAt  string `json:"acquiredAt"`
}

func workspaceResourceKey(path string) string { return "workspace-write:" + filepath.ToSlash(path) }

func (s *Store) AcquireWorkspaceLease(ctx context.Context, scope RunScope, path string) (ResourceLease, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || !filepath.IsAbs(path) {
		return ResourceLease{}, errors.New("workspace lease requires an absolute path")
	}
	key := workspaceResourceKey(path)
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
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
