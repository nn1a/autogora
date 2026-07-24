package processguard

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

var (
	ErrFencedCommandNotStarted      = errors.New("fenced command has not started")
	ErrFencedCommandAlreadyStarted  = errors.New("fenced command has already started")
	ErrFencedCommandAlreadyReleased = errors.New("fenced command start is already released")
	ErrFencedCommandStartAborted    = errors.New("fenced command start is aborted")
	ErrFencedCommandClosed          = errors.New("fenced command is closed")
)

type fencedCommandState uint8

const (
	fencedCommandCreated fencedCommandState = iota
	fencedCommandStarted
	fencedCommandReleased
	fencedCommandAborted
	fencedCommandFinished
)

// FencedCommand holds a guard process behind a one-shot start fence. Command
// is the durable process: the guarded target does not start until Release.
type FencedCommand struct {
	Command *exec.Cmd
	reader  *os.File
	writer  *os.File
	proof   teardownProof
	report  func(error)

	mu               sync.Mutex
	context          context.Context
	cancel           context.CancelFunc
	state            fencedCommandState
	closed           bool
	releaseCommitted bool
	lifecycleErr     error
	waitStarted      bool
	waitDone         chan struct{}
	waitComplete     bool
	waitErr          error
	closeCancelDelay time.Duration
	closeCancelTimer *time.Timer
}

func (c *FencedCommand) Start() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrFencedCommandClosed
	}
	if c.state != fencedCommandCreated {
		c.mu.Unlock()
		return ErrFencedCommandAlreadyStarted
	}
	if err := c.Command.Start(); err != nil {
		closeErr := c.closeFenceLocked()
		c.proof.close()
		c.cancel()
		err = errors.Join(err, closeErr)
		c.state = fencedCommandFinished
		c.finishWaitLocked(err)
		c.mu.Unlock()
		return err
	}
	if err := c.proof.afterStart(); err != nil {
		c.state = fencedCommandStarted
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			ErrTeardownUnconfirmed,
			err,
		)
		c.cancel()
		process := c.Command.Process
		c.mu.Unlock()
		if process != nil {
			_ = process.Kill()
		}
		return c.Wait()
	}
	c.state = fencedCommandStarted
	c.mu.Unlock()
	return nil
}

// AbortStart closes the one-shot fence without writing the release byte. A
// started guard exits without launching the target, and Wait still verifies
// the guard's teardown proof.
func (c *FencedCommand) AbortStart() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrFencedCommandClosed
	}
	switch c.state {
	case fencedCommandCreated:
		return ErrFencedCommandNotStarted
	case fencedCommandStarted:
		err := c.closeFenceLocked()
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.state = fencedCommandAborted
		if err != nil {
			c.cancel()
		}
		return err
	case fencedCommandReleased:
		return ErrFencedCommandAlreadyReleased
	case fencedCommandAborted:
		return ErrFencedCommandStartAborted
	default:
		if c.releaseCommitted {
			return ErrFencedCommandAlreadyReleased
		}
		return ErrFencedCommandStartAborted
	}
}

func (c *FencedCommand) Wait() error {
	c.mu.Lock()
	if c.waitComplete {
		err := c.waitErr
		c.mu.Unlock()
		return err
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
	switch c.state {
	case fencedCommandCreated:
		c.mu.Unlock()
		return ErrFencedCommandNotStarted
	case fencedCommandStarted, fencedCommandReleased, fencedCommandAborted:
		c.waitStarted = true
	default:
		err := c.waitErr
		if err == nil {
			err = ErrFencedCommandNotStarted
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	err := c.waitForProcess()

	c.mu.Lock()
	err = errors.Join(err, c.lifecycleErr, c.closeFenceLocked())
	c.state = fencedCommandFinished
	c.stopCloseCancelTimerLocked()
	c.mu.Unlock()

	// Durable teardown reporters must complete before any concurrent Wait
	// caller can observe this terminal result.
	return c.reportAndFinishWait(err)
}

func (c *FencedCommand) waitForProcess() error {
	defer c.cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- c.Command.Wait()
	}()
	select {
	case err := <-waited:
		return errors.Join(err, c.proof.confirm())
	case <-c.context.Done():
		select {
		case err := <-waited:
			return errors.Join(err, c.proof.confirm())
		case <-time.After(teardownConfirmationLimit):
			c.proof.close()
			return errors.Join(c.context.Err(), ErrTeardownUnconfirmed)
		}
	}
}

func (c *FencedCommand) Release() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.releaseCommitted, ErrFencedCommandClosed
	}
	switch c.state {
	case fencedCommandCreated:
		return false, ErrFencedCommandNotStarted
	case fencedCommandReleased:
		return true, ErrFencedCommandAlreadyReleased
	case fencedCommandAborted:
		return false, ErrFencedCommandStartAborted
	case fencedCommandStarted:
	default:
		if c.releaseCommitted {
			return true, ErrFencedCommandAlreadyReleased
		}
		return false, ErrFencedCommandStartAborted
	}
	if contextErr := c.context.Err(); contextErr != nil {
		closeErr := c.closeFenceLocked()
		err := errors.Join(contextErr, closeErr)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.state = fencedCommandAborted
		return false, err
	}

	written, writeErr := c.writer.Write([]byte{'\n'})
	closeErr := c.closeFenceLocked()
	err := errors.Join(writeErr, closeErr)
	if written == 1 {
		c.releaseCommitted = true
		c.state = fencedCommandReleased
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		return true, err
	}
	err = errors.Join(err, io.ErrShortWrite)
	c.lifecycleErr = errors.Join(c.lifecycleErr, err)
	c.state = fencedCommandAborted
	if closeErr != nil {
		c.cancel()
	}
	return false, err
}

func (c *FencedCommand) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	closeErr := c.closeFenceLocked()
	c.lifecycleErr = errors.Join(c.lifecycleErr, closeErr)
	waitForStartedGuard := false
	scheduleCancellation := false
	switch c.state {
	case fencedCommandCreated:
		c.proof.close()
		c.cancel()
		c.state = fencedCommandFinished
		c.finishWaitLocked(errors.Join(ErrFencedCommandClosed, closeErr))
	case fencedCommandStarted:
		c.state = fencedCommandAborted
		if closeErr != nil {
			c.cancel()
		} else {
			scheduleCancellation = true
		}
		waitForStartedGuard = !c.waitStarted
	case fencedCommandReleased:
		c.cancel()
		waitForStartedGuard = !c.waitStarted
	case fencedCommandAborted:
		if closeErr != nil {
			c.cancel()
		} else {
			scheduleCancellation = true
		}
		waitForStartedGuard = !c.waitStarted
	case fencedCommandFinished:
		c.proof.close()
		c.cancel()
	}
	if scheduleCancellation {
		c.scheduleCloseCancellationLocked()
	}
	c.mu.Unlock()
	if waitForStartedGuard {
		_ = c.Wait()
	}
}

func (c *FencedCommand) closeFenceLocked() error {
	var result error
	if c.writer != nil {
		result = errors.Join(result, c.writer.Close())
		c.writer = nil
	}
	if c.reader != nil {
		result = errors.Join(result, c.reader.Close())
		c.reader = nil
	}
	return result
}

func (c *FencedCommand) finishWaitLocked(err error) {
	if c.waitComplete {
		return
	}
	c.waitErr = err
	c.waitComplete = true
	close(c.waitDone)
}

func (c *FencedCommand) scheduleCloseCancellationLocked() {
	if c.closeCancelTimer != nil {
		return
	}
	delay := c.closeCancelDelay
	if delay <= 0 {
		delay = teardownConfirmationLimit
	}
	c.closeCancelTimer = time.AfterFunc(delay, c.cancel)
}

func (c *FencedCommand) stopCloseCancelTimerLocked() {
	if c.closeCancelTimer == nil {
		return
	}
	c.closeCancelTimer.Stop()
	c.closeCancelTimer = nil
}

func (c *FencedCommand) reportUnconfirmed(err error) error {
	if errors.Is(err, ErrTeardownUnconfirmed) && c.report != nil {
		c.report(err)
	}
	return err
}

func (c *FencedCommand) reportAndFinishWait(err error) (result error) {
	result = err
	defer func() {
		c.mu.Lock()
		c.finishWaitLocked(result)
		c.mu.Unlock()
	}()
	result = c.reportUnconfirmed(result)
	return result
}

func newFencedCommand(
	reportContext context.Context,
	commandContext context.Context,
	cancel context.CancelFunc,
	command *exec.Cmd,
	reader, writer *os.File,
	proof teardownProof,
) *FencedCommand {
	return &FencedCommand{
		Command:          command,
		reader:           reader,
		writer:           writer,
		proof:            proof,
		report:           teardownFailureReporter(reportContext),
		context:          commandContext,
		cancel:           cancel,
		state:            fencedCommandCreated,
		waitDone:         make(chan struct{}),
		closeCancelDelay: teardownConfirmationLimit,
	}
}
