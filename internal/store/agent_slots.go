package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AgentSlotOwnerKind string

const (
	AgentSlotOwnerWorker      AgentSlotOwnerKind = "worker"
	AgentSlotOwnerPlanner     AgentSlotOwnerKind = "planner"
	AgentSlotOwnerCoordinator AgentSlotOwnerKind = "coordinator"
	AgentSlotOwnerJudge       AgentSlotOwnerKind = "judge"
)

const (
	globalAgentSlotColumns         = "agent_id, slot, owner_kind, board, run_id, owner_id, lease_token, acquired_at, expires_at"
	globalAgentSlotTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type GlobalAgentSlot struct {
	AgentID    string             `json:"agentId"`
	Slot       int                `json:"slot"`
	OwnerKind  AgentSlotOwnerKind `json:"ownerKind"`
	Board      string             `json:"board"`
	RunID      *string            `json:"runId,omitempty"`
	OwnerID    string             `json:"ownerId"`
	LeaseToken string             `json:"leaseToken"`
	AcquiredAt string             `json:"acquiredAt"`
	ExpiresAt  *string            `json:"expiresAt,omitempty"`
}

type AcquireGlobalAgentSlotInput struct {
	AgentID   string
	Limit     int
	OwnerKind AgentSlotOwnerKind
	Board     string
	RunID     *string
	OwnerID   string
	TTL       time.Duration
	Current   time.Time
}

func scanGlobalAgentSlot(row scanner) (GlobalAgentSlot, error) {
	var slot GlobalAgentSlot
	var ownerKind string
	var runID, expiresAt sql.NullString
	if err := row.Scan(
		&slot.AgentID, &slot.Slot, &ownerKind, &slot.Board, &runID,
		&slot.OwnerID, &slot.LeaseToken, &slot.AcquiredAt, &expiresAt,
	); err != nil {
		return GlobalAgentSlot{}, err
	}
	slot.OwnerKind = AgentSlotOwnerKind(ownerKind)
	slot.RunID = stringPointer(runID)
	slot.ExpiresAt = stringPointer(expiresAt)
	return slot, nil
}

func normalizeGlobalAgentSlotInput(input AcquireGlobalAgentSlotInput) (AcquireGlobalAgentSlotInput, string, *string, error) {
	input.AgentID = strings.TrimSpace(input.AgentID)
	input.Board = strings.TrimSpace(input.Board)
	input.OwnerID = strings.TrimSpace(input.OwnerID)
	input.RunID = normalizedPointer(input.RunID)

	switch {
	case input.AgentID == "":
		return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot requires an agent ID")
	case input.Limit <= 0:
		return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot limit must be positive")
	case input.Board == "":
		return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot requires a board")
	case input.OwnerID == "":
		return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot requires an owner ID")
	}

	var expiresAt *string
	switch input.OwnerKind {
	case AgentSlotOwnerWorker:
		if input.RunID == nil {
			return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("worker agent slot requires a run ID")
		}
		if input.TTL != 0 {
			return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("worker agent slot must not expire")
		}
	case AgentSlotOwnerPlanner, AgentSlotOwnerCoordinator, AgentSlotOwnerJudge:
		if input.TTL <= 0 {
			return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("planner, coordinator, and judge agent slots require a positive TTL")
		}
	default:
		return AcquireGlobalAgentSlotInput{}, "", nil, fmt.Errorf("unsupported global agent slot owner kind %q", input.OwnerKind)
	}

	current := input.Current.UTC()
	if current.Year() < 0 || current.Year() > 9999 {
		return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot time must fit RFC3339")
	}
	timestamp := current.Format(globalAgentSlotTimestampLayout)
	if input.OwnerKind != AgentSlotOwnerWorker {
		expires := current.Add(input.TTL)
		if expires.Year() < 0 || expires.Year() > 9999 {
			return AcquireGlobalAgentSlotInput{}, "", nil, errors.New("global agent slot expiry must fit RFC3339")
		}
		formatted := expires.Format(globalAgentSlotTimestampLayout)
		expiresAt = &formatted
	}
	input.Current = current
	return input, timestamp, expiresAt, nil
}

func cleanupExpiredGlobalAgentSlots(ctx context.Context, tx *sql.Tx, timestamp string) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM global_agent_slots
		WHERE owner_kind IN ('planner', 'coordinator', 'judge') AND expires_at <= ?`, timestamp)
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	return int(removed), err
}

// AcquireGlobalAgentSlot atomically allocates the lowest available 1-based
// slot for an agent. Expired short-lived planner, coordinator, and judge slots
// are removed before every allocation; worker slots remain until explicitly
// released.
func (s *Store) AcquireGlobalAgentSlot(ctx context.Context, raw AcquireGlobalAgentSlotInput) (slot GlobalAgentSlot, acquired bool, err error) {
	if err := s.requireCoordinationStore(); err != nil {
		return GlobalAgentSlot{}, false, err
	}
	input, timestamp, expiresAt, err := normalizeGlobalAgentSlotInput(raw)
	if err != nil {
		return GlobalAgentSlot{}, false, err
	}

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		if err := ensureBoardNotRemoving(ctx, tx, input.Board, boardRemovalScopeCoordination); err != nil {
			return err
		}
		if _, err := cleanupExpiredGlobalAgentSlots(ctx, tx, timestamp); err != nil {
			return fmt.Errorf("clean up expired global agent slots: %w", err)
		}

		existing, existingErr := scanGlobalAgentSlot(tx.QueryRowContext(ctx,
			"SELECT "+globalAgentSlotColumns+" FROM global_agent_slots WHERE owner_id = ?", input.OwnerID))
		if existingErr == nil {
			slot = existing
			acquired = existing.AgentID == input.AgentID &&
				existing.OwnerKind == input.OwnerKind &&
				existing.Board == input.Board &&
				equalOptionalString(existing.RunID, input.RunID)
			return nil
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return fmt.Errorf("read global agent slot owner %q: %w", input.OwnerID, existingErr)
		}

		var active int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM global_agent_slots WHERE agent_id = ?", input.AgentID).Scan(&active); err != nil {
			return fmt.Errorf("count global agent slots for %q: %w", input.AgentID, err)
		}
		if active >= input.Limit {
			return nil
		}

		rows, err := tx.QueryContext(ctx,
			"SELECT slot FROM global_agent_slots WHERE agent_id = ? ORDER BY slot", input.AgentID)
		if err != nil {
			return fmt.Errorf("list occupied global agent slots for %q: %w", input.AgentID, err)
		}
		next := 1
		for rows.Next() {
			var occupied int
			if err := rows.Scan(&occupied); err != nil {
				rows.Close()
				return err
			}
			if occupied == next {
				next++
			} else if occupied > next {
				break
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}

		slot = GlobalAgentSlot{
			AgentID: input.AgentID, Slot: next, OwnerKind: input.OwnerKind,
			Board: input.Board, RunID: input.RunID, OwnerID: input.OwnerID,
			LeaseToken: newID("as"), AcquiredAt: timestamp, ExpiresAt: expiresAt,
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO global_agent_slots(
			agent_id, slot, owner_kind, board, run_id, owner_id, lease_token, acquired_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			slot.AgentID, slot.Slot, slot.OwnerKind, slot.Board, nullableString(slot.RunID),
			slot.OwnerID, slot.LeaseToken, slot.AcquiredAt, nullableString(slot.ExpiresAt)); err != nil {
			return fmt.Errorf("acquire global agent slot for %q: %w", input.AgentID, err)
		}
		acquired = true
		return nil
	})
	return slot, acquired, err
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// ReleaseGlobalAgentSlot removes only the exact lease instance observed by the
// caller. LeaseToken prevents a stale owner from deleting a reacquired slot.
func (s *Store) ReleaseGlobalAgentSlot(ctx context.Context, expected GlobalAgentSlot) (bool, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return false, err
	}
	expected.AgentID = strings.TrimSpace(expected.AgentID)
	expected.Board = strings.TrimSpace(expected.Board)
	expected.RunID = normalizedPointer(expected.RunID)
	expected.OwnerID = strings.TrimSpace(expected.OwnerID)
	expected.LeaseToken = strings.TrimSpace(expected.LeaseToken)
	if expected.AgentID == "" || expected.Slot < 1 || expected.Board == "" ||
		expected.OwnerID == "" || expected.LeaseToken == "" {
		return false, errors.New("exact global agent slot owner and token are required")
	}
	switch expected.OwnerKind {
	case AgentSlotOwnerWorker:
		if expected.RunID == nil {
			return false, errors.New("exact worker agent slot requires a run ID")
		}
	case AgentSlotOwnerPlanner, AgentSlotOwnerCoordinator, AgentSlotOwnerJudge:
	default:
		return false, fmt.Errorf("unsupported global agent slot owner kind %q", expected.OwnerKind)
	}

	var released bool
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM global_agent_slots
			WHERE agent_id = ? AND slot = ? AND owner_kind = ? AND board = ?
				AND run_id IS ? AND owner_id = ? AND lease_token = ?`,
			expected.AgentID, expected.Slot, expected.OwnerKind, expected.Board,
			nullableString(expected.RunID), expected.OwnerID, expected.LeaseToken)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		released = err == nil && changed == 1
		return err
	})
	return released, err
}

func (s *Store) ListGlobalAgentSlots(ctx context.Context, agentID string) ([]GlobalAgentSlot, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return nil, err
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, errors.New("global agent slots require an agent ID")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+globalAgentSlotColumns+" FROM global_agent_slots WHERE agent_id = ? ORDER BY slot", agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GlobalAgentSlot, 0)
	for rows.Next() {
		slot, err := scanGlobalAgentSlot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, slot)
	}
	return result, rows.Err()
}

// ListGlobalAgentSlotsForBoard returns exact lease records so board removal
// can conservatively release worker slots whose owning run is terminal.
func (s *Store) ListGlobalAgentSlotsForBoard(ctx context.Context, board string) ([]GlobalAgentSlot, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return nil, err
	}
	board = strings.TrimSpace(board)
	if board == "" {
		return nil, errors.New("global agent slots require a board")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+globalAgentSlotColumns+" FROM global_agent_slots WHERE board = ? ORDER BY agent_id, slot", board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]GlobalAgentSlot, 0)
	for rows.Next() {
		slot, err := scanGlobalAgentSlot(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, slot)
	}
	return result, rows.Err()
}

// CleanupExpiredGlobalAgentSlots removes only expiring planner, coordinator,
// and judge leases. Worker slots deliberately have no expiry and require exact
// release.
func (s *Store) CleanupExpiredGlobalAgentSlots(ctx context.Context, current time.Time) (int, error) {
	if err := s.requireCoordinationStore(); err != nil {
		return 0, err
	}
	current = current.UTC()
	if current.Year() < 0 || current.Year() > 9999 {
		return 0, errors.New("global agent slot cleanup time must fit RFC3339")
	}
	timestamp := current.Format(globalAgentSlotTimestampLayout)
	removed := 0
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		var err error
		removed, err = cleanupExpiredGlobalAgentSlots(ctx, tx, timestamp)
		return err
	})
	return removed, err
}
