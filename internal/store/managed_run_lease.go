package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nn1a/autogora/internal/model"
)

// RenewManagedRunLease keeps host-owned setup, Git integration, judgment, and
// finalization phases under the same durable claim as the coding-agent process.
// It deliberately emits no task event; heartbeat_at and claim_expires_at are
// the observable lease state, while run_managed records ownership once.
func (s *Store) RenewManagedRunLease(
	ctx context.Context,
	scope RunScope,
) (model.Run, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		var managed bool
		if err := tx.QueryRowContext(
			ctx,
			`SELECT EXISTS(
				SELECT 1 FROM managed_runs WHERE run_id = ?
			)`,
			run.ID,
		).Scan(&managed); err != nil {
			return err
		}
		if !managed {
			return errors.New("run is not managed by the dispatcher")
		}
		return extendRunLease(ctx, tx, task, run, false)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}
