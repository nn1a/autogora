package processguard

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

var ErrTeardownUnconfirmed = errors.New("guarded process teardown is unconfirmed")

// The parent wait margin must remain strictly longer than the Linux guard's
// five-second cleanup deadline. This lets the guard exit with a positive or
// negative attestation before a bounded caller returns.
const teardownConfirmationLimit = 7 * time.Second

type teardownFailureReporterKey struct{}

// WithTeardownFailureReporter installs a fail-closed callback for callers that
// own durable run state. The callback runs before an unconfirmed teardown is
// returned, so later cleanup cannot accidentally acknowledge the run after
// swallowing a nested command error.
func WithTeardownFailureReporter(
	ctx context.Context,
	reporter func(error),
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, teardownFailureReporterKey{}, reporter)
}

func teardownFailureReporter(ctx context.Context) func(error) {
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(teardownFailureReporterKey{}).(func(error))
	return reporter
}

// ReportTeardownFailure forwards a containment error discovered by a caller
// that applies an additional wait bound around a guarded command.
func ReportTeardownFailure(ctx context.Context, err error) {
	if !errors.Is(err, ErrTeardownUnconfirmed) {
		return
	}
	if reporter := teardownFailureReporter(ctx); reporter != nil {
		reporter(err)
	}
}

type teardownProof interface {
	afterStart() error
	confirm() error
	close()
}

// Command is an exec.Cmd whose context is bounded and whose platform-specific
// process guard must finish tearing down descendants before Wait returns.
type Command struct {
	*exec.Cmd
	cancel  context.CancelFunc
	context context.Context
	proof   teardownProof
	report  func(error)
}

// NewCommandContext wraps a command in the strongest process containment
// primitive available on the current platform. maximum is also applied when
// the caller supplied an unbounded context.
func NewCommandContext(
	ctx context.Context,
	maximum time.Duration,
	name string,
	args ...string,
) *Command {
	bounded, cancel := boundedContext(ctx, maximum)
	command, proof := newGuardedCommandContext(bounded, name, args...)
	return &Command{
		Cmd:     command,
		cancel:  cancel,
		context: bounded,
		proof:   proof,
		report:  teardownFailureReporter(ctx),
	}
}

func boundedContext(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maximum <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, maximum)
}

func (c *Command) Start() error {
	if err := c.Cmd.Start(); err != nil {
		c.proof.close()
		c.cancel()
		return err
	}
	if err := c.proof.afterStart(); err != nil {
		_ = c.Cmd.Process.Kill()
		c.proof.close()
		c.cancel()
		return c.reportUnconfirmed(errors.Join(ErrTeardownUnconfirmed, err))
	}
	return nil
}

func (c *Command) Wait() error {
	defer c.cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- c.Cmd.Wait()
	}()
	select {
	case err := <-waited:
		return c.reportUnconfirmed(errors.Join(err, c.proof.confirm()))
	case <-c.context.Done():
		select {
		case err := <-waited:
			return c.reportUnconfirmed(errors.Join(err, c.proof.confirm()))
		case <-time.After(teardownConfirmationLimit):
			c.proof.close()
			return c.reportUnconfirmed(errors.Join(c.context.Err(), ErrTeardownUnconfirmed))
		}
	}
}

func (c *Command) reportUnconfirmed(err error) error {
	if errors.Is(err, ErrTeardownUnconfirmed) && c.report != nil {
		c.report(err)
	}
	return err
}

func (c *Command) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Command) Output() ([]byte, error) {
	if c.Cmd.Stdout != nil {
		return nil, errors.New("processguard: Stdout already set")
	}
	var stdout bytes.Buffer
	c.Cmd.Stdout = &stdout
	err := c.Run()
	return stdout.Bytes(), err
}

func (c *Command) CombinedOutput() ([]byte, error) {
	if c.Cmd.Stdout != nil || c.Cmd.Stderr != nil {
		return nil, errors.New("processguard: Stdout or Stderr already set")
	}
	var output bytes.Buffer
	c.Cmd.Stdout = &output
	c.Cmd.Stderr = &output
	err := c.Run()
	return output.Bytes(), err
}

// Close releases the command deadline after a caller abandons a command
// before Start or Wait.
func (c *Command) Close() {
	c.proof.close()
	c.cancel()
}
