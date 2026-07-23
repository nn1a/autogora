package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func coordinatorTestConfig(agents ...agentconfig.Agent) agentconfig.Config {
	defaults := []string{}
	for _, agent := range agents {
		if agent.Enabled && hasAgentRole(agent, agentconfig.RoleWorker) {
			defaults = append(defaults, agent.ID)
		}
	}
	return agentconfig.Normalize(agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 2},
		Defaults:      agentconfig.Defaults{WorkerAgents: defaults},
		Agents:        agents,
	})
}

func coordinatorWorker(id string, fallbacks ...string) agentconfig.Agent {
	return agentconfig.Agent{
		ID: id, Runtime: model.RuntimeCodex, Command: "/bin/true",
		Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleWorker}, Fallbacks: fallbacks,
	}
}

func observeCoordinatorTestBoard(
	t *testing.T,
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	config agentconfig.Config,
	current time.Time,
) []model.CoordinationIncident {
	t.Helper()
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	incidents, err := reconcileCoordinatorIncidents(
		ctx, manager, opened, metadata, Options{
			AgentConfig: &config, Getenv: func(string) string { return "" },
		}, current,
	)
	if err != nil {
		t.Fatal(err)
	}
	return incidents
}

func findCoordinatorIncident(
	incidents []model.CoordinationIncident,
	trigger model.CoordinationTrigger,
) *model.CoordinationIncident {
	for index := range incidents {
		if incidents[index].Trigger == trigger {
			return &incidents[index]
		}
	}
	return nil
}

func TestCoordinatorObserverRepeatedBlockThresholdAndReconciliation(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	config := coordinatorTestConfig(coordinatorWorker("worker"))
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "repeat the same block", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := store.BlockInput{Kind: model.BlockKindCapability, Reason: "required compiler is unavailable"}
	if _, err := opened.BlockTask(ctx, task.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	if incidents := observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now()); findCoordinatorIncident(
		incidents, model.CoordinationTriggerRepeatedBlock,
	) != nil {
		t.Fatalf("one block recurrence opened an incident: %+v", incidents)
	}
	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.BlockTask(ctx, task.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	incidents := observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now())
	repeated := findCoordinatorIncident(incidents, model.CoordinationTriggerRepeatedBlock)
	if repeated == nil || repeated.TaskID == nil || *repeated.TaskID != task.Task.ID ||
		repeated.Status != model.CoordinationIncidentOpen {
		t.Fatalf("repeated block incident = %+v", repeated)
	}
	var details map[string]any
	if err := json.Unmarshal(repeated.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["blockRecurrences"] != float64(2) || details["blockReason"] != block.Reason {
		t.Fatalf("repeated block details = %#v", details)
	}

	done := model.TaskStatusDone
	if _, err := opened.UpdateTask(ctx, task.Task.ID, store.UpdateTaskInput{Status: &done}); err != nil {
		t.Fatal(err)
	}
	observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now())
	resolved, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		Trigger: model.CoordinationTriggerRepeatedBlock,
		Status:  model.CoordinationIncidentResolved,
	})
	if err != nil || len(resolved) != 1 || resolved[0].ID != repeated.ID {
		t.Fatalf("resolved repeated block incidents = %+v, %v", resolved, err)
	}
}

func TestCoordinatorObserverRetryExhaustionPreservesFailureDetails(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	worker := coordinatorWorker("worker")
	worker.Roles = []agentconfig.Role{agentconfig.RoleWorker, agentconfig.RoleCoordinator}
	worker.MaxConcurrent = 2
	config := coordinatorTestConfig(worker)
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "exhaust retries", Assignee: &assignee, Runtime: model.RuntimeCodex, MaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
		if err != nil || claim == nil {
			t.Fatalf("claim attempt %d: %+v, %v", attempt, claim, err)
		}
		message := fmt.Sprintf("compiler failed on attempt %d", attempt)
		if _, err := opened.FailRun(ctx, store.RunScope{
			RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
		}, message, store.FailRunOptions{}); err != nil {
			t.Fatal(err)
		}
		if attempt == 1 {
			incidents := observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now())
			if findCoordinatorIncident(incidents, model.CoordinationTriggerRetryExhausted) != nil {
				t.Fatalf("retry incident opened below the configured threshold: %+v", incidents)
			}
		}
	}
	incidents := observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now())
	exhausted := findCoordinatorIncident(incidents, model.CoordinationTriggerRetryExhausted)
	if exhausted == nil {
		t.Fatalf("retry exhaustion incident missing: %+v", incidents)
	}
	var details map[string]any
	if err := json.Unmarshal(exhausted.Details, &details); err != nil {
		t.Fatal(err)
	}
	lastRun, ok := details["lastRun"].(map[string]any)
	if details["failureCount"] != float64(2) || details["maxRetries"] != float64(2) ||
		!ok || lastRun["error"] != "compiler failed on attempt 2" ||
		lastRun["status"] != string(model.RunStatusFailed) {
		t.Fatalf("retry exhaustion details = %#v", details)
	}

	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildCoordinatorIncidentSnapshot(
		ctx, manager, opened, metadata, Options{
			AgentConfig: &config, Getenv: func(string) string { return "" },
		}, *exhausted,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].FailureCount != 2 ||
		snapshot.Nodes[0].CurrentRunID != nil || snapshot.Nodes[0].BlockReason == nil ||
		*snapshot.Nodes[0].BlockReason != "compiler failed on attempt 2" {
		t.Fatalf("retry snapshot node = %+v", snapshot.Nodes)
	}
	if len(snapshot.AvailableAgents) != 1 || snapshot.AvailableAgents[0].ID != "worker" ||
		!snapshot.AvailableAgents[0].Enabled ||
		!containsCoordinatorString(snapshot.AvailableAgents[0].Roles, "worker") ||
		!containsCoordinatorString(snapshot.AvailableAgents[0].Roles, "coordinator") {
		t.Fatalf("snapshot agents = %+v", snapshot.AvailableAgents)
	}
}

func TestCoordinatorObserverUsesFallbackHealthAndIgnoresFullCapacity(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	primary, fallback := coordinatorWorker("primary", "fallback"), coordinatorWorker("fallback")
	config := coordinatorTestConfig(primary, fallback)
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "primary", Status: model.AgentHealthMissing,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "fallback", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	coordinationStore, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runID := "capacity-owned-by-another-board"
	_, acquired, err := coordinationStore.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: "fallback", Limit: 1, OwnerKind: store.AgentSlotOwnerWorker,
		Board: "other", RunID: &runID, OwnerID: "observer-capacity-fixture",
		Current: time.Now(),
	})
	if err != nil || !acquired {
		t.Fatalf("fill fallback capacity: acquired=%v err=%v", acquired, err)
	}
	if err := coordinationStore.Close(); err != nil {
		t.Fatal(err)
	}
	assignee := "primary"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "use a fallback", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskUpdatedAt, err := time.Parse(time.RFC3339Nano, task.Task.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Observe beyond the default idle threshold. A full global slot still must
	// not become either agent_exhausted or graph_stalled while the route is
	// otherwise healthy.
	incidents := observeCoordinatorTestBoard(t, ctx, manager, opened, config, taskUpdatedAt.Add(301*time.Second))
	if findCoordinatorIncident(incidents, model.CoordinationTriggerAgentExhausted) != nil {
		t.Fatalf("full but healthy fallback was treated as exhausted: %+v", incidents)
	}
	if findCoordinatorIncident(incidents, model.CoordinationTriggerGraphStalled) != nil {
		t.Fatalf("full but healthy fallback stalled the graph: %+v", incidents)
	}

	cooldown := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "fallback", Status: model.AgentHealthRateLimited, CooldownUntil: &cooldown,
	}); err != nil {
		t.Fatal(err)
	}
	incidents = observeCoordinatorTestBoard(t, ctx, manager, opened, config, time.Now())
	exhausted := findCoordinatorIncident(incidents, model.CoordinationTriggerAgentExhausted)
	if exhausted == nil || exhausted.TaskID == nil || *exhausted.TaskID != task.Task.ID {
		t.Fatalf("unhealthy fallback chain incident = %+v", incidents)
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildCoordinatorIncidentSnapshot(
		ctx, manager, opened, metadata, Options{
			AgentConfig: &config, Getenv: func(string) string { return "" },
		}, *exhausted,
	)
	if err != nil {
		t.Fatal(err)
	}
	var fallbackSnapshot *struct {
		active int
	}
	for _, agent := range snapshot.AvailableAgents {
		if agent.ID == "fallback" {
			fallbackSnapshot = &struct{ active int }{active: agent.ActiveSlots}
		}
	}
	if fallbackSnapshot == nil || fallbackSnapshot.active != 1 {
		t.Fatalf("global fallback capacity missing from snapshot: %+v", snapshot.AvailableAgents)
	}
}

func TestCoordinatorObserverGraphStalledWaitsForIdleAndReconciles(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	config := coordinatorTestConfig(coordinatorWorker("worker"))
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "parked manual work"})
	if err != nil {
		t.Fatal(err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, task.Task.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	metadata.Orchestration.Autopilot.Coordination.IdleSeconds = 60
	options := Options{AgentConfig: &config, Getenv: func(string) string { return "" }}
	incidents, err := reconcileCoordinatorIncidents(
		ctx, manager, opened, metadata, options, updatedAt.Add(59*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if findCoordinatorIncident(incidents, model.CoordinationTriggerGraphStalled) != nil {
		t.Fatalf("graph stalled before idle threshold: %+v", incidents)
	}
	incidents, err = reconcileCoordinatorIncidents(
		ctx, manager, opened, metadata, options, updatedAt.Add(60*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	stalled := findCoordinatorIncident(incidents, model.CoordinationTriggerGraphStalled)
	if stalled == nil {
		t.Fatalf("idle graph incident missing: %+v", incidents)
	}

	assignee := "worker"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "now runnable", Assignee: &assignee, Runtime: model.RuntimeCodex,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileCoordinatorIncidents(
		ctx, manager, opened, metadata, options, updatedAt.Add(61*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	resolved, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		Trigger: model.CoordinationTriggerGraphStalled,
		Status:  model.CoordinationIncidentResolved,
	})
	if err != nil || len(resolved) != 1 || resolved[0].ID != stalled.ID {
		t.Fatalf("resolved graph incidents = %+v, %v", resolved, err)
	}
}

func TestCoordinatorObserverGraphStalledIgnoresIntentionalWaits(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, context.Context, *store.Store) model.Task
	}{
		{
			name: "future scheduled task",
			setup: func(t *testing.T, ctx context.Context, opened *store.Store) model.Task {
				scheduledAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
					Title: "run later", Status: model.TaskStatusScheduled, ScheduledAt: &scheduledAt,
				})
				if err != nil {
					t.Fatal(err)
				}
				return detail.Task
			},
		},
		{
			name: "imported issue in triage",
			setup: func(t *testing.T, ctx context.Context, opened *store.Store) model.Task {
				key := "github-issue:owner/repository:42"
				detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
					Title: "review imported issue", Status: model.TaskStatusTriage, IdempotencyKey: &key,
				})
				if err != nil {
					t.Fatal(err)
				}
				return detail.Task
			},
		},
		{
			name: "task awaiting user input",
			setup: func(t *testing.T, ctx context.Context, opened *store.Store) model.Task {
				assignee := "worker"
				detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
					Title: "ask the user", Assignee: &assignee, Runtime: model.RuntimeCodex,
				})
				if err != nil {
					t.Fatal(err)
				}
				detail, err = opened.BlockTask(ctx, detail.Task.ID, store.BlockInput{
					Kind: model.BlockKindNeedsInput, Reason: "choose the target environment",
				})
				if err != nil {
					t.Fatal(err)
				}
				return detail.Task
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			manager, _ := testManager(t)
			opened, err := manager.OpenStore(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			config := coordinatorTestConfig(coordinatorWorker("worker"))
			if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
				AgentID: "worker", Status: model.AgentHealthReady,
			}); err != nil {
				t.Fatal(err)
			}
			task := test.setup(t, ctx, opened)
			updatedAt, err := time.Parse(time.RFC3339Nano, task.UpdatedAt)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := manager.Read("default")
			if err != nil {
				t.Fatal(err)
			}
			metadata.Orchestration.Autopilot.Coordination.IdleSeconds = 60
			incidents, err := reconcileCoordinatorIncidents(
				ctx, manager, opened, metadata,
				Options{AgentConfig: &config, Getenv: func(string) string { return "" }},
				updatedAt.Add(60*time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			if stalled := findCoordinatorIncident(incidents, model.CoordinationTriggerGraphStalled); stalled != nil {
				t.Fatalf("intentional wait opened graph_stalled: %+v", stalled)
			}
		})
	}
}

func TestCoordinatorObserverGraphStalledUsesActionableTaskAmongIntentionalWaits(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	config := coordinatorTestConfig(coordinatorWorker("worker"))
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	actionable, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "actionable parked work"})
	if err != nil {
		t.Fatal(err)
	}
	actionableUpdatedAt, err := time.Parse(time.RFC3339Nano, actionable.Task.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := actionableUpdatedAt.Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "scheduled later", Status: model.TaskStatusScheduled, ScheduledAt: &scheduledAt,
	}); err != nil {
		t.Fatal(err)
	}
	importKey := "github-issue:owner/repository:84"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "imported triage", Status: model.TaskStatusTriage, IdempotencyKey: &importKey,
	}); err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	needsInput, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "waiting for a decision", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.BlockTask(ctx, needsInput.Task.ID, store.BlockInput{
		Kind: model.BlockKindNeedsInput, Reason: "select a deployment region",
	}); err != nil {
		t.Fatal(err)
	}
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	metadata.Orchestration.Autopilot.Coordination.IdleSeconds = 60
	incidents, err := reconcileCoordinatorIncidents(
		ctx, manager, opened, metadata,
		Options{AgentConfig: &config, Getenv: func(string) string { return "" }},
		actionableUpdatedAt.Add(60*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	stalled := findCoordinatorIncident(incidents, model.CoordinationTriggerGraphStalled)
	if stalled == nil || stalled.TaskID == nil || *stalled.TaskID != actionable.Task.ID {
		t.Fatalf("mixed graph_stalled focus = %+v", stalled)
	}
	var details map[string]any
	if err := json.Unmarshal(stalled.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["unfinishedTasks"] != float64(4) ||
		details["actionableTasks"] != float64(1) ||
		details["intentionallyWaiting"] != float64(3) {
		t.Fatalf("mixed graph_stalled details = %#v", details)
	}
}

func TestBoundCoordinatorGraphLimitsNodesAndDependencies(t *testing.T) {
	graph := model.RelationshipGraph{
		FocusTaskID: "n249", RootTaskID: "n248", TotalConnectedNodes: 250,
		Nodes: make([]model.RelationshipNode, 0, 250),
	}
	for index := 0; index < 250; index++ {
		graph.Nodes = append(graph.Nodes, model.RelationshipNode{
			Task: model.RelationshipTask{ID: fmt.Sprintf("n%03d", index)},
		})
	}
	for index := 0; index < 900; index++ {
		from := fmt.Sprintf("n%03d", index%198)
		to := fmt.Sprintf("n%03d", (index+1)%198)
		graph.Dependencies = append(graph.Dependencies, model.DependencyEdge{
			PrerequisiteID: from, DependentID: to,
		})
	}
	nodes, dependencies, truncated := boundCoordinatorGraph(graph)
	if len(nodes) != coordinatorSnapshotNodeLimit ||
		len(dependencies) != coordinatorSnapshotDependencyLimit || !truncated {
		t.Fatalf("bounded graph nodes=%d dependencies=%d truncated=%v", len(nodes), len(dependencies), truncated)
	}
	selected := map[string]bool{}
	for _, node := range nodes {
		selected[node.Task.ID] = true
	}
	if !selected[graph.FocusTaskID] || !selected[graph.RootTaskID] {
		t.Fatalf("bounded graph omitted focus/root: selected=%v", selected)
	}
}
