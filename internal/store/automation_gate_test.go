package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

const automationTestSourceKind = "automation_test_source"

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

func copyAutomationPermitForTest(permit *AutomationPermit) *AutomationPermit {
	copied := reflect.New(reflect.TypeOf(permit).Elem())
	copied.Elem().Set(reflect.ValueOf(permit).Elem())
	return copied.Interface().(*AutomationPermit)
}

type automationPermitQueryHook struct {
	querier
	afterQueryRow func()
}

func (q automationPermitQueryHook) QueryRowContext(
	ctx context.Context,
	query string,
	args ...any,
) *sql.Row {
	row := q.querier.QueryRowContext(ctx, query, args...)
	if q.afterQueryRow != nil {
		q.afterQueryRow()
	}
	return row
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
			Kind:               automationTestSourceKind,
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
			Kind:     automationTestSourceKind,
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
			Kind:              automationTestSourceKind,
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
		Kind:              automationTestSourceKind,
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

func TestEnsureAutomationQuarantineSourceDistinguishesResolvedSource(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	input := AutomationQuarantineSourceInput{
		Board:              "default",
		Kind:               automationTestSourceKind,
		SourceID:           "publication-lifecycle",
		ObservedUpdatedAt:  "epoch-one",
		ObservedClaimEpoch: "1",
		DiagnosticCode:     "process_teardown_unconfirmed",
	}

	first, outcome, err := opened.EnsureAutomationQuarantineSource(ctx, input)
	if err != nil ||
		outcome != AutomationQuarantineSourceCreated ||
		!first.Active ||
		first.Generation != 1 ||
		first.ActiveSourceCount != 1 {
		t.Fatalf("created source: gate=%+v outcome=%q err=%v", first, outcome, err)
	}

	validatorCalls := 0
	duplicate := input
	duplicate.ValidateCurrent = func(
		context.Context,
		AutomationQuarantineSourceInput,
	) (bool, error) {
		validatorCalls++
		return false, nil
	}
	existing, outcome, err := opened.EnsureAutomationQuarantineSource(
		ctx,
		duplicate,
	)
	if err != nil ||
		outcome != AutomationQuarantineSourceExistingActive ||
		existing.Generation != first.Generation ||
		existing.ActiveSourceCount != 1 ||
		validatorCalls != 0 {
		t.Fatalf(
			"existing active source: gate=%+v outcome=%q calls=%d err=%v",
			existing,
			outcome,
			validatorCalls,
			err,
		)
	}

	sources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board:    input.Board,
			Kind:     input.Kind,
			SourceID: input.SourceID,
		},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("list active source=%+v err=%v", sources, err)
	}
	cleared, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		automationConfirmation(first, sources),
	)
	if err != nil || !changed || cleared.Active {
		t.Fatalf("resolve source: gate=%+v changed=%v err=%v", cleared, changed, err)
	}

	otherInput := input
	otherInput.SourceID = "other-publication"
	otherInput.ObservedUpdatedAt = "epoch-two"
	otherInput.ObservedClaimEpoch = "2"
	other, outcome, err := opened.EnsureAutomationQuarantineSource(
		ctx,
		otherInput,
	)
	if err != nil ||
		outcome != AutomationQuarantineSourceCreated ||
		!other.Active ||
		other.Generation != 2 ||
		other.ActiveSourceCount != 1 {
		t.Fatalf("other source: gate=%+v outcome=%q err=%v", other, outcome, err)
	}

	validatorCalls = 0
	resolved, outcome, err := opened.EnsureAutomationQuarantineSource(
		ctx,
		duplicate,
	)
	if err != nil ||
		outcome != AutomationQuarantineSourceExistingResolved ||
		!resolved.Active ||
		resolved.Generation != other.Generation ||
		resolved.ActiveSourceCount != 1 ||
		validatorCalls != 0 {
		t.Fatalf(
			"existing resolved source: gate=%+v outcome=%q calls=%d err=%v",
			resolved,
			outcome,
			validatorCalls,
			err,
		)
	}

	legacy, activated, err := opened.ActivateAutomationQuarantine(ctx, duplicate)
	if err != nil ||
		activated ||
		legacy.Generation != other.Generation ||
		legacy.ActiveSourceCount != 1 ||
		validatorCalls != 0 {
		t.Fatalf(
			"legacy resolved activation: gate=%+v activated=%v calls=%d err=%v",
			legacy,
			activated,
			validatorCalls,
			err,
		)
	}
	resolvedSources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board:    input.Board,
			Kind:     input.Kind,
			SourceID: input.SourceID,
		},
	)
	if err != nil ||
		len(resolvedSources) != 1 ||
		resolvedSources[0].Disposition != string(AutomationSourceAbandoned) {
		t.Fatalf("resolved source changed=%+v err=%v", resolvedSources, err)
	}
}

func TestAutomationQuarantineRecoverySnapshotUsesExactPhaseOneSourceSet(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"dispatcher-recovery-snapshot",
	)
	gate, source := activateAutomationTestSource(
		t,
		opened,
		"publication-recovery-snapshot",
		"1",
	)
	activeSnapshot, err := opened.GetAutomationQuarantineRecoverySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if activeSnapshot.UnacknowledgedSessionCount != 1 {
		t.Fatalf("active recovery snapshot = %+v", activeSnapshot)
	}
	if err := opened.AcknowledgeAutomationQuarantine(
		ctx,
		lease,
		gate.Generation,
	); err != nil {
		t.Fatal(err)
	}
	phaseOneStop := errors.New("stop after phase one")
	opened.automationAfterConfirmationPhaseOne = func() error {
		return phaseOneStop
	}
	confirmation := automationConfirmation(
		gate,
		[]AutomationQuarantineSource{source},
	)
	if _, _, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	); !errors.Is(err, phaseOneStop) {
		t.Fatalf("phase-one confirmation error = %v", err)
	}

	snapshot, err := opened.GetAutomationQuarantineRecoverySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Gate.Active ||
		snapshot.Gate.Generation != gate.Generation ||
		snapshot.Gate.ActiveSourceCount != 0 ||
		!snapshot.Gate.ConfirmationPending ||
		!snapshot.Confirmation.Pending ||
		snapshot.Confirmation.Actor == nil ||
		*snapshot.Confirmation.Actor != confirmation.Actor ||
		snapshot.Confirmation.Reason == nil ||
		*snapshot.Confirmation.Reason != confirmation.Reason ||
		!snapshot.Confirmation.HelpersStopped ||
		!snapshot.Confirmation.ExternalWritesStopped ||
		len(snapshot.Sources) != 1 ||
		snapshot.Sources[0].SourceKey != source.SourceKey ||
		snapshot.Sources[0].ResolvedGeneration == nil ||
		*snapshot.Sources[0].ResolvedGeneration != gate.Generation ||
		snapshot.UnacknowledgedSessionCount != 0 {
		t.Fatalf("phase-one recovery snapshot = %+v", snapshot)
	}
	record, err := readAutomationGate(ctx, opened.db)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), record.PermitToken) ||
		strings.Contains(string(encoded), opened.automation.lockPath) ||
		strings.Contains(string(encoded), "leaseToken") {
		t.Fatalf("recovery snapshot leaked a credential: %s", encoded)
	}
}

func TestConcurrentExactAutomationSourceActivationRotatesOnce(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	input := AutomationQuarantineSourceInput{
		Board:              "default",
		Kind:               automationTestSourceKind,
		SourceID:           "pub-concurrent",
		ObservedUpdatedAt:  "epoch-one",
		ObservedClaimEpoch: "1",
		DiagnosticCode:     "process_teardown_unconfirmed",
		ValidateCurrent: func(
			context.Context,
			AutomationQuarantineSourceInput,
		) (bool, error) {
			return true, nil
		},
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

func TestPublicationQuarantineActivationRejectsTerminalizedAndDeletedTuple(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	claimed := claimPublicationForRecovery(t, opened, "stale_activation")
	input := AutomationQuarantineSourceInput{
		Board:              opened.board,
		Kind:               "publication",
		SourceID:           claimed.ID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: strconv.FormatInt(claimed.ClaimEpoch, 10),
		DiagnosticCode:     "process_teardown_unconfirmed",
		ValidateCurrent:    opened.ValidatePublishingAutomationSource,
	}
	if _, err := opened.FailPublication(
		ctx,
		claimed.ID,
		FailPublicationInput{
			ExpectedUpdatedAt: claimed.UpdatedAt,
			ClaimToken:        claimed.ClaimToken,
			ClaimEpoch:        claimed.ClaimEpoch,
			Error:             "publication process stopped",
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := opened.DeleteTask(ctx, claimed.TaskID); err != nil {
		t.Fatalf("delete terminalized publication task: %v", err)
	}

	gate, activated, err := opened.ActivateAutomationQuarantine(ctx, input)
	if !errors.Is(err, ErrAutomationSourceStale) || activated ||
		gate.Active || gate.Generation != 0 {
		t.Fatalf("stale activation: gate=%+v activated=%v err=%v",
			gate, activated, err)
	}
	var stale *AutomationSourceStaleError
	if !errors.As(err, &stale) ||
		stale.Board != input.Board ||
		stale.Kind != input.Kind ||
		stale.SourceID != input.SourceID {
		t.Fatalf("typed stale error=%#v err=%v", stale, err)
	}
	sources, listErr := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{
			Board:    input.Board,
			Kind:     input.Kind,
			SourceID: input.SourceID,
		},
	)
	if listErr != nil || len(sources) != 0 {
		t.Fatalf("stale activation sources=%+v err=%v", sources, listErr)
	}
	encoded, encodeErr := json.Marshal(input)
	if encodeErr != nil || strings.Contains(string(encoded), "ValidateCurrent") {
		t.Fatalf("source validator JSON=%s err=%v", encoded, encodeErr)
	}
}

func TestPublicationQuarantineActivationSerializesExactValidatorAndMutations(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	claimed := claimPublicationForRecovery(t, opened, "atomic_activation")
	_, pendingChangeSet := createPublicationSource(
		t,
		opened,
		"atomic_pending_claim",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pendingClaim, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(pendingChangeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	validatorEntered := make(chan struct{})
	releaseValidator := make(chan struct{})
	input := AutomationQuarantineSourceInput{
		Board:              opened.board,
		Kind:               "publication",
		SourceID:           claimed.ID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: strconv.FormatInt(claimed.ClaimEpoch, 10),
		DiagnosticCode:     "process_teardown_unconfirmed",
		ValidateCurrent: func(
			validatorContext context.Context,
			source AutomationQuarantineSourceInput,
		) (bool, error) {
			exact, err := opened.ValidatePublishingAutomationSource(
				validatorContext,
				source,
			)
			close(validatorEntered)
			<-releaseValidator
			return exact, err
		},
	}
	type activationResult struct {
		gate      AutomationQuarantine
		activated bool
		err       error
	}
	activationDone := make(chan activationResult, 1)
	go func() {
		gate, activated, err := opened.ActivateAutomationQuarantine(
			ctx,
			input,
		)
		activationDone <- activationResult{gate, activated, err}
	}()
	select {
	case <-validatorEntered:
	case <-time.After(time.Second):
		t.Fatal("publication validator did not run")
	}

	terminalDone := make(chan error, 1)
	go func() {
		_, err := opened.FailPublication(
			ctx,
			claimed.ID,
			FailPublicationInput{
				ExpectedUpdatedAt: claimed.UpdatedAt,
				ClaimToken:        claimed.ClaimToken,
				ClaimEpoch:        claimed.ClaimEpoch,
				Error:             "must wait behind quarantine activation",
			},
		)
		terminalDone <- err
	}()
	type claimResult struct {
		acquired bool
		err      error
	}
	claimDone := make(chan claimResult, 1)
	go func() {
		_, acquired, err := opened.ClaimPublication(
			ctx,
			pendingClaim.ID,
			ClaimPublicationInput{
				ExpectedUpdatedAt: pendingClaim.UpdatedAt,
				TTL:               time.Minute,
			},
		)
		claimDone <- claimResult{acquired: acquired, err: err}
	}()
	select {
	case err := <-terminalDone:
		close(releaseValidator)
		t.Fatalf("terminalization overlapped source validation: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case result := <-claimDone:
		close(releaseValidator)
		t.Fatalf("claim overlapped source validation: %+v", result)
	default:
	}
	close(releaseValidator)
	activation := <-activationDone
	if activation.err != nil || !activation.activated ||
		!activation.gate.Active {
		t.Fatalf("atomic activation=%+v", activation)
	}
	if err := <-terminalDone; !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("terminalization after activation error=%v", err)
	}
	if result := <-claimDone; !errors.Is(
		result.err,
		ErrAutomationQuarantined,
	) || result.acquired {
		t.Fatalf("claim after activation result=%+v", result)
	}
	preservedPending, err := opened.GetPublication(ctx, pendingClaim.ID)
	if err != nil ||
		preservedPending.Status != model.PublicationPending ||
		preservedPending.ClaimEpoch != pendingClaim.ClaimEpoch ||
		preservedPending.UpdatedAt != pendingClaim.UpdatedAt {
		t.Fatalf(
			"pending claim changed behind activation: value=%+v err=%v",
			preservedPending,
			err,
		)
	}
	if err := opened.DeleteTask(ctx, claimed.TaskID); !errors.Is(
		err,
		ErrAutomationQuarantined,
	) {
		t.Fatalf("delete after activation error=%v", err)
	}

	validatorCalls := 0
	duplicate := input
	duplicate.ValidateCurrent = func(
		context.Context,
		AutomationQuarantineSourceInput,
	) (bool, error) {
		validatorCalls++
		return false, nil
	}
	repeated, activated, err := opened.ActivateAutomationQuarantine(
		ctx,
		duplicate,
	)
	if err != nil || activated || !repeated.Active || validatorCalls != 0 {
		t.Fatalf("duplicate activation: gate=%+v activated=%v calls=%d err=%v",
			repeated, activated, validatorCalls, err)
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
			Kind:     automationTestSourceKind,
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

func TestAutomationQuarantineFencesDestructiveMutations(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "preserve recovery evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	gate, source := activateAutomationTestSource(
		t,
		opened,
		"destructive-mutation",
		"1",
	)
	confirmation := automationConfirmation(
		gate,
		[]AutomationQuarantineSource{source},
	)

	invalid := confirmation
	invalid.Sources = append(
		[]AutomationQuarantineSourceResolution(nil),
		confirmation.Sources...,
	)
	invalid.Sources[0].Outcome = PublicationRecoveryFailed
	if _, _, err := opened.ConfirmAutomationQuarantine(
		ctx,
		invalid,
	); !errors.Is(err, ErrAutomationRecoveryScope) {
		t.Fatalf("non-publication recovery result error=%v", err)
	}
	if err := opened.DeleteTask(
		ctx,
		task.Task.ID,
	); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("task deletion during quarantine error=%v", err)
	}
	if _, err := opened.AcquireBoardRemovalGuard(
		ctx,
		"alpha",
	); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("board removal during quarantine error=%v", err)
	}
	callbackRan := false
	if err := opened.RunWithAutomationGateOpen(ctx, func() error {
		callbackRan = true
		return nil
	}); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("cross-store removal during quarantine error=%v", err)
	}
	if callbackRan {
		t.Fatal("quarantined cross-store removal callback ran")
	}
	if _, err := opened.GetTask(ctx, task.Task.ID); err != nil {
		t.Fatalf("quarantine did not preserve task: %v", err)
	}

	if _, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	); err != nil || !changed {
		t.Fatalf("clear quarantine: changed=%v err=%v", changed, err)
	}
	callbackErr := errors.New("stop removal")
	if err := opened.RunWithAutomationGateOpen(ctx, func() error {
		callbackRan = true
		return callbackErr
	}); !errors.Is(err, callbackErr) {
		t.Fatalf("cross-store callback error=%v", err)
	}
	if !callbackRan {
		t.Fatal("open-gate cross-store callback did not run")
	}
	guard, err := opened.AcquireBoardRemovalGuard(ctx, "alpha")
	if err != nil {
		t.Fatalf("acquire board removal guard after clear: %v", err)
	}
	if released, err := opened.ReleaseBoardRemovalGuard(
		ctx,
		guard,
	); err != nil || !released {
		t.Fatalf("release board removal guard: released=%v err=%v", released, err)
	}
	if err := opened.DeleteTask(ctx, task.Task.ID); err != nil {
		t.Fatalf("delete task after clear: %v", err)
	}
}

func TestAutomationConfirmationEvidenceBindsInactivePublicationReplay(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	gate, activated, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:              "default",
			Kind:               "publication",
			SourceID:           "pub_noop_guard",
			ObservedUpdatedAt:  "2026-07-24T01:00:00.000Z",
			ObservedClaimEpoch: "1",
			DiagnosticCode:     "process_teardown_unconfirmed",
			ValidateCurrent: func(
				context.Context,
				AutomationQuarantineSourceInput,
			) (bool, error) {
				return true, nil
			},
		},
	)
	if err != nil || !activated {
		t.Fatalf("activate publication source: gate=%+v activated=%v err=%v",
			gate, activated, err)
	}
	sources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{ActiveOnly: true, Limit: 1000},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("publication sources=%+v err=%v", sources, err)
	}
	resultURL := "https://example.test/pulls/noop"
	guardCalls := 0
	confirmation := AutomationQuarantineConfirmation{
		Generation:            gate.Generation,
		Actor:                 "operator",
		Reason:                "all external writers are stopped",
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources: []AutomationQuarantineSourceResolution{{
			SourceKey:          sources[0].SourceKey,
			ObservedUpdatedAt:  sources[0].ObservedUpdatedAt,
			ObservedClaimEpoch: sources[0].ObservedClaimEpoch,
			Disposition:        AutomationSourceSuperseded,
			Outcome:            PublicationRecoveryPublished,
			ResultURL:          &resultURL,
		}},
		Guard: func(
			context.Context,
			AutomationQuarantineSnapshot,
		) error {
			guardCalls++
			return nil
		},
	}
	cleared, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	)
	if err != nil || !changed || cleared.Active || guardCalls != 1 {
		t.Fatalf("initial no-op confirmation: gate=%+v changed=%v calls=%d err=%v",
			cleared, changed, guardCalls, err)
	}

	tests := []struct {
		name   string
		mutate func(*AutomationQuarantineSourceResolution)
	}{
		{
			name: "outcome",
			mutate: func(value *AutomationQuarantineSourceResolution) {
				value.Outcome = PublicationRecoverySuperseded
				value.ResultURL = nil
			},
		},
		{
			name: "result URL",
			mutate: func(value *AutomationQuarantineSourceResolution) {
				changedURL := "https://example.test/pulls/changed"
				value.ResultURL = &changedURL
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedConfirmation := confirmation
			changedConfirmation.Sources = append(
				[]AutomationQuarantineSourceResolution(nil),
				confirmation.Sources...,
			)
			test.mutate(&changedConfirmation.Sources[0])
			if _, _, err := opened.ConfirmAutomationQuarantine(
				ctx,
				changedConfirmation,
			); !errors.Is(err, ErrAutomationGateConflict) {
				t.Fatalf("changed inactive replay error=%v", err)
			}
			if guardCalls != 1 {
				t.Fatalf("changed inactive replay called Guard %d time(s)", guardCalls)
			}
		})
	}

	replayed, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	)
	if err != nil || changed || replayed.Active || guardCalls != 2 {
		t.Fatalf("exact inactive replay: gate=%+v changed=%v calls=%d err=%v",
			replayed, changed, guardCalls, err)
	}
}

func TestAutomationConfirmationEvidenceSchemaIsStrictAndImmutable(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	gate, source := activateAutomationTestSource(
		t,
		opened,
		"immutable-confirmation",
		"1",
	)
	confirmation := automationConfirmation(
		gate,
		[]AutomationQuarantineSource{source},
	)
	if _, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	); err != nil || !changed {
		t.Fatalf("record confirmation evidence: changed=%v err=%v", changed, err)
	}

	var digest, recordedAt string
	if err := opened.db.QueryRowContext(ctx, `
		SELECT confirmation_digest, recorded_at
		FROM automation_quarantine_confirmation_evidence
		WHERE generation = ?
	`, gate.Generation).Scan(&digest, &recordedAt); err != nil {
		t.Fatal(err)
	}
	expectedDigest, err := automationConfirmationDigest(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	if digest != expectedDigest || len(digest) != 64 || recordedAt == "" {
		t.Fatalf("confirmation evidence digest=%q recordedAt=%q", digest, recordedAt)
	}
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE automation_quarantine_confirmation_evidence
		SET confirmation_digest = ?
		WHERE generation = ?
	`, strings.Repeat("0", 64), gate.Generation); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("confirmation evidence update error=%v", err)
	}
	if _, err := opened.db.ExecContext(ctx, `
		DELETE FROM automation_quarantine_confirmation_evidence
		WHERE generation = ?
	`, gate.Generation); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("confirmation evidence delete error=%v", err)
	}
	for _, test := range []struct {
		name       string
		generation any
		digest     string
		recordedAt string
	}{
		{
			name:       "fractional generation",
			generation: 1.5,
			digest:     strings.Repeat("0", 64),
			recordedAt: now(),
		},
		{
			name:       "uppercase digest",
			generation: 1001,
			digest:     strings.Repeat("A", 64),
			recordedAt: now(),
		},
		{
			name:       "empty timestamp",
			generation: 1002,
			digest:     strings.Repeat("0", 64),
			recordedAt: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := opened.db.ExecContext(ctx, `
				INSERT INTO automation_quarantine_confirmation_evidence(
					generation, confirmation_digest, recorded_at
				) VALUES (?, ?, ?)
			`, test.generation, test.digest, test.recordedAt); err == nil {
				t.Fatal("strict confirmation evidence schema accepted invalid row")
			}
		})
	}
	var triggerCount int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name IN (
			'automation_confirmation_evidence_prevent_update',
			'automation_confirmation_evidence_prevent_delete'
		)
	`).Scan(&triggerCount); err != nil || triggerCount != 2 {
		t.Fatalf("confirmation evidence triggers=%d err=%v", triggerCount, err)
	}
}

func TestAutomationQuarantineFencesPublicationCompletion(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Store, model.Publication) error
	}{
		{
			name: "complete",
			mutate: func(
				ctx context.Context,
				opened *Store,
				claimed model.Publication,
			) error {
				_, err := opened.CompletePublication(
					ctx,
					claimed.ID,
					CompletePublicationInput{
						ExpectedUpdatedAt: claimed.UpdatedAt,
						ClaimToken:        claimed.ClaimToken,
						ClaimEpoch:        claimed.ClaimEpoch,
					},
				)
				return err
			},
		},
		{
			name: "fail",
			mutate: func(
				ctx context.Context,
				opened *Store,
				claimed model.Publication,
			) error {
				_, err := opened.FailPublication(
					ctx,
					claimed.ID,
					FailPublicationInput{
						ExpectedUpdatedAt: claimed.UpdatedAt,
						ClaimToken:        claimed.ClaimToken,
						ClaimEpoch:        claimed.ClaimEpoch,
						Error:             "must remain publishing",
					},
				)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			opened := openAutomationTestStore(t)
			_, changeSet := createPublicationSource(
				t,
				opened,
				"quarantine_"+test.name,
				model.WorkflowRoleFinalizer,
				model.TaskStatusDone,
				model.RunStatusCompleted,
				"ready",
			)
			pending, _, err := opened.EnsurePublication(
				ctx,
				publicationPolicyInput(changeSet.ID, false),
			)
			if err != nil {
				t.Fatal(err)
			}
			claimed, acquired, err := opened.ClaimPublication(
				ctx,
				pending.ID,
				ClaimPublicationInput{
					ExpectedUpdatedAt: pending.UpdatedAt,
					TTL:               time.Minute,
				},
			)
			if err != nil || !acquired {
				t.Fatalf(
					"claim publication = %+v, acquired=%v, err=%v",
					claimed,
					acquired,
					err,
				)
			}
			activateAutomationTestSource(
				t,
				opened,
				"publication-"+test.name,
				"1",
			)

			if err := test.mutate(
				ctx,
				opened,
				claimed,
			); !errors.Is(err, ErrAutomationQuarantined) {
				t.Fatalf(
					"%s publication behind quarantine error = %v",
					test.name,
					err,
				)
			}
			var status model.PublicationStatus
			var claimToken, updatedAt string
			var claimEpoch int64
			if err := opened.db.QueryRowContext(
				ctx,
				`SELECT status, claim_token, claim_epoch, updated_at
				 FROM publications WHERE id = ?`,
				claimed.ID,
			).Scan(
				&status,
				&claimToken,
				&claimEpoch,
				&updatedAt,
			); err != nil {
				t.Fatal(err)
			}
			if status != model.PublicationPublishing ||
				claimToken != claimed.ClaimToken ||
				claimEpoch != claimed.ClaimEpoch ||
				updatedAt != claimed.UpdatedAt {
				t.Fatalf(
					"quarantine changed publishing evidence: status=%s epoch=%d updated=%s",
					status,
					claimEpoch,
					updatedAt,
				)
			}
		})
	}
}

func TestAutomationQuarantineFencesPublicationClaim(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, changeSet := createPublicationSource(
		t,
		opened,
		"quarantine_claim",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	activateAutomationTestSource(
		t,
		opened,
		"publication-claim",
		"1",
	)

	if _, acquired, err := opened.ClaimPublication(
		ctx,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	); !errors.Is(err, ErrAutomationQuarantined) || acquired {
		t.Fatalf(
			"claim behind quarantine = acquired=%v, err=%v",
			acquired,
			err,
		)
	}
	var status model.PublicationStatus
	var claimToken, claimExpiry sql.NullString
	var claimEpoch int64
	var updatedAt string
	if err := opened.db.QueryRowContext(
		ctx,
		`SELECT status, claim_token, claim_expires_at, claim_epoch, updated_at
		 FROM publications WHERE id = ?`,
		pending.ID,
	).Scan(
		&status,
		&claimToken,
		&claimExpiry,
		&claimEpoch,
		&updatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if status != model.PublicationPending ||
		claimToken.Valid ||
		claimExpiry.Valid ||
		claimEpoch != pending.ClaimEpoch ||
		updatedAt != pending.UpdatedAt {
		t.Fatalf(
			"quarantine changed pending publication: status=%s epoch=%d updated=%s",
			status,
			claimEpoch,
			updatedAt,
		)
	}
}

func TestAutomationGateActivationWaitsForCrossStoreMutation(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- opened.RunWithAutomationGateOpen(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cross-store mutation did not acquire the automation lock")
	}

	input := AutomationQuarantineSourceInput{
		Board:              "default",
		Kind:               automationTestSourceKind,
		SourceID:           "cross-store-mutation",
		ObservedUpdatedAt:  "2026-07-24T00:00:00.000Z",
		ObservedClaimEpoch: "1",
		DiagnosticCode:     "process_teardown_unconfirmed",
	}
	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, _, err := opened.ActivateAutomationQuarantine(
		blockedContext,
		input,
	); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-callbackDone
		t.Fatalf("activation overlapped cross-store mutation: %v", err)
	}
	close(release)
	if err := <-callbackDone; err != nil {
		t.Fatalf("cross-store mutation callback: %v", err)
	}
	gate, activated, err := opened.ActivateAutomationQuarantine(ctx, input)
	if err != nil || !activated || !gate.Active {
		t.Fatalf(
			"activation after cross-store mutation = %+v, activated=%v, err=%v",
			gate,
			activated,
			err,
		)
	}
}

func TestPublishingDeleteFencePrecedesLaterQuarantineActivation(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	task, changeSet := createPublicationSource(
		t,
		opened,
		"delete_activation_interleaving",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if err != nil || !acquired {
		t.Fatalf(
			"claim publication = %+v, acquired=%v, err=%v",
			claimed,
			acquired,
			err,
		)
	}

	entered := make(chan struct{})
	releaseDelete := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- opened.RunWithAutomationGateOpen(ctx, func() error {
			close(entered)
			<-releaseDelete
			return opened.DeleteTask(ctx, task.ID)
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delete fence did not acquire the automation lock")
	}

	activation := AutomationQuarantineSourceInput{
		Board:              "default",
		Kind:               "publication",
		SourceID:           claimed.ID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: fmt.Sprintf("%d", claimed.ClaimEpoch),
		DiagnosticCode:     "process_teardown_unconfirmed",
		ValidateCurrent:    opened.ValidatePublishingAutomationSource,
	}
	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, _, err := opened.ActivateAutomationQuarantine(
		blockedContext,
		activation,
	); !errors.Is(err, context.DeadlineExceeded) {
		close(releaseDelete)
		<-deleteDone
		t.Fatalf("activation overlapped the destructive fence: %v", err)
	}
	close(releaseDelete)
	if err := <-deleteDone; !errors.Is(
		err,
		ErrPublicationStateConflict,
	) {
		t.Fatalf("publishing delete fence error = %v", err)
	}
	gate, activated, err := opened.ActivateAutomationQuarantine(
		ctx,
		activation,
	)
	if err != nil || !activated || !gate.Active {
		t.Fatalf(
			"activation after rejected delete = %+v, activated=%v, err=%v",
			gate,
			activated,
			err,
		)
	}
	if _, err := opened.GetTask(ctx, task.ID); err != nil {
		t.Fatalf("rejected delete lost its task: %v", err)
	}
	preserved, err := opened.GetPublication(ctx, claimed.ID)
	if err != nil || preserved.Status != model.PublicationPublishing {
		t.Fatalf(
			"rejected delete lost publishing evidence = %+v, err=%v",
			preserved,
			err,
		)
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
	if released, err := opened.ReleaseAutomationDispatcherSession(
		ctx,
		lease,
	); err != nil || !released {
		t.Fatalf("release expired session = %v, err=%v", released, err)
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

func TestAutomationPermitExpiryUsesPostQueryBoundary(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"dispatcher-query-boundary",
	)
	expiresAt := time.Date(
		2026,
		time.July,
		24,
		12,
		0,
		0,
		0,
		time.UTC,
	)
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE automation_dispatcher_sessions
		SET expires_at = ?
		WHERE session_id = ?
	`, expiresAt.Format(automationTimestampLayout), lease.SessionID); err != nil {
		t.Fatal(err)
	}

	boundary := expiresAt.Add(-time.Second)
	queryCompleted := false
	clockCalls := 0
	query := automationPermitQueryHook{
		querier: opened.automation.authorityDB,
		afterQueryRow: func() {
			// Advancing this injected boundary models an authority query that
			// begins while the lease is live and completes after it expires.
			boundary = expiresAt
			queryCompleted = true
		},
	}
	_, _, err := readAutomationPermitState(
		ctx,
		query,
		lease,
		func() time.Time {
			clockCalls++
			if !queryCompleted {
				t.Fatal("expiry boundary was sampled before the authority query")
			}
			return boundary
		},
	)
	if !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("post-query expiry error = %v", err)
	}
	if clockCalls != 1 {
		t.Fatalf("boundary clock calls = %d, want 1", clockCalls)
	}
}

func TestAutomationPermitExactExpiryValidation(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"dispatcher-exact-expiry",
	)
	permit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := permit.Close(); err != nil {
			t.Errorf("close permit: %v", err)
		}
	}()

	permit.mu.Lock()
	expiresAt, err := opened.validateAutomationPermitLockedWithExpiry(
		ctx,
		permit,
	)
	permit.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := time.Parse(automationTimestampLayout, lease.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(expected) {
		t.Fatalf("exact expiry = %s, want %s", expiresAt, expected)
	}
	if err := validateAutomationSessionExpiry(
		expiresAt,
		expiresAt.Add(-time.Nanosecond),
	); err != nil {
		t.Fatalf("live boundary validation: %v", err)
	}
	if err := validateAutomationSessionExpiry(
		expiresAt,
		expiresAt,
	); !errors.Is(err, ErrAutomationHostNotIdle) {
		t.Fatalf("exact expiry boundary error = %v", err)
	}
}

func TestAutomationPermitSessionExpiryRowFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("missing exact session", func(t *testing.T) {
		opened := openAutomationTestStore(t)
		lease := registerAutomationTestSession(
			t,
			opened,
			"default",
			"dispatcher-missing-exact-session",
		)
		lease.SessionID += "-other"
		if _, err := opened.AcquireAutomationPermitForSession(
			ctx,
			lease,
		); !errors.Is(err, ErrAutomationHostNotIdle) {
			t.Fatalf("missing exact session error = %v", err)
		}
	})

	t.Run("released session", func(t *testing.T) {
		opened := openAutomationTestStore(t)
		lease := registerAutomationTestSession(
			t,
			opened,
			"default",
			"dispatcher-released-exact-session",
		)
		released, err := opened.ReleaseAutomationDispatcherSession(ctx, lease)
		if err != nil || !released {
			t.Fatalf("release session = %v, err=%v", released, err)
		}
		if _, err := opened.AcquireAutomationPermitForSession(
			ctx,
			lease,
		); !errors.Is(err, ErrAutomationHostNotIdle) {
			t.Fatalf("released exact session error = %v", err)
		}
	})

	for _, expiresAt := range []string{
		"not-a-timestamp",
		"2026-07-24T12:00:00Z",
	} {
		t.Run("malformed "+expiresAt, func(t *testing.T) {
			opened := openAutomationTestStore(t)
			lease := registerAutomationTestSession(
				t,
				opened,
				"default",
				"dispatcher-malformed-expiry",
			)
			if _, err := opened.db.ExecContext(ctx, `
				UPDATE automation_dispatcher_sessions
				SET expires_at = ?
				WHERE session_id = ?
			`, expiresAt, lease.SessionID); err != nil {
				t.Fatal(err)
			}
			if _, err := opened.AcquireAutomationPermitForSession(
				ctx,
				lease,
			); !errors.Is(err, ErrAutomationHostNotIdle) {
				t.Fatalf("malformed session expiry error = %v", err)
			}
		})
	}
}

func TestAutomationPermitValueCopyCannotUseOrReleaseOriginalCapability(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"dispatcher-copied-permit",
	)
	permit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	copied := copyAutomationPermitForTest(permit)

	if err := opened.ValidateAutomationPermit(ctx, copied); !errors.Is(
		err,
		ErrAutomationPermitClosed,
	) {
		t.Fatalf("copied permit validation error = %v", err)
	}
	mutated := false
	if err := opened.WithAutomationPermit(ctx, copied, func() error {
		mutated = true
		return nil
	}); !errors.Is(err, ErrAutomationPermitClosed) {
		t.Fatalf("copied permit mutation error = %v", err)
	}
	if mutated {
		t.Fatal("copied permit ran the guarded mutation")
	}
	if err := copied.Close(); !errors.Is(err, ErrAutomationPermitClosed) {
		t.Fatalf("copied permit close error = %v", err)
	}
	for label, rendered := range map[string]string{
		"string":    copied.String(),
		"go-string": copied.GoString(),
		"formatted": fmt.Sprintf("%+v %#v", copied, copied),
	} {
		if strings.Contains(rendered, lease.leaseToken) ||
			strings.Contains(rendered, opened.automation.lockPath) {
			t.Fatalf("%s rendered a copied permit secret: %s", label, rendered)
		}
	}

	blockedContext, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, _, blockedErr := opened.ActivateAutomationQuarantine(
		blockedContext,
		AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              automationTestSourceKind,
			SourceID:          "copied-permit-must-not-unlock",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "copied_permit_close",
		},
	)
	if !errors.Is(blockedErr, context.DeadlineExceeded) {
		t.Fatalf("quarantine behind copied Close error = %v", blockedErr)
	}
	if err := opened.ValidateAutomationPermit(ctx, permit); err != nil {
		t.Fatalf("original permit after copied Close: %v", err)
	}
	mutated = false
	if err := opened.WithAutomationPermit(ctx, permit, func() error {
		mutated = true
		return nil
	}); err != nil {
		t.Fatalf("original permit mutation: %v", err)
	}
	if !mutated {
		t.Fatal("original permit did not run the guarded mutation")
	}

	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	copiedAfterClose := copyAutomationPermitForTest(permit)
	if err := opened.ValidateAutomationPermit(
		ctx,
		copiedAfterClose,
	); !errors.Is(err, ErrAutomationPermitClosed) {
		t.Fatalf("post-close copy validation error = %v", err)
	}
	if err := copiedAfterClose.Close(); !errors.Is(
		err,
		ErrAutomationPermitClosed,
	) {
		t.Fatalf("post-close copy close error = %v", err)
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
			'automation_quarantine_confirmation_evidence',
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
	if err != nil || schemaVersion != 29 || version != schemaVersion ||
		tables != 5 || releasedAtColumns != 1 ||
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
