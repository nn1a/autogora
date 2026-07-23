package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/dispatcher"
)

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

func TestControllerReportsRunnerFailure(t *testing.T) {
	controller := New(Options{Run: func(context.Context, dispatcher.Options) error { return errors.New("lease unavailable") }})
	controller.Start(context.Background(), agentconfig.Default())
	deadline := time.Now().Add(time.Second)
	for controller.Status().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := controller.Status()
	if status.Running || status.LastError != "lease unavailable" {
		t.Fatalf("runner failure status = %#v", status)
	}
}
