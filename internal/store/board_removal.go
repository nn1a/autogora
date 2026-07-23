package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrBoardBusy              = errors.New("board has active work or leases")
	ErrBoardRemovalInProgress = errors.New("board removal is in progress")
)

const (
	boardRemovalScopeLocal        = "local"
	boardRemovalScopeCoordination = "coordination"
)

// BoardBusyError reports every resource that must be released before a board
// can be archived or deleted.
type BoardBusyError struct {
	Board                 string
	ActiveRuns            int
	RunningTasks          int
	LocalWorkspaceLeases  int
	GlobalAgentSlots      int
	GlobalWorkspaceLeases int
	LiveTerminalProcesses int
}

func (e *BoardBusyError) Error() string {
	reasons := make([]string, 0, 5)
	if e.ActiveRuns > 0 {
		reasons = append(reasons, fmt.Sprintf("%d active run(s)", e.ActiveRuns))
	}
	if e.RunningTasks > 0 {
		reasons = append(reasons, fmt.Sprintf("%d running task(s)", e.RunningTasks))
	}
	if e.LocalWorkspaceLeases > 0 {
		reasons = append(reasons, fmt.Sprintf("%d local workspace lease(s)", e.LocalWorkspaceLeases))
	}
	if e.GlobalAgentSlots > 0 {
		reasons = append(reasons, fmt.Sprintf("%d global agent lease(s)", e.GlobalAgentSlots))
	}
	if e.GlobalWorkspaceLeases > 0 {
		reasons = append(reasons, fmt.Sprintf("%d global workspace lease(s)", e.GlobalWorkspaceLeases))
	}
	if e.LiveTerminalProcesses > 0 {
		reasons = append(reasons, fmt.Sprintf("%d terminal run process(es) still alive", e.LiveTerminalProcesses))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "unknown active resource")
	}
	return fmt.Sprintf("%s: %s: %s", ErrBoardBusy, e.Board, strings.Join(reasons, ", "))
}

func (e *BoardBusyError) Unwrap() error { return ErrBoardBusy }

// RunProcessOwner is the durable process ownership information needed to
// decide whether a terminal run can safely release its remaining resources.
type RunProcessOwner struct {
	RunID           string
	Status          model.RunStatus
	PID             *int
	ProcessIdentity *string
}

func (s *Store) ListTerminalRunProcesses(ctx context.Context) ([]RunProcessOwner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.status, r.pid, i.process_identity
		FROM task_runs r
		LEFT JOIN run_process_identities i ON i.run_id = r.id
		WHERE r.status <> 'running'
		ORDER BY r.ended_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RunProcessOwner, 0)
	for rows.Next() {
		var owner RunProcessOwner
		var pid sql.NullInt64
		var identity sql.NullString
		if err := rows.Scan(&owner.RunID, &owner.Status, &pid, &identity); err != nil {
			return nil, err
		}
		if pid.Valid {
			value := int(pid.Int64)
			owner.PID = &value
		}
		owner.ProcessIdentity = stringPointer(identity)
		result = append(result, owner)
	}
	return result, rows.Err()
}

// BoardRemovalGuard is an exact-token barrier. A local guard prevents every
// ordinary Store mutation. A coordination guard prevents new global agent and
// workspace leases for the board.
type BoardRemovalGuard struct {
	Board string
	Token string
	Scope string
}

func (s *Store) boardRemovalScope(board string) (string, error) {
	switch {
	case s.board == board:
		return boardRemovalScopeLocal, nil
	case s.board == "default" && board != "default":
		return boardRemovalScopeCoordination, nil
	default:
		return "", fmt.Errorf("board removal guard for %s cannot use store for %s", board, s.board)
	}
}

// AcquireBoardRemovalGuard checks the resources owned by a board and installs
// a write barrier in the same transaction. That atomic check-and-insert closes
// the gap where a claim or lease could otherwise appear after a read-only
// safety check.
func (s *Store) AcquireBoardRemovalGuard(ctx context.Context, rawBoard string) (guard BoardRemovalGuard, err error) {
	board := strings.TrimSpace(rawBoard)
	if board == "" {
		return BoardRemovalGuard{}, errors.New("board removal guard requires a board")
	}
	scope, err := s.boardRemovalScope(board)
	if err != nil {
		return BoardRemovalGuard{}, err
	}

	guard = BoardRemovalGuard{Board: board, Token: newID("br"), Scope: scope}
	err = s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
		var existing string
		existingErr := tx.QueryRowContext(ctx,
			"SELECT token FROM board_removal_guards WHERE board = ? AND scope = ?", board, scope).Scan(&existing)
		switch {
		case existingErr == nil:
			return fmt.Errorf("%w: %s", ErrBoardRemovalInProgress, board)
		case !errors.Is(existingErr, sql.ErrNoRows):
			return existingErr
		}

		busy := &BoardBusyError{Board: board}
		if scope == boardRemovalScopeCoordination {
			timestamp := time.Now().UTC().Format(globalAgentSlotTimestampLayout)
			if _, err := cleanupExpiredGlobalAgentSlots(ctx, tx, timestamp); err != nil {
				return fmt.Errorf("clean up expired global agent leases: %w", err)
			}
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM global_agent_slots WHERE board = ?", board).Scan(&busy.GlobalAgentSlots); err != nil {
				return fmt.Errorf("count global agent leases for board %s: %w", board, err)
			}
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM global_workspace_leases WHERE board = ?", board).Scan(&busy.GlobalWorkspaceLeases); err != nil {
				return fmt.Errorf("count global workspace leases for board %s: %w", board, err)
			}
		} else {
			// Removal deletes the whole board database, so malformed legacy
			// rows carrying another board value must still block it.
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM task_runs WHERE status = 'running'").Scan(&busy.ActiveRuns); err != nil {
				return fmt.Errorf("count active runs for board %s: %w", board, err)
			}
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM tasks WHERE status = 'running'").Scan(&busy.RunningTasks); err != nil {
				return fmt.Errorf("count running tasks for board %s: %w", board, err)
			}
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM resource_leases").Scan(&busy.LocalWorkspaceLeases); err != nil {
				return fmt.Errorf("count local workspace leases for board %s: %w", board, err)
			}
		}
		if busy.ActiveRuns > 0 || busy.RunningTasks > 0 || busy.LocalWorkspaceLeases > 0 ||
			busy.GlobalAgentSlots > 0 || busy.GlobalWorkspaceLeases > 0 {
			return busy
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO board_removal_guards(board, scope, token, acquired_at)
			VALUES (?, ?, ?, ?)`, guard.Board, guard.Scope, guard.Token, now())
		return err
	})
	if err != nil {
		return BoardRemovalGuard{}, err
	}
	return guard, nil
}

// ReleaseBoardRemovalGuard removes only the exact barrier installed by the
// caller. It is used when filesystem removal fails before the board moves.
func (s *Store) ReleaseBoardRemovalGuard(ctx context.Context, guard BoardRemovalGuard) (bool, error) {
	guard.Board = strings.TrimSpace(guard.Board)
	guard.Token = strings.TrimSpace(guard.Token)
	guard.Scope = strings.TrimSpace(guard.Scope)
	if guard.Board == "" || guard.Token == "" ||
		(guard.Scope != boardRemovalScopeLocal && guard.Scope != boardRemovalScopeCoordination) {
		return false, errors.New("exact board removal guard is required")
	}
	var released bool
	err := s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM board_removal_guards
			WHERE board = ? AND scope = ? AND token = ?`, guard.Board, guard.Scope, guard.Token)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		released = err == nil && changed == 1
		return err
	})
	return released, err
}

// ClearBoardRemovalTombstone makes a successfully recreated board eligible for
// new global leases. Only the coordination store can clear this durable marker.
func (s *Store) ClearBoardRemovalTombstone(ctx context.Context, board string) error {
	if err := s.requireCoordinationStore(); err != nil {
		return err
	}
	board = strings.TrimSpace(board)
	if board == "" || board == "default" {
		return errors.New("board removal tombstone requires a non-default board")
	}
	return s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM board_removal_guards WHERE board = ? AND scope = ?", board, boardRemovalScopeCoordination)
		return err
	})
}

// ClearLocalBoardRemovalGuard recovers an interrupted removal after the
// Manager has acquired the non-stealable per-board operating-system lock.
// Callers outside Manager removal recovery must not use this bypass.
func (s *Store) ClearLocalBoardRemovalGuard(ctx context.Context, board string) error {
	board = strings.TrimSpace(board)
	if board == "" || s.board != board {
		return fmt.Errorf("local board removal recovery for %s cannot use store for %s", board, s.board)
	}
	return s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM board_removal_guards WHERE board = ? AND scope = ?", board, boardRemovalScopeLocal)
		return err
	})
}

// HasExactBoardRemovalGuard confirms that the caller still owns the durable
// barrier it installed. Tokens prevent a stale remover from validating a
// replacement barrier.
func (s *Store) HasExactBoardRemovalGuard(ctx context.Context, guard BoardRemovalGuard) (bool, error) {
	guard.Board = strings.TrimSpace(guard.Board)
	guard.Token = strings.TrimSpace(guard.Token)
	guard.Scope = strings.TrimSpace(guard.Scope)
	if guard.Board == "" || guard.Token == "" ||
		(guard.Scope != boardRemovalScopeLocal && guard.Scope != boardRemovalScopeCoordination) {
		return false, errors.New("exact board removal guard is required")
	}
	expectedScope, err := s.boardRemovalScope(guard.Board)
	if err != nil {
		return false, err
	}
	if expectedScope != guard.Scope {
		return false, fmt.Errorf("board removal guard scope %s cannot use store scope %s", guard.Scope, expectedScope)
	}
	var found int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM board_removal_guards
		WHERE board = ? AND scope = ? AND token = ?`, guard.Board, guard.Scope, guard.Token).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) HasBoardRemovalGuard(ctx context.Context, board string) (bool, error) {
	board = strings.TrimSpace(board)
	if board == "" {
		return false, errors.New("board removal guard requires a board")
	}
	scope, err := s.boardRemovalScope(board)
	if err != nil {
		return false, err
	}
	var found int
	err = s.db.QueryRowContext(ctx,
		"SELECT 1 FROM board_removal_guards WHERE board = ? AND scope = ?", board, scope).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func ensureBoardNotRemoving(ctx context.Context, q querier, board, scope string) error {
	var found int
	err := q.QueryRowContext(ctx,
		"SELECT 1 FROM board_removal_guards WHERE board = ? AND scope = ?", board, scope).Scan(&found)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrBoardRemovalInProgress, board)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return err
	}
}
