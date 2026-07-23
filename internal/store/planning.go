package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

// RecordAutoDecomposeFailure leaves an auditable retry boundary without
// changing task lifecycle state. The dispatcher owns the bounded retry policy.
func (s *Store) RecordAutoDecomposeFailure(ctx context.Context, taskID, failure string, attempt int, retryAt time.Time) error {
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Status != model.TaskStatusTriage {
			return nil
		}
		failure = strings.TrimSpace(failure)
		if len(failure) > 2000 {
			failure = failure[:2000]
		}
		return appendEvent(ctx, tx, taskID, "auto_decompose_failed", map[string]any{
			"error": failure, "attempt": max(1, attempt), "retryAt": retryAt.UTC().Format(time.RFC3339Nano),
		}, nil)
	})
}
