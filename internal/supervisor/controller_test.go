package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/dispatcher"
)

func waitForStatus(t *testing.T, controller *Controller, match func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := controller.Status()
		if match(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status := controller.Status()
	t.Fatalf("timed out waiting for supervisor status: %#v", status)
	return status
}

func receiveAttempt(t *testing.T, attempts <-chan int) int {
	t.Helper()
	select {
	case attempt := <-attempts:
		return attempt
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatcher attempt")
		return 0
	}
}

func TestControllerStartsAppliesAndStopsSupervisorSettings(t *testing.T) {
	var mu sync.Mutex
	runs := make([]dispatcher.Options, 0, 2)
	started := make(chan struct{}, 2)
	controller := New(Options{DBPath: "/tmp/autogora.db", CLIPath: "/tmp/autogora", Run: func(ctx context.Context, options dispatcher.Options) error {
		mu.Lock()
		runs = append(runs, options)
		mu.Unlock()
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}})
	config := agentconfig.Default()
	config.Supervisor = agentconfig.Supervisor{AutoStart: true, MaxWorkers: 2}
	if !controller.Start(context.Background(), config) {
		t.Fatal("supervisor did not start")
	}
	<-started
	if controller.Start(context.Background(), config) {
		t.Fatal("duplicate supervisor start was accepted")
	}
	config.Supervisor.MaxWorkers = 4
	config.Supervisor.AllowWrites = true
	applyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Apply(applyCtx, context.Background(), config); err != nil {
		t.Fatal(err)
	}
	<-started
	status := controller.Status()
	if !status.Running || !status.Desired || status.MaxWorkers != 4 || !status.AllowWrites {
		t.Fatalf("unexpected running status: %#v", status)
	}
	if err := controller.Stop(applyCtx); err != nil {
		t.Fatal(err)
	}
	status = controller.Status()
	if status.Running || status.Desired || status.StoppedAt == "" {
		t.Fatalf("unexpected stopped status: %#v", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 || runs[0].MaxWorkers != 2 || runs[1].MaxWorkers != 4 || !runs[1].AllowWrites || runs[1].AgentConfig == nil {
		t.Fatalf("dispatcher options = %#v", runs)
	}
}

func TestControllerPassesLiveAgentConfigLoaderAlongsideStartSnapshot(t *testing.T) {
	started := make(chan dispatcher.Options, 1)
	live := agentconfig.Default()
	live.Agents = []agentconfig.Agent{{
		ID: "coordinator", Runtime: "codex", Command: "codex",
		Model: "first-model", Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
	}}
	live.Defaults.CoordinatorAgents = []string{"coordinator"}
	var mu sync.Mutex
	controller := New(Options{
		AgentConfigLoader: func() (agentconfig.Config, error) {
			mu.Lock()
			defer mu.Unlock()
			return agentconfig.Normalize(live), nil
		},
		Run: func(ctx context.Context, options dispatcher.Options) error {
			started <- options
			<-ctx.Done()
			return ctx.Err()
		},
	})
	snapshot := agentconfig.Normalize(live)
	if !controller.Start(context.Background(), snapshot) {
		t.Fatal("supervisor did not start")
	}
	var options dispatcher.Options
	select {
	case options = <-started:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not pass dispatcher options")
	}
	if options.AgentConfig == nil || options.AgentConfigLoader == nil ||
		options.AgentConfig.Agents[0].Model != "first-model" {
		t.Fatalf("dispatcher did not receive snapshot and live loader: %#v", options)
	}
	mu.Lock()
	next := agentconfig.Normalize(live)
	next.Agents = append([]agentconfig.Agent(nil), next.Agents...)
	next.Agents[0].Roles = append([]agentconfig.Role(nil), next.Agents[0].Roles...)
	next.Agents[0].Model = "second-model"
	live = next
	mu.Unlock()
	reloaded, err := options.AgentConfigLoader()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Agents[0].Model != "second-model" ||
		options.AgentConfig.Agents[0].Model != "first-model" {
		t.Fatalf(
			"dispatcher loader semantics = snapshot:%#v live:%#v",
			options.AgentConfig,
			reloaded,
		)
	}
	stop, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Stop(stop); err != nil {
		t.Fatal(err)
	}
}

func TestControllerReconcileUsesExplicitDesiredStateInsteadOfAutoStart(t *testing.T) {
	started := make(chan dispatcher.Options, 2)
	controller := New(Options{Run: func(ctx context.Context, options dispatcher.Options) error {
		started <- options
		<-ctx.Done()
		return ctx.Err()
	}})
	config := agentconfig.Default()
	config.Supervisor = agentconfig.Supervisor{
		AutoStart: true, MaxWorkers: 2, AllowWrites: true,
	}
	apply, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Reconcile(
		apply, context.Background(), config, false,
	); err != nil {
		t.Fatal(err)
	}
	if status := controller.Status(); status.Desired || status.Running {
		t.Fatalf("stopped intent was overridden by AutoStart: %#v", status)
	}
	config.Supervisor.AutoStart = false
	config.Supervisor.MaxWorkers = 4
	if err := controller.Reconcile(
		apply, context.Background(), config, true,
	); err != nil {
		t.Fatal(err)
	}
	select {
	case options := <-started:
		if options.MaxWorkers != 4 || !options.AllowWrites {
			t.Fatalf("explicit desired start used stale options: %#v", options)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit desired start did not run")
	}
	if status := controller.Status(); !status.Desired || !status.Running {
		t.Fatalf("AutoStart=false overrode running intent: %#v", status)
	}
	if err := controller.Stop(apply); err != nil {
		t.Fatal(err)
	}
}

func TestControllerRestartsAfterTwoFailures(t *testing.T) {
	var count atomic.Int32
	attempts := make(chan int, 3)
	controller := New(Options{
		RestartMinDelay: 5 * time.Millisecond,
		RestartMaxDelay: 20 * time.Millisecond,
		StableRunWindow: time.Second,
		Run: func(ctx context.Context, _ dispatcher.Options) error {
			attempt := int(count.Add(1))
			attempts <- attempt
			if attempt <= 2 {
				return errors.New("lease unavailable")
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if !controller.Start(context.Background(), agentconfig.Default()) {
		t.Fatal("supervisor did not start")
	}
	for expected := 1; expected <= 3; expected++ {
		if attempt := receiveAttempt(t, attempts); attempt != expected {
			t.Fatalf("dispatcher attempt = %d, want %d", attempt, expected)
		}
	}
	status := waitForStatus(t, controller, func(status Status) bool {
		return status.Running && status.RestartCount == 2
	})
	if !status.Desired || status.NextAttemptAt != "" || status.LastError != "lease unavailable" {
		t.Fatalf("unexpected recovered status: %#v", status)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestControllerStopInterruptsRestartBackoff(t *testing.T) {
	attempts := make(chan int, 2)
	controller := New(Options{
		RestartMinDelay: 2 * time.Second,
		RestartMaxDelay: 2 * time.Second,
		Run: func(context.Context, dispatcher.Options) error {
			attempts <- 1
			return errors.New("temporary dispatcher failure")
		},
	})
	controller.Start(context.Background(), agentconfig.Default())
	receiveAttempt(t, attempts)
	status := waitForStatus(t, controller, func(status Status) bool {
		return status.Desired && !status.Running && status.NextAttemptAt != ""
	})
	if status.RestartCount != 1 || status.LastError == "" {
		t.Fatalf("backoff status = %#v", status)
	}

	started := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("stop waited for the backoff timer: %v", elapsed)
	}
	select {
	case <-attempts:
		t.Fatal("dispatcher restarted after supervisor stop")
	case <-time.After(30 * time.Millisecond):
	}
	status = controller.Status()
	if status.Running || status.Desired || status.NextAttemptAt != "" || status.StoppedAt == "" {
		t.Fatalf("stopped status = %#v", status)
	}
}

func TestControllerParentCancellationInterruptsRestartBackoff(t *testing.T) {
	attempted := make(chan int, 1)
	controller := New(Options{
		RestartMinDelay: 2 * time.Second,
		RestartMaxDelay: 2 * time.Second,
		Run: func(context.Context, dispatcher.Options) error {
			attempted <- 1
			return errors.New("temporary dispatcher failure")
		},
	})
	parent, cancel := context.WithCancel(context.Background())
	controller.Start(parent, agentconfig.Default())
	receiveAttempt(t, attempted)
	waitForStatus(t, controller, func(status Status) bool { return status.NextAttemptAt != "" })
	cancelledAt := time.Now()
	cancel()
	status := waitForStatus(t, controller, func(status Status) bool {
		return !status.Running && !status.Desired && status.StoppedAt != ""
	})
	if elapsed := time.Since(cancelledAt); elapsed > 250*time.Millisecond {
		t.Fatalf("parent cancellation waited for the backoff timer: %v", elapsed)
	}
	if status.NextAttemptAt != "" || status.RestartCount != 1 {
		t.Fatalf("cancelled status = %#v", status)
	}
}

func TestControllerApplyIsolatesRestartGeneration(t *testing.T) {
	attempts := make(chan int, 4)
	controller := New(Options{
		RestartMinDelay: 150 * time.Millisecond,
		RestartMaxDelay: 150 * time.Millisecond,
		Run: func(ctx context.Context, options dispatcher.Options) error {
			attempts <- options.MaxWorkers
			if options.MaxWorkers == 2 {
				return errors.New("old generation failed")
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	oldConfig := agentconfig.Default()
	oldConfig.Supervisor = agentconfig.Supervisor{AutoStart: true, MaxWorkers: 2}
	controller.Start(context.Background(), oldConfig)
	if workers := receiveAttempt(t, attempts); workers != 2 {
		t.Fatalf("old generation workers = %d", workers)
	}
	waitForStatus(t, controller, func(status Status) bool { return status.NextAttemptAt != "" })

	newConfig := oldConfig
	newConfig.Supervisor.MaxWorkers = 4
	applyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Apply(applyCtx, context.Background(), newConfig); err != nil {
		t.Fatal(err)
	}
	if workers := receiveAttempt(t, attempts); workers != 4 {
		t.Fatalf("new generation workers = %d", workers)
	}
	select {
	case workers := <-attempts:
		t.Fatalf("stale generation restarted with %d workers", workers)
	case <-time.After(200 * time.Millisecond):
	}
	status := controller.Status()
	if !status.Running || !status.Desired || status.MaxWorkers != 4 || status.RestartCount != 0 || status.LastError != "" || status.NextAttemptAt != "" {
		t.Fatalf("new generation status = %#v", status)
	}
	if err := controller.Stop(applyCtx); err != nil {
		t.Fatal(err)
	}
}

func TestControllerStableRunRelaxesBackoff(t *testing.T) {
	var count atomic.Int32
	attempts := make(chan int, 3)
	releaseSecond := make(chan struct{})
	secondFailedAt := make(chan time.Time, 1)
	controller := New(Options{
		RestartMinDelay: 80 * time.Millisecond,
		RestartMaxDelay: time.Second,
		StableRunWindow: 15 * time.Millisecond,
		Run: func(ctx context.Context, _ dispatcher.Options) error {
			attempt := int(count.Add(1))
			attempts <- attempt
			switch attempt {
			case 1:
				return errors.New("first failure")
			case 2:
				select {
				case <-releaseSecond:
					secondFailedAt <- time.Now()
					return errors.New("failure after stable run")
				case <-ctx.Done():
					return ctx.Err()
				}
			default:
				<-ctx.Done()
				return ctx.Err()
			}
		},
	})
	controller.Start(context.Background(), agentconfig.Default())
	receiveAttempt(t, attempts)
	if attempt := receiveAttempt(t, attempts); attempt != 2 {
		t.Fatalf("second dispatcher attempt = %d", attempt)
	}
	time.Sleep(25 * time.Millisecond)
	close(releaseSecond)
	failedAt := <-secondFailedAt
	status := waitForStatus(t, controller, func(status Status) bool {
		return status.RestartCount == 2 && status.NextAttemptAt != ""
	})
	nextAttempt, err := time.Parse(time.RFC3339Nano, status.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	if delay := nextAttempt.Sub(failedAt); delay < 70*time.Millisecond || delay > 120*time.Millisecond {
		t.Fatalf("stable run did not reset backoff, next delay = %v", delay)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
