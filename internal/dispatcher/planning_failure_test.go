package dispatcher

import (
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestAutoDecomposePlannerSetupFailureConsumesClaimedAttempt(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:  "rough goal with invalid Planner setup",
		Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	logs := make([]string, 0)
	options := Options{
		AutoDecompose:        boolValue(true),
		AutoDecomposePerTick: 1,
		PlannerRuntime:       model.RuntimeManual,
		Getenv:               func(string) string { return "" },
		OnLog: func(message string) {
			logs = append(logs, message)
		},
	}
	decomposeBoardTriage(
		ctx,
		manager,
		[]string{"default"},
		options,
		&autoDecomposeDiagnostics{},
	)

	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	state, err := check.GetAutoDecomposeState(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Attempts != 1 ||
		state.ClaimToken != nil || state.ClaimExpiresAt != nil ||
		state.NextAttemptAt == nil || state.LastError == nil ||
		!strings.Contains(*state.LastError, "invalid planner runtime") {
		t.Fatalf("Planner setup failure scheduler state = %+v", state)
	}
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	failedEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "auto_decompose_failed" {
			failedEvents++
		}
	}
	if failedEvents != 1 || detail.Task.Status != model.TaskStatusTriage {
		t.Fatalf(
			"Planner setup failure audit events=%d task=%+v",
			failedEvents,
			detail.Task,
		)
	}
	foundFailureLog := false
	for _, message := range logs {
		if strings.Contains(message, "configure Planner") {
			foundFailureLog = true
			break
		}
	}
	if !foundFailureLog {
		t.Fatalf("Planner setup failure was not logged: %v", logs)
	}
}

func TestAutoDecomposeDoesNotConfigurePlannerWithoutClaimableTask(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	key := "github-issue:owner/repository:99"
	imported, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "imported issue requires review", Status: model.TaskStatusTriage,
		IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	logs := make([]string, 0)
	decomposeBoardTriage(
		ctx,
		manager,
		[]string{"default"},
		Options{
			AutoDecompose: boolValue(true), PlannerRuntime: model.RuntimeManual,
			Getenv: func(string) string { return "" },
			OnLog: func(message string) {
				logs = append(logs, message)
			},
		},
		&autoDecomposeDiagnostics{},
	)
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	state, err := check.GetAutoDecomposeState(ctx, imported.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("imported task received a planning attempt: %+v", state)
	}
	for _, message := range logs {
		if strings.Contains(message, "configure Planner") ||
			strings.Contains(message, "invalid planner runtime") {
			t.Fatalf("Planner was configured before a successful claim: %v", logs)
		}
	}
}
