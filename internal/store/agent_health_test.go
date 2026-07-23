package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestAgentHealthUpsertAndUnknownState(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	unknown, err := store.GetAgentHealth(ctx, " new-agent ")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.AgentID != "new-agent" || unknown.Status != model.AgentHealthUnknown || unknown.UpdatedAt != "" {
		t.Fatalf("unexpected synthesized health: %+v", unknown)
	}

	ready, err := store.SetAgentHealth(ctx, SetAgentHealthInput{AgentID: " codex-primary ", Status: model.AgentHealthReady})
	if err != nil {
		t.Fatal(err)
	}
	if ready.AgentID != "codex-primary" || ready.Status != model.AgentHealthReady || ready.UpdatedAt == "" {
		t.Fatalf("unexpected ready health: %+v", ready)
	}

	cooldown := " 2030-01-02T03:04:05+09:00 "
	longError := strings.Repeat("가", 2000)
	limited, err := store.SetAgentHealth(ctx, SetAgentHealthInput{
		AgentID: "codex-primary", Status: model.AgentHealthRateLimited,
		CooldownUntil: &cooldown, LastError: &longError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if limited.Status != model.AgentHealthRateLimited || limited.CooldownUntil == nil || *limited.CooldownUntil != "2030-01-01T18:04:05.000Z" {
		t.Fatalf("cooldown was not normalized: %+v", limited)
	}
	if limited.LastError == nil || len(*limited.LastError) > maxAgentHealthErrorBytes {
		t.Fatalf("error was not bounded: %d bytes", len(*limited.LastError))
	}

	listed, err := store.ListAgentHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Status != model.AgentHealthRateLimited {
		t.Fatalf("upsert created an unexpected row: %+v", listed)
	}
}

func TestAgentHealthValidationAndRunForeignKey(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	badCooldown := "tomorrow"
	tests := []SetAgentHealthInput{
		{Status: model.AgentHealthReady},
		{AgentID: "worker", Status: "offline"},
		{AgentID: "worker", Status: model.AgentHealthRateLimited, CooldownUntil: &badCooldown},
	}
	for _, input := range tests {
		if _, err := store.SetAgentHealth(ctx, input); err == nil {
			t.Fatalf("invalid input was accepted: %+v", input)
		}
	}

	missingRun := "run_missing"
	if _, err := store.SetAgentHealth(ctx, SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthUnhealthy, LastRunID: &missingRun,
	}); err == nil {
		t.Fatal("missing last run foreign key was accepted")
	}

	task, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Health probe", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: %+v, %v", claim, err)
	}
	withRun, err := store.SetAgentHealth(ctx, SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthUnhealthy, LastRunID: &claim.Run.ID,
	})
	if err != nil || withRun.LastRunID == nil || *withRun.LastRunID != claim.Run.ID {
		t.Fatalf("last run was not recorded: %+v, %v", withRun, err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", task.Task.ID); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := store.GetAgentHealth(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.LastRunID != nil {
		t.Fatalf("deleted run reference was retained: %+v", afterDelete)
	}
}

func TestClearExpiredAgentCooldowns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	expired := current.Add(-time.Second).Format(time.RFC3339Nano)
	future := current.Add(time.Hour).Format(time.RFC3339Nano)
	for _, input := range []SetAgentHealthInput{
		{AgentID: "expired", Status: model.AgentHealthRateLimited, CooldownUntil: &expired},
		{AgentID: "future", Status: model.AgentHealthRateLimited, CooldownUntil: &future},
		{AgentID: "unhealthy", Status: model.AgentHealthUnhealthy, CooldownUntil: &expired},
		{AgentID: "auth", Status: model.AgentHealthAuthRequired, CooldownUntil: &future},
		{AgentID: "missing", Status: model.AgentHealthMissing},
	} {
		if _, err := store.SetAgentHealth(ctx, input); err != nil {
			t.Fatal(err)
		}
	}

	if unavailable, _ := store.GetAgentHealth(ctx, "expired"); IsAgentUnavailable(unavailable, current) {
		t.Fatal("expired cooldown should be eligible before cleanup")
	}
	if unavailable, _ := store.GetAgentHealth(ctx, "future"); !IsAgentUnavailable(unavailable, current) {
		t.Fatal("future cooldown should prevent dispatch")
	}
	count, err := store.ClearExpiredAgentCooldowns(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("cleared %d cooldowns, want 2", count)
	}
	cleared, err := store.GetAgentHealth(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Status != model.AgentHealthUnknown || cleared.CooldownUntil != nil || cleared.UpdatedAt != "2030-01-02T03:04:05.000Z" {
		t.Fatalf("expired cooldown was not cleared: %+v", cleared)
	}
	unhealthy, err := store.GetAgentHealth(ctx, "unhealthy")
	if err != nil {
		t.Fatal(err)
	}
	if unhealthy.Status != model.AgentHealthUnknown || unhealthy.CooldownUntil != nil {
		t.Fatalf("expired unhealthy cooldown was not cleared: %+v", unhealthy)
	}
	auth, _ := store.GetAgentHealth(ctx, "auth")
	missing, _ := store.GetAgentHealth(ctx, "missing")
	if !IsAgentUnavailable(auth, current) || !IsAgentUnavailable(missing, current) {
		t.Fatalf("active availability failures became eligible: auth=%+v missing=%+v", auth, missing)
	}
}
