package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	_ "modernc.org/sqlite"
)

func createAttemptTestIncident(
	t *testing.T,
	opened *Store,
	board string,
	trigger model.CoordinationTrigger,
) model.CoordinationIncident {
	t.Helper()
	revision := int64(0)
	incident, created, err := opened.CreateCoordinationIncident(
		context.Background(),
		CreateCoordinationIncidentInput{
			Board: board, Trigger: trigger, ExpectedGraphRevision: &revision,
			Summary: "Coordinator analysis is required",
		},
	)
	if err != nil || !created {
		t.Fatalf("create coordination incident: created=%v value=%+v err=%v", created, incident, err)
	}
	return incident
}

func reserveAttemptInput(
	id string,
	incident model.CoordinationIncident,
	revision int64,
	current time.Time,
) ReserveCoordinationAttemptInput {
	return ReserveCoordinationAttemptInput{
		ID: id, IncidentID: incident.ID, Board: incident.Board,
		ExpectedGraphRevision: &revision,
		Since:                 current.Add(-time.Hour), Current: current,
		MaxCalls: 10, TTL: time.Minute,
	}
}

type boundAttemptProposalFixture struct {
	opened   *Store
	current  time.Time
	incident model.CoordinationIncident
	attempt  model.CoordinationAttempt
	proposal model.CoordinationProposal
}

func createBoundAttemptProposalFixture(t *testing.T) boundAttemptProposalFixture {
	t.Helper()
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	current := time.Now().UTC()
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		reserveAttemptInput("bound-attempt-"+newID("test"), incident, 0, current),
	)
	if err != nil || !reserved.Reserved {
		opened.Close()
		t.Fatalf("reserve bound attempt: %+v, %v", reserved, err)
	}
	attemptID := reserved.Attempt.ID
	revision := reserved.Incident.GraphRevision
	proposal, created, err := opened.CreateCoordinationProposal(
		ctx,
		CreateCoordinationProposalInput{
			IncidentID: incident.ID, AttemptID: &attemptID,
			CoordinatorAgent: "coordinator", CoordinatorModel: "coordinator-model",
			CoordinatorProvider:   "coordinator-provider",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               current.Add(time.Second),
			Summary:               "Bound proposal",
			Rationale:             "Exercise exact attempt recovery.",
		},
	)
	if err != nil || !created {
		opened.Close()
		t.Fatalf("create bound proposal: created=%t value=%+v error=%v", created, proposal, err)
	}
	return boundAttemptProposalFixture{
		opened: opened, current: current,
		incident: reserved.Incident, attempt: reserved.Attempt, proposal: proposal,
	}
}

func recoverBoundAttemptInput(
	fixture boundAttemptProposalFixture,
	status model.CoordinationAttemptStatus,
	attemptError *string,
) RecoverCoordinationAttemptInput {
	proposalRevision := fixture.proposal.ExpectedGraphRevision
	incidentRevision := fixture.incident.GraphRevision
	return RecoverCoordinationAttemptInput{
		Board: fixture.incident.Board, ProposalID: fixture.proposal.ID,
		ExpectedProposalStatus:        fixture.proposal.Status,
		ExpectedProposalGraphRevision: &proposalRevision,
		ExpectedIncidentGraphRevision: &incidentRevision,
		ClaimToken:                    fixture.incident.ClaimToken,
		Current:                       fixture.current.Add(2 * time.Second),
		Status:                        status,
		Error:                         attemptError,
	}
}

func TestBoundCoordinationProposalForeignKeysCascadeSafely(t *testing.T) {
	t.Run("attempt deletion removes bound proposal", func(t *testing.T) {
		fixture := createBoundAttemptProposalFixture(t)
		defer fixture.opened.Close()
		ctx := context.Background()
		if _, err := fixture.opened.db.ExecContext(
			ctx,
			"DELETE FROM coordination_attempts WHERE id = ?",
			fixture.attempt.ID,
		); err != nil {
			t.Fatal(err)
		}
		var proposals, incidents int
		if err := fixture.opened.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM coordination_proposals WHERE id = ?",
			fixture.proposal.ID,
		).Scan(&proposals); err != nil {
			t.Fatal(err)
		}
		if err := fixture.opened.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM coordination_incidents WHERE id = ?",
			fixture.incident.ID,
		).Scan(&incidents); err != nil {
			t.Fatal(err)
		}
		if proposals != 0 || incidents != 1 {
			t.Fatalf("attempt cascade counts: proposals=%d incidents=%d", proposals, incidents)
		}
	})

	t.Run("incident deletion removes attempt and proposal", func(t *testing.T) {
		fixture := createBoundAttemptProposalFixture(t)
		defer fixture.opened.Close()
		ctx := context.Background()
		if _, err := fixture.opened.db.ExecContext(
			ctx,
			"DELETE FROM coordination_incidents WHERE id = ?",
			fixture.incident.ID,
		); err != nil {
			t.Fatal(err)
		}
		var attempts, proposals int
		if err := fixture.opened.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM coordination_attempts WHERE id = ?",
			fixture.attempt.ID,
		).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if err := fixture.opened.db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM coordination_proposals WHERE id = ?",
			fixture.proposal.ID,
		).Scan(&proposals); err != nil {
			t.Fatal(err)
		}
		rows, err := fixture.opened.db.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		foreignKeyViolation := rows.Next()
		if attempts != 0 || proposals != 0 || foreignKeyViolation {
			t.Fatalf(
				"incident cascade counts: attempts=%d proposals=%d foreignKeyViolation=%t",
				attempts,
				proposals,
				foreignKeyViolation,
			)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRecoverBoundCoordinationAttemptRequiresExactTerminalResult(t *testing.T) {
	t.Run("same terminal result is idempotent", func(t *testing.T) {
		fixture := createBoundAttemptProposalFixture(t)
		defer fixture.opened.Close()
		ctx := context.Background()
		finished, err := fixture.opened.FinishCoordinationAttempt(
			ctx,
			fixture.attempt.ID,
			FinishCoordinationAttemptInput{
				Status:           model.CoordinationAttemptSucceeded,
				SelectedAgent:    fixture.proposal.CoordinatorAgent,
				SelectedModel:    fixture.proposal.CoordinatorModel,
				SelectedProvider: fixture.proposal.CoordinatorProvider,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		recovered, changed, err := fixture.opened.RecoverCoordinationAttemptForProposal(
			ctx,
			recoverBoundAttemptInput(
				fixture,
				model.CoordinationAttemptSucceeded,
				nil,
			),
		)
		if err != nil || changed || recovered.ID != finished.ID ||
			recovered.EndedAt == nil || *recovered.EndedAt != *finished.EndedAt {
			t.Fatalf(
				"idempotent terminal recovery: changed=%t recovered=%+v error=%v",
				changed,
				recovered,
				err,
			)
		}

		oppositeError := "recovered validation failed"
		_, changed, err = fixture.opened.RecoverCoordinationAttemptForProposal(
			ctx,
			recoverBoundAttemptInput(
				fixture,
				model.CoordinationAttemptFailed,
				&oppositeError,
			),
		)
		if changed || !errors.Is(err, ErrCoordinationStateConflict) {
			t.Fatalf("opposite terminal recovery: changed=%t error=%v", changed, err)
		}
		unchanged, err := fixture.opened.ListCoordinationAttempts(
			ctx,
			CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
		)
		if err != nil || len(unchanged) != 1 ||
			unchanged[0].Status != model.CoordinationAttemptSucceeded ||
			unchanged[0].EndedAt == nil ||
			*unchanged[0].EndedAt != *finished.EndedAt {
			t.Fatalf("opposite recovery mutated terminal attempt: %+v, %v", unchanged, err)
		}
	})

	t.Run("selection mismatch conflicts", func(t *testing.T) {
		fixture := createBoundAttemptProposalFixture(t)
		defer fixture.opened.Close()
		ctx := context.Background()
		finished, err := fixture.opened.FinishCoordinationAttempt(
			ctx,
			fixture.attempt.ID,
			FinishCoordinationAttemptInput{
				Status:        model.CoordinationAttemptSucceeded,
				SelectedAgent: "different-coordinator",
				SelectedModel: "different-model",
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		_, changed, err := fixture.opened.RecoverCoordinationAttemptForProposal(
			ctx,
			recoverBoundAttemptInput(
				fixture,
				model.CoordinationAttemptSucceeded,
				nil,
			),
		)
		if changed || !errors.Is(err, ErrCoordinationStateConflict) {
			t.Fatalf("selection mismatch recovery: changed=%t error=%v", changed, err)
		}
		unchanged, err := fixture.opened.ListCoordinationAttempts(
			ctx,
			CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
		)
		if err != nil || len(unchanged) != 1 ||
			unchanged[0].SelectedAgent != finished.SelectedAgent ||
			unchanged[0].SelectedModel != finished.SelectedModel ||
			unchanged[0].Status != model.CoordinationAttemptSucceeded {
			t.Fatalf("selection mismatch mutated terminal attempt: %+v, %v", unchanged, err)
		}
	})
}

func TestReserveCoordinationAttemptAtomicallyEnforcesBudgetAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	seed, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	firstIncident := createAttemptTestIncident(
		t,
		seed,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	secondIncident := createAttemptTestIncident(
		t,
		seed,
		"default",
		model.CoordinationTriggerAgentExhausted,
	)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	firstStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()

	current := time.Date(2030, time.March, 4, 5, 6, 7, 0, time.UTC)
	firstInput := reserveAttemptInput("reserve-race-first", firstIncident, 0, current)
	secondInput := reserveAttemptInput("reserve-race-second", secondIncident, 0, current)
	firstInput.MaxCalls, secondInput.MaxCalls = 1, 1
	type reservation struct {
		result ReserveCoordinationAttemptResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan reservation, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		value, err := firstStore.ReserveCoordinationAttempt(ctx, firstInput)
		results <- reservation{result: value, err: err}
	}()
	go func() {
		defer workers.Done()
		<-start
		value, err := secondStore.ReserveCoordinationAttempt(ctx, secondInput)
		results <- reservation{result: value, err: err}
	}()
	close(start)
	workers.Wait()
	close(results)

	reserved, exhausted := 0, 0
	var exhaustedIncident string
	for value := range results {
		if value.err != nil {
			t.Fatalf("concurrent reservation: %v", value.err)
		}
		if value.result.Reserved {
			reserved++
			if value.result.Attempt.Status != model.CoordinationAttemptStarted ||
				value.result.Incident.ClaimToken == "" {
				t.Fatalf("invalid winning reservation: %+v", value.result)
			}
		}
		if value.result.BudgetExhausted {
			exhausted++
			exhaustedIncident = value.result.Incident.ID
			if value.result.RetryAt == nil {
				t.Fatalf("budget result has no retry time: %+v", value.result)
			}
		}
	}
	if reserved != 1 || exhausted != 1 {
		t.Fatalf("concurrent budget results: reserved=%d exhausted=%d", reserved, exhausted)
	}
	attempts, err := firstStore.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(attempts) != 1 {
		t.Fatalf("budget race persisted attempts = %+v, %v", attempts, err)
	}
	notClaimed, err := firstStore.GetCoordinationIncident(ctx, exhaustedIncident)
	if err != nil {
		t.Fatal(err)
	}
	if notClaimed.Status != model.CoordinationIncidentOpen ||
		notClaimed.ClaimToken != "" || notClaimed.ClaimExpiresAt != nil {
		t.Fatalf("budget loser was claimed: %+v", notClaimed)
	}
}

func TestReserveCoordinationAttemptDoesNotStealLiveClaimAndRetriesExactID(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	current := time.Date(2030, time.April, 5, 6, 7, 8, 0, time.UTC)
	input := reserveAttemptInput("reserve-live-owner", incident, 0, current)
	first, err := opened.ReserveCoordinationAttempt(ctx, input)
	if err != nil || !first.Reserved || first.Incident.ClaimToken == "" {
		t.Fatalf("first reservation = %+v, %v", first, err)
	}

	otherInput := reserveAttemptInput(
		"reserve-live-other",
		incident,
		0,
		current.Add(time.Second),
	)
	other, err := opened.ReserveCoordinationAttempt(ctx, otherInput)
	if err != nil {
		t.Fatal(err)
	}
	if other.Reserved || other.BudgetExhausted ||
		other.Incident.ClaimToken != "" ||
		other.Attempt.ID != "" {
		t.Fatalf("live claim was stolen or charged: %+v", other)
	}
	stored, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil || stored.ClaimToken != first.Incident.ClaimToken {
		t.Fatalf("live owner changed in storage: %+v, %v", stored, err)
	}
	attempts, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(attempts) != 1 ||
		attempts[0].ID != first.Attempt.ID ||
		attempts[0].Status != model.CoordinationAttemptStarted {
		t.Fatalf("attempts after live contention = %+v, %v", attempts, err)
	}

	retryInput := input
	retryInput.Current = current.Add(2 * time.Second)
	retryInput.Since = retryInput.Current.Add(-time.Hour)
	retried, err := opened.ReserveCoordinationAttempt(ctx, retryInput)
	if err != nil || !retried.Reserved ||
		retried.Attempt.ID != first.Attempt.ID ||
		retried.Incident.ClaimToken != first.Incident.ClaimToken {
		t.Fatalf("exact reservation retry = %+v, %v", retried, err)
	}
}

func TestReserveCoordinationAttemptRejectsStaleOpenGraph(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	focus, err := opened.CreateTask(ctx, CreateTaskInput{Title: "focus"})
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		TaskID: &focus.Task.ID, Trigger: model.CoordinationTriggerRepeatedBlock,
		Summary: "Task remains blocked", ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := opened.CreateTask(ctx, CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, prerequisite.Task.ID, focus.Task.ID); err != nil {
		t.Fatal(err)
	}
	current := time.Date(2030, time.May, 6, 7, 8, 9, 0, time.UTC)

	staleBoard := reserveAttemptInput("reserve-stale-board", incident, 0, current)
	if _, err := opened.ReserveCoordinationAttempt(ctx, staleBoard); !errors.Is(
		err,
		ErrGraphRevisionConflict,
	) {
		t.Fatalf("stale board revision error = %v", err)
	}
	staleIncident := reserveAttemptInput("reserve-stale-incident", incident, 1, current)
	if _, err := opened.ReserveCoordinationAttempt(ctx, staleIncident); !errors.Is(
		err,
		ErrGraphRevisionConflict,
	) {
		t.Fatalf("stale open incident revision error = %v", err)
	}
	refreshed, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		TaskID: &focus.Task.ID, Trigger: model.CoordinationTriggerRepeatedBlock,
		Summary: "Task is still blocked", ExpectedGraphRevision: revisionPointer(1),
	})
	if err != nil || created || refreshed.GraphRevision != 1 {
		t.Fatalf("refresh incident: created=%v value=%+v err=%v", created, refreshed, err)
	}
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		reserveAttemptInput("reserve-current-graph", refreshed, 1, current),
	)
	if err != nil || !reserved.Reserved ||
		reserved.Incident.GraphRevision != 1 ||
		reserved.Attempt.Status != model.CoordinationAttemptStarted {
		t.Fatalf("current graph reservation = %+v, %v", reserved, err)
	}
}

func TestReserveCoordinationAttemptRebasesExpiredClaimAndCleansPriorAttempt(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	focus, err := opened.CreateTask(ctx, CreateTaskInput{Title: "focus"})
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		TaskID: &focus.Task.ID, Trigger: model.CoordinationTriggerGraphStalled,
		Summary: "Graph is stalled", ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2030, time.June, 7, 8, 9, 10, 0, time.UTC)
	firstInput := reserveAttemptInput("reserve-before-expiry", incident, 0, current)
	firstInput.TTL = MinCoordinationIncidentClaimTTL
	first, err := opened.ReserveCoordinationAttempt(ctx, firstInput)
	if err != nil || !first.Reserved {
		t.Fatalf("first reservation = %+v, %v", first, err)
	}
	prerequisite, err := opened.CreateTask(ctx, CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, prerequisite.Task.ID, focus.Task.ID); err != nil {
		t.Fatal(err)
	}

	liveInput := reserveAttemptInput(
		"reserve-before-lease-boundary",
		incident,
		1,
		current.Add(MinCoordinationIncidentClaimTTL-time.Nanosecond),
	)
	liveInput.TTL = MinCoordinationIncidentClaimTTL
	live, err := opened.ReserveCoordinationAttempt(ctx, liveInput)
	if err != nil || live.Reserved ||
		live.Incident.ClaimToken != "" ||
		live.Incident.GraphRevision != 0 {
		t.Fatalf("live stale reservation was rewritten: %+v, %v", live, err)
	}

	reclaimTime := current.Add(MinCoordinationIncidentClaimTTL)
	reclaimInput := reserveAttemptInput(
		"reserve-after-expiry",
		incident,
		1,
		reclaimTime,
	)
	reclaimInput.TTL = MinCoordinationIncidentClaimTTL
	reclaimed, err := opened.ReserveCoordinationAttempt(ctx, reclaimInput)
	if err != nil || !reclaimed.Reserved ||
		reclaimed.Incident.ClaimToken == first.Incident.ClaimToken ||
		reclaimed.Incident.GraphRevision != 1 {
		t.Fatalf("expired reservation was not rebased: %+v, %v", reclaimed, err)
	}
	attempts, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		IncidentID: incident.ID,
	})
	if err != nil || len(attempts) != 2 {
		t.Fatalf("reclaimed attempts = %+v, %v", attempts, err)
	}
	var expired, active *model.CoordinationAttempt
	for index := range attempts {
		switch attempts[index].ID {
		case first.Attempt.ID:
			expired = &attempts[index]
		case reclaimed.Attempt.ID:
			active = &attempts[index]
		}
	}
	if expired == nil || expired.Status != model.CoordinationAttemptFailed ||
		expired.Error == nil || *expired.Error != coordinationLeaseExpiredError ||
		expired.EndedAt == nil {
		t.Fatalf("prior attempt was not failed cleanly: %+v", expired)
	}
	if active == nil || active.Status != model.CoordinationAttemptStarted ||
		active.EndedAt != nil || active.Error != nil {
		t.Fatalf("new attempt is not active: %+v", active)
	}
	expectedEnd := reclaimTime.UTC().Format(coordinationAttemptTimestampLayout)
	if *expired.EndedAt != expectedEnd {
		t.Fatalf("expired attempt endedAt = %q, want %q", *expired.EndedAt, expectedEnd)
	}
}

func TestReserveCoordinationAttemptBudgetCountsStartedAndFailed(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	audited := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	failed, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "budget-failed", IncidentID: audited.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := "analysis failed"
	if _, err := opened.FinishCoordinationAttempt(ctx, failed.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptFailed, Error: &message,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "budget-started", IncidentID: audited.ID,
	}); err != nil {
		t.Fatal(err)
	}
	waiting := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerAgentExhausted,
	)
	current := time.Now().UTC().Add(time.Minute)
	input := reserveAttemptInput("budget-rejected", waiting, 0, current)
	input.MaxCalls = 2
	exhausted, err := opened.ReserveCoordinationAttempt(ctx, input)
	if err != nil || exhausted.Reserved || !exhausted.BudgetExhausted ||
		exhausted.RetryAt == nil {
		t.Fatalf("budget reservation = %+v, %v", exhausted, err)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, *exhausted.RetryAt)
	if err != nil || !retryAt.After(current) {
		t.Fatalf("budget retryAt = %v, %v; current=%v", retryAt, err, current)
	}
	attempts, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(attempts) != 2 {
		t.Fatalf("budget failure persisted another call: %+v, %v", attempts, err)
	}
	stillOpen, err := opened.GetCoordinationIncident(ctx, waiting.ID)
	if err != nil || stillOpen.Status != model.CoordinationIncidentOpen {
		t.Fatalf("budget failure claimed incident: %+v, %v", stillOpen, err)
	}
}

func TestReserveCoordinationAttemptValidatesPolicyInputs(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	current := time.Now().UTC()
	valid := reserveAttemptInput("reserve-validation", incident, 0, current)
	tests := []struct {
		name   string
		mutate func(*ReserveCoordinationAttemptInput)
	}{
		{name: "missing graph", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.ExpectedGraphRevision = nil
		}},
		{name: "negative graph", mutate: func(value *ReserveCoordinationAttemptInput) {
			revision := int64(-1)
			value.ExpectedGraphRevision = &revision
		}},
		{name: "zero max calls", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.MaxCalls = 0
		}},
		{name: "oversized max calls", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.MaxCalls = MaxCoordinationAttemptCalls + 1
		}},
		{name: "short ttl", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.TTL = MinCoordinationIncidentClaimTTL - time.Nanosecond
		}},
		{name: "long ttl", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.TTL = MaxCoordinationIncidentClaimTTL + time.Nanosecond
		}},
		{name: "zero current", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.Current = time.Time{}
		}},
		{name: "zero since", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.Since = time.Time{}
		}},
		{name: "future since", mutate: func(value *ReserveCoordinationAttemptInput) {
			value.Since = value.Current.Add(time.Nanosecond)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			if _, err := opened.ReserveCoordinationAttempt(ctx, input); err == nil {
				t.Fatal("invalid reservation succeeded")
			}
		})
	}
	attempts, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(attempts) != 0 {
		t.Fatalf("invalid inputs persisted attempts: %+v, %v", attempts, err)
	}
}

func TestCoordinationAttemptsCountFailuresAndPersistSelection(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	since := time.Now().UTC().Add(-time.Minute)
	failed, created, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "attempt-failed", IncidentID: incident.ID,
		SelectedAgent: " codex-coordinator ", SelectedRuntime: model.RuntimeCodex,
		SelectedModel: " gpt-5.4 ", SelectedProvider: " openai ", SelectedSource: " global_default ",
	})
	if err != nil || !created {
		t.Fatalf("start failed attempt: created=%v value=%+v err=%v", created, failed, err)
	}
	if failed.SelectedAgent != "codex-coordinator" || failed.SelectedModel != "gpt-5.4" ||
		failed.SelectedProvider != "openai" || failed.SelectedSource != "global_default" {
		t.Fatalf("selection was not normalized: %+v", failed)
	}
	failure := "analysis command failed"
	failed, err = opened.FinishCoordinationAttempt(ctx, failed.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptFailed, Error: &failure,
	})
	if err != nil || failed.Status != model.CoordinationAttemptFailed ||
		failed.Error == nil || *failed.Error != failure || failed.EndedAt == nil {
		t.Fatalf("finish failed attempt: %+v, %v", failed, err)
	}

	succeeded, created, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "attempt-succeeded", IncidentID: incident.ID,
		SelectedAgent: "claude-coordinator", SelectedRuntime: model.RuntimeClaude,
		SelectedSource: "global_fallback",
	})
	if err != nil || !created {
		t.Fatalf("start successful attempt: created=%v value=%+v err=%v", created, succeeded, err)
	}
	succeeded, err = opened.FinishCoordinationAttempt(ctx, succeeded.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptSucceeded,
	})
	if err != nil || succeeded.Status != model.CoordinationAttemptSucceeded ||
		succeeded.Error != nil || succeeded.EndedAt == nil {
		t.Fatalf("finish successful attempt: %+v, %v", succeeded, err)
	}

	count, err := opened.CountCoordinationAttemptsSince(ctx, "default", since)
	if err != nil || count != 2 {
		t.Fatalf("attempt count including failure = %d, %v; want 2", count, err)
	}
	count, err = opened.CountCoordinationAttemptsSince(ctx, "default", time.Now().UTC().Add(time.Minute))
	if err != nil || count != 0 {
		t.Fatalf("future attempt count = %d, %v; want 0", count, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		Board: "default", IncidentID: incident.ID,
	})
	if err != nil || len(persisted) != 2 {
		t.Fatalf("persisted attempts = %+v, %v", persisted, err)
	}
	if persisted[0].Status != model.CoordinationAttemptSucceeded ||
		persisted[1].Status != model.CoordinationAttemptFailed {
		t.Fatalf("persisted statuses = %+v", persisted)
	}
}

func TestCoordinationAttemptStartAndFinishAreExactlyIdempotent(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	input := StartCoordinationAttemptInput{
		ID: "attempt-idempotent", IncidentID: incident.ID,
		SelectedAgent: "codex", SelectedRuntime: model.RuntimeCodex,
		SelectedModel: "gpt-5.4", SelectedProvider: "openai", SelectedSource: "global_role",
	}
	started, created, err := opened.StartCoordinationAttempt(ctx, input)
	if err != nil || !created {
		t.Fatalf("first start: created=%v value=%+v err=%v", created, started, err)
	}
	retried, created, err := opened.StartCoordinationAttempt(ctx, input)
	if err != nil || created || retried.StartedAt != started.StartedAt {
		t.Fatalf("idempotent start: created=%v value=%+v err=%v", created, retried, err)
	}
	different := input
	different.SelectedModel = "another-model"
	if _, _, err := opened.StartCoordinationAttempt(ctx, different); err == nil {
		t.Fatal("attempt ID was reused with different immutable selection")
	}

	longError := strings.Repeat("실패", MaxCoordinationAttemptErrorBytes)
	finished, err := opened.FinishCoordinationAttempt(ctx, started.ID, FinishCoordinationAttemptInput{
		ExpectedStatus: model.CoordinationAttemptStarted,
		Status:         model.CoordinationAttemptFailed,
		Error:          &longError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Error == nil || len(*finished.Error) > MaxCoordinationAttemptErrorBytes ||
		!utf8.ValidString(*finished.Error) || finished.EndedAt == nil {
		t.Fatalf("bounded finish result = %+v", finished)
	}
	firstEnd := *finished.EndedAt
	retriedFinish, err := opened.FinishCoordinationAttempt(ctx, started.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptFailed, Error: &longError,
	})
	if err != nil || retriedFinish.EndedAt == nil || *retriedFinish.EndedAt != firstEnd {
		t.Fatalf("idempotent finish = %+v, %v", retriedFinish, err)
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, started.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptSucceeded,
	}); !errors.Is(err, ErrCoordinationStateConflict) {
		t.Fatalf("competing finish error = %v", err)
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, started.ID, FinishCoordinationAttemptInput{
		ExpectedStatus: model.CoordinationAttemptSucceeded,
		Status:         model.CoordinationAttemptFailed,
	}); err == nil {
		t.Fatal("finish accepted a non-started expected status")
	}
}

func TestCoordinationAttemptFinishRecordsLateSuccessfulSelection(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	attempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "attempt-late-selection", IncidentID: incident.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	finish := FinishCoordinationAttemptInput{
		Status:        model.CoordinationAttemptSucceeded,
		SelectedAgent: " codex-coordinator ", SelectedRuntime: model.RuntimeCodex,
		SelectedModel: " gpt-5.4 ", SelectedProvider: " openai ",
		SelectedSource: " global_fallback ",
	}
	finished, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, finish)
	if err != nil {
		t.Fatal(err)
	}
	if finished.SelectedAgent != "codex-coordinator" ||
		finished.SelectedRuntime != model.RuntimeCodex ||
		finished.SelectedModel != "gpt-5.4" ||
		finished.SelectedProvider != "openai" ||
		finished.SelectedSource != "global_fallback" {
		t.Fatalf("late selection was not persisted: %+v", finished)
	}
	firstEnd := *finished.EndedAt
	retried, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, finish)
	if err != nil || retried.EndedAt == nil || *retried.EndedAt != firstEnd {
		t.Fatalf("late-selection finish retry = %+v, %v", retried, err)
	}
	conflicting := finish
	conflicting.SelectedModel = "different-model"
	if _, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, conflicting); !errors.Is(
		err,
		ErrCoordinationStateConflict,
	) {
		t.Fatalf("conflicting late selection error = %v", err)
	}

	// An omitted selection on an idempotent retry means "keep the recorded
	// selection"; it must not clear fields learned from OnSelected.
	withoutSelection, err := opened.FinishCoordinationAttempt(
		ctx,
		attempt.ID,
		FinishCoordinationAttemptInput{Status: model.CoordinationAttemptSucceeded},
	)
	if err != nil || withoutSelection.SelectedAgent != finished.SelectedAgent {
		t.Fatalf("selection-free finish retry = %+v, %v", withoutSelection, err)
	}
	restarted, created, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: attempt.ID, IncidentID: incident.ID,
	})
	if err != nil || created || restarted.SelectedAgent != finished.SelectedAgent {
		t.Fatalf("idempotent start after late selection: created=%v value=%+v err=%v", created, restarted, err)
	}
}

func TestCoordinationAttemptFinishRejectsConflictingPreselectedAgent(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	attempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID: incident.ID, SelectedAgent: "claude-coordinator",
		SelectedRuntime: model.RuntimeClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, FinishCoordinationAttemptInput{
		Status:        model.CoordinationAttemptSucceeded,
		SelectedAgent: "codex-coordinator", SelectedRuntime: model.RuntimeCodex,
	}); !errors.Is(err, ErrCoordinationStateConflict) {
		t.Fatalf("conflicting preselection error = %v", err)
	}
	listed, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(listed) != 1 ||
		listed[0].Status != model.CoordinationAttemptStarted ||
		listed[0].SelectedAgent != "claude-coordinator" {
		t.Fatalf("selection conflict mutated started attempt: %+v, %v", listed, err)
	}
	finished, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, FinishCoordinationAttemptInput{
		Status:        model.CoordinationAttemptSucceeded,
		SelectedAgent: "claude-coordinator", SelectedRuntime: model.RuntimeClaude,
		SelectedModel: "claude-opus-4-1",
	})
	if err != nil || finished.SelectedModel != "claude-opus-4-1" {
		t.Fatalf("compatible finish did not fill empty fields: %+v, %v", finished, err)
	}
}

func TestCoordinationAttemptFinishCASAllowsOneCompetingOutcome(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	seed, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident := createAttemptTestIncident(
		t,
		seed,
		"default",
		model.CoordinationTriggerAgentExhausted,
	)
	attempt, _, err := seed.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "attempt-race", IncidentID: incident.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := first.FinishCoordinationAttempt(ctx, attempt.ID, FinishCoordinationAttemptInput{
			Status: model.CoordinationAttemptSucceeded,
		})
		results <- err
	}()
	go func() {
		defer workers.Done()
		<-start
		message := "fallback chain failed"
		_, err := second.FinishCoordinationAttempt(ctx, attempt.ID, FinishCoordinationAttemptInput{
			Status: model.CoordinationAttemptFailed, Error: &message,
		})
		results <- err
	}()
	close(start)
	workers.Wait()
	close(results)

	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrCoordinationStateConflict):
			conflicts++
		default:
			t.Fatalf("unexpected competing finish error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("competing finishes: successes=%d conflicts=%d", successes, conflicts)
	}
	listed, err := first.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{})
	if err != nil || len(listed) != 1 || listed[0].EndedAt == nil ||
		listed[0].Status == model.CoordinationAttemptStarted {
		t.Fatalf("stored competing outcome = %+v, %v", listed, err)
	}
}

func TestCoordinationAttemptsEnforceBoardIncidentAndListScope(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	alpha := createAttemptTestIncident(
		t,
		opened,
		"alpha",
		model.CoordinationTriggerRepeatedBlock,
	)
	beta := createAttemptTestIncident(
		t,
		opened,
		"beta",
		model.CoordinationTriggerIntegrationConflict,
	)
	if _, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "wrong-board", IncidentID: beta.ID, Board: "alpha",
	}); err == nil {
		t.Fatal("attempt started against an incident from another board")
	}
	alphaAttempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "alpha-attempt", IncidentID: alpha.ID, Board: "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	betaAttempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "beta-attempt", IncidentID: beta.ID, Board: "beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, betaAttempt.ID, FinishCoordinationAttemptInput{
		Board: "alpha", Status: model.CoordinationAttemptSucceeded,
	}); err == nil {
		t.Fatal("attempt was finished through another board scope")
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, alphaAttempt.ID, FinishCoordinationAttemptInput{
		Board: "alpha", Status: model.CoordinationAttemptSucceeded,
	}); err != nil {
		t.Fatal(err)
	}
	message := "beta failed"
	if _, err := opened.FinishCoordinationAttempt(ctx, betaAttempt.ID, FinishCoordinationAttemptInput{
		Board: "beta", Status: model.CoordinationAttemptFailed, Error: &message,
	}); err != nil {
		t.Fatal(err)
	}

	alphaList, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{Board: "alpha"})
	if err != nil || len(alphaList) != 1 || alphaList[0].IncidentID != alpha.ID {
		t.Fatalf("alpha attempts = %+v, %v", alphaList, err)
	}
	betaList, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		Board: "beta", IncidentID: beta.ID, Status: model.CoordinationAttemptFailed,
	})
	if err != nil || len(betaList) != 1 || betaList[0].ID != betaAttempt.ID {
		t.Fatalf("filtered beta attempts = %+v, %v", betaList, err)
	}
	crossIncident, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		Board: "alpha", IncidentID: beta.ID,
	})
	if err != nil || len(crossIncident) != 0 {
		t.Fatalf("cross-board incident list = %+v, %v", crossIncident, err)
	}
	since := time.Now().UTC().Add(-time.Minute)
	alphaCount, err := opened.CountCoordinationAttemptsSince(ctx, "alpha", since)
	if err != nil || alphaCount != 1 {
		t.Fatalf("alpha count = %d, %v", alphaCount, err)
	}
	betaCount, err := opened.CountCoordinationAttemptsSince(ctx, "beta", since)
	if err != nil || betaCount != 1 {
		t.Fatalf("beta count = %d, %v", betaCount, err)
	}
	if _, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		Board: "alpha", Status: "unknown",
	}); err == nil {
		t.Fatal("invalid attempt status filter succeeded")
	}
	if _, err := opened.ListCoordinationAttempts(ctx, CoordinationAttemptFilter{
		Board: "alpha", Limit: 501,
	}); err == nil {
		t.Fatal("out-of-range attempt limit succeeded")
	}
}

func TestCoordinationAttemptRequiresActiveIncidentAndDoesNotExposeClaim(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	active := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	claimTime := time.Now().UTC()
	claimed, won, err := opened.ClaimCoordinationIncident(ctx, active.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !won {
		t.Fatalf("claim incident: won=%v value=%+v err=%v", won, claimed, err)
	}
	attempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID: active.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), claimed.ClaimToken) ||
		strings.Contains(string(encoded), "claimToken") {
		t.Fatalf("coordination claim leaked through attempt JSON: %s", encoded)
	}
	if _, err := opened.TransitionCoordinationIncident(ctx, active.ID, TransitionCoordinationIncidentInput{
		ExpectedStatus: model.CoordinationIncidentCoordinating,
		Status:         model.CoordinationIncidentResolved,
		ClaimToken:     claimed.ClaimToken,
		Current:        claimTime.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: "terminal-new-attempt", IncidentID: active.ID,
	}); err == nil {
		t.Fatal("new attempt started for a terminal incident")
	}
	retried, created, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		ID: attempt.ID, IncidentID: active.ID,
	})
	if err != nil || created || retried.ID != attempt.ID {
		t.Fatalf("idempotent start after incident resolution: created=%v value=%+v err=%v", created, retried, err)
	}
}

func TestLatestSchemaRecreatesCoordinationAttemptTableAndAdvancesVersion(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
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
	if _, err := raw.ExecContext(ctx, "DROP TABLE coordination_attempts"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 20"); err != nil {
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
	var version int
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 21 || version != schemaVersion {
		t.Fatalf("schema version = constant:%d database:%d, want 21", schemaVersion, version)
	}
	incident := createAttemptTestIncident(
		t,
		reopened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	attempt, created, err := reopened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID: incident.ID,
	})
	if err != nil || !created || attempt.Status != model.CoordinationAttemptStarted {
		t.Fatalf("recreated attempt table is unusable: created=%v value=%+v err=%v", created, attempt, err)
	}
}

func TestOpenRejectsCoordinationProposalTableWithoutAttemptBinding(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
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
		DROP TABLE coordination_proposals;
		CREATE TABLE coordination_proposals (
			id TEXT PRIMARY KEY,
			incident_id TEXT NOT NULL REFERENCES coordination_incidents(id) ON DELETE CASCADE,
			coordinator_agent TEXT NOT NULL,
			coordinator_model TEXT NOT NULL DEFAULT '',
			coordinator_provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK (status IN ('draft', 'validating', 'validated', 'awaiting_approval', 'approved', 'rejected', 'superseded', 'applying', 'applied', 'failed')),
			expected_graph_revision INTEGER NOT NULL CHECK (expected_graph_revision >= 0),
			summary TEXT NOT NULL,
			rationale TEXT NOT NULL,
			actions_json TEXT NOT NULL DEFAULT '[]',
			validation_errors_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			applied_at TEXT
		);
		PRAGMA user_version = 19;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if err == nil {
		reopened.Close()
		t.Fatal("unsupported coordination proposal schema was accepted")
	}
	if !strings.Contains(err.Error(), "coordination_proposals.attempt_id is missing") ||
		!strings.Contains(err.Error(), "fresh store or reset the data directory") {
		t.Fatalf("open error = %q, want fresh-store/reset guidance", err)
	}

	raw, err = sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.QueryContext(ctx, "PRAGMA table_info(coordination_proposals)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "attempt_id" {
			t.Fatal("initialization silently upgraded the incompatible table")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinationAttemptInputBounds(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerRetryExhausted,
	)
	if _, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID: incident.ID, SelectedRuntime: "invalid",
	}); err == nil {
		t.Fatal("invalid selected runtime succeeded")
	}
	if _, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID:    incident.ID,
		SelectedModel: strings.Repeat("x", maxCoordinationAttemptModelBytes+1),
	}); err == nil {
		t.Fatal("oversized selected model succeeded")
	}
	if _, err := opened.CountCoordinationAttemptsSince(ctx, "default", time.Time{}); err == nil {
		t.Fatal("zero attempt budget window succeeded")
	}
	attempt, _, err := opened.StartCoordinationAttempt(ctx, StartCoordinationAttemptInput{
		IncidentID: incident.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := "must not exist"
	if _, err := opened.FinishCoordinationAttempt(ctx, attempt.ID, FinishCoordinationAttemptInput{
		Status: model.CoordinationAttemptSucceeded, Error: &message,
	}); err == nil {
		t.Fatal("successful attempt accepted an error")
	}
}
