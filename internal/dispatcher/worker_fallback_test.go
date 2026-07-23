package dispatcher

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/agenthealth"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func workerFallbackAgent(id string, enabled bool, fallbacks ...string) agentconfig.Agent {
	return agentconfig.Agent{
		ID: id, Runtime: model.RuntimeCodex, Command: "/bin/true",
		Enabled: enabled, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleWorker}, Fallbacks: fallbacks,
	}
}

func workerFallbackProfiles(agents ...agentconfig.Agent) []orchestration.ProfileRoute {
	profiles := make([]orchestration.ProfileRoute, 0, len(agents))
	for _, agent := range agents {
		profiles = append(profiles, orchestration.ProfileRoute{
			Name: agent.ID, Runtime: agent.Runtime, Disabled: !agent.Enabled,
			MaxConcurrent: agent.MaxConcurrent, Fallbacks: append([]string(nil), agent.Fallbacks...),
		})
	}
	return profiles
}

func workerFallbackProfileIndex(profiles ...orchestration.ProfileRoute) map[string]orchestration.ProfileRoute {
	result := make(map[string]orchestration.ProfileRoute, len(profiles))
	for _, profile := range profiles {
		result[profile.Name] = profile
	}
	return result
}

func setWorkerUnavailable(t *testing.T, opened *store.Store, id string) {
	t.Helper()
	cooldown := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := opened.SetAgentHealth(context.Background(), store.SetAgentHealthInput{
		AgentID: id, Status: model.AgentHealthRateLimited, CooldownUntil: &cooldown,
	}); err != nil {
		t.Fatalf("set %s unavailable: %v", id, err)
	}
}

func occupyWorkerProfile(t *testing.T, opened *store.Store, id string) {
	t.Helper()
	assignee := id
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "occupy " + id, Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim capacity fixture: claim=%+v err=%v", claim, err)
	}
	if _, err := opened.RecordRunAgentConfig(
		context.Background(),
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		store.RecordRunAgentConfigInput{
			Profile: id, Runtime: model.RuntimeCodex, Source: "test_fixture",
		},
	); err != nil {
		t.Fatalf("record capacity fixture: %v", err)
	}
}

func TestResolveRunProfileUsesWorkerDefaultsAsImplicitFallbacks(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	claude := workerFallbackAgent("claude", true)
	claude.Runtime, claude.Command, claude.Model = model.RuntimeClaude, "/bin/claude", "claude-model"
	codex := workerFallbackAgent("codex", true)
	codex.Command, codex.Model = "/bin/codex", "codex-model"
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 2},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"codex", "claude"}},
		Agents:        []agentconfig.Agent{claude, codex},
	}
	setWorkerUnavailable(t, opened, "claude")
	assignee := "claude"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "fall back through worker roster", Assignee: &assignee, Runtime: model.RuntimeClaude,
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveRunProfile(ctx, manager, opened, task.Task, Options{
		AgentConfig: &config, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "codex" || resolved.Runtime != model.RuntimeCodex ||
		resolved.Command != "/bin/codex" || resolved.Model != "codex-model" ||
		resolved.Source != "fallback" || resolved.FallbackFrom == nil ||
		*resolved.FallbackFrom != "claude" || !resolved.GlobalRegistered {
		t.Fatalf("implicit worker fallback was not resolved and audited: %#v", resolved)
	}
}

func TestOrderedWorkerProfileCandidatesUsesBoundedFallbackTiers(t *testing.T) {
	config := agentconfig.Config{
		Defaults: agentconfig.Defaults{WorkerAgents: []string{"default-x", "default-y"}},
		Agents: []agentconfig.Agent{
			workerFallbackAgent("registry-first", true),
			workerFallbackAgent("default-x", true),
			workerFallbackAgent("disabled", false),
			{ID: "planner-only", Enabled: true, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
			workerFallbackAgent("default-y", true),
			workerFallbackAgent("registry-last", true),
		},
	}
	byName := workerFallbackProfileIndex(
		orchestration.ProfileRoute{Name: "requested", Runtime: model.RuntimeCodex, Fallbacks: []string{"explicit-b", "explicit-c"}},
		orchestration.ProfileRoute{Name: "explicit-b", Runtime: model.RuntimeCodex, Fallbacks: []string{"explicit-d"}},
		orchestration.ProfileRoute{Name: "explicit-c", Runtime: model.RuntimeCodex, Fallbacks: []string{"explicit-e"}},
		orchestration.ProfileRoute{Name: "explicit-d", Runtime: model.RuntimeCodex},
		// This malformed board-only edge proves a cycle cannot expand the queue.
		orchestration.ProfileRoute{Name: "explicit-e", Runtime: model.RuntimeCodex, Fallbacks: []string{"requested"}},
	)

	got := orderedWorkerProfileCandidates(" requested ", byName, config)
	want := []string{
		"requested", "explicit-b", "explicit-c", "explicit-d", "explicit-e",
		"default-x", "default-y", "registry-first", "registry-last",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate order = %#v, want %#v", got, want)
	}
}

func TestSelectAvailableProfileHonorsExplicitThenDefaultPrecedence(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	remaining := workerFallbackAgent("registry-first", true)
	defaultAgent := workerFallbackAgent("default", true)
	explicit := workerFallbackAgent("explicit", true)
	requested := workerFallbackAgent("requested", true, "explicit")
	config := agentconfig.Config{
		Defaults: agentconfig.Defaults{WorkerAgents: []string{"default"}},
		Agents:   []agentconfig.Agent{remaining, defaultAgent, explicit, requested},
	}
	profiles := workerFallbackProfiles(remaining, defaultAgent, explicit, requested)
	setWorkerUnavailable(t, opened, "requested")

	selected, available, err := selectAvailableProfile(
		ctx, agenthealth.New(manager, opened), opened, "requested", profiles, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !available || selected.Name != "explicit" {
		t.Fatalf("selected=%#v available=%t, want explicit fallback", selected, available)
	}

	setWorkerUnavailable(t, opened, "explicit")
	selected, available, err = selectAvailableProfile(
		ctx, agenthealth.New(manager, opened), opened, "requested", profiles, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !available || selected.Name != "default" {
		t.Fatalf("selected=%#v available=%t, want default before registry order", selected, available)
	}
}

func TestSelectAvailableProfileSkipsDisabledUnhealthyAndFullCandidates(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	requested := workerFallbackAgent("requested", true, "unhealthy")
	unhealthy := workerFallbackAgent("unhealthy", true)
	full := workerFallbackAgent("full-default", true)
	disabled := workerFallbackAgent("disabled-roster", false)
	remaining := workerFallbackAgent("remaining", true)
	config := agentconfig.Config{
		Defaults: agentconfig.Defaults{WorkerAgents: []string{"full-default"}},
		Agents:   []agentconfig.Agent{requested, unhealthy, full, disabled, remaining},
	}
	profiles := workerFallbackProfiles(requested, unhealthy, full, disabled, remaining)
	profiles[0].Disabled = true
	setWorkerUnavailable(t, opened, "unhealthy")
	occupyWorkerProfile(t, opened, "full-default")

	selected, available, err := selectAvailableProfile(
		ctx, agenthealth.New(manager, opened), opened, "requested", profiles, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !available || selected.Name != "remaining" {
		t.Fatalf("selected=%#v available=%t, want remaining roster worker", selected, available)
	}
}

func TestSelectAvailableProfileReturnsNoCandidate(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	selected, available, err := selectAvailableProfile(
		ctx,
		agenthealth.New(manager, opened),
		opened,
		"missing",
		nil,
		agentconfig.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if available || selected.Name != "" {
		t.Fatalf("selected=%#v available=%t, want no candidate", selected, available)
	}
}

func TestClaimProfilePolicyKeepsRequestedProfileWhenAlternateWorkerIsAvailable(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	claude := workerFallbackAgent("claude", true)
	claude.Runtime = model.RuntimeClaude
	codex := workerFallbackAgent("codex", true)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 2},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"codex", "claude"}},
		Agents:        []agentconfig.Agent{claude, codex},
	}
	setWorkerUnavailable(t, opened, "claude")

	excluded, limits, err := claimProfilePolicy(
		ctx, manager, opened, "default",
		Options{AgentConfig: &config, Getenv: func(string) string { return "" }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded) != 0 {
		t.Fatalf("profiles were excluded despite an alternate worker: %#v", excluded)
	}
	if !reflect.DeepEqual(limits, map[string]int{"claude": 1, "codex": 1}) {
		t.Fatalf("profile limits changed unexpectedly: %#v", limits)
	}
}
