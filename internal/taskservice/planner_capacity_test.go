package taskservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentcapacity"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func capacityPlannerCommand(t *testing.T, name, result string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sh")
	contents := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"run_result\",\"text\":\"{\\\"winner\\\":\\\"" + result + "\\\"}\"}'\n"
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTaskServiceSkipsCapacityFullPlannerWithoutChangingHealth(t *testing.T) {
	isolateGlobalAgentConfig(t)
	primaryCommand := capacityPlannerCommand(t, "primary", "primary")
	backupCommand := capacityPlannerCommand(t, "backup", "backup")
	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"primary"}
	config.Agents = []agentconfig.Agent{
		{ID: "primary", Runtime: model.RuntimeCline, Command: primaryCommand, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}, Fallbacks: []string{"backup"}},
		{ID: "backup", Runtime: model.RuntimeCline, Command: backupCommand, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
	}
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	seed, acquired, err := agentcapacity.New(manager).AcquireEphemeral(ctx, "primary", 1, store.AgentSlotOwnerPlanner, "default", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("seed primary capacity: %+v, %v, %v", seed, acquired, err)
	}
	defer seed.Release(ctx)
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	planner, err := New(opened, manager, "default").plannerForRole(metadata, agentconfig.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(ctx, orchestration.PlannerRequest{Kind: orchestration.PlannerSpecify, Prompt: "Specify", Schema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["winner"] != "backup" {
		t.Fatalf("capacity fallback result = %#v", value)
	}
	primaryHealth, err := opened.GetAgentHealth(ctx, "primary")
	if err != nil || primaryHealth.Status != model.AgentHealthUnknown {
		t.Fatalf("capacity changed primary health: %+v, %v", primaryHealth, err)
	}
	backupHealth, err := opened.GetAgentHealth(ctx, "backup")
	if err != nil || backupHealth.Status != model.AgentHealthReady {
		t.Fatalf("selected backup health: %+v, %v", backupHealth, err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backupSlots, err := coordination.ListGlobalAgentSlots(ctx, "backup")
	coordination.Close()
	if err != nil || len(backupSlots) != 0 {
		t.Fatalf("taskservice leaked backup slot: %+v, %v", backupSlots, err)
	}
}

func TestTaskServiceJudgeUsesAndReleasesGlobalCapacity(t *testing.T) {
	isolateGlobalAgentConfig(t)
	command := capacityPlannerCommand(t, "judge", "judge")
	config := agentconfig.Default()
	config.Defaults.JudgeAgents = []string{"judge"}
	config.Agents = []agentconfig.Agent{{
		ID: "judge", Runtime: model.RuntimeCline, Command: command, Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleJudge},
	}}
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	planner, err := New(opened, manager, "default").plannerForRole(metadata, agentconfig.RoleJudge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(ctx, orchestration.PlannerRequest{Kind: orchestration.PlannerGoalJudge, Prompt: "Judge", Schema: map[string]any{"type": "object"}}); err != nil {
		t.Fatal(err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := coordination.ListGlobalAgentSlots(ctx, "judge")
	coordination.Close()
	if err != nil || len(slots) != 0 {
		t.Fatalf("taskservice leaked judge slot: %+v, %v", slots, err)
	}
}

func TestTaskServiceRolePlannersUseSharedHealthAcrossBoards(t *testing.T) {
	isolateGlobalAgentConfig(t)
	backupCommand := capacityPlannerCommand(t, "backup", "backup")
	roles := []agentconfig.Role{
		agentconfig.RolePlanner, agentconfig.RoleCoordinator, agentconfig.RoleJudge,
	}
	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"primary"}
	config.Defaults.CoordinatorAgents = []string{"primary"}
	config.Defaults.JudgeAgents = []string{"primary"}
	config.Agents = []agentconfig.Agent{
		{
			ID: "primary", Runtime: model.RuntimeCline, Command: "/must-not-run",
			Enabled: true, MaxConcurrent: 1, Roles: roles, Fallbacks: []string{"backup"},
		},
		{
			ID: "backup", Runtime: model.RuntimeCline, Command: backupCommand,
			Enabled: true, MaxConcurrent: 1, Roles: roles,
		},
	}
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
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
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "primary", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	metadata, err := manager.Read("alpha")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		role agentconfig.Role
		kind orchestration.PlannerKind
	}{
		{name: "planner", role: agentconfig.RolePlanner, kind: orchestration.PlannerSpecify},
		{name: "coordinator", role: agentconfig.RoleCoordinator, kind: orchestration.PlannerCoordinator},
		{name: "judge", role: agentconfig.RoleJudge, kind: orchestration.PlannerGoalJudge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			planner, err := New(opened, manager, "alpha").plannerForRole(metadata, test.role)
			if err != nil {
				t.Fatal(err)
			}
			value, err := planner(ctx, orchestration.PlannerRequest{
				Kind: test.kind, Prompt: "use shared healthy fallback",
				Schema: map[string]any{"type": "object"},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, ok := value.(map[string]any)
			if !ok || result["winner"] != "backup" {
				t.Fatalf("role planner result = %#v", value)
			}
		})
	}
}
