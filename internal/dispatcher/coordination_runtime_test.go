package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/coordinator"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type coordinationRuntimeFixture struct {
	manager  *boards.Manager
	dbPath   string
	options  Options
	task     model.Task
	incident model.CoordinationIncident
	current  time.Time
}

func setCoordinationTestMode(
	t *testing.T,
	manager *boards.Manager,
	board string,
	mode boards.CoordinationMode,
) {
	t.Helper()
	enabled := true
	if _, err := manager.Update(board, boards.Update{Orchestration: &boards.OrchestrationUpdate{
		Autopilot: &boards.AutopilotUpdate{
			Enabled: &enabled,
			Coordination: &boards.CoordinationUpdate{
				Mode: &mode,
			},
		},
	}}); err != nil {
		t.Fatal(err)
	}
}

func setCoordinationTestEnabled(
	t *testing.T,
	manager *boards.Manager,
	board string,
	enabled bool,
) {
	t.Helper()
	if _, err := manager.Update(board, boards.Update{Orchestration: &boards.OrchestrationUpdate{
		Autopilot: &boards.AutopilotUpdate{Enabled: &enabled},
	}}); err != nil {
		t.Fatal(err)
	}
}

func coordinatorRuntimeConfig() agentconfig.Config {
	return agentconfig.Normalize(agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 2},
		Defaults: agentconfig.Defaults{
			WorkerAgents:      []string{"worker"},
			CoordinatorAgents: []string{"coordinator"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: "worker", Runtime: model.RuntimeCodex, Command: "/bin/true",
				Enabled: true, MaxConcurrent: 2,
				Roles: []agentconfig.Role{agentconfig.RoleWorker},
			},
			{
				ID: "coordinator", Runtime: model.RuntimeCodex, Command: "/bin/true",
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
			},
		},
	})
}

func seedCoordinationRuntimeFixture(
	t *testing.T,
	mode boards.CoordinationMode,
) coordinationRuntimeFixture {
	t.Helper()
	ctx := context.Background()
	manager, dbPath := testManager(t)
	setCoordinationTestMode(t, manager, "default", mode)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	config := coordinatorRuntimeConfig()
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "recover repeated block", Assignee: &assignee,
		Runtime: model.RuntimeCodex, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := store.BlockInput{
		Kind: model.BlockKindCapability, Reason: "required compiler is unavailable",
	}
	if _, err := opened.BlockTask(ctx, task.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.BlockTask(ctx, task.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	task, err = opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC()
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	incidents, err := reconcileCoordinatorIncidents(ctx, manager, opened, metadata, Options{
		AgentConfig: &config, Getenv: func(string) string { return "" },
	}, current)
	if err != nil {
		t.Fatal(err)
	}
	incident := findCoordinatorIncident(incidents, model.CoordinationTriggerRepeatedBlock)
	if incident == nil {
		t.Fatalf("repeated block incident was not detected: %+v", incidents)
	}
	return coordinationRuntimeFixture{
		manager: manager, dbPath: dbPath, task: task.Task, incident: *incident, current: current,
		options: Options{
			Autopilot: true, PlannerTimeout: time.Minute,
			AgentConfig: &config, Getenv: func(string) string { return "" },
			Now: func() time.Time { return current.Add(time.Second) },
		},
	}
}

func unblockCoordinationPlanner(
	fixture coordinationRuntimeFixture,
	calls *atomic.Int32,
) orchestration.Planner {
	return func(context.Context, orchestration.PlannerRequest) (any, error) {
		calls.Add(1)
		return coordinator.Proposal{
			IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
			Summary:   "Retry the blocked task",
			Rationale: "The assigned worker is healthy and the preserved workspace is clean.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionUnblockTask, TaskID: fixture.task.ID,
				ExpectedUpdatedAt: fixture.task.UpdatedAt,
				Reason:            "Retry the capability block with the healthy assigned worker.",
			}},
		}, nil
	}
}

func priorityCoordinationPlanner(
	fixture coordinationRuntimeFixture,
	calls *atomic.Int32,
) orchestration.Planner {
	return func(context.Context, orchestration.PlannerRequest) (any, error) {
		calls.Add(1)
		priority := 8
		return coordinator.Proposal{
			IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
			Summary:   "Raise the recovery task priority",
			Rationale: "The task is blocking the graph and deterministic retries were exhausted.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
				ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
				Reason: "Let the existing worker retry this critical path first.",
			}},
		}, nil
	}
}

func clineCoordinationPlannerCommand(
	t *testing.T,
	marker string,
	proposal coordinator.Proposal,
) string {
	t.Helper()
	proposalJSON, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	eventJSON, err := json.Marshal(map[string]any{
		"type": "run_result",
		"text": string(proposalJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	return executableFixture(
		t,
		"touch '"+marker+"'\nprintf '%s\\n' '"+string(eventJSON)+"'",
	)
}

func TestCoordinationRuntimeObserveDoesNotCallAgent(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeObserve)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Observe invoked Coordinator %d times", calls.Load())
	}
	opened, _ := fixture.manager.OpenStore(context.Background(), "default")
	defer opened.Close()
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(), store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("Observe attempts = %+v, %v", attempts, err)
	}
}

func TestCoordinateIncidentRechecksObserveBoundaryBeforeAnalysis(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	setCoordinationTestMode(t, fixture.manager, "default", boards.CoordinationModeObserve)

	err = coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		fixture.current,
	)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.GetTask(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(), store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(attempts) != 0 || task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentOpen {
		t.Fatalf(
			"stale candidate crossed Observe boundary: calls=%d attempts=%+v task=%+v incident=%+v",
			calls.Load(), attempts, task.Task, incident,
		)
	}
}

func TestCoordinateIncidentRetiresReservationWhenPolicyChangesDuringPreparation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *boards.Manager)
	}{
		{
			name: "observe",
			change: func(t *testing.T, manager *boards.Manager) {
				setCoordinationTestMode(t, manager, "default", boards.CoordinationModeObserve)
			},
		},
		{
			name: "disabled",
			change: func(t *testing.T, manager *boards.Manager) {
				setCoordinationTestEnabled(t, manager, "default", false)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
			var calls atomic.Int32
			fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
			var clocks atomic.Int32
			fixture.options.Now = func() time.Time {
				if clocks.Add(1) == 2 {
					test.change(t, fixture.manager)
				}
				return fixture.current.Add(time.Duration(clocks.Load()) * time.Second)
			}
			if err := runCoordinationPass(
				context.Background(), fixture.manager, []string{"default"},
				fixture.options, &coordinationRuntimeState{}, fixture.current,
			); err != nil {
				t.Fatal(err)
			}
			opened, err := fixture.manager.OpenStore(context.Background(), "default")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			incident, err := opened.GetCoordinationIncident(
				context.Background(),
				fixture.incident.ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			attempts, err := opened.ListCoordinationAttempts(
				context.Background(),
				store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
			)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 || len(attempts) != 0 ||
				incident.Status != model.CoordinationIncidentOpen ||
				incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
				t.Fatalf(
					"policy boundary leaked reservation: calls=%d attempts=%+v incident=%+v",
					calls.Load(),
					attempts,
					incident,
				)
			}
		})
	}
}

func TestCoordinateIncidentRebuildsChangedCoordinatorProfileBeforePaidCall(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	priority := 8
	proposal := coordinator.Proposal{
		IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
		Summary:   "Raise the recovery task priority",
		Rationale: "Use the newly selected Coordinator profile.",
		Actions: []coordinator.Action{{
			Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
			ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
			Reason: "Prioritize the blocked graph.",
		}},
	}
	markerRoot := t.TempDir()
	oldMarker, newMarker := markerRoot+"/old-called", markerRoot+"/new-called"
	oldCommand := clineCoordinationPlannerCommand(t, oldMarker, proposal)
	newCommand := clineCoordinationPlannerCommand(t, newMarker, proposal)
	config := coordinatorRuntimeConfig()
	for index := range config.Agents {
		if config.Agents[index].ID == "coordinator" {
			config.Agents[index].Runtime = model.RuntimeCline
			config.Agents[index].Command = oldCommand
		}
	}
	config.Agents = append(config.Agents, agentconfig.Agent{
		ID: "next-coordinator", Runtime: model.RuntimeCline, Command: newCommand,
		Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
	})
	config = agentconfig.Normalize(config)
	fixture.options.AgentConfig = &config
	fixture.options.CoordinatorPlanner = nil
	var clocks atomic.Int32
	fixture.options.Now = func() time.Time {
		if clocks.Add(1) == 2 {
			profile := "next-coordinator"
			if _, updateErr := fixture.manager.Update(
				"default",
				boards.Update{Orchestration: &boards.OrchestrationUpdate{
					Autopilot: &boards.AutopilotUpdate{
						Coordination: &boards.CoordinationUpdate{
							Profile: store.OptionalString{Set: true, Value: &profile},
						},
					},
				}},
			); updateErr != nil {
				t.Fatal(updateErr)
			}
		}
		return fixture.current.Add(time.Duration(clocks.Load()) * time.Second)
	}
	candidate := coordinationCandidate{
		board: "default", metadata: metadata, incident: fixture.incident,
		mode: boards.CoordinationModeAssist,
	}
	if err := coordinateIncident(
		context.Background(),
		fixture.manager,
		candidate,
		fixture.options,
		fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale Coordinator command ran before policy recheck: %v", err)
	}
	if _, err := os.Stat(newMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new Coordinator command ran during stale preparation: %v", err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		opened.Close()
		t.Fatalf("stale profile retained attempt reservation: %+v", attempts)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	if err := coordinateIncident(
		context.Background(),
		fixture.manager,
		candidate,
		fixture.options,
		fixture.current.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale Coordinator command ran on rebuilt pass: %v", err)
	}
	if _, err := os.Stat(newMarker); err != nil {
		t.Fatalf("rebuilt Coordinator command did not run: %v", err)
	}
	opened, err = fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	proposals, err := opened.ListCoordinationProposals(
		context.Background(),
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err = opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 ||
		proposals[0].CoordinatorAgent != "next-coordinator" ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded ||
		attempts[0].SelectedAgent != "next-coordinator" {
		t.Fatalf("rebuilt Coordinator selection: proposals=%+v attempts=%+v", proposals, attempts)
	}
}

func TestCoordinateIncidentDetectsLiveGlobalConfigChangeBeforePaidCall(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	priority := 8
	proposal := coordinator.Proposal{
		IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
		Summary:   "Raise the recovery task priority",
		Rationale: "Use the live global Coordinator profile.",
		Actions: []coordinator.Action{{
			Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
			ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
			Reason: "Prioritize the blocked graph.",
		}},
	}
	markerRoot := t.TempDir()
	oldMarker, newMarker := markerRoot+"/old-live-called", markerRoot+"/new-live-called"
	oldCommand := clineCoordinationPlannerCommand(t, oldMarker, proposal)
	newCommand := clineCoordinationPlannerCommand(t, newMarker, proposal)
	config := coordinatorRuntimeConfig()
	for index := range config.Agents {
		if config.Agents[index].ID == "coordinator" {
			config.Agents[index].Runtime = model.RuntimeCline
			config.Agents[index].Command = oldCommand
		}
	}
	config.Agents = append(config.Agents, agentconfig.Agent{
		ID: "next-live-coordinator", Runtime: model.RuntimeCline, Command: newCommand,
		Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
	})
	config = agentconfig.Normalize(config)
	configPath := markerRoot + "/config.json"
	configOptions := agentconfig.Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return configPath
		}
		return ""
	}}
	if err := agentconfig.Save(configOptions, config); err != nil {
		t.Fatal(err)
	}
	fixedSnapshot := config
	fixture.options.AgentConfig = &fixedSnapshot
	fixture.options.AgentConfigLoader = func() (agentconfig.Config, error) {
		return agentconfig.Load(configOptions)
	}
	fixture.options.CoordinatorPlanner = nil
	var clocks atomic.Int32
	fixture.options.Now = func() time.Time {
		if clocks.Add(1) == 3 {
			config.Defaults.CoordinatorAgents = []string{"next-live-coordinator"}
			if saveErr := agentconfig.Save(configOptions, config); saveErr != nil {
				t.Fatal(saveErr)
			}
		}
		return fixture.current.Add(time.Duration(clocks.Load()) * time.Second)
	}
	err = coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		fixture.current,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale global Coordinator command ran: %v", err)
	}
	if _, err := os.Stat(newMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new global Coordinator command ran before planner rebuild: %v", err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		incident.ClaimExpiresAt != nil {
		t.Fatalf(
			"live config change retained stale reservation: incident=%+v attempts=%+v",
			incident,
			attempts,
		)
	}
}

func TestCoordinateIncidentDetectsGlobalConfigChangeBetweenSnapshotAndPlanner(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	priority := 8
	proposal := coordinator.Proposal{
		IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
		Summary:   "Raise the recovery task priority",
		Rationale: "Use the live global Coordinator profile.",
		Actions: []coordinator.Action{{
			Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
			ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
			Reason: "Prioritize the blocked graph.",
		}},
	}
	markerRoot := t.TempDir()
	oldMarker, newMarker := markerRoot+"/old-snapshot-called", markerRoot+"/new-planner-called"
	oldCommand := clineCoordinationPlannerCommand(t, oldMarker, proposal)
	newCommand := clineCoordinationPlannerCommand(t, newMarker, proposal)
	config := coordinatorRuntimeConfig()
	for index := range config.Agents {
		if config.Agents[index].ID == "coordinator" {
			config.Agents[index].Runtime = model.RuntimeCline
			config.Agents[index].Command = oldCommand
		}
	}
	config.Agents = append(config.Agents, agentconfig.Agent{
		ID: "next-gap-coordinator", Runtime: model.RuntimeCline, Command: newCommand,
		Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
	})
	config = agentconfig.Normalize(config)
	configPath := markerRoot + "/config.json"
	configOptions := agentconfig.Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return configPath
		}
		return ""
	}}
	if err := agentconfig.Save(configOptions, config); err != nil {
		t.Fatal(err)
	}
	fixedSnapshot := config
	fixture.options.AgentConfig = &fixedSnapshot
	var loads atomic.Int32
	fixture.options.AgentConfigLoader = func() (agentconfig.Config, error) {
		loaded, loadErr := agentconfig.Load(configOptions)
		if loadErr != nil {
			return agentconfig.Config{}, loadErr
		}
		if loads.Add(1) == 1 {
			config.Defaults.CoordinatorAgents = []string{"next-gap-coordinator"}
			if saveErr := agentconfig.Save(configOptions, config); saveErr != nil {
				return agentconfig.Config{}, saveErr
			}
		}
		return loaded, nil
	}
	fixture.options.CoordinatorPlanner = nil
	var clocks atomic.Int32
	fixture.options.Now = func() time.Time {
		return fixture.current.Add(time.Duration(clocks.Add(1)) * time.Second)
	}
	err = coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		fixture.current,
	)
	if err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 {
		t.Fatalf("live agent configuration loads = %d, want shared snapshot and boundary", loads.Load())
	}
	if _, err := os.Stat(oldMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot Coordinator command ran: %v", err)
	}
	if _, err := os.Stat(newMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planner Coordinator command ran with a stale agent snapshot: %v", err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		incident.ClaimExpiresAt != nil {
		t.Fatalf(
			"snapshot config change retained stale reservation: incident=%+v attempts=%+v",
			incident,
			attempts,
		)
	}
}

func TestCoordinateIncidentOwnsCachedLoaderConfigBeforeNormalization(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	cached := coordinatorRuntimeConfig()
	workerIndex := -1
	for index := range cached.Agents {
		if cached.Agents[index].ID == "worker" {
			workerIndex = index
			cached.Agents[index].Roles = []agentconfig.Role{
				agentconfig.RoleWorker,
				agentconfig.RolePlanner,
			}
			break
		}
	}
	if workerIndex < 0 {
		t.Fatal("worker agent is missing")
	}
	cached = agentconfig.Normalize(cached)
	var loads atomic.Int32
	fixture.options.AgentConfigLoader = func() (agentconfig.Config, error) {
		if loads.Add(1) == 2 {
			// Mutate the shared backing array while keeping worker profiles,
			// commands, and Coordinator candidates otherwise identical.
			cached.Agents[workerIndex].Roles[1] = agentconfig.RoleJudge
		}
		return cached, nil
	}
	var paidCalls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		context.Context,
		orchestration.PlannerRequest,
	) (any, error) {
		paidCalls.Add(1)
		return nil, errors.New("stale cached config crossed the paid boundary")
	}
	var clocks atomic.Int32
	fixture.options.Now = func() time.Time {
		return fixture.current.Add(time.Duration(clocks.Add(1)) * time.Second)
	}
	err = coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		fixture.current,
	)
	if err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 || paidCalls.Load() != 0 {
		t.Fatalf(
			"cached loader boundary: loads=%d paidCalls=%d",
			loads.Load(),
			paidCalls.Load(),
		)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		len(attempts) != 0 {
		t.Fatalf(
			"cached loader change retained stale reservation: incident=%+v attempts=%+v",
			incident,
			attempts,
		)
	}
}

func TestCoordinateIncidentRechecksChangedCallBudgetBeforePaidCall(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	prior, _, err := opened.StartCoordinationAttempt(
		context.Background(),
		store.StartCoordinationAttemptInput{
			ID: "prior-budgeted-attempt", IncidentID: fixture.incident.ID,
			Board: "default",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	priorError := "earlier Coordinator call failed"
	if _, err := opened.FinishCoordinationAttempt(
		context.Background(),
		prior.ID,
		store.FinishCoordinationAttemptInput{
			Board: "default", Status: model.CoordinationAttemptFailed,
			Error: &priorError,
		},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	var clocks atomic.Int32
	fixture.options.Now = func() time.Time {
		if clocks.Add(1) == 2 {
			maxCalls := 1
			if _, updateErr := fixture.manager.Update(
				"default",
				boards.Update{Orchestration: &boards.OrchestrationUpdate{
					Autopilot: &boards.AutopilotUpdate{
						Coordination: &boards.CoordinationUpdate{
							MaxCallsPerHour: &maxCalls,
						},
					},
				}},
			); updateErr != nil {
				t.Fatal(updateErr)
			}
		}
		return fixture.current.Add(time.Duration(clocks.Load()) * time.Second)
	}
	candidate := coordinationCandidate{
		board: "default", metadata: metadata, incident: fixture.incident,
		mode: boards.CoordinationModeAssist,
	}
	if err := coordinateIncident(
		context.Background(),
		fixture.manager,
		candidate,
		fixture.options,
		fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if err := coordinateIncident(
		context.Background(),
		fixture.manager,
		candidate,
		fixture.options,
		fixture.current.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	opened, err = fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 ||
		len(attempts) != 1 ||
		attempts[0].ID != prior.ID ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		incident.ClaimExpiresAt != nil {
		t.Fatalf(
			"changed call budget leaked paid analysis: calls=%d attempts=%+v incident=%+v",
			calls.Load(),
			attempts,
			incident,
		)
	}
}

func TestCoordinationRuntimeAssistRequestsApproval(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Assist calls = %d, want 1", calls.Load())
	}
	opened, _ := fixture.manager.OpenStore(context.Background(), "default")
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval {
		t.Fatalf("Assist result: task=%+v incident=%+v proposals=%+v", task.Task, incident, proposals)
	}
}

func TestCoordinationRuntimeConcurrentPassesShareOneAnalysisAndApplication(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		entered <- struct{}{}
		<-release
		return base(ctx, request)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runCoordinationPass(
			context.Background(), fixture.manager, []string{"default"},
			fixture.options, &coordinationRuntimeState{}, fixture.current,
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first Coordinator analysis did not start")
	}
	secondErr := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	)
	close(release)
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first Coordinator pass did not finish")
	}

	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(), store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.GetTask(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalApplied ||
		incident.Status != model.CoordinationIncidentResolved ||
		task.Task.Priority != 8 {
		t.Fatalf(
			"concurrent passes duplicated recovery: calls=%d attempts=%+v proposals=%+v incident=%+v task=%+v",
			calls.Load(), attempts, proposals, incident, task.Task,
		)
	}
}

func TestCoordinationRuntimeRenewsLeaseAcrossLongPreparation(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	fixture.options.PlannerTimeout = 2 * time.Second
	baseTime := fixture.current
	renewAt := baseTime.Add(16 * time.Second)
	var clockCalls atomic.Int32
	fixture.options.Now = func() time.Time {
		switch clockCalls.Add(1) {
		case 1:
			return baseTime
		case 2:
			return baseTime.Add(15 * time.Second)
		default:
			return renewAt
		}
	}
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		opened, err := fixture.manager.OpenStore(ctx, "default")
		if err != nil {
			return nil, err
		}
		incident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
		closeErr := opened.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		expiry, err := time.Parse(time.RFC3339Nano, *incident.ClaimExpiresAt)
		if err != nil {
			return nil, err
		}
		minimum := renewAt.Add(fixture.options.PlannerTimeout + coordinationClaimGrace)
		if expiry.Before(minimum) {
			return nil, errors.New("renewed claim does not cover timeout plus grace")
		}
		entered <- struct{}{}
		<-release
		return base(ctx, request)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- runCoordinationPass(
			context.Background(), fixture.manager, []string{"default"},
			fixture.options, &coordinationRuntimeState{}, baseTime,
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Coordinator analysis did not start")
	}

	// The original reservation expired at baseTime+17s. The pre-analysis
	// renewal keeps it live through baseTime+33s, so this pass at +18s cannot
	// reclaim the incident and start a duplicate paid call.
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, baseTime.Add(18*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Coordinator analysis did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("long preparation made %d paid calls, want 1", calls.Load())
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded {
		t.Fatalf("long-preparation attempts = %+v", attempts)
	}
}

func TestCoordinationRuntimeRechecksLeaseAfterSlowLiveConfigPreflight(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	baseTime := fixture.current
	firstOptions := fixture.options
	firstOptions.PlannerTimeout = time.Second
	config := agentconfig.Normalize(*fixture.options.AgentConfig)
	var loaderCalls atomic.Int32
	preflightEntered := make(chan struct{})
	releasePreflight := make(chan struct{})
	firstOptions.AgentConfigLoader = func() (agentconfig.Config, error) {
		if loaderCalls.Add(1) == 2 {
			close(preflightEntered)
			<-releasePreflight
		}
		return config, nil
	}
	var firstClock atomic.Int32
	reclaimAt := baseTime.Add(coordinationClaimTTL(firstOptions) + time.Second)
	firstOptions.Now = func() time.Time {
		switch firstClock.Add(1) {
		case 1, 2, 3:
			return baseTime
		default:
			return reclaimAt.Add(time.Second)
		}
	}
	var firstPaidCalls atomic.Int32
	firstOptions.CoordinatorPlanner = func(
		context.Context,
		orchestration.PlannerRequest,
	) (any, error) {
		firstPaidCalls.Add(1)
		return nil, errors.New("stale owner crossed the paid boundary")
	}
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinateIncident(
			context.Background(),
			fixture.manager,
			coordinationCandidate{
				board: "default", metadata: metadata, incident: fixture.incident,
				mode: boards.CoordinationModeAssist,
			},
			firstOptions,
			baseTime,
		)
	}()
	select {
	case <-preflightEntered:
	case <-time.After(time.Second):
		t.Fatal("first Coordinator did not enter the slow live-config preflight")
	}

	secondOptions := fixture.options
	secondOptions.PlannerTimeout = time.Second
	secondOptions.Now = func() time.Time { return reclaimAt }
	var secondPaidCalls atomic.Int32
	secondOptions.CoordinatorPlanner = priorityCoordinationPlanner(
		fixture,
		&secondPaidCalls,
	)
	if err := coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		secondOptions,
		reclaimAt,
	); err != nil {
		close(releasePreflight)
		t.Fatalf("reclaiming Coordinator failed: %v", err)
	}
	close(releasePreflight)
	select {
	case firstErr := <-firstDone:
		if firstErr == nil {
			t.Fatal("stale Coordinator unexpectedly retained the reclaimed lease")
		}
	case <-time.After(time.Second):
		t.Fatal("stale Coordinator did not stop after the preflight was released")
	}
	if firstPaidCalls.Load() != 0 || secondPaidCalls.Load() != 1 {
		t.Fatalf(
			"slow preflight paid calls: stale=%d reclaimer=%d",
			firstPaidCalls.Load(),
			secondPaidCalls.Load(),
		)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		context.Background(),
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(attempts) != 2 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded ||
		attempts[1].Status != model.CoordinationAttemptFailed ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval {
		t.Fatalf(
			"slow preflight recovery: incident=%+v attempts=%+v proposals=%+v",
			incident,
			attempts,
			proposals,
		)
	}
}

func TestCoordinationRuntimeAutoAppliesOnlyConditionalActions(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, _ := fixture.manager.OpenStore(context.Background(), "default")
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	attempts, _ := opened.ListCoordinationAttempts(
		context.Background(), store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Priority != 8 ||
		incident.Status != model.CoordinationIncidentResolved ||
		len(proposals) != 1 || proposals[0].Status != model.CoordinationProposalApplied ||
		len(attempts) != 1 || attempts[0].Status != model.CoordinationAttemptSucceeded {
		t.Fatalf(
			"Auto result: calls=%d task=%+v incident=%+v proposals=%+v attempts=%+v",
			calls.Load(), task.Task, incident, proposals, attempts,
		)
	}
}

func TestCoordinationRuntimePolicyChangeDowngradesAutoToApproval(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		result, err := base(ctx, request)
		setCoordinationTestMode(t, fixture.manager, "default", boards.CoordinationModeAssist)
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, _ := fixture.manager.OpenStore(context.Background(), "default")
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	if task.Task.Priority != 1 || incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("policy downgrade auto-applied: task=%+v incident=%+v", task.Task, incident)
	}
}

func TestCoordinationRuntimeRevalidatesTaskAfterPaidAnalysis(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		result, err := base(ctx, request)
		opened, openErr := fixture.manager.OpenStore(ctx, "default")
		if openErr != nil {
			return nil, openErr
		}
		defer opened.Close()
		title := "changed while Coordinator was analyzing"
		if _, updateErr := opened.UpdateTask(
			ctx, fixture.task.ID, store.UpdateTaskInput{Title: &title},
		); updateErr != nil {
			return nil, updateErr
		}
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Title != "changed while Coordinator was analyzing" ||
		task.Task.Priority != 1 || incident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 || proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"latest task was not protected: calls=%d task=%+v incident=%+v proposals=%+v",
			calls.Load(), task.Task, incident, proposals,
		)
	}
}

func TestCoordinationRuntimeLatestHealthDowngradesAutoToApproval(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	base := unblockCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		result, err := base(ctx, request)
		opened, openErr := fixture.manager.OpenStore(ctx, "default")
		if openErr != nil {
			return nil, openErr
		}
		defer opened.Close()
		if _, healthErr := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
			AgentID: "worker", Status: model.AgentHealthUnhealthy,
		}); healthErr != nil {
			return nil, healthErr
		}
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Status != fixture.task.Status ||
		incident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval {
		t.Fatalf(
			"latest health did not downgrade auto apply: calls=%d task=%+v incident=%+v proposals=%+v",
			calls.Load(), task.Task, incident, proposals,
		)
	}
}

func TestCoordinationRuntimeLatestActionLimitSupersedesProposal(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		_ context.Context,
		_ orchestration.PlannerRequest,
	) (any, error) {
		calls.Add(1)
		priority := 8
		result := coordinator.Proposal{
			IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
			Summary:   "Update the recovery route and priority",
			Rationale: "Both changes were valid against the analysis snapshot.",
			Actions: []coordinator.Action{
				{
					Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
					ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
					Reason: "Prioritize recovery.",
				},
				{
					Kind: coordinator.ActionSetRoute, TaskID: fixture.task.ID,
					ExpectedUpdatedAt: fixture.task.UpdatedAt,
					Assignee:          "worker", Runtime: model.RuntimeCodex,
					Reason: "Keep the healthy worker route.",
				},
			},
		}
		maxActions := 1
		_, err := fixture.manager.Update("default", boards.Update{
			Orchestration: &boards.OrchestrationUpdate{
				Autopilot: &boards.AutopilotUpdate{
					Coordination: &boards.CoordinationUpdate{
						MaxActionsPerIncident: &maxActions,
					},
				},
			},
		})
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 || proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"latest action limit was ignored: calls=%d task=%+v incident=%+v proposals=%+v",
			calls.Load(), task.Task, incident, proposals,
		)
	}
}

func TestCoordinationRuntimeObserveChangeRetiresAnalyzedProposal(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		result, err := base(ctx, request)
		setCoordinationTestMode(t, fixture.manager, "default", boards.CoordinationModeObserve)
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(),
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"Observe policy change retained analysis: calls=%d task=%+v incident=%+v proposals=%+v",
			calls.Load(), task.Task, incident, proposals,
		)
	}
}

func TestCoordinationRuntimeDisabledChangeNeverAutoApplies(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	var calls atomic.Int32
	base := priorityCoordinationPlanner(fixture, &calls)
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		result, err := base(ctx, request)
		setCoordinationTestEnabled(t, fixture.manager, "default", false)
		return result, err
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		context.Background(), store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"disabled policy change left an active proposal: calls=%d task=%+v incident=%+v proposals=%+v",
			calls.Load(), task.Task, incident, proposals,
		)
	}
}

func TestCoordinationRuntimeLimitsPaidAnalysisToOneBoardPerPass(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	ctx := context.Background()
	if _, err := fixture.manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	setCoordinationTestMode(t, fixture.manager, "alpha", boards.CoordinationModeAssist)
	alphaStore, err := fixture.manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alphaStore.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	alphaTask, err := alphaStore.CreateTask(ctx, store.CreateTaskInput{
		Title: "alpha repeated block", Assignee: &assignee,
		Runtime: model.RuntimeCodex, Priority: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := store.BlockInput{Kind: model.BlockKindCapability, Reason: "alpha compiler missing"}
	if _, err := alphaStore.BlockTask(ctx, alphaTask.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	if _, err := alphaStore.UnblockTask(ctx, alphaTask.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := alphaStore.BlockTask(ctx, alphaTask.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	alphaTask, _ = alphaStore.GetTask(ctx, alphaTask.Task.ID)
	alphaMetadata, _ := fixture.manager.Read("alpha")
	alphaIncidents, err := reconcileCoordinatorIncidents(
		ctx, fixture.manager, alphaStore, alphaMetadata, fixture.options, fixture.current,
	)
	if err != nil {
		t.Fatal(err)
	}
	alphaIncident := findCoordinatorIncident(alphaIncidents, model.CoordinationTriggerRepeatedBlock)
	if alphaIncident == nil {
		t.Fatal("alpha incident was not detected")
	}
	if err := alphaStore.Close(); err != nil {
		t.Fatal(err)
	}
	type proposalTarget struct {
		incident model.CoordinationIncident
		task     model.Task
	}
	targets := map[string]proposalTarget{
		fixture.task.ID:   {incident: fixture.incident, task: fixture.task},
		alphaTask.Task.ID: {incident: *alphaIncident, task: alphaTask.Task},
	}
	calls := map[string]int{}
	fixture.options.CoordinatorPlanner = func(
		_ context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		target := targets[request.TaskID]
		calls[request.TaskID]++
		priority := 7
		return coordinator.Proposal{
			IncidentID:            target.incident.ID,
			ExpectedGraphRevision: target.incident.GraphRevision,
			Summary:               "Raise recovery priority", Rationale: "Recover one board fairly.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionUpdatePriority, TaskID: target.task.ID,
				ExpectedUpdatedAt: target.task.UpdatedAt, Priority: &priority,
				Reason: "Prioritize the blocked graph.",
			}},
		}, nil
	}
	state := &coordinationRuntimeState{}
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default", "alpha"},
		fixture.options, state, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if calls[fixture.task.ID]+calls[alphaTask.Task.ID] != 1 {
		t.Fatalf("first pass calls = %#v", calls)
	}
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default", "alpha"},
		fixture.options, state, fixture.current.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if calls[fixture.task.ID] != 1 || calls[alphaTask.Task.ID] != 1 {
		t.Fatalf("board fairness calls = %#v", calls)
	}
}

func TestCoordinationRuntimeInvalidBoardsDoNotStarveHealthyCandidate(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	plannerErr := errors.New("temporary Coordinator failure")
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		context.Context,
		orchestration.PlannerRequest,
	) (any, error) {
		if calls.Add(1) == 1 {
			return nil, plannerErr
		}
		priority := 8
		return coordinator.Proposal{
			IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
			Summary:   "Raise the recovery task priority",
			Rationale: "The healthy board remains actionable after another board fails observation.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
				ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
				Reason: "Let the available worker retry the critical path.",
			}},
		}, nil
	}
	state := &coordinationRuntimeState{}
	boardSlugs := []string{"INVALID!", "missing-second", "default"}

	firstErr := runCoordinationPass(
		context.Background(), fixture.manager, boardSlugs,
		fixture.options, state, fixture.current,
	)
	if !errors.Is(firstErr, plannerErr) {
		t.Fatalf("first pass error = %v, want joined planner error", firstErr)
	}
	if !strings.Contains(firstErr.Error(), `board "INVALID!" metadata`) ||
		!strings.Contains(firstErr.Error(), `board "missing-second" store`) {
		t.Fatalf("first pass did not aggregate missing boards: %v", firstErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("first pass Coordinator calls = %d, want 1", calls.Load())
	}

	secondErr := runCoordinationPass(
		context.Background(), fixture.manager, boardSlugs,
		fixture.options, state,
		fixture.current.Add(coordinationRetryBackoffBase+2*time.Second),
	)
	if secondErr == nil ||
		!strings.Contains(secondErr.Error(), `board "INVALID!" metadata`) ||
		!strings.Contains(secondErr.Error(), `board "missing-second" store`) {
		t.Fatalf("second pass did not retain aggregate board errors: %v", secondErr)
	}
	if errors.Is(secondErr, plannerErr) {
		t.Fatalf("second pass unexpectedly retained the prior planner error: %v", secondErr)
	}
	if calls.Load() != 2 {
		t.Fatalf("healthy board Coordinator calls = %d, want one in each pass", calls.Load())
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(), fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("healthy board incident = %+v", incident)
	}
}

func TestCoordinationRuntimeAdvancesCursorPastFailedBoardWithoutCandidate(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeObserve)
	state := &coordinationRuntimeState{}
	boardSlugs := []string{"missing", "default"}

	if err := runCoordinationPass(
		context.Background(), fixture.manager, boardSlugs,
		fixture.options, state, fixture.current,
	); err == nil || !strings.Contains(err.Error(), `board "missing" store`) {
		t.Fatalf("first observation error = %v", err)
	}
	if state.nextBoard != "default" {
		t.Fatalf("next board after failed first board = %q, want default", state.nextBoard)
	}
	if err := runCoordinationPass(
		context.Background(), fixture.manager, boardSlugs,
		fixture.options, state, fixture.current.Add(time.Second),
	); err == nil || !strings.Contains(err.Error(), `board "missing" store`) {
		t.Fatalf("second observation error = %v", err)
	}
	if state.nextBoard != "missing" {
		t.Fatalf("next board after healthy first board = %q, want missing", state.nextBoard)
	}
}

func TestCoordinationRuntimeReconcileErrorDoesNotStarveHealthyCandidate(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	ctx := context.Background()
	if _, err := fixture.manager.Create(ctx, "broken", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	setCoordinationTestMode(t, fixture.manager, "broken", boards.CoordinationModeObserve)
	brokenStore, err := fixture.manager.OpenStore(ctx, "broken")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	brokenTask, err := brokenStore.CreateTask(ctx, store.CreateTaskInput{
		Title: "broken board repeated block", Assignee: &assignee,
		Runtime: model.RuntimeCodex, Priority: 1,
	})
	if err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	block := store.BlockInput{
		Kind: model.BlockKindCapability, Reason: "broken board compiler is unavailable",
	}
	if _, err := brokenStore.BlockTask(ctx, brokenTask.Task.ID, block); err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	if _, err := brokenStore.UnblockTask(ctx, brokenTask.Task.ID); err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	if _, err := brokenStore.BlockTask(ctx, brokenTask.Task.ID, block); err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	metadata, err := fixture.manager.Read("broken")
	if err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	incidents, err := reconcileCoordinatorIncidents(
		ctx, fixture.manager, brokenStore, metadata, fixture.options, fixture.current,
	)
	if err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	incident := findCoordinatorIncident(incidents, model.CoordinationTriggerRepeatedBlock)
	if incident == nil {
		brokenStore.Close()
		t.Fatalf("broken board incident was not detected: %+v", incidents)
	}
	if _, err := brokenStore.TransitionCoordinationIncident(
		ctx, incident.ID, store.TransitionCoordinationIncidentInput{
			ExpectedStatus: model.CoordinationIncidentOpen,
			Status:         model.CoordinationIncidentDismissed,
		},
	); err != nil {
		brokenStore.Close()
		t.Fatal(err)
	}
	if err := brokenStore.Close(); err != nil {
		t.Fatal(err)
	}
	brokenPath, err := fixture.manager.DBPath("broken")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", brokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		ctx,
		"UPDATE coordination_incidents SET details_json = ? WHERE id = ?",
		"{", incident.ID,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	passErr := runCoordinationPass(
		ctx, fixture.manager, []string{"broken", "default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current.Add(time.Second),
	)
	if passErr == nil ||
		!strings.Contains(passErr.Error(), `board "broken" reconciliation`) ||
		!strings.Contains(passErr.Error(), "decode coordinator incident details") {
		t.Fatalf("reconciliation error was not preserved: %v", passErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("healthy board Coordinator calls = %d, want 1", calls.Load())
	}
}

func TestCoordinationRuntimeCancellationStopsBeforeHealthyCandidate(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runCoordinationPass(
		ctx, fixture.manager, []string{"missing", "default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pass error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled pass Coordinator calls = %d, want 0", calls.Load())
	}
}

func TestCoordinationRuntimeFailureReopensWithBackoff(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		context.Context,
		orchestration.PlannerRequest,
	) (any, error) {
		calls.Add(1)
		return nil, errors.New("Coordinator runtime unavailable")
	}
	run := func() error {
		return runCoordinationPass(
			context.Background(), fixture.manager, []string{"default"},
			fixture.options, &coordinationRuntimeState{}, fixture.current,
		)
	}
	if err := run(); err == nil {
		t.Fatal("failed Coordinator call returned nil")
	}
	if err := run(); err != nil {
		t.Fatalf("backoff observation failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("backoff made %d calls, want 1", calls.Load())
	}
	opened, _ := fixture.manager.OpenStore(context.Background(), "default")
	defer opened.Close()
	incident, _ := opened.GetCoordinationIncident(context.Background(), fixture.incident.ID)
	attempts, _ := opened.ListCoordinationAttempts(
		context.Background(), store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if incident.Status != model.CoordinationIncidentOpen ||
		len(attempts) != 1 || attempts[0].Status != model.CoordinationAttemptFailed {
		t.Fatalf("failure recovery: incident=%+v attempts=%+v", incident, attempts)
	}
}

func TestCoordinateIncidentPreparationFailurePersistsBackoff(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		context.Background(),
		"UPDATE coordination_incidents SET details_json = ? WHERE id = ?",
		"{",
		fixture.incident.ID,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	preparationErr := coordinateIncident(
		context.Background(),
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		fixture.current,
	)
	if preparationErr == nil ||
		!strings.Contains(preparationErr.Error(), "decode coordinator incident details") {
		t.Fatalf("snapshot preparation error = %v", preparationErr)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		incident.ClaimExpiresAt != nil ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptFailed ||
		attempts[0].EndedAt == nil ||
		attempts[0].Error == nil ||
		!strings.Contains(*attempts[0].Error, "decode coordinator incident details") {
		t.Fatalf(
			"preparation failure durability: calls=%d incident=%+v attempts=%+v",
			calls.Load(),
			incident,
			attempts,
		)
	}
	endedAt, err := time.Parse(time.RFC3339Nano, *attempts[0].EndedAt)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := activeCoordinationCandidates(
		context.Background(),
		opened,
		metadata,
		endedAt.Add(coordinationRetryBackoffBase-time.Nanosecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("preparation failure immediately reclaimed the incident: %+v", candidates)
	}
	candidates, err = activeCoordinationCandidates(
		context.Background(),
		opened,
		metadata,
		endedAt.Add(coordinationRetryBackoffBase),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != fixture.incident.ID {
		t.Fatalf("preparation failure did not become retryable after backoff: %+v", candidates)
	}
}

func TestCoordinateIncidentReusablePreparationFailureClosesBoundAttempt(t *testing.T) {
	for _, proposalStatus := range []model.CoordinationProposalStatus{
		model.CoordinationProposalValidating,
		model.CoordinationProposalValidated,
	} {
		for _, attemptStatus := range []model.CoordinationAttemptStatus{
			model.CoordinationAttemptStarted,
			model.CoordinationAttemptSucceeded,
			model.CoordinationAttemptFailed,
		} {
			name := string(proposalStatus) + "_" + string(attemptStatus)
			t.Run(name, func(t *testing.T) {
				testCoordinateIncidentReusablePreparationFailureClosesBoundAttempt(
					t,
					proposalStatus,
					attemptStatus,
				)
			})
		}
	}
}

func finishReusablePreparationAttempt(
	t *testing.T,
	ctx context.Context,
	opened *store.Store,
	attemptID string,
	status model.CoordinationAttemptStatus,
) {
	t.Helper()
	if status == model.CoordinationAttemptStarted {
		return
	}
	input := store.FinishCoordinationAttemptInput{
		Board:         "default",
		Status:        status,
		SelectedAgent: "reusable-coordinator",
	}
	if status == model.CoordinationAttemptFailed {
		message := "original terminal failure"
		input.Error = &message
	}
	if _, err := opened.FinishCoordinationAttempt(ctx, attemptID, input); err != nil {
		t.Fatalf("finish reusable attempt as %s: %v", status, err)
	}
}

func testCoordinateIncidentReusablePreparationFailureClosesBoundAttempt(
	t *testing.T,
	proposalStatus model.CoordinationProposalStatus,
	attemptStatus model.CoordinationAttemptStatus,
) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	ctx := context.Background()
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	reservedAt := fixture.current
	revision := fixture.incident.GraphRevision
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		store.ReserveCoordinationAttemptInput{
			ID:         "reusable-preparation-attempt",
			IncidentID: fixture.incident.ID, Board: "default",
			ExpectedGraphRevision: &revision,
			Since:                 reservedAt.Add(-time.Hour),
			Current:               reservedAt,
			MaxCalls:              4,
			TTL:                   coordinationClaimTTL(fixture.options),
		},
	)
	if err != nil || !reserved.Reserved {
		opened.Close()
		t.Fatalf("reserve reusable attempt: %+v, %v", reserved, err)
	}
	attemptID := reserved.Attempt.ID
	proposal, created, err := opened.CreateCoordinationProposal(
		ctx,
		store.CreateCoordinationProposalInput{
			IncidentID: reserved.Incident.ID, AttemptID: &attemptID,
			CoordinatorAgent:      "reusable-coordinator",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               reservedAt.Add(time.Second),
			Summary:               "Durable reusable proposal",
			Rationale:             "Exercise snapshot preparation recovery.",
			Actions:               json.RawMessage(`[]`),
		},
	)
	if err != nil || !created {
		opened.Close()
		t.Fatalf("create reusable proposal: created=%t value=%+v error=%v", created, proposal, err)
	}
	if proposalStatus == model.CoordinationProposalValidated {
		proposal, err = opened.TransitionCoordinationProposal(
			ctx,
			proposal.ID,
			store.TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalValidating,
				Status:                model.CoordinationProposalValidated,
				ExpectedGraphRevision: &revision,
				ClaimToken:            reserved.Incident.ClaimToken,
				Current:               reservedAt.Add(2 * time.Second),
			},
		)
		if err != nil {
			opened.Close()
			t.Fatalf("validate reusable proposal: %v", err)
		}
	}
	finishReusablePreparationAttempt(
		t,
		ctx,
		opened,
		attemptID,
		attemptStatus,
	)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		ctx,
		"UPDATE coordination_incidents SET details_json = ? WHERE id = ?",
		"{",
		fixture.incident.ID,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reclaimAt := reservedAt.Add(coordinationClaimTTL(fixture.options) + time.Second)
	fixture.options.Now = func() time.Time { return reclaimAt }
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	preparationErr := coordinateIncident(
		ctx,
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		reclaimAt,
	)
	if preparationErr == nil ||
		!strings.Contains(preparationErr.Error(), "decode coordinator incident details") {
		t.Fatalf("reusable snapshot preparation error = %v", preparationErr)
	}
	opened, err = fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("reusable attempt count = %d, want 1: %+v", len(attempts), attempts)
	}
	expectedAttemptError := "decode coordinator incident details"
	if attemptStatus == model.CoordinationAttemptFailed {
		expectedAttemptError = "original terminal failure"
	}
	expectedAttemptStatus := attemptStatus
	if attemptStatus == model.CoordinationAttemptStarted {
		expectedAttemptStatus = model.CoordinationAttemptFailed
	}
	attemptErrorMatches := attempts[0].Error != nil &&
		strings.Contains(*attempts[0].Error, expectedAttemptError)
	if expectedAttemptStatus == model.CoordinationAttemptSucceeded {
		attemptErrorMatches = attempts[0].Error == nil
	}
	if calls.Load() != 0 ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		incident.ClaimExpiresAt != nil ||
		len(proposals) != 1 ||
		proposals[0].ID != proposal.ID ||
		proposals[0].Status != model.CoordinationProposalSuperseded ||
		attempts[0].ID != attemptID ||
		attempts[0].Status != expectedAttemptStatus ||
		attempts[0].EndedAt == nil ||
		!attemptErrorMatches {
		t.Fatalf(
			"reusable preparation recovery: calls=%d incident=%+v proposals=%+v attempts=%+v",
			calls.Load(),
			incident,
			proposals,
			attempts,
		)
	}
	endedAt, err := time.Parse(time.RFC3339Nano, *attempts[0].EndedAt)
	if err != nil {
		t.Fatal(err)
	}
	if attemptStatus == model.CoordinationAttemptSucceeded {
		candidates, err := activeCoordinationCandidates(
			ctx,
			opened,
			metadata,
			endedAt.Add(time.Nanosecond),
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].ID != incident.ID {
			t.Fatalf(
				"succeeded paid attempt unexpectedly imposed failure backoff: %+v",
				candidates,
			)
		}
		secondAt := reclaimAt.Add(time.Second)
		fixture.options.Now = func() time.Time { return secondAt }
		secondErr := coordinateIncident(
			ctx,
			fixture.manager,
			coordinationCandidate{
				board: "default", metadata: metadata, incident: incident,
				mode: boards.CoordinationModeAssist,
			},
			fixture.options,
			secondAt,
		)
		if secondErr == nil ||
			!strings.Contains(secondErr.Error(), "decode coordinator incident details") {
			t.Fatalf("second snapshot preparation error = %v", secondErr)
		}
		attempts, err = opened.ListCoordinationAttempts(
			ctx,
			store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 2 ||
			attempts[0].Status != model.CoordinationAttemptFailed ||
			attempts[1].Status != model.CoordinationAttemptSucceeded {
			t.Fatalf(
				"post-success preparation retry did not establish backoff: %+v",
				attempts,
			)
		}
		endedAt, err = time.Parse(time.RFC3339Nano, *attempts[0].EndedAt)
		if err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := activeCoordinationCandidates(
		ctx,
		opened,
		metadata,
		endedAt.Add(coordinationRetryBackoffBase-time.Nanosecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("reusable preparation failure skipped backoff: %+v", candidates)
	}
}

func TestCoordinateIncidentReusableValidProposalPreservesTerminalFailedAttempt(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	ctx := context.Background()
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	reservedAt := fixture.current
	revision := fixture.incident.GraphRevision
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		store.ReserveCoordinationAttemptInput{
			ID:         "reusable-valid-terminal-failed",
			IncidentID: fixture.incident.ID, Board: "default",
			ExpectedGraphRevision: &revision,
			Since:                 reservedAt.Add(-time.Hour),
			Current:               reservedAt,
			MaxCalls:              4,
			TTL:                   coordinationClaimTTL(fixture.options),
		},
	)
	if err != nil || !reserved.Reserved {
		opened.Close()
		t.Fatalf("reserve reusable attempt: %+v, %v", reserved, err)
	}
	priority := 8
	actions, err := json.Marshal([]coordinator.Action{{
		Kind: coordinator.ActionUpdatePriority, TaskID: fixture.task.ID,
		ExpectedUpdatedAt: fixture.task.UpdatedAt, Priority: &priority,
		Reason: "Prioritize the blocked graph.",
	}})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	attemptID := reserved.Attempt.ID
	proposal, created, err := opened.CreateCoordinationProposal(
		ctx,
		store.CreateCoordinationProposalInput{
			IncidentID: reserved.Incident.ID, AttemptID: &attemptID,
			CoordinatorAgent:      "reusable-coordinator",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               reservedAt.Add(time.Second),
			Summary:               "Durable valid reusable proposal",
			Rationale:             "Reuse the paid analysis without rewriting its audit.",
			Actions:               actions,
		},
	)
	if err != nil || !created {
		opened.Close()
		t.Fatalf("create reusable proposal: created=%t value=%+v error=%v", created, proposal, err)
	}
	finishReusablePreparationAttempt(
		t,
		ctx,
		opened,
		attemptID,
		model.CoordinationAttemptFailed,
	)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reclaimAt := reservedAt.Add(coordinationClaimTTL(fixture.options) + time.Second)
	fixture.options.Now = func() time.Time { return reclaimAt }
	var paidCalls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(
		fixture,
		&paidCalls,
	)
	if err := coordinateIncident(
		ctx,
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		reclaimAt,
	); err != nil {
		t.Fatal(err)
	}
	opened, err = fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if paidCalls.Load() != 0 ||
		incident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptFailed ||
		attempts[0].Error == nil ||
		*attempts[0].Error != "original terminal failure" {
		t.Fatalf(
			"valid reusable terminal audit: paid=%d incident=%+v proposals=%+v attempts=%+v",
			paidCalls.Load(),
			incident,
			proposals,
			attempts,
		)
	}
}

func TestCoordinateIncidentReusableDecodeFailurePreservesTerminalSucceededAttempt(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	ctx := context.Background()
	metadata, err := fixture.manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	reservedAt := fixture.current
	revision := fixture.incident.GraphRevision
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		store.ReserveCoordinationAttemptInput{
			ID:         "reusable-decode-terminal-succeeded",
			IncidentID: fixture.incident.ID, Board: "default",
			ExpectedGraphRevision: &revision,
			Since:                 reservedAt.Add(-time.Hour),
			Current:               reservedAt,
			MaxCalls:              4,
			TTL:                   coordinationClaimTTL(fixture.options),
		},
	)
	if err != nil || !reserved.Reserved {
		opened.Close()
		t.Fatalf("reserve reusable attempt: %+v, %v", reserved, err)
	}
	attemptID := reserved.Attempt.ID
	proposal, created, err := opened.CreateCoordinationProposal(
		ctx,
		store.CreateCoordinationProposalInput{
			IncidentID: reserved.Incident.ID, AttemptID: &attemptID,
			CoordinatorAgent:      "reusable-coordinator",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               reservedAt.Add(time.Second),
			Summary:               "Durable reusable proposal",
			Rationale:             "Preserve its completed paid-attempt audit.",
			Actions:               json.RawMessage(`[]`),
		},
	)
	if err != nil || !created {
		opened.Close()
		t.Fatalf("create reusable proposal: created=%t value=%+v error=%v", created, proposal, err)
	}
	proposal, err = opened.TransitionCoordinationProposal(
		ctx,
		proposal.ID,
		store.TransitionCoordinationProposalInput{
			ExpectedStatus:        model.CoordinationProposalValidating,
			Status:                model.CoordinationProposalValidated,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               reservedAt.Add(2 * time.Second),
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	finishReusablePreparationAttempt(
		t,
		ctx,
		opened,
		attemptID,
		model.CoordinationAttemptSucceeded,
	)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		ctx,
		"UPDATE coordination_proposals SET actions_json = '{' WHERE id = ?",
		proposal.ID,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reclaimAt := reservedAt.Add(coordinationClaimTTL(fixture.options) + time.Second)
	fixture.options.Now = func() time.Time { return reclaimAt }
	var paidCalls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(
		fixture,
		&paidCalls,
	)
	reuseErr := coordinateIncident(
		ctx,
		fixture.manager,
		coordinationCandidate{
			board: "default", metadata: metadata, incident: fixture.incident,
			mode: boards.CoordinationModeAssist,
		},
		fixture.options,
		reclaimAt,
	)
	if reuseErr == nil ||
		!strings.Contains(reuseErr.Error(), "decode coordination proposal") {
		t.Fatalf("reusable decode error = %v", reuseErr)
	}
	opened, err = fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if paidCalls.Load() != 0 ||
		incident.Status != model.CoordinationIncidentOpen ||
		incident.ClaimToken != "" ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalSuperseded ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded ||
		attempts[0].Error != nil {
		t.Fatalf(
			"decode cleanup terminal audit: paid=%d incident=%+v proposals=%+v attempts=%+v",
			paidCalls.Load(),
			incident,
			proposals,
			attempts,
		)
	}
}

func TestCoordinationRuntimeRecoversStartedAttemptFromReusableProposal(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	current := fixture.current
	fixture.options.Now = func() time.Time { return current.Add(time.Second) }
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)

	raw, err := sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TRIGGER fail_coordination_attempt_success
		BEFORE UPDATE OF status ON coordination_attempts
		WHEN OLD.status = 'started' AND NEW.status = 'succeeded'
		BEGIN
			SELECT RAISE(FAIL, 'injected coordination attempt finish failure');
		END
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	firstErr := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, current,
	)
	if firstErr == nil ||
		!strings.Contains(firstErr.Error(), "injected coordination attempt finish failure") {
		t.Fatalf("injected attempt finish error = %v", firstErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("initial Coordinator calls = %d, want 1", calls.Load())
	}

	ctx := context.Background()
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil || len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalValidated ||
		proposals[0].AttemptID == nil {
		opened.Close()
		t.Fatalf("durable reusable proposal = %+v, %v", proposals, err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil || len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptStarted {
		opened.Close()
		t.Fatalf("abandoned attempt = %+v, %v", attempts, err)
	}
	abandonedAttemptID := attempts[0].ID
	if *proposals[0].AttemptID != abandonedAttemptID {
		opened.Close()
		t.Fatalf(
			"proposal attempt binding = %q, want %q",
			*proposals[0].AttemptID,
			abandonedAttemptID,
		)
	}
	claimedIncident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	proposalRevision := proposals[0].ExpectedGraphRevision
	incidentRevision := claimedIncident.GraphRevision
	duplicateAttemptID := abandonedAttemptID
	if _, _, err := opened.CreateCoordinationProposal(
		ctx,
		store.CreateCoordinationProposalInput{
			IncidentID: fixture.incident.ID, AttemptID: &duplicateAttemptID,
			CoordinatorAgent:      "duplicate-binding",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &incidentRevision,
			ClaimToken:            claimedIncident.ClaimToken,
			Current:               current.Add(time.Second),
			Summary:               "duplicate attempt binding",
			Rationale:             "One paid attempt must bind at most one proposal.",
		},
	); err == nil {
		opened.Close()
		t.Fatal("second proposal reused the bound attempt")
	}
	_, recovered, recoveryErr := opened.RecoverCoordinationAttemptForProposal(
		ctx,
		store.RecoverCoordinationAttemptInput{
			Board: "default", ProposalID: proposals[0].ID,
			ExpectedProposalStatus:        model.CoordinationProposalValidated,
			ExpectedProposalGraphRevision: &proposalRevision,
			ExpectedIncidentGraphRevision: &incidentRevision,
			ClaimToken:                    "not-the-live-owner",
			Current:                       current.Add(time.Second),
			Status:                        model.CoordinationAttemptSucceeded,
		},
	)
	if recovered || !errors.Is(recoveryErr, store.ErrCoordinationClaimNotOwner) {
		opened.Close()
		t.Fatalf("wrong-token recovery = recovered %t, error %v", recovered, recoveryErr)
	}
	wrongRevision := incidentRevision + 1
	_, recovered, recoveryErr = opened.RecoverCoordinationAttemptForProposal(
		ctx,
		store.RecoverCoordinationAttemptInput{
			Board: "default", ProposalID: proposals[0].ID,
			ExpectedProposalStatus:        model.CoordinationProposalValidated,
			ExpectedProposalGraphRevision: &proposalRevision,
			ExpectedIncidentGraphRevision: &wrongRevision,
			ClaimToken:                    claimedIncident.ClaimToken,
			Current:                       current.Add(time.Second),
			Status:                        model.CoordinationAttemptSucceeded,
		},
	)
	if recovered || !errors.Is(recoveryErr, store.ErrGraphRevisionConflict) {
		opened.Close()
		t.Fatalf("wrong-graph recovery = recovered %t, error %v", recovered, recoveryErr)
	}

	olderAttempt, _, err := opened.StartCoordinationAttempt(
		ctx,
		store.StartCoordinationAttemptInput{
			IncidentID: fixture.incident.ID,
			Board:      "default",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	terminalAttempt, _, err := opened.StartCoordinationAttempt(
		ctx,
		store.StartCoordinationAttemptInput{
			IncidentID: fixture.incident.ID,
			Board:      "default",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	terminalError := "independent terminal audit record"
	terminalAttempt, err = opened.FinishCoordinationAttempt(
		ctx,
		terminalAttempt.ID,
		store.FinishCoordinationAttemptInput{
			Board:  "default",
			Status: model.CoordinationAttemptFailed,
			Error:  &terminalError,
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}

	proposalCreatedAt, err := time.Parse(time.RFC3339Nano, proposals[0].CreatedAt)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	raw, err = sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		UPDATE coordination_attempts
		SET started_at = CASE id
			WHEN ? THEN ?
			WHEN ? THEN ?
			ELSE started_at
		END
		WHERE id IN (?, ?)
	`,
		olderAttempt.ID,
		proposalCreatedAt.Add(-time.Hour).Format(time.RFC3339Nano),
		abandonedAttemptID,
		proposalCreatedAt.Add(time.Hour).Format(time.RFC3339Nano),
		olderAttempt.ID,
		abandonedAttemptID,
	); err != nil {
		raw.Close()
		opened.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	graph, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	revision := graph.Revision
	otherIncident, _, err := opened.CreateCoordinationIncident(
		ctx,
		store.CreateCoordinationIncidentInput{
			Board: "default", Trigger: model.CoordinationTriggerGraphStalled,
			Severity:              model.CoordinationSeverityWarning,
			ExpectedGraphRevision: &revision,
			Summary:               "unrelated incident audit record",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	otherAttempt, _, err := opened.StartCoordinationAttempt(
		ctx,
		store.StartCoordinationAttemptInput{
			IncidentID: otherIncident.ID,
			Board:      "default",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	otherClaim, claimed, err := opened.ClaimCoordinationIncident(
		ctx,
		otherIncident.ID,
		store.ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &revision,
			TTL:                   coordinationClaimTTL(fixture.options),
			Current:               current,
		},
	)
	if err != nil || !claimed {
		opened.Close()
		t.Fatalf("claim unrelated incident = %+v, claimed=%t, error=%v", otherClaim, claimed, err)
	}
	unboundProposal, _, err := opened.CreateCoordinationProposal(
		ctx,
		store.CreateCoordinationProposalInput{
			IncidentID: otherIncident.ID, CoordinatorAgent: "manual-coordinator",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            otherClaim.ClaimToken,
			Current:               current.Add(time.Second),
			Summary:               "unbound manual proposal",
			Rationale:             "This proposal did not consume a paid runtime attempt.",
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if unboundProposal.AttemptID != nil {
		opened.Close()
		t.Fatalf("manual proposal unexpectedly bound attempt %q", *unboundProposal.AttemptID)
	}
	unboundRevision := unboundProposal.ExpectedGraphRevision
	otherIncidentRevision := otherClaim.GraphRevision
	_, recovered, recoveryErr = opened.RecoverCoordinationAttemptForProposal(
		ctx,
		store.RecoverCoordinationAttemptInput{
			Board: "default", ProposalID: unboundProposal.ID,
			ExpectedProposalStatus:        model.CoordinationProposalValidating,
			ExpectedProposalGraphRevision: &unboundRevision,
			ExpectedIncidentGraphRevision: &otherIncidentRevision,
			ClaimToken:                    otherClaim.ClaimToken,
			Current:                       current.Add(2 * time.Second),
			Status:                        model.CoordinationAttemptSucceeded,
		},
	)
	if recoveryErr != nil || recovered {
		opened.Close()
		t.Fatalf("unbound proposal recovery = recovered %t, error %v", recovered, recoveryErr)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err = sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DROP TRIGGER fail_coordination_attempt_success"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	current = fixture.current.Add(coordinationClaimTTL(fixture.options) + time.Second)
	secondErr := runCoordinationPass(
		ctx, fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, current,
	)
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("reusable proposal made %d paid calls, want 1 total", calls.Load())
	}

	opened, err = fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err = opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err = opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]model.CoordinationAttempt, len(attempts))
	for _, attempt := range attempts {
		byID[attempt.ID] = attempt
	}
	unrelatedAttempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: otherIncident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval ||
		proposals[0].AttemptID == nil ||
		*proposals[0].AttemptID != abandonedAttemptID ||
		byID[abandonedAttemptID].Status != model.CoordinationAttemptSucceeded ||
		byID[abandonedAttemptID].SelectedAgent != proposals[0].CoordinatorAgent ||
		byID[abandonedAttemptID].EndedAt == nil ||
		byID[olderAttempt.ID].Status != model.CoordinationAttemptStarted ||
		byID[terminalAttempt.ID].Status != model.CoordinationAttemptFailed ||
		len(unrelatedAttempts) != 1 ||
		unrelatedAttempts[0].ID != otherAttempt.ID ||
		unrelatedAttempts[0].Status != model.CoordinationAttemptStarted {
		t.Fatalf(
			"recovered audit mismatch: incident=%+v proposals=%+v attempts=%+v unrelated=%+v",
			incident,
			proposals,
			attempts,
			unrelatedAttempts,
		)
	}
}

func TestCoordinationRuntimeBoundsWholeAnalysisByPlannerTimeout(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	fixture.options.PlannerTimeout = 25 * time.Millisecond
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		ctx context.Context,
		_ orchestration.PlannerRequest,
	) (any, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	err := runCoordinationPass(
		context.Background(), fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Coordinator timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Coordinator analysis exceeded its total timeout: %s", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("Coordinator calls = %d, want 1", calls.Load())
	}
	opened, openErr := fixture.manager.OpenStore(context.Background(), "default")
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer opened.Close()
	incident, getErr := opened.GetCoordinationIncident(
		context.Background(), fixture.incident.ID,
	)
	if getErr != nil {
		t.Fatal(getErr)
	}
	attempts, listErr := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if incident.Status != model.CoordinationIncidentOpen ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptFailed {
		t.Fatalf("timeout recovery: incident=%+v attempts=%+v", incident, attempts)
	}
}

func TestCoordinationClaimAlwaysCoversAnalysisTimeoutAndGrace(t *testing.T) {
	for _, configured := range []time.Duration{
		0,
		25 * time.Millisecond,
		time.Minute,
		24 * time.Hour,
	} {
		options := Options{PlannerTimeout: configured}
		analysis := coordinationAnalysisTimeout(options)
		claim := coordinationClaimTTL(options)
		if analysis <= 0 ||
			claim < analysis+coordinationClaimGrace ||
			claim > store.MaxCoordinationIncidentClaimTTL {
			t.Fatalf(
				"configured=%s analysis=%s claim=%s grace=%s",
				configured,
				analysis,
				claim,
				coordinationClaimGrace,
			)
		}
	}
}

func TestReconcilePendingCoordinationSupersedesStaleApproval(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	ctx := context.Background()
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	metadata, _ := fixture.manager.Read("default")
	if err := reconcilePendingCoordination(
		ctx, fixture.manager, opened, metadata, fixture.options,
		fixture.current.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		ctx, store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if incident.Status != model.CoordinationIncidentOpen ||
		incident.GraphRevision != 1 || len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf("stale approval recovery: incident=%+v proposals=%+v", incident, proposals)
	}
}

func TestReconcilePendingCoordinationRevalidatesTaskVersions(t *testing.T) {
	for _, approved := range []bool{false, true} {
		name := "awaiting"
		if approved {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
			var calls atomic.Int32
			fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
			ctx := context.Background()
			if err := runCoordinationPass(
				ctx, fixture.manager, []string{"default"},
				fixture.options, &coordinationRuntimeState{}, fixture.current,
			); err != nil {
				t.Fatal(err)
			}
			opened, err := fixture.manager.OpenStore(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			proposals, err := opened.ListCoordinationProposals(
				ctx, store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
			)
			if err != nil || len(proposals) != 1 {
				t.Fatalf("pending proposals = %+v, %v", proposals, err)
			}
			if approved {
				revision := fixture.incident.GraphRevision
				result, approveErr := opened.ApproveCoordinationProposal(
					ctx, proposals[0].ID, store.ApproveCoordinationProposalInput{
						ExpectedUpdatedAt:     proposals[0].UpdatedAt,
						ExpectedGraphRevision: &revision,
					},
				)
				if approveErr != nil {
					t.Fatal(approveErr)
				}
				proposals[0] = result.Proposal
			}
			title := "changed before the approval decision was applied"
			if _, err := opened.UpdateTask(
				ctx, fixture.task.ID, store.UpdateTaskInput{Title: &title},
			); err != nil {
				t.Fatal(err)
			}
			metadata, _ := fixture.manager.Read("default")
			if err := reconcilePendingCoordination(
				ctx, fixture.manager, opened, metadata, fixture.options,
				fixture.current.Add(2*time.Second),
			); err != nil {
				t.Fatal(err)
			}
			incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
			proposals, _ = opened.ListCoordinationProposals(
				ctx, store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
			)
			if calls.Load() != 1 || incident.Status != model.CoordinationIncidentOpen ||
				len(proposals) != 1 ||
				proposals[0].Status != model.CoordinationProposalSuperseded {
				t.Fatalf(
					"stale %s proposal remained active: calls=%d incident=%+v proposals=%+v",
					name, calls.Load(), incident, proposals,
				)
			}
		})
	}
}

func TestReconcilePendingCoordinationRevalidatesAgentHealth(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = func(
		context.Context,
		orchestration.PlannerRequest,
	) (any, error) {
		calls.Add(1)
		return coordinator.Proposal{
			IncidentID: fixture.incident.ID, ExpectedGraphRevision: fixture.incident.GraphRevision,
			Summary:   "Keep the worker route",
			Rationale: "The worker was healthy in the analysis snapshot.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionSetRoute, TaskID: fixture.task.ID,
				ExpectedUpdatedAt: fixture.task.UpdatedAt,
				Assignee:          "worker", Runtime: model.RuntimeCodex,
				Reason: "Use the configured worker.",
			}},
		}, nil
	}
	ctx := context.Background()
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthUnhealthy,
	}); err != nil {
		t.Fatal(err)
	}
	metadata, _ := fixture.manager.Read("default")
	if err := reconcilePendingCoordination(
		ctx, fixture.manager, opened, metadata, fixture.options,
		fixture.current.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		ctx, store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || incident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 || proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"unhealthy route remained pending: calls=%d incident=%+v proposals=%+v",
			calls.Load(), incident, proposals,
		)
	}
}

func TestCoordinationRuntimeResolvesDisappearedConditionBeforeSelectingCandidate(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	var calls atomic.Int32
	fixture.options.CoordinatorPlanner = priorityCoordinationPlanner(fixture, &calls)
	ctx := context.Background()
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current,
	); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("initial Coordinator calls = %d, want 1", calls.Load())
	}
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	status := model.TaskStatusTodo
	if _, err := opened.UpdateTask(
		ctx, fixture.task.ID, store.UpdateTaskInput{Status: &status},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new graph root"})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, fixture.task.ID); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runCoordinationPass(
		ctx, fixture.manager, []string{"default"},
		fixture.options, &coordinationRuntimeState{}, fixture.current.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	opened, err = fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	proposals, _ := opened.ListCoordinationProposals(
		ctx, store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if calls.Load() != 1 || incident.Status != model.CoordinationIncidentResolved ||
		len(proposals) != 1 || proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"disappeared condition was selected again: calls=%d incident=%+v proposals=%+v",
			calls.Load(), incident, proposals,
		)
	}
}

func TestReconcilePendingCoordinationRefreshesOpenIncidentGraph(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeObserve)
	ctx := context.Background()
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	metadata, _ := fixture.manager.Read("default")
	if err := reconcilePendingCoordination(
		ctx, fixture.manager, opened, metadata, fixture.options,
		fixture.current.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if incident.Status != model.CoordinationIncidentOpen || incident.GraphRevision != 1 {
		t.Fatalf("open incident graph was not refreshed: %+v", incident)
	}
}

func TestOneShotDispatcherRunsTaskUnblockedByCoordinator(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAuto)
	cliPath := buildAutogora(t)
	worker := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "Coordinator recovery completed" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	config := *fixture.options.AgentConfig
	for index := range config.Agents {
		if config.Agents[index].ID == "worker" {
			config.Agents[index].Command = worker
		}
	}
	config = agentconfig.Normalize(config)
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Run(ctx, Options{
		DBPath: fixture.dbPath, CLIPath: cliPath, Once: true,
		Autopilot: true, MaxWorkers: 1, AutoDecompose: boolValue(false),
		PlannerTimeout: time.Minute, AgentConfig: &config,
		CoordinatorPlanner: unblockCoordinationPlanner(fixture, &calls),
		Now:                fixture.options.Now,
		Getenv:             func(string) string { return "" },
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.GetTask(context.Background(), fixture.task.ID)
	var completed *model.Run
	for index := range task.Runs {
		if task.Runs[index].Status == model.RunStatusCompleted {
			completed = &task.Runs[index]
		}
	}
	if calls.Load() != 1 || task.Task.Status != model.TaskStatusDone ||
		completed == nil || completed.Summary == nil ||
		*completed.Summary != "Coordinator recovery completed" {
		t.Fatalf("one-shot recovery: calls=%d task=%+v", calls.Load(), task)
	}
}

func TestCoordinationRuntimeFinalizerExhaustionRequiresBlockClearingAction(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 1)
	cliPath := buildAutogora(t)
	worker := executableFixture(t, `
"$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "conflict still needs manual resolution" --kind needs_input >/dev/null`)

	// The first real finalizer run consumes the one conflict-resolution attempt.
	runFinalizerFixture(t, fixture, cliPath, worker)
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.UnblockTask(context.Background(), fixture.finalizerID); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	// The next run reaches the durable Finalizer ledger limit without launching
	// another worker and emits the real resolution_exhausted incident.
	runFinalizerFixture(t, fixture, cliPath, worker)
	opened, err = fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	incidents, err := opened.ListCoordinationIncidents(
		context.Background(),
		store.CoordinationIncidentFilter{
			TaskID:  fixture.finalizerID,
			Trigger: model.CoordinationTriggerIntegrationConflict,
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		opened.Close()
		t.Fatalf("Finalizer exhaustion incidents = %+v", incidents)
	}
	var incidentDetails map[string]any
	if err := json.Unmarshal(incidents[0].Details, &incidentDetails); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if incidentDetails["code"] != workspace.IntegrationFailureResolutionExhausted {
		opened.Close()
		t.Fatalf("Finalizer incident details = %#v", incidentDetails)
	}
	originalPriority := detail.Task.Priority
	originalBlockKind, originalBlockReason := detail.Task.BlockKind, detail.Task.BlockReason
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	setCoordinationTestMode(t, fixture.manager, "default", boards.CoordinationModeAuto)
	config := coordinatorRuntimeConfig()
	var calls atomic.Int32
	options := Options{
		Autopilot: true, PlannerTimeout: time.Minute,
		AgentConfig: &config, Getenv: func(string) string { return "" },
		Now: func() time.Time {
			return time.Now().UTC()
		},
		CoordinatorPlanner: func(
			context.Context,
			orchestration.PlannerRequest,
		) (any, error) {
			calls.Add(1)
			priority := originalPriority + 10
			return coordinator.Proposal{
				IncidentID: incidents[0].ID, ExpectedGraphRevision: incidents[0].GraphRevision,
				Summary:   "Prioritize the exhausted Finalizer",
				Rationale: "Try to advance the same still-blocked integration task.",
				Actions: []coordinator.Action{{
					Kind: coordinator.ActionUpdatePriority, TaskID: detail.Task.ID,
					ExpectedUpdatedAt: detail.Task.UpdatedAt, Priority: &priority,
					Reason: "raise the blocked Finalizer priority",
				}},
			}, nil
		},
	}
	passErr := runCoordinationPass(
		context.Background(),
		fixture.manager,
		[]string{"default"},
		options,
		&coordinationRuntimeState{},
		time.Now().UTC(),
	)
	if passErr == nil || !strings.Contains(passErr.Error(), "failed deterministic validation") {
		t.Fatalf("route-only exhaustion recovery error = %v", passErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("Finalizer exhaustion Coordinator calls = %d, want 1", calls.Load())
	}
	opened, err = fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	afterTask, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	afterIncident, err := opened.GetCoordinationIncident(context.Background(), incidents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		context.Background(),
		store.CoordinationProposalFilter{IncidentID: incidents[0].ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: incidents[0].ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterTask.Task.Priority != originalPriority ||
		afterTask.Task.BlockKind == nil || originalBlockKind == nil ||
		*afterTask.Task.BlockKind != *originalBlockKind ||
		afterTask.Task.BlockReason == nil || originalBlockReason == nil ||
		*afterTask.Task.BlockReason != *originalBlockReason ||
		afterIncident.Status != model.CoordinationIncidentOpen ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalFailed ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptFailed {
		t.Fatalf(
			"route-only exhaustion changed the blocked condition: task=%+v incident=%+v proposals=%+v attempts=%+v",
			afterTask.Task,
			afterIncident,
			proposals,
			attempts,
		)
	}
}

func TestCoordinationRuntimeEmptyIntegrationProposalEscalatesOnce(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	setCoordinationTestMode(t, manager, "default", boards.CoordinationModeAuto)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	config := coordinatorRuntimeConfig()
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "integration decision", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	const reason = "finalizer resolution attempts exhausted"
	task, err = opened.BlockTask(ctx, task.Task.ID, store.BlockInput{
		Kind: model.BlockKindNeedsInput, Reason: reason,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	incident := createCoordinatorIntegrationIncidentWithCode(
		t,
		ctx,
		opened,
		task.Task.ID,
		reason,
		workspace.IntegrationFailureResolutionExhausted,
	)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	current := time.Now().UTC()
	var calls atomic.Int32
	options := Options{
		Autopilot: true, PlannerTimeout: time.Minute,
		AgentConfig: &config, Getenv: func(string) string { return "" },
		Now: func() time.Time { return current.Add(time.Second) },
		CoordinatorPlanner: func(
			context.Context,
			orchestration.PlannerRequest,
		) (any, error) {
			calls.Add(1)
			return coordinator.Proposal{
				IncidentID: incident.ID, ExpectedGraphRevision: incident.GraphRevision,
				Summary:   "Manual integration decision required",
				Rationale: "No bounded action can safely resolve the exhausted conflict.",
				Actions:   []coordinator.Action{},
			}, nil
		},
	}
	if err := runCoordinationPass(
		ctx,
		manager,
		[]string{"default"},
		options,
		&coordinationRuntimeState{},
		current,
	); err != nil {
		t.Fatal(err)
	}
	if err := runCoordinationPass(
		ctx,
		manager,
		[]string{"default"},
		options,
		&coordinationRuntimeState{},
		current.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	opened, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	preservedTask, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	preservedIncident, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 ||
		preservedTask.Task.Status != model.TaskStatusBlocked ||
		preservedTask.Task.BlockKind == nil ||
		*preservedTask.Task.BlockKind != model.BlockKindNeedsInput ||
		preservedTask.Task.BlockReason == nil ||
		*preservedTask.Task.BlockReason != reason ||
		preservedIncident.Status != model.CoordinationIncidentAwaitingApproval ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalAwaitingApproval ||
		string(proposals[0].Actions) != "[]" ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded {
		t.Fatalf(
			"manual integration escalation: calls=%d task=%+v incident=%+v proposals=%+v attempts=%+v",
			calls.Load(),
			preservedTask.Task,
			preservedIncident,
			proposals,
			attempts,
		)
	}

	graphBefore, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	status := model.TaskStatusTodo
	if _, err := opened.UpdateTask(
		ctx,
		task.Task.ID,
		store.UpdateTaskInput{Status: &status},
	); err != nil {
		t.Fatal(err)
	}
	graphAfter, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if graphAfter.Revision != graphBefore.Revision {
		t.Fatalf(
			"manual task resolution unexpectedly changed graph revision: before=%d after=%d",
			graphBefore.Revision,
			graphAfter.Revision,
		)
	}
	if err := runCoordinationPass(
		ctx,
		manager,
		[]string{"default"},
		options,
		&coordinationRuntimeState{},
		current.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	resolvedIncident, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err = opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 ||
		resolvedIncident.Status != model.CoordinationIncidentResolved ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalSuperseded {
		t.Fatalf(
			"manually resolved escalation remained pending: calls=%d incident=%+v proposals=%+v",
			calls.Load(),
			resolvedIncident,
			proposals,
		)
	}
}
