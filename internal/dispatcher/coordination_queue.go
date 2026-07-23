package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/boards"
)

type coordinationPass struct {
	boards []string
	done   chan struct{}
}

// coordinationQueue keeps exceptional graph analysis out of the worker
// lifecycle loop. Like planningQueue, it coalesces repeated ticks so a slow
// external Coordinator cannot accumulate unbounded work.
type coordinationQueue struct {
	ctx     context.Context
	manager *boards.Manager
	options Options
	queue   chan coordinationPass
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
	state   coordinationRuntimeState
}

type coordinationRuntimeState struct {
	nextBoard string
}

func startCoordinationQueue(
	ctx context.Context,
	manager *boards.Manager,
	options Options,
) *coordinationQueue {
	queue := &coordinationQueue{
		ctx: ctx, manager: manager, options: options,
		queue: make(chan coordinationPass, 1), done: make(chan struct{}),
	}
	go queue.run()
	return queue
}

// Enqueue coalesces repeated lifecycle ticks into one pending coordination
// pass. A nil channel means a pass is already pending or shutdown has begun.
func (q *coordinationQueue) Enqueue(boardSlugs []string) <-chan struct{} {
	boardSlugs = uniqueBoardSlugs(boardSlugs)
	if len(boardSlugs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped || q.ctx.Err() != nil {
		return nil
	}
	request := coordinationPass{boards: boardSlugs, done: make(chan struct{})}
	select {
	case q.queue <- request:
		return request.done
	default:
		return nil
	}
}

func (q *coordinationQueue) run() {
	defer close(q.done)
	defer q.stopAndDrain()
	for {
		select {
		case <-q.ctx.Done():
			return
		case request := <-q.queue:
			if err := runCoordinationPass(
				q.ctx, q.manager, request.boards, q.options, &q.state, q.options.currentTime(),
			); err != nil {
				q.options.log("coordination pass failed: %v", err)
			}
			close(request.done)
		}
	}
}

func (q *coordinationQueue) stopAndDrain() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
	for {
		select {
		case request := <-q.queue:
			close(request.done)
		default:
			return
		}
	}
}

func (q *coordinationQueue) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-q.done:
		return true
	case <-timer.C:
		return false
	}
}
