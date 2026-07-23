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
	DBPath  string
	CLIPath string
	OnLog   func(string)
	Run     RunFunc
}

type Status struct {
	Running     bool   `json:"running"`
	Desired     bool   `json:"desired"`
	MaxWorkers  int    `json:"maxWorkers"`
	AllowWrites bool   `json:"allowWrites"`
	StartedAt   string `json:"startedAt,omitempty"`
	StoppedAt   string `json:"stoppedAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

type Controller struct {
	mu         sync.Mutex
	options    Options
	cancel     context.CancelFunc
	done       chan struct{}
	generation uint64
	status     Status
}

func New(options Options) *Controller {
	if options.Run == nil {
		options.Run = dispatcher.Run
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

func (c *Controller) Start(parent context.Context, config agentconfig.Config) bool {
	config = agentconfig.Normalize(config)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.Running {
		c.status.Desired = true
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
	go func() {
		err := c.options.Run(ctx, dispatcher.Options{
			DBPath: c.options.DBPath, CLIPath: c.options.CLIPath,
			MaxWorkers: config.Supervisor.MaxWorkers, AllowWrites: config.Supervisor.AllowWrites,
			AgentConfig: &config, OnLog: c.options.OnLog,
		})
		c.mu.Lock()
		if c.generation == generation {
			c.status.Running = false
			c.status.StoppedAt = timestamp()
			if err != nil && !errors.Is(err, context.Canceled) {
				c.status.LastError = err.Error()
			}
			c.cancel = nil
		}
		close(done)
		c.mu.Unlock()
	}()
	return true
}

func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.status.Desired = false
	if !c.status.Running || c.cancel == nil {
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
	if err := c.Stop(ctx); err != nil {
		return err
	}
	if config.Supervisor.AutoStart {
		c.Start(parent, config)
	}
	return nil
}
