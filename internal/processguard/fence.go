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
	ErrFencedCommandNotStarted       = errors.New("fenced command has not started")
	ErrFencedCommandAlreadyStarted   = errors.New("fenced command has already started")
	ErrFencedCommandAlreadyReleased  = errors.New("fenced command start is already released")
	ErrFencedCommandNotReady         = errors.New("fenced command start is not ready")
	ErrFencedCommandStartAborted     = errors.New("fenced command start is aborted")
	ErrFencedCommandClosed           = errors.New("fenced command is closed")
	ErrFencedCommandReadinessTimeout = errors.New(
		"fenced command readiness handshake timed out",
	)
	ErrFencedCommandConfigurationChanged = errors.New(
		"fenced command guard configuration changed after construction",
	)
)

type fencedCommandState uint8

const (
	fencedCommandCreated fencedCommandState = iota
	fencedCommandStarting
	fencedCommandStarted
	fencedCommandReleased
	fencedCommandAborted
	fencedCommandFinished
)

type fencedIdentityHandshake interface {
	receive(context.Context, int) (DurableIdentity, error)
	close() error
}

type fencedLifetimeLease interface {
	close() error
}

// FencedCommand holds a private guard process behind a one-shot start fence.
// Safe command configuration is copied into that private command by Start;
// the guarded target does not start until Release.
type FencedCommand struct {
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	command *exec.Cmd
	reader  *os.File
	writer  *os.File
	proof   teardownProof
	report  func(error)

	mu               sync.Mutex
	context          context.Context
	cancel           context.CancelFunc
	startValidator   func() error
	identityReady    fencedIdentityHandshake
	lifetimeLease    fencedLifetimeLease
	identity         DurableIdentity
	identityErr      error
	pid              int
	processState     *os.ProcessState
	processWait      <-chan error
	startFiles       []*os.File
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

	teardownConfirmationDelay time.Duration
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
	c.command.Dir = c.Dir
	c.command.Env = append([]string(nil), c.Env...)
	c.command.Stdin = c.Stdin
	c.command.Stdout = c.Stdout
	c.command.Stderr = c.Stderr
	var validationErr error
	if c.startValidator != nil {
		validationErr = c.startValidator()
	}
	if validationErr != nil {
		closeErr := errors.Join(
			c.closeFenceLocked(),
			c.closeStartFilesLocked(),
			c.closeIdentityReadyLocked(),
			c.closeLifetimeLeaseLocked(),
		)
		c.proof.close()
		c.cancel()
		err := errors.Join(
			ErrFencedCommandConfigurationChanged,
			validationErr,
			closeErr,
		)
		c.state = fencedCommandFinished
		c.finishWaitLocked(err)
		c.mu.Unlock()
		return err
	}
	if contextErr := c.context.Err(); contextErr != nil {
		closeErr := errors.Join(
			c.closeFenceLocked(),
			c.closeStartFilesLocked(),
			c.closeIdentityReadyLocked(),
			c.closeLifetimeLeaseLocked(),
		)
		c.proof.close()
		c.cancel()
		err := errors.Join(contextErr, closeErr)
		c.state = fencedCommandFinished
		c.finishWaitLocked(err)
		c.mu.Unlock()
		return err
	}
	// Start the private canonical command immediately after validation. Only
	// the safe configuration fields above are copied into this command.
	processWait, err := startOwnedProcess(c.command)
	if err != nil {
		closeErr := errors.Join(
			c.closeFenceLocked(),
			c.closeStartFilesLocked(),
			c.closeIdentityReadyLocked(),
			c.closeLifetimeLeaseLocked(),
		)
		c.proof.close()
		c.cancel()
		err = errors.Join(err, closeErr)
		c.state = fencedCommandFinished
		c.finishWaitLocked(err)
		c.mu.Unlock()
		return err
	}
	c.processWait = c.observeProcessWait(processWait, c.lifetimeLease)
	c.pid = c.command.Process.Pid
	startFileErr := c.closeStartFilesLocked()
	proofErr := c.proof.afterStart()
	if startFileErr != nil || proofErr != nil {
		c.state = fencedCommandStarted
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			startFileErr,
			proofErr,
		)
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			c.closeIdentityReadyLocked(),
		)
		closeErr := c.closeFenceLocked()
		resumeErr := resumePrivateFencedCommand(c.command)
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			closeErr,
			resumeErr,
		)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return c.Wait()
	}
	handshake := c.identityReady
	if handshake == nil {
		c.identityErr = ErrDurableProcessIdentityUnavailable
		c.state = fencedCommandStarted
		c.mu.Unlock()
		return nil
	}

	// Readiness can wait on kernel and child-process state. Publish a distinct
	// starting state and release the mutex so Close or AbortStart can close the
	// private handshake and cancel the guard promptly.
	c.state = fencedCommandStarting
	pid := c.pid
	c.mu.Unlock()
	identity, receiveErr := handshake.receive(c.context, pid)

	c.mu.Lock()
	if c.state != fencedCommandStarting {
		c.mu.Unlock()
		return c.Wait()
	}
	c.identityReady = nil
	if receiveErr != nil {
		c.identityErr = receiveErr
		c.lifecycleErr = errors.Join(c.lifecycleErr, receiveErr)
		closeErr := c.closeFenceLocked()
		c.lifecycleErr = errors.Join(c.lifecycleErr, closeErr)
		c.state = fencedCommandAborted
		_ = resumePrivateFencedCommand(c.command)
		c.cancel()
		c.mu.Unlock()
		return c.Wait()
	}
	c.identity = identity
	if signalErr := verifyExactFencedProcessSignal(identity); signalErr != nil {
		c.lifecycleErr = errors.Join(c.lifecycleErr, signalErr)
		closeErr := c.closeFenceLocked()
		c.lifecycleErr = errors.Join(c.lifecycleErr, closeErr)
		c.state = fencedCommandAborted
		_ = resumePrivateFencedCommand(c.command)
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return c.Wait()
	}
	if contextErr := c.context.Err(); contextErr != nil {
		closeErr := errors.Join(
			c.closeFenceLocked(),
			resumePrivateFencedCommand(c.command),
		)
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			contextErr,
			closeErr,
		)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return c.Wait()
	}
	c.state = fencedCommandStarted
	c.mu.Unlock()
	go c.watchContextCancellation()
	return nil
}

// Identity returns the exact durable guard identity captured by Start. It is
// available before Release and remains available after Wait. Platforms without
// restart-safe process identity return ErrDurableProcessIdentityUnavailable.
func (c *FencedCommand) Identity() (DurableIdentity, error) {
	if c == nil {
		return DurableIdentity{}, ErrDurableProcessIdentityUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity.Version != 0 {
		return c.identity, nil
	}
	if c.identityErr != nil {
		return DurableIdentity{}, c.identityErr
	}
	if c.state == fencedCommandStarting {
		return DurableIdentity{}, ErrFencedCommandNotReady
	}
	if c.state == fencedCommandCreated {
		return DurableIdentity{}, ErrFencedCommandNotStarted
	}
	return DurableIdentity{}, ErrDurableProcessIdentityUnavailable
}

// PID returns the private guard PID after Start without exposing the
// *os.Process object owned by exec.Cmd.
func (c *FencedCommand) PID() (int, error) {
	if c == nil {
		return 0, ErrFencedCommandNotStarted
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pid <= 0 {
		return 0, ErrFencedCommandNotStarted
	}
	return c.pid, nil
}

// ProcessState returns the reaped private guard state after its actual Wait
// completes, including when a bounded public Wait returned earlier.
func (c *FencedCommand) ProcessState() *os.ProcessState {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.processState
}

// IsGuard reports whether this command uses the platform process guard.
func (c *FencedCommand) IsGuard() bool {
	return c != nil && IsGuardCommand(c.command)
}

// RequestStop asks the exact durable guard process to terminate gracefully.
// It never exposes SIGKILL or falls back to a reusable numeric PID because the
// guard must remain alive long enough to clean descendants and attest
// quiescence.
func (c *FencedCommand) RequestStop() error {
	if c == nil {
		return ErrFencedCommandNotStarted
	}
	c.mu.Lock()
	switch c.state {
	case fencedCommandCreated:
		c.mu.Unlock()
		return ErrFencedCommandNotStarted
	case fencedCommandStarting:
		err := errors.Join(
			c.closeIdentityReadyLocked(),
			c.closeFenceLocked(),
			resumePrivateFencedCommand(c.command),
		)
		c.identityErr = ErrFencedCommandStartAborted
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			ErrFencedCommandStartAborted,
			err,
		)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return err
	case fencedCommandStarted:
		err := errors.Join(
			c.closeFenceLocked(),
			resumePrivateFencedCommand(c.command),
		)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return err
	case fencedCommandAborted:
		err := resumePrivateFencedCommand(c.command)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.scheduleCloseCancellationLocked()
		c.mu.Unlock()
		return errors.Join(ErrFencedCommandStartAborted, err)
	case fencedCommandReleased:
		// Exact signaling is the graceful path. Retain a bounded launch-context
		// fallback so transient pidfd/proc failures cannot leave shutdown
		// waiting forever.
		c.scheduleCloseCancellationLocked()
	}
	identity := c.identity
	guard := c.IsGuard()
	c.mu.Unlock()
	if identity.Version == 0 {
		if !guard {
			c.cancel()
			return nil
		}
		return ErrDurableProcessIdentityUnavailable
	}
	return requestExactFencedProcessStop(identity)
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
	case fencedCommandStarting:
		err := errors.Join(
			c.closeIdentityReadyLocked(),
			c.closeFenceLocked(),
			resumePrivateFencedCommand(c.command),
		)
		c.identityErr = ErrFencedCommandStartAborted
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			ErrFencedCommandStartAborted,
			err,
		)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		return err
	case fencedCommandStarted:
		err := errors.Join(
			c.closeFenceLocked(),
			resumePrivateFencedCommand(c.command),
		)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
		return err
	case fencedCommandReleased:
		return ErrFencedCommandAlreadyReleased
	case fencedCommandAborted:
		err := resumePrivateFencedCommand(c.command)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.scheduleCloseCancellationLocked()
		return errors.Join(ErrFencedCommandStartAborted, err)
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
	case fencedCommandStarting:
		c.mu.Unlock()
		return ErrFencedCommandNotReady
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
	waited := c.processWait
	collect := func(err error) error {
		return errors.Join(err, c.proof.confirm())
	}
	select {
	case err := <-waited:
		return collect(err)
	case <-c.context.Done():
		limit := c.teardownConfirmationDelay
		if limit <= 0 {
			limit = teardownConfirmationLimit
		}
		select {
		case err := <-waited:
			return collect(err)
		case <-time.After(limit):
			c.proof.close()
			return errors.Join(c.context.Err(), ErrTeardownUnconfirmed)
		}
	}
}

func (c *FencedCommand) observeProcessWait(
	waited <-chan error,
	lease fencedLifetimeLease,
) <-chan error {
	terminal := make(chan error, 1)
	go func() {
		waitErr := <-waited
		c.mu.Lock()
		c.processState = c.command.ProcessState
		c.mu.Unlock()
		if lease != nil {
			waitErr = errors.Join(waitErr, lease.close())
		}
		terminal <- waitErr
	}()
	return terminal
}

// Release commits one byte to the parent side of the start fence. A true
// result means the write committed; only a durable teardown receipt can prove
// that the guard observed it before interruption.
func (c *FencedCommand) Release() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.releaseCommitted, ErrFencedCommandClosed
	}
	switch c.state {
	case fencedCommandCreated:
		return false, ErrFencedCommandNotStarted
	case fencedCommandStarting:
		return false, ErrFencedCommandNotReady
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
		resumeErr := resumePrivateFencedCommand(c.command)
		err := errors.Join(contextErr, closeErr, resumeErr)
		c.lifecycleErr = errors.Join(c.lifecycleErr, err)
		c.state = fencedCommandAborted
		c.scheduleCloseCancellationLocked()
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
	resumeErr := resumePrivateFencedCommand(c.command)
	err = errors.Join(err, resumeErr)
	c.lifecycleErr = errors.Join(c.lifecycleErr, err)
	c.state = fencedCommandAborted
	c.scheduleCloseCancellationLocked()
	return false, err
}

func (c *FencedCommand) Close() {
	c.mu.Lock()
	if c.closed {
		waitForStartedGuard := c.waitStarted && !c.waitComplete ||
			c.state == fencedCommandStarting ||
			c.state == fencedCommandStarted ||
			c.state == fencedCommandReleased ||
			c.state == fencedCommandAborted
		c.mu.Unlock()
		if waitForStartedGuard {
			_ = c.Wait()
		}
		return
	}
	c.closed = true
	closeErr := c.closeFenceLocked()
	c.lifecycleErr = errors.Join(c.lifecycleErr, closeErr)
	waitForStartedGuard := false
	scheduleCancellation := false
	requestReleasedStop := false
	switch c.state {
	case fencedCommandCreated:
		startFileErr := c.closeStartFilesLocked()
		identityErr := c.closeIdentityReadyLocked()
		leaseErr := c.closeLifetimeLeaseLocked()
		c.proof.close()
		c.cancel()
		c.state = fencedCommandFinished
		c.finishWaitLocked(errors.Join(
			ErrFencedCommandClosed,
			closeErr,
			startFileErr,
			identityErr,
			leaseErr,
		))
	case fencedCommandStarting:
		identityErr := c.closeIdentityReadyLocked()
		resumeErr := resumePrivateFencedCommand(c.command)
		c.identityErr = ErrFencedCommandClosed
		c.lifecycleErr = errors.Join(
			c.lifecycleErr,
			ErrFencedCommandClosed,
			identityErr,
			resumeErr,
		)
		c.state = fencedCommandAborted
		scheduleCancellation = true
		waitForStartedGuard = true
	case fencedCommandStarted:
		resumeErr := resumePrivateFencedCommand(c.command)
		c.lifecycleErr = errors.Join(c.lifecycleErr, resumeErr)
		c.state = fencedCommandAborted
		scheduleCancellation = true
		waitForStartedGuard = true
	case fencedCommandReleased:
		requestReleasedStop = true
		scheduleCancellation = true
		waitForStartedGuard = true
	case fencedCommandAborted:
		resumeErr := resumePrivateFencedCommand(c.command)
		c.lifecycleErr = errors.Join(c.lifecycleErr, resumeErr)
		scheduleCancellation = true
		waitForStartedGuard = true
	case fencedCommandFinished:
		c.proof.close()
		c.cancel()
		waitForStartedGuard = c.waitStarted && !c.waitComplete
	}
	if scheduleCancellation {
		c.scheduleCloseCancellationLocked()
	}
	c.mu.Unlock()
	if requestReleasedStop {
		_ = c.RequestStop()
	}
	if waitForStartedGuard {
		_ = c.Wait()
	}
}

func (c *FencedCommand) watchContextCancellation() {
	select {
	case <-c.context.Done():
		_ = c.RequestStop()
	case <-c.waitDone:
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

func (c *FencedCommand) closeStartFilesLocked() error {
	var result error
	for _, file := range c.startFiles {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	c.startFiles = nil
	return result
}

func (c *FencedCommand) closeIdentityReadyLocked() error {
	if c.identityReady == nil {
		return nil
	}
	err := c.identityReady.close()
	c.identityReady = nil
	return err
}

func (c *FencedCommand) closeLifetimeLeaseLocked() error {
	if c.lifetimeLease == nil {
		return nil
	}
	err := c.lifetimeLease.close()
	c.lifetimeLease = nil
	return err
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
	configurePrivateFencedCommand(command)
	return &FencedCommand{
		command:          command,
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
