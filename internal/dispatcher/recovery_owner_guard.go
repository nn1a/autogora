package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/store"
)

const recoveryOwnershipTTL = 30 * time.Second

type recoveryOwnershipGuard struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	stop   context.CancelFunc
	done   chan struct{}

	opened      *store.Store
	observation store.RunRecoveryObservation
	ttl         time.Duration
	log         func(string, ...any)

	mu       sync.Mutex
	deadline time.Time
	err      error
}

func startRecoveryOwnershipGuard(
	parent context.Context,
	opened *store.Store,
	observation store.RunRecoveryObservation,
	log func(string, ...any),
) (*recoveryOwnershipGuard, error) {
	return startRecoveryOwnershipGuardWithTTL(
		parent,
		opened,
		observation,
		recoveryOwnershipTTL,
		log,
	)
}

func startRecoveryOwnershipGuardWithTTL(
	parent context.Context,
	opened *store.Store,
	observation store.RunRecoveryObservation,
	ttl time.Duration,
	log func(string, ...any),
) (*recoveryOwnershipGuard, error) {
	if ttl <= 0 {
		return nil, errors.New("recovery owner guard TTL must be positive")
	}
	if observation.ObservedRecoveryOwnerExpiresAt == nil {
		return nil, errors.New("recovery owner expiry is missing")
	}
	expires, err := time.Parse(
		time.RFC3339Nano,
		*observation.ObservedRecoveryOwnerExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("parse recovery owner expiry: %w", err)
	}
	if !expires.After(time.Now()) {
		return nil, store.ErrRunRecoveryOwned
	}
	ctx, cancel := context.WithCancelCause(parent)
	heartbeatCtx, stop := context.WithCancel(context.Background())
	guard := &recoveryOwnershipGuard{
		ctx:         ctx,
		cancel:      cancel,
		stop:        stop,
		done:        make(chan struct{}),
		opened:      opened,
		observation: observation,
		ttl:         ttl,
		log:         log,
		deadline:    expires,
	}
	go guard.heartbeat(heartbeatCtx)
	return guard, nil
}

func (g *recoveryOwnershipGuard) currentDeadline() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deadline
}

func (g *recoveryOwnershipGuard) currentObservation() store.RunRecoveryObservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observation
}

func (g *recoveryOwnershipGuard) setLease(
	observation store.RunRecoveryObservation,
	deadline time.Time,
) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.observation = observation
	g.deadline = deadline
}

func (g *recoveryOwnershipGuard) lose(err error) {
	if err == nil {
		err = store.ErrRunRecoveryOwned
	}
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
	g.cancel(err)
}

func (g *recoveryOwnershipGuard) heartbeat(ctx context.Context) {
	defer close(g.done)
	timer := time.NewTimer(g.ttl / 3)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		remaining := time.Until(g.currentDeadline())
		if remaining <= 0 {
			g.lose(errors.New("recovery owner lease deadline elapsed"))
			return
		}
		renewCtx, cancel := context.WithTimeout(
			ctx,
			min(5*time.Second, remaining/2),
		)
		renewed, err := g.opened.RenewObservedRunRecoveryOwnership(
			renewCtx,
			g.currentObservation(),
			g.ttl,
		)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil && renewed.ObservedRecoveryOwnerExpiresAt != nil {
			expires, parseErr := time.Parse(
				time.RFC3339Nano,
				*renewed.ObservedRecoveryOwnerExpiresAt,
			)
			if parseErr != nil || !expires.After(time.Now()) {
				g.lose(errors.Join(parseErr, store.ErrRunRecoveryOwned))
				return
			}
			g.setLease(renewed, expires)
			timer.Reset(g.ttl / 3)
			continue
		}
		if errors.Is(err, store.ErrRunRecoveryOwned) ||
			errors.Is(err, store.ErrRunRecoveryObservationChanged) {
			g.lose(err)
			return
		}
		remaining = time.Until(g.currentDeadline())
		if remaining <= 0 {
			g.lose(err)
			return
		}
		if g.log != nil {
			g.log(
				"recovery owner renewal failed for %s; retrying before deadline: %v",
				g.currentObservation().RunID,
				err,
			)
		}
		timer.Reset(min(100*time.Millisecond, remaining/2))
	}
}

func (g *recoveryOwnershipGuard) Stop() error {
	g.stop()
	<-g.done
	g.cancel(context.Canceled)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}
