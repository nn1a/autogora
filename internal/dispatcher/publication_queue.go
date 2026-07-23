package dispatcher

import (
	"context"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/boards"
)

type publicationPass struct {
	boards []string
	done   chan struct{}
}

// publicationQueue keeps host-side Git operations out of the worker lifecycle
// loop. At most one pass runs and one later pass waits, so repeated watch ticks
// cannot build an unbounded publication backlog.
type publicationQueue struct {
	ctx     context.Context
	manager *boards.Manager
	options Options
	queue   chan publicationPass
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
	state   publicationRuntimeState
}

func startPublicationQueue(
	ctx context.Context,
	manager *boards.Manager,
	options Options,
) *publicationQueue {
	queue := &publicationQueue{
		ctx: ctx, manager: manager, options: options,
		queue: make(chan publicationPass, 1), done: make(chan struct{}),
	}
	go queue.run()
	return queue
}

// Enqueue coalesces repeated lifecycle ticks into one pending publication
// pass. A nil result means a later pass is already queued or shutdown began.
func (q *publicationQueue) Enqueue(boardSlugs []string) <-chan struct{} {
	boardSlugs = uniqueBoardSlugs(boardSlugs)
	if len(boardSlugs) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped || q.ctx.Err() != nil {
		return nil
	}
	request := publicationPass{boards: boardSlugs, done: make(chan struct{})}
	select {
	case q.queue <- request:
		return request.done
	default:
		return nil
	}
}

func (q *publicationQueue) run() {
	defer close(q.done)
	defer q.stopAndDrain()
	for {
		select {
		case <-q.ctx.Done():
			return
		case request := <-q.queue:
			if err := runPublicationPass(
				q.ctx, q.manager, request.boards, q.options, &q.state,
				q.options.currentTime(),
			); err != nil {
				q.options.log("publication pass failed: %v", err)
			}
			close(request.done)
		}
	}
}

func (q *publicationQueue) stopAndDrain() {
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

func (q *publicationQueue) Wait(timeout time.Duration) bool {
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
