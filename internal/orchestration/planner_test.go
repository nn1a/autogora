package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(source), "testdata", name)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}, "required": []string{"title", "body"}}
}

func plannerResult(t *testing.T, runtime model.Runtime, envName, fixture, selectedModel, provider string) map[string]any {
	t.Helper()
	planner, err := CreateCLIPlanner(CLIPlannerOptions{
		Runtime: runtime, Model: selectedModel, Provider: provider, CWD: t.TempDir(), Timeout: 5 * time.Second,
		Getenv: func(name string) string {
			if name == envName {
				return fixturePath(t, fixture)
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(context.Background(), PlannerRequest{Kind: PlannerSpecify, Prompt: "Specify this", Schema: testSchema()})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	result := map[string]any{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCodexCLIPlannerUsesStrictSchema(t *testing.T) {
	value := plannerResult(t, model.RuntimeCodex, "AUTOGORA_CODEX_BIN", "planner-agent.sh", "gpt-test", "")
	if value["title"] != "Planner-generated task specification" {
		t.Fatalf("unexpected result: %#v", value)
	}
}

func TestClineCLIPlannerReadsFinalNDJSONResult(t *testing.T) {
	value := plannerResult(t, model.RuntimeCline, "AUTOGORA_CLINE_BIN", "planner-cline-agent.sh", "cline-test", "openrouter")
	if value["title"] != "Cline-generated task specification" {
		t.Fatalf("unexpected result: %#v", value)
	}
}

func TestGeminiCLIPlannerUsesDenyAllPolicy(t *testing.T) {
	value := plannerResult(t, model.RuntimeGemini, "AUTOGORA_GEMINI_BIN", "planner-gemini-agent.sh", "gemini-test", "")
	if value["title"] != "Gemini-generated task specification" {
		t.Fatalf("unexpected result: %#v", value)
	}
}

func TestClaudeCLIPlannerPassesConfiguredModel(t *testing.T) {
	value := plannerResult(t, model.RuntimeClaude, "AUTOGORA_CLAUDE_BIN", "planner-claude-agent.sh", "claude-test", "")
	if value["title"] != "Claude-generated task specification" {
		t.Fatalf("unexpected result: %#v", value)
	}
}

func TestCLIPlannerRejectsManualRuntime(t *testing.T) {
	if _, err := CreateCLIPlanner(CLIPlannerOptions{Runtime: model.RuntimeManual}); err == nil {
		t.Fatal("expected manual planner runtime to be rejected")
	}
}

func TestCLIPlannerRejectsMissingWorkingDirectoryBeforeAgentHealthClassification(t *testing.T) {
	_, err := CreateCLIPlanner(CLIPlannerOptions{Runtime: model.RuntimeCodex, CWD: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("expected a missing planner working directory to be rejected")
	}
	if _, retry := ClassifyPlannerFailure(err); retry {
		t.Fatalf("working directory error was classified as an agent failure: %v", err)
	}
}

func TestCLIPlannerUsesRegisteredCommand(t *testing.T) {
	planner, err := CreateCLIPlanner(CLIPlannerOptions{
		Runtime: model.RuntimeCodex, Command: fixturePath(t, "planner-agent.sh"), Model: "gpt-test",
		CWD: t.TempDir(), Timeout: 5 * time.Second, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(context.Background(), PlannerRequest{Kind: PlannerSpecify, Prompt: "Specify this", Schema: testSchema()})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["title"] != "Planner-generated task specification" {
		t.Fatalf("registered planner command result = %#v", value)
	}
}
