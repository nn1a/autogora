package processguard

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

var (
	ErrTeardownUnconfirmed = errors.New(
		"guarded process teardown is unconfirmed",
	)
	ErrCommandNotStarted = errors.New(
		"guarded command has not started",
	)
	ErrCommandAlreadyStarted = errors.New(
		"guarded command has already started",
	)
	ErrCommandClosed = errors.New(
		"guarded command is closed",
	)
)

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
	parent := teardownFailureReporter(ctx)
	if parent == nil {
		return context.WithValue(ctx, teardownFailureReporterKey{}, reporter)
	}
	// The outer reporter owns the broader safety boundary. In the dispatcher
	// that means persisting the global automation quarantine before a nested
	// managed-run reporter records its local recovery fence.
	combined := func(err error) {
		parent(err)
		reporter(err)
	}
	return context.WithValue(ctx, teardownFailureReporterKey{}, combined)
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

type commandState uint8

const (
	commandCreated commandState = iota
	commandStarted
	commandFinished
)

// Command is an exec.Cmd whose context is bounded and whose platform-specific
// process guard must finish tearing down descendants before Wait returns.
type Command struct {
	*exec.Cmd
	mu           sync.Mutex
	cancel       context.CancelFunc
	launchCancel context.CancelFunc
	context      context.Context
	proof        teardownProof
	report       func(error)
	startErr     error

	state                     commandState
	closed                    bool
	lifecycleErr              error
	processWait               <-chan error
	waitStarted               bool
	waitDone                  chan struct{}
	waitComplete              bool
	waitErr                   error
	teardownConfirmationDelay time.Duration
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
	bounded, boundedCancel := boundedContext(ctx, maximum)
	launchContext, launchCancel := context.WithCancel(context.Background())
	cancel := func() {
		boundedCancel()
		launchCancel()
	}
	command, proof, startErr := newGuardedCommandContext(
		launchContext,
		name,
		args...,
	)
	return &Command{
		Cmd:          command,
		cancel:       cancel,
		launchCancel: launchCancel,
		context:      bounded,
		proof:        proof,
		report:       teardownFailureReporter(ctx),
		startErr:     startErr,
		waitDone:     make(chan struct{}),
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrCommandClosed
	}
	if c.state != commandCreated {
		c.mu.Unlock()
		return ErrCommandAlreadyStarted
	}
	if c.startErr != nil {
		c.proof.close()
		c.cancel()
		c.state = commandFinished
		c.finishWaitLocked(c.startErr)
		err := c.startErr
		c.mu.Unlock()
		return err
	}
	if contextErr := c.context.Err(); contextErr != nil {
		c.proof.close()
		c.cancel()
		c.state = commandFinished
		c.finishWaitLocked(contextErr)
		c.mu.Unlock()
		return contextErr
	}
	processWait, err := startOwnedProcess(c.Cmd)
	if err != nil {
		c.proof.close()
		c.cancel()
		c.state = commandFinished
		c.finishWaitLocked(err)
		c.mu.Unlock()
		return err
	}
	c.processWait = processWait
	c.state = commandStarted
	if err := c.proof.afterStart(); err != nil {
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			err,
		)
		c.cancel()
		c.mu.Unlock()
		return c.Wait()
	}
	c.forwardCancellationLocked()
	c.mu.Unlock()
	return nil
}

func (c *Command) forwardCancellationLocked() {
	if c.launchCancel == nil {
		return
	}
	if c.waitDone == nil {
		c.waitDone = make(chan struct{})
	}
	bounded := c.context
	waitDone := c.waitDone
	launchCancel := c.launchCancel
	go func() {
		select {
		case <-bounded.Done():
			launchCancel()
		case <-waitDone:
		}
	}()
}

func (c *Command) Wait() error {
	c.mu.Lock()
	if c.startErr != nil {
		c.proof.close()
		c.cancel()
		if c.state == commandCreated {
			c.state = commandFinished
			c.finishWaitLocked(c.startErr)
		}
		err := c.waitErr
		c.mu.Unlock()
		return err
	}
	if c.waitComplete {
		err := c.waitErr
		c.mu.Unlock()
		return err
	}
	if c.state == commandCreated {
		c.mu.Unlock()
		return ErrCommandNotStarted
	}
	if c.waitStarted {
		done := c.waitDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		err := c.waitErr
		c.mu.Unlock()
		return err
	}
	c.waitStarted = true
	c.mu.Unlock()

	err := c.waitForProcess()

	c.mu.Lock()
	err = errors.Join(err, c.lifecycleErr)
	c.state = commandFinished
	c.mu.Unlock()
	err = c.reportUnconfirmed(err)

	c.mu.Lock()
	c.finishWaitLocked(err)
	c.mu.Unlock()
	return err
}

func (c *Command) waitForProcess() error {
	defer c.cancel()
	waited := c.processWait
	select {
	case err := <-waited:
		return errors.Join(err, c.proof.confirm())
	case <-c.context.Done():
		limit := c.teardownConfirmationDelay
		if limit <= 0 {
			limit = teardownConfirmationLimit
		}
		select {
		case err := <-waited:
			return errors.Join(err, c.proof.confirm())
		case <-time.After(limit):
			c.proof.close()
			return errors.Join(c.context.Err(), ErrTeardownUnconfirmed)
		}
	}
}

func (c *Command) finishWaitLocked(err error) {
	if c.waitComplete {
		return
	}
	if c.waitDone == nil {
		c.waitDone = make(chan struct{})
	}
	c.waitErr = err
	c.waitComplete = true
	close(c.waitDone)
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

type synchronizedOutputBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedOutputBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *synchronizedOutputBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buffer.Len() == 0 {
		return nil
	}
	result := make([]byte, b.buffer.Len())
	copy(result, b.buffer.Bytes())
	return result
}

func (c *Command) Output() ([]byte, error) {
	if c.Cmd.Stdout != nil {
		return nil, errors.New("processguard: Stdout already set")
	}
	var stdout synchronizedOutputBuffer
	c.Cmd.Stdout = &stdout
	err := c.Run()
	return stdout.snapshot(), err
}

func (c *Command) CombinedOutput() ([]byte, error) {
	if c.Cmd.Stdout != nil || c.Cmd.Stderr != nil {
		return nil, errors.New("processguard: Stdout or Stderr already set")
	}
	var output synchronizedOutputBuffer
	c.Cmd.Stdout = &output
	c.Cmd.Stderr = &output
	err := c.Run()
	return output.snapshot(), err
}

// Close releases the command deadline. A started command is allowed to finish
// its guarded teardown before Close returns.
func (c *Command) Close() {
	c.mu.Lock()
	if c.closed {
		wait := c.waitStarted && !c.waitComplete
		c.mu.Unlock()
		if wait {
			_ = c.Wait()
		}
		return
	}
	c.closed = true
	wait := false
	switch c.state {
	case commandCreated:
		c.proof.close()
		c.cancel()
		c.state = commandFinished
		c.finishWaitLocked(ErrCommandClosed)
	case commandStarted:
		c.cancel()
		wait = true
	case commandFinished:
		c.proof.close()
		c.cancel()
		wait = c.waitStarted && !c.waitComplete
	}
	c.mu.Unlock()
	if wait {
		_ = c.Wait()
	}
}
