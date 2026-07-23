package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	setupcfg "github.com/nn1a/autogora/internal/setup"
)

func newAgentConfigTestApp(t *testing.T) (*App, agentconfig.Options) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	getenv := func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = t.TempDir()
	app.Getenv = getenv
	return app, agentconfig.Options{Getenv: getenv}
}

func TestAgentsCLIManagesRegistryDefaultsAndSupervisor(t *testing.T) {
	app, configOptions := newAgentConfigTestApp(t)
	pathOutput := runApp(t, app, "agents", "path")
	if !strings.Contains(pathOutput, `"exists": false`) {
		t.Fatalf("initial path output = %s", pathOutput)
	}

	runApp(t, app, "agents", "set", "primary", "--runtime", "codex", "--command", "/tools/codex", "--model", "gpt-test", "--provider", "openai", "--roles", "worker,planner,coordinator", "--max-concurrent", "2")
	runApp(t, app, "agents", "set", "backup", "--runtime", "claude", "--roles", "worker,judge")
	runApp(t, app, "agents", "set", "primary", "--runtime", "codex", "--fallbacks", "backup")
	runApp(t, app, "agents", "defaults", "--worker", "primary,backup", "--planner", "primary", "--coordinator", "primary", "--judge", "backup")
	runApp(t, app, "agents", "supervisor", "--auto-start=true", "--max-workers", "3", "--allow-writes=true")
	runApp(t, app, "agents", "supervisor", "--auto-start=false", "--allow-writes=false")
	runApp(t, app, "agents", "disable", "backup")
	runApp(t, app, "agents", "enable", "backup")

	config, err := agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	primary, found := config.Find("primary")
	if !found || primary.Runtime != model.RuntimeCodex || primary.Command != "/tools/codex" || primary.Model != "gpt-test" || primary.Provider != "openai" || primary.MaxConcurrent != 2 {
		t.Fatalf("primary agent = %#v, found=%t", primary, found)
	}
	if !reflect.DeepEqual(primary.Roles, []agentconfig.Role{agentconfig.RoleWorker, agentconfig.RolePlanner, agentconfig.RoleCoordinator}) || !reflect.DeepEqual(primary.Fallbacks, []string{"backup"}) {
		t.Fatalf("primary routing = %#v", primary)
	}
	backup, found := config.Find("backup")
	if !found || !backup.Enabled {
		t.Fatalf("backup agent = %#v, found=%t", backup, found)
	}
	if config.Supervisor.AutoStart || config.Supervisor.AllowWrites || config.Supervisor.MaxWorkers != 3 {
		t.Fatalf("supervisor = %#v", config.Supervisor)
	}
	if !reflect.DeepEqual(config.Defaults.WorkerAgents, []string{"primary", "backup"}) ||
		!reflect.DeepEqual(config.Defaults.PlannerAgents, []string{"primary"}) ||
		!reflect.DeepEqual(config.Defaults.CoordinatorAgents, []string{"primary"}) ||
		!reflect.DeepEqual(config.Defaults.JudgeAgents, []string{"backup"}) {
		t.Fatalf("defaults = %#v", config.Defaults)
	}

	listOutput := runApp(t, app, "agents", "list")
	if !strings.Contains(listOutput, `"exists": true`) || !strings.Contains(listOutput, `"model": "gpt-test"`) {
		t.Fatalf("list output = %s", listOutput)
	}
	runApp(t, app, "agents", "remove", "backup")
	config, err = agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	primary, _ = config.Find("primary")
	if _, found := config.Find("backup"); found || len(primary.Fallbacks) != 0 || len(config.Defaults.JudgeAgents) != 0 || !reflect.DeepEqual(config.Defaults.WorkerAgents, []string{"primary"}) {
		t.Fatalf("remove did not clear references: %#v", config)
	}
}

type agentDetectionRunner struct {
	paths map[string]string
	calls [][]string
}

func (runner *agentDetectionRunner) LookPath(file string) (string, error) {
	path, found := runner.paths[file]
	if !found {
		return "", errors.New("not found")
	}
	return path, nil
}

func (runner *agentDetectionRunner) Run(_ context.Context, _ string, file string, args ...string) (setupcfg.CommandOutput, error) {
	runner.calls = append(runner.calls, append([]string{file}, args...))
	if strings.HasSuffix(file, "cline") {
		return setupcfg.CommandOutput{Stderr: "cline test version\n"}, errors.New("test version exit")
	}
	return setupcfg.CommandOutput{Stdout: "codex test version\nignored detail\n"}, nil
}

func TestAgentsDetectUsesVersionOnlyAndPreservesExistingSettings(t *testing.T) {
	app, configOptions := newAgentConfigTestApp(t)
	existing := agentconfig.Default()
	existing.Agents = []agentconfig.Agent{{
		ID: "codex", Runtime: model.RuntimeCodex, Command: "/custom/codex", Model: "pinned-model",
		Provider: "custom-provider", Enabled: false, MaxConcurrent: 7, Roles: []agentconfig.Role{agentconfig.RoleJudge},
	}}
	if err := agentconfig.Save(configOptions, existing); err != nil {
		t.Fatal(err)
	}
	runner := &agentDetectionRunner{paths: map[string]string{"codex": "/detected/codex", "cline": "/detected/cline"}}
	app.CommandRunner = runner

	output := runApp(t, app, "agents", "detect", "--save")
	if !strings.Contains(output, `"version": "codex test version"`) || !strings.Contains(output, `"state": "version_unavailable"`) || strings.Count(output, `"saved": true`) != 2 {
		t.Fatalf("detect output = %s", output)
	}
	for _, call := range runner.calls {
		if len(call) != 2 || call[1] != "--version" {
			t.Fatalf("detection made unsafe call: %#v", call)
		}
	}

	config, err := agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	codex, found := config.Find("codex")
	if !found || codex.Command != "/custom/codex" || codex.Model != "pinned-model" || codex.Provider != "custom-provider" || codex.Enabled || codex.MaxConcurrent != 7 || !reflect.DeepEqual(codex.Roles, []agentconfig.Role{agentconfig.RoleJudge}) {
		t.Fatalf("detect overwrote existing agent: %#v", codex)
	}
	cline, found := config.Find("cline")
	wantRoles := []agentconfig.Role{agentconfig.RoleWorker, agentconfig.RolePlanner, agentconfig.RoleCoordinator, agentconfig.RoleJudge}
	if !found || cline.Command != "/detected/cline" || !cline.Enabled || !reflect.DeepEqual(cline.Roles, wantRoles) {
		t.Fatalf("detected cline = %#v, found=%t", cline, found)
	}

	runApp(t, app, "agents", "detect", "--save")
	config, err = agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Agents) != 2 {
		t.Fatalf("repeated detection duplicated agents: %#v", config.Agents)
	}
}

func TestAgentsPresetPreviewsAndAppliesDetectedUnpinnedAgents(t *testing.T) {
	app, configOptions := newAgentConfigTestApp(t)
	runner := &agentDetectionRunner{paths: map[string]string{
		"codex": "/detected/codex", "claude": "/detected/claude",
	}}
	app.CommandRunner = runner

	catalog := runApp(t, app, "agents", "presets")
	for _, id := range []string{"codex", "claude", "codex-claude", "claude-codex"} {
		if !strings.Contains(catalog, `"id": "`+id+`"`) {
			t.Fatalf("preset catalog does not contain %s: %s", id, catalog)
		}
	}

	preview := runApp(t, app, "agents", "preset", "codex-claude")
	if !strings.Contains(preview, `"applied": false`) ||
		!strings.Contains(preview, `"coordinatorAgents":`) ||
		!strings.Contains(preview, `"/detected/codex"`) ||
		!strings.Contains(preview, `"/detected/claude"`) {
		t.Fatalf("preset preview = %s", preview)
	}
	if exists, err := agentconfig.Exists(configOptions); err != nil || exists {
		t.Fatalf("preview wrote config: exists=%t err=%v", exists, err)
	}

	applied := runApp(t, app, "agents", "preset", "codex-claude", "--apply")
	if !strings.Contains(applied, `"applied": true`) {
		t.Fatalf("preset apply = %s", applied)
	}
	config, err := agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := config.Find("codex")
	claude, _ := config.Find("claude")
	wantRoles := []agentconfig.Role{
		agentconfig.RoleWorker, agentconfig.RolePlanner, agentconfig.RoleCoordinator, agentconfig.RoleJudge,
	}
	if codex.Command != "/detected/codex" || claude.Command != "/detected/claude" ||
		codex.Model != "" || claude.Model != "" ||
		!reflect.DeepEqual(codex.Roles, wantRoles) || !reflect.DeepEqual(claude.Roles, wantRoles) {
		t.Fatalf("applied agents: codex=%#v claude=%#v", codex, claude)
	}
	if !reflect.DeepEqual(config.Defaults.CoordinatorAgents, []string{"claude", "codex"}) {
		t.Fatalf("coordinator defaults = %#v", config.Defaults)
	}
	for _, call := range runner.calls {
		if len(call) != 2 || call[1] != "--version" {
			t.Fatalf("preset detection made unsafe call: %#v", call)
		}
	}

	if err := app.Run(context.Background(), []string{"agents", "preset", "codex", "--replace"}); err == nil {
		t.Fatal("--replace without --apply succeeded")
	}
}

func TestAgentsCLIRejectsUnsafeOrInvalidConfiguration(t *testing.T) {
	app, _ := newAgentConfigTestApp(t)
	tests := [][]string{
		{"agents", "set", "manual", "--runtime", "manual"},
		{"agents", "set", "empty-roles", "--runtime", "codex", "--roles="},
		{"agents", "set", "zero", "--runtime", "codex", "--max-concurrent", "0"},
		{"agents", "set", "secret", "--runtime", "codex", "--api-key", "do-not-store"},
		{"agents", "defaults"},
		{"agents", "supervisor", "--max-workers", "0"},
	}
	for _, args := range tests {
		if err := app.Run(context.Background(), args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}

func TestAgentsCommandHelp(t *testing.T) {
	for _, args := range [][]string{{"agents", "--help"}, {"help", "agents"}} {
		app := New(&bytes.Buffer{}, &bytes.Buffer{})
		output := runApp(t, app, args...)
		if !strings.HasPrefix(output, "autogora agents") || !strings.Contains(output, "never sends a prompt") {
			t.Fatalf("agents help output = %q", output)
		}
	}
}
