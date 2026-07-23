package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

func saveMCPAgentConfig(t *testing.T, path string, config agentconfig.Config) func(string) string {
	t.Helper()
	getenv := func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}
	if err := agentconfig.Save(agentconfig.Options{Getenv: getenv}, config); err != nil {
		t.Fatal(err)
	}
	return getenv
}

func TestRegisteredPlannerSettingsRespectGlobalBoardAndRequestPrecedence(t *testing.T) {
	directory := t.TempDir()
	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"disabled-planner", "global-planner"}
	config.Agents = []agentconfig.Agent{
		{ID: "disabled-planner", Runtime: model.RuntimeClaude, Command: "/disabled/claude", Enabled: false, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		{ID: "global-planner", Runtime: model.RuntimeCline, Command: "/registered/cline", Model: "global-model", Provider: "global-provider", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
	}
	service := &Service{getenv: saveMCPAgentConfig(t, filepath.Join(directory, "config.json"), config)}
	unpinned := boards.Metadata{Orchestration: boards.OrchestrationSettings{PlannerRuntime: model.RuntimeCodex}}

	runtime, command, modelName, provider, err := service.registeredPlannerSettings(unpinned, "", "request-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if runtime != model.RuntimeCline || command != "/registered/cline" || modelName != "request-model" || provider != "global-provider" {
		t.Fatalf("global planner settings = %s/%q/%q/%q", runtime, command, modelName, provider)
	}

	pinned := boards.Metadata{Orchestration: boards.OrchestrationSettings{
		PlannerRuntime: model.RuntimeClaude, PlannerModel: "board-model", PlannerProvider: "board-provider",
	}}
	runtime, command, modelName, provider, err = service.registeredPlannerSettings(pinned, "", "", "request-provider")
	if err != nil {
		t.Fatal(err)
	}
	if runtime != model.RuntimeClaude || command != "" || modelName != "board-model" || provider != "request-provider" {
		t.Fatalf("board planner settings = %s/%q/%q/%q", runtime, command, modelName, provider)
	}

	runtime, command, modelName, provider, err = service.registeredPlannerSettings(unpinned, model.RuntimeGemini, "explicit-model", "explicit-provider")
	if err != nil {
		t.Fatal(err)
	}
	if runtime != model.RuntimeGemini || command != "" || modelName != "explicit-model" || provider != "explicit-provider" {
		t.Fatalf("explicit planner settings = %s/%q/%q/%q", runtime, command, modelName, provider)
	}
}

func TestMCPPlanningToolsUseEnabledGlobalPlannerCommand(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(directory, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "default", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(directory, "planner-args")
	markerPath := filepath.Join(directory, "planner-called")
	binaryPath := filepath.Join(directory, "registered-cline")
	writePlanner := func(result string) {
		t.Helper()
		script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > " + strconv.Quote(argsPath) +
			"\nprintf 'called\\n' > " + strconv.Quote(markerPath) + "\nprintf '%s\\n' " + strconv.Quote(result) + "\n"
		if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePlanner(`{"type":"run_result","text":"{\"title\":\"Global specification\",\"body\":\"Configured planner command\"}"}`)

	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"disabled-planner", "global-planner"}
	config.Agents = []agentconfig.Agent{
		{ID: "disabled-planner", Runtime: model.RuntimeCodex, Command: filepath.Join(directory, "must-not-run"), Enabled: false, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		{ID: "global-planner", Runtime: model.RuntimeCline, Command: binaryPath, Model: "global-model", Provider: "global-provider", Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
	}
	server, service := New(manager, "test")
	service.getenv = saveMCPAgentConfig(t, filepath.Join(directory, "config.json"), config)
	session, done := connectTestServer(t, server)
	defer closeTestServer(t, session, done)

	var rough struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "autogora_create", map[string]any{"title": "rough", "status": "triage"}, &rough)
	var specified struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "autogora_specify", map[string]any{"task_id": rough.Task.ID}, &specified)
	if specified.Task.Title != "Global specification" || specified.Task.Body != "Configured planner command" {
		t.Fatalf("unexpected global planner specification: %+v", specified.Task)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("registered planner command was not invoked: %v", err)
	}
	assertGlobalPlannerArguments(t, argsPath)

	writePlanner(`{"type":"run_result","text":"{\"fanout\":false,\"rootTitle\":\"Global decomposition\",\"rootBody\":\"Configured graph\",\"reason\":\"single task\",\"tasks\":[],\"dependencies\":[]}"}`)
	var root struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "autogora_create", map[string]any{"title": "rough graph", "status": "triage"}, &root)
	var decomposed orchestration.DecompositionResult
	toolJSON(t, session, "autogora_decompose", map[string]any{"task_id": root.Task.ID}, &decomposed)
	if decomposed.Fanout || decomposed.Task.Task.Title != "Global decomposition" || decomposed.Task.Task.Assignee == nil || *decomposed.Task.Task.Assignee != "cline-worker" {
		t.Fatalf("unexpected global planner decomposition: %+v", decomposed)
	}
	assertGlobalPlannerArguments(t, argsPath)
}

func assertGlobalPlannerArguments(t *testing.T, path string) {
	t.Helper()
	args, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	arguments := "\n" + string(args) + "\n"
	if !strings.Contains(arguments, "\n--model\nglobal-model\n") || !strings.Contains(arguments, "\n--provider\nglobal-provider\n") {
		t.Fatalf("global planner model/provider were not passed to Cline:\n%s", args)
	}
}
