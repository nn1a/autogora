package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

var ErrGraphRevisionConflict = errors.New("graph revision conflict")

type GraphRevisionConflictError struct {
	Board    string
	Expected int64
	Actual   int64
}

func (e *GraphRevisionConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: board %s is at revision %d, expected %d", ErrGraphRevisionConflict, e.Board, e.Actual, e.Expected)
}

func (e *GraphRevisionConflictError) Unwrap() error { return ErrGraphRevisionConflict }

func normalizedBoard(board, fallback string) string {
	board = strings.TrimSpace(board)
	if board == "" {
		board = fallback
	}
	return board
}

func readBoardGraphState(ctx context.Context, q querier, board string) (model.BoardGraphState, error) {
	var state model.BoardGraphState
	err := q.QueryRowContext(ctx,
		"SELECT board, revision, updated_at FROM board_graph_state WHERE board = ?", board,
	).Scan(&state.Board, &state.Revision, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.BoardGraphState{Board: board}, nil
	}
	return state, err
}

func bumpBoardGraphRevision(ctx context.Context, q querier, board string) (model.BoardGraphState, error) {
	timestamp := now()
	if _, err := q.ExecContext(ctx, `
		INSERT INTO board_graph_state(board, revision, updated_at) VALUES (?, 1, ?)
		ON CONFLICT(board) DO UPDATE SET revision = board_graph_state.revision + 1, updated_at = excluded.updated_at
	`, board, timestamp); err != nil {
		return model.BoardGraphState{}, err
	}
	return readBoardGraphState(ctx, q, board)
}

func requireBoardGraphRevision(ctx context.Context, q querier, board string, expected int64) (model.BoardGraphState, error) {
	state, err := readBoardGraphState(ctx, q, board)
	if err != nil {
		return model.BoardGraphState{}, err
	}
	if state.Revision != expected {
		return state, &GraphRevisionConflictError{Board: board, Expected: expected, Actual: state.Revision}
	}
	return state, nil
}

func (s *Store) GetBoardGraphState(ctx context.Context, board string) (model.BoardGraphState, error) {
	return readBoardGraphState(ctx, s.db, normalizedBoard(board, s.board))
}
