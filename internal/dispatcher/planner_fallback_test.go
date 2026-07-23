package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
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

func TestDispatcherRolePlannerFallsBackAndAuditsActualSelection(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Decompose me", Status: model.TaskStatusTriage})
	if err != nil {
		t.Fatal(err)
	}
	primary := executableFixture(t, "echo 'quota exceeded: rate limit' >&2\nexit 1")
	backup := executableFixture(t, `
case " $* " in *" --model backup-model "*) ;; *) exit 9 ;; esac
case " $* " in *" --provider backup-provider "*) ;; *) exit 9 ;; esac
printf '%s\n' '{"type":"run_result","text":"{\"ok\":true}"}'`)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{PlannerAgents: []string{"primary"}},
		Agents: []agentconfig.Agent{
			{ID: "primary", Runtime: model.RuntimeCline, Command: primary, Model: "primary-model", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}, Fallbacks: []string{"backup"}},
			{ID: "backup", Runtime: model.RuntimeCline, Command: backup, Model: "backup-model", Provider: "backup-provider", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		},
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	options := Options{PlannerTimeout: 5 * time.Second, RateLimitCooldown: durationValue(time.Hour), AgentRetryCooldown: durationValue(time.Hour), Getenv: func(string) string { return "" }}
	var selected orchestration.PlannerSelection
	planner, err := createRolePlannerWithSelection(
		manager, opened, metadata, configuredProfileSet{Config: config}, options,
		agentconfig.RolePlanner, t.TempDir(),
		func(_ context.Context, selection orchestration.PlannerSelection) error {
			selected = selection
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(ctx, orchestration.PlannerRequest{TaskID: task.Task.ID, Kind: orchestration.PlannerDecompose, Prompt: "plan", Schema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := value.(map[string]any); result["ok"] != true {
		t.Fatalf("planner result = %#v", value)
	}
	primaryHealth, err := opened.GetAgentHealth(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if primaryHealth.Status != model.AgentHealthRateLimited || primaryHealth.CooldownUntil == nil {
		t.Fatalf("primary health = %#v", primaryHealth)
	}
	if selected.Candidate.Profile != "backup" || selected.Candidate.Model != "backup-model" ||
		selected.Candidate.Provider != "backup-provider" || selected.FallbackFrom == nil ||
		*selected.FallbackFrom != "primary" {
		t.Fatalf("selection callback = %#v", selected)
	}
	assertPlannerSelectionEvent(t, opened, task.Task.ID, "planner", "backup", "backup-model", "backup-provider", "primary")
}

func TestDispatcherJudgeUsesOrderedGlobalDefaultFallback(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Judge me"})
	if err != nil {
		t.Fatal(err)
	}
	backup := executableFixture(t, `printf '%s\n' '{"type":"run_result","text":"{\"complete\":true}"}'`)
	missing := filepath.Join(t.TempDir(), "missing-judge")
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{JudgeAgents: []string{"judge-primary", "judge-backup"}},
		Agents: []agentconfig.Agent{
			{ID: "judge-primary", Runtime: model.RuntimeCline, Command: missing, Model: "missing-model", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleJudge}},
			{ID: "judge-backup", Runtime: model.RuntimeCline, Command: backup, Model: "judge-model", Provider: "judge-provider", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleJudge}},
		},
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	planner, err := createRolePlanner(manager, opened, metadata, configuredProfileSet{Config: config}, Options{PlannerTimeout: 5 * time.Second, Getenv: func(string) string { return "" }}, agentconfig.RoleJudge, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(ctx, orchestration.PlannerRequest{TaskID: task.Task.ID, Kind: orchestration.PlannerGoalJudge, Prompt: "judge", Schema: map[string]any{"type": "object"}}); err != nil {
		t.Fatal(err)
	}
	health, err := opened.GetAgentHealth(ctx, "judge-primary")
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != model.AgentHealthMissing {
		t.Fatalf("primary judge health = %#v", health)
	}
	assertPlannerSelectionEvent(t, opened, task.Task.ID, "judge", "judge-backup", "judge-model", "judge-provider", "judge-primary")
}

func TestDispatcherCoordinatorRequiresCoordinatorRoleAndHonorsBoardProfile(t *testing.T) {
	profile := "board-coord"
	metadata := boards.Metadata{Orchestration: boards.OrchestrationSettings{
		Autopilot: boards.AutopilotSettings{Coordination: boards.CoordinationSettings{Profile: &profile}},
	}}
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{CoordinatorAgents: []string{"global-coord"}},
		Agents: []agentconfig.Agent{
			{ID: "global-coord", Runtime: model.RuntimeCodex, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleCoordinator}},
			{ID: "board-coord", Runtime: model.RuntimeClaude, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleCoordinator}},
			{ID: "wrong-role", Runtime: model.RuntimeCline, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		},
	}
	candidates := dispatcherPlannerCandidates(metadata, configuredProfileSet{Config: config}, Options{}, agentconfig.RoleCoordinator)
	if len(candidates) != 1 || candidates[0].Profile != profile || candidates[0].Runtime != model.RuntimeClaude {
		t.Fatalf("board coordinator candidates = %#v", candidates)
	}
	metadata.Orchestration.Autopilot.Coordination.Profile = nil
	candidates = dispatcherPlannerCandidates(metadata, configuredProfileSet{Config: config}, Options{}, agentconfig.RoleCoordinator)
	if len(candidates) != 1 || candidates[0].Profile != "global-coord" {
		t.Fatalf("global coordinator candidates = %#v", candidates)
	}
}

func TestDispatcherCoordinatorUsesOneExternalCandidatePerBudgetedAttempt(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	primary := executableFixture(t, "echo 'quota exceeded: rate limit' >&2\nexit 1")
	backup := executableFixture(t, `printf '%s\n' '{"type":"run_result","text":"{\"ok\":true}"}'`)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{CoordinatorAgents: []string{"primary"}},
		Agents: []agentconfig.Agent{
			{
				ID: "primary", Runtime: model.RuntimeCline, Command: primary,
				Enabled: true, MaxConcurrent: 1,
				Roles:     []agentconfig.Role{agentconfig.RoleCoordinator},
				Fallbacks: []string{"backup"},
			},
			{
				ID: "backup", Runtime: model.RuntimeCline, Command: backup,
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
			},
		},
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		PlannerTimeout:     5 * time.Second,
		RateLimitCooldown:  durationValue(time.Hour),
		AgentRetryCooldown: durationValue(time.Hour),
		Getenv:             func(string) string { return "" },
	}
	planner, err := createRolePlanner(
		manager, opened, metadata, configuredProfileSet{Config: config},
		options, agentconfig.RoleCoordinator, t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := orchestration.PlannerRequest{
		Kind: orchestration.PlannerCoordinator, Prompt: "coordinate",
		Schema: map[string]any{"type": "object"},
	}
	if _, err := planner(ctx, request); err == nil {
		t.Fatal("first budgeted attempt unexpectedly invoked the fallback")
	}
	health, err := opened.GetAgentHealth(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != model.AgentHealthRateLimited {
		t.Fatalf("primary health = %#v", health)
	}
	value, err := planner(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result := value.(map[string]any); result["ok"] != true {
		t.Fatalf("fallback result = %#v", value)
	}
}

func TestDispatcherPlannerSharesCapacityWithWorkerAcrossBoards(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alpha.Close()
	assignee := "primary"
	ownerTask, err := alpha.CreateTask(ctx, store.CreateTaskInput{Title: "active primary worker", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := alpha.ClaimTask(ctx, store.ClaimOptions{TaskID: ownerTask.Task.ID})
	if err != nil || owner == nil {
		t.Fatalf("claim primary worker: %+v, %v", owner, err)
	}
	lease, acquired, err := agentcapacity.New(manager).AcquireWorker(ctx, assignee, 1, "alpha", owner.Run.ID)
	if err != nil || !acquired {
		t.Fatalf("acquire primary worker capacity: %+v, acquired=%v, err=%v", lease, acquired, err)
	}

	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "plan through backup", Status: model.TaskStatusTriage})
	if err != nil {
		t.Fatal(err)
	}
	backup := executableFixture(t, `printf '%s\n' '{"type":"run_result","text":"{\"ok\":true}"}'`)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{PlannerAgents: []string{"primary", "backup"}},
		Agents: []agentconfig.Agent{
			{ID: "primary", Runtime: model.RuntimeCline, Command: "/missing-capacity-must-skip", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleWorker, agentconfig.RolePlanner}},
			{ID: "backup", Runtime: model.RuntimeCline, Command: backup, Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		},
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	planner, err := createRolePlanner(manager, opened, metadata, configuredProfileSet{Config: config}, Options{PlannerTimeout: 5 * time.Second, Getenv: func(string) string { return "" }}, agentconfig.RolePlanner, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(ctx, orchestration.PlannerRequest{TaskID: task.Task.ID, Kind: orchestration.PlannerDecompose, Prompt: "plan", Schema: map[string]any{"type": "object"}}); err != nil {
		t.Fatal(err)
	}
	primaryHealth, err := opened.GetAgentHealth(ctx, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if primaryHealth.Status != model.AgentHealthUnknown || primaryHealth.LastError != nil {
		t.Fatalf("capacity skip changed primary health = %#v", primaryHealth)
	}
	assertPlannerSelectionEvent(t, opened, task.Task.ID, "planner", "backup", "", "", "primary")
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backupSlots, listErr := coordination.ListGlobalAgentSlots(ctx, "backup")
	closeErr := coordination.Close()
	if listErr != nil || closeErr != nil || len(backupSlots) != 0 {
		t.Fatalf("backup planner capacity was not released: %#v, %v", backupSlots, errors.Join(listErr, closeErr))
	}
	if _, err := alpha.FailRun(ctx, store.RunScope{RunID: owner.Run.ID, ClaimToken: owner.ClaimToken}, "test complete", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertPlannerSelectionEvent(t *testing.T, opened *store.Store, taskID, role, profile, modelName, provider, fallbackFrom string) {
	t.Helper()
	events, err := opened.ListEvents(context.Background(), store.EventFilter{TaskID: taskID, Kinds: []string{"orchestration_agent_selected"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("selection events = %#v", events)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["role"] != role || payload["profile"] != profile || payload["model"] != modelName || payload["provider"] != provider || payload["fallbackFrom"] != fallbackFrom {
		t.Fatalf("selection payload = %#v", payload)
	}
}
