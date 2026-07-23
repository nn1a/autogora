package dispatcher

import (
	"context"
	"database/sql"
	"errors"
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

func TestCoordinationRuntimeObserveChangeNeverAutoApplies(t *testing.T) {
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
	if calls.Load() != 1 || task.Task.Priority != 1 ||
		incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf(
			"Observe policy change auto-applied: calls=%d task=%+v incident=%+v",
			calls.Load(), task.Task, incident,
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
