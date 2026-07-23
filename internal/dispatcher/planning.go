package dispatcher

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/store"
)

const (
	autoDecomposeDiagnosticEntries  = 2048
	autoDecomposeCandidatePageSize  = 100
	autoDecomposeCandidateScanLimit = 1000
	autoDecomposeClaimGrace         = 30 * time.Second
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
