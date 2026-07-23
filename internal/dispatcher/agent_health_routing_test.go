package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestResolveRunProfileUsesSharedGlobalHealthAcrossBoards(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "beta", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	config := coordinatorTestConfig(
		coordinatorWorker("primary", "backup"),
		coordinatorWorker("backup"),
	)
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := coordination.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "primary", Status: model.AgentHealthRateLimited, CooldownUntil: &cooldown,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "backup", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}

	beta, err := manager.OpenStore(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	defer beta.Close()
	// A stale board-local observation must not override the shared state.
	if _, err := beta.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "primary", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "primary"
	task, err := beta.CreateTask(ctx, store.CreateTaskInput{
		Title: "use shared fallback", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveRunProfile(ctx, manager, beta, task.Task, Options{
		AgentConfig: &config, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "backup" || !resolved.GlobalRegistered ||
		resolved.FallbackFrom == nil || *resolved.FallbackFrom != "primary" {
		t.Fatalf("resolved profile ignored shared global health: %#v", resolved)
	}
}

func TestMaintainBoardsClearsGlobalAndLocalCooldowns(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthUnhealthy, CooldownUntil: &expired,
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "board-worker", Status: model.AgentHealthUnhealthy, CooldownUntil: &expired,
	}); err != nil {
		t.Fatal(err)
	}
	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}

	if err := maintainBoards(ctx, manager, []string{"alpha"}, Options{}); err != nil {
		t.Fatal(err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	global, err := coordination.GetAgentHealth(ctx, "global-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
	alpha, err = manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	local, err := alpha.GetAgentHealth(ctx, "board-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}
	if global.Status != model.AgentHealthUnknown || local.Status != model.AgentHealthUnknown {
		t.Fatalf("expired cooldowns were not cleared: global=%#v local=%#v", global, local)
	}
}

func TestRegisteredAgentHasRoleIgnoresBoardOnlyAndWrongRoleProfiles(t *testing.T) {
	config := agentconfig.Config{Agents: []agentconfig.Agent{
		{ID: "worker", Roles: []agentconfig.Role{agentconfig.RoleWorker}},
		{ID: "planner", Roles: []agentconfig.Role{agentconfig.RolePlanner}},
	}}
	if !registeredAgentHasRole(config, "worker", agentconfig.RoleWorker) {
		t.Fatal("registered worker role was not detected")
	}
	if registeredAgentHasRole(config, "planner", agentconfig.RoleWorker) ||
		registeredAgentHasRole(config, "board-only", agentconfig.RoleWorker) {
		t.Fatal("non-worker route was classified as global worker health")
	}
}
