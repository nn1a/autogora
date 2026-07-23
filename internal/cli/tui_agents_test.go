package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/supervisor"
)

func tuiAgentConfigOptions(t *testing.T) agentconfig.Options {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	return agentconfig.Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}}
}

func waitTUISupervisorStart(t *testing.T, starts <-chan dispatcher.Options) dispatcher.Options {
	t.Helper()
	select {
	case options := <-starts:
		return options
	case <-time.After(2 * time.Second):
		t.Fatal("Supervisor did not start")
		return dispatcher.Options{}
	}
}

func TestTUIGlobalAgentsBackendSavesAppliesAndControlsSupervisor(t *testing.T) {
	configOptions := tuiAgentConfigOptions(t)
	starts := make(chan dispatcher.Options, 4)
	controller := supervisor.New(supervisor.Options{
		Run: func(ctx context.Context, options dispatcher.Options) error {
			starts <- options
			<-ctx.Done()
			return ctx.Err()
		},
	})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = controller.Stop(stop)
	})
	detectCalls := 0
	backend := &tuiGlobalAgentsBackend{
		options: configOptions, controller: controller, parent: context.Background(),
		detect: func(_ context.Context, config agentconfig.Config) ([]agentconfig.Detection, error) {
			detectCalls++
			return []agentconfig.Detection{{
				ID: "codex", Runtime: model.RuntimeCodex,
				Executable: "/fake/codex", State: "installed",
				Configured: len(config.Agents) > 0,
			}}, nil
		},
		activeRuns: func(context.Context) (int, error) { return 2, nil },
	}

	initial, err := backend.LoadGlobalAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial.Exists || initial.Path == "" || initial.ActiveRuns != 2 ||
		initial.Revision != agentconfig.MissingRevision || len(initial.Presets) == 0 {
		t.Fatalf("unexpected initial TUI agent context: %#v", initial)
	}
	detections, err := backend.DetectGlobalAgents(context.Background(), initial.Config)
	if err != nil || detectCalls != 1 || detections[0].Executable != "/fake/codex" {
		t.Fatalf("injected safe detection was not used: calls=%d detections=%#v err=%v",
			detectCalls, detections, err)
	}

	config := agentconfig.Default()
	config.Supervisor = agentconfig.Supervisor{
		AutoStart: true, MaxWorkers: 3, AllowWrites: true,
	}
	saved, err := backend.SaveGlobalAgents(
		context.Background(), initial.Revision, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case options := <-starts:
		t.Fatalf("ordinary save started a stopped Supervisor: %#v", options)
	case <-time.After(50 * time.Millisecond):
	}
	if !saved.Exists || saved.Supervisor.Desired ||
		saved.Revision == agentconfig.MissingRevision {
		t.Fatalf("ordinary save did not preserve stopped intent: %#v", saved)
	}

	started, err := backend.StartSupervisor(
		context.Background(), saved.Revision, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	first := waitTUISupervisorStart(t, starts)
	if !started.Supervisor.Desired || first.MaxWorkers != 3 || !first.AllowWrites {
		t.Fatalf("explicit start did not apply snapshot: started=%#v options=%#v", started, first)
	}

	config.Supervisor.AutoStart = false
	config.Supervisor.MaxWorkers = 5
	config.Supervisor.AllowWrites = false
	reconciled, err := backend.SaveGlobalAgents(
		context.Background(), started.Revision, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	second := waitTUISupervisorStart(t, starts)
	if !reconciled.Supervisor.Desired || second.MaxWorkers != 5 || second.AllowWrites {
		t.Fatalf("ordinary save did not preserve running intent: status=%#v options=%#v",
			reconciled.Supervisor, second)
	}
	loaded, err := agentconfig.Load(configOptions)
	if err != nil || loaded.Supervisor.MaxWorkers != 5 || loaded.Supervisor.AllowWrites {
		t.Fatalf("configuration was not persisted: %#v err=%v", loaded, err)
	}

	stopped, err := backend.StopSupervisor(context.Background())
	if err != nil || stopped.Supervisor.Desired || stopped.Supervisor.Running {
		t.Fatalf("Supervisor stop failed: %#v err=%v", stopped.Supervisor, err)
	}
	config.Supervisor.AutoStart = true
	config.Supervisor.MaxWorkers = 6
	savedStopped, err := backend.SaveGlobalAgents(
		context.Background(), stopped.Revision, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case options := <-starts:
		t.Fatalf("AutoStart overrode current stopped intent on save: %#v", options)
	case <-time.After(50 * time.Millisecond):
	}
	if savedStopped.Supervisor.Desired {
		t.Fatalf("save changed stopped desired state: %#v", savedStopped.Supervisor)
	}
	config.Supervisor.AutoStart = false
	started, err = backend.StartSupervisor(
		context.Background(), savedStopped.Revision, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	manual := waitTUISupervisorStart(t, starts)
	if !started.Supervisor.Desired || manual.MaxWorkers != 6 {
		t.Fatalf("manual start did not use saved draft: status=%#v options=%#v",
			started.Supervisor, manual)
	}
}

func TestTUIGlobalAgentsBackendAppliesProcessWriteOverrideWithoutSavingIt(t *testing.T) {
	configOptions := tuiAgentConfigOptions(t)
	starts := make(chan dispatcher.Options, 3)
	controller := supervisor.New(supervisor.Options{
		Run: func(ctx context.Context, options dispatcher.Options) error {
			starts <- options
			<-ctx.Done()
			return ctx.Err()
		},
	})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = controller.Stop(stop)
	})
	backend := &tuiGlobalAgentsBackend{
		options: configOptions, controller: controller, parent: context.Background(),
		overrideAllowWrites: true, overrideAllowWritesValue: false,
	}
	initial, err := backend.LoadGlobalAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	config := initial.Config
	config.Supervisor.AutoStart = true
	config.Supervisor.AllowWrites = true
	started, err := backend.StartSupervisor(context.Background(), initial.Revision, config)
	if err != nil {
		t.Fatal(err)
	}
	if options := waitTUISupervisorStart(t, starts); options.AllowWrites {
		t.Fatalf("explicit process override did not cap Supervisor writes: %#v", options)
	}
	loaded, err := agentconfig.Load(configOptions)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Supervisor.AllowWrites {
		t.Fatalf("process override leaked into persisted configuration: %#v", loaded.Supervisor)
	}

	config.Supervisor.MaxWorkers = 2
	if _, err := backend.SaveGlobalAgents(
		context.Background(), started.Revision, config,
	); err != nil {
		t.Fatal(err)
	}
	if options := waitTUISupervisorStart(t, starts); options.AllowWrites {
		t.Fatalf("save/reconcile dropped process write override: %#v", options)
	}
}

func TestTUIGlobalAgentsBackendRejectsStaleConfigRevision(t *testing.T) {
	configOptions := tuiAgentConfigOptions(t)
	controller := supervisor.New(supervisor.Options{
		Run: func(ctx context.Context, _ dispatcher.Options) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	backend := &tuiGlobalAgentsBackend{
		options: configOptions, controller: controller, parent: context.Background(),
	}
	opened, err := backend.LoadGlobalAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	external := agentconfig.Default()
	external.Supervisor.MaxWorkers = 9
	if err := agentconfig.Save(configOptions, external); err != nil {
		t.Fatal(err)
	}
	stale := opened.Config
	stale.Supervisor.MaxWorkers = 3
	_, err = backend.SaveGlobalAgents(
		context.Background(), opened.Revision, stale,
	)
	if !errors.Is(err, agentconfig.ErrRevisionConflict) ||
		!strings.Contains(err.Error(), "rerun") {
		t.Fatalf("stale backend save error = %v", err)
	}
	loaded, err := agentconfig.Load(configOptions)
	if err != nil || loaded.Supervisor.MaxWorkers != 9 {
		t.Fatalf("stale backend save overwrote external config: %#v err=%v", loaded, err)
	}
}

func TestTUITargetedDispatchLoadsLatestGlobalConfiguration(t *testing.T) {
	configOptions := tuiAgentConfigOptions(t)
	config := agentconfig.Default()
	config.Supervisor.AllowWrites = false
	config.Agents = []agentconfig.Agent{{
		ID: "worker", Runtime: model.RuntimeCodex, Command: "codex",
		Model: "first-model", Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleWorker},
	}}
	config.Defaults.WorkerAgents = []string{"worker"}
	if err := agentconfig.Save(configOptions, config); err != nil {
		t.Fatal(err)
	}

	var mutex sync.Mutex
	captured := []dispatcher.Options{}
	run := func(_ context.Context, options dispatcher.Options) error {
		mutex.Lock()
		captured = append(captured, options)
		mutex.Unlock()
		return nil
	}
	dispatch := newTUITaskDispatcher(
		run, configOptions, "/tmp/tasks.db", "/bin/autogora", "default",
		"/workspace", func(string) string { return "" }, false, false,
	)
	if err := dispatch(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	config.Agents[0].Model = "second-model"
	config.Supervisor.AllowWrites = true
	if err := agentconfig.Save(configOptions, config); err != nil {
		t.Fatal(err)
	}
	if err := dispatch(context.Background(), "task-2"); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 2 ||
		captured[0].AgentConfig == nil || captured[1].AgentConfig == nil ||
		captured[0].AgentConfig.Agents[0].Model != "first-model" ||
		captured[1].AgentConfig.Agents[0].Model != "second-model" ||
		captured[0].AllowWrites || !captured[1].AllowWrites {
		t.Fatalf("targeted dispatch did not reload the latest config: %#v", captured)
	}

	override := newTUITaskDispatcher(
		run, configOptions, "/tmp/tasks.db", "/bin/autogora", "default",
		"/workspace", func(string) string { return "" }, true, false,
	)
	if err := override(context.Background(), "task-3"); err != nil {
		t.Fatal(err)
	}
	if captured[2].AllowWrites {
		t.Fatal("explicit TUI --allow-writes=false did not remain an upper-bound override")
	}
}
