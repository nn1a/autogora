package supervisor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/dispatcher"
)

type RunFunc func(context.Context, dispatcher.Options) error

type Options struct {
	DBPath            string
	CLIPath           string
	WorkingDirectory  string
	Getenv            func(string) string
	AgentConfigLoader dispatcher.AgentConfigLoader
	OnLog             func(string)
	Run               RunFunc
	RestartMinDelay   time.Duration
	RestartMaxDelay   time.Duration
	StableRunWindow   time.Duration
}

type Status struct {
	Running       bool   `json:"running"`
	Desired       bool   `json:"desired"`
	MaxWorkers    int    `json:"maxWorkers"`
	AllowWrites   bool   `json:"allowWrites"`
	RestartCount  int    `json:"restartCount"`
	StartedAt     string `json:"startedAt,omitempty"`
	StoppedAt     string `json:"stoppedAt,omitempty"`
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

type Controller struct {
	mu         sync.Mutex
	options    Options
	cancel     context.CancelFunc
	done       chan struct{}
	generation uint64
	status     Status
}

const (
	defaultRestartMinDelay = 500 * time.Millisecond
	defaultRestartMaxDelay = 30 * time.Second
	defaultStableRunWindow = 30 * time.Second
)

var errUnexpectedDispatcherExit = errors.New("dispatcher exited unexpectedly without an error")

func New(options Options) *Controller {
	if options.Run == nil {
		options.Run = dispatcher.Run
	}
	if options.RestartMinDelay <= 0 {
		options.RestartMinDelay = defaultRestartMinDelay
	}
	if options.RestartMaxDelay <= 0 {
		options.RestartMaxDelay = defaultRestartMaxDelay
	}
	if options.RestartMaxDelay < options.RestartMinDelay {
		options.RestartMaxDelay = options.RestartMinDelay
	}
	if options.StableRunWindow <= 0 {
		options.StableRunWindow = defaultStableRunWindow
	}
	return &Controller{options: options}
}

func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Controller) dispatcherOptions(config agentconfig.Config) dispatcher.Options {
	return dispatcher.Options{
		DBPath: c.options.DBPath, CLIPath: c.options.CLIPath,
		MaxWorkers: config.Supervisor.MaxWorkers, AllowWrites: config.Supervisor.AllowWrites,
		Autopilot:   true,
		AgentConfig: &config, AgentConfigLoader: c.options.AgentConfigLoader,
		WorkingDirectory: c.options.WorkingDirectory,
		Getenv:           c.options.Getenv, OnLog: c.options.OnLog,
	}
}

func (c *Controller) restartDelay(failures int) time.Duration {
	delay := c.options.RestartMinDelay
	for range failures {
		if delay >= c.options.RestartMaxDelay || delay > c.options.RestartMaxDelay/2 {
			return c.options.RestartMaxDelay
		}
		delay *= 2
	}
	return min(delay, c.options.RestartMaxDelay)
}

func (c *Controller) markRunning(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation || !c.status.Desired || c.cancel == nil {
		return false
	}
	c.status.Running = true
	c.status.NextAttemptAt = ""
	return true
}

func (c *Controller) markBackoff(generation uint64, restartCount int, err error, next time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation || !c.status.Desired || c.cancel == nil {
		return false
	}
	c.status.Running = false
	c.status.RestartCount = restartCount
	c.status.NextAttemptAt = next.UTC().Format(time.RFC3339Nano)
	c.status.LastError = err.Error()
	return true
}

func (c *Controller) finishGeneration(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return
	}
	c.status.Running = false
	c.status.Desired = false
	c.status.NextAttemptAt = ""
	c.status.StoppedAt = timestamp()
	c.cancel, c.done = nil, nil
}

func (c *Controller) runGeneration(ctx context.Context, generation uint64, config agentconfig.Config, done chan struct{}) {
	defer close(done)
	failures, restartCount := 0, 0
	options := c.dispatcherOptions(config)
	for {
		if ctx.Err() != nil || !c.markRunning(generation) {
			c.finishGeneration(generation)
			return
		}
		started := time.Now()
		err := c.options.Run(ctx, options)
		runDuration := time.Since(started)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			c.finishGeneration(generation)
			return
		}
		if err == nil {
			err = errUnexpectedDispatcherExit
		}
		if runDuration >= c.options.StableRunWindow {
			failures = 0
		}
		delay := c.restartDelay(failures)
		failures++
		restartCount++
		if !c.markBackoff(generation, restartCount, err, time.Now().Add(delay)) {
			c.finishGeneration(generation)
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			c.finishGeneration(generation)
			return
		case <-timer.C:
		}
	}
}

func (c *Controller) Start(parent context.Context, config agentconfig.Config) bool {
	config = agentconfig.Normalize(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return false
	}
	c.generation++
	generation := c.generation
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	c.cancel, c.done = cancel, done
	c.status = Status{
		Running: true, Desired: true, MaxWorkers: config.Supervisor.MaxWorkers,
		AllowWrites: config.Supervisor.AllowWrites, StartedAt: timestamp(),
	}
	go c.runGeneration(ctx, generation, config, done)
	return true
}

func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.status.Desired = false
	c.status.NextAttemptAt = ""
	if c.cancel == nil {
		c.mu.Unlock()
		return nil
	}
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) Apply(ctx context.Context, parent context.Context, config agentconfig.Config) error {
	return c.Reconcile(ctx, parent, config, config.Supervisor.AutoStart)
}

// Reconcile applies a new dispatcher snapshot while preserving an explicit
// process-level desired state supplied by the caller. AutoStart is a policy
// for future UI sessions and does not implicitly override current manual
// start/stop intent.
func (c *Controller) Reconcile(
	ctx context.Context,
	parent context.Context,
	config agentconfig.Config,
	desired bool,
) error {
	if err := c.Stop(ctx); err != nil {
		return err
	}
	if desired {
		c.Start(parent, config)
	}
	return nil
}
