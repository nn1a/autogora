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
	store  *store.Store
	owner  string
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func startSupervisorLease(ctx context.Context, cancelDispatcher context.CancelFunc, manager *boards.Manager) (*supervisorLease, error) {
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		return nil, err
	}
	owner := fmt.Sprintf("pid-%d-%s", os.Getpid(), uuid.NewString())
	current, acquired, err := opened.AcquireServiceLease(ctx, dispatcherLeaseName, owner, dispatcherLeaseTTL, time.Now())
	if err != nil {
		opened.Close()
		return nil, err
	}
	if !acquired {
		opened.Close()
		return nil, fmt.Errorf("%w: owner=%s expires=%s", ErrDispatcherAlreadyRunning, current.Owner, current.ExpiresAt)
	}
	keepalive, cancel := context.WithCancel(ctx)
	lease := &supervisorLease{store: opened, owner: owner, cancel: cancel, done: make(chan struct{})}
	go lease.renew(keepalive, cancelDispatcher)
	return lease, nil
}

func (l *supervisorLease) renew(ctx context.Context, cancelDispatcher context.CancelFunc) {
	defer close(l.done)
	ticker := time.NewTicker(dispatcherLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case current := <-ticker.C:
			if _, err := l.store.RenewServiceLease(ctx, dispatcherLeaseName, l.owner, dispatcherLeaseTTL, current); err != nil {
				l.mu.Lock()
				l.err = fmt.Errorf("dispatcher supervisor lease lost: %w", err)
				l.mu.Unlock()
				cancelDispatcher()
				return
			}
		}
	}
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
