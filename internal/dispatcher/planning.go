package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

const (
	autoDecomposeDiagnosticEntries  = 2048
	autoDecomposeCandidatePageSize  = 100
	autoDecomposeCandidateScanLimit = 1000
	autoDecomposeClaimGrace         = 30 * time.Second
	autoDecomposeHeartbeatMaxPeriod = 30 * time.Second
)

func autoDecomposeClaimTTL(options Options) time.Duration {
	timeout := options.PlannerTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ttl := timeout + autoDecomposeClaimGrace
	if ttl < time.Second {
		return time.Second
	}
	if ttl > 15*time.Minute {
		return 15 * time.Minute
	}
	return ttl
}

func autoDecomposeHeartbeatPeriod(claimTTL time.Duration) time.Duration {
	period := claimTTL / 3
	if period <= 0 {
		return time.Millisecond
	}
	if period > autoDecomposeHeartbeatMaxPeriod {
		return autoDecomposeHeartbeatMaxPeriod
	}
	return period
}

func autoDecomposeClaimLost(err error) bool {
	return errors.Is(err, store.ErrAutoDecomposeClaimNotOwner) ||
		errors.Is(err, store.ErrAutoDecomposeClaimExpired) ||
		errors.Is(err, store.ErrAutoDecomposeTaskChanged)
}

func resetAutoDecomposeTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func extendAutoDecomposeDeadline(
	previousDeadline time.Time,
	previousExpiry time.Time,
	renewedExpiry time.Time,
	observed time.Time,
	monotonicNow time.Time,
) (time.Time, error) {
	if renewedExpiry.Before(previousExpiry) || !renewedExpiry.After(observed) {
		return time.Time{}, store.ErrAutoDecomposeClaimExpired
	}
	// The durable-expiry delta protects frozen/backward clocks. The wall-clock
	// bound protects forward jumps and time spent waiting for SQLite. Taking
	// the earlier monotonic boundary keeps both failure modes conservative.
	deltaBound := previousDeadline.Add(renewedExpiry.Sub(previousExpiry))
	wallBound := monotonicNow.Add(renewedExpiry.Sub(observed))
	if wallBound.Before(deltaBound) {
		return wallBound, nil
	}
	return deltaBound, nil
}

type autoDecomposeLeaseGuard struct {
	ctx             context.Context
	cancel          context.CancelFunc
	heartbeatCtx    context.Context
	cancelHeartbeat context.CancelFunc
	heartbeatDone   chan struct{}
	opened          *store.Store
	claimTTL        time.Duration
	options         Options

	claimMu sync.Mutex
	claim   store.AutoDecomposeClaim

	deadlineMu        sync.Mutex
	confirmedDeadline time.Time

	heartbeatStop sync.Once
	watchdogMu    sync.Mutex
	watchdog      *time.Timer
	stopOnce      sync.Once
}

// startAutoDecomposeLeaseGuard keeps one paid Planner invocation's durable
// ownership current. The capability token and original task version remain
// unchanged; losing either cancels the whole decomposition context.
func startAutoDecomposeLeaseGuard(
	ctx context.Context,
	opened *store.Store,
	claim store.AutoDecomposeClaim,
	claimTTL time.Duration,
	options Options,
) *autoDecomposeLeaseGuard {
	guardCtx, cancel := context.WithCancel(ctx)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(guardCtx)
	guard := &autoDecomposeLeaseGuard{
		ctx: guardCtx, cancel: cancel,
		heartbeatCtx: heartbeatCtx, cancelHeartbeat: cancelHeartbeat,
		heartbeatDone: make(chan struct{}), opened: opened,
		claim: claim, claimTTL: claimTTL, options: options,
	}
	go guard.runHeartbeat()
	return guard
}

func (g *autoDecomposeLeaseGuard) currentClaim() store.AutoDecomposeClaim {
	g.claimMu.Lock()
	defer g.claimMu.Unlock()
	return g.claim
}

func (g *autoDecomposeLeaseGuard) setClaim(claim store.AutoDecomposeClaim) {
	g.claimMu.Lock()
	defer g.claimMu.Unlock()
	g.claim = claim
}

func (g *autoDecomposeLeaseGuard) currentDeadline() time.Time {
	g.deadlineMu.Lock()
	defer g.deadlineMu.Unlock()
	return g.confirmedDeadline
}

func (g *autoDecomposeLeaseGuard) setDeadline(deadline time.Time) {
	g.deadlineMu.Lock()
	defer g.deadlineMu.Unlock()
	g.confirmedDeadline = deadline
}

func (g *autoDecomposeLeaseGuard) cancelLostClaim(err error) {
	claim := g.currentClaim()
	g.options.log(
		"auto-decompose claim lost %s; canceling Planner: %v",
		claim.TaskID, err,
	)
	g.cancel()
}

func (g *autoDecomposeLeaseGuard) runHeartbeat() {
	defer close(g.heartbeatDone)
	active := g.currentClaim()
	expiry, err := time.Parse(time.RFC3339Nano, active.ExpiresAt)
	monotonicNow := time.Now()
	current := g.options.currentTime()
	if err != nil || !expiry.After(current) {
		g.cancelLostClaim(fmt.Errorf("invalid live claim expiry %q", active.ExpiresAt))
		return
	}
	remaining := expiry.Sub(current)
	period := autoDecomposeHeartbeatPeriod(g.claimTTL)
	renewTimer := time.NewTimer(min(period, remaining/2))
	expiryTimer := time.NewTimer(remaining)
	defer renewTimer.Stop()
	defer expiryTimer.Stop()
	confirmedDeadline := monotonicNow.Add(remaining)
	g.setDeadline(confirmedDeadline)

	for {
		select {
		case <-g.heartbeatCtx.Done():
			return
		case <-expiryTimer.C:
			g.cancelLostClaim(store.ErrAutoDecomposeClaimExpired)
			return
		case <-renewTimer.C:
		}
		confirmedDeadline = g.currentDeadline()
		realRemaining := time.Until(confirmedDeadline)
		if realRemaining <= 0 {
			g.cancelLostClaim(store.ErrAutoDecomposeClaimExpired)
			return
		}
		current = g.options.currentTime()
		renewCtx, cancelRenew := context.WithTimeout(g.heartbeatCtx, realRemaining)
		renewed, renewErr := g.opened.RenewAutoDecomposeClaim(
			renewCtx, active, g.claimTTL, current,
		)
		renewCtxErr := renewCtx.Err()
		cancelRenew()
		if g.heartbeatCtx.Err() != nil {
			return
		}
		if renewErr == nil && renewCtxErr == nil && time.Now().Before(confirmedDeadline) {
			renewedExpiry, parseErr := time.Parse(time.RFC3339Nano, renewed.ExpiresAt)
			monotonicNow = time.Now()
			observed := g.options.currentTime()
			nextDeadline, deadlineErr := extendAutoDecomposeDeadline(
				confirmedDeadline, expiry, renewedExpiry, observed, monotonicNow,
			)
			if parseErr != nil || deadlineErr != nil {
				g.cancelLostClaim(fmt.Errorf(
					"invalid renewed claim expiry %q", renewed.ExpiresAt,
				))
				return
			}
			confirmedDeadline = nextDeadline
			active = renewed
			expiry = renewedExpiry
			g.setClaim(renewed)
			g.setDeadline(confirmedDeadline)
			remaining = time.Until(confirmedDeadline)
			if remaining <= 0 {
				g.cancelLostClaim(store.ErrAutoDecomposeClaimExpired)
				return
			}
			resetAutoDecomposeTimer(expiryTimer, remaining)
			resetAutoDecomposeTimer(renewTimer, min(period, remaining/2))
			continue
		}
		if renewErr == nil {
			renewErr = store.ErrAutoDecomposeClaimExpired
		}
		if autoDecomposeClaimLost(renewErr) ||
			time.Until(g.currentDeadline()) <= 0 {
			g.cancelLostClaim(renewErr)
			return
		}
		realRemaining = time.Until(g.currentDeadline())
		retryDelay := min(period, realRemaining/2)
		if retryDelay <= 0 {
			g.cancelLostClaim(store.ErrAutoDecomposeClaimExpired)
			return
		}
		g.options.log(
			"auto-decompose heartbeat failed %s; retrying before %s: %v",
			active.TaskID, active.ExpiresAt, renewErr,
		)
		resetAutoDecomposeTimer(renewTimer, retryDelay)
	}
}

func (g *autoDecomposeLeaseGuard) stopHeartbeat() {
	g.heartbeatStop.Do(func() {
		g.cancelHeartbeat()
		<-g.heartbeatDone
	})
}

// sealPlannerResult switches from active renewal to a fixed expiry watchdog.
// A final CAS renewal leaves enough lease for local decode, validation, graph
// mutation, and claim completion without mistaking those mutations for a user
// edit.
func (g *autoDecomposeLeaseGuard) sealPlannerResult() error {
	g.stopHeartbeat()
	if err := g.ctx.Err(); err != nil {
		return err
	}
	active := g.currentClaim()
	current := g.options.currentTime()
	previousExpiry, err := time.Parse(time.RFC3339Nano, active.ExpiresAt)
	if err != nil {
		g.cancel()
		return fmt.Errorf("parse final auto-decompose lease: %w", err)
	}
	confirmedDeadline := g.currentDeadline()
	remaining := time.Until(confirmedDeadline)
	if remaining <= 0 {
		g.cancel()
		return store.ErrAutoDecomposeClaimExpired
	}
	renewCtx, cancelRenew := context.WithTimeout(g.ctx, remaining)
	renewed, err := g.opened.RenewAutoDecomposeClaim(
		renewCtx, active, g.claimTTL, current,
	)
	renewCtxErr := renewCtx.Err()
	cancelRenew()
	if err != nil {
		g.cancel()
		return fmt.Errorf("seal auto-decompose Planner result: %w", err)
	}
	if renewCtxErr != nil {
		g.cancel()
		return fmt.Errorf("seal auto-decompose Planner result: %w", renewCtxErr)
	}
	expiry, err := time.Parse(time.RFC3339Nano, renewed.ExpiresAt)
	monotonicNow := time.Now()
	observed := g.options.currentTime()
	nextDeadline, deadlineErr := extendAutoDecomposeDeadline(
		confirmedDeadline, previousExpiry, expiry, observed, monotonicNow,
	)
	if err != nil || deadlineErr != nil {
		g.cancel()
		return fmt.Errorf("sealed auto-decompose claim has invalid expiry %q", renewed.ExpiresAt)
	}
	confirmedDeadline = nextDeadline
	remaining = time.Until(confirmedDeadline)
	if remaining <= 0 {
		g.cancel()
		return store.ErrAutoDecomposeClaimExpired
	}
	g.setClaim(renewed)
	g.setDeadline(confirmedDeadline)
	g.watchdogMu.Lock()
	g.watchdog = time.AfterFunc(remaining, func() {
		g.options.log(
			"auto-decompose claim expired %s before its result was persisted",
			renewed.TaskID,
		)
		g.cancel()
	})
	g.watchdogMu.Unlock()
	return nil
}

func (g *autoDecomposeLeaseGuard) planner(planner orchestration.Planner) orchestration.Planner {
	return func(ctx context.Context, request orchestration.PlannerRequest) (any, error) {
		value, err := planner(ctx, request)
		if err != nil {
			return nil, err
		}
		if err := g.sealPlannerResult(); err != nil {
			return nil, err
		}
		return value, nil
	}
}

func (g *autoDecomposeLeaseGuard) Stop() {
	g.stopOnce.Do(func() {
		g.stopHeartbeat()
		g.watchdogMu.Lock()
		if g.watchdog != nil {
			g.watchdog.Stop()
		}
		g.watchdogMu.Unlock()
		g.cancel()
	})
}

type planningPass struct {
	boards []string
	done   chan struct{}
}

// planningQueue keeps planner work off the dispatcher lifecycle loop.
// At most one pass runs and one later pass waits, so a hung planner cannot
// create an unbounded queue while maintenance and worker claims continue.
type planningQueue struct {
	ctx         context.Context
	manager     *boards.Manager
	options     Options
	diagnostics *autoDecomposeDiagnostics
	queue       chan planningPass
	done        chan struct{}
	mu          sync.Mutex
	stopped     bool
}

func startPlanningQueue(ctx context.Context, manager *boards.Manager, options Options) *planningQueue {
	queue := &planningQueue{
		ctx: ctx, manager: manager, options: options, diagnostics: &autoDecomposeDiagnostics{},
		queue: make(chan planningPass, 1), done: make(chan struct{}),
	}
	go queue.run()
	return queue
}

func uniqueBoardSlugs(boardSlugs []string) []string {
	result := make([]string, 0, len(boardSlugs))
	seen := make(map[string]struct{}, len(boardSlugs))
	for _, board := range boardSlugs {
		board = strings.TrimSpace(board)
		if board == "" {
			continue
		}
		if _, exists := seen[board]; exists {
			continue
		}
		seen[board] = struct{}{}
		result = append(result, board)
	}
	return result
}

func rotatedBoardSlugs(boardSlugs []string, next string) []string {
	boards := uniqueBoardSlugs(boardSlugs)
	if len(boards) < 2 || strings.TrimSpace(next) == "" {
		return boards
	}
	start := -1
	for index, board := range boards {
		if board == next {
			start = index
			break
		}
	}
	if start <= 0 {
		return boards
	}
	result := make([]string, 0, len(boards))
	result = append(result, boards[start:]...)
	result = append(result, boards[:start]...)
	return result
}

func boardAfter(boardSlugs []string, current string) string {
	if len(boardSlugs) == 0 {
		return ""
	}
	for index, board := range boardSlugs {
		if board == current {
			return boardSlugs[(index+1)%len(boardSlugs)]
		}
	}
	return boardSlugs[0]
}

func (d *autoDecomposeDiagnostics) orderedPlanningBoards(boardSlugs []string) []string {
	next := ""
	if d != nil {
		next = d.nextPlanningBoard
	}
	boards := rotatedBoardSlugs(boardSlugs, next)
	if d != nil && len(boards) > 0 {
		// Advance even when every board is idle so no board permanently owns
		// the first (and therefore cheapest) probe in subsequent passes.
		d.nextPlanningBoard = boardAfter(boards, boards[0])
	}
	return boards
}

func (d *autoDecomposeDiagnostics) advancePlanningBoard(boardSlugs []string, board string) {
	if d != nil {
		d.nextPlanningBoard = boardAfter(boardSlugs, board)
	}
}

func (d *autoDecomposeDiagnostics) triageCursor(board string) *store.TaskListCursor {
	if d == nil || d.triageCursors == nil {
		return nil
	}
	cursor, found := d.triageCursors[board]
	if !found {
		return nil
	}
	return &cursor
}

func (d *autoDecomposeDiagnostics) setTriageCursor(board string, cursor *store.TaskListCursor) {
	if d == nil {
		return
	}
	if cursor == nil {
		delete(d.triageCursors, board)
		return
	}
	if d.triageCursors == nil {
		d.triageCursors = make(map[string]store.TaskListCursor)
	}
	d.triageCursors[board] = *cursor
}

// Enqueue coalesces repeated lifecycle ticks into a single pending pass. A nil
// result means an equivalent later pass is already queued or shutdown began.
func (p *planningQueue) Enqueue(boardSlugs []string) <-chan struct{} {
	boards := uniqueBoardSlugs(boardSlugs)
	if len(boards) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.ctx.Err() != nil {
		return nil
	}
	request := planningPass{boards: boards, done: make(chan struct{})}
	select {
	case p.queue <- request:
		return request.done
	default:
		return nil
	}
}

func (p *planningQueue) run() {
	defer close(p.done)
	defer p.stopAndDrain()
	for {
		select {
		case <-p.ctx.Done():
			return
		case request := <-p.queue:
			decomposeBoardTriage(p.ctx, p.manager, request.boards, p.options, p.diagnostics)
			close(request.done)
		}
	}
}

func (p *planningQueue) stopAndDrain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	for {
		select {
		case request := <-p.queue:
			close(request.done)
		default:
			return
		}
	}
}

func (p *planningQueue) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}
