package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const (
	minBoardFailureBackoff = 250 * time.Millisecond
	maxBoardFailureBackoff = time.Minute
)

// globalCoordinationError marks a shared coordination-store failure. A
// per-board circuit must never hide this error because continuing could violate
// global capacity, health, or ownership guarantees across every board.
type globalCoordinationError struct {
	operation string
	err       error
}

func (e *globalCoordinationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.operation, e.err)
}

func (e *globalCoordinationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func markGlobalCoordinationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var existing *globalCoordinationError
	if errors.As(err, &existing) {
		return err
	}
	return &globalCoordinationError{operation: operation, err: err}
}

func isGlobalCoordinationError(err error) bool {
	var target *globalCoordinationError
	return errors.As(err, &target)
}

type boardFailureState struct {
	failures   int
	generation uint64
	probing    bool
	retryAt    time.Time
}

type boardFailure struct {
	Failures   int
	Generation uint64
	Delay      time.Duration
	RetryAt    time.Time
}

// boardFailureCircuit keeps local infrastructure failures from turning a
// watch loop into either a process-wide outage or a hot retry loop. It is
// intentionally owned by the dispatcher goroutine; worker goroutines report
// results through the existing completion queue instead of mutating it.
type boardFailureCircuit struct {
	base   time.Duration
	limit  time.Duration
	states map[string]boardFailureState
	now    func() time.Time
}

func newBoardFailureCircuit(base time.Duration, now func() time.Time) *boardFailureCircuit {
	if base < minBoardFailureBackoff {
		base = minBoardFailureBackoff
	}
	if base > maxBoardFailureBackoff {
		base = maxBoardFailureBackoff
	}
	if now == nil {
		now = time.Now
	}
	return &boardFailureCircuit{
		base: base, limit: maxBoardFailureBackoff,
		states: make(map[string]boardFailureState), now: now,
	}
}

func (c *boardFailureCircuit) currentTime() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now().UTC()
}

func (c *boardFailureCircuit) generation(board string) uint64 {
	if c == nil {
		return 0
	}
	return c.states[board].generation
}

func (c *boardFailureCircuit) ready(board string) bool {
	if c == nil {
		return true
	}
	state, exists := c.states[board]
	return !exists || state.failures == 0 ||
		(!state.probing && !c.currentTime().Before(state.retryAt))
}

// beginProbe reserves the single half-open probe allowed after backoff. A
// healthy board does not need a reservation and may keep filling available
// worker slots normally.
func (c *boardFailureCircuit) beginProbe(board string) bool {
	if c == nil {
		return true
	}
	state, exists := c.states[board]
	if !exists || state.failures == 0 {
		return true
	}
	if state.probing || c.currentTime().Before(state.retryAt) {
		return false
	}
	state.probing = true
	c.states[board] = state
	return true
}

func (c *boardFailureCircuit) failure(board string) boardFailure {
	if c == nil {
		return boardFailure{}
	}
	state := c.states[board]
	state.failures++
	state.generation++
	state.probing = false
	delay := c.base
	for attempt := 1; attempt < state.failures && delay < c.limit; attempt++ {
		if delay > c.limit/2 {
			delay = c.limit
			break
		}
		delay *= 2
	}
	if delay > c.limit {
		delay = c.limit
	}
	state.retryAt = c.currentTime().Add(delay)
	c.states[board] = state
	return boardFailure{
		Failures: state.failures, Generation: state.generation,
		Delay: delay, RetryAt: state.retryAt,
	}
}

// success clears a circuit only when no newer failure happened after the
// operation or worker started. This prevents a slow successful worker from
// erasing a newer failure reported by another worker on the same board.
func (c *boardFailureCircuit) success(board string, generation uint64) bool {
	if c == nil {
		return false
	}
	state, exists := c.states[board]
	if !exists || state.failures == 0 || state.generation != generation {
		return false
	}
	state.failures = 0
	state.probing = false
	state.retryAt = time.Time{}
	c.states[board] = state
	return true
}

func (c *boardFailureCircuit) eligible(boards []string) []string {
	result := make([]string, 0, len(boards))
	for _, board := range boards {
		if c.ready(board) {
			result = append(result, board)
		}
	}
	return result
}

func (c *boardFailureCircuit) retain(boards []string) {
	if c == nil {
		return
	}
	known := make(map[string]struct{}, len(boards))
	for _, board := range boards {
		known[board] = struct{}{}
	}
	for board := range c.states {
		if _, exists := known[board]; !exists {
			delete(c.states, board)
		}
	}
}

func resilientAllBoardWatch(options Options) bool {
	return !options.Once &&
		strings.TrimSpace(options.Board) == "" &&
		strings.TrimSpace(options.TaskID) == "" &&
		options.ExpectedUpdatedAt == nil
}

// dispatcherTestHooks provide deterministic fault injection without weakening
// the production interfaces. They are unexported and only same-package tests
// can install them.
type dispatcherTestHooks struct {
	readMetadata     func(*boards.Manager, string) (boards.Metadata, error)
	openStore        func(context.Context, *boards.Manager, string) (*store.Store, error)
	maintainBoard    func(context.Context, *boards.Manager, string, Options) error
	claimProfile     func(context.Context, *boards.Manager, *store.Store, string, Options) ([]string, map[string]int, error)
	claimTask        func(context.Context, *store.Store, store.ClaimOptions) (*model.ClaimedTask, error)
	runClaim         func(context.Context, *boards.Manager, *store.Store, *model.ClaimedTask, Options, *ProcessSet, string) error
	discoveredBoards func(context.Context, string) ([]string, error)
}

func (o Options) readBoardMetadata(manager *boards.Manager, board string) (boards.Metadata, error) {
	if o.testHooks != nil && o.testHooks.readMetadata != nil {
		return o.testHooks.readMetadata(manager, board)
	}
	return manager.Read(board)
}

func (o Options) openBoardStore(ctx context.Context, manager *boards.Manager, board string) (*store.Store, error) {
	if o.testHooks != nil && o.testHooks.openStore != nil {
		return o.testHooks.openStore(ctx, manager, board)
	}
	return manager.OpenStore(ctx, board)
}

func (o Options) maintainOneBoard(ctx context.Context, manager *boards.Manager, board string) error {
	if o.testHooks != nil && o.testHooks.maintainBoard != nil {
		return o.testHooks.maintainBoard(ctx, manager, board, o)
	}
	return maintainBoard(ctx, manager, board, o)
}

func (o Options) boardClaimProfilePolicy(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	board string,
) ([]string, map[string]int, error) {
	if o.testHooks != nil && o.testHooks.claimProfile != nil {
		return o.testHooks.claimProfile(ctx, manager, opened, board, o)
	}
	return claimProfilePolicy(ctx, manager, opened, board, o)
}

func (o Options) claimBoardTask(
	ctx context.Context,
	opened *store.Store,
	input store.ClaimOptions,
) (*model.ClaimedTask, error) {
	if o.testHooks != nil && o.testHooks.claimTask != nil {
		return o.testHooks.claimTask(ctx, opened, input)
	}
	return opened.ClaimTask(ctx, input)
}

func (o Options) executeClaim(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	claim *model.ClaimedTask,
	processes *ProcessSet,
	approvalDir string,
) error {
	if o.testHooks != nil && o.testHooks.runClaim != nil {
		return o.testHooks.runClaim(ctx, manager, opened, claim, o, processes, approvalDir)
	}
	return runClaim(ctx, manager, opened, claim, o, processes, approvalDir)
}

func (o Options) discoverBoards(ctx context.Context) ([]string, error) {
	if o.testHooks != nil && o.testHooks.discoveredBoards != nil {
		return o.testHooks.discoveredBoards(ctx, o.DBPath)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dbPath, err := filepath.Abs(o.DBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve dispatcher database path: %w", err)
	}
	root := filepath.Join(filepath.Dir(dbPath), "boards")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("discover boards: %w", err)
	}
	result := []string{"default"}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "_archived" {
			continue
		}
		slug, normalizeErr := boards.NormalizeSlug(entry.Name())
		if normalizeErr != nil || slug == "default" {
			continue
		}
		result = append(result, slug)
	}
	sort.Strings(result[1:])
	return result, nil
}
