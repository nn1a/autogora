package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func createPublicationAttemptRecoveryDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutatePublicationAttemptRecoveryDatabase(
	t *testing.T,
	path string,
	query string,
) {
	t.Helper()
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(context.Background(), query); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalPublicationAttemptRecoveryTimestampAcceptsDurableFormats(
	t *testing.T,
) {
	t.Parallel()
	for _, value := range []string{
		"2026-07-24T01:02:03Z",
		"2026-07-24T01:02:03.8488666Z",
		"2026-07-24T01:02:03.848866600Z",
		"2026-07-24T01:02:03.000000000Z",
	} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			got, err := canonicalPublicationAttemptRecoveryTimestamp(
				value,
				"timestamp",
				true,
			)
			if err != nil || got != value {
				t.Fatalf("timestamp=%q err=%v", got, err)
			}
		})
	}
}

func TestCanonicalPublicationAttemptRecoveryTimestampRejectsAlternateForms(
	t *testing.T,
) {
	t.Parallel()
	for _, value := range []string{
		" 2026-07-24T01:02:03Z",
		"2026-07-24T01:02:03+00:00",
		"2026-07-24T01:02:03.0Z",
		"2026-07-24T01:02:03.84886660Z",
	} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := canonicalPublicationAttemptRecoveryTimestamp(
				value,
				"timestamp",
				true,
			); err == nil {
				t.Fatalf("timestamp %q unexpectedly accepted", value)
			}
		})
	}
}

func TestPublicationAttemptRecoveryReaderReportsPreV28Unsupported(
	t *testing.T,
) {
	ctx := context.Background()
	path := createPublicationAttemptRecoveryDatabase(t)
	mutatePublicationAttemptRecoveryDatabase(t, path, `
		DROP TABLE publication_attempt_results;
		DROP TABLE publication_attempt_intents;
		PRAGMA user_version = 27;
	`)
	reader, err := OpenPublicationRecoveryReader(ctx, path, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	values, next, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{
				AfterStartedAt: "not evaluated for an unsupported schema",
				AfterID:        "also not evaluated",
			},
		)
	if err != nil || supported || len(values) != 0 ||
		next != (PublicationAttemptRecoveryCursor{}) {
		t.Fatalf(
			"pre-v28 page values=%+v next=%+v supported=%v err=%v",
			values,
			next,
			supported,
			err,
		)
	}
}

func TestPublicationAttemptRecoveryReaderRejectsPartialPreV28Schema(
	t *testing.T,
) {
	ctx := context.Background()
	path := createPublicationAttemptRecoveryDatabase(t)
	mutatePublicationAttemptRecoveryDatabase(t, path, `
		DROP TABLE publication_attempt_results;
		PRAGMA user_version = 27;
	`)
	reader, err := OpenPublicationRecoveryReader(ctx, path, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, _, _, err = reader.ListPublicationAttemptRecoveryPage(
		ctx,
		PublicationAttemptFilter{},
	)
	if err == nil || !strings.Contains(err.Error(), "partially present") {
		t.Fatalf("partial pre-v28 schema error = %v", err)
	}
}

func TestPublicationAttemptRecoveryReaderAcceptsCompleteLedgerWithStaleVersion(
	t *testing.T,
) {
	ctx := context.Background()
	path := createPublicationAttemptRecoveryDatabase(t)
	mutatePublicationAttemptRecoveryDatabase(
		t,
		path,
		"PRAGMA user_version = 27",
	)
	reader, err := OpenPublicationRecoveryReader(ctx, path, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	values, next, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{},
		)
	if err != nil || !supported || len(values) != 0 ||
		next != (PublicationAttemptRecoveryCursor{}) {
		t.Fatalf(
			"complete stale-version page=%+v next=%+v supported=%v err=%v",
			values,
			next,
			supported,
			err,
		)
	}
}

func TestPublicationAttemptRecoveryReaderRejectsMalformedV28Schema(
	t *testing.T,
) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate string
		want   string
	}{
		{
			name: "missing immutable trigger",
			mutate: `
				DROP TRIGGER publication_attempt_intents_prevent_update;
			`,
			want: "incomplete",
		},
		{
			name: "wrong keyset index",
			mutate: `
				DROP INDEX idx_publication_attempt_intents_board_started;
				CREATE INDEX idx_publication_attempt_intents_board_started
				ON publication_attempt_intents(board, id, started_at);
			`,
			want: "incompatible columns",
		},
		{
			name: "forged immutable trigger",
			mutate: `
				DROP TRIGGER publication_attempt_results_prevent_delete;
				CREATE TRIGGER publication_attempt_results_prevent_delete
				BEFORE DELETE ON publication_attempt_results
				BEGIN
					SELECT 1;
				END;
			`,
			want: "incompatible SQL contract",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := createPublicationAttemptRecoveryDatabase(t)
			mutatePublicationAttemptRecoveryDatabase(t, path, test.mutate)
			reader, err := OpenPublicationRecoveryReader(
				ctx,
				path,
				"default",
			)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			_, _, _, err = reader.ListPublicationAttemptRecoveryPage(
				ctx,
				PublicationAttemptFilter{},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("malformed v28 schema error = %v", err)
			}
		})
	}
}

func insertPublicationAttemptRecoveryIntent(
	t *testing.T,
	opened *Store,
	id string,
	startedAt string,
) PublicationAttemptIntent {
	t.Helper()
	intent := PublicationAttemptIntent{
		ID:                   id,
		Board:                "default",
		PublicationID:        "pub_" + strings.TrimPrefix(id, "pat_"),
		ChangeSetID:          "chg_" + strings.TrimPrefix(id, "pat_"),
		Mode:                 model.PublicationModeLocalFF,
		TargetBranch:         "main",
		Remote:               "origin",
		BaseCommit:           "base-" + id,
		HeadCommit:           "head-" + id,
		DurableRef:           "refs/autogora/" + id,
		ClaimEpoch:           1,
		PublicationUpdatedAt: startedAt,
		ClaimExpiresAt: time.Date(
			2026,
			7,
			25,
			0,
			0,
			0,
			0,
			time.UTC,
		).Format(time.RFC3339Nano),
		SessionID:      "dispatcher-pagination",
		GateGeneration: 1,
		StartedAt:      startedAt,
	}
	intent.SourceKey = publicationAttemptSourceKey(
		intent.Board,
		intent.PublicationID,
		intent.PublicationUpdatedAt,
		intent.ClaimEpoch,
	)
	intent.EffectFingerprint = publicationEffectFingerprint(intent)
	if _, err := opened.db.ExecContext(context.Background(), `
		INSERT INTO publication_attempt_intents(
			id, board, publication_id, source_key, change_set_id, mode,
			target_branch, remote, base_commit, head_commit, durable_ref,
			effect_fingerprint, claim_epoch, publication_updated_at,
			claim_expires_at, session_id, gate_generation, started_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		intent.ID,
		intent.Board,
		intent.PublicationID,
		intent.SourceKey,
		intent.ChangeSetID,
		intent.Mode,
		intent.TargetBranch,
		intent.Remote,
		intent.BaseCommit,
		intent.HeadCommit,
		intent.DurableRef,
		intent.EffectFingerprint,
		intent.ClaimEpoch,
		intent.PublicationUpdatedAt,
		intent.ClaimExpiresAt,
		intent.SessionID,
		intent.GateGeneration,
		intent.StartedAt,
	); err != nil {
		t.Fatal(err)
	}
	return intent
}

func TestPublicationAttemptRecoveryReaderUsesBoundedMonotonicPages(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	intents := make([]PublicationAttemptIntent, 0, 102)
	for index := 0; index < 102; index++ {
		intents = append(
			intents,
			insertPublicationAttemptRecoveryIntent(
				t,
				opened,
				fmt.Sprintf("pat_%03d", index),
				base.Format(time.RFC3339Nano),
			),
		)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenPublicationRecoveryReader(ctx, path, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	first, next, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{Limit: 1000},
		)
	if err != nil || !supported || len(first) != 100 {
		t.Fatalf(
			"first page len=%d next=%+v supported=%v err=%v",
			len(first),
			next,
			supported,
			err,
		)
	}
	if next.StartedAt != intents[99].StartedAt ||
		next.ID != intents[99].ID {
		t.Fatalf("first next = %+v, want intent 99", next)
	}
	for index, observation := range first {
		if observation.Attempt.Intent.ID != intents[index].ID ||
			observation.Publication.Present {
			t.Fatalf("first[%d] = %+v", index, observation)
		}
		if index > 0 && !publicationAttemptRecoveryKeyAfter(
			observation.Attempt.Intent.StartedAt,
			observation.Attempt.Intent.ID,
			PublicationAttemptRecoveryCursor{
				StartedAt: first[index-1].Attempt.Intent.StartedAt,
				ID:        first[index-1].Attempt.Intent.ID,
			},
		) {
			t.Fatalf("first page did not advance at %d", index)
		}
	}

	second, finalNext, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{
				AfterStartedAt: next.StartedAt,
				AfterID:        next.ID,
				Limit:          100,
			},
		)
	if err != nil || !supported || len(second) != 2 ||
		finalNext != (PublicationAttemptRecoveryCursor{}) ||
		second[0].Attempt.Intent.ID != intents[100].ID ||
		second[1].Attempt.Intent.ID != intents[101].ID {
		t.Fatalf(
			"second page=%+v next=%+v supported=%v err=%v",
			second,
			finalNext,
			supported,
			err,
		)
	}
	eof, eofNext, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{
				AfterStartedAt: intents[101].StartedAt,
				AfterID:        intents[101].ID,
			},
		)
	if err != nil || !supported || len(eof) != 0 ||
		eofNext != (PublicationAttemptRecoveryCursor{}) {
		t.Fatalf(
			"EOF page=%+v next=%+v supported=%v err=%v",
			eof,
			eofNext,
			supported,
			err,
		)
	}
}

func TestPublicationAttemptRecoveryReaderObservesReceiptAndCurrentState(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"reader_observation",
	)
	claimed, operation, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-reader-observation",
		time.Minute,
	)
	intent := operation.Intent()
	claimCredential := operation.state.claimToken
	unknown, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		operation,
		PublicationAttemptResultInput{
			Outcome:        PublicationAttemptUnknown,
			ExecutorStatus: PublicationExecutorUnknown,
			ErrorKind:      PublicationErrorCommandStartUncertain,
			Error:          "external effect cannot be proven",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if released, err := opened.ReleaseAutomationDispatcherSession(
		ctx,
		lease,
	); err != nil || !released {
		opened.Close()
		t.Fatalf("release observation session=%v err=%v", released, err)
	}
	recoveredAt := now()
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id,
			observed_updated_at, observed_claim_epoch, outcome,
			disposition, result_url, actor, reason, recovered_at,
			result_updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
	`,
		intent.SourceKey,
		1,
		intent.PublicationID,
		intent.PublicationUpdatedAt,
		intent.ClaimEpoch,
		PublicationRecoveryFailed,
		AutomationSourceAbandoned,
		"operator",
		"remote operation failed",
		recoveredAt,
		recoveredAt,
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}

	_, knownPending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"reader_known",
	)
	knownClaimed, knownOperation, knownLease := beginPublicationAttemptFixture(
		t,
		opened,
		knownPending,
		"publisher-reader-known",
		time.Minute,
	)
	knownIntent := knownOperation.Intent()
	knownResult, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		knownOperation,
		PublicationAttemptResultInput{
			Outcome:        PublicationAttemptFailed,
			ExecutorStatus: PublicationExecutorFailed,
			ErrorKind:      PublicationErrorRemoteConflict,
			Error:          "known remote conflict",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if released, err := opened.ReleaseAutomationDispatcherSession(
		ctx,
		knownLease,
	); err != nil || !released {
		opened.Close()
		t.Fatalf("release known session=%v err=%v", released, err)
	}
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE publications
		SET status = 'publishing', error = NULL, url = NULL,
			claim_token = 'forged-known-result-claim',
			claim_expires_at = ?, published_at = NULL, updated_at = ?
		WHERE id = ?
	`,
		knownIntent.ClaimExpiresAt,
		knownIntent.PublicationUpdatedAt,
		knownIntent.PublicationID,
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenPublicationRecoveryReader(ctx, path, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	values, next, supported, err :=
		reader.ListPublicationAttemptRecoveryPage(
			ctx,
			PublicationAttemptFilter{},
		)
	if err != nil || !supported || len(values) != 1 ||
		next != (PublicationAttemptRecoveryCursor{}) {
		t.Fatalf(
			"observation page=%+v next=%+v supported=%v err=%v",
			values,
			next,
			supported,
			err,
		)
	}
	observation := values[0]
	if observation.Attempt.Intent != intent ||
		observation.Attempt.Result == nil ||
		!reflect.DeepEqual(*observation.Attempt.Result, unknown) ||
		observation.Attempt.RecoveryReceipt == nil ||
		observation.Attempt.RecoveryReceipt.SourceKey != intent.SourceKey ||
		!observation.Publication.Present ||
		observation.Publication.ID != claimed.ID ||
		observation.Publication.Board != intent.Board ||
		observation.Publication.Status != model.PublicationPublishing ||
		observation.Publication.ChangeSetID != intent.ChangeSetID ||
		observation.Publication.Mode != intent.Mode ||
		observation.Publication.TargetBranch != intent.TargetBranch ||
		observation.Publication.Remote != intent.Remote ||
		observation.Publication.BaseCommit != intent.BaseCommit ||
		observation.Publication.HeadCommit != intent.HeadCommit ||
		observation.Publication.DurableRef != intent.DurableRef ||
		observation.Publication.ClaimEpoch != intent.ClaimEpoch ||
		observation.Publication.UpdatedAt != intent.PublicationUpdatedAt {
		t.Fatalf("observation = %+v", observation)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if claimCredential == "" ||
		strings.Contains(string(encoded), claimCredential) ||
		strings.Contains(string(encoded), "claimToken") {
		t.Fatalf("observation leaked claim token: %s", encoded)
	}

	exact, found, supported, err :=
		reader.GetPublicationAttemptRecoveryObservation(
			ctx,
			intent.ID,
		)
	if err != nil || !found || !supported ||
		!reflect.DeepEqual(exact, observation) {
		t.Fatalf(
			"exact observation=%+v found=%v supported=%v err=%v",
			exact,
			found,
			supported,
			err,
		)
	}
	known, found, supported, err :=
		reader.GetPublicationAttemptRecoveryObservationForPublication(
			ctx,
			knownIntent.PublicationID,
			knownIntent.PublicationUpdatedAt,
			knownIntent.ClaimEpoch,
		)
	if err != nil || !found || !supported ||
		known.Attempt.Intent != knownIntent ||
		known.Attempt.Result == nil ||
		!reflect.DeepEqual(*known.Attempt.Result, knownResult) ||
		!known.Publication.Present ||
		known.Publication.ID != knownClaimed.ID ||
		known.Publication.Status != model.PublicationPublishing ||
		known.Publication.UpdatedAt !=
			knownIntent.PublicationUpdatedAt ||
		known.Publication.ClaimEpoch != knownIntent.ClaimEpoch {
		t.Fatalf(
			"known tuple=%+v found=%v supported=%v err=%v",
			known,
			found,
			supported,
			err,
		)
	}
	knownByID, found, supported, err :=
		reader.GetPublicationAttemptRecoveryObservation(
			ctx,
			knownIntent.ID,
		)
	if err != nil || !found || !supported ||
		!reflect.DeepEqual(knownByID, known) {
		t.Fatalf(
			"known exact=%+v found=%v supported=%v err=%v",
			knownByID,
			found,
			supported,
			err,
		)
	}
}
