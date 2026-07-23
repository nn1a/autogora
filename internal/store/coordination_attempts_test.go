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

func TestLatestSchemaRecreatesCoordinationAttemptTableWithoutVersionChange(t *testing.T) {
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
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 19"); err != nil {
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
	if schemaVersion != 19 || version != schemaVersion {
		t.Fatalf("schema version = constant:%d database:%d, want 19", schemaVersion, version)
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
