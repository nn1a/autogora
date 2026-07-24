package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func plannerAttemptFixture(
	t *testing.T,
	opened *Store,
	suffix string,
) (model.Task, BeginPlannerAttemptInput) {
	t.Helper()
	ctx := context.Background()
	created, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:  "Plan " + suffix,
		Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	return created.Task, BeginPlannerAttemptInput{
		TaskID:                created.Task.ID,
		Board:                 "default",
		ExpectedTaskUpdatedAt: created.Task.UpdatedAt,
		ExpectedGraphRevision: graph.Revision,
		Kind:                  PlannerAttemptDecompose,
		SchemaVersion:         1,
		SnapshotHash:          plannerHash([]byte("snapshot-" + suffix)),
		ConfigHash:            plannerHash([]byte("config-" + suffix)),
		IdempotencyKey:        "planner:" + suffix,
		Attempt:               1,
	}
}

func TestBeginPlannerAttemptPersistsExactReplayBeforeExternalCall(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	task, input := plannerAttemptFixture(t, opened, "begin")
	intent, created, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil || !created {
		t.Fatalf("begin planner attempt = %+v, created=%v, err=%v", intent, created, err)
	}
	if intent.ID == "" ||
		intent.Board != input.Board ||
		intent.TaskID != input.TaskID ||
		intent.TaskUpdatedAt != input.ExpectedTaskUpdatedAt ||
		intent.GraphRevision != input.ExpectedGraphRevision ||
		intent.Kind != input.Kind ||
		intent.SchemaVersion != input.SchemaVersion ||
		intent.SnapshotHash != input.SnapshotHash ||
		intent.ConfigHash != input.ConfigHash ||
		intent.IdempotencyKey != input.IdempotencyKey ||
		intent.Attempt != input.Attempt ||
		intent.StartedAt == "" {
		t.Fatalf("planner intent = %+v", intent)
	}

	// Crash evidence is deliberately independent of task lifetime. Replay must
	// consult the ledger before mutable task preconditions.
	if _, err := opened.db.ExecContext(
		ctx,
		"DELETE FROM tasks WHERE id = ?",
		task.ID,
	); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil || created || replayed != intent {
		t.Fatalf(
			"replay after task deletion = %+v, created=%v, err=%v",
			replayed,
			created,
			err,
		)
	}
	record, err := opened.GetPlannerAttemptByIdempotencyKey(
		ctx,
		"default",
		input.IdempotencyKey,
	)
	if err != nil || record.Intent != intent || record.Proposal != nil {
		t.Fatalf("replay lookup = %+v, err=%v", record, err)
	}
	unresolved, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{Limit: 1},
	)
	if err != nil || len(unresolved) != 1 || unresolved[0] != intent {
		t.Fatalf("unresolved intents = %+v, err=%v", unresolved, err)
	}

	conflictInput := input
	conflictInput.ConfigHash = plannerHash([]byte("different config"))
	_, _, err = opened.BeginPlannerAttempt(ctx, conflictInput)
	var conflict *PlannerAttemptConflictError
	if !errors.Is(err, ErrPlannerAttemptConflict) ||
		!errors.As(err, &conflict) ||
		conflict.ExistingAttemptID != intent.ID {
		t.Fatalf("idempotency conflict = %#v", err)
	}
}

func TestBeginPlannerAttemptRequiresCurrentTriageSnapshotAndGraph(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, input := plannerAttemptFixture(t, opened, "preconditions")
	staleTask := input
	staleTask.ExpectedTaskUpdatedAt = "2000-01-01T00:00:00Z"
	if _, _, err := opened.BeginPlannerAttempt(ctx, staleTask); err == nil ||
		!strings.Contains(err.Error(), "changed at") {
		t.Fatalf("stale task snapshot error = %v", err)
	}

	staleGraph := input
	staleGraph.IdempotencyKey += ":graph"
	if err := opened.withWrite(ctx, func(tx *sql.Tx) error {
		_, err := bumpBoardGraphRevision(ctx, tx, "default")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.BeginPlannerAttempt(ctx, staleGraph); !errors.Is(
		err,
		ErrGraphRevisionConflict,
	) {
		t.Fatalf("stale graph error = %v", err)
	}

	nonTriage, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:  "Already specified",
		Status: model.TaskStatusTodo,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	notTriage := input
	notTriage.TaskID = nonTriage.Task.ID
	notTriage.ExpectedTaskUpdatedAt = nonTriage.Task.UpdatedAt
	notTriage.ExpectedGraphRevision = graph.Revision
	notTriage.IdempotencyKey += ":status"
	if _, _, err := opened.BeginPlannerAttempt(ctx, notTriage); err == nil ||
		!strings.Contains(err.Error(), "expected triage") {
		t.Fatalf("non-Triage error = %v", err)
	}
}

func TestBeginPlannerAttemptPreservesExactRFC3339NanoTaskToken(
	t *testing.T,
) {
	timestampState.Lock()
	previousClock, previousLast := timestampState.clock, timestampState.last
	fixed := time.Date(
		2031,
		time.February,
		3,
		4,
		5,
		6,
		123_000_000,
		time.UTC,
	)
	timestampState.clock = func() time.Time { return fixed }
	timestampState.last = time.Time{}
	timestampState.Unlock()
	t.Cleanup(func() {
		timestampState.Lock()
		timestampState.clock, timestampState.last = previousClock, previousLast
		timestampState.Unlock()
	})

	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, input := plannerAttemptFixture(t, opened, "timestamp")
	if !strings.HasSuffix(task.UpdatedAt, ".123Z") {
		t.Fatalf("fixture updatedAt = %q, want trimmed trailing zeroes", task.UpdatedAt)
	}
	intent, created, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil || !created ||
		intent.TaskUpdatedAt != task.UpdatedAt ||
		intent.StartedAt != "2031-02-03T04:05:06.123000002Z" {
		t.Fatalf(
			"exact timestamp begin = %+v, created=%v, err=%v",
			intent,
			created,
			err,
		)
	}

	nonCanonical := input
	nonCanonical.IdempotencyKey += ":expanded"
	nonCanonical.ExpectedTaskUpdatedAt = strings.Replace(
		task.UpdatedAt,
		".123Z",
		".123000000Z",
		1,
	)
	if _, _, err := opened.BeginPlannerAttempt(
		ctx,
		nonCanonical,
	); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("expanded timestamp error = %v", err)
	}
	nonCanonical.IdempotencyKey += ":whitespace"
	nonCanonical.ExpectedTaskUpdatedAt = " " + task.UpdatedAt
	if _, _, err := opened.BeginPlannerAttempt(
		ctx,
		nonCanonical,
	); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("whitespace timestamp error = %v", err)
	}
}

func TestPlannerLedgerTimestampOrderingMatchesTimeAcrossFractionWidths(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	base := time.Date(2032, time.March, 4, 5, 6, 7, 0, time.UTC)
	clockValues := []time.Time{
		base.Add(10 * time.Millisecond),
		base.Add(20 * time.Millisecond),
		base.Add(100 * time.Millisecond),
		base.Add(110 * time.Millisecond),
	}
	timestampState.Lock()
	previousClock, previousLast := timestampState.clock, timestampState.last
	clockIndex := 0
	timestampState.clock = func() time.Time {
		if clockIndex >= len(clockValues) {
			return clockValues[len(clockValues)-1]
		}
		value := clockValues[clockIndex]
		clockIndex++
		return value
	}
	timestampState.last = time.Time{}
	timestampState.Unlock()
	t.Cleanup(func() {
		timestampState.Lock()
		timestampState.clock, timestampState.last = previousClock, previousLast
		timestampState.Unlock()
	})

	_, input := plannerAttemptFixture(t, opened, "ledger-time-order")
	first, created, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil || !created {
		t.Fatalf("first ledger attempt: created=%v err=%v", created, err)
	}
	secondInput := input
	secondInput.IdempotencyKey += ":second"
	secondInput.Attempt = 2
	second, created, err := opened.BeginPlannerAttempt(ctx, secondInput)
	if err != nil || !created {
		t.Fatalf("second ledger attempt: created=%v err=%v", created, err)
	}
	if first.StartedAt != "2032-03-04T05:06:07.100000000Z" ||
		second.StartedAt != "2032-03-04T05:06:07.110000000Z" ||
		first.StartedAt >= second.StartedAt {
		t.Fatalf(
			"fixed ledger timestamps first=%q second=%q",
			first.StartedAt,
			second.StartedAt,
		)
	}
	firstTime, err := time.Parse(time.RFC3339Nano, first.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	secondTime, err := time.Parse(time.RFC3339Nano, second.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !firstTime.Before(secondTime) {
		t.Fatalf("ledger time order first=%s second=%s", firstTime, secondTime)
	}

	page, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{Limit: 1},
	)
	if err != nil || len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("first ledger page = %+v, err=%v", page, err)
	}
	next, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{
			AfterStartedAt: page[0].StartedAt,
			AfterID:        page[0].ID,
			Limit:          1,
		},
	)
	if err != nil || len(next) != 1 || next[0].ID != second.ID {
		t.Fatalf("second ledger page = %+v, err=%v", next, err)
	}
	if _, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{
			AfterStartedAt: "2032-03-04T05:06:07.1Z",
			AfterID:        first.ID,
			Limit:          1,
		},
	); err == nil || !strings.Contains(err.Error(), "fixed-width") {
		t.Fatalf("variable-width ledger cursor error = %v", err)
	}
}

func TestRecordPlannerProposalCanonicalizesAndReplaysExactResponse(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, input := plannerAttemptFixture(t, opened, "valid")
	intent, _, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("{\n  \"tasks\": [], \"fanout\": false,\n  \"reason\": \"one worker\"\n}")
	proposalInput := RecordPlannerProposalInput{
		Response:         raw,
		ValidationStatus: PlannerProposalValid,
	}
	proposal, created, err := opened.RecordPlannerProposal(
		ctx,
		intent,
		proposalInput,
	)
	if err != nil || !created {
		t.Fatalf("record proposal = %+v, created=%v, err=%v", proposal, created, err)
	}
	wantPayload := `{"fanout":false,"reason":"one worker","tasks":[]}`
	if string(proposal.Payload) != wantPayload ||
		proposal.ResponseHash != plannerHash(raw) ||
		proposal.PayloadHash == nil ||
		*proposal.PayloadHash != plannerHash([]byte(wantPayload)) ||
		proposal.ValidationError != nil ||
		proposal.ValidationErrorHash != nil ||
		proposal.RecordedAt == "" {
		t.Fatalf("proposal receipt = %+v", proposal)
	}

	replayed, created, err := opened.RecordPlannerProposal(
		ctx,
		intent,
		proposalInput,
	)
	if err != nil || created || !samePlannerProposal(replayed, proposal) ||
		replayed.RecordedAt != proposal.RecordedAt {
		t.Fatalf(
			"proposal replay = %+v, created=%v, err=%v",
			replayed,
			created,
			err,
		)
	}
	_, _, err = opened.RecordPlannerProposal(
		ctx,
		intent,
		RecordPlannerProposalInput{
			Response:         []byte(wantPayload),
			ValidationStatus: PlannerProposalValid,
		},
	)
	if !errors.Is(err, ErrPlannerProposalConflict) {
		t.Fatalf("different raw response conflict = %v", err)
	}

	record, err := opened.GetPlannerAttempt(ctx, intent.ID)
	if err != nil || record.Proposal == nil ||
		!samePlannerProposal(*record.Proposal, proposal) {
		t.Fatalf("get planner attempt = %+v, err=%v", record, err)
	}
	unresolved, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{},
	)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("resolved attempt remained listed: %+v, err=%v", unresolved, err)
	}
}

func TestRecordInvalidPlannerResponseKeepsBoundedCrashReceipt(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	_, input := plannerAttemptFixture(t, opened, "invalid")
	intent, _, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"tasks":[`)
	validationError := strings.Repeat(
		"invalid planner output ",
		MaxPlannerProposalErrorBytes,
	)
	proposal, created, err := opened.RecordPlannerProposal(
		ctx,
		intent,
		RecordPlannerProposalInput{
			Response:         raw,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  validationError,
		},
	)
	if err != nil || !created {
		t.Fatalf("record invalid response = %+v, created=%v, err=%v", proposal, created, err)
	}
	if len(proposal.Payload) != 0 ||
		proposal.PayloadHash != nil ||
		proposal.ResponseHash != plannerHash(raw) ||
		proposal.ValidationError == nil ||
		len(*proposal.ValidationError) != MaxPlannerProposalErrorBytes ||
		proposal.ValidationErrorHash == nil ||
		*proposal.ValidationErrorHash != plannerHash(
			[]byte(validationError),
		) {
		t.Fatalf("invalid response receipt = %+v", proposal)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	record, err := restarted.GetPlannerAttemptByIdempotencyKey(
		ctx,
		"default",
		input.IdempotencyKey,
	)
	if err != nil || record.Proposal == nil ||
		!samePlannerProposal(*record.Proposal, proposal) {
		t.Fatalf("restarted receipt = %+v, err=%v", record, err)
	}
	replayed, created, err := restarted.RecordPlannerProposal(
		ctx,
		intent,
		RecordPlannerProposalInput{
			Response:         raw,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  validationError,
		},
	)
	if err != nil || created || !samePlannerProposal(replayed, proposal) {
		t.Fatalf(
			"restarted proposal replay = %+v, created=%v, err=%v",
			replayed,
			created,
			err,
		)
	}
	_, _, err = restarted.RecordPlannerProposal(
		ctx,
		intent,
		RecordPlannerProposalInput{
			Response:         raw,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  validationError + "different suffix",
		},
	)
	if !errors.Is(err, ErrPlannerProposalConflict) {
		t.Fatalf("same bounded prefix error replay = %v", err)
	}
}

func TestPlannerProposalPayloadBoundaries(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, exactInput := plannerAttemptFixture(t, opened, "exact-bound")
	exactIntent, _, err := opened.BeginPlannerAttempt(ctx, exactInput)
	if err != nil {
		t.Fatal(err)
	}
	exact := append(
		append([]byte{'"'}, bytes.Repeat(
			[]byte{'x'},
			MaxPlannerProposalPayloadBytes-2,
		)...),
		'"',
	)
	exactProposal, created, err := opened.RecordPlannerProposal(
		ctx,
		exactIntent,
		RecordPlannerProposalInput{
			Response:         exact,
			ValidationStatus: PlannerProposalValid,
		},
	)
	if err != nil || !created ||
		len(exactProposal.Payload) != MaxPlannerProposalPayloadBytes {
		t.Fatalf(
			"exact payload boundary = bytes:%d created=%v err=%v",
			len(exactProposal.Payload),
			created,
			err,
		)
	}

	_, overInput := plannerAttemptFixture(t, opened, "over-bound")
	overIntent, _, err := opened.BeginPlannerAttempt(ctx, overInput)
	if err != nil {
		t.Fatal(err)
	}
	over := append(
		append([]byte{'"'}, bytes.Repeat(
			[]byte{'x'},
			MaxPlannerProposalPayloadBytes-1,
		)...),
		'"',
	)
	if _, _, err := opened.RecordPlannerProposal(
		ctx,
		overIntent,
		RecordPlannerProposalInput{
			Response:         over,
			ValidationStatus: PlannerProposalValid,
		},
	); !errors.Is(err, ErrPlannerProposalPayloadTooLarge) {
		t.Fatalf("oversized valid payload error = %v", err)
	}
	invalid, created, err := opened.RecordPlannerProposal(
		ctx,
		overIntent,
		RecordPlannerProposalInput{
			Response:         over,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  "response exceeds proposal schema boundary",
		},
	)
	if err != nil || !created || len(invalid.Payload) != 0 ||
		invalid.PayloadHash != nil ||
		invalid.ResponseHash != plannerHash(over) {
		t.Fatalf(
			"oversized invalid receipt = %+v, created=%v, err=%v",
			invalid,
			created,
			err,
		)
	}

	_, hardInput := plannerAttemptFixture(t, opened, "hard-response-bound")
	hardIntent, _, err := opened.BeginPlannerAttempt(ctx, hardInput)
	if err != nil {
		t.Fatal(err)
	}
	parseLimitedResponse := bytes.Repeat(
		[]byte{'x'},
		MaxPlannerResponseParseBytes+1,
	)
	parseLimited, created, err := opened.RecordPlannerProposal(
		ctx,
		hardIntent,
		RecordPlannerProposalInput{
			Response:         parseLimitedResponse,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  "response exceeds canonical parse boundary",
		},
	)
	if err != nil || !created ||
		len(parseLimited.Payload) != 0 ||
		parseLimited.PayloadHash != nil ||
		parseLimited.ResponseHash != plannerHash(parseLimitedResponse) {
		t.Fatalf(
			"parse-limited invalid receipt = %+v, created=%v, err=%v",
			parseLimited,
			created,
			err,
		)
	}

	_, hardLimitInput := plannerAttemptFixture(
		t,
		opened,
		"hard-response-reject",
	)
	hardLimitIntent, _, err := opened.BeginPlannerAttempt(ctx, hardLimitInput)
	if err != nil {
		t.Fatal(err)
	}
	tooLargeResponse := bytes.Repeat(
		[]byte{'x'},
		MaxPlannerResponseBytes+1,
	)
	if _, _, err := opened.RecordPlannerProposal(
		ctx,
		hardLimitIntent,
		RecordPlannerProposalInput{
			Response:         tooLargeResponse,
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  "malformed oversized response",
		},
	); !errors.Is(err, ErrPlannerResponseTooLarge) {
		t.Fatalf("hard response boundary error = %v", err)
	}
	if _, _, err := opened.RecordPlannerProposal(
		ctx,
		hardLimitIntent,
		RecordPlannerProposalInput{
			Response:         []byte(`{"invalid":true}`),
			ValidationStatus: PlannerProposalInvalid,
			ValidationError:  "bad\x00error",
		},
	); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("validation error NUL boundary = %v", err)
	}
}

func TestListUnresolvedPlannerAttemptsUsesCompositeCursor(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, base := plannerAttemptFixture(t, opened, "paging")
	for index := int64(1); index <= 101; index++ {
		input := base
		input.IdempotencyKey = "planner:paging:" +
			fmt.Sprintf("%03d", index)
		input.Attempt = index
		if _, created, err := opened.BeginPlannerAttempt(
			ctx,
			input,
		); err != nil || !created {
			t.Fatalf(
				"begin paging attempt %d: created=%v err=%v",
				index,
				created,
				err,
			)
		}
	}
	first, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{Limit: 100},
	)
	if err != nil || len(first) != 100 {
		t.Fatalf("first unresolved page = %d, err=%v", len(first), err)
	}
	last := first[len(first)-1]
	second, err := opened.ListUnresolvedPlannerAttempts(
		ctx,
		PlannerAttemptFilter{
			AfterStartedAt: last.StartedAt,
			AfterID:        last.ID,
			Limit:          100,
		},
	)
	if err != nil || len(second) != 1 {
		t.Fatalf("second unresolved page = %+v, err=%v", second, err)
	}
	if second[0].StartedAt < last.StartedAt ||
		(second[0].StartedAt == last.StartedAt &&
			second[0].ID <= last.ID) {
		t.Fatalf(
			"composite cursor regressed: last=%+v next=%+v",
			last,
			second[0],
		)
	}
	seen := make(map[string]bool, 101)
	for _, value := range append(first, second...) {
		if seen[value.ID] {
			t.Fatalf("duplicate attempt across pages: %s", value.ID)
		}
		seen[value.ID] = true
	}
	if len(seen) != 101 {
		t.Fatalf("paged attempts = %d, want 101", len(seen))
	}
}

func TestPlannerLedgerDatabaseImmutabilityAndIdentityGuards(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, input := plannerAttemptFixture(t, opened, "guards")
	intent, _, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	proposal, _, err := opened.RecordPlannerProposal(
		ctx,
		intent,
		RecordPlannerProposalInput{
			Response:         []byte(`{"fanout":false}`),
			ValidationStatus: PlannerProposalValid,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"planner_attempt_intents",
		"planner_attempt_proposals",
	} {
		rows, err := opened.db.QueryContext(
			ctx,
			"PRAGMA foreign_key_list("+table+")",
		)
		if err != nil {
			t.Fatal(err)
		}
		hasForeignKey := rows.Next()
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			t.Fatal(errors.Join(rowsErr, closeErr))
		}
		if hasForeignKey {
			t.Fatalf("%s unexpectedly depends on a foreign key", table)
		}
	}

	for _, statement := range []string{
		"UPDATE planner_attempt_intents SET attempt = attempt + 1 WHERE id = ?",
		"DELETE FROM planner_attempt_intents WHERE id = ?",
		"UPDATE planner_attempt_proposals SET response_hash = lower(hex(randomblob(32))) WHERE attempt_id = ?",
		"DELETE FROM planner_attempt_proposals WHERE attempt_id = ?",
	} {
		if _, err := opened.db.ExecContext(ctx, statement, intent.ID); err == nil ||
			!strings.Contains(err.Error(), "immutable") {
			t.Fatalf("immutability statement %q error = %v", statement, err)
		}
	}

	if _, err := opened.db.ExecContext(ctx, `
		INSERT INTO planner_attempt_intents(
			id, board, task_id, task_updated_at, graph_revision, kind,
			schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at
		)
		SELECT 'pla_identity_guard', board, task_id, task_updated_at,
			graph_revision, kind, schema_version, snapshot_hash, config_hash,
			'planner:identity-guard', attempt, started_at
		FROM planner_attempt_intents WHERE id = ?
	`, intent.ID); err != nil {
		t.Fatal(err)
	}
	_, err = opened.db.ExecContext(ctx, `
		INSERT INTO planner_attempt_proposals(
			attempt_id, board, task_id, task_updated_at, graph_revision,
			kind, schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at, response_hash, payload_json, payload_hash,
			validation_status, validation_error, validation_error_hash,
			recorded_at
		)
		SELECT 'pla_identity_guard', board, 'wrong_task', task_updated_at,
			graph_revision, kind, schema_version, snapshot_hash, config_hash,
			'planner:identity-guard', attempt, started_at, response_hash,
			payload_json, payload_hash, validation_status, validation_error,
			validation_error_hash, recorded_at
		FROM planner_attempt_proposals WHERE attempt_id = ?
	`, proposal.AttemptID)
	if err == nil || !strings.Contains(
		err.Error(),
		"does not match its attempt intent",
	) {
		t.Fatalf("proposal identity guard error = %v", err)
	}
}

func TestPlannerLedgerMigratesV28AndPreservesExistingTasks(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := plannerAttemptFixture(t, opened, "migration")
	if _, err := opened.db.ExecContext(ctx, `
		DROP TABLE planner_attempt_proposals;
		DROP TABLE planner_attempt_intents;
		PRAGMA user_version = 28;
	`); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	preserved, err := migrated.GetTask(ctx, task.ID)
	if err != nil || preserved.Task.ID != task.ID {
		t.Fatalf("preserved task = %+v, err=%v", preserved, err)
	}
	var version, tableCount, triggerCount, errorHashColumns int
	if err := migrated.db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table'
			AND name IN ('planner_attempt_intents', 'planner_attempt_proposals')
	`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger'
			AND name LIKE 'planner_attempt_%'
	`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pragma_table_info('planner_attempt_proposals')
		WHERE name = 'validation_error_hash'
	`).Scan(&errorHashColumns); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || schemaVersion != 31 ||
		tableCount != 2 || triggerCount != 5 ||
		errorHashColumns != 1 {
		t.Fatalf(
			"migration version=%d constant=%d tables=%d triggers=%d errorHashColumns=%d",
			version,
			schemaVersion,
			tableCount,
			triggerCount,
			errorHashColumns,
		)
	}
}

func TestPlannerProposalResultColumnsRejectOversizeAndWrongIdentity(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, input := plannerAttemptFixture(t, opened, "sql-boundary")
	intent, _, err := opened.BeginPlannerAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	_, err = opened.db.ExecContext(ctx, `
		INSERT INTO planner_attempt_proposals(
			attempt_id, board, task_id, task_updated_at, graph_revision,
			kind, schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at, response_hash, payload_json, payload_hash,
			validation_status, validation_error, validation_error_hash,
			recorded_at
		)
		SELECT id, board, task_id, task_updated_at, graph_revision, kind,
			schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at, ?, ?, ?, 'valid', NULL, NULL, ?
		FROM planner_attempt_intents WHERE id = ?
	`,
		plannerHash([]byte("oversized")),
		`"`+strings.Repeat("x", MaxPlannerProposalPayloadBytes)+`"`,
		plannerHash([]byte("not relevant")),
		plannerAttemptNow(),
		intent.ID,
	)
	if err == nil {
		t.Fatal("oversized SQL proposal unexpectedly inserted")
	}
	_, err = opened.db.ExecContext(ctx, `
		INSERT INTO planner_attempt_proposals(
			attempt_id, board, task_id, task_updated_at, graph_revision,
			kind, schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at, response_hash, payload_json, payload_hash,
			validation_status, validation_error, validation_error_hash,
			recorded_at
		)
		SELECT id, board, task_id, task_updated_at, graph_revision, kind,
			schema_version, snapshot_hash, config_hash, idempotency_key,
			attempt, started_at, ?, ?, ?, 'invalid', 'bad response', NULL, ?
		FROM planner_attempt_intents WHERE id = ?
	`,
		plannerHash([]byte(`{"invalid":true}`)),
		`{"invalid":true}`,
		plannerHash([]byte(`{"invalid":true}`)),
		plannerAttemptNow(),
		intent.ID,
	)
	if err == nil {
		t.Fatal("invalid SQL proposal without error hash unexpectedly inserted")
	}

	var count int
	if err := opened.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM planner_attempt_proposals WHERE attempt_id = ?",
		intent.ID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed boundary insert left %d proposal rows", count)
	}
}
