package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func openAutomationTestStore(t *testing.T) *Store {
	t.Helper()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close Store: %v", err)
		}
	})
	return opened
}

func registerAutomationTestSession(
	t *testing.T,
	opened *Store,
	board string,
	sessionID string,
) AutomationDispatcherSessionLease {
	t.Helper()
	lease, acquired, err := opened.RegisterAutomationDispatcherSession(
		context.Background(),
		board,
		sessionID,
		time.Minute,
	)
	if err != nil || !acquired || lease.leaseToken == "" {
		t.Fatalf("register session = %s, acquired=%v, err=%v", lease, acquired, err)
	}
	return lease
}

func activateAutomationTestSource(
	t *testing.T,
	opened *Store,
	sourceID string,
	claimEpoch string,
) (AutomationQuarantine, AutomationQuarantineSource) {
	t.Helper()
	gate, activated, err := opened.ActivateAutomationQuarantine(
		context.Background(),
		AutomationQuarantineSourceInput{
			Board:              "default",
			Kind:               "publication",
			SourceID:           sourceID,
			ObservedUpdatedAt:  "2026-07-24T00:00:00.000Z",
			ObservedClaimEpoch: claimEpoch,
			DiagnosticCode:     "process_teardown_unconfirmed",
		},
	)
	if err != nil || !activated {
		t.Fatalf("activate quarantine = %+v, activated=%v, err=%v", gate, activated, err)
	}
	sources, err := opened.ListAutomationQuarantineSources(
		context.Background(),
		AutomationQuarantineSourceFilter{
			Board:    "default",
			Kind:     "publication",
			SourceID: sourceID,
		},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("list source = %+v, err=%v", sources, err)
	}
	return gate, sources[0]
}

func automationConfirmation(
	gate AutomationQuarantine,
	sources []AutomationQuarantineSource,
) AutomationQuarantineConfirmation {
	resolutions := make([]AutomationQuarantineSourceResolution, 0, len(sources))
	for _, source := range sources {
		resolutions = append(resolutions, AutomationQuarantineSourceResolution{
			SourceKey:          source.SourceKey,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: source.ObservedClaimEpoch,
			Disposition:        AutomationSourceAbandoned,
		})
	}
	return AutomationQuarantineConfirmation{
		Generation:            gate.Generation,
		Actor:                 "operator",
		Reason:                "verified every host-side writer is stopped",
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources:               resolutions,
	}
}

func TestAutomationQuarantineSerializesPermitAndRotatesOnlyForNewSource(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(t, opened, "default", "dispatcher-one")

	initial, err := opened.GetAutomationQuarantine(ctx)
	if err != nil || initial.Active || initial.Generation != 0 {
		t.Fatalf("initial gate = %+v, err=%v", initial, err)
	}
	permit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.ValidateAutomationPermit(ctx, permit); err != nil {
		t.Fatalf("validate permit: %v", err)
	}

	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, _, blockedErr := opened.ActivateAutomationQuarantine(
		blockedContext,
		AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              "publication",
			SourceID:          "pub-one",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	)
	if !errors.Is(blockedErr, context.DeadlineExceeded) {
		t.Fatalf("exclusive activation behind permit error = %v", blockedErr)
	}
	if strings.Contains(blockedErr.Error(), opened.automation.lockPath) {
		t.Fatalf("lock error exposed its path: %v", blockedErr)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := permit.Close(); err != nil {
		t.Fatalf("idempotent permit close: %v", err)
	}
	if err := opened.ValidateAutomationPermit(ctx, permit); !errors.Is(err, ErrAutomationPermitClosed) {
		t.Fatalf("closed permit validation error = %v", err)
	}

	firstInput := AutomationQuarantineSourceInput{
		Board:             "default",
		Kind:              "publication",
		SourceID:          "pub-one",
		ObservedUpdatedAt: "epoch-one",
		DiagnosticCode:    "process_teardown_unconfirmed",
	}
	first, activated, err := opened.ActivateAutomationQuarantine(ctx, firstInput)
	if err != nil || !activated || !first.Active || first.Generation != 1 ||
		first.ActiveSourceCount != 1 {
		t.Fatalf("first activation = %+v, activated=%v, err=%v", first, activated, err)
	}
	if _, err := opened.AcquireAutomationPermitForSession(ctx, lease); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("permit during quarantine error = %v", err)
	}
	duplicate := firstInput
	duplicate.DiagnosticCode = "updated_diagnostic_is_not_a_new_epoch"
	repeated, activated, err := opened.ActivateAutomationQuarantine(ctx, duplicate)
	if err != nil || activated || repeated.Generation != first.Generation ||
		repeated.ActiveSourceCount != 1 {
		t.Fatalf("duplicate activation = %+v, activated=%v, err=%v", repeated, activated, err)
	}
	secondInput := firstInput
	secondInput.ObservedClaimEpoch = "2"
	second, activated, err := opened.ActivateAutomationQuarantine(ctx, secondInput)
	if err != nil || !activated || second.Generation != 2 ||
		second.ActiveSourceCount != 2 {
		t.Fatalf("new epoch activation = %+v, activated=%v, err=%v", second, activated, err)
	}
}

func TestConcurrentExactAutomationSourceActivationRotatesOnce(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	input := AutomationQuarantineSourceInput{
		Board:              "default",
		Kind:               "publication",
		SourceID:           "pub-concurrent",
		ObservedUpdatedAt:  "epoch-one",
		ObservedClaimEpoch: "1",
		DiagnosticCode:     "process_teardown_unconfirmed",
	}
	const callers = 16
	var wait sync.WaitGroup
	wait.Add(callers)
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			_, activated, err := opened.ActivateAutomationQuarantine(ctx, input)
			results <- activated
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	activations := 0
	for activated := range results {
		if activated {
			activations++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent activation error = %v", err)
		}
	}
	gate, err := opened.GetAutomationQuarantine(ctx)
	if err != nil || activations != 1 || gate.Generation != 1 ||
		gate.ActiveSourceCount != 1 {
		t.Fatalf(
			"concurrent activations=%d gate=%+v err=%v",
			activations,
			gate,
			err,
		)
	}
}

func TestAutomationQuarantineConfirmationRequiresExactSourcesSessionAckAndGuard(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(t, opened, "default", "dispatcher-ack")
	gate, _ := activateAutomationTestSource(t, opened, "pub-ack", "1")
	sources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{ActiveOnly: true, Limit: 1000},
	)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := automationConfirmation(gate, sources)

	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, confirmation); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("confirmation without dispatcher ACK error = %v", err)
	}
	if err := opened.AcknowledgeAutomationQuarantine(
		ctx,
		lease,
		gate.Generation,
	); err != nil {
		t.Fatal(err)
	}
	missing := confirmation
	missing.Sources = nil
	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, missing); !errors.Is(err, ErrAutomationSourceConflict) {
		t.Fatalf("inexact source set error = %v", err)
	}
	guardErr := errors.New("verified worker is still alive")
	guarded := confirmation
	guarded.Guard = func(context.Context, AutomationQuarantineSnapshot) error {
		return guardErr
	}
	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, guarded); !errors.Is(err, guardErr) {
		t.Fatalf("confirmation guard error = %v", err)
	}
	stillActive, err := opened.GetAutomationQuarantine(ctx)
	if err != nil || !stillActive.Active || stillActive.ActiveSourceCount != 1 {
		t.Fatalf("guard changed gate = %+v, err=%v", stillActive, err)
	}

	cleared, changed, err := opened.ConfirmAutomationQuarantine(ctx, confirmation)
	if err != nil || !changed || cleared.Active || cleared.ActiveSourceCount != 0 {
		t.Fatalf("clear = %+v, changed=%v, err=%v", cleared, changed, err)
	}
	repeated, changed, err := opened.ConfirmAutomationQuarantine(ctx, confirmation)
	if err != nil || changed || repeated.Active || repeated.Generation != gate.Generation {
		t.Fatalf("idempotent clear = %+v, changed=%v, err=%v", repeated, changed, err)
	}
	resolved, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board:    "default",
			Kind:     "publication",
			SourceID: "pub-ack",
		},
	)
	if err != nil || len(resolved) != 1 ||
		resolved[0].Disposition != string(AutomationSourceAbandoned) ||
		resolved[0].ResolvedGeneration == nil ||
		*resolved[0].ResolvedGeneration != gate.Generation {
		t.Fatalf("resolved source = %+v, err=%v", resolved, err)
	}
}

func TestAutomationQuarantineConfirmationRetriesAfterPhaseOneCrash(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	gate, source := activateAutomationTestSource(t, opened, "pub-crash", "1")
	confirmation := automationConfirmation(gate, []AutomationQuarantineSource{source})
	crash := errors.New("simulated crash after phase one")
	opened.automationAfterConfirmationPhaseOne = func() error {
		opened.automationAfterConfirmationPhaseOne = nil
		return crash
	}
	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, confirmation); !errors.Is(err, crash) {
		t.Fatalf("phase-one crash error = %v", err)
	}
	pending, err := opened.GetAutomationQuarantine(ctx)
	if err != nil || !pending.Active || !pending.ConfirmationPending ||
		pending.ActiveSourceCount != 0 {
		t.Fatalf("phase-one state = %+v, err=%v", pending, err)
	}
	different := confirmation
	different.Actor = "other-operator"
	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, different); !errors.Is(err, ErrAutomationGateConflict) {
		t.Fatalf("different retry error = %v", err)
	}
	cleared, changed, err := opened.ConfirmAutomationQuarantine(ctx, confirmation)
	if err != nil || !changed || cleared.Active {
		t.Fatalf("phase-one retry = %+v, changed=%v, err=%v", cleared, changed, err)
	}
}

func TestAutomationPermitRejectsExpiredReleasedAndWrongSession(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:   "must not be claimed by a stale supervisor",
		Runtime: model.RuntimeCodex,
		Status:  model.TaskStatusReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := registerAutomationTestSession(t, opened, "default", "dispatcher-stale")
	wrong := lease
	wrong.leaseToken = "not-the-session-token"
	if _, err := opened.AcquireAutomationPermitForSession(ctx, wrong); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("wrong token permit error = %v", err)
	}

	permit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Second).UTC().Format(automationTimestampLayout)
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
		SET expires_at = ? WHERE session_id = ?`, past, lease.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := opened.ValidateAutomationPermit(ctx, permit); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("expired permit validation error = %v", err)
	}
	if claimed, err := opened.ClaimTaskAutomated(ctx, permit, ClaimOptions{
		TaskID: task.Task.ID,
	}); !errors.Is(err, ErrAutomationHostNotIdle) || claimed != nil {
		t.Fatalf("stale automated claim = %+v, err=%v", claimed, err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	if manual, err := opened.ClaimTask(ctx, ClaimOptions{
		TaskID: task.Task.ID,
	}); err != nil || manual == nil {
		t.Fatalf("manual claim was gated = %+v, err=%v", manual, err)
	}

	releaseLease := registerAutomationTestSession(t, opened, "default", "dispatcher-release")
	if released, err := opened.ReleaseAutomationDispatcherSession(ctx, releaseLease); err != nil || !released {
		t.Fatalf("release session = %v, err=%v", released, err)
	}
	if released, err := opened.ReleaseAutomationDispatcherSession(ctx, releaseLease); err != nil || !released {
		t.Fatalf("idempotent release session = %v, err=%v", released, err)
	}
	if _, err := opened.AcquireAutomationPermitForSession(ctx, releaseLease); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("released session permit error = %v", err)
	}

	expired, acquired, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"dispatcher-expired",
		MinAutomationSessionTTL,
	)
	if err != nil || !acquired {
		t.Fatalf("register expired fixture = %s, acquired=%v, err=%v", expired, acquired, err)
	}
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
		SET expires_at = ? WHERE session_id = ?`,
		time.Now().Add(-time.Second).UTC().Format(automationTimestampLayout),
		expired.SessionID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.AcquireAutomationPermitForSession(ctx, expired); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("expired session permit error = %v", err)
	}
}

func TestAutomationDispatcherAckIsDurableAndExpiryAllowsConfirmation(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(t, opened, "default", "dispatcher-audit")
	gate, source := activateAutomationTestSource(t, opened, "pub-audit", "1")
	if err := opened.AcknowledgeAutomationQuarantine(
		ctx,
		lease,
		gate.Generation,
	); err != nil {
		t.Fatal(err)
	}
	if released, err := opened.ReleaseAutomationDispatcherSession(ctx, lease); err != nil || !released {
		t.Fatalf("release = %v, err=%v", released, err)
	}
	var acknowledgements int
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_dispatcher_acks
		WHERE session_id = ? AND generation = ?`,
		lease.SessionID,
		gate.Generation,
	).Scan(&acknowledgements); err != nil || acknowledgements != 1 {
		t.Fatalf("durable ACK count = %d, err=%v", acknowledgements, err)
	}
	confirmation := automationConfirmation(gate, []AutomationQuarantineSource{source})
	if _, changed, err := opened.ConfirmAutomationQuarantine(ctx, confirmation); err != nil || !changed {
		t.Fatalf("confirmation after release changed=%v, err=%v", changed, err)
	}

	expiring, acquired, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"dispatcher-expiry-ack",
		MinAutomationSessionTTL,
	)
	if err != nil || !acquired {
		t.Fatalf("register expiring session = %s, acquired=%v, err=%v", expiring, acquired, err)
	}
	if _, err := opened.db.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
		SET expires_at = ? WHERE session_id = ?`,
		time.Now().Add(-time.Second).UTC().Format(automationTimestampLayout),
		expiring.SessionID,
	); err != nil {
		t.Fatal(err)
	}
	nextGate, nextSource := activateAutomationTestSource(t, opened, "pub-expiry", "2")
	expiredConfirmation := automationConfirmation(
		nextGate,
		[]AutomationQuarantineSource{nextSource},
	)
	if _, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		expiredConfirmation,
	); err != nil || !changed {
		t.Fatalf("confirmation after session expiry changed=%v, err=%v", changed, err)
	}
}

func TestAutomationCapabilitiesDoNotRenderSecrets(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(t, opened, "default", "dispatcher-secret")
	permit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	for label, rendered := range map[string]string{
		"permit":  fmt.Sprintf("%+v %#v", permit, permit),
		"session": fmt.Sprintf("%+v %#v", lease, lease),
		"configuration": fmt.Sprintf("%+v %#v", AutomationGateConfig{
			AuthorityDBPath: "/private/authority.db",
		}, AutomationGateConfig{
			AuthorityDBPath: "/private/authority.db",
		}),
	} {
		if strings.Contains(rendered, lease.leaseToken) ||
			strings.Contains(rendered, opened.automation.lockPath) ||
			strings.Contains(rendered, "/private/") {
			t.Fatalf("%s rendered a secret: %s", label, rendered)
		}
	}
	encoded, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), lease.leaseToken) {
		t.Fatalf("session JSON rendered its token: %s", encoded)
	}
	encoded, err = json.Marshal(AutomationGateConfig{
		AuthorityDBPath: "/private/authority.db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/private/") {
		t.Fatalf("automation configuration JSON rendered a path: %s", encoded)
	}
}

func TestAutomationGateRejectsUnboundedAndIncompleteInputs(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	if _, _, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:          "default",
			Kind:           "publication",
			SourceID:       "missing-epoch",
			DiagnosticCode: "process_teardown_unconfirmed",
		},
	); err == nil {
		t.Fatal("source without an observed epoch was accepted")
	}
	if _, _, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              "publication",
			SourceID:          strings.Repeat("x", maxAutomationSourceIDBytes+1),
			ObservedUpdatedAt: "epoch",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	); err == nil {
		t.Fatal("unbounded source ID was accepted")
	}
	if _, _, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:              "default",
			Kind:               "publication",
			SourceID:           "raw-token",
			ObservedClaimEpoch: "secret-claim-token",
			DiagnosticCode:     "process_teardown_unconfirmed",
		},
	); err == nil {
		t.Fatal("a raw claim token was accepted as a public claim epoch")
	}
	if _, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board: strings.Repeat("b", maxAutomationBoardBytes+1),
		},
	); err == nil {
		t.Fatal("unbounded source filter was accepted")
	}
	gate, source := activateAutomationTestSource(t, opened, "pub-proof", "1")
	if _, _, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"late-dispatcher",
		time.Minute,
	); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("dispatcher registered during quarantine: %v", err)
	}
	incomplete := automationConfirmation(gate, []AutomationQuarantineSource{source})
	incomplete.HelpersStopped = false
	if _, _, err := opened.ConfirmAutomationQuarantine(ctx, incomplete); err == nil {
		t.Fatal("confirmation without both quiescence attestations was accepted")
	}
	if _, _, err := opened.RegisterAutomationDispatcherSession(
		ctx,
		"default",
		"invalid-ttl",
		MinAutomationSessionTTL-time.Nanosecond,
	); err == nil {
		t.Fatal("too-short dispatcher session was accepted")
	}
}

func TestSchema25AddsAutomationQuarantineAuthority(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.db.ExecContext(ctx, `
		DROP TABLE automation_dispatcher_acks;
		DROP TABLE automation_dispatcher_sessions;
		DROP TABLE automation_quarantine_sources;
		DROP TABLE automation_quarantine_gate;
		PRAGMA user_version = 24;
	`); err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version, tables, releasedAtColumns int
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN (
			'automation_quarantine_gate',
			'automation_quarantine_sources',
			'automation_dispatcher_sessions',
			'automation_dispatcher_acks'
		)`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(
		'automation_dispatcher_sessions'
	) WHERE name = 'released_at'`).Scan(&releasedAtColumns); err != nil {
		t.Fatal(err)
	}
	gate, err := reopened.GetAutomationQuarantine(ctx)
	if err != nil || schemaVersion != 25 || version != schemaVersion ||
		tables != 4 || releasedAtColumns != 1 ||
		gate.Active || gate.Generation != 0 {
		t.Fatalf(
			"migration version=%d tables=%d releasedAt=%d gate=%+v err=%v",
			version,
			tables,
			releasedAtColumns,
			gate,
			err,
		)
	}
}
