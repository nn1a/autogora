package dispatcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

type fakeAutomationSessionAuthority struct {
	mu sync.Mutex

	gate        store.AutomationQuarantine
	renewErr    error
	activateErr error
	registerOK  bool
	calls       []string
	acked       []int64
	activated   []store.AutomationQuarantineSourceInput
}

func (f *fakeAutomationSessionAuthority) addCall(value string) {
	f.mu.Lock()
	f.calls = append(f.calls, value)
	f.mu.Unlock()
}

func (f *fakeAutomationSessionAuthority) RegisterAutomationDispatcherSession(
	_ context.Context,
	board string,
	sessionID string,
	_ time.Duration,
) (store.AutomationDispatcherSessionLease, bool, error) {
	f.addCall("register")
	f.mu.Lock()
	acquired := f.registerOK
	f.mu.Unlock()
	return store.AutomationDispatcherSessionLease{
		SessionID:    sessionID,
		Board:        board,
		RegisteredAt: "2026-07-24T00:00:00.000000000Z",
		RenewedAt:    "2026-07-24T00:00:00.000000000Z",
		ExpiresAt:    "2026-07-24T00:01:00.000000000Z",
	}, acquired, nil
}

func (f *fakeAutomationSessionAuthority) RenewAutomationDispatcherSession(
	_ context.Context,
	lease store.AutomationDispatcherSessionLease,
	_ time.Duration,
) (store.AutomationDispatcherSessionLease, error) {
	f.addCall("renew")
	f.mu.Lock()
	defer f.mu.Unlock()
	return lease, f.renewErr
}

func (f *fakeAutomationSessionAuthority) ReleaseAutomationDispatcherSession(
	_ context.Context,
	_ store.AutomationDispatcherSessionLease,
) (bool, error) {
	f.addCall("release")
	return true, nil
}

func (f *fakeAutomationSessionAuthority) AcknowledgeAutomationQuarantine(
	_ context.Context,
	_ store.AutomationDispatcherSessionLease,
	generation int64,
) error {
	f.addCall("ack")
	f.mu.Lock()
	f.acked = append(f.acked, generation)
	f.mu.Unlock()
	return nil
}

func (f *fakeAutomationSessionAuthority) GetAutomationQuarantine(
	ctx context.Context,
) (store.AutomationQuarantine, error) {
	f.addCall("inspect")
	if err := ctx.Err(); err != nil {
		return store.AutomationQuarantine{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gate, nil
}

func (f *fakeAutomationSessionAuthority) ActivateAutomationQuarantine(
	_ context.Context,
	input store.AutomationQuarantineSourceInput,
) (store.AutomationQuarantine, bool, error) {
	f.addCall("activate")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activated = append(f.activated, input)
	if f.activateErr != nil {
		return f.gate, false, f.activateErr
	}
	if !f.gate.Active {
		f.gate.Active = true
		f.gate.Generation++
		if f.gate.Generation < 1 {
			f.gate.Generation = 1
		}
	}
	return f.gate, true, nil
}

func (f *fakeAutomationSessionAuthority) setActivationError(err error) {
	f.mu.Lock()
	f.activateErr = err
	f.mu.Unlock()
}

func (f *fakeAutomationSessionAuthority) setGate(
	gate store.AutomationQuarantine,
) {
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
}

func (f *fakeAutomationSessionAuthority) snapshot() (
	[]string,
	[]int64,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...),
		append([]int64(nil), f.acked...)
}

func waitAutomationSessionCancellation(
	t *testing.T,
	canceled <-chan struct{},
) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-canceled:
	case <-timer.C:
		t.Fatal("automation session did not cancel the dispatcher")
	}
}

func TestAutomationSessionAcknowledgesOnlyAfterCleanUnwind(t *testing.T) {
	authority := &fakeAutomationSessionAuthority{registerOK: true}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error {
			authority.addCall("close")
			return nil
		},
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-test",
		time.Minute,
		20*time.Millisecond,
		5*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if lease := session.currentLease(); lease.Board != globalAutomationSessionBoard {
		t.Fatalf("session board=%q", lease.Board)
	}
	authority.setGate(store.AutomationQuarantine{
		Active: true, Generation: 7,
	})
	waitAutomationSessionCancellation(t, canceled)

	shutdownErr := session.Shutdown(true)
	if !errors.Is(shutdownErr, store.ErrAutomationQuarantined) {
		t.Fatalf("shutdown error=%v", shutdownErr)
	}
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 1 || acknowledged[0] != 7 {
		t.Fatalf("acknowledged=%v calls=%v", acknowledged, calls)
	}
	ackIndex, releaseIndex, closeIndex := -1, -1, -1
	for index, call := range calls {
		switch call {
		case "ack":
			ackIndex = index
		case "release":
			releaseIndex = index
		case "close":
			closeIndex = index
		}
	}
	if ackIndex < 0 || releaseIndex <= ackIndex || closeIndex <= releaseIndex {
		t.Fatalf("shutdown order=%v", calls)
	}
}

func TestAutomationSessionWithholdsAckWhenQueueShutdownIsUnclear(t *testing.T) {
	authority := &fakeAutomationSessionAuthority{registerOK: true}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-unclear",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	authority.setGate(store.AutomationQuarantine{
		Active: true, Generation: 8,
	})
	if err := session.CheckGate(context.Background()); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("gate error=%v", err)
	}
	waitAutomationSessionCancellation(t, canceled)
	if err := session.Shutdown(false); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("unexpected ACK=%v calls=%v", acknowledged, calls)
	}
	if calls[len(calls)-1] != "release" {
		t.Fatalf("session was not released: %v", calls)
	}
}

func TestAutomationSessionUnclearShutdownActivatesQuarantineBeforeRelease(
	t *testing.T,
) {
	authority := &fakeAutomationSessionAuthority{registerOK: true}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-unclear-inactive",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Shutdown(false); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
	waitAutomationSessionCancellation(t, canceled)
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("unclear shutdown unexpectedly ACKed=%v", acknowledged)
	}
	activateIndex, releaseIndex := -1, -1
	for index, call := range calls {
		switch call {
		case "activate":
			activateIndex = index
		case "release":
			releaseIndex = index
		}
	}
	if activateIndex < 0 || releaseIndex <= activateIndex {
		t.Fatalf("unclear shutdown order=%v", calls)
	}
	if !session.TeardownUnconfirmed() ||
		session.QuarantineGeneration() < 1 {
		t.Fatalf(
			"unclear shutdown latch=%t generation=%d",
			session.TeardownUnconfirmed(),
			session.QuarantineGeneration(),
		)
	}
}

func TestAutomationSessionTeardownReporterActivatesWithoutFalseAck(
	t *testing.T,
) {
	authority := &fakeAutomationSessionAuthority{registerOK: true}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-teardown",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	reportContext := processguard.WithTeardownFailureReporter(
		context.Background(),
		session.reportTeardownFailure,
	)
	processguard.ReportTeardownFailure(
		reportContext,
		processguard.ErrTeardownUnconfirmed,
	)
	waitAutomationSessionCancellation(t, canceled)
	if err := session.Shutdown(true); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("unconfirmed teardown unexpectedly ACKed=%v calls=%v", acknowledged, calls)
	}
	if !session.TeardownUnconfirmed() {
		t.Fatal("teardown reporter did not latch uncertainty")
	}
}

func TestAutomationSessionRetriesFailedUncertaintySourceOnShutdown(
	t *testing.T,
) {
	activationFailure := errors.New("injected activation failure")
	authority := &fakeAutomationSessionAuthority{
		registerOK:  true,
		activateErr: activationFailure,
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-activation-retry",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	session.reportTeardownFailure(processguard.ErrTeardownUnconfirmed)
	waitAutomationSessionCancellation(t, canceled)
	if !errors.Is(session.Err(), activationFailure) {
		t.Fatalf("session error=%v", session.Err())
	}
	if session.UncertaintySourcePersisted() {
		t.Fatal("failed activation was recorded as persisted")
	}

	authority.setActivationError(nil)
	if err := session.Shutdown(true); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
	if !errors.Is(session.Err(), activationFailure) {
		t.Fatalf("original activation failure was lost: %v", session.Err())
	}
	if !session.UncertaintySourcePersisted() {
		t.Fatal("shutdown did not persist the exact uncertainty source")
	}
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("uncertain session unexpectedly ACKed=%v", acknowledged)
	}
	activations, releases := 0, 0
	for _, call := range calls {
		switch call {
		case "activate":
			activations++
		case "release":
			releases++
		}
	}
	if activations != 2 || releases != 1 {
		t.Fatalf(
			"activation retry calls=%v activations=%d releases=%d",
			calls,
			activations,
			releases,
		)
	}
}

func TestAutomationSessionUnrelatedGenerationCannotAuthorizeRelease(
	t *testing.T,
) {
	activationFailure := errors.New("persistent activation failure")
	authority := &fakeAutomationSessionAuthority{
		registerOK:  true,
		activateErr: activationFailure,
		gate: store.AutomationQuarantine{
			Active:     true,
			Generation: 9,
		},
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-unrelated-generation",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	session.reportTeardownFailure(processguard.ErrTeardownUnconfirmed)
	waitAutomationSessionCancellation(t, canceled)
	if err := session.Shutdown(true); !errors.Is(
		err,
		activationFailure,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
	if session.QuarantineGeneration() != 9 {
		t.Fatalf(
			"observed generation=%d",
			session.QuarantineGeneration(),
		)
	}
	if session.UncertaintySourcePersisted() {
		t.Fatal("failed exact source was recorded as persisted")
	}
	calls, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("uncertain session unexpectedly ACKed=%v", acknowledged)
	}
	for _, call := range calls {
		if call == "release" {
			t.Fatalf(
				"unrelated generation authorized session release: %v",
				calls,
			)
		}
	}
}

func TestAutomationSessionRenewFailureCancelsDispatcher(t *testing.T) {
	renewFailure := errors.New("injected session renewal failure")
	authority := &fakeAutomationSessionAuthority{
		registerOK: true,
		renewErr:   renewFailure,
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-renew",
		time.Minute,
		5*time.Millisecond,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitAutomationSessionCancellation(t, canceled)
	if !errors.Is(session.Err(), renewFailure) {
		t.Fatalf("session error=%v", session.Err())
	}
	if err := session.Shutdown(true); err != nil {
		t.Fatalf("shutdown error=%v", err)
	}
	_, acknowledged := authority.snapshot()
	if len(acknowledged) != 0 {
		t.Fatalf("renewal failure unexpectedly ACKed=%v", acknowledged)
	}
}

func TestAutomationSessionCallerCancellationDoesNotPoisonSession(
	t *testing.T,
) {
	authority := &fakeAutomationSessionAuthority{registerOK: true}
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() {},
		"dispatcher-canceled-check",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.CheckGate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("gate error=%v", err)
	}
	if err := session.Err(); err != nil {
		t.Fatalf("caller cancellation poisoned session: %v", err)
	}
	if err := session.Shutdown(true); err != nil {
		t.Fatalf("shutdown error=%v", err)
	}
}

func TestAutomationSessionIDsAreProcessScopedAndUnique(t *testing.T) {
	first, second := automationDispatcherSessionID(), automationDispatcherSessionID()
	if first == second || first == "" || second == "" {
		t.Fatalf("session IDs first=%q second=%q", first, second)
	}
}

func TestActiveAutomationQuarantinePreventsEveryDispatcherEntryPath(
	t *testing.T,
) {
	tests := []struct {
		name  string
		once  bool
		board string
	}{
		{name: "once all boards", once: true},
		{name: "once explicit board", once: true, board: "default"},
		{name: "watch all boards"},
		{name: "watch explicit board", board: "default"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, dbPath := testManager(t)
			coordination, err := manager.OpenCoordinationStore(
				context.Background(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, activated, err := coordination.ActivateAutomationQuarantine(
				context.Background(),
				store.AutomationQuarantineSourceInput{
					Board:              "default",
					Kind:               "publication",
					SourceID:           "startup-gate",
					ObservedUpdatedAt:  "2026-07-24T00:00:00.000Z",
					ObservedClaimEpoch: "1",
					DiagnosticCode:     "process_teardown_unconfirmed",
				},
			); err != nil || !activated {
				_ = coordination.Close()
				t.Fatalf("activate=%t err=%v", activated, err)
			}
			if err := coordination.Close(); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			calls := make(map[string]int)
			record := func(kind string) {
				mu.Lock()
				calls[kind]++
				mu.Unlock()
			}
			config := agentconfig.Default()
			err = Run(context.Background(), Options{
				DBPath: dbPath, CLIPath: "/tmp/autogora",
				Board: test.board, Once: test.once,
				AgentConfig: &config,
				testHooks: &dispatcherTestHooks{
					maintainGlobal: func(
						context.Context,
						*boards.Manager,
						Options,
					) error {
						record("global maintenance")
						return nil
					},
					readMetadata: func(
						*boards.Manager,
						string,
					) (boards.Metadata, error) {
						record("metadata")
						return boards.Metadata{}, nil
					},
					maintainBoard: func(
						context.Context,
						*boards.Manager,
						string,
						Options,
					) error {
						record("maintenance")
						return nil
					},
					claimTask: func(
						context.Context,
						*store.Store,
						store.ClaimOptions,
					) (*model.ClaimedTask, error) {
						record("claim")
						return nil, nil
					},
					notifications: func(
						context.Context,
						*boards.Manager,
						[]string,
						Options,
					) {
						record("notifications")
					},
					queueEnqueued: func(string, []string) {
						record("enqueue")
					},
				},
			})
			if !errors.Is(err, store.ErrAutomationQuarantined) {
				t.Fatalf("dispatcher error=%v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for kind, count := range calls {
				if count != 0 {
					t.Fatalf("%s calls=%d, all calls=%v", kind, count, calls)
				}
			}
		})
	}
}

func TestAutomationPermitFailureCleanupDoesNotConsumeFailureBudget(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "permit cleanup", Status: model.TaskStatusReady,
		Runtime: model.RuntimeCodex, Assignee: &assignee, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	if err := cleanupClaimAfterAutomationPermitFailure(
		opened,
		claim,
		1,
	); err != nil {
		t.Fatal(err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != model.RunStatusReclaimed ||
		inspection.Task.FailureCount != 0 ||
		inspection.Task.CurrentRunID != nil ||
		inspection.Task.Status != model.TaskStatusReady {
		t.Fatalf("cleanup result=%+v", inspection)
	}
}

func TestAutomationWorkerReleaseGateSerializesQuarantineActivation(
	t *testing.T,
) {
	ctx := context.Background()
	manager, _ := testManager(t)
	authority, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		ctx,
		authority,
		authority.Close,
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-release-gate",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		_ = session.Shutdown(true)
		t.Fatal(err)
	}
	defer opened.Close()
	activationStore, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		_ = session.Shutdown(true)
		t.Fatal(err)
	}
	defer activationStore.Close()

	type releaseResult struct {
		released bool
		err      error
	}
	entered := make(chan struct{})
	continueRelease := make(chan struct{})
	result := make(chan releaseResult, 1)
	go func() {
		released, releaseErr := session.workerReleaseGate(opened)(
			ctx,
			func() (bool, error) {
				close(entered)
				<-continueRelease
				return true, nil
			},
		)
		result <- releaseResult{released: released, err: releaseErr}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker release did not enter its automation permit")
	}
	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	_, _, activationErr := activationStore.ActivateAutomationQuarantine(
		blockedContext,
		store.AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              "publication",
			SourceID:          "release-gate-publication",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	)
	cancel()
	if !errors.Is(activationErr, context.DeadlineExceeded) {
		t.Fatalf("quarantine crossed a worker release permit: %v", activationErr)
	}
	close(continueRelease)
	select {
	case release := <-result:
		if release.err != nil || !release.released {
			t.Fatalf("worker release result=%+v", release)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker release did not finish")
	}

	gate, activated, err := activationStore.ActivateAutomationQuarantine(
		ctx,
		store.AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              "publication",
			SourceID:          "release-gate-publication",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	)
	if err != nil || !activated || !gate.Active {
		t.Fatalf("activation=%+v changed=%t err=%v", gate, activated, err)
	}
	releaseCalled := false
	if _, err := session.workerReleaseGate(opened)(
		ctx,
		func() (bool, error) {
			releaseCalled = true
			return true, nil
		},
	); !errors.Is(err, store.ErrAutomationQuarantined) {
		t.Fatalf("release behind quarantine error=%v", err)
	}
	if releaseCalled {
		t.Fatal("worker release ran behind active quarantine")
	}
	waitAutomationSessionCancellation(t, canceled)
	if err := session.Shutdown(true); !errors.Is(
		err,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("shutdown error=%v", err)
	}
}
