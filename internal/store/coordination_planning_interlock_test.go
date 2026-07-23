package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func createPlanningInterlockIncident(
	t *testing.T,
	ctx context.Context,
	opened *Store,
	taskID string,
	trigger model.CoordinationTrigger,
) model.CoordinationIncident {
	t.Helper()
	graph, err := opened.RelationshipGraph(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	details, err := json.Marshal(map[string]any{
		"taskUpdatedAt": task.Task.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	revision := graph.GraphRevision
	incident, created, err := opened.CreateCoordinationIncident(
		ctx,
		CreateCoordinationIncidentInput{
			TaskID: &taskID, Trigger: trigger,
			Summary:               "claim interlock test",
			ExpectedGraphRevision: &revision,
			Details:               details,
		},
	)
	if err != nil || !created {
		t.Fatalf("create coordination incident: created=%t value=%+v err=%v", created, incident, err)
	}
	return incident
}

func TestGraphStalledCoordinatorClaimsWaitForLivePlannerLease(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2041, 1, 2, 3, 4, 5, 0, time.UTC)
	task := createPlanningTask(t, opened, "Planner owns this Triage task")
	incident := createPlanningInterlockIncident(
		t, ctx, opened, task.Task.ID, model.CoordinationTriggerGraphStalled,
	)
	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, AutoDecomposeMaxAttempts, 10*time.Second, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, decision, 1)

	revision := incident.GraphRevision
	claimedIncident, won, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               current.Add(time.Second),
		},
	)
	if err != nil || won || claimedIncident.Status != model.CoordinationIncidentOpen {
		t.Fatalf(
			"Coordinator claimed over live Planner: won=%t incident=%+v err=%v",
			won,
			claimedIncident,
			err,
		)
	}

	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		ReserveCoordinationAttemptInput{
			ID: "planner-interlocked-attempt", IncidentID: incident.ID,
			Board: "default", ExpectedGraphRevision: &revision,
			Since: current.Add(-time.Hour), Current: current.Add(2 * time.Second),
			MaxCalls: 6, TTL: MinCoordinationIncidentClaimTTL,
		},
	)
	if err != nil || reserved.Reserved || reserved.BudgetExhausted {
		t.Fatalf("Coordinator reservation over live Planner = %+v, err=%v", reserved, err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		CoordinationAttemptFilter{IncidentID: incident.ID},
	)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("interlocked reservation charged a call: attempts=%+v err=%v", attempts, err)
	}

	claimedIncident, won, err = opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               current.Add(10 * time.Second),
		},
	)
	if err != nil || !won ||
		claimedIncident.Status != model.CoordinationIncidentCoordinating {
		t.Fatalf(
			"Coordinator did not claim after Planner expiry: won=%t incident=%+v err=%v",
			won,
			claimedIncident,
			err,
		)
	}
}

func TestPlannerLeaseDoesNotBlockOtherCoordinationTriggers(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC)
	task := createPlanningTask(t, opened, "Repeated block remains exceptional")
	incident := createPlanningInterlockIncident(
		t, ctx, opened, task.Task.ID, model.CoordinationTriggerRepeatedBlock,
	)
	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, AutoDecomposeMaxAttempts, time.Minute, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, decision, 1)
	revision := incident.GraphRevision
	_, won, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               current.Add(time.Second),
		},
	)
	if err != nil || !won {
		t.Fatalf("Planner blocked non-graph coordination: won=%t err=%v", won, err)
	}
}

func TestPlannerClaimWaitsForLiveGraphStalledCoordinatorLease(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2041, 3, 4, 5, 6, 7, 0, time.UTC)
	task := createPlanningTask(t, opened, "Coordinator owns this Triage task")
	incident := createPlanningInterlockIncident(
		t, ctx, opened, task.Task.ID, model.CoordinationTriggerGraphStalled,
	)
	revision := incident.GraphRevision
	coordinating, won, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               current,
		},
	)
	if err != nil || !won || coordinating.ClaimExpiresAt == nil {
		t.Fatalf("claim Coordinator: won=%t incident=%+v err=%v", won, coordinating, err)
	}

	blocked, err := opened.ClaimAutoDecompose(
		ctx,
		task.Task.ID,
		AutoDecomposeMaxAttempts,
		time.Minute,
		current.Add(time.Second),
	)
	if err != nil || blocked.Eligibility != AutoDecomposeBusy ||
		blocked.Claim != nil || blocked.RetryAt == nil ||
		*blocked.RetryAt != *coordinating.ClaimExpiresAt {
		t.Fatalf("Planner decision under Coordinator lease = %+v, err=%v", blocked, err)
	}
	state, err := opened.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil || state != nil {
		t.Fatalf("blocked Planner mutated scheduler state: state=%+v err=%v", state, err)
	}

	expiry, err := time.Parse(time.RFC3339Nano, *coordinating.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := opened.ClaimAutoDecompose(
		ctx,
		task.Task.ID,
		AutoDecomposeMaxAttempts,
		time.Minute,
		expiry,
	)
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, recovered, 1)
	reclaimed, reclaimedWon, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               expiry,
		},
	)
	if err != nil || reclaimedWon ||
		reclaimed.ClaimToken != coordinating.ClaimToken {
		t.Fatalf(
			"expired Coordinator reclaimed over new Planner: won=%t incident=%+v err=%v",
			reclaimedWon,
			reclaimed,
			err,
		)
	}
}

func TestEditedTaskVersionCanStartFreshPlanningPastStaleCoordinatorLease(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2041, 3, 5, 6, 7, 8, 0, time.UTC)
	task := createPlanningTask(t, opened, "Original rough goal")
	incident := createPlanningInterlockIncident(
		t, ctx, opened, task.Task.ID, model.CoordinationTriggerGraphStalled,
	)
	revision := incident.GraphRevision
	coordinating, won, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   time.Minute,
			Current:               current,
		},
	)
	if err != nil || !won {
		t.Fatalf("claim stale-version Coordinator: won=%t value=%+v err=%v", won, coordinating, err)
	}
	revised := "Revised rough goal"
	edited, err := opened.UpdateTask(
		ctx,
		task.Task.ID,
		UpdateTaskInput{Title: &revised},
	)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Task.UpdatedAt == task.Task.UpdatedAt {
		t.Fatal("task edit did not create a new optimistic version")
	}
	decision, err := opened.ClaimAutoDecompose(
		ctx,
		task.Task.ID,
		AutoDecomposeMaxAttempts,
		time.Minute,
		current.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	claim := requirePlanningClaim(t, decision, 1)
	if claim.TaskUpdatedAt != edited.Task.UpdatedAt {
		t.Fatalf("fresh Planner claimed version %s, want %s", claim.TaskUpdatedAt, edited.Task.UpdatedAt)
	}
}

type planningInterlockRaceResult struct {
	planningDecision AutoDecomposeDecision
	planningErr      error
	coordinationWon  bool
	coordinationErr  error
}

func runPlanningInterlockRace(
	t *testing.T,
	reserveAttempt bool,
) planningInterlockRaceResult {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	plannerStore := openPlanningTestStore(t, path)
	task := createPlanningTask(t, plannerStore, "Concurrent planning ownership")
	incident := createPlanningInterlockIncident(
		t, ctx, plannerStore, task.Task.ID, model.CoordinationTriggerGraphStalled,
	)
	coordinatorStore := openPlanningTestStore(t, path)
	current := time.Date(2041, 4, 5, 6, 7, 8, 0, time.UTC)
	revision := incident.GraphRevision
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	result := planningInterlockRaceResult{}
	go func() {
		defer wait.Done()
		<-start
		result.planningDecision, result.planningErr = plannerStore.ClaimAutoDecompose(
			ctx,
			task.Task.ID,
			AutoDecomposeMaxAttempts,
			time.Minute,
			current,
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		if reserveAttempt {
			reservation, err := coordinatorStore.ReserveCoordinationAttempt(
				ctx,
				ReserveCoordinationAttemptInput{
					ID:         "concurrent-coordination-attempt",
					IncidentID: incident.ID, Board: "default",
					ExpectedGraphRevision: &revision,
					Since:                 current.Add(-time.Hour), Current: current,
					MaxCalls: 6, TTL: MinCoordinationIncidentClaimTTL,
				},
			)
			result.coordinationWon, result.coordinationErr = reservation.Reserved, err
			return
		}
		_, result.coordinationWon, result.coordinationErr =
			coordinatorStore.ClaimCoordinationIncident(
				ctx,
				incident.ID,
				ClaimCoordinationIncidentInput{
					ExpectedGraphRevision: &revision,
					TTL:                   MinCoordinationIncidentClaimTTL,
					Current:               current,
				},
			)
	}()
	close(start)
	wait.Wait()

	state, err := plannerStore.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedIncident, err := plannerStore.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	plannerLive := state != nil && state.ClaimToken != nil &&
		state.ClaimExpiresAt != nil
	coordinatorLive, err := coordinationIncidentHasLiveClaim(storedIncident, current)
	if err != nil {
		t.Fatal(err)
	}
	if plannerLive == coordinatorLive {
		t.Fatalf(
			"serialized claims live state planner=%t coordinator=%t state=%+v incident=%+v result=%+v",
			plannerLive,
			coordinatorLive,
			state,
			storedIncident,
			result,
		)
	}
	return result
}

func TestPlanningAndIncidentClaimTransactionsHaveOneWinner(t *testing.T) {
	result := runPlanningInterlockRace(t, false)
	if result.planningErr != nil || result.coordinationErr != nil {
		t.Fatalf("concurrent claim errors: %+v", result)
	}
	plannerWon := result.planningDecision.Eligibility == AutoDecomposeClaimed
	plannerBlocked := result.planningDecision.Eligibility == AutoDecomposeBusy
	if plannerWon == result.coordinationWon || !plannerWon && !plannerBlocked {
		t.Fatalf("concurrent incident claim result = %+v", result)
	}
}

func TestPlanningAndAttemptReservationTransactionsHaveOneWinner(t *testing.T) {
	result := runPlanningInterlockRace(t, true)
	if result.planningErr != nil || result.coordinationErr != nil {
		t.Fatalf("concurrent reservation errors: %+v", result)
	}
	plannerWon := result.planningDecision.Eligibility == AutoDecomposeClaimed
	plannerBlocked := result.planningDecision.Eligibility == AutoDecomposeBusy
	if plannerWon == result.coordinationWon || !plannerWon && !plannerBlocked {
		t.Fatalf("concurrent attempt reservation result = %+v", result)
	}
}
