package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/publicationeffect"
)

func publicationEffectPrepareFixture(
	t *testing.T,
	sequence int64,
) PublicationEffectPrepareInput {
	t.Helper()
	afterOIDCharacter := byte('b' + sequence)
	descriptor, err := publicationeffect.NewLocalRefCAS(
		publicationeffect.LocalRefCASInput{
			GitCommonDirPath: "/repo/.git",
			TargetRef:        "refs/heads/main",
			BeforeOID:        strings.Repeat("a", 40),
			AfterOID: strings.Repeat(
				string(afterOIDCharacter),
				40,
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return PublicationEffectPrepareInput{
		Sequence:              sequence,
		Kind:                  PublicationEffectKind(descriptor.Kind()),
		DescriptorVersion:     int64(descriptor.Version()),
		Descriptor:            descriptor.CanonicalJSON(),
		DescriptorFingerprint: descriptor.Fingerprint(),
	}
}

func publicationEffectPushPrepareFixture(
	t *testing.T,
	sequence int64,
) PublicationEffectPrepareInput {
	t.Helper()
	descriptor, err := publicationeffect.NewPRBranchPush(
		publicationeffect.PRBranchPushInput{
			RepositoryIdentity: "github.com/nn1a/autogora",
			RemoteURL:          "git@github.com:nn1a/autogora.git",
			SourceOID:          strings.Repeat("b", 40),
			TargetRef: "refs/heads/autogora/" +
				fmt.Sprintf("publication-%d", sequence),
			ExpectedAbsent: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return PublicationEffectPrepareInput{
		Sequence:              sequence,
		Kind:                  PublicationEffectKind(descriptor.Kind()),
		DescriptorVersion:     int64(descriptor.Version()),
		Descriptor:            descriptor.CanonicalJSON(),
		DescriptorFingerprint: descriptor.Fingerprint(),
	}
}

func publicationEffectResultFixture(
	t *testing.T,
	outcome PublicationEffectOutcome,
) PublicationEffectResultInput {
	t.Helper()
	evidence, err := json.Marshal(map[string]any{
		"observedOID": strings.Repeat("c", 40),
		"ref":         "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := PublicationEffectResultInput{
		Outcome:             outcome,
		Evidence:            evidence,
		EvidenceFingerprint: publicationEffectJSONFingerprint(evidence),
	}
	if outcome == PublicationEffectUnknown {
		input.ErrorKind = "command_result_uncertain"
		input.ErrorDetailFingerprint = strings.Repeat("d", 64)
	}
	return input
}

func preparePublicationEffectFixture(
	t *testing.T,
	opened *Store,
	attempt *PublicationAttemptPermit,
	sequence int64,
) (PublicationEffectIntent, *PublicationEffectPermit) {
	t.Helper()
	intent, permit, created, err := opened.PreparePublicationEffect(
		context.Background(),
		attempt,
		publicationEffectPrepareFixture(t, sequence),
	)
	if err != nil || !created || permit == nil {
		t.Fatalf(
			"prepare publication effect = %+v, permit=%s, created=%v, err=%v",
			intent,
			permit,
			created,
			err,
		)
	}
	return intent, permit
}

func startPublicationEffectFixture(
	t *testing.T,
	opened *Store,
	lease AutomationDispatcherSessionLease,
	attempt *PublicationAttemptPermit,
	effect *PublicationEffectPermit,
) {
	t.Helper()
	automation, err := opened.AcquireAutomationPermitForSession(
		context.Background(),
		lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	released, startErr := opened.WithPublicationEffectCommandStart(
		context.Background(),
		automation,
		attempt,
		effect,
		func() (bool, error) { return true, nil },
	)
	closeErr := automation.Close()
	if startErr != nil || !released || closeErr != nil {
		t.Fatalf(
			"start publication effect released=%v, startErr=%v, closeErr=%v",
			released,
			startErr,
			closeErr,
		)
	}
}

func openPublicationEffectFileStore(
	t *testing.T,
) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "board.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close Store: %v", err)
		}
	})
	return opened, path
}

type publicationEffectFileFixture struct {
	store        *Store
	path         string
	attempt      *PublicationAttemptPermit
	lease        AutomationDispatcherSessionLease
	effectIntent PublicationEffectIntent
	effectPermit *PublicationEffectPermit
}

func newPublicationEffectFileFixture(
	t *testing.T,
	suffix string,
) publicationEffectFileFixture {
	t.Helper()
	opened, path := openPublicationEffectFileStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		suffix,
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-"+suffix,
		time.Minute,
	)
	intent, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	return publicationEffectFileFixture{
		store:        opened,
		path:         path,
		attempt:      attempt,
		lease:        lease,
		effectIntent: intent,
		effectPermit: effect,
	}
}

func closePublicationEffectFixtureForRawAccess(
	t *testing.T,
	fixture publicationEffectFileFixture,
) *sql.DB {
	t.Helper()
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(fixture.path))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func insertDirectKnownPublicationAttemptResult(
	t *testing.T,
	db *sql.DB,
	parent PublicationAttemptIntent,
) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO publication_attempt_results(
			attempt_id, board, publication_id, claim_epoch, outcome,
			executor_status, error_kind, result_url, error,
			publication_updated_at, recorded_at
		) VALUES (?, ?, ?, ?, 'failed', 'failed', 'command_failed',
			NULL, 'direct known result', ?, ?)
	`,
		parent.ID,
		parent.Board,
		parent.PublicationID,
		parent.ClaimEpoch,
		parent.PublicationUpdatedAt,
		"2026-07-24T15:30:00.000000000Z",
	); err != nil {
		t.Fatal(err)
	}
}

func requirePublicationEffectMigrationFailure(
	t *testing.T,
	path string,
	errorFragment string,
) {
	t.Helper()
	if reopened, err := Open(path, "default", ""); err == nil {
		reopened.Close()
		t.Fatal("corrupt publication effect ledger unexpectedly opened")
	} else if !strings.Contains(err.Error(), errorFragment) {
		t.Fatalf(
			"publication effect migration error = %v, want %q",
			err,
			errorFragment,
		)
	}
	inspection, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var version int
	if err := inspection.QueryRow(
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 30 {
		t.Fatalf(
			"failed migration changed schema version to %d",
			version,
		)
	}
}

func TestPublicationEffectPrepareStartFinishAndRecoveryView(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_lifecycle",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-lifecycle",
		time.Minute,
	)

	intent, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	if intent.ID == "" ||
		intent.AttemptID != attempt.Intent().ID ||
		intent.Board != "default" ||
		intent.PublicationID != pending.ID ||
		intent.ClaimEpoch != 1 ||
		intent.Sequence != 1 ||
		intent.Kind != PublicationEffectLocalRefCAS ||
		intent.DescriptorVersion != 1 ||
		intent.DescriptorFingerprint !=
			publicationEffectJSONFingerprint(intent.Descriptor) ||
		intent.IdentityFingerprint !=
			publicationEffectIdentityFingerprint(intent) ||
		intent.ParentEffectFingerprint != attempt.Intent().EffectFingerprint ||
		intent.ParentProvenanceFingerprint !=
			attempt.Intent().ExecutionProvenanceFingerprint ||
		len(intent.PreparedAt) != 30 {
		t.Fatalf("prepared intent = %+v", intent)
	}
	if bytes.Contains(intent.Descriptor, []byte("/repo/.git")) {
		t.Fatalf(
			"typed descriptor retained its local path: %s",
			intent.Descriptor,
		)
	}
	record, err := opened.GetPublicationEffect(ctx, intent.ID)
	if err != nil || record.Result != nil ||
		!samePublicationEffectIntent(record.Intent, intent) {
		t.Fatalf("get prepared effect = %+v, err=%v", record, err)
	}
	unresolved, err := opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{},
	)
	if err != nil || len(unresolved) != 1 ||
		unresolved[0].Intent.ID != intent.ID ||
		unresolved[0].Result != nil {
		t.Fatalf("unresolved effects = %+v, err=%v", unresolved, err)
	}

	startPublicationEffectFixture(t, opened, lease, attempt, effect)
	resultInput := publicationEffectResultFixture(
		t,
		PublicationEffectApplied,
	)
	result, err := opened.FinishPublicationEffect(
		ctx,
		effect,
		resultInput,
	)
	if err != nil ||
		result.EffectID != intent.ID ||
		result.AttemptID != intent.AttemptID ||
		result.Sequence != intent.Sequence ||
		result.IdentityFingerprint != intent.IdentityFingerprint ||
		result.Outcome != PublicationEffectApplied ||
		!reflect.DeepEqual(result.Evidence, resultInput.Evidence) ||
		result.EvidenceFingerprint != resultInput.EvidenceFingerprint ||
		result.ErrorKind != nil ||
		result.ErrorDetailFingerprint != nil ||
		len(result.RecordedAt) != 30 {
		t.Fatalf("finish effect = %+v, err=%v", result, err)
	}
	record, err = opened.GetPublicationEffect(ctx, intent.ID)
	if err != nil || record.Result == nil ||
		record.Result.RecordedAt != result.RecordedAt {
		t.Fatalf("get finished effect = %+v, err=%v", record, err)
	}
	unresolved, err = opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{},
	)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("resolved list = %+v, err=%v", unresolved, err)
	}
}

func TestPublicationEffectIntentIsCommittedBeforeCommandRelease(
	t *testing.T,
) {
	ctx := context.Background()
	opened, path := openPublicationEffectFileStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_commit_boundary",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-commit-boundary",
		time.Minute,
	)
	intent, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)

	observer, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	automation, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	var observed int
	released, err := opened.WithPublicationEffectCommandStart(
		ctx,
		automation,
		attempt,
		effect,
		func() (bool, error) {
			if err := observer.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM publication_effect_intents
				WHERE id = ? AND identity_fingerprint = ?
			`,
				intent.ID,
				intent.IdentityFingerprint,
			).Scan(&observed); err != nil {
				return false, err
			}
			return observed == 1, nil
		},
	)
	closeErr := automation.Close()
	if err != nil || closeErr != nil || !released || observed != 1 {
		t.Fatalf(
			"release observed=%d, released=%v, err=%v, closeErr=%v",
			observed,
			released,
			err,
			closeErr,
		)
	}
}

func TestPublicationEffectPrepareIsSequentialIdempotentAndConcurrent(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_idempotency",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-idempotency",
		time.Minute,
	)
	firstInput := publicationEffectPrepareFixture(t, 1)
	first, firstPermit, created, err := opened.PreparePublicationEffect(
		ctx,
		attempt,
		firstInput,
	)
	if err != nil || !created {
		t.Fatalf("first prepare = %+v, created=%v, err=%v", first, created, err)
	}
	replayed, replayPermit, created, err :=
		opened.PreparePublicationEffect(ctx, attempt, firstInput)
	if err != nil || created || replayPermit == nil ||
		!samePublicationEffectIntent(first, replayed) {
		t.Fatalf(
			"replay = %+v, permit=%s, created=%v, err=%v",
			replayed,
			replayPermit,
			created,
			err,
		)
	}
	startPublicationEffectFixture(
		t,
		opened,
		lease,
		attempt,
		firstPermit,
	)
	automation, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	released, startErr := opened.WithPublicationEffectCommandStart(
		ctx,
		automation,
		attempt,
		replayPermit,
		func() (bool, error) { return true, nil },
	)
	closeErr := automation.Close()
	if !errors.Is(startErr, ErrPublicationEffectAlreadyStarted) ||
		closeErr != nil || released {
		t.Fatalf(
			"idempotent replay release=%v startErr=%v closeErr=%v",
			released,
			startErr,
			closeErr,
		)
	}
	changed := firstInput
	changed.Descriptor = append(
		json.RawMessage(nil),
		firstInput.Descriptor...,
	)
	changed.Descriptor = json.RawMessage(strings.Replace(
		string(changed.Descriptor),
		strings.Repeat("c", 40),
		strings.Repeat("d", 40),
		1,
	))
	changed.DescriptorFingerprint =
		publicationEffectJSONFingerprint(changed.Descriptor)
	if _, _, _, err := opened.PreparePublicationEffect(
		ctx,
		attempt,
		changed,
	); !errors.Is(err, ErrPublicationEffectSequence) {
		t.Fatalf("changed occupied sequence error = %v", err)
	}
	if _, _, _, err := opened.PreparePublicationEffect(
		ctx,
		attempt,
		publicationEffectPrepareFixture(t, 3),
	); !errors.Is(err, ErrPublicationEffectSequence) {
		t.Fatalf("gapped sequence error = %v", err)
	}

	secondInput := publicationEffectPrepareFixture(t, 2)
	const callers = 8
	type prepareResult struct {
		intent  PublicationEffectIntent
		created bool
		err     error
	}
	results := make(chan prepareResult, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			intent, _, created, err := opened.PreparePublicationEffect(
				ctx,
				attempt,
				secondInput,
			)
			results <- prepareResult{
				intent:  intent,
				created: created,
				err:     err,
			}
		}()
	}
	ready.Wait()
	close(start)
	var secondID string
	var createdCount int
	for range callers {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent prepare error = %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if secondID == "" {
			secondID = result.intent.ID
		}
		if result.intent.ID != secondID {
			t.Fatalf(
				"concurrent IDs = %s and %s",
				secondID,
				result.intent.ID,
			)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
}

func TestPublicationEffectRejectsReadOnlyAndSensitivePayloads(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_payload",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-payload",
		time.Minute,
	)
	base := publicationEffectPrepareFixture(t, 1)
	tests := []struct {
		name   string
		mutate func(*PublicationEffectPrepareInput)
	}{
		{
			name: "read-only kind",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Kind = "git_status"
			},
		},
		{
			name: "argv",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(
					`{"argv":["git","push"]}`,
				)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "repository path",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(
					`{"repositoryPath":"/private/repo"}`,
				)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "absolute path value",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(
					`{"target":"/private/repo"}`,
				)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "credential URI",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(
					`{"endpoint":"https://user:pass@example.com/repo"}`,
				)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "raw body",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(
					`{"rawBody":"private request"}`,
				)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "non-canonical",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.Descriptor = json.RawMessage(`{ "ref": "main" }`)
				input.DescriptorFingerprint =
					publicationEffectJSONFingerprint(input.Descriptor)
			},
		},
		{
			name: "hash mismatch",
			mutate: func(input *PublicationEffectPrepareInput) {
				input.DescriptorFingerprint = strings.Repeat("0", 64)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.Descriptor = append(
				json.RawMessage(nil),
				base.Descriptor...,
			)
			test.mutate(&input)
			if _, _, _, err := opened.PreparePublicationEffect(
				ctx,
				attempt,
				input,
			); err == nil {
				t.Fatal("unsafe effect unexpectedly prepared")
			}
		})
	}
	var count int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM publication_effect_intents
	`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("effect row count = %d, err=%v", count, err)
	}
}

func TestPublicationEffectAcceptsSecretFreeRemoteRepositoryDescriptor(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_remote_descriptor",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-remote-descriptor",
		time.Minute,
	)
	input := publicationEffectPushPrepareFixture(t, 1)
	intent, _, created, err := opened.PreparePublicationEffect(
		ctx,
		attempt,
		input,
	)
	if err != nil || !created ||
		!bytes.Equal(intent.Descriptor, input.Descriptor) {
		t.Fatalf(
			"remote descriptor = %+v, created=%v, err=%v",
			intent,
			created,
			err,
		)
	}
	if bytes.Contains(intent.Descriptor, []byte("git@")) {
		t.Fatalf(
			"typed descriptor retained its credential-bearing remote: %s",
			intent.Descriptor,
		)
	}
}

func TestPublicationEffectRejectsSensitiveOrUnboundedEvidence(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_evidence",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-evidence",
		time.Minute,
	)
	_, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	base := publicationEffectResultFixture(
		t,
		PublicationEffectNotApplied,
	)
	tests := []struct {
		name   string
		mutate func(*PublicationEffectResultInput)
	}{
		{
			name: "credential field",
			mutate: func(input *PublicationEffectResultInput) {
				input.Evidence = json.RawMessage(`{"token":"secret"}`)
				input.EvidenceFingerprint =
					publicationEffectJSONFingerprint(input.Evidence)
			},
		},
		{
			name: "credential URI",
			mutate: func(input *PublicationEffectResultInput) {
				input.Evidence = json.RawMessage(
					`{"endpoint":"https://user:pass@example.com/result"}`,
				)
				input.EvidenceFingerprint =
					publicationEffectJSONFingerprint(input.Evidence)
			},
		},
		{
			name: "filesystem path",
			mutate: func(input *PublicationEffectResultInput) {
				input.Evidence = json.RawMessage(
					`{"location":"/private/worktree"}`,
				)
				input.EvidenceFingerprint =
					publicationEffectJSONFingerprint(input.Evidence)
			},
		},
		{
			name: "oversized",
			mutate: func(input *PublicationEffectResultInput) {
				input.Evidence = json.RawMessage(
					`{"value":"` +
						strings.Repeat(
							"x",
							MaxPublicationEffectEvidenceBytes,
						) +
						`"}`,
				)
				input.EvidenceFingerprint =
					publicationEffectJSONFingerprint(input.Evidence)
			},
		},
		{
			name: "hash mismatch",
			mutate: func(input *PublicationEffectResultInput) {
				input.EvidenceFingerprint = strings.Repeat("0", 64)
			},
		},
		{
			name: "error identifier",
			mutate: func(input *PublicationEffectResultInput) {
				input.ErrorKind = "CommandFailed"
				input.ErrorDetailFingerprint = strings.Repeat("d", 64)
			},
		},
		{
			name: "raw filesystem error detail",
			mutate: func(input *PublicationEffectResultInput) {
				input.ErrorKind = "command_failed"
				input.ErrorDetailFingerprint = "/home/user/private/repository"
			},
		},
		{
			name: "credential URI error detail",
			mutate: func(input *PublicationEffectResultInput) {
				input.ErrorKind = "command_failed"
				input.ErrorDetailFingerprint =
					"https://secret@example.com/repository"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.Evidence = append(
				json.RawMessage(nil),
				base.Evidence...,
			)
			test.mutate(&input)
			if _, err := opened.FinishPublicationEffect(
				ctx,
				effect,
				input,
			); err == nil {
				t.Fatal("unsafe effect evidence unexpectedly recorded")
			}
		})
	}
	var count int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM publication_effect_results
	`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("effect result count = %d, err=%v", count, err)
	}
}

func TestPublicationEffectPermitIsNonCopyableAndParentScoped(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_permit",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-permit",
		time.Minute,
	)
	intent, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	copiedValue := *effect
	copied := &copiedValue
	automation, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	if released, err := opened.WithPublicationEffectCommandStart(
		ctx,
		automation,
		attempt,
		copied,
		func() (bool, error) {
			calls.Add(1)
			return true, nil
		},
	); !errors.Is(err, ErrPublicationEffectPermitClosed) || released {
		t.Fatalf("copied release=%v, err=%v", released, err)
	}
	if err := automation.Close(); err != nil {
		t.Fatal(err)
	}

	forgedParent := &PublicationAttemptPermit{
		state: &publicationAttemptPermitState{
			intent: attempt.Intent(),
		},
	}
	automation, err = opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if released, err := opened.WithPublicationEffectCommandStart(
		ctx,
		automation,
		forgedParent,
		effect,
		func() (bool, error) {
			calls.Add(1)
			return true, nil
		},
	); !errors.Is(err, ErrPublicationEffectScope) || released {
		t.Fatalf("wrong parent release=%v, err=%v", released, err)
	}
	if err := automation.Close(); err != nil {
		t.Fatal(err)
	}

	startPublicationEffectFixture(t, opened, lease, attempt, effect)
	automation, err = opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if released, err := opened.WithPublicationEffectCommandStart(
		ctx,
		automation,
		attempt,
		effect,
		func() (bool, error) {
			calls.Add(1)
			return true, nil
		},
	); !errors.Is(err, ErrPublicationEffectAlreadyStarted) || released {
		t.Fatalf("second release=%v, err=%v", released, err)
	}
	if err := automation.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected guarded release calls = %d", calls.Load())
	}
	encoded, err := json.Marshal(effect)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("permit JSON = %s, err=%v", encoded, err)
	}
	rendered := fmt.Sprintf("%+v %#v", effect, effect)
	if strings.Contains(rendered, intent.DescriptorFingerprint) ||
		strings.Contains(rendered, intent.ParentEffectFingerprint) {
		t.Fatalf("permit rendered durable identity: %s", rendered)
	}
}

func TestPublicationEffectStartRequiresFreshAuthorityAndExactLiveTuple(
	t *testing.T,
) {
	t.Run("automation permit", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(
			t,
			opened,
			"effect_start_automation",
		)
		_, attempt, _ := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-effect-start-automation",
			time.Minute,
		)
		_, effect := preparePublicationEffectFixture(
			t,
			opened,
			attempt,
			1,
		)
		called := false
		released, err := opened.WithPublicationEffectCommandStart(
			ctx,
			nil,
			attempt,
			effect,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		if !errors.Is(err, ErrAutomationPermitClosed) ||
			released || called {
			t.Fatalf(
				"nil automation release=%v called=%v err=%v",
				released,
				called,
				err,
			)
		}
	})

	t.Run("expired claim", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		current := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
		opened.publicationClock = func() time.Time { return current }
		_, pending := createPendingPublicationAttemptFixture(
			t,
			opened,
			"effect_start_expired",
		)
		_, attempt, lease := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-effect-start-expired",
			MinPublicationClaimTTL,
		)
		_, effect := preparePublicationEffectFixture(
			t,
			opened,
			attempt,
			1,
		)
		current = current.Add(MinPublicationClaimTTL + time.Nanosecond)
		automation, err := opened.AcquireAutomationPermitForSession(
			ctx,
			lease,
		)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		released, err := opened.WithPublicationEffectCommandStart(
			ctx,
			automation,
			attempt,
			effect,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		closeErr := automation.Close()
		if !errors.Is(err, ErrPublicationClaimExpired) ||
			closeErr != nil || released || called {
			t.Fatalf(
				"expired release=%v called=%v err=%v closeErr=%v",
				released,
				called,
				err,
				closeErr,
			)
		}
	})

	t.Run("changed publication", func(t *testing.T) {
		ctx := context.Background()
		opened := openAutomationTestStore(t)
		_, pending := createPendingPublicationAttemptFixture(
			t,
			opened,
			"effect_start_changed",
		)
		_, attempt, lease := beginPublicationAttemptFixture(
			t,
			opened,
			pending,
			"publisher-effect-start-changed",
			time.Minute,
		)
		_, effect := preparePublicationEffectFixture(
			t,
			opened,
			attempt,
			1,
		)
		if _, err := opened.db.ExecContext(ctx, `
			UPDATE publications SET head_commit = ?
			WHERE id = ? AND board = ?
		`, strings.Repeat("d", 40), pending.ID, "default"); err != nil {
			t.Fatal(err)
		}
		automation, err := opened.AcquireAutomationPermitForSession(
			ctx,
			lease,
		)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		released, err := opened.WithPublicationEffectCommandStart(
			ctx,
			automation,
			attempt,
			effect,
			func() (bool, error) {
				called = true
				return true, nil
			},
		)
		closeErr := automation.Close()
		if !errors.Is(err, ErrPublicationEffectScope) ||
			closeErr != nil || released || called {
			t.Fatalf(
				"changed release=%v called=%v err=%v closeErr=%v",
				released,
				called,
				err,
				closeErr,
			)
		}
	})
}

func TestPublicationEffectPanickingReleaseFailsClosed(t *testing.T) {
	state := &publicationEffectPermitState{}
	recovered := false
	func() {
		defer func() {
			recovered = recover() != nil
		}()
		_, _ = releasePublicationEffectCommandFence(
			state,
			func() (bool, error) {
				panic("release boundary panic")
			},
		)
	}()
	if !recovered || !state.started {
		t.Fatalf(
			"panicking release recovered=%v started=%v",
			recovered,
			state.started,
		)
	}
}

func TestPublicationEffectPanickingStartRollsBackAndStoreRemainsUsable(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_start_panic",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-start-panic",
		time.Minute,
	)
	intent, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	automation, err := opened.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	recovered := false
	func() {
		defer func() {
			recovered = recover() != nil
		}()
		_, _ = opened.WithPublicationEffectCommandStart(
			ctx,
			automation,
			attempt,
			effect,
			func() (bool, error) {
				panic("process release panic")
			},
		)
	}()
	closeErr := automation.Close()
	if !recovered || closeErr != nil {
		t.Fatalf(
			"panicking start recovered=%v closeErr=%v",
			recovered,
			closeErr,
		)
	}

	result, err := opened.FinishPublicationEffect(
		ctx,
		effect,
		publicationEffectResultFixture(
			t,
			PublicationEffectUnknown,
		),
	)
	if err != nil || result.EffectID != intent.ID ||
		result.Outcome != PublicationEffectUnknown {
		t.Fatalf(
			"post-panic result = %+v, err=%v",
			result,
			err,
		)
	}
	record, err := opened.GetPublicationEffect(ctx, intent.ID)
	if err != nil || record.Result == nil ||
		record.Result.Outcome != PublicationEffectUnknown {
		t.Fatalf(
			"post-panic database read = %+v, err=%v",
			record,
			err,
		)
	}
}

func TestPublicationEffectFinishAfterExpiryIsExactAndIdempotent(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	current := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	opened.publicationClock = func() time.Time { return current }
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_expiry",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-expiry",
		MinPublicationClaimTTL,
	)
	_, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	startPublicationEffectFixture(t, opened, lease, attempt, effect)
	current = current.Add(MinPublicationClaimTTL + time.Nanosecond)

	input := publicationEffectResultFixture(
		t,
		PublicationEffectUnknown,
	)
	first, err := opened.FinishPublicationEffect(ctx, effect, input)
	if err != nil || first.Outcome != PublicationEffectUnknown {
		t.Fatalf("finish after expiry = %+v, err=%v", first, err)
	}
	replayed, err := opened.FinishPublicationEffect(ctx, effect, input)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("replay finish = %+v, err=%v", replayed, err)
	}
	changed := input
	changed.ErrorDetailFingerprint = strings.Repeat("e", 64)
	if _, err := opened.FinishPublicationEffect(
		ctx,
		effect,
		changed,
	); !errors.Is(err, ErrPublicationEffectResultConflict) {
		t.Fatalf("conflicting finish error = %v", err)
	}

	intent := effect.Intent()
	for _, statement := range []string{
		"UPDATE publication_effect_intents SET kind = 'pr_create' WHERE id = ?",
		"DELETE FROM publication_effect_intents WHERE id = ?",
		"UPDATE publication_effect_results SET outcome = 'applied' WHERE effect_id = ?",
		"DELETE FROM publication_effect_results WHERE effect_id = ?",
	} {
		if _, err := opened.db.ExecContext(ctx, statement, intent.ID); err == nil {
			t.Fatalf("immutable statement unexpectedly succeeded: %s", statement)
		}
	}
}

func TestPublicationEffectDatabaseGuardsExactIdentity(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_identity_guard",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-identity-guard",
		time.Minute,
	)
	intent, _ := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	second := publicationEffectPrepareFixture(t, 2)
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_effect_intents(
			id, attempt_id, board, publication_id, claim_epoch,
			sequence, kind, descriptor_version, descriptor_json,
			descriptor_fingerprint, identity_fingerprint,
			parent_effect_fingerprint, parent_provenance_fingerprint,
			prepared_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"pef_forged_parent",
		intent.AttemptID,
		intent.Board,
		intent.PublicationID,
		intent.ClaimEpoch,
		second.Sequence,
		second.Kind,
		second.DescriptorVersion,
		string(second.Descriptor),
		second.DescriptorFingerprint,
		strings.Repeat("1", 64),
		strings.Repeat("2", 64),
		intent.ParentProvenanceFingerprint,
		intent.PreparedAt,
	); err == nil ||
		!strings.Contains(err.Error(), "does not match its parent") {
		t.Fatalf("forged parent insert error = %v", err)
	}

	evidence := publicationEffectResultFixture(
		t,
		PublicationEffectNotApplied,
	)
	if _, err := opened.db.ExecContext(ctx, `
			INSERT INTO publication_effect_results(
				effect_id, attempt_id, board, publication_id, claim_epoch,
				sequence, identity_fingerprint, outcome, evidence_json,
				evidence_fingerprint, error_kind,
				error_detail_fingerprint, recorded_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
	`,
		intent.ID,
		intent.AttemptID,
		intent.Board,
		intent.PublicationID,
		intent.ClaimEpoch,
		intent.Sequence+1,
		intent.IdentityFingerprint,
		evidence.Outcome,
		string(evidence.Evidence),
		evidence.EvidenceFingerprint,
		intent.PreparedAt,
	); err == nil ||
		!strings.Contains(err.Error(), "does not match its intent") {
		t.Fatalf("forged result insert error = %v", err)
	}
	var intentCount, resultCount int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM publication_effect_intents),
			(SELECT COUNT(*) FROM publication_effect_results)
	`).Scan(&intentCount, &resultCount); err != nil {
		t.Fatal(err)
	}
	if intentCount != 1 || resultCount != 0 {
		t.Fatalf(
			"guarded row counts intents=%d results=%d",
			intentCount,
			resultCount,
		)
	}
}

func TestPublicationEffectParentResultRequiresResolvedEffects(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_parent_result",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-parent-result",
		time.Minute,
	)
	_, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	parentInput := PublicationAttemptResultInput{
		Outcome:        PublicationAttemptFailed,
		ExecutorStatus: PublicationExecutorFailed,
		ErrorKind:      PublicationErrorCommandFailed,
		Error:          "command failed before a known effect result",
	}
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		attempt,
		parentInput,
	); err == nil ||
		!strings.Contains(err.Error(), "unresolved command effects") {
		t.Fatalf("parent finish with unresolved effect error = %v", err)
	}
	notApplied := publicationEffectResultFixture(
		t,
		PublicationEffectNotApplied,
	)
	if _, err := opened.FinishPublicationEffect(
		ctx,
		effect,
		notApplied,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		attempt,
		parentInput,
	); err != nil {
		t.Fatalf("parent finish after effect result: %v", err)
	}
}

func TestPublicationEffectUnknownChildRequiresUnknownParentResult(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_unknown_parent",
	)
	_, attempt, lease := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-unknown-parent",
		time.Minute,
	)
	_, effect := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	startPublicationEffectFixture(t, opened, lease, attempt, effect)
	if _, err := opened.FinishPublicationEffect(
		ctx,
		effect,
		publicationEffectResultFixture(
			t,
			PublicationEffectUnknown,
		),
	); err != nil {
		t.Fatal(err)
	}
	knownParent := PublicationAttemptResultInput{
		Outcome:        PublicationAttemptFailed,
		ExecutorStatus: PublicationExecutorFailed,
		ErrorKind:      PublicationErrorCommandFailed,
		Error:          "known parent cannot hide an unknown command effect",
	}
	if _, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		attempt,
		knownParent,
	); !errors.Is(err, ErrPublicationEffectParentOutcome) {
		t.Fatalf("known parent outcome error = %v", err)
	}

	parentIntent := attempt.Intent()
	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO publication_attempt_results(
			attempt_id, board, publication_id, claim_epoch, outcome,
			executor_status, error_kind, result_url, error,
			publication_updated_at, recorded_at
		) VALUES (?, ?, ?, ?, 'failed', 'failed', 'command_failed',
			NULL, 'direct known result', ?, ?)
	`,
		parentIntent.ID,
		parentIntent.Board,
		parentIntent.PublicationID,
		parentIntent.ClaimEpoch,
		parentIntent.PublicationUpdatedAt,
		"2026-07-24T15:00:00.000000000Z",
	); err == nil ||
		!strings.Contains(err.Error(), "requires an unknown result") {
		t.Fatalf("direct known parent result error = %v", err)
	}

	unknownParent := PublicationAttemptResultInput{
		Outcome:        PublicationAttemptUnknown,
		ExecutorStatus: PublicationExecutorUnknown,
		ErrorKind:      PublicationErrorUnknown,
		Error:          "command effect outcome remains uncertain",
	}
	result, err := opened.FinishAutomatedPublicationAttempt(
		ctx,
		attempt,
		unknownParent,
	)
	if err != nil || result.Outcome != PublicationAttemptUnknown {
		t.Fatalf("unknown parent result = %+v, err=%v", result, err)
	}
}

func TestPublicationEffectUnresolvedListUsesStableCursor(
	t *testing.T,
) {
	ctx := context.Background()
	opened := openAutomationTestStore(t)
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_cursor",
	)
	_, attempt, _ := beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-cursor",
		time.Minute,
	)
	first, firstPermit := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		1,
	)
	second, _ := preparePublicationEffectFixture(
		t,
		opened,
		attempt,
		2,
	)
	page, err := opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{Limit: 1},
	)
	if err != nil || len(page) != 1 ||
		page[0].Intent.ID != first.ID {
		t.Fatalf("first page = %+v, err=%v", page, err)
	}
	next, err := opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{
			AfterPreparedAt: page[0].Intent.PreparedAt,
			AfterID:         page[0].Intent.ID,
			Limit:           1,
		},
	)
	if err != nil || len(next) != 1 ||
		next[0].Intent.ID != second.ID {
		t.Fatalf("second page = %+v, err=%v", next, err)
	}
	if _, err := opened.FinishPublicationEffect(
		ctx,
		firstPermit,
		publicationEffectResultFixture(
			t,
			PublicationEffectNotApplied,
		),
	); err != nil {
		t.Fatal(err)
	}
	remaining, err := opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{},
	)
	if err != nil || len(remaining) != 1 ||
		remaining[0].Intent.ID != second.ID {
		t.Fatalf("remaining = %+v, err=%v", remaining, err)
	}
	if _, err := opened.ListUnresolvedPublicationEffects(
		ctx,
		PublicationEffectFilter{AfterID: first.ID},
	); err == nil {
		t.Fatal("cursor ID without timestamp unexpectedly accepted")
	}
}

func TestPublicationEffectMigrationRejectsPreexistingLedgerCorruption(
	t *testing.T,
) {
	t.Run("orphan intent", func(t *testing.T) {
		opened, path := openPublicationEffectFileStore(t)
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		input := publicationEffectPrepareFixture(t, 1)
		intent := PublicationEffectIntent{
			ID:                          "pef_orphan",
			AttemptID:                   "pat_missing",
			Board:                       "default",
			PublicationID:               "pub_missing",
			ClaimEpoch:                  1,
			Sequence:                    input.Sequence,
			Kind:                        input.Kind,
			DescriptorVersion:           input.DescriptorVersion,
			Descriptor:                  input.Descriptor,
			DescriptorFingerprint:       input.DescriptorFingerprint,
			ParentEffectFingerprint:     strings.Repeat("a", 64),
			ParentProvenanceFingerprint: strings.Repeat("b", 64),
			PreparedAt:                  "2026-07-24T16:00:00.000000000Z",
		}
		intent.IdentityFingerprint =
			publicationEffectIdentityFingerprint(intent)
		raw, err := sql.Open("sqlite", dataSourceName(path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA foreign_keys = OFF"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec(`
			DROP TRIGGER publication_effect_intents_parent_guard;
			INSERT INTO publication_effect_intents(
				id, attempt_id, board, publication_id, claim_epoch,
				sequence, kind, descriptor_version, descriptor_json,
				descriptor_fingerprint, identity_fingerprint,
				parent_effect_fingerprint,
				parent_provenance_fingerprint, prepared_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			intent.ID,
			intent.AttemptID,
			intent.Board,
			intent.PublicationID,
			intent.ClaimEpoch,
			intent.Sequence,
			intent.Kind,
			intent.DescriptorVersion,
			string(intent.Descriptor),
			intent.DescriptorFingerprint,
			intent.IdentityFingerprint,
			intent.ParentEffectFingerprint,
			intent.ParentProvenanceFingerprint,
			intent.PreparedAt,
		); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			path,
			"foreign key",
		)
	})

	t.Run("result identity", func(t *testing.T) {
		fixture := newPublicationEffectFileFixture(
			t,
			"effect_migration_result_identity",
		)
		raw := closePublicationEffectFixtureForRawAccess(t, fixture)
		evidence := publicationEffectResultFixture(
			t,
			PublicationEffectNotApplied,
		)
		if _, err := raw.Exec(`
			DROP TRIGGER publication_effect_results_identity_guard;
				INSERT INTO publication_effect_results(
					effect_id, attempt_id, board, publication_id,
					claim_epoch, sequence, identity_fingerprint, outcome,
					evidence_json, evidence_fingerprint, error_kind,
					error_detail_fingerprint, recorded_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
		`,
			fixture.effectIntent.ID,
			fixture.effectIntent.AttemptID,
			fixture.effectIntent.Board,
			fixture.effectIntent.PublicationID,
			fixture.effectIntent.ClaimEpoch,
			fixture.effectIntent.Sequence+1,
			fixture.effectIntent.IdentityFingerprint,
			evidence.Outcome,
			string(evidence.Evidence),
			evidence.EvidenceFingerprint,
			"2026-07-24T16:01:00.000000000Z",
		); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			fixture.path,
			"mismatched effect result identity",
		)
	})

	t.Run("parent tuple", func(t *testing.T) {
		fixture := newPublicationEffectFileFixture(
			t,
			"effect_migration_parent_tuple",
		)
		raw := closePublicationEffectFixtureForRawAccess(t, fixture)
		corrupt := fixture.effectIntent
		corrupt.Board = "other"
		corrupt.IdentityFingerprint =
			publicationEffectIdentityFingerprint(corrupt)
		if _, err := raw.Exec(`
			DROP TRIGGER publication_effect_intents_prevent_update;
			UPDATE publication_effect_intents
			SET board = ?, identity_fingerprint = ?
			WHERE id = ?
		`,
			corrupt.Board,
			corrupt.IdentityFingerprint,
			corrupt.ID,
		); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			fixture.path,
			"mismatched effect intent parent",
		)
	})

	t.Run("parent unresolved", func(t *testing.T) {
		fixture := newPublicationEffectFileFixture(
			t,
			"effect_migration_parent_unresolved",
		)
		parent := fixture.attempt.Intent()
		raw := closePublicationEffectFixtureForRawAccess(t, fixture)
		if _, err := raw.Exec(`
			DROP TRIGGER
				publication_attempt_results_require_resolved_effects
		`); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		insertDirectKnownPublicationAttemptResult(t, raw, parent)
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			fixture.path,
			"parent result with unresolved effect",
		)
	})

	t.Run("known parent with unknown effect", func(t *testing.T) {
		fixture := newPublicationEffectFileFixture(
			t,
			"effect_migration_parent_unknown",
		)
		startPublicationEffectFixture(
			t,
			fixture.store,
			fixture.lease,
			fixture.attempt,
			fixture.effectPermit,
		)
		if _, err := fixture.store.FinishPublicationEffect(
			context.Background(),
			fixture.effectPermit,
			publicationEffectResultFixture(
				t,
				PublicationEffectUnknown,
			),
		); err != nil {
			t.Fatal(err)
		}
		parent := fixture.attempt.Intent()
		raw := closePublicationEffectFixtureForRawAccess(t, fixture)
		if _, err := raw.Exec(`
			DROP TRIGGER
				publication_attempt_results_require_unknown_effect_outcome
		`); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		insertDirectKnownPublicationAttemptResult(t, raw, parent)
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			fixture.path,
			"known parent result with unknown effect",
		)
	})

	t.Run("untyped descriptor", func(t *testing.T) {
		fixture := newPublicationEffectFileFixture(
			t,
			"effect_migration_untyped_descriptor",
		)
		raw := closePublicationEffectFixtureForRawAccess(t, fixture)
		corrupt := fixture.effectIntent
		corrupt.Descriptor = json.RawMessage(
			`{"version":1,"kind":"local_ref_cas","target":{"gitCommonDirPath":"/private/repo/.git"}}`,
		)
		corrupt.DescriptorFingerprint =
			publicationEffectJSONFingerprint(corrupt.Descriptor)
		corrupt.IdentityFingerprint =
			publicationEffectIdentityFingerprint(corrupt)
		if _, err := raw.Exec(`
			DROP TRIGGER publication_effect_intents_prevent_update;
			UPDATE publication_effect_intents
			SET descriptor_json = ?, descriptor_fingerprint = ?,
				identity_fingerprint = ?
			WHERE id = ?
		`,
			string(corrupt.Descriptor),
			corrupt.DescriptorFingerprint,
			corrupt.IdentityFingerprint,
			corrupt.ID,
		); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec(publicationEffectSchema); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 30"); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		requirePublicationEffectMigrationFailure(
			t,
			fixture.path,
			"invalid intent",
		)
	})
}

func TestPublicationEffectSchemaRejectsPredicateCompatibleTriggerRewrite(
	t *testing.T,
) {
	opened, path := openPublicationEffectFileStore(t)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		DROP TRIGGER publication_effect_results_identity_guard;
		CREATE TRIGGER publication_effect_results_identity_guard
		BEFORE INSERT ON publication_effect_results
		WHEN NOT EXISTS (
			SELECT 1 FROM publication_effect_intents i
			WHERE i.id = NEW.effect_id
				AND i.attempt_id = NEW.attempt_id
				AND i.board = NEW.board
				AND i.publication_id = NEW.publication_id
				AND i.claim_epoch = NEW.claim_epoch
				AND i.sequence = NEW.sequence
				AND i.identity_fingerprint = NEW.identity_fingerprint
		) AND 1 = 1
		BEGIN
			SELECT RAISE(
				ABORT,
				'publication effect result does not match its intent'
			);
		END;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(path, "default", ""); err == nil {
		reopened.Close()
		t.Fatal("rewritten trigger unexpectedly accepted")
	} else if !strings.Contains(
		err.Error(),
		"publication_effect_results_identity_guard definition",
	) {
		t.Fatalf("rewritten trigger error = %v", err)
	}
}

func downgradePublicationEffectStoreToV30(
	t *testing.T,
	opened *Store,
	path string,
) {
	t.Helper()
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		DROP TABLE publication_effect_results;
		DROP TABLE publication_effect_intents;
		PRAGMA user_version = 30;
	`); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationEffectSchemaMigratesV30AtomicallyAndConcurrently(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "v30.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	_, pending := createPendingPublicationAttemptFixture(
		t,
		opened,
		"effect_migration",
	)
	_, _, _ = beginPublicationAttemptFixture(
		t,
		opened,
		pending,
		"publisher-effect-migration",
		time.Minute,
	)
	downgradePublicationEffectStoreToV30(t, opened, path)

	const openers = 4
	results := make(chan error, openers)
	var ready sync.WaitGroup
	ready.Add(openers)
	start := make(chan struct{})
	for range openers {
		go func() {
			ready.Done()
			<-start
			store, err := Open(path, "default", "")
			if err == nil {
				err = store.Close()
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	for range openers {
		if err := <-results; err != nil {
			t.Fatalf("concurrent v30 migration: %v", err)
		}
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, tableCount, triggerCount, indexCount int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM sqlite_master
			 WHERE type = 'table' AND name LIKE 'publication_effect_%'),
			(SELECT COUNT(*) FROM sqlite_master
			 WHERE type = 'trigger'
			   AND (
			     name LIKE 'publication_effect_%'
			     OR name IN (
			       'publication_attempt_results_require_resolved_effects',
			       'publication_attempt_results_require_unknown_effect_outcome'
			     )
			   )),
			(SELECT COUNT(*) FROM sqlite_master
			 WHERE type = 'index'
			   AND name LIKE 'idx_publication_effect_%')
	`).Scan(&tableCount, &triggerCount, &indexCount); err != nil {
		t.Fatal(err)
	}
	if version != 31 || tableCount != 2 ||
		triggerCount != 8 || indexCount != 2 {
		t.Fatalf(
			"migrated schema version=%d tables=%d triggers=%d indexes=%d",
			version,
			tableCount,
			triggerCount,
			indexCount,
		)
	}
}

func TestPublicationEffectSchemaRollsBackConflictAndRejectsFutureVersion(
	t *testing.T,
) {
	t.Run("conflict", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conflict.db")
		opened, err := Open(path, "default", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dataSourceName(path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			DROP TABLE publication_effect_results;
			DROP TABLE publication_effect_intents;
			CREATE TABLE publication_effect_intents(id TEXT PRIMARY KEY);
			PRAGMA user_version = 30;
		`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		if reopened, err := Open(path, "default", ""); err == nil {
			reopened.Close()
			t.Fatal("incompatible v30 schema unexpectedly opened")
		}
		db, err = sql.Open("sqlite", dataSourceName(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version, resultTables, intentColumns int
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`
			SELECT
				(SELECT COUNT(*) FROM sqlite_master
				 WHERE type = 'table'
				   AND name = 'publication_effect_results'),
				(SELECT COUNT(*) FROM pragma_table_info(
					'publication_effect_intents'
				))
		`).Scan(&resultTables, &intentColumns); err != nil {
			t.Fatal(err)
		}
		if version != 30 || resultTables != 0 || intentColumns != 1 {
			t.Fatalf(
				"rollback version=%d resultTables=%d intentColumns=%d",
				version,
				resultTables,
				intentColumns,
			)
		}
	})

	t.Run("future", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "future.db")
		opened, err := Open(path, "default", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dataSourceName(path))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version = 32"); err != nil {
			t.Fatal(err)
		}
		var beforeObjects int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
		`).Scan(&beforeObjects); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if reopened, err := Open(path, "default", ""); err == nil {
			reopened.Close()
			t.Fatal("future database unexpectedly opened")
		}
		db, err = sql.Open("sqlite", dataSourceName(path))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version, afterObjects int
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
		`).Scan(&afterObjects); err != nil {
			t.Fatal(err)
		}
		if version != 32 || beforeObjects != afterObjects {
			t.Fatalf(
				"future database changed: version=%d objects=%d->%d",
				version,
				beforeObjects,
				afterObjects,
			)
		}
	})
}
