package agentconfig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestBuiltinPresetsProduceValidUnpinnedConfigurations(t *testing.T) {
	wantIDs := []string{"codex", "claude", "codex-claude", "claude-codex"}
	presets := BuiltinPresets()
	if len(presets) != len(wantIDs) {
		t.Fatalf("preset count = %d, want %d", len(presets), len(wantIDs))
	}
	for index, wantID := range wantIDs {
		if presets[index].ID != wantID {
			t.Fatalf("preset[%d].ID = %q, want %q", index, presets[index].ID, wantID)
		}
		config, err := BuildPreset(wantID, nil)
		if err != nil {
			t.Fatalf("BuildPreset(%q): %v", wantID, err)
		}
		if err := Validate(config); err != nil {
			t.Fatalf("Validate(%q): %v", wantID, err)
		}
		for _, agent := range config.Agents {
			if agent.Model != "" || agent.Provider != "" {
				t.Fatalf("%s pins a stale model or provider: %#v", wantID, agent)
			}
			wantRoles := []Role{RoleWorker, RolePlanner, RoleCoordinator, RoleJudge}
			if !reflect.DeepEqual(agent.Roles, wantRoles) {
				t.Fatalf("%s agent roles = %#v, want %#v", wantID, agent.Roles, wantRoles)
			}
		}
	}
}

func TestDualRuntimePresetsDefineFallbackAndIndependentJudgeOrder(t *testing.T) {
	tests := []struct {
		id        string
		primary   string
		secondary string
	}{
		{id: "codex-claude", primary: "codex", secondary: "claude"},
		{id: "claude-codex", primary: "claude", secondary: "codex"},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			config, err := BuildPreset(test.id, nil)
			if err != nil {
				t.Fatal(err)
			}
			primary, found := config.Find(test.primary)
			if !found || !reflect.DeepEqual(primary.Fallbacks, []string{test.secondary}) {
				t.Fatalf("primary = %#v, found=%t", primary, found)
			}
			if !reflect.DeepEqual(config.Defaults.WorkerAgents, []string{test.primary, test.secondary}) ||
				!reflect.DeepEqual(config.Defaults.PlannerAgents, []string{test.primary, test.secondary}) ||
				!reflect.DeepEqual(config.Defaults.CoordinatorAgents, []string{test.secondary, test.primary}) ||
				!reflect.DeepEqual(config.Defaults.JudgeAgents, []string{test.secondary, test.primary}) {
				t.Fatalf("defaults = %#v", config.Defaults)
			}
		})
	}
}

func TestBuiltinPresetsReturnIndependentCopies(t *testing.T) {
	first := BuiltinPresets()
	first[0].Agents[0].Command = "/mutated"
	first[0].Agents[0].Roles[0] = RoleJudge
	first[0].Defaults.WorkerAgents[0] = "mutated"

	second := BuiltinPresets()
	if second[0].Agents[0].Command != "codex" ||
		second[0].Agents[0].Roles[0] != RoleWorker ||
		second[0].Defaults.WorkerAgents[0] != "codex" {
		t.Fatalf("built-in catalog was mutated: %#v", second[0])
	}
}

func TestBuildPresetUsesAuthoritativeExecutableDetections(t *testing.T) {
	detections := []Detection{
		{ID: "codex", Runtime: model.RuntimeCodex, Executable: "/tools/codex", State: "installed"},
		{ID: "claude", Runtime: model.RuntimeClaude, State: "missing"},
	}
	config, err := BuildPreset(" CODEX-CLAUDE ", detections)
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := config.Find("codex")
	claude, _ := config.Find("claude")
	if !codex.Enabled || codex.Command != "/tools/codex" {
		t.Fatalf("available codex = %#v", codex)
	}
	if claude.Enabled || claude.Command != "claude" {
		t.Fatalf("missing claude = %#v", claude)
	}
	if codex.Model != "" || claude.Model != "" {
		t.Fatalf("detection pinned models: codex=%#v claude=%#v", codex, claude)
	}

	unchecked, err := BuildPreset("codex-claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range unchecked.Agents {
		if !agent.Enabled {
			t.Fatalf("nil detections unexpectedly disabled %#v", agent)
		}
	}
}

func TestApplyPresetMergesWithoutOverwritingExistingConfiguration(t *testing.T) {
	current := Default()
	current.Supervisor = Supervisor{AutoStart: true, MaxWorkers: 4, AllowWrites: true}
	current.Defaults.WorkerAgents = []string{"custom"}
	current.Agents = []Agent{
		{
			ID: "custom", Runtime: model.RuntimeCodex, Command: "/custom/worker",
			Model: "custom-model", Provider: "custom-provider", Enabled: true,
			MaxConcurrent: 3, Roles: []Role{RoleWorker},
		},
		{
			ID: "codex", Runtime: model.RuntimeCodex, Command: "/existing/codex",
			Model: "user-selected-model", Provider: "user-provider", Enabled: false,
			MaxConcurrent: 7, Roles: []Role{RoleWorker, RolePlanner, RoleCoordinator, RoleJudge},
		},
	}

	result, err := ApplyPreset(current, "codex-claude", PresetApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Supervisor, current.Supervisor) {
		t.Fatalf("supervisor changed: got %#v want %#v", result.Supervisor, current.Supervisor)
	}
	codex, _ := result.Find("codex")
	if codex.Command != "/existing/codex" || codex.Model != "user-selected-model" ||
		codex.Provider != "user-provider" || codex.Enabled || codex.MaxConcurrent != 7 {
		t.Fatalf("existing codex was overwritten: %#v", codex)
	}
	claude, found := result.Find("claude")
	if !found || claude.Runtime != model.RuntimeClaude || !claude.Enabled {
		t.Fatalf("missing preset agent was not added: %#v, found=%t", claude, found)
	}
	if !reflect.DeepEqual(result.Defaults.WorkerAgents, []string{"custom"}) {
		t.Fatalf("non-empty worker defaults were overwritten: %#v", result.Defaults)
	}
	if !reflect.DeepEqual(result.Defaults.PlannerAgents, []string{"codex", "claude"}) ||
		!reflect.DeepEqual(result.Defaults.CoordinatorAgents, []string{"claude", "codex"}) ||
		!reflect.DeepEqual(result.Defaults.JudgeAgents, []string{"claude", "codex"}) {
		t.Fatalf("empty role defaults were not filled: %#v", result.Defaults)
	}
}

func TestApplyPresetExplicitReplacementUpdatesPresetOwnedEntries(t *testing.T) {
	current := Default()
	current.Supervisor = Supervisor{AutoStart: true, MaxWorkers: 2}
	current.Defaults.WorkerAgents = []string{"custom"}
	current.Agents = []Agent{
		{
			ID: "custom", Runtime: model.RuntimeGemini, Command: "gemini",
			Enabled: true, MaxConcurrent: 1, Roles: []Role{RoleWorker},
		},
		{
			ID: "codex", Runtime: model.RuntimeCodex, Command: "/old/codex",
			Model: "old-model", Enabled: false, MaxConcurrent: 8, Roles: []Role{RoleWorker},
		},
	}
	detections := []Detection{
		{ID: "codex", Runtime: model.RuntimeCodex, Executable: "/detected/codex", State: "version_unavailable"},
		{ID: "claude", Runtime: model.RuntimeClaude, Executable: "/detected/claude", State: "installed"},
	}
	result, err := ApplyPreset(current, "codex-claude", PresetApplyOptions{
		Detections: detections, ReplaceExisting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := result.Find("codex")
	if codex.Command != "/detected/codex" || codex.Model != "" || !codex.Enabled ||
		codex.MaxConcurrent != 1 || !reflect.DeepEqual(codex.Fallbacks, []string{"claude"}) {
		t.Fatalf("codex replacement = %#v", codex)
	}
	if _, found := result.Find("custom"); !found {
		t.Fatal("replacement removed an unrelated agent")
	}
	if !reflect.DeepEqual(result.Defaults.WorkerAgents, []string{"codex", "claude"}) ||
		!reflect.DeepEqual(result.Defaults.CoordinatorAgents, []string{"claude", "codex"}) ||
		!reflect.DeepEqual(result.Defaults.JudgeAgents, []string{"claude", "codex"}) {
		t.Fatalf("replacement defaults = %#v", result.Defaults)
	}
	if !reflect.DeepEqual(result.Supervisor, current.Supervisor) {
		t.Fatalf("replacement changed supervisor: %#v", result.Supervisor)
	}
}

func TestApplyPresetRejectsIncompatibleNonDestructiveMerge(t *testing.T) {
	current := Default()
	current.Agents = []Agent{{
		ID: "codex", Runtime: model.RuntimeCodex, Command: "codex",
		Enabled: true, MaxConcurrent: 1, Roles: []Role{RoleJudge},
	}}
	original := cloneConfig(current)
	_, err := ApplyPreset(current, "codex", PresetApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "without the worker role") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(current, original) {
		t.Fatalf("failed merge mutated input: got %#v want %#v", current, original)
	}
}

func TestBuildPresetRejectsUnknownID(t *testing.T) {
	_, err := BuildPreset("unknown", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown agent preset "unknown"`) {
		t.Fatalf("error = %v", err)
	}
}
