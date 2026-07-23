package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func TestCLIDecomposeUsesBoardRoutesAndSkipsDisabledDefaults(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defaultProfile, orchestratorProfile := "disabled", "orchestrator"
	profiles := []boards.Profile{
		{Name: "disabled", Runtime: model.RuntimeCodex, Disabled: true, Priority: 100},
		{Name: "worker", Runtime: model.RuntimeClaude, Priority: 10},
		{Name: "orchestrator", Runtime: model.RuntimeGemini, Priority: 5},
	}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{
		Profiles:            &profiles,
		DefaultProfile:      store.OptionalString{Set: true, Value: &defaultProfile},
		OrchestratorProfile: store.OptionalString{Set: true, Value: &orchestratorProfile},
	}}); err != nil {
		t.Fatal(err)
	}

	rootID := createTriageTaskForCLI(t, app, dbPath, "disabled")
	plan := orchestration.DecompositionPlan{
		Fanout: true, RootTitle: "Coordinate work", RootBody: "Merge and verify the child result.", Reason: "route safely",
		Tasks: []orchestration.DecompositionTask{{Key: "child", Title: "Implement", Body: "Implement and test.", Assignee: "disabled", Runtime: model.RuntimeCodex, Priority: 1}},
	}
	result := decomposeWithCLI(t, app, dbPath, rootID, plan)
	if result.Graph == nil || len(result.Graph.ChildIDs) != 1 || result.Task.Task.Assignee == nil || *result.Task.Task.Assignee != "orchestrator" || result.Task.Task.Runtime != model.RuntimeGemini {
		t.Fatalf("board orchestrator route not used: %#v", result)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	child, err := opened.GetTask(ctx, result.Graph.ChildIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Assignee == nil || *child.Task.Assignee != "worker" || child.Task.Runtime != model.RuntimeClaude {
		t.Fatalf("disabled board default was not bypassed safely: %#v", child.Task)
	}

	blockedID := createTriageTaskForCLI(t, app, dbPath, "")
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	blockedOutput := runApp(t, app, "decompose", blockedID, "--db", dbPath, "--default-profile", "disabled:gemini", "--plan-json", string(encodedPlan))
	if !strings.Contains(blockedOutput, `"ok": false`) || !strings.Contains(blockedOutput, "enabled worker profile") {
		t.Fatalf("explicit default bypassed disabled profile: %s", blockedOutput)
	}
}

func TestCLIDecomposeExplicitRoutesOverrideBoardDefaults(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	boardDefault := "board-worker"
	profiles := []boards.Profile{{Name: boardDefault, Runtime: model.RuntimeClaude}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{
		Profiles: &profiles, DefaultProfile: store.OptionalString{Set: true, Value: &boardDefault},
	}}); err != nil {
		t.Fatal(err)
	}

	rootID := createTriageTaskForCLI(t, app, dbPath, "")
	plan := orchestration.DecompositionPlan{
		Fanout: true, RootTitle: "Coordinate work", RootBody: "Merge and verify the child result.", Reason: "explicit routing",
		Tasks: []orchestration.DecompositionTask{{Key: "child", Title: "Implement", Body: "Implement and test.", Assignee: "unknown", Runtime: model.RuntimeCodex, Priority: 1}},
	}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	output := runApp(t, app, "decompose", rootID, "--db", dbPath,
		"--profile", "cli-worker:gemini", "--default-profile", "cli-worker", "--orchestrator-profile", "board-worker", "--plan-json", string(encodedPlan))
	result := decodeCLIDecomposition(t, output)
	if result.Graph == nil || len(result.Graph.ChildIDs) != 1 || result.Task.Task.Assignee == nil || *result.Task.Task.Assignee != "board-worker" || result.Task.Task.Runtime != model.RuntimeClaude {
		t.Fatalf("explicit orchestrator route not used: %#v", result)
	}
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	child, err := opened.GetTask(context.Background(), result.Graph.ChildIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Assignee == nil || *child.Task.Assignee != "cli-worker" || child.Task.Runtime != model.RuntimeGemini {
		t.Fatalf("explicit default/profile route not used: %#v", child.Task)
	}
}

func createTriageTaskForCLI(t *testing.T, app *App, dbPath, assignee string) string {
	t.Helper()
	args := []string{"create", "Route work", "--triage", "--db", dbPath}
	if assignee != "" {
		args = append(args, "--assignee", assignee, "--runtime", "codex")
	}
	output := runApp(t, app, args...)
	var created struct {
		Task model.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		t.Fatal(err)
	}
	return created.Task.ID
}

func decomposeWithCLI(t *testing.T, app *App, dbPath, taskID string, plan orchestration.DecompositionPlan) orchestration.DecompositionResult {
	t.Helper()
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return decodeCLIDecomposition(t, runApp(t, app, "decompose", taskID, "--db", dbPath, "--plan-json", string(encoded)))
}

func decodeCLIDecomposition(t *testing.T, output string) orchestration.DecompositionResult {
	t.Helper()
	var results []struct {
		OK    bool                              `json:"ok"`
		Value orchestration.DecompositionResult `json:"value"`
		Error string                            `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("decomposition failed: %s", output)
	}
	return results[0].Value
}
