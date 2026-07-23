package taskservice

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func TestBoardContextMergesStoredAndConfiguredProfiles(t *testing.T) {
	isolateGlobalAgentConfig(t)
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "autogora.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	configured := []boards.Profile{{Name: "reviewer", Runtime: model.RuntimeGemini, Description: "reviews changes"}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &configured}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	assignee := "reviewer"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Review", Assignee: &assignee, Runtime: model.RuntimeCodex}); err != nil {
		t.Fatal(err)
	}
	worker := "implementer"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Implement", Assignee: &worker, Runtime: model.RuntimeClaude}); err != nil {
		t.Fatal(err)
	}

	board, err := New(opened, manager, "default").BoardContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Profiles) != 2 {
		t.Fatalf("expected two profiles, got %#v", board.Profiles)
	}
	if board.Profiles[0].Name != "reviewer" || board.Profiles[0].Runtime != model.RuntimeGemini || board.Profiles[0].Description == "" {
		t.Fatalf("configured profile did not override task-derived route: %#v", board.Profiles[0])
	}
	if board.Profiles[1].Name != "implementer" || board.Profiles[1].Runtime != model.RuntimeClaude {
		t.Fatalf("task-derived profile missing: %#v", board.Profiles[1])
	}
}

func TestBoardContextMergesGlobalWorkerAgentsWithBoardSafetyLimits(t *testing.T) {
	isolateGlobalAgentConfig(t)
	config := agentconfig.Default()
	config.Agents = []agentconfig.Agent{
		{
			ID: "shared", Runtime: model.RuntimeCodex, Command: "codex", Model: "global-model", Provider: "global-provider",
			Enabled: false, MaxConcurrent: 2, Roles: []agentconfig.Role{agentconfig.RoleWorker}, Fallbacks: []string{"fallback-worker"},
		},
		{
			ID: "fallback-worker", Runtime: model.RuntimeClaude, Command: "claude", Model: "fallback-model",
			Enabled: true, MaxConcurrent: 3, Roles: []agentconfig.Role{agentconfig.RoleWorker},
		},
		{
			ID: "planner-only", Runtime: model.RuntimeGemini, Command: "gemini",
			Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner},
		},
	}
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	root := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	profiles := []boards.Profile{
		{
			Name: "shared", Runtime: model.RuntimeGemini, Model: "board-model", Provider: "board-provider",
			Description: "board-specific work", MaxConcurrent: 9, Priority: 8, Fallbacks: []string{"board-only"},
		},
		{Name: "board-only", Runtime: model.RuntimeCline, Description: "local worker"},
	}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	taskWorker := "task-only"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Existing route", Assignee: &taskWorker, Runtime: model.RuntimeGemini}); err != nil {
		t.Fatal(err)
	}

	board, err := New(opened, manager, "default").BoardContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Metadata.Orchestration.Profiles) != 2 {
		t.Fatalf("global profiles leaked into board metadata: %#v", board.Metadata.Orchestration.Profiles)
	}
	if len(board.Profiles) != 4 {
		t.Fatalf("expected global, board, and task-derived worker routes, got %#v", board.Profiles)
	}
	shared := routeNamed(t, board.Profiles, "shared")
	if shared.Runtime != model.RuntimeCodex {
		t.Fatalf("board changed registered runtime: %#v", shared)
	}
	if shared.Model != "board-model" || shared.Provider != "board-provider" || shared.Description != "board-specific work" || shared.Priority != 8 {
		t.Fatalf("board overrides were not applied: %#v", shared)
	}
	if !shared.Disabled || shared.MaxConcurrent != 2 {
		t.Fatalf("board bypassed global availability limits: %#v", shared)
	}
	if len(shared.Fallbacks) != 1 || shared.Fallbacks[0] != "board-only" {
		t.Fatalf("board fallback override missing: %#v", shared)
	}
	fallback := routeNamed(t, board.Profiles, "fallback-worker")
	if fallback.Runtime != model.RuntimeClaude || fallback.Model != "fallback-model" || fallback.Disabled || fallback.MaxConcurrent != 3 {
		t.Fatalf("global worker route missing fields: %#v", fallback)
	}
	_ = routeNamed(t, board.Profiles, "board-only")
	_ = routeNamed(t, board.Profiles, "task-only")
	for _, profile := range board.Profiles {
		if profile.Name == "planner-only" {
			t.Fatalf("planner-only agent appeared in worker routes: %#v", board.Profiles)
		}
	}
}

func TestMergeProfileRoutesLetsBoardTightenGlobalLimit(t *testing.T) {
	global := []orchestration.ProfileRoute{{Name: "worker", Runtime: model.RuntimeCodex, MaxConcurrent: 4}}
	board := []orchestration.ProfileRoute{{Name: "worker", Runtime: model.RuntimeClaude, Disabled: true, MaxConcurrent: 2}}

	profiles := mergeProfileRoutes(global, board)
	if len(profiles) != 1 || profiles[0].Runtime != model.RuntimeCodex || !profiles[0].Disabled || profiles[0].MaxConcurrent != 2 {
		t.Fatalf("board did not safely tighten global route: %#v", profiles)
	}
}

func TestServiceUsesGlobalDefaultPlannerWhenBoardIsUnpinned(t *testing.T) {
	isolateGlobalAgentConfig(t)
	command := filepath.Join(t.TempDir(), "cline-planner.sh")
	contents := `#!/bin/sh
case " $* " in *" --model planner-model "*) ;; *) exit 9 ;; esac
case " $* " in *" --provider test-provider "*) ;; *) exit 9 ;; esac
printf '%s\n' '{"type":"run_result","text":"{\"title\":\"Global planner\",\"body\":\"Configured result\"}"}'
`
	if err := os.WriteFile(command, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"planner"}
	config.Agents = []agentconfig.Agent{{
		ID: "planner", Runtime: model.RuntimeCline, Command: command, Model: "planner-model", Provider: "test-provider",
		Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner},
	}}
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}
	planner, err := (&Service{}).planner(boards.Metadata{Orchestration: boards.OrchestrationSettings{PlannerRuntime: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(context.Background(), orchestration.PlannerRequest{
		Kind: orchestration.PlannerSpecify, Prompt: "Specify", Schema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["title"] != "Global planner" {
		t.Fatalf("global planner result = %#v", value)
	}
}

func TestSharedServiceUsesBoardProfilesForExplicitDecomposition(t *testing.T) {
	isolateGlobalAgentConfig(t)
	ctx := context.Background()
	root := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	profiles := []boards.Profile{{Name: "coder", Runtime: model.RuntimeCodex, Description: "implements changes"}}
	defaultProfile := "coder"
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles, DefaultProfile: store.OptionalString{Set: true, Value: &defaultProfile}}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	rootTask, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Build feature", Body: "Split the work", Status: model.TaskStatusTriage})
	if err != nil {
		t.Fatal(err)
	}
	plan := &orchestration.DecompositionPlan{
		Fanout: true, RootTitle: "Build and verify feature", RootBody: "Complete every child and synthesize the result.", Reason: "Separate implementation",
		Tasks: []orchestration.DecompositionTask{{Key: "implementation", Title: "Implement feature", Body: "Implement and test it.", Assignee: "coder", Runtime: model.RuntimeGemini, Priority: 8}},
	}
	result, err := New(opened, manager, "default").DecomposeTask(ctx, rootTask.Task.ID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Fanout || result.Graph == nil || len(result.Graph.ChildIDs) != 1 {
		t.Fatalf("decomposition graph missing: %#v", result)
	}
	childDetail, err := opened.GetTask(ctx, result.Graph.ChildIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	child := childDetail.Task
	if child.Assignee == nil || *child.Assignee != "coder" || child.Runtime != model.RuntimeCodex {
		t.Fatalf("board profile was not applied to child: %#v", child)
	}
}

func isolateGlobalAgentConfig(t *testing.T) {
	t.Helper()
	t.Setenv("AUTOGORA_CONFIG", filepath.Join(t.TempDir(), "config.json"))
}

func routeNamed(t *testing.T, profiles []orchestration.ProfileRoute, name string) orchestration.ProfileRoute {
	t.Helper()
	for _, profile := range profiles {
		if profile.Name == name {
			return profile
		}
	}
	t.Fatalf("profile %q missing from %#v", name, profiles)
	return orchestration.ProfileRoute{}
}

func TestServiceDelegatesInteractiveDispatch(t *testing.T) {
	called := ""
	service := (&Service{}).WithTaskDispatcher(func(_ context.Context, taskID string) error {
		called = taskID
		return nil
	})
	if err := service.DispatchTask(context.Background(), "t_ready"); err != nil || called != "t_ready" {
		t.Fatalf("dispatch delegation: called=%q err=%v", called, err)
	}
	if err := (&Service{}).DispatchTask(context.Background(), "t_ready"); err == nil {
		t.Fatal("missing task dispatcher was accepted")
	}
}
