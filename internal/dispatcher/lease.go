package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/store"
)

const (
	dispatcherLeaseName = "dispatcher-supervisor"
	dispatcherLeaseTTL  = 30 * time.Second
)

var ErrDispatcherAlreadyRunning = errors.New("another dispatcher supervisor is already running")

type supervisorLease struct {
	store    *store.Store
	owner    string
	cancel   context.CancelFunc
	done     chan struct{}
	deadline time.Time
	mu       sync.Mutex
	err      error
}

func startSupervisorLease(ctx context.Context, cancelDispatcher context.CancelFunc, manager *boards.Manager) (*supervisorLease, error) {
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		return nil, err
	}
	owner := fmt.Sprintf("pid-%d-%s", os.Getpid(), uuid.NewString())
	started := time.Now()
	current, acquired, err := opened.AcquireServiceLease(
		ctx,
		dispatcherLeaseName,
		owner,
		dispatcherLeaseTTL,
		started,
	)
	if err != nil {
		opened.Close()
		return nil, err
	}
	if !acquired {
		opened.Close()
		return nil, fmt.Errorf("%w: owner=%s expires=%s", ErrDispatcherAlreadyRunning, current.Owner, current.ExpiresAt)
	}
	deadline, err := serviceLeaseMonotonicDeadline(started, current)
	if err != nil || !time.Now().Before(deadline) {
		_ = opened.ReleaseServiceLease(ctx, dispatcherLeaseName, owner)
		opened.Close()
		if err != nil {
			return nil, fmt.Errorf("dispatcher supervisor lease deadline: %w", err)
		}
		return nil, errors.New("dispatcher supervisor lease expired during acquisition")
	}
	keepalive, cancel := context.WithCancel(ctx)
	lease := &supervisorLease{
		store: opened, owner: owner, cancel: cancel, done: make(chan struct{}),
		deadline: deadline,
	}
	go lease.renew(keepalive, cancelDispatcher)
	return lease, nil
}

func (l *supervisorLease) renew(ctx context.Context, cancelDispatcher context.CancelFunc) {
	defer close(l.done)
	ticker := time.NewTicker(dispatcherLeaseTTL / 3)
	defer ticker.Stop()
	deadlineTimer := time.NewTimer(time.Until(l.deadline))
	defer deadlineTimer.Stop()
	lose := func(err error) {
		err = supervisorLeaseRenewalError(ctx, err)
		if err == nil {
			return
		}
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
		cancelDispatcher()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadlineTimer.C:
			lose(errors.New("dispatcher supervisor lease reached its local deadline"))
			return
		case <-ticker.C:
			// ticker.C is the scheduled tick time, not the time at which this
			// delayed goroutine actually enters the renewal transaction.
			started := time.Now()
			renewalCtx, cancel := context.WithDeadline(ctx, l.deadline)
			renewed, err := l.store.RenewServiceLease(
				renewalCtx,
				dispatcherLeaseName,
				l.owner,
				dispatcherLeaseTTL,
				started,
			)
			cancel()
			if err != nil {
				lose(err)
				return
			}
			nextDeadline, err := serviceLeaseMonotonicDeadline(started, renewed)
			if err != nil || !time.Now().Before(nextDeadline) {
				if err == nil {
					err = errors.New("dispatcher supervisor lease expired during renewal")
				}
				lose(err)
				return
			}
			l.deadline = nextDeadline
			if !deadlineTimer.Stop() {
				select {
				case <-deadlineTimer.C:
				default:
				}
			}
			deadlineTimer.Reset(time.Until(nextDeadline))
		}
	}
}

func serviceLeaseMonotonicDeadline(
	started time.Time,
	lease store.ServiceLease,
) (time.Time, error) {
	expires, err := time.Parse(time.RFC3339Nano, lease.ExpiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse lease expiry: %w", err)
	}
	remaining := expires.Sub(started)
	if remaining <= 0 {
		return time.Time{}, errors.New("service lease expiry is not after renewal start")
	}
	// Add the persisted wall-clock delta to the local start value so deadline
	// comparisons retain Go's monotonic clock component.
	return started.Add(remaining), nil
}

func supervisorLeaseRenewalError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("dispatcher supervisor lease lost: %w", err)
}

func (l *supervisorLease) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *supervisorLease) Close() {
	if l == nil {
		return
	}
	l.cancel()
	<-l.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.store.ReleaseServiceLease(ctx, dispatcherLeaseName, l.owner)
	_ = l.store.Close()
}
