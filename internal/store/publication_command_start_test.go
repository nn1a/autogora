package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func acquirePublicationCommandPermit(
	t *testing.T,
	opened *Store,
	lease AutomationDispatcherSessionLease,
) *AutomationPermit {
	t.Helper()
	permit, err := opened.AcquireAutomationPermitForSession(
		context.Background(),
		lease,
	)
	if err != nil {
		t.Fatalf("acquire publication command permit: %v", err)
	}
	t.Cleanup(func() {
		if err := permit.Close(); err != nil {
			t.Errorf("close publication command permit: %v", err)
		}
	})
	return permit
}

func TestPublicationAttemptCommandStartAllowsMultipleExactSessionCommands(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "command_multiple")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-command-multiple",
		time.Minute,
	)

	firstError := errors.New("first release result")
	secondError := errors.New("second release result")
	tests := []struct {
		name     string
		released bool
		err      error
	}{
		{name: "released", released: true},
		{name: "not released with error", err: firstError},
		{name: "released with error", released: true, err: secondError},
		{name: "not released without error"},
	}
	var calls int
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			permit := acquirePublicationCommandPermit(t, opened, lease)
			released, err := opened.WithPublicationAttemptCommandStart(
				ctx,
				permit,
				attempt,
				func() (bool, error) {
					calls++
					return test.released, test.err
				},
			)
			if released != test.released || !errors.Is(err, test.err) {
				t.Fatalf(
					"command release = %v, %v; want %v, %v",
					released,
					err,
					test.released,
					test.err,
				)
			}
			if err := permit.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if calls != len(tests) {
		t.Fatalf("release calls = %d, want %d", calls, len(tests))
	}
}

func TestPublicationAttemptCommandStartRejectsWrongCapabilities(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(t, opened, "wrong_store")
		_, attempt, _ := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-wrong-store",
			time.Minute,
		)

		other := openAutomationTestStore(t)
		otherLease := registerAutomationTestSession(
			t,
			other,
			"default",
			"publisher-other-store",
		)
		otherPermit := acquirePublicationCommandPermit(t, other, otherLease)
		called := false
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			otherPermit,
			attempt,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if err == nil || released || called {
			t.Fatalf(
				"wrong store release = %v, called=%v, err=%v",
				released,
				called,
				err,
			)
		}
	})

	t.Run("replacement session", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(t, opened, "wrong_session")
		_, attempt, original := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-original-session",
			time.Minute,
		)
		if released, err := opened.ReleaseAutomationDispatcherSession(
			ctx,
			original,
		); err != nil || !released {
			t.Fatalf("release original session = %v, %v", released, err)
		}
		replacement := registerAutomationTestSession(
			t,
			opened,
			"default",
			"publisher-replacement-session",
		)
		permit := acquirePublicationCommandPermit(t, opened, replacement)
		called := false
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if !errors.Is(err, ErrPublicationAttemptScope) || released || called {
			t.Fatalf(
				"replacement release = %v, called=%v, err=%v",
				released,
				called,
				err,
			)
		}
	})

	t.Run("gate generation", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(t, opened, "wrong_generation")
		_, attempt, lease := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-wrong-generation",
			time.Minute,
		)
		permit := acquirePublicationCommandPermit(t, opened, lease)
		permit.generation++
		called := false
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if !errors.Is(err, ErrAutomationGateConflict) || released || called {
			t.Fatalf(
				"wrong generation release = %v, called=%v, err=%v",
				released,
				called,
				err,
			)
		}
	})
}

func TestPublicationAttemptCommandStartRequiresLiveClaimAndSession(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		current := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
		opened.publicationClock = func() time.Time { return current }
		_, pending := createPendingPublicationAttemptFixture(t, opened, "expired_claim")
		_, attempt, lease := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-expired-claim",
			MinPublicationClaimTTL,
		)
		current = current.Add(MinPublicationClaimTTL + time.Nanosecond)

		permit := acquirePublicationCommandPermit(t, opened, lease)
		called := false
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if !errors.Is(err, ErrPublicationClaimExpired) || released || called {
			t.Fatalf(
				"expired claim release = %v, called=%v, err=%v",
				released,
				called,
				err,
			)
		}
	})

	t.Run("session", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(t, opened, "expired_session")
		_, attempt, lease := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-expired-session",
			time.Minute,
		)
		permit := acquirePublicationCommandPermit(t, opened, lease)
		expired := time.Now().UTC().Add(-time.Minute).Format(automationTimestampLayout)
		if _, err := opened.db.ExecContext(ctx, `
			UPDATE automation_dispatcher_sessions SET expires_at = ?
			WHERE session_id = ?
		`, expired, lease.SessionID); err != nil {
			t.Fatal(err)
		}
		called := false
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if !errors.Is(err, ErrAutomationHostNotIdle) || released || called {
			t.Fatalf(
				"expired session release = %v, called=%v, err=%v",
				released,
				called,
				err,
			)
		}
	})
}

func TestPublicationAttemptCommandStartRechecksSessionAfterAttemptWait(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "session_wait")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-session-wait",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)

	attempt.state.mu.Lock()
	stateLocked := true
	defer func() {
		if stateLocked {
			attempt.state.mu.Unlock()
		}
	}()
	expiresAt := time.Now().UTC().Add(400 * time.Millisecond)
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE automation_dispatcher_sessions SET expires_at = ?
		WHERE session_id = ?
	`, expiresAt.Format(automationTimestampLayout), lease.SessionID); err != nil {
		t.Fatal(err)
	}

	type commandResult struct {
		released bool
		err      error
	}
	var called atomic.Bool
	done := make(chan commandResult, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called.Store(true)
				return true, nil
			},
		)
		done <- commandResult{released: released, err: err}
	}()

	lockDeadline := time.Now().Add(time.Second)
	for permit.mu.TryLock() {
		permit.mu.Unlock()
		if time.Now().After(lockDeadline) {
			t.Fatal("command start did not acquire the automation permit mutex")
		}
		time.Sleep(time.Millisecond)
	}
	wait := time.Until(expiresAt.Add(30 * time.Millisecond))
	if wait > 0 {
		time.Sleep(wait)
	}
	select {
	case result := <-done:
		t.Fatalf("command start did not wait for attempt mutex: %+v", result)
	default:
	}

	attempt.state.mu.Unlock()
	stateLocked = false
	result := <-done
	if !errors.Is(result.err, ErrAutomationHostNotIdle) ||
		result.released || called.Load() {
		t.Fatalf(
			"expired session after wait release = %v, called=%v, err=%v",
			result.released,
			called.Load(),
			result.err,
		)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAttemptCommandStartRechecksClaimAfterFinalAuthorityWait(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	opened.db.SetMaxOpenConns(2)
	opened.db.SetMaxIdleConns(2)
	baseTime := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	var publicationTime atomic.Int64
	publicationTime.Store(baseTime.UnixNano())
	opened.publicationClock = func() time.Time {
		return time.Unix(0, publicationTime.Load()).UTC()
	}
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"claim_authority_wait",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-claim-authority-wait",
		MinPublicationClaimTTL,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)

	heldConnection, err := opened.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connectionHeld := true
	defer func() {
		if connectionHeld {
			_ = heldConnection.Close()
		}
	}()
	initialWaits := opened.db.Stats().WaitCount
	type commandResult struct {
		released bool
		err      error
	}
	var called atomic.Bool
	done := make(chan commandResult, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called.Store(true)
				return true, nil
			},
		)
		done <- commandResult{released: released, err: err}
	}()

	waitDeadline := time.NewTimer(2 * time.Second)
	defer waitDeadline.Stop()
	waitTicker := time.NewTicker(time.Millisecond)
	defer waitTicker.Stop()
waiting:
	for {
		if opened.db.Stats().WaitCount > initialWaits {
			break waiting
		}
		select {
		case result := <-done:
			t.Fatalf("command passed final authority wait: %+v", result)
		case <-waitDeadline.C:
			t.Fatal("final authority validation did not wait for a connection")
		case <-waitTicker.C:
		}
	}

	publicationTime.Store(
		baseTime.Add(MinPublicationClaimTTL + time.Nanosecond).UnixNano(),
	)
	if err := heldConnection.Close(); err != nil {
		t.Fatal(err)
	}
	connectionHeld = false
	result := <-done
	if !errors.Is(result.err, ErrPublicationClaimExpired) ||
		result.released || called.Load() {
		t.Fatalf(
			"expired claim after authority wait release = %v, called=%v, err=%v",
			result.released,
			called.Load(),
			result.err,
		)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAttemptCommandStartRechecksSessionAfterClaimValidation(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	baseTime := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	opened.publicationClock = func() time.Time { return baseTime }
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"session_after_claim",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-session-after-claim",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)
	sessionExpiresAt := time.Now().UTC().Add(250 * time.Millisecond)
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE automation_dispatcher_sessions SET expires_at = ?
		WHERE session_id = ?
	`, sessionExpiresAt.Format(automationTimestampLayout), lease.SessionID); err != nil {
		t.Fatal(err)
	}

	claimClockEntered := make(chan struct{})
	allowClaimClock := make(chan struct{})
	var claimClockOnce sync.Once
	opened.publicationClock = func() time.Time {
		claimClockOnce.Do(func() { close(claimClockEntered) })
		<-allowClaimClock
		return baseTime
	}
	type commandResult struct {
		released bool
		err      error
	}
	var called atomic.Bool
	done := make(chan commandResult, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				called.Store(true)
				return true, nil
			},
		)
		done <- commandResult{released: released, err: err}
	}()
	select {
	case <-claimClockEntered:
	case result := <-done:
		t.Fatalf("command did not reach claim validation: %+v", result)
	case <-time.After(2 * time.Second):
		t.Fatal("command did not reach claim validation")
	}
	wait := time.Until(sessionExpiresAt.Add(20 * time.Millisecond))
	if wait > 0 {
		time.Sleep(wait)
	}
	close(allowClaimClock)

	result := <-done
	if !errors.Is(result.err, ErrAutomationHostNotIdle) ||
		result.released || called.Load() {
		t.Fatalf(
			"expired session after claim release = %v, called=%v, err=%v",
			result.released,
			called.Load(),
			result.err,
		)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAttemptCommandStartCancellationKeepsBarrierThroughRelease(
	t *testing.T,
) {
	callerContext, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	opened := openAutomationTestStore(t)
	task, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"canceled_release",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-canceled-release",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)

	releaseEntered := make(chan struct{})
	allowRelease := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(allowRelease) })
	type commandResult struct {
		released bool
		err      error
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			callerContext,
			permit,
			attempt,
			func() (bool, error) {
				close(releaseEntered)
				<-allowRelease
				return true, nil
			},
		)
		commandDone <- commandResult{released: released, err: err}
	}()
	<-releaseEntered
	cancelCaller()

	mutationStarted := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		close(mutationStarted)
		mutationContext, cancelMutation := context.WithTimeout(
			context.Background(),
			2*time.Second,
		)
		defer cancelMutation()
		for {
			_, err := opened.AddComment(
				mutationContext,
				task.ID,
				"operator",
				"must wait for the command release barrier",
			)
			if err == nil {
				mutationDone <- nil
				return
			}
			select {
			case <-mutationContext.Done():
				mutationDone <- errors.Join(mutationContext.Err(), err)
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	finishStarted := make(chan struct{})
	finishDone := make(chan error, 1)
	go func() {
		close(finishStarted)
		_, err := opened.FinishAutomatedPublicationAttempt(
			context.Background(),
			attempt,
			PublicationAttemptResultInput{
				Outcome:        PublicationAttemptUnknown,
				ExecutorStatus: PublicationExecutorUnknown,
				ErrorKind:      PublicationErrorCommandStartUncertain,
				Error:          "caller canceled after command release",
			},
		)
		finishDone <- err
	}()
	<-mutationStarted
	<-finishStarted
	select {
	case err := <-mutationDone:
		t.Fatalf("board mutation passed a release in flight: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish passed a release in flight: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(allowRelease) })
	command := <-commandDone
	if !command.released || command.err != nil {
		t.Fatalf(
			"linearized command release = %v, err=%v",
			command.released,
			command.err,
		)
	}
	if err := <-mutationDone; err != nil {
		t.Fatalf("board mutation after release: %v", err)
	}
	if err := <-finishDone; err != nil {
		t.Fatalf("finish after release: %v", err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAttemptCommandStartCancellationBeforeBoundaryDeniesRelease(
	t *testing.T,
) {
	callerContext, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"cancel_before_boundary",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-cancel-before-boundary",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)

	attempt.state.mu.Lock()
	stateLocked := true
	defer func() {
		if stateLocked {
			attempt.state.mu.Unlock()
		}
	}()
	type commandResult struct {
		released bool
		err      error
	}
	var called atomic.Bool
	done := make(chan commandResult, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			callerContext,
			permit,
			attempt,
			func() (bool, error) {
				called.Store(true)
				return true, nil
			},
		)
		done <- commandResult{released: released, err: err}
	}()

	lockDeadline := time.Now().Add(time.Second)
	for permit.mu.TryLock() {
		permit.mu.Unlock()
		if time.Now().After(lockDeadline) {
			t.Fatal("command start did not acquire the automation permit mutex")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancelCaller()
	attempt.state.mu.Unlock()
	stateLocked = false

	result := <-done
	if !errors.Is(result.err, context.Canceled) ||
		result.released || called.Load() {
		t.Fatalf(
			"canceled command release = %v, called=%v, err=%v",
			result.released,
			called.Load(),
			result.err,
		)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationAttemptCommandStartRejectsFinishedAndRecordedAttempts(
	t *testing.T,
) {
	tests := []struct {
		name   string
		result PublicationAttemptResultInput
	}{
		{
			name: "known finish",
			result: PublicationAttemptResultInput{
				Outcome:        PublicationAttemptFailed,
				ExecutorStatus: PublicationExecutorFailed,
				ErrorKind:      PublicationErrorCommandFailed,
				Error:          "command failed",
			},
		},
		{
			name: "unknown finish",
			result: PublicationAttemptResultInput{
				Outcome:        PublicationAttemptUnknown,
				ExecutorStatus: PublicationExecutorUnknown,
				ErrorKind:      PublicationErrorCommandStartUncertain,
				Error:          "command start is uncertain",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			opened := openAutomationTestStore(t)
			_, pending := createPendingPublicationAttemptFixture(
				t,
				opened,
				strings.ReplaceAll(test.name, " ", "_"),
			)
			_, attempt, lease := beginPublicationAttemptFixture(
				t,
				opened,
				pending,
				"publisher-"+strings.ReplaceAll(test.name, " ", "-"),
				time.Minute,
			)

			attempt.state.mu.Lock()
			forgedOpen := &PublicationAttemptPermit{
				state: &publicationAttemptPermitState{
					intent:        attempt.state.intent,
					claimToken:    attempt.state.claimToken,
					authorityPath: attempt.state.authorityPath,
					lockPath:      attempt.state.lockPath,
					gateToken:     attempt.state.gateToken,
					sessionBoard:  attempt.state.sessionBoard,
					sessionToken:  attempt.state.sessionToken,
				},
			}
			attempt.state.mu.Unlock()

			if _, err := opened.FinishAutomatedPublicationAttempt(
				ctx,
				attempt,
				test.result,
			); err != nil {
				t.Fatalf("finish publication attempt: %v", err)
			}

			for _, candidate := range []*PublicationAttemptPermit{
				attempt,
				forgedOpen,
			} {
				permit := acquirePublicationCommandPermit(t, opened, lease)
				called := false
				released, err := opened.WithPublicationAttemptCommandStart(
					ctx,
					permit,
					candidate,
					func() (bool, error) {
						called = true
						return true, nil
					},
				)
				if !errors.Is(err, ErrPublicationAttemptPermitClosed) ||
					released || called {
					t.Fatalf(
						"finished release = %v, called=%v, err=%v",
						released,
						called,
						err,
					)
				}
				if err := permit.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPublicationAttemptCommandStartSerializesQuarantine(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "command_quarantine")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-command-quarantine",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)
	intent := attempt.Intent()
	quarantineInput := AutomationQuarantineSourceInput{
		Board:             "default",
		Kind:              automationTestSourceKind,
		SourceID:          "publication-command-release",
		ObservedUpdatedAt: intent.PublicationUpdatedAt,
		ObservedClaimEpoch: strconv.FormatInt(
			intent.ClaimEpoch,
			10,
		),
		DiagnosticCode: "process_teardown_unconfirmed",
	}

	releaseEntered := make(chan struct{})
	allowRelease := make(chan struct{})
	commandDone := make(chan error, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				close(releaseEntered)
				<-allowRelease
				return true, nil
			},
		)
		if err == nil && !released {
			err = errors.New("command release unexpectedly returned false")
		}
		commandDone <- err
	}()
	<-releaseEntered

	activationStarted := make(chan struct{})
	type activationResult struct {
		activated bool
		err       error
	}
	activationDone := make(chan activationResult, 1)
	go func() {
		close(activationStarted)
		_, activated, err := opened.ActivateAutomationQuarantine(
			ctx,
			quarantineInput,
		)
		activationDone <- activationResult{activated: activated, err: err}
	}()
	<-activationStarted
	select {
	case result := <-activationDone:
		t.Fatalf("quarantine passed active release callback: %+v", result)
	case <-time.After(40 * time.Millisecond):
	}

	close(allowRelease)
	if err := <-commandDone; err != nil {
		t.Fatalf("command release: %v", err)
	}
	select {
	case result := <-activationDone:
		t.Fatalf("quarantine passed an open command permit: %+v", result)
	case <-time.After(40 * time.Millisecond):
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-activationDone
	if result.err != nil || !result.activated {
		t.Fatalf("activate quarantine = %+v", result)
	}
	if next, err := opened.AcquireAutomationPermitForSession(
		ctx,
		lease,
	); !errors.Is(err, ErrAutomationQuarantined) || next != nil {
		t.Fatalf("permit after quarantine = %v, err=%v", next, err)
	}
}

func TestPublicationAttemptCommandStartCopiedPermitSerializesWithFinish(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "command_copy")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-command-copy",
		time.Minute,
	)
	copied := *attempt
	firstPermit := acquirePublicationCommandPermit(t, opened, lease)
	secondPermit := acquirePublicationCommandPermit(t, opened, lease)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			firstPermit,
			attempt,
			func() (bool, error) {
				close(firstEntered)
				<-releaseFirst
				return true, nil
			},
		)
		if err == nil && !released {
			err = errors.New("first release unexpectedly returned false")
		}
		firstDone <- err
	}()
	<-firstEntered

	var secondCalled atomic.Bool
	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		released, err := opened.WithPublicationAttemptCommandStart(
			ctx,
			secondPermit,
			&copied,
			func() (bool, error) {
				secondCalled.Store(true)
				close(secondEntered)
				<-releaseSecond
				return true, nil
			},
		)
		if err == nil && !released {
			err = errors.New("second release unexpectedly returned false")
		}
		secondDone <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("copied attempt permit did not share release serialization")
	case <-time.After(40 * time.Millisecond):
	}

	finishDone := make(chan error, 1)
	go func() {
		_, err := opened.FinishAutomatedPublicationAttempt(
			ctx,
			attempt,
			PublicationAttemptResultInput{
				Outcome:        PublicationAttemptUnknown,
				ExecutorStatus: PublicationExecutorUnknown,
				ErrorKind:      PublicationErrorCommandStartUncertain,
				Error:          "test finish",
			},
		)
		finishDone <- err
	}()
	select {
	case err := <-finishDone:
		t.Fatalf("finish passed active command release: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := firstPermit.Close(); err != nil {
		t.Fatal(err)
	}
	<-secondEntered
	if !secondCalled.Load() {
		t.Fatal("second release callback was not called")
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish passed second command release: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseSecond)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if err := secondPermit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-finishDone; err != nil {
		t.Fatalf("finish after commands: %v", err)
	}
}

func TestPublicationAttemptCommandStartConcurrentCallsAreRaceSafe(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "command_race")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-command-race",
		time.Minute,
	)

	const commandCount = 12
	permits := make([]*AutomationPermit, commandCount)
	for index := range permits {
		permits[index] = acquirePublicationCommandPermit(t, opened, lease)
	}
	var calls atomic.Int64
	var wait sync.WaitGroup
	errorsFound := make(chan error, commandCount)
	for index := range permits {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			candidate := *attempt
			released, err := opened.WithPublicationAttemptCommandStart(
				ctx,
				permits[index],
				&candidate,
				func() (bool, error) {
					calls.Add(1)
					return true, nil
				},
			)
			if err != nil {
				errorsFound <- err
			} else if !released {
				errorsFound <- errors.New("release unexpectedly returned false")
			}
			if closeErr := permits[index].Close(); closeErr != nil {
				errorsFound <- closeErr
			}
		}(index)
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent command: %v", err)
	}
	if calls.Load() != commandCount {
		t.Fatalf("release calls = %d, want %d", calls.Load(), commandCount)
	}
}

func TestPublicationAttemptCommandStartRejectsPublicationTupleChange(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "tuple_change")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-tuple-change",
		time.Minute,
	)
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publications SET remote = 'changed' WHERE id = ?",
		pending.ID,
	); err != nil {
		t.Fatal(err)
	}
	permit := acquirePublicationCommandPermit(t, opened, lease)
	called := false
	released, err := opened.WithPublicationAttemptCommandStart(
		ctx,
		permit,
		attempt,
		func() (bool, error) {
			called = true
			return true, nil
		},
	)
	if !errors.Is(err, ErrPublicationAttemptScope) || released || called {
		t.Fatalf(
			"changed tuple release = %v, called=%v, err=%v",
			released,
			called,
			err,
		)
	}
}

func TestPublicationAttemptCommandStartRejectsNilRelease(t *testing.T) {
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "nil_release")
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-nil-release",
		time.Minute,
	)
	permit := acquirePublicationCommandPermit(t, opened, lease)
	released, err := opened.WithPublicationAttemptCommandStart(
		context.Background(),
		permit,
		attempt,
		nil,
	)
	if err == nil || released {
		t.Fatalf("nil release = %v, %v", released, err)
	}
	if publication, getErr := opened.GetPublication(
		context.Background(),
		pending.ID,
	); getErr != nil || publication.Status != model.PublicationPublishing {
		t.Fatalf("publication after nil release = %+v, %v", publication, getErr)
	}
}
