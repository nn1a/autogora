package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

const (
	globalAutomationSessionBoard    = "*"
	automationDispatcherSessionTTL  = 30 * time.Second
	automationGateMonitorInterval   = 250 * time.Millisecond
	automationSessionOperationLimit = 5 * time.Second

	automationSessionSourceKind       = "dispatcher_session_uncertainty"
	automationTeardownDiagnostic      = "process_teardown_unconfirmed"
	automationShutdownDiagnostic      = "dispatcher_shutdown_unconfirmed"
	automationStartBoundaryDiagnostic = "worker_start_boundary_unconfirmed"
	automaticWorkerStartBlockedReason = "Automatic worker start authorization could not be verified"
)

var errAutomaticWorkerStartBlocked = errors.New(
	"automatic worker start authorization failed",
)

type automationSessionAuthority interface {
	RegisterAutomationDispatcherSession(
		context.Context,
		string,
		string,
		time.Duration,
	) (store.AutomationDispatcherSessionLease, bool, error)
	RenewAutomationDispatcherSession(
		context.Context,
		store.AutomationDispatcherSessionLease,
		time.Duration,
	) (store.AutomationDispatcherSessionLease, error)
	ReleaseAutomationDispatcherSession(
		context.Context,
		store.AutomationDispatcherSessionLease,
	) (bool, error)
	AcknowledgeAutomationQuarantine(
		context.Context,
		store.AutomationDispatcherSessionLease,
		int64,
	) error
	GetAutomationQuarantine(
		context.Context,
	) (store.AutomationQuarantine, error)
	ActivateAutomationQuarantine(
		context.Context,
		store.AutomationQuarantineSourceInput,
	) (store.AutomationQuarantine, bool, error)
}

type automationDispatcherSession struct {
	authority        automationSessionAuthority
	closeAuthority   func() error
	cancelDispatcher context.CancelFunc
	ttl              time.Duration
	renewInterval    time.Duration
	monitorInterval  time.Duration

	leaseMu sync.RWMutex
	lease   store.AutomationDispatcherSessionLease

	errMu sync.Mutex
	err   error

	generation  atomic.Int64
	unconfirmed atomic.Bool
	sourceSaved atomic.Bool

	uncertaintyMu         sync.Mutex
	uncertaintyDiagnostic string

	monitorCancel context.CancelFunc
	monitorDone   chan struct{}

	shutdownOnce sync.Once
	shutdownErr  error
}

func automationDispatcherSessionID() string {
	return fmt.Sprintf("dispatcher-%d-%s", os.Getpid(), uuid.NewString())
}

func startAutomationDispatcherSession(
	ctx context.Context,
	manager *boards.Manager,
	cancelDispatcher context.CancelFunc,
) (*automationDispatcherSession, error) {
	authority, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"open automation session authority: %w",
			err,
		)
	}
	session, err := startAutomationDispatcherSessionWithAuthority(
		ctx,
		authority,
		authority.Close,
		cancelDispatcher,
		automationDispatcherSessionID(),
		automationDispatcherSessionTTL,
		automationDispatcherSessionTTL/3,
		automationGateMonitorInterval,
	)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func startAutomationDispatcherSessionWithAuthority(
	ctx context.Context,
	authority automationSessionAuthority,
	closeAuthority func() error,
	cancelDispatcher context.CancelFunc,
	sessionID string,
	ttl time.Duration,
	renewInterval time.Duration,
	monitorInterval time.Duration,
) (*automationDispatcherSession, error) {
	if authority == nil {
		return nil, errors.New("automation session authority is required")
	}
	if cancelDispatcher == nil {
		return nil, errors.New("automation session cancellation is required")
	}
	if closeAuthority == nil {
		closeAuthority = func() error { return nil }
	}
	if renewInterval <= 0 {
		renewInterval = ttl / 3
	}
	if renewInterval <= 0 {
		renewInterval = time.Second
	}
	if monitorInterval <= 0 {
		monitorInterval = automationGateMonitorInterval
	}
	lease, acquired, err := authority.RegisterAutomationDispatcherSession(
		ctx,
		globalAutomationSessionBoard,
		sessionID,
		ttl,
	)
	if err != nil {
		return nil, errors.Join(err, closeAuthority())
	}
	if !acquired {
		return nil, errors.Join(
			store.ErrAutomationHostNotIdle,
			closeAuthority(),
		)
	}
	monitorContext, monitorCancel := context.WithCancel(context.Background())
	session := &automationDispatcherSession{
		authority:        authority,
		closeAuthority:   closeAuthority,
		cancelDispatcher: cancelDispatcher,
		ttl:              ttl,
		renewInterval:    renewInterval,
		monitorInterval:  monitorInterval,
		lease:            lease,
		monitorCancel:    monitorCancel,
		monitorDone:      make(chan struct{}),
	}
	go session.monitor(monitorContext)
	return session, nil
}

func (s *automationDispatcherSession) currentLease() store.AutomationDispatcherSessionLease {
	s.leaseMu.RLock()
	defer s.leaseMu.RUnlock()
	return s.lease
}

func (s *automationDispatcherSession) replaceLease(
	lease store.AutomationDispatcherSessionLease,
) {
	s.leaseMu.Lock()
	s.lease = lease
	s.leaseMu.Unlock()
}

func (s *automationDispatcherSession) recordFailure(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
	s.cancelDispatcher()
}

func (s *automationDispatcherSession) quarantineObservation() string {
	lease := s.currentLease()
	for _, value := range []string{
		lease.RegisteredAt,
		lease.RenewedAt,
		lease.ExpiresAt,
	} {
		if value != "" {
			return value
		}
	}
	return "dispatcher-session-registered"
}

// activateUnconfirmedQuarantine persists a process-lifetime source before
// local recovery or shutdown can make the uncertain helper invisible. The
// latch is irreversible for this session and prevents a false ACK.
func (s *automationDispatcherSession) activateUnconfirmedQuarantine(
	diagnostic string,
) error {
	if s == nil {
		return errors.New("automation dispatcher session is not running")
	}
	s.unconfirmed.Store(true)
	s.uncertaintyMu.Lock()
	if s.uncertaintyDiagnostic == "" {
		s.uncertaintyDiagnostic = diagnostic
	}
	diagnostic = s.uncertaintyDiagnostic
	s.uncertaintyMu.Unlock()
	lease := s.currentLease()
	operationContext, cancel := context.WithTimeout(
		context.Background(),
		automationSessionOperationLimit,
	)
	gate, _, err := s.authority.ActivateAutomationQuarantine(
		operationContext,
		store.AutomationQuarantineSourceInput{
			Board:             globalAutomationSessionBoard,
			Kind:              automationSessionSourceKind,
			SourceID:          lease.SessionID,
			ObservedUpdatedAt: s.quarantineObservation(),
			DiagnosticCode:    diagnostic,
		},
	)
	cancel()
	if err != nil {
		failure := fmt.Errorf(
			"activate global automation quarantine: %w",
			err,
		)
		s.recordFailure(failure)
		return failure
	}
	if !gate.Active {
		failure := errors.New(
			"global automation quarantine source did not activate the gate",
		)
		s.recordFailure(failure)
		return failure
	}
	// A nil activation error means the exact source insert committed (or the
	// same exact source already existed). Record that proof before observeGate
	// returns the expected quarantine error.
	s.sourceSaved.Store(true)
	return s.observeGate(gate)
}

func (s *automationDispatcherSession) reportTeardownFailure(err error) {
	if !errors.Is(err, processguard.ErrTeardownUnconfirmed) {
		return
	}
	_ = s.activateUnconfirmedQuarantine(automationTeardownDiagnostic)
}

func (s *automationDispatcherSession) observeGate(
	gate store.AutomationQuarantine,
) error {
	if !gate.Active {
		return nil
	}
	for {
		current := s.generation.Load()
		if current >= gate.Generation ||
			s.generation.CompareAndSwap(current, gate.Generation) {
			break
		}
	}
	quarantined := &store.AutomationQuarantinedError{
		Generation: gate.Generation,
	}
	s.errMu.Lock()
	if s.err == nil || errors.Is(s.err, store.ErrAutomationQuarantined) {
		s.err = quarantined
	}
	s.errMu.Unlock()
	s.cancelDispatcher()
	return quarantined
}

func (s *automationDispatcherSession) inspectGate(
	ctx context.Context,
	failOnContextCancellation bool,
) error {
	gate, err := s.authority.GetAutomationQuarantine(ctx)
	if err != nil {
		err = fmt.Errorf("inspect automation quarantine: %w", err)
		if !failOnContextCancellation {
			if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
				return err
			}
		}
		s.recordFailure(err)
		return err
	}
	return s.observeGate(gate)
}

func (s *automationDispatcherSession) renew(ctx context.Context) error {
	renewed, err := s.authority.RenewAutomationDispatcherSession(
		ctx,
		s.currentLease(),
		s.ttl,
	)
	if err != nil {
		err = fmt.Errorf("renew automation dispatcher session: %w", err)
		s.recordFailure(err)
		return err
	}
	s.replaceLease(renewed)
	return nil
}

func (s *automationDispatcherSession) monitor(ctx context.Context) {
	defer close(s.monitorDone)
	monitorTicker := time.NewTicker(s.monitorInterval)
	renewTicker := time.NewTicker(s.renewInterval)
	defer monitorTicker.Stop()
	defer renewTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-monitorTicker.C:
			operationContext, cancel := context.WithTimeout(
				context.Background(),
				automationSessionOperationLimit,
			)
			_ = s.inspectGate(operationContext, true)
			cancel()
		case <-renewTicker.C:
			operationContext, cancel := context.WithTimeout(
				context.Background(),
				automationSessionOperationLimit,
			)
			_ = s.renew(operationContext)
			cancel()
		}
	}
}

func (s *automationDispatcherSession) CheckGate(ctx context.Context) error {
	if s == nil {
		return errors.New("automation dispatcher session is not running")
	}
	if err := s.inspectGate(ctx, false); err != nil {
		return err
	}
	return s.Err()
}

func (s *automationDispatcherSession) Err() error {
	if s == nil {
		return nil
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *automationDispatcherSession) recordPermitBoundaryFailure(err error) {
	if err == nil {
		return
	}
	var quarantined *store.AutomationQuarantinedError
	if errors.As(err, &quarantined) && quarantined.Generation > 0 {
		for {
			current := s.generation.Load()
			if current >= quarantined.Generation ||
				s.generation.CompareAndSwap(
					current,
					quarantined.Generation,
				) {
				break
			}
		}
	}
	s.recordFailure(fmt.Errorf("automation permit boundary: %w", err))
}

func isAutomationPermitAuthorityFailure(err error) bool {
	return errors.Is(err, store.ErrAutomationQuarantined) ||
		errors.Is(err, store.ErrAutomationGateConflict) ||
		errors.Is(err, store.ErrAutomationHostNotIdle) ||
		errors.Is(err, store.ErrAutomationPermitClosed) ||
		errors.Is(err, store.ErrAutomationGateNotReady) ||
		errors.Is(err, store.ErrAutomationLockUnavailable)
}

func isCallerContextFailure(ctx context.Context, err error) bool {
	if ctx == nil || err == nil {
		return false
	}
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}

func (s *automationDispatcherSession) QuarantineGeneration() int64 {
	if s == nil {
		return 0
	}
	return s.generation.Load()
}

func (s *automationDispatcherSession) TeardownUnconfirmed() bool {
	if s == nil {
		return false
	}
	return s.unconfirmed.Load()
}

func (s *automationDispatcherSession) UncertaintySourcePersisted() bool {
	if s == nil {
		return false
	}
	return s.sourceSaved.Load()
}

// Shutdown keeps the heartbeat live through the final exact-generation ACK.
// If any queue has not proved termination, it deliberately withholds the ACK
// and activates a durable quarantine before releasing the exact session.
func (s *automationDispatcherSession) Shutdown(
	automationStopped bool,
) error {
	if s == nil {
		return nil
	}
	s.shutdownOnce.Do(func() {
		var quarantineErr error
		if !automationStopped ||
			(s.TeardownUnconfirmed() &&
				!s.UncertaintySourcePersisted()) {
			quarantineErr = s.activateUnconfirmedQuarantine(
				automationShutdownDiagnostic,
			)
		}
		operationContext, cancel := context.WithTimeout(
			context.Background(),
			automationSessionOperationLimit,
		)
		refreshErr := s.inspectGate(operationContext, true)
		cancel()

		generation := s.QuarantineGeneration()
		var acknowledgeErr error
		if automationStopped && !s.TeardownUnconfirmed() && generation > 0 {
			operationContext, cancel = context.WithTimeout(
				context.Background(),
				automationSessionOperationLimit,
			)
			acknowledgeErr = s.authority.AcknowledgeAutomationQuarantine(
				operationContext,
				s.currentLease(),
				generation,
			)
			cancel()
		}

		s.monitorCancel()
		monitorWait := time.NewTimer(
			automationSessionOperationLimit + time.Second,
		)
		monitorStopped := false
		select {
		case <-s.monitorDone:
			monitorStopped = true
			if !monitorWait.Stop() {
				<-monitorWait.C
			}
		case <-monitorWait.C:
		}
		var monitorErr error
		if !monitorStopped {
			monitorErr = errors.New(
				"automation session monitor did not stop within its operation bound",
			)
		}

		var releaseErr error
		// Only an exact persisted source makes it safe to release a session
		// after uncertainty. A generation observed from an unrelated source is
		// not evidence for this process lifetime.
		if !s.TeardownUnconfirmed() ||
			s.UncertaintySourcePersisted() {
			operationContext, cancel = context.WithTimeout(
				context.Background(),
				automationSessionOperationLimit,
			)
			released, err := s.authority.ReleaseAutomationDispatcherSession(
				operationContext,
				s.currentLease(),
			)
			cancel()
			releaseErr = err
			if releaseErr == nil && !released {
				releaseErr = store.ErrAutomationHostNotIdle
			}
		} else {
			releaseErr = errors.New(
				"automation dispatcher session release withheld after unconfirmed shutdown",
			)
		}
		s.shutdownErr = errors.Join(
			quarantineErr,
			refreshErr,
			acknowledgeErr,
			monitorErr,
			releaseErr,
			s.closeAuthority(),
		)
	})
	return s.shutdownErr
}

const automaticClaimPermitFailureReason = "Automatic claim authorization could not be verified before worker launch"

type automaticWorkerStartBlockedError struct {
	cause error
}

func (e *automaticWorkerStartBlockedError) Error() string {
	return fmt.Sprintf("%s: %v", errAutomaticWorkerStartBlocked, e.cause)
}

func (e *automaticWorkerStartBlockedError) Unwrap() []error {
	return []error{errAutomaticWorkerStartBlocked, e.cause}
}

func (s *automationDispatcherSession) workerReleaseGate(
	opened *store.Store,
) WorkerReleaseGate {
	return func(
		ctx context.Context,
		release WorkerRelease,
	) (released bool, resultErr error) {
		if release == nil {
			return false, errors.New("worker release is required")
		}
		permit, err := opened.AcquireAutomationPermitForSession(
			ctx,
			s.currentLease(),
		)
		if err != nil {
			if !isCallerContextFailure(ctx, err) {
				s.recordPermitBoundaryFailure(err)
			}
			return false, &automaticWorkerStartBlockedError{cause: err}
		}
		releaseCalled := false
		var releaseErr error
		guardErr := opened.WithAutomationPermit(ctx, permit, func() error {
			releaseCalled = true
			released, releaseErr = release()
			return releaseErr
		})
		closeErr := permit.Close()
		if closeErr != nil {
			_ = s.activateUnconfirmedQuarantine(
				automationStartBoundaryDiagnostic,
			)
			return released, errors.Join(
				&automaticWorkerStartBlockedError{cause: closeErr},
				guardErr,
			)
		}
		if !releaseCalled {
			if guardErr != nil && !isCallerContextFailure(ctx, guardErr) {
				s.recordPermitBoundaryFailure(guardErr)
			}
			return false, &automaticWorkerStartBlockedError{cause: guardErr}
		}
		return released, releaseErr
	}
}

func cleanupClaimAfterAutomationPermitFailure(
	opened *store.Store,
	claim *model.ClaimedTask,
	failureLimit int,
) error {
	if claim == nil {
		return nil
	}
	durable, cancel := durableContext()
	defer cancel()
	countFailure := false
	return failRunDurably(
		durable,
		opened,
		store.RunScope{
			RunID:      claim.Run.ID,
			ClaimToken: claim.ClaimToken,
		},
		automaticClaimPermitFailureReason,
		store.FailRunOptions{
			Outcome:      model.RunStatusReclaimed,
			CountFailure: &countFailure,
			FailureLimit: failureLimit,
		},
	)
}

func claimBoardTaskWithAutomationSession(
	ctx context.Context,
	session *automationDispatcherSession,
	options Options,
	opened *store.Store,
	input store.ClaimOptions,
) (*model.ClaimedTask, error) {
	if session == nil {
		return nil, errors.New("automation dispatcher session is required")
	}
	permit, err := opened.AcquireAutomationPermitForSession(
		ctx,
		session.currentLease(),
	)
	if err != nil {
		if !isCallerContextFailure(ctx, err) {
			session.recordPermitBoundaryFailure(err)
		}
		return nil, err
	}
	claim, claimErr := options.claimBoardTask(
		ctx,
		opened,
		permit,
		input,
	)
	var validateErr error
	if claimErr == nil {
		validateErr = opened.ValidateAutomationPermit(ctx, permit)
	}
	preCloseErr := errors.Join(claimErr, validateErr)
	var cleanupErr error
	if preCloseErr != nil && claim != nil {
		cleanupErr = cleanupClaimAfterAutomationPermitFailure(
			opened,
			claim,
			options.FailureLimit,
		)
		claim = nil
	}
	closeErr := permit.Close()
	if preCloseErr == nil && closeErr != nil && claim != nil {
		cleanupErr = cleanupClaimAfterAutomationPermitFailure(
			opened,
			claim,
			options.FailureLimit,
		)
		claim = nil
	}
	boundaryErr := errors.Join(preCloseErr, closeErr, cleanupErr)
	if boundaryErr != nil {
		if (validateErr != nil &&
			!isCallerContextFailure(ctx, validateErr)) ||
			closeErr != nil || cleanupErr != nil ||
			isAutomationPermitAuthorityFailure(claimErr) {
			session.recordPermitBoundaryFailure(boundaryErr)
		}
		return nil, boundaryErr
	}
	return claim, nil
}
