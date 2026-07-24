package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

var errManagedRunLeaseLost = errors.New("managed run lease lost")

const (
	managedRunHeartbeatMin = 100 * time.Millisecond
	managedRunHeartbeatMax = 30 * time.Second
)

// managedRunLeaseGuard keeps the dispatcher-owned claim alive across every
// host phase, including profile selection, workspace preparation, Git
// integration, gaps between goal turns, and terminal reconciliation. Worker
// heartbeats may run at the same time; both renew the same scoped claim.
type managedRunLeaseGuard struct {
	ctx             context.Context
	cancel          context.CancelCauseFunc
	leaseCtx        context.Context
	cancelLease     context.CancelCauseFunc
	heartbeatCtx    context.Context
	cancelHeartbeat context.CancelFunc
	done            chan struct{}
	opened          *store.Store
	scope           store.RunScope
	log             func(string, ...any)

	mu            sync.Mutex
	deadline      time.Time
	err           error
	recoveryFence bool
	stopOnce      sync.Once

	quiescenceOnce sync.Once
	quiescenceErr  error
	fenceErr       error
}

func managedRunHeartbeatPeriod(ttl time.Duration) time.Duration {
	period := ttl / 3
	if period < managedRunHeartbeatMin {
		return managedRunHeartbeatMin
	}
	if period > managedRunHeartbeatMax {
		return managedRunHeartbeatMax
	}
	return period
}

func managedRunLeaseTiming(run model.Run) (time.Duration, time.Duration, error) {
	heartbeat, heartbeatErr := time.Parse(time.RFC3339Nano, run.HeartbeatAt)
	expiry, expiryErr := time.Parse(time.RFC3339Nano, run.ClaimExpiresAt)
	if heartbeatErr != nil || expiryErr != nil {
		return 0, 0, fmt.Errorf(
			"parse managed run lease timestamps: %w",
			errors.Join(heartbeatErr, expiryErr),
		)
	}
	ttl := expiry.Sub(heartbeat)
	remaining := time.Until(expiry)
	if ttl <= 0 || remaining <= 0 {
		return 0, 0, fmt.Errorf(
			"%w: invalid expiry %q for heartbeat %q",
			errManagedRunLeaseLost,
			run.ClaimExpiresAt,
			run.HeartbeatAt,
		)
	}
	return ttl, remaining, nil
}

func startManagedRunLeaseGuard(
	parent context.Context,
	opened *store.Store,
	scope store.RunScope,
	log func(string, ...any),
) (*managedRunLeaseGuard, error) {
	// Renew synchronously before any host-owned setup. MarkRunManaged and this
	// renewal form the ownership handoff from claim acquisition to runClaim.
	renewed, err := opened.RenewManagedRunLease(parent, scope)
	if err != nil {
		return nil, fmt.Errorf("establish managed run lease: %w", err)
	}
	ttl, remaining, err := managedRunLeaseTiming(renewed)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancelCause(parent)
	// Lease renewal and terminal persistence outlive dispatcher cancellation
	// for the bounded duration of runClaim's durable cleanup. Only explicit
	// Stop or confirmed lease loss ends this background ownership.
	leaseCtx, cancelLease := context.WithCancelCause(context.Background())
	heartbeatCtx, cancelHeartbeat := context.WithCancel(leaseCtx)
	guard := &managedRunLeaseGuard{
		ctx: runCtx, cancel: cancel,
		leaseCtx: leaseCtx, cancelLease: cancelLease,
		heartbeatCtx: heartbeatCtx, cancelHeartbeat: cancelHeartbeat,
		done: make(chan struct{}), opened: opened, scope: scope, log: log,
		deadline: time.Now().Add(remaining),
	}
	guard.ctx = processguard.WithTeardownFailureReporter(
		guard.ctx,
		guard.reportTeardownUnconfirmed,
	)
	guard.leaseCtx = processguard.WithTeardownFailureReporter(
		guard.leaseCtx,
		guard.reportTeardownUnconfirmed,
	)
	guard.heartbeatCtx = processguard.WithTeardownFailureReporter(
		guard.heartbeatCtx,
		guard.reportTeardownUnconfirmed,
	)
	go guard.heartbeat(ttl)
	return guard, nil
}

func (g *managedRunLeaseGuard) reportTeardownUnconfirmed(err error) {
	if !errors.Is(err, processguard.ErrTeardownUnconfirmed) {
		return
	}
	g.quiescenceOnce.Do(func() {
		cause := fmt.Errorf(
			"managed run process teardown is unconfirmed: %w",
			err,
		)
		g.mu.Lock()
		g.quiescenceErr = cause
		g.mu.Unlock()
		// Cancel both contexts before attempting persistence so no caller can
		// continue into FailRun/Finalize while a guarded descendant may still
		// mutate the workspace.
		g.lose(cause)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		fenceErr := g.ensureOperatorFence(
			ctx,
			"Process teardown could not be proven; inspect the worker and host processes before recovery",
			"process_teardown_unconfirmed",
		)
		cancel()
		g.mu.Lock()
		g.fenceErr = fenceErr
		g.mu.Unlock()
	})
}

func (g *managedRunLeaseGuard) quiescenceFailure() (error, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.quiescenceErr, g.fenceErr
}

func (g *managedRunLeaseGuard) ensureOperatorFence(
	ctx context.Context,
	reason string,
	diagnosticCode string,
) error {
	for attempt := 0; attempt < 3; attempt++ {
		inspection, err := g.opened.GetRun(ctx, g.scope.RunID)
		if err != nil {
			return err
		}
		if inspection.Run.Status != model.RunStatusRunning ||
			inspection.Task.Status != model.TaskStatusRunning ||
			inspection.Task.CurrentRunID == nil ||
			*inspection.Task.CurrentRunID != inspection.Run.ID {
			return fmt.Errorf(
				"cannot install operator recovery fence on terminal run %s",
				g.scope.RunID,
			)
		}
		identity, err := g.opened.GetRunProcessIdentity(ctx, g.scope.RunID)
		if err != nil {
			return err
		}
		reclaim, err := g.opened.GetDeferredReclaim(ctx, g.scope.RunID)
		if err != nil {
			return err
		}
		if reclaim != nil && reclaim.RequiresOperator {
			return nil
		}
		observation := store.ObserveRunForRecovery(
			inspection.Run,
			identity,
			reclaim,
		)
		outcome := model.RunStatusReclaimed
		countFailure := false
		if reclaim != nil {
			outcome = reclaim.Outcome
			countFailure = reclaim.CountFailure
		}
		_, err = g.opened.RequireObservedRunRecoveryInterventionWithDiagnostic(
			ctx,
			observation,
			15,
			reason,
			outcome,
			countFailure,
			diagnosticCode,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrRunRecoveryObservationChanged) {
			return err
		}
	}
	return fmt.Errorf(
		"%w: run %s changed while installing operator recovery fence",
		store.ErrRunRecoveryObservationChanged,
		g.scope.RunID,
	)
}

func (g *managedRunLeaseGuard) currentDeadline() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deadline
}

func (g *managedRunLeaseGuard) setDeadline(value time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deadline = value
}

func (g *managedRunLeaseGuard) lose(err error) {
	if err == nil {
		err = errManagedRunLeaseLost
	}
	wrapped := fmt.Errorf("%w: %v", errManagedRunLeaseLost, err)
	g.mu.Lock()
	if g.err == nil {
		g.err = wrapped
	}
	g.mu.Unlock()
	g.cancel(wrapped)
	g.cancelLease(wrapped)
}

func (g *managedRunLeaseGuard) loseToRecoveryFence(err error) {
	g.mu.Lock()
	g.recoveryFence = true
	g.mu.Unlock()
	g.lose(err)
}

func (g *managedRunLeaseGuard) heartbeat(initialTTL time.Duration) {
	defer close(g.done)
	period := managedRunHeartbeatPeriod(initialTTL)
	timer := time.NewTimer(min(period, time.Until(g.currentDeadline())/2))
	defer timer.Stop()
	for {
		select {
		case <-g.heartbeatCtx.Done():
			return
		case <-timer.C:
		}

		remaining := time.Until(g.currentDeadline())
		if remaining <= 0 {
			g.lose(errors.New("confirmed lease deadline elapsed"))
			return
		}
		timeout := min(5*time.Second, remaining/2)
		if timeout < managedRunHeartbeatMin {
			timeout = remaining
		}
		renewCtx, cancel := context.WithTimeout(g.heartbeatCtx, timeout)
		renewed, err := g.opened.RenewManagedRunLease(renewCtx, g.scope)
		renewCtxErr := renewCtx.Err()
		cancel()
		if g.heartbeatCtx.Err() != nil {
			return
		}
		if err == nil && renewCtxErr == nil {
			ttl, nextRemaining, timingErr := managedRunLeaseTiming(renewed)
			if timingErr != nil {
				g.lose(timingErr)
				return
			}
			g.setDeadline(time.Now().Add(nextRemaining))
			period = managedRunHeartbeatPeriod(ttl)
			resetManagedRunLeaseTimer(
				timer,
				min(period, time.Until(g.currentDeadline())/2),
			)
			continue
		}
		if errors.Is(err, store.ErrRunTerminationPending) {
			// A Supervisor won the durable recovery fence. Cancel the host
			// immediately instead of retrying until the old lease deadline.
			g.loseToRecoveryFence(err)
			return
		}

		// A terminal run is the normal end of this guard. For any other
		// failure, retain the last confirmed monotonic deadline and retry.
		terminalCtx, terminalCancel := context.WithTimeout(
			g.heartbeatCtx,
			min(time.Second, max(managedRunHeartbeatMin, remaining/2)),
		)
		terminal, terminalErr := g.opened.IsRunTerminal(
			terminalCtx,
			g.scope.RunID,
		)
		terminalCancel()
		if terminalErr == nil && terminal {
			g.lose(errors.New("run became terminal while the host lease guard was active"))
			return
		}
		remaining = time.Until(g.currentDeadline())
		if remaining <= 0 {
			g.lose(errors.Join(err, renewCtxErr, terminalErr))
			return
		}
		if g.log != nil {
			g.log(
				"managed run lease renewal failed for %s; retrying before the confirmed deadline: %v",
				g.scope.RunID,
				errors.Join(err, renewCtxErr, terminalErr),
			)
		}
		resetManagedRunLeaseTimer(
			timer,
			min(managedRunHeartbeatMin, remaining/2),
		)
	}
}

func resetManagedRunLeaseTimer(timer *time.Timer, delay time.Duration) {
	if delay <= 0 {
		delay = time.Millisecond
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func (g *managedRunLeaseGuard) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *managedRunLeaseGuard) Check() error {
	if err := g.Err(); err != nil {
		return err
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (g *managedRunLeaseGuard) DurableContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(g.leaseCtx, 15*time.Second)
}

func (g *managedRunLeaseGuard) Stop() error {
	var ackErr error
	g.stopOnce.Do(func() {
		g.cancelHeartbeat()
		<-g.done
		// A foreground claim-scoped mutation can observe the fence before the
		// heartbeat does. Read the immutable fence directly at the quiescent
		// runClaim boundary instead of relying on the heartbeat flag.
		lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		reclaim, lookupErr := g.opened.GetDeferredReclaim(
			lookupCtx,
			g.scope.RunID,
		)
		lookupCancel()
		quiescenceErr, priorFenceErr := g.quiescenceFailure()
		if lookupErr != nil {
			ackErr = lookupErr
		} else if quiescenceErr != nil ||
			(reclaim != nil && !processguard.TeardownProofAvailable()) {
			reason := "This platform cannot prove that all managed host processes stopped; inspect them before recovery"
			diagnosticCode := "process_teardown_proof_unavailable"
			if quiescenceErr != nil {
				reason = "Process teardown could not be proven; inspect the worker and host processes before recovery"
				diagnosticCode = "process_teardown_unconfirmed"
			}
			fenceCtx, fenceCancel := context.WithTimeout(
				context.Background(),
				5*time.Second,
			)
			ackErr = errors.Join(
				priorFenceErr,
				g.ensureOperatorFence(fenceCtx, reason, diagnosticCode),
			)
			fenceCancel()
		} else if reclaim != nil && reclaim.HostAcknowledgedAt == nil {
			// Stop runs only after runClaim has unwound every worker and
			// host-owned Git operation, so this is the quiescence proof the
			// Supervisor waits for before inspecting the worktree.
			ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, ackErr = g.opened.AcknowledgeRunRecoveryFence(
				ackCtx,
				g.scope,
				reclaim.FenceToken,
				reclaim.FenceGeneration,
			)
			ackCancel()
		}
		g.cancel(context.Canceled)
		g.cancelLease(context.Canceled)
	})
	err := g.Err()
	quiescenceErr, fenceErr := g.quiescenceFailure()
	err = errors.Join(err, quiescenceErr, fenceErr)
	if ackErr != nil {
		err = errors.Join(err, fmt.Errorf(
			"acknowledge recovery fence for %s: %w",
			g.scope.RunID,
			ackErr,
		))
	}
	if err == nil {
		return nil
	}
	safetyErr := errors.Join(ackErr, quiescenceErr, fenceErr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	terminal, terminalErr := g.opened.IsRunTerminal(ctx, g.scope.RunID)
	if safetyErr == nil && terminalErr == nil && terminal {
		// Normal finalization can race the last heartbeat. Any unexpected
		// terminalization already canceled the guarded host context.
		return nil
	}
	return errors.Join(err, terminalErr)
}
