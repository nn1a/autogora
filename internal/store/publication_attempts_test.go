package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func createPendingPublicationAttemptFixture(
	t *testing.T,
	opened *Store,
	suffix string,
) (model.Task, model.Publication) {
	t.Helper()
	task, changeSet := createPublicationSource(
		t,
		opened,
		"attempt_"+suffix,
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	publication, created, err := opened.EnsurePublication(
		context.Background(),
		publicationPolicyInput(changeSet.ID, false),
	)
	if err != nil || !created || publication.Status != model.PublicationPending {
		t.Fatalf(
			"create pending publication = %+v, created=%v, err=%v",
			publication,
			created,
			err,
		)
	}
	return task, publication
}

func beginPublicationAttemptFixture(
	t *testing.T,
	opened *Store,
	publication model.Publication,
	sessionID string,
	ttl time.Duration,
) (
	model.Publication,
	*PublicationAttemptPermit,
	AutomationDispatcherSessionLease,
) {
	t.Helper()
	ctx := context.Background()
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		sessionID,
	)
	basePermit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	claimed, operation, acquired, err := opened.BeginAutomatedPublicationAttempt(
		ctx,
		basePermit,
		publication.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: publication.UpdatedAt,
			TTL:               ttl,
		},
	)
	if closeErr := basePermit.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !acquired || operation == nil {
		t.Fatalf(
			"begin publication attempt = %+v, op=%s, acquired=%v, err=%v",
			claimed,
			operation,
			acquired,
			err,
		)
	}
	return claimed, operation, lease
}

func TestPublicationExecutionProvenanceCanonicalizesAndBoundsSnapshot(
	t *testing.T,
) {
	value := model.Publication{
		TaskID:         "task_provenance",
		RepositoryPath: "/repo/provenance",
		WorktreePath:   "/worktree/provenance",
		SourceSnapshot: json.RawMessage(`{"second":2,"first":1}`),
	}
	first, err := publicationExecutionProvenanceFingerprint(value)
	if err != nil {
		t.Fatal(err)
	}
	value.SourceSnapshot = json.RawMessage(
		"{\n  \"first\": 1,\n  \"second\": 2\n}",
	)
	second, err := publicationExecutionProvenanceFingerprint(value)
	if err != nil || first != second {
		t.Fatalf(
			"canonical provenance first=%q second=%q err=%v",
			first,
			second,
			err,
		)
	}
	value.SourceSnapshot = nil
	if _, err := publicationExecutionProvenanceFingerprint(value); err == nil {
		t.Fatal("empty source snapshot unexpectedly accepted")
	}
	value.SourceSnapshot = json.RawMessage(
		`{"value":"` +
			strings.Repeat(
				"x",
				MaxPublicationSourceSnapshotBytes,
			) +
			`"}`,
	)
	if _, err := publicationExecutionProvenanceFingerprint(value); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized source snapshot error=%v", err)
	}
}

func TestBeginAutomatedPublicationAttemptAtomicallyClaimsAndRecordsIntent(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "begin")
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"publisher-begin",
	)
	basePermit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	claimed, operation, acquired, err := opened.BeginAutomatedPublicationAttempt(
		ctx,
		basePermit,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if err != nil || !acquired || operation == nil {
		t.Fatalf(
			"begin = %+v, operation=%s, acquired=%v, err=%v",
			claimed,
			operation,
			acquired,
			err,
		)
	}
	if claimed.Status != model.PublicationPublishing ||
		claimed.ClaimEpoch != 1 ||
		claimed.ClaimToken != "" ||
		claimed.ClaimExpiresAt == nil {
		t.Fatalf("safe claimed publication = %+v", claimed)
	}
	intent := operation.Intent()
	if intent.ID == "" ||
		intent.Board != "default" ||
		intent.PublicationID != pending.ID ||
		intent.ChangeSetID != pending.ChangeSetID ||
		intent.Mode != pending.Mode ||
		intent.TargetBranch != pending.TargetBranch ||
		intent.Remote != pending.Remote ||
		intent.BaseCommit != pending.BaseCommit ||
		intent.HeadCommit != pending.HeadCommit ||
		intent.DurableRef != pending.DurableRef ||
		len(intent.SourceKey) != 64 ||
		len(intent.ExecutionProvenanceFingerprint) != 64 ||
		len(intent.EffectFingerprint) != 64 ||
		intent.ClaimEpoch != claimed.ClaimEpoch ||
		intent.PublicationUpdatedAt != claimed.UpdatedAt ||
		intent.ClaimExpiresAt != *claimed.ClaimExpiresAt ||
		intent.SessionID != lease.SessionID {
		t.Fatalf("attempt intent = %+v", intent)
	}
	expectedProvenance, err :=
		publicationExecutionProvenanceFingerprint(pending)
	if err != nil ||
		intent.ExecutionProvenanceFingerprint != expectedProvenance {
		t.Fatalf(
			"execution provenance=%q want=%q err=%v",
			intent.ExecutionProvenanceFingerprint,
			expectedProvenance,
			err,
		)
	}
	if len(intent.StartedAt) != len("2026-07-24T00:00:00.000000000Z") {
		t.Fatalf("attempt startedAt is not fixed-width: %q", intent.StartedAt)
	}
	_, expectedSourceKey, err := normalizeAutomationSource(
		AutomationQuarantineSourceInput{
			Board:              intent.Board,
			Kind:               "publication",
			SourceID:           intent.PublicationID,
			ObservedUpdatedAt:  intent.PublicationUpdatedAt,
			ObservedClaimEpoch: fmt.Sprintf("%d", intent.ClaimEpoch),
			DiagnosticCode:     "process_teardown_unconfirmed",
			ValidateCurrent: func(
				context.Context,
				AutomationQuarantineSourceInput,
			) (bool, error) {
				return true, nil
			},
		},
	)
	if err != nil || intent.SourceKey != expectedSourceKey {
		t.Fatalf(
			"attempt source key=%s want=%s err=%v",
			intent.SourceKey,
			expectedSourceKey,
			err,
		)
	}
	encodedIntent, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedIntent), pending.RepositoryPath) ||
		strings.Contains(string(encodedIntent), pending.WorktreePath) ||
		strings.Contains(string(encodedIntent), string(pending.SourceSnapshot)) {
		t.Fatalf("attempt intent leaked an execution path: %s", encodedIntent)
	}
	for _, mutate := range []func(*model.Publication){
		func(value *model.Publication) { value.TaskID += "_different" },
		func(value *model.Publication) {
			value.RepositoryPath += "_different"
		},
		func(value *model.Publication) {
			value.WorktreePath += "_different"
		},
		func(value *model.Publication) {
			value.SourceSnapshot = json.RawMessage(
				`{"changed":"source snapshot"}`,
			)
		},
	} {
		changed := pending
		changed.SourceSnapshot = append(
			json.RawMessage(nil),
			pending.SourceSnapshot...,
		)
		mutate(&changed)
		fingerprint, fingerprintErr :=
			publicationExecutionProvenanceFingerprint(changed)
		if fingerprintErr != nil {
			t.Fatal(fingerprintErr)
		}
		if fingerprint == intent.ExecutionProvenanceFingerprint {
			t.Fatalf(
				"execution provenance ignored mutation: %+v",
				changed,
			)
		}
	}
	changedEffect := intent
	changedEffect.HeadCommit += "-different"
	if publicationEffectFingerprint(changedEffect) == intent.EffectFingerprint {
		t.Fatal("effect fingerprint ignored a changed head commit")
	}
	record, err := opened.GetPublicationAttempt(ctx, intent.ID)
	if err != nil || record.Intent != intent || record.Result != nil ||
		record.RecoveryReceipt != nil {
		t.Fatalf("stored attempt = %+v, err=%v", record, err)
	}

	secretValues := []string{
		operation.state.claimToken,
		operation.state.gateToken,
		operation.state.sessionToken,
		operation.state.authorityPath,
		operation.state.lockPath,
	}
	encoded, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%s\n%+v\n%#v", operation, operation, operation)
	for _, secret := range secretValues {
		if secret != "" &&
			(strings.Contains(string(encoded), secret) ||
				strings.Contains(formatted, secret)) {
			t.Fatalf(
				"operation credential leaked: json=%s formatted=%s",
				encoded,
				formatted,
			)
		}
	}
	if string(encoded) != "{}" {
		t.Fatalf("operation JSON = %s, want {}", encoded)
	}

	contended, secondOperation, secondAcquired, err :=
		opened.BeginAutomatedPublicationAttempt(
			ctx,
			basePermit,
			pending.ID,
			ClaimPublicationInput{
				ExpectedUpdatedAt: pending.UpdatedAt,
				TTL:               time.Minute,
			},
		)
	if err != nil || secondAcquired || secondOperation != nil ||
		contended.Status != model.PublicationPublishing ||
		contended.ClaimToken != "" ||
		contended.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf(
			"contended begin = %+v, op=%s, acquired=%v, err=%v",
			contended,
			secondOperation,
			secondAcquired,
			err,
		)
	}
	if err := basePermit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginAutomatedPublicationAttemptRollsBackClaimWhenIntentFails(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "rollback")
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"publisher-begin-rollback",
	)
	basePermit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer basePermit.Close()
	if _, err := opened.db.ExecContext(ctx, `
		CREATE TRIGGER reject_publication_attempt_intent_for_test
		BEFORE INSERT ON publication_attempt_intents
		BEGIN
			SELECT RAISE(ABORT, 'reject intent for atomicity test');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if _, operation, acquired, err :=
		opened.BeginAutomatedPublicationAttempt(
			ctx,
			basePermit,
			pending.ID,
			ClaimPublicationInput{
				ExpectedUpdatedAt: pending.UpdatedAt,
				TTL:               time.Minute,
			},
		); err == nil || acquired || operation != nil {
		t.Fatalf(
			"rejected begin operation=%s acquired=%v err=%v",
			operation,
			acquired,
			err,
		)
	}
	preserved, err := opened.GetPublication(ctx, pending.ID)
	if err != nil ||
		preserved.Status != model.PublicationPending ||
		preserved.ClaimEpoch != 0 ||
		preserved.UpdatedAt != pending.UpdatedAt ||
		preserved.ClaimExpiresAt != nil {
		t.Fatalf("rolled-back publication = %+v, err=%v", preserved, err)
	}
	var intentCount int
	if err := opened.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM publication_attempt_intents",
	).Scan(&intentCount); err != nil || intentCount != 0 {
		t.Fatalf("intent count=%d err=%v", intentCount, err)
	}
}

func TestFinishAutomatedPublicationAttemptAfterExpiryIsAtomicAndIdempotent(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	baseTime := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	publicationTime := baseTime
	opened.publicationClock = func() time.Time { return publicationTime }
	task, pending := createPendingPublicationAttemptFixture(t, opened, "finish")
	claimed, operation, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-finish",
		MinPublicationClaimTTL,
	)
	publicationTime = baseTime.Add(MinPublicationClaimTTL + time.Second)

	if _, err := opened.db.ExecContext(ctx, `
		CREATE TRIGGER reject_publication_attempt_result_for_test
		BEFORE INSERT ON publication_attempt_results
		BEGIN
			SELECT RAISE(ABORT, 'reject result for atomicity test');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	url := "https://example.test/pull/attempt"
	input := PublicationAttemptResultInput{
		Outcome:        PublicationAttemptPublished,
		ExecutorStatus: PublicationExecutorPublished,
		URL:            &url,
	}
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		input,
	); err == nil {
		t.Fatal("result trigger did not reject finish")
	}
	stillPublishing, err := opened.GetPublication(ctx, pending.ID)
	if err != nil ||
		stillPublishing.Status != model.PublicationPublishing ||
		stillPublishing.UpdatedAt != claimed.UpdatedAt ||
		stillPublishing.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf(
			"result rollback publication = %+v, err=%v",
			stillPublishing,
			err,
		)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"DROP TRIGGER reject_publication_attempt_result_for_test",
	); err != nil {
		t.Fatal(err)
	}

	result, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		input,
	)
	if err != nil ||
		result.Outcome != PublicationAttemptPublished ||
		result.ExecutorStatus != PublicationExecutorPublished ||
		result.URL == nil || *result.URL != url ||
		result.Error != nil || result.ErrorKind != nil ||
		len(result.RecordedAt) !=
			len("2026-07-24T00:00:00.000000000Z") {
		t.Fatalf("published result = %+v, err=%v", result, err)
	}
	copiedOperation := *operation
	if !strings.Contains(copiedOperation.String(), "finished") {
		t.Fatalf("copied permit did not share finish state: %s", &copiedOperation)
	}
	published, err := opened.GetPublication(ctx, pending.ID)
	if err != nil ||
		published.Status != model.PublicationPublished ||
		published.URL == nil || *published.URL != url ||
		published.ClaimExpiresAt != nil ||
		published.PublishedAt == nil ||
		published.UpdatedAt != result.PublicationUpdatedAt {
		t.Fatalf("published publication = %+v, err=%v", published, err)
	}
	replayed, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		&copiedOperation,
		input,
	)
	if err != nil || !reflect.DeepEqual(replayed, result) {
		t.Fatalf("exact result replay = %+v, err=%v", replayed, err)
	}
	conflicting := input
	conflicting.ExecutorStatus = PublicationExecutorAlreadyPublished
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		conflicting,
	); !errors.Is(err, ErrPublicationAttemptResultConflict) {
		t.Fatalf("different result replay error = %v", err)
	}
	if !strings.Contains(operation.String(), "finished") {
		t.Fatalf("finished operation string = %s", operation)
	}

	intent := operation.Intent()
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publication_attempt_intents SET session_id = 'changed' WHERE id = ?",
		intent.ID,
	); err == nil {
		t.Fatal("immutable intent was updated")
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"DELETE FROM publication_attempt_intents WHERE id = ?",
		intent.ID,
	); err == nil {
		t.Fatal("immutable intent was deleted")
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publication_attempt_results SET outcome = 'failed' WHERE attempt_id = ?",
		intent.ID,
	); err == nil {
		t.Fatal("immutable result was updated")
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"DELETE FROM publication_attempt_results WHERE attempt_id = ?",
		intent.ID,
	); err == nil {
		t.Fatal("immutable result was deleted")
	}

	if err := opened.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("delete terminal publication task: %v", err)
	}
	if _, err := opened.GetPublication(
		ctx,
		pending.ID,
	); !errors.Is(err, ErrPublicationNotFound) {
		t.Fatalf("publication after task cascade error = %v", err)
	}
	preserved, err := opened.GetPublicationAttempt(ctx, intent.ID)
	if err != nil || preserved.Intent != intent ||
		preserved.Result == nil || !reflect.DeepEqual(*preserved.Result, result) {
		t.Fatalf("attempt after task cascade = %+v, err=%v", preserved, err)
	}
	replayedAfterDelete, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		input,
	)
	if err != nil || !reflect.DeepEqual(replayedAfterDelete, result) {
		t.Fatalf(
			"receipt replay after task cascade = %+v, err=%v",
			replayedAfterDelete,
			err,
		)
	}
}

func TestFinishPublicationAttemptCleansUpAfterSessionReleaseAndQuarantine(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(t, opened, "quarantine")
	_, operation, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-quarantine",
		time.Minute,
	)
	if released, err := opened.ReleaseAutomationDispatcherSession(
		ctx,
		lease,
	); err != nil || !released {
		t.Fatalf("release session=%v err=%v", released, err)
	}
	if _, activated, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              automationTestSourceKind,
			SourceID:          "unrelated-publication-cleanup",
			ObservedUpdatedAt: "unrelated-epoch",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	); err != nil || !activated {
		t.Fatalf("activate quarantine=%v err=%v", activated, err)
	}
	result, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		PublicationAttemptResultInput{
			Outcome:        PublicationAttemptFailed,
			ExecutorStatus: PublicationExecutorFailed,
			ErrorKind:      PublicationErrorRemoteConflict,
			Error:          "remote branch changed",
		},
	)
	if err != nil ||
		result.Outcome != PublicationAttemptFailed ||
		result.ExecutorStatus != PublicationExecutorFailed ||
		result.ErrorKind == nil ||
		*result.ErrorKind != PublicationErrorRemoteConflict {
		t.Fatalf("cleanup result = %+v, err=%v", result, err)
	}
	failed, err := publicationForBoard(ctx, opened.db, pending.ID, "default")
	if err != nil ||
		failed.Status != model.PublicationFailed ||
		failed.Error == nil || *failed.Error != "remote branch changed" ||
		failed.ClaimToken != "" || failed.ClaimExpiresAt != nil {
		t.Fatalf("cleanup publication = %+v, err=%v", failed, err)
	}
}

func TestUnknownAndIntentOnlyAttemptsRemainUnresolvedUntilExactRecovery(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, firstPending := createPendingPublicationAttemptFixture(t, opened, "intent_only")
	firstClaimed, firstOperation, firstLease := beginPublicationAttemptFixture(
		t,
		opened,
		firstPending,
		"publisher-intent-only",
		time.Minute,
	)
	if released, err := opened.ReleaseAutomationDispatcherSession(
		ctx,
		firstLease,
	); err != nil || !released {
		t.Fatalf("release first session=%v err=%v", released, err)
	}

	secondTask, secondPending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"unknown",
	)
	secondClaimed, secondOperation, secondLease := beginPublicationAttemptFixture(
		t,
		opened,
		secondPending,
		"publisher-unknown",
		time.Minute,
	)
	unknownInput := PublicationAttemptResultInput{
		Outcome:        PublicationAttemptUnknown,
		ExecutorStatus: PublicationExecutorUnknown,
		ErrorKind:      PublicationErrorCommandTimeout,
		Error:          "push timed out after the remote may have accepted it",
	}
	unknown, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		secondOperation,
		unknownInput,
	)
	if err != nil ||
		unknown.Outcome != PublicationAttemptUnknown ||
		unknown.ErrorKind == nil ||
		*unknown.ErrorKind != PublicationErrorCommandTimeout {
		t.Fatalf("unknown result = %+v, err=%v", unknown, err)
	}
	preserved, err := publicationForBoard(
		ctx,
		opened.db,
		secondPending.ID,
		"default",
	)
	if err != nil ||
		preserved.Status != model.PublicationPublishing ||
		preserved.UpdatedAt != secondClaimed.UpdatedAt ||
		preserved.ClaimToken == "" ||
		preserved.ClaimExpiresAt == nil {
		t.Fatalf("unknown publication = %+v, err=%v", preserved, err)
	}

	unresolved, err := opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 2 {
		t.Fatalf("unresolved attempts = %+v, err=%v", unresolved, err)
	}
	byID := make(map[string]PublicationAttemptRecord, len(unresolved))
	for _, record := range unresolved {
		byID[record.Intent.ID] = record
	}
	firstIntent := firstOperation.Intent()
	secondIntent := secondOperation.Intent()
	if byID[firstIntent.ID].Result != nil ||
		byID[secondIntent.ID].Result == nil ||
		byID[secondIntent.ID].Result.Outcome != PublicationAttemptUnknown {
		t.Fatalf("unresolved classification = %+v", unresolved)
	}
	if firstIntent.PublicationUpdatedAt != firstClaimed.UpdatedAt ||
		secondIntent.PublicationUpdatedAt != secondClaimed.UpdatedAt {
		t.Fatalf(
			"unresolved tuples = first:%+v second:%+v",
			firstIntent,
			secondIntent,
		)
	}
	firstPage, err := opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 1},
	)
	if err != nil || len(firstPage) != 1 {
		t.Fatalf("first unresolved page = %+v, err=%v", firstPage, err)
	}
	secondPage, err := opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{
			AfterStartedAt: firstPage[0].Intent.StartedAt,
			AfterID:        firstPage[0].Intent.ID,
			Limit:          1,
		},
	)
	if err != nil || len(secondPage) != 1 ||
		secondPage[0].Intent.ID == firstPage[0].Intent.ID {
		t.Fatalf("second unresolved page = %+v, err=%v", secondPage, err)
	}

	recoveryTimestamp := now()
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id, observed_updated_at,
			observed_claim_epoch, outcome, disposition, result_url, actor,
			reason, recovered_at, result_updated_at
		) VALUES (?, 1, ?, ?, ?, 'failed', 'abandoned', NULL, 'operator',
			'verified remote failure', ?, ?)
	`,
		secondIntent.SourceKey,
		secondIntent.PublicationID,
		secondIntent.PublicationUpdatedAt,
		secondIntent.ClaimEpoch,
		recoveryTimestamp,
		recoveryTimestamp,
	); err != nil {
		t.Fatal(err)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 2 {
		t.Fatalf(
			"receipt beside Publishing was hidden = %+v, err=%v",
			unresolved,
			err,
		)
	}
	byID = make(map[string]PublicationAttemptRecord, len(unresolved))
	for _, record := range unresolved {
		byID[record.Intent.ID] = record
	}
	if byID[secondIntent.ID].RecoveryReceipt == nil {
		t.Fatalf("Publishing integrity record omitted recovery receipt: %+v", unresolved)
	}
	recoveredRecord, err := opened.GetPublicationAttempt(ctx, secondIntent.ID)
	if err != nil ||
		recoveredRecord.RecoveryReceipt == nil ||
		recoveredRecord.RecoveryReceipt.SourceKey != secondIntent.SourceKey {
		t.Fatalf("recovered attempt = %+v, err=%v", recoveredRecord, err)
	}

	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id, observed_updated_at,
			observed_claim_epoch, outcome, disposition, result_url, actor,
			reason, recovered_at, result_updated_at
		) VALUES (?, 1, 'mismatched-publication', ?, ?, 'failed', 'abandoned',
			NULL, 'operator', 'mismatched receipt', ?, ?)
	`,
		firstIntent.SourceKey,
		firstIntent.PublicationUpdatedAt,
		firstIntent.ClaimEpoch,
		recoveryTimestamp,
		recoveryTimestamp,
	); err != nil {
		t.Fatal(err)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 2 {
		t.Fatalf("mismatched receipt hid attempt = %+v, err=%v", unresolved, err)
	}
	if _, err := opened.GetPublicationAttempt(
		ctx,
		firstIntent.ID,
	); err == nil || !strings.Contains(
		err.Error(),
		"does not match its attempt",
	) {
		t.Fatalf("mismatched receipt integrity error = %v", err)
	}

	if _, err := opened.db.ExecContext(ctx, `
		UPDATE publications
		SET status = 'failed', url = NULL, error = 'verified remote failure',
			claim_token = NULL, claim_expires_at = NULL, updated_at = ?
		WHERE id = ? AND board = 'default' AND status = 'publishing'
			AND updated_at = ? AND claim_epoch = ?
	`,
		recoveryTimestamp,
		secondIntent.PublicationID,
		secondIntent.PublicationUpdatedAt,
		secondIntent.ClaimEpoch,
	); err != nil {
		t.Fatal(err)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 1 ||
		unresolved[0].Intent.ID != firstIntent.ID {
		t.Fatalf(
			"unresolved after exact recovery = %+v, err=%v",
			unresolved,
			err,
		)
	}
	retried, err := opened.RetryPublication(
		ctx,
		secondPending.ID,
		RetryPublicationInput{ExpectedUpdatedAt: recoveryTimestamp},
	)
	if err != nil || retried.Status != model.PublicationPending {
		t.Fatalf("retry recovered publication = %+v, err=%v", retried, err)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 1 ||
		unresolved[0].Intent.ID != firstIntent.ID {
		t.Fatalf(
			"old recovery resurrected after retry = %+v, err=%v",
			unresolved,
			err,
		)
	}
	basePermit, err := opened.AcquireAutomationPermitForSession(ctx, secondLease)
	if err != nil {
		t.Fatal(err)
	}
	_, newOperation, acquired, err := opened.BeginAutomatedPublicationAttempt(
		ctx,
		basePermit,
		retried.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: retried.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if closeErr := basePermit.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !acquired || newOperation == nil {
		t.Fatalf(
			"begin retry attempt operation=%s acquired=%v err=%v",
			newOperation,
			acquired,
			err,
		)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 2 {
		t.Fatalf("new retry unresolved = %+v, err=%v", unresolved, err)
	}
	for _, record := range unresolved {
		if record.Intent.ID == secondIntent.ID {
			t.Fatalf("old recovered attempt returned after new claim: %+v", unresolved)
		}
	}
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		newOperation,
		PublicationAttemptResultInput{
			Outcome:        PublicationAttemptFailed,
			ExecutorStatus: PublicationExecutorFailed,
			ErrorKind:      PublicationErrorRemoteConflict,
			Error:          "new retry failed deterministically",
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := opened.DeleteTask(ctx, secondTask.ID); err != nil {
		t.Fatalf("delete recovered and retried task: %v", err)
	}
	unresolved, err = opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 10},
	)
	if err != nil || len(unresolved) != 1 ||
		unresolved[0].Intent.ID != firstIntent.ID {
		t.Fatalf(
			"receipt-only recovery after task cascade = %+v, err=%v",
			unresolved,
			err,
		)
	}
}

func TestListUnresolvedPublicationAttemptsScansPastRecoveredRawPages(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	lease := registerAutomationTestSession(
		t,
		opened,
		"default",
		"publisher-recovered-pages",
	)
	var wanted PublicationAttemptIntent
	for index := 0; index < 3; index++ {
		_, pending := createPendingPublicationAttemptFixture(
			t,
			opened,
			fmt.Sprintf("recovered_page_%d", index),
		)
		basePermit, err := opened.AcquireAutomationPermitForSession(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		_, operation, acquired, err := opened.BeginAutomatedPublicationAttempt(
			ctx,
			basePermit,
			pending.ID,
			ClaimPublicationInput{
				ExpectedUpdatedAt: pending.UpdatedAt,
				TTL:               time.Minute,
			},
		)
		if closeErr := basePermit.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || !acquired || operation == nil {
			t.Fatalf(
				"begin raw page attempt %d operation=%s acquired=%v err=%v",
				index,
				operation,
				acquired,
				err,
			)
		}
		intent := operation.Intent()
		if index == 2 {
			wanted = intent
			continue
		}
		if _, err := opened.FinishAutomatedPublicationAttempt(
			ctx,
			operation,
			PublicationAttemptResultInput{
				Outcome:        PublicationAttemptUnknown,
				ExecutorStatus: PublicationExecutorUnknown,
				ErrorKind:      PublicationErrorCommandTimeout,
				Error:          "remote result was uncertain",
			},
		); err != nil {
			t.Fatal(err)
		}
		recoveredAt := now()
		reason := fmt.Sprintf("operator recovered attempt %d", index)
		if _, err := opened.db.ExecContext(ctx, `
			INSERT INTO publication_recovery_receipts(
				source_key, first_generation, publication_id,
				observed_updated_at, observed_claim_epoch, outcome,
				disposition, result_url, actor, reason, recovered_at,
				result_updated_at
			) VALUES (?, 1, ?, ?, ?, 'failed', 'abandoned', NULL,
				'operator', ?, ?, ?)
		`,
			intent.SourceKey,
			intent.PublicationID,
			intent.PublicationUpdatedAt,
			intent.ClaimEpoch,
			reason,
			recoveredAt,
			recoveredAt,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.db.ExecContext(ctx, `
			UPDATE publications
			SET status = 'failed', url = NULL, error = ?,
				claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND board = 'default' AND status = 'publishing'
				AND updated_at = ? AND claim_epoch = ?
		`,
			reason,
			recoveredAt,
			intent.PublicationID,
			intent.PublicationUpdatedAt,
			intent.ClaimEpoch,
		); err != nil {
			t.Fatal(err)
		}
	}

	values, err := opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{Limit: 1},
	)
	if err != nil || len(values) != 1 || values[0].Intent.ID != wanted.ID {
		t.Fatalf(
			"unresolved after recovered raw prefix = %+v, want=%s err=%v",
			values,
			wanted.ID,
			err,
		)
	}
	after, err := opened.ListUnresolvedPublicationAttempts(
		ctx,
		PublicationAttemptFilter{
			AfterStartedAt: values[0].Intent.StartedAt,
			AfterID:        values[0].Intent.ID,
			Limit:          1,
		},
	)
	if err != nil || len(after) != 0 {
		t.Fatalf("unresolved after final cursor = %+v, err=%v", after, err)
	}
}

func TestSchema30AddsImmutablePublicationAttemptLedger(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		DROP TABLE publication_attempt_results;
		DROP TABLE publication_attempt_intents;
		PRAGMA user_version = 27;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version, tables, triggers, foreignKeys int
	if err := reopened.db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN (
			'publication_attempt_intents', 'publication_attempt_results'
		)
	`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name IN (
			'publication_attempt_intents_prevent_update',
			'publication_attempt_intents_prevent_delete',
			'publication_attempt_intents_require_v30_evidence',
			'publication_attempt_results_identity_guard',
			'publication_attempt_results_prevent_update',
			'publication_attempt_results_prevent_delete',
			'publication_attempt_results_require_v30_evidence'
		)
	`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM pragma_foreign_key_list(
				'publication_attempt_intents'
			))
			+
			(SELECT COUNT(*) FROM pragma_foreign_key_list(
				'publication_attempt_results'
			))
	`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || schemaVersion != 30 ||
		tables != 2 || triggers != 7 || foreignKeys != 0 {
		t.Fatalf(
			"publication attempt migration version=%d constant=%d tables=%d triggers=%d foreignKeys=%d",
			version,
			schemaVersion,
			tables,
			triggers,
			foreignKeys,
		)
	}
	if _, err := reopened.db.ExecContext(ctx, `
		INSERT INTO publication_attempt_results(
			attempt_id, board, publication_id, claim_epoch, outcome,
			executor_status, error_kind, result_url, error,
			publication_updated_at, recorded_at
		) VALUES (
			'pat_orphan', 'default', 'pub_orphan', 1, 'failed', 'failed',
			'internal', NULL, 'orphan result',
			'2026-07-24T00:00:00Z', '2026-07-24T00:00:00Z'
		)
	`); err == nil {
		t.Fatal("orphan publication attempt result bypassed identity guard")
	}
}

func TestSchema30MigratesNullableProvenanceAndRejectsOldEvidence(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	oldIntent := insertPublicationAttemptRecoveryIntent(
		t,
		opened,
		"pat_pre_provenance",
		"2026-07-24T00:00:00.100000000Z",
	)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		DROP TRIGGER publication_attempt_intents_require_v30_evidence;
		DROP TRIGGER publication_attempt_results_require_v30_evidence;
		ALTER TABLE publication_attempt_intents
			DROP COLUMN execution_provenance_fingerprint;
		PRAGMA user_version = 29;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var nullable, version, triggerCount int
	if err := migrated.db.QueryRowContext(ctx, `
		SELECT CASE WHEN "notnull" = 0 THEN 1 ELSE 0 END
		FROM pragma_table_info('publication_attempt_intents')
		WHERE name = 'execution_provenance_fingerprint'
	`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name IN (
			'publication_attempt_intents_require_v30_evidence',
			'publication_attempt_results_require_v30_evidence'
		)
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if nullable != 1 || version != schemaVersion || triggerCount != 2 {
		t.Fatalf(
			"migration nullable=%d version=%d triggers=%d",
			nullable,
			version,
			triggerCount,
		)
	}
	if _, err := migrated.GetPublicationAttempt(
		ctx,
		oldIntent.ID,
	); err == nil ||
		!strings.Contains(err.Error(), "lacks v30 execution provenance") {
		t.Fatalf("old attempt evidence error=%v", err)
	}
	if _, err := migrated.db.ExecContext(ctx, `
		INSERT INTO publication_attempt_results(
			attempt_id, board, publication_id, claim_epoch, outcome,
			executor_status, error_kind, result_url, error,
			publication_updated_at, recorded_at
		) VALUES (?, ?, ?, ?, 'failed', 'failed', 'internal', NULL,
			'old attempt cannot execute', ?, ?)
	`,
		oldIntent.ID,
		oldIntent.Board,
		oldIntent.PublicationID,
		oldIntent.ClaimEpoch,
		oldIntent.PublicationUpdatedAt,
		"2026-07-24T00:00:00.110000000Z",
	); err == nil ||
		!strings.Contains(err.Error(), "requires v30 evidence") {
		t.Fatalf("old attempt result insert error=%v", err)
	}
}

func TestSchema30ConcurrentOpenSerializesProvenanceMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		DROP TRIGGER publication_attempt_intents_require_v30_evidence;
		DROP TRIGGER publication_attempt_results_require_v30_evidence;
		ALTER TABLE publication_attempt_intents
			DROP COLUMN execution_provenance_fingerprint;
		PRAGMA user_version = 29;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			value, openErr := Open(path, "default", "")
			if openErr == nil {
				openErr = value.Close()
			}
			results <- openErr
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent provenance migration error=%v", err)
		}
	}
}

func TestOpenRejectsNewerSchemaWithoutMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		DROP INDEX idx_publication_attempt_intents_publication;
		PRAGMA user_version = 31;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if newer, err := Open(path, "default", ""); err == nil {
		newer.Close()
		t.Fatal("newer schema unexpectedly opened")
	} else if !strings.Contains(
		err.Error(),
		"schema version 31 is newer than supported version 30",
	) {
		t.Fatalf("newer schema error=%v", err)
	}
	inspection, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var version, indexCount, provenanceColumns int
	if err := inspection.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index'
			AND name = 'idx_publication_attempt_intents_publication'
	`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_table_info(
			'publication_attempt_intents'
		)
		WHERE name = 'execution_provenance_fingerprint'
	`).Scan(&provenanceColumns); err != nil {
		t.Fatal(err)
	}
	if version != 31 || indexCount != 0 || provenanceColumns != 1 {
		t.Fatalf(
			"newer schema mutated: version=%d index=%d provenanceColumns=%d",
			version,
			indexCount,
			provenanceColumns,
		)
	}
}
