package agentcoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
)

const (
	DefaultEphemeralSlotTTL = 3 * time.Minute
	MinEphemeralSlotTTL     = 30 * time.Second
	// EphemeralSlotCleanupGrace keeps planner and judge capacity reserved while
	// a timed-out process exits and its lease is released.
	EphemeralSlotCleanupGrace = 30 * time.Second
	// The supported planner timeout is at most ten minutes. Keep this bound
	// above that timeout plus cleanup grace so a live attempt never loses its
	// slot at the timeout boundary.
	MaxEphemeralSlotTTL = 11 * time.Minute
	leaseReleaseTimeout = 5 * time.Second
)

type Coordinator struct {
	manager *boards.Manager
	now     func() time.Time
}

type Lease struct {
	Slot        store.GlobalAgentSlot
	coordinator *Coordinator
}

func New(manager *boards.Manager) *Coordinator {
	return &Coordinator{manager: manager, now: time.Now}
}

func (c *Coordinator) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *Coordinator) coordinationStore(ctx context.Context) (*store.Store, error) {
	if c == nil || c.manager == nil {
		return nil, errors.New("agent slot coordination requires a board manager")
	}
	return c.manager.OpenCoordinationStore(ctx)
}

func workerOwnerID(agentID, board, runID string) string {
	digest := sha256.Sum256([]byte(agentID + "\x00" + board + "\x00" + runID))
	return "worker:" + hex.EncodeToString(digest[:])
}

func boundedEphemeralTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultEphemeralSlotTTL
	}
	if ttl < MinEphemeralSlotTTL {
		return MinEphemeralSlotTTL
	}
	if ttl > MaxEphemeralSlotTTL {
		return MaxEphemeralSlotTTL
	}
	return ttl
}

func (c *Coordinator) workerRunIsActive(ctx context.Context, board, runID string) (bool, error) {
	if c == nil || c.manager == nil {
		return false, errors.New("agent slot coordination requires a board manager")
	}
	opened, err := c.manager.OpenStore(ctx, board)
	if err != nil {
		return false, err
	}
	terminal, statusErr := opened.IsRunTerminal(ctx, runID)
	processMayStillRun := false
	if statusErr == nil && terminal {
		inspection, inspectErr := opened.GetRun(ctx, runID)
		var identity *string
		var identityErr error
		if inspectErr == nil {
			identity, identityErr = opened.GetRunProcessIdentity(ctx, runID)
		}
		if inspectErr != nil || identityErr != nil {
			statusErr = errors.Join(inspectErr, identityErr)
		} else {
			processMayStillRun = runcontrol.ProcessMayStillBeRunning(inspection.Run.PID, identity)
		}
	}
	closeErr := opened.Close()
	if statusErr != nil || closeErr != nil {
		return false, errors.Join(statusErr, closeErr)
	}
	return !terminal || processMayStillRun, nil
}

// AcquireWorker acquires one global slot for an already-running worker. If the
// configured limit is full, terminal worker owners are verified against their
// board databases and removed with exact-token comparison before one retry.
func (c *Coordinator) AcquireWorker(ctx context.Context, agentID string, limit int, board, runID string) (*Lease, bool, error) {
	agentID = strings.TrimSpace(agentID)
	board = strings.TrimSpace(board)
	runID = strings.TrimSpace(runID)
	if agentID == "" || board == "" || runID == "" {
		return nil, false, errors.New("worker slot requires an agent, board, and run ID")
	}
	active, err := c.workerRunIsActive(ctx, board, runID)
	if err != nil {
		return nil, false, fmt.Errorf("verify worker run %s on board %s: %w", runID, board, err)
	}
	if !active {
		return nil, false, fmt.Errorf("worker run is terminal: %s", runID)
	}
	coordination, err := c.coordinationStore(ctx)
	if err != nil {
		return nil, false, err
	}
	defer coordination.Close()
	input := store.AcquireGlobalAgentSlotInput{
		AgentID: agentID, Limit: limit, OwnerKind: store.AgentSlotOwnerWorker,
		Board: board, RunID: &runID, OwnerID: workerOwnerID(agentID, board, runID), Current: c.currentTime(),
	}
	slot, acquired, err := coordination.AcquireGlobalAgentSlot(ctx, input)
	if err != nil || acquired {
		if err != nil {
			return nil, false, err
		}
		return &Lease{Slot: slot, coordinator: c}, true, nil
	}
	cleaned, err := c.cleanupTerminalWorkers(ctx, coordination, agentID)
	if err != nil {
		return nil, false, err
	}
	if cleaned == 0 {
		return nil, false, nil
	}
	slot, acquired, err = coordination.AcquireGlobalAgentSlot(ctx, input)
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return &Lease{Slot: slot, coordinator: c}, true, nil
}

// AcquireEphemeral acquires a planner or judge slot with a bounded lifetime.
// The database also removes expired non-worker slots during every acquisition,
// so a crashed caller cannot hold capacity indefinitely.
func (c *Coordinator) AcquireEphemeral(ctx context.Context, agentID string, limit int, kind store.AgentSlotOwnerKind, board string, ttl time.Duration) (*Lease, bool, error) {
	if kind != store.AgentSlotOwnerPlanner && kind != store.AgentSlotOwnerJudge {
		return nil, false, fmt.Errorf("ephemeral slot owner must be planner or judge, got %s", kind)
	}
	coordination, err := c.coordinationStore(ctx)
	if err != nil {
		return nil, false, err
	}
	defer coordination.Close()
	slot, acquired, err := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: strings.TrimSpace(agentID), Limit: limit, OwnerKind: kind, Board: strings.TrimSpace(board),
		OwnerID: string(kind) + ":" + uuid.NewString(), TTL: boundedEphemeralTTL(ttl), Current: c.currentTime(),
	})
	if err != nil || !acquired {
		return nil, acquired, err
	}
	return &Lease{Slot: slot, coordinator: c}, true, nil
}

func (c *Coordinator) cleanupTerminalWorkers(ctx context.Context, coordination *store.Store, agentID string) (int, error) {
	slots, err := coordination.ListGlobalAgentSlots(ctx, agentID)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, slot := range slots {
		if slot.OwnerKind != store.AgentSlotOwnerWorker || slot.RunID == nil || strings.TrimSpace(*slot.RunID) == "" {
			continue
		}
		active, err := c.workerRunIsActive(ctx, slot.Board, *slot.RunID)
		if err != nil || active {
			// An unknown board, unavailable database, missing run, or running
			// owner remains busy. Safety takes precedence over reclaiming capacity.
			continue
		}
		released, err := coordination.ReleaseGlobalAgentSlot(ctx, slot)
		if err != nil {
			return cleaned, err
		}
		if released {
			cleaned++
		}
	}
	return cleaned, nil
}

func (c *Coordinator) CleanupTerminalWorkers(ctx context.Context, agentID string) (int, error) {
	coordination, err := c.coordinationStore(ctx)
	if err != nil {
		return 0, err
	}
	defer coordination.Close()
	return c.cleanupTerminalWorkers(ctx, coordination, strings.TrimSpace(agentID))
}

// Release uses a cancellation-independent, bounded context so deferred release
// still runs after a planner, judge, or worker context is canceled.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.coordinator == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	coordination, err := l.coordinator.coordinationStore(releaseCtx)
	if err != nil {
		return err
	}
	defer coordination.Close()
	_, err = coordination.ReleaseGlobalAgentSlot(releaseCtx, l.Slot)
	return err
}
