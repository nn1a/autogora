package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func observationState(
	taskUpdatedAt string,
	attempts int,
	nextAttemptAt *string,
	claimToken *string,
	claimExpiresAt *string,
) *AutoDecomposeState {
	return &AutoDecomposeState{
		TaskID: "task", TaskUpdatedAt: taskUpdatedAt,
		Attempts: attempts, MaxAttempts: 3,
		NextAttemptAt: nextAttemptAt, ClaimToken: claimToken,
		ClaimExpiresAt: claimExpiresAt,
	}
}

func TestClassifyAutoDecomposeObservationOwnership(t *testing.T) {
	current := time.Date(2040, 7, 8, 9, 10, 11, 0, time.UTC)
	currentAt := current.Format(time.RFC3339Nano)
	futureAt := current.Add(time.Minute).Format(time.RFC3339Nano)
	token := "plan-token"
	cases := []struct {
		name         string
		taskVersion  string
		state        *AutoDecomposeState
		wantKind     AutoDecomposeObservationKind
		plannerOwned bool
		wantError    string
	}{
		{
			name: "no state", taskVersion: "v1",
			wantKind: AutoDecomposeObservationNoState, plannerOwned: true,
		},
		{
			name: "stale version ignores stale lease data", taskVersion: "v2",
			state:    observationState("v1", 3, nil, &token, stringValue("invalid")),
			wantKind: AutoDecomposeObservationStaleVersion, plannerOwned: true,
		},
		{
			name: "backoff", taskVersion: "v1",
			state:    observationState("v1", 1, &futureAt, nil, nil),
			wantKind: AutoDecomposeObservationBackoff, plannerOwned: true,
		},
		{
			name: "retry due", taskVersion: "v1",
			state:    observationState("v1", 1, &currentAt, nil, nil),
			wantKind: AutoDecomposeObservationDue, plannerOwned: true,
		},
		{
			name: "expired retryable claim", taskVersion: "v1",
			state:    observationState("v1", 1, nil, &token, &currentAt),
			wantKind: AutoDecomposeObservationExpiredRetryable, plannerOwned: true,
		},
		{
			name: "live final attempt", taskVersion: "v1",
			state:    observationState("v1", 3, nil, &token, &futureAt),
			wantKind: AutoDecomposeObservationLiveClaim, plannerOwned: true,
		},
		{
			name: "exhausted without lease", taskVersion: "v1",
			state:    observationState("v1", 3, nil, nil, nil),
			wantKind: AutoDecomposeObservationExhausted,
		},
		{
			name: "expired final attempt", taskVersion: "v1",
			state:    observationState("v1", 3, nil, &token, &currentAt),
			wantKind: AutoDecomposeObservationExhausted,
		},
		{
			name: "invalid matching claim expiry", taskVersion: "v1",
			state:     observationState("v1", 1, nil, &token, stringValue("invalid")),
			wantError: "parse auto-decompose claim expiry",
		},
		{
			name: "invalid matching retry time", taskVersion: "v1",
			state:     observationState("v1", 1, stringValue("invalid"), nil, nil),
			wantError: "parse auto-decompose retry time",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			observation, err := classifyAutoDecomposeObservation(
				"task", test.taskVersion, test.state, current, 3,
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("classification error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if observation.Kind != test.wantKind ||
				observation.PlannerOwned() != test.plannerOwned {
				t.Fatalf(
					"classification = %+v, plannerOwned=%t; want kind=%s plannerOwned=%t",
					observation,
					observation.PlannerOwned(),
					test.wantKind,
					test.plannerOwned,
				)
			}
		})
	}
}

func TestListAutoDecomposeObservationsUsesBoardAndTaskVersion(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2040, 8, 9, 10, 11, 12, 0, time.UTC)

	task := createPlanningTask(t, opened, "Planner-owned rough goal")
	other, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "another board goal", Board: "other", Status: task.Task.Status,
	})
	if err != nil {
		t.Fatal(err)
	}
	observations, err := opened.ListAutoDecomposeObservations(
		ctx, current, AutoDecomposeMaxAttempts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 ||
		observations[task.Task.ID].Kind != AutoDecomposeObservationNoState {
		t.Fatalf("board-scoped no-state observations = %+v", observations)
	}
	if _, found := observations[other.Task.ID]; found {
		t.Fatalf("observation leaked from another board: %+v", observations)
	}

	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, AutoDecomposeMaxAttempts, 5*time.Minute, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstClaim := requirePlanningClaim(t, decision, 1)
	revised := "Revised rough goal"
	if _, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{
		Title: &revised,
	}); err != nil {
		t.Fatal(err)
	}
	observations, err = opened.ListAutoDecomposeObservations(
		ctx, current.Add(time.Second), AutoDecomposeMaxAttempts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observations[task.Task.ID].Kind != AutoDecomposeObservationStaleVersion {
		t.Fatalf("edited task observation = %+v", observations[task.Task.ID])
	}

	reset, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, AutoDecomposeMaxAttempts, 5*time.Minute, current.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	resetClaim := requirePlanningClaim(t, reset, 1)
	if resetClaim.TaskUpdatedAt == firstClaim.TaskUpdatedAt {
		t.Fatal("edited task did not reset its planning version")
	}
	observations, err = opened.ListAutoDecomposeObservations(
		ctx, current.Add(3*time.Second), AutoDecomposeMaxAttempts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observations[task.Task.ID].Kind != AutoDecomposeObservationLiveClaim ||
		observations[task.Task.ID].State == nil ||
		observations[task.Task.ID].State.Attempts != 1 {
		t.Fatalf("reset task observation = %+v", observations[task.Task.ID])
	}
}

func TestListAutoDecomposeObservationsRejectsInvalidCurrentVersionExpiry(t *testing.T) {
	ctx := context.Background()
	opened := openPlanningTestStore(t, ":memory:")
	current := time.Date(2040, 9, 10, 11, 12, 13, 0, time.UTC)
	task := createPlanningTask(t, opened, "Corrupt Planner expiry")
	decision, err := opened.ClaimAutoDecompose(
		ctx, task.Task.ID, AutoDecomposeMaxAttempts, time.Minute, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	requirePlanningClaim(t, decision, 1)
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE auto_decompose_state
		SET claim_expires_at = 'invalid'
		WHERE task_id = ?
	`, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ListAutoDecomposeObservations(
		ctx, current, AutoDecomposeMaxAttempts,
	); err == nil || !strings.Contains(err.Error(), "parse auto-decompose claim expiry") {
		t.Fatalf("invalid expiry observation error = %v", err)
	}
}
