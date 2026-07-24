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

func claimPublicationForRecovery(
	t *testing.T,
	opened *Store,
	suffix string,
) model.Publication {
	t.Helper()
	ctx := context.Background()
	_, changeSet := createPublicationSource(
		t,
		opened,
		"recovery_"+suffix,
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
		t.Fatalf("claim publication: acquired=%v err=%v", acquired, err)
	}
	return claimed
}

func publicationRecoveryInput(
	claimed model.Publication,
	sourceNumber int,
	outcome PublicationRecoveryOutcome,
) PublicationRecoveryInput {
	input := PublicationRecoveryInput{
		SourceKey:          fmt.Sprintf("%064x", sourceNumber),
		FirstGeneration:    int64(sourceNumber + 10),
		PublicationID:      claimed.ID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: claimed.ClaimEpoch,
		Outcome:            outcome,
		Disposition:        AutomationSourceSuperseded,
		Actor:              "release-operator",
		Reason:             "external publication process is confirmed stopped",
	}
	switch outcome {
	case PublicationRecoveryPublished:
		rawURL := fmt.Sprintf("https://example.test/pulls/%d", sourceNumber)
		input.ResultURL = &rawURL
	case PublicationRecoveryFailed:
		input.Disposition = AutomationSourceAbandoned
		input.Reason = "remote rejected the publication"
	case PublicationRecoverySuperseded:
		input.Reason = "operator superseded the uncertain publication"
	}
	return input
}

func applyPublicationRecoveryForTest(
	opened *Store,
	ctx context.Context,
	input PublicationRecoveryInput,
) (PublicationRecoveryResult, error) {
	confirmation := AutomationQuarantineConfirmation{
		Generation: input.FirstGeneration,
		Actor:      input.Actor,
		Reason:     input.Reason,
		Sources: []AutomationQuarantineSourceResolution{{
			SourceKey:          input.SourceKey,
			ObservedUpdatedAt:  input.ObservedUpdatedAt,
			ObservedClaimEpoch: fmt.Sprintf("%d", input.ObservedClaimEpoch),
			Disposition:        input.Disposition,
			Outcome:            input.Outcome,
			ResultURL:          clonePublicationRecoveryString(input.ResultURL),
		}},
	}
	permit, err := newAutomationRecoveryPermit(
		confirmation,
		[]AutomationQuarantineSource{{
			SourceKey:          input.SourceKey,
			Generation:         input.FirstGeneration,
			Board:              opened.board,
			Kind:               "publication",
			SourceID:           input.PublicationID,
			ObservedUpdatedAt:  input.ObservedUpdatedAt,
			ObservedClaimEpoch: fmt.Sprintf("%d", input.ObservedClaimEpoch),
		}},
	)
	if err != nil {
		return PublicationRecoveryResult{}, err
	}
	return opened.ApplyPublicationRecovery(ctx, permit, input)
}

func TestAutomationRecoveryPermitIsCallbackScopedAndResultBound(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	claimed := claimPublicationForRecovery(t, opened, "permit_scope")
	gate, activated, err := opened.ActivateAutomationQuarantine(
		ctx,
		AutomationQuarantineSourceInput{
			Board:              opened.board,
			Kind:               "publication",
			SourceID:           claimed.ID,
			ObservedUpdatedAt:  claimed.UpdatedAt,
			ObservedClaimEpoch: fmt.Sprintf("%d", claimed.ClaimEpoch),
			DiagnosticCode:     "process_teardown_unconfirmed",
			ValidateCurrent:    opened.ValidatePublishingAutomationSource,
		},
	)
	if err != nil || !activated {
		t.Fatalf("activate publication quarantine: gate=%+v activated=%v err=%v",
			gate, activated, err)
	}
	sources, err := opened.ListAutomationQuarantineSources(
		ctx,
		AutomationQuarantineSourceFilter{ActiveOnly: true, Limit: 1000},
	)
	if err != nil || len(sources) != 1 {
		t.Fatalf("publication quarantine sources=%+v err=%v", sources, err)
	}
	source := sources[0]
	input := publicationRecoveryInput(
		claimed,
		110,
		PublicationRecoveryPublished,
	)
	input.SourceKey = source.SourceKey
	input.FirstGeneration = source.Generation
	confirmation := AutomationQuarantineConfirmation{
		Generation:            gate.Generation,
		Actor:                 input.Actor,
		Reason:                input.Reason,
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources: []AutomationQuarantineSourceResolution{{
			SourceKey:          source.SourceKey,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: source.ObservedClaimEpoch,
			Disposition:        input.Disposition,
			Outcome:            input.Outcome,
			ResultURL:          clonePublicationRecoveryString(input.ResultURL),
		}},
	}

	for name, permit := range map[string]*AutomationRecoveryPermit{
		"nil":  nil,
		"zero": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := opened.ApplyPublicationRecovery(
				ctx,
				permit,
				input,
			); !errors.Is(err, ErrAutomationRecoveryPermit) {
				t.Fatalf("invalid permit error=%v", err)
			}
		})
	}
	if _, found, err := opened.GetPublicationRecoveryReceipt(
		ctx,
		input.SourceKey,
	); err != nil || found {
		t.Fatalf("invalid permit wrote receipt: found=%v err=%v", found, err)
	}

	var captured *AutomationRecoveryPermit
	var applied PublicationRecoveryResult
	guardCalls := 0
	confirmation.Guard = func(
		guardContext context.Context,
		snapshot AutomationQuarantineSnapshot,
	) error {
		guardCalls++
		captured = snapshot.RecoveryPermit
		if captured == nil {
			return errors.New("recovery permit is missing")
		}
		if guardCalls == 1 {
			wrongOutcome := input
			wrongOutcome.Outcome = PublicationRecoverySuperseded
			wrongOutcome.ResultURL = nil
			if _, err := opened.ApplyPublicationRecovery(
				guardContext,
				captured,
				wrongOutcome,
			); !errors.Is(err, ErrAutomationRecoveryScope) {
				return fmt.Errorf("wrong outcome permit error: %w", err)
			}
			wrongURL := input
			value := "https://example.test/pulls/not-authorized"
			wrongURL.ResultURL = &value
			if _, err := opened.ApplyPublicationRecovery(
				guardContext,
				captured,
				wrongURL,
			); !errors.Is(err, ErrAutomationRecoveryScope) {
				return fmt.Errorf("wrong URL permit error: %w", err)
			}
		}
		var applyErr error
		applied, applyErr = opened.ApplyPublicationRecovery(
			guardContext,
			captured,
			input,
		)
		return applyErr
	}

	cleared, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	)
	if err != nil || !changed || cleared.Active || !applied.Changed ||
		applied.Publication.Status != model.PublicationPublished {
		t.Fatalf("guarded recovery: gate=%+v changed=%v result=%+v err=%v",
			cleared, changed, applied, err)
	}
	expired := captured
	copied := *expired
	for name, permit := range map[string]*AutomationRecoveryPermit{
		"captured": expired,
		"copy":     &copied,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := opened.ApplyPublicationRecovery(
				ctx,
				permit,
				input,
			); !errors.Is(err, ErrAutomationRecoveryPermit) {
				t.Fatalf("expired permit error=%v", err)
			}
		})
	}

	replayedGate, changed, err := opened.ConfirmAutomationQuarantine(
		ctx,
		confirmation,
	)
	if err != nil || changed || replayedGate.Active || guardCalls != 2 ||
		applied.Changed {
		t.Fatalf("inactive guarded replay: gate=%+v changed=%v calls=%d result=%+v err=%v",
			replayedGate, changed, guardCalls, applied, err)
	}
}

func TestApplyPublicationRecoveryTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		outcome    PublicationRecoveryOutcome
		wantStatus model.PublicationStatus
		eventKind  string
	}{
		{
			name:       "published",
			outcome:    PublicationRecoveryPublished,
			wantStatus: model.PublicationPublished,
			eventKind:  "publication_completed",
		},
		{
			name:       "failed",
			outcome:    PublicationRecoveryFailed,
			wantStatus: model.PublicationFailed,
			eventKind:  "publication_failed",
		},
		{
			name:       "superseded",
			outcome:    PublicationRecoverySuperseded,
			wantStatus: model.PublicationSuperseded,
			eventKind:  "publication_superseded",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			opened, err := Open(":memory:", "default", "")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			claimed := claimPublicationForRecovery(t, opened, test.name)
			input := publicationRecoveryInput(claimed, index+1, test.outcome)

			result, err := applyPublicationRecoveryForTest(opened, ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed ||
				result.Publication.Status != test.wantStatus ||
				result.Publication.ClaimEpoch != claimed.ClaimEpoch ||
				result.Receipt.SourceKey != input.SourceKey ||
				result.Receipt.ResultUpdatedAt != result.Publication.UpdatedAt ||
				result.Receipt.ObservedClaimEpoch != claimed.ClaimEpoch {
				t.Fatalf("recovery result = %+v", result)
			}
			switch test.outcome {
			case PublicationRecoveryPublished:
				if !sameOptionalString(result.Publication.URL, input.ResultURL) ||
					result.Publication.Error != nil ||
					result.Publication.PublishedAt == nil {
					t.Fatalf("published recovery = %+v", result.Publication)
				}
			case PublicationRecoveryFailed, PublicationRecoverySuperseded:
				if result.Publication.URL != nil ||
					result.Publication.Error == nil ||
					*result.Publication.Error != input.Reason {
					t.Fatalf("failed/superseded recovery = %+v", result.Publication)
				}
			}

			replayed, err := applyPublicationRecoveryForTest(opened, ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if replayed.Changed ||
				!reflect.DeepEqual(replayed.Receipt, result.Receipt) ||
				replayed.Publication.UpdatedAt != result.Publication.UpdatedAt {
				t.Fatalf("idempotent replay = %+v", replayed)
			}
			receipt, found, err := opened.GetPublicationRecoveryReceipt(
				ctx,
				input.SourceKey,
			)
			if err != nil || !found || !reflect.DeepEqual(receipt, result.Receipt) {
				t.Fatalf("get receipt: found=%v value=%+v err=%v", found, receipt, err)
			}

			events, err := opened.ListEvents(ctx, EventFilter{
				TaskID: claimed.TaskID,
				Kinds:  []string{test.eventKind},
			})
			if err != nil || len(events) != 1 {
				t.Fatalf("terminal events = %+v, err=%v", events, err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), claimed.ClaimToken) ||
				strings.Contains(string(encoded), claimed.RepositoryPath) ||
				strings.Contains(string(encoded), claimed.WorktreePath) ||
				strings.Contains(string(encoded), "claimToken") ||
				strings.Contains(string(encoded), "claimExpiresAt") ||
				strings.Contains(string(encoded), "repositoryPath") ||
				strings.Contains(string(encoded), "worktreePath") ||
				strings.Contains(string(encoded), "policySnapshot") ||
				strings.Contains(string(encoded), "sourceSnapshot") {
				t.Fatalf("recovery result leaked execution state: %s", encoded)
			}
			var receiptCount int
			if err := opened.db.QueryRowContext(
				ctx,
				`SELECT COUNT(*) FROM publication_recovery_receipts
				 WHERE source_key = ?`,
				input.SourceKey,
			).Scan(&receiptCount); err != nil || receiptCount != 1 {
				t.Fatalf("receipt count=%d err=%v", receiptCount, err)
			}
		})
	}
}

func TestPublicationRecoveryReceiptReplaySurvivesTaskDeletion(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claimed := claimPublicationForRecovery(t, opened, "deleted_task_replay")
	input := publicationRecoveryInput(
		claimed,
		111,
		PublicationRecoveryFailed,
	)
	applied, err := applyPublicationRecoveryForTest(opened, ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.DeleteTask(ctx, claimed.TaskID); err != nil {
		t.Fatalf("delete recovered task: %v", err)
	}
	if _, err := opened.GetPublication(ctx, claimed.ID); !errors.Is(
		err,
		ErrPublicationNotFound,
	) {
		t.Fatalf("publication after task deletion error=%v", err)
	}

	replayed, err := applyPublicationRecoveryForTest(opened, ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Changed || replayed.Publication.Present ||
		replayed.Publication.ID != claimed.ID ||
		replayed.Publication.Board != opened.board ||
		replayed.Publication.Status != model.PublicationFailed ||
		replayed.Publication.ClaimEpoch != claimed.ClaimEpoch ||
		replayed.Publication.UpdatedAt != applied.Receipt.ResultUpdatedAt ||
		replayed.Publication.Error == nil ||
		*replayed.Publication.Error != input.Reason ||
		!reflect.DeepEqual(replayed.Receipt, applied.Receipt) {
		t.Fatalf("receipt-only replay=%+v", replayed)
	}
}

func TestApplyPublicationRecoveryRejectsWrongOrUnresolvedTuple(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	claimed := claimPublicationForRecovery(t, opened, "wrong_tuple")
	tests := []struct {
		name   string
		mutate func(*PublicationRecoveryInput)
	}{
		{
			name: "updated at",
			mutate: func(input *PublicationRecoveryInput) {
				input.ObservedUpdatedAt = "2026-01-01T00:00:00Z"
			},
		},
		{
			name: "claim epoch",
			mutate: func(input *PublicationRecoveryInput) {
				input.ObservedClaimEpoch++
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := publicationRecoveryInput(
				claimed,
				20+index,
				PublicationRecoveryFailed,
			)
			test.mutate(&input)
			if _, err := applyPublicationRecoveryForTest(opened,
				ctx,
				input,
			); !errors.Is(err, ErrPublicationRecoveryConflict) {
				t.Fatalf("wrong tuple error = %v", err)
			}
		})
	}

	changedAt := now()
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publications SET updated_at = ? WHERE id = ?",
		changedAt,
		claimed.ID,
	); err != nil {
		t.Fatal(err)
	}
	superseded := publicationRecoveryInput(
		claimed,
		30,
		PublicationRecoverySuperseded,
	)
	if _, err := applyPublicationRecoveryForTest(opened,
		ctx,
		superseded,
	); !errors.Is(err, ErrPublicationRecoveryConflict) {
		t.Fatalf("different Publishing tuple supersede error = %v", err)
	}
	var status model.PublicationStatus
	var token string
	if err := opened.db.QueryRowContext(
		ctx,
		"SELECT status, claim_token FROM publications WHERE id = ?",
		claimed.ID,
	).Scan(&status, &token); err != nil {
		t.Fatal(err)
	}
	if status != model.PublicationPublishing || token != claimed.ClaimToken {
		t.Fatalf("wrong tuple mutated publication: status=%s tokenChanged=%v",
			status, token != claimed.ClaimToken)
	}

	_, pendingChangeSet := createPublicationSource(
		t,
		opened,
		"recovery_pending_conflict",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(pendingChangeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	pendingInput := publicationRecoveryInput(
		pending,
		31,
		PublicationRecoveryPublished,
	)
	pendingInput.ObservedClaimEpoch = 1
	if _, err := applyPublicationRecoveryForTest(opened,
		ctx,
		pendingInput,
	); !errors.Is(err, ErrPublicationRecoveryConflict) {
		t.Fatalf("pending publication recovery error = %v", err)
	}
	var receipts int
	if err := opened.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM publication_recovery_receipts",
	).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("wrong tuple wrote %d receipts, err=%v", receipts, err)
	}
}

func TestApplyPublicationRecoveryAdoptsExactTerminalResult(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claimed := claimPublicationForRecovery(t, opened, "terminal_adopt")
	input := publicationRecoveryInput(
		claimed,
		40,
		PublicationRecoveryPublished,
	)
	terminalAt := now()
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE publications
		SET status = 'published', url = ?, error = NULL, published_at = ?,
			claim_token = NULL, claim_expires_at = NULL, updated_at = ?
		WHERE id = ?
	`, *input.ResultURL, terminalAt, terminalAt, claimed.ID); err != nil {
		t.Fatal(err)
	}

	result, err := applyPublicationRecoveryForTest(opened, ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed ||
		result.Publication.Status != model.PublicationPublished ||
		result.Publication.UpdatedAt != terminalAt ||
		result.Receipt.ResultUpdatedAt != terminalAt {
		t.Fatalf("adopted terminal result = %+v", result)
	}

	conflicting := input
	conflicting.SourceKey = fmt.Sprintf("%064x", 41)
	conflicting.ResultURL = stringPointer(sql.NullString{
		String: "https://example.test/pulls/different",
		Valid:  true,
	})
	if _, err := applyPublicationRecoveryForTest(opened,
		ctx,
		conflicting,
	); !errors.Is(err, ErrPublicationRecoveryConflict) {
		t.Fatalf("different terminal result error = %v", err)
	}
}

func TestPublicationRecoveryReceiptReplayConflictsOnAnyChangedEvidence(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claimed := claimPublicationForRecovery(t, opened, "replay_conflict")
	input := publicationRecoveryInput(
		claimed,
		50,
		PublicationRecoveryPublished,
	)
	result, err := applyPublicationRecoveryForTest(opened, ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	changes := []struct {
		name   string
		mutate func(*PublicationRecoveryInput)
	}{
		{"generation", func(value *PublicationRecoveryInput) { value.FirstGeneration++ }},
		{"publication", func(value *PublicationRecoveryInput) { value.PublicationID = "pub_other" }},
		{"updatedAt", func(value *PublicationRecoveryInput) { value.ObservedUpdatedAt += "1" }},
		{"claimEpoch", func(value *PublicationRecoveryInput) { value.ObservedClaimEpoch++ }},
		{"actor", func(value *PublicationRecoveryInput) { value.Actor = "another-operator" }},
		{"reason", func(value *PublicationRecoveryInput) { value.Reason = "another reason" }},
		{"result", func(value *PublicationRecoveryInput) {
			rawURL := "https://example.test/pulls/other"
			value.ResultURL = &rawURL
		}},
		{"outcome", func(value *PublicationRecoveryInput) {
			value.Outcome = PublicationRecoveryFailed
			value.Disposition = AutomationSourceAbandoned
			value.ResultURL = nil
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			changed := input
			change.mutate(&changed)
			if _, err := applyPublicationRecoveryForTest(opened,
				ctx,
				changed,
			); !errors.Is(err, ErrPublicationRecoveryConflict) {
				t.Fatalf("changed receipt replay error = %v", err)
			}
		})
	}

	mutatedAt := now()
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publications SET updated_at = ? WHERE id = ?",
		mutatedAt,
		claimed.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := applyPublicationRecoveryForTest(opened,
		ctx,
		input,
	); !errors.Is(err, ErrPublicationRecoveryConflict) {
		t.Fatalf("post-receipt publication mutation error = %v", err)
	}
	receipt, found, err := opened.GetPublicationRecoveryReceipt(ctx, input.SourceKey)
	if err != nil || !found || !reflect.DeepEqual(receipt, result.Receipt) {
		t.Fatalf("conflict changed receipt: found=%v receipt=%+v err=%v",
			found, receipt, err)
	}
}

func TestApplyPublicationRecoveryRollsBackTerminalMutationAndReceipt(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claimed := claimPublicationForRecovery(t, opened, "rollback")
	input := publicationRecoveryInput(
		claimed,
		60,
		PublicationRecoveryFailed,
	)
	if _, err := opened.db.ExecContext(ctx, `
		CREATE TRIGGER reject_recovery_event
		BEFORE INSERT ON task_events
		WHEN NEW.kind = 'publication_failed'
		BEGIN
			SELECT RAISE(ABORT, 'reject recovery event');
		END
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := applyPublicationRecoveryForTest(opened, ctx, input); err == nil ||
		!strings.Contains(err.Error(), "reject recovery event") {
		t.Fatalf("rollback recovery error = %v", err)
	}
	var status model.PublicationStatus
	var epoch int64
	var token, expiry sql.NullString
	if err := opened.db.QueryRowContext(ctx, `
		SELECT status, claim_epoch, claim_token, claim_expires_at
		FROM publications WHERE id = ?
	`, claimed.ID).Scan(&status, &epoch, &token, &expiry); err != nil {
		t.Fatal(err)
	}
	if status != model.PublicationPublishing ||
		epoch != claimed.ClaimEpoch ||
		!token.Valid || token.String != claimed.ClaimToken ||
		!expiry.Valid {
		t.Fatalf("rolled-back publication: status=%s epoch=%d token=%v expiry=%v",
			status, epoch, token.Valid, expiry.Valid)
	}
	if _, found, err := opened.GetPublicationRecoveryReceipt(
		ctx,
		input.SourceKey,
	); err != nil || found {
		t.Fatalf("rolled-back receipt: found=%v err=%v", found, err)
	}
}

func TestApplyPublicationRecoveryDoesNotDependOnStoredClaimCredential(t *testing.T) {
	tests := []PublicationRecoveryOutcome{
		PublicationRecoveryPublished,
		PublicationRecoveryFailed,
	}
	for index, outcome := range tests {
		t.Run(string(outcome), func(t *testing.T) {
			ctx := context.Background()
			opened, err := Open(":memory:", "default", "")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			claimed := claimPublicationForRecovery(
				t,
				opened,
				"missing_credential_"+string(outcome),
			)
			conn, err := opened.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(
				ctx,
				"PRAGMA ignore_check_constraints = ON",
			); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, `
				UPDATE publications
				SET claim_token = NULL, claim_expires_at = NULL
				WHERE id = ?
			`, claimed.ID); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(
				ctx,
				"PRAGMA ignore_check_constraints = OFF",
			); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}

			input := publicationRecoveryInput(claimed, 90+index, outcome)
			result, err := applyPublicationRecoveryForTest(opened, ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Publication.Status == model.PublicationPublishing ||
				result.Publication.ClaimEpoch != claimed.ClaimEpoch {
				t.Fatalf("credential-free recovery = %+v", result)
			}
		})
	}
}

func TestRetryAndSupersedePublicationRespectAutomationQuarantine(t *testing.T) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	claimed := claimPublicationForRecovery(t, opened, "gate_guard_retry")
	failedInput := publicationRecoveryInput(
		claimed,
		100,
		PublicationRecoveryFailed,
	)
	failed, err := applyPublicationRecoveryForTest(opened, ctx, failedInput)
	if err != nil {
		t.Fatal(err)
	}

	exclusive, err := acquireAutomationFileLock(
		ctx,
		opened.automation.lockPath,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		_, err := opened.RetryPublication(
			blockedContext,
			failed.Publication.ID,
			RetryPublicationInput{
				ExpectedUpdatedAt: failed.Publication.UpdatedAt,
			},
		)
		blocked <- err
	}()
	if err := <-blocked; !errors.Is(err, context.DeadlineExceeded) {
		exclusive.Close()
		t.Fatalf("retry behind recovery lock error = %v", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}

	_, _ = activateAutomationTestSource(
		t,
		opened,
		"gate-guard-retry",
		"1",
	)
	if _, err := opened.RetryPublication(
		ctx,
		failed.Publication.ID,
		RetryPublicationInput{
			ExpectedUpdatedAt: failed.Publication.UpdatedAt,
		},
	); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("retry during quarantine error = %v", err)
	}

	_, supersedeChangeSet := createPublicationSource(
		t,
		opened,
		"recovery_gate_guard_supersede",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(supersedeChangeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SupersedePublication(
		ctx,
		pending.ID,
		SupersedePublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			Reason:            "operator rejected publication",
		},
	); !errors.Is(err, ErrAutomationQuarantined) {
		t.Fatalf("supersede during quarantine error = %v", err)
	}
}

func TestPublicationRecoveryReceiptSchemaIsStrictAndImmutable(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claimed := claimPublicationForRecovery(t, opened, "immutable")
	input := publicationRecoveryInput(
		claimed,
		70,
		PublicationRecoverySuperseded,
	)
	if _, err := applyPublicationRecoveryForTest(opened, ctx, input); err != nil {
		t.Fatal(err)
	}
	unknownDisposition := input
	unknownDisposition.SourceKey = fmt.Sprintf("%064x", 73)
	unknownDisposition.Disposition = AutomationSourceDisposition("unknown")
	if _, err := applyPublicationRecoveryForTest(opened,
		ctx,
		unknownDisposition,
	); err == nil || !strings.Contains(err.Error(), "requires superseded disposition") {
		t.Fatalf("unknown recovery disposition error = %v", err)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		`UPDATE publication_recovery_receipts
		 SET actor = 'changed' WHERE source_key = ?`,
		input.SourceKey,
	); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("receipt update error = %v", err)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"DELETE FROM publication_recovery_receipts WHERE source_key = ?",
		input.SourceKey,
	); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("receipt delete error = %v", err)
	}
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id,
			observed_updated_at, observed_claim_epoch, outcome,
			disposition, result_url, actor, reason, recovered_at,
			result_updated_at
		) VALUES (?, 1, 'pub_invalid', 'observed', 1, 'published',
			'abandoned', NULL, 'actor', 'reason', 'recovered', 'result')
	`, fmt.Sprintf("%064x", 71)); err == nil {
		t.Fatal("schema accepted invalid outcome/disposition coupling")
	}
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id,
			observed_updated_at, observed_claim_epoch, outcome,
			disposition, result_url, actor, reason, recovered_at,
			result_updated_at
		) VALUES (?, 1.5, 'pub_invalid', 'observed', 1, 'failed',
			'abandoned', NULL, 'actor', 'reason', 'recovered', 'result')
	`, fmt.Sprintf("%064x", 72)); err == nil {
		t.Fatal("schema accepted non-integer first generation")
	}
	var tokenColumnCount int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_table_info('publication_recovery_receipts')
		WHERE lower(name) LIKE '%token%'
	`).Scan(&tokenColumnCount); err != nil || tokenColumnCount != 0 {
		t.Fatalf("receipt token columns=%d err=%v", tokenColumnCount, err)
	}
}

func TestPublicationRecoveryReaderFindsTokenFreeReceiptAndResult(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "reader.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimPublicationForRecovery(t, opened, "reader")
	input := publicationRecoveryInput(
		claimed,
		80,
		PublicationRecoveryPublished,
	)
	applied, err := applyPublicationRecoveryForTest(opened, ctx, input)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenPublicationRecoveryReader(ctx, dbPath, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	receipt, found, err := reader.GetPublicationRecoveryReceipt(
		ctx,
		input.SourceKey,
	)
	if err != nil || !found || !reflect.DeepEqual(receipt, applied.Receipt) {
		t.Fatalf("reader receipt: found=%v value=%+v err=%v", found, receipt, err)
	}
	publication, found, err := reader.GetPublicationForRecovery(
		ctx,
		claimed.ID,
	)
	if err != nil || !found ||
		publication.Status != model.PublicationPublished ||
		!sameOptionalString(publication.URL, input.ResultURL) ||
		publication.ClaimToken != "" {
		t.Fatalf("reader publication: found=%v value=%+v err=%v",
			found, publication, err)
	}
	encoded, err := json.Marshal(struct {
		Receipt     PublicationRecoveryReceipt `json:"receipt"`
		Publication model.Publication          `json:"publication"`
	}{receipt, publication})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), claimed.ClaimToken) ||
		strings.Contains(string(encoded), "claimToken") {
		t.Fatalf("reader leaked claim token: %s", encoded)
	}
}

func TestPublicationRecoveryReaderRejectsNonCanonicalCurrentResult(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "malformed-reader.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimPublicationForRecovery(t, opened, "malformed_reader")
	conn, err := opened.db.Conn(ctx)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(
		ctx,
		"PRAGMA ignore_check_constraints = ON",
	); err != nil {
		conn.Close()
		opened.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(
		ctx,
		"UPDATE publications SET url = ' padded-result ' WHERE id = ?",
		claimed.ID,
	); err != nil {
		conn.Close()
		opened.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenPublicationRecoveryReader(ctx, dbPath, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, _, err := reader.GetPublicationForRecovery(
		ctx,
		claimed.ID,
	); err == nil || !strings.Contains(err.Error(), "not stored canonically") {
		t.Fatalf("non-canonical recovery result error = %v", err)
	}
}

func TestPublicationRecoveryReaderRejectsMutableReceiptSchema(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mutable-receipt.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		ctx,
		"DROP TRIGGER publication_recovery_receipts_prevent_delete",
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenPublicationRecoveryReader(ctx, dbPath, "default")
	if err == nil {
		reader.Close()
		t.Fatal("mutable receipt schema was accepted")
	}
	if !strings.Contains(err.Error(), "immutable triggers are missing") {
		t.Fatalf("mutable receipt schema error = %v", err)
	}
}
