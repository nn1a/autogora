// Package publisher applies a completed finalizer change set to a host Git
// repository. It intentionally contains no database or scheduling policy: the
// caller owns publication leases and persists the returned result.
package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
)

const (
	DefaultCommandTimeout = 30 * time.Second
	MaxCommandTimeout     = 5 * time.Minute
	MaxCommandOutputBytes = 1024 * 1024
	MaxErrorDetailBytes   = 4 * 1024
)

// ResultStatus tells the caller whether Execute changed a publication target
// or found that the requested state already existed.
type ResultStatus string

const (
	ResultPublished        ResultStatus = "published"
	ResultAlreadyPublished ResultStatus = "already_published"
	ResultManualRequired   ResultStatus = "manual_required"
)

// Result is safe to persist as the host-side outcome of a publication claim.
// URL is populated only for pull-request publication.
type Result struct {
	Status       ResultStatus          `json:"status"`
	Mode         model.PublicationMode `json:"mode"`
	HeadCommit   string                `json:"headCommit"`
	TargetBranch string                `json:"targetBranch"`
	Branch       string                `json:"branch,omitempty"`
	URL          *string               `json:"url,omitempty"`
	Message      string                `json:"message,omitempty"`
}

// ErrorKind is a stable, coarse classification suitable for retry and UI
// decisions. Error details are bounded before they leave this package.
type ErrorKind string

const (
	ErrorInvalidInput          ErrorKind = "invalid_input"
	ErrorManualMode            ErrorKind = "manual_mode"
	ErrorRepository            ErrorKind = "repository"
	ErrorSourceChanged         ErrorKind = "source_changed"
	ErrorNonFastForward        ErrorKind = "non_fast_forward"
	ErrorDirtyWorktree         ErrorKind = "dirty_worktree"
	ErrorRemoteConflict        ErrorKind = "remote_conflict"
	ErrorCommandTimeout        ErrorKind = "command_timeout"
	ErrorTeardownUnconfirmed   ErrorKind = "teardown_unconfirmed"
	ErrorCommandFailed         ErrorKind = "command_failed"
	ErrorCanceled              ErrorKind = "canceled"
	ErrorCommandStartBlocked   ErrorKind = "command_start_blocked"
	ErrorCommandStartUncertain ErrorKind = "command_start_uncertain"
)

var (
	ErrInvalidInput          = errors.New("invalid publication input")
	ErrManualMode            = errors.New("manual publication cannot be executed")
	ErrRepository            = errors.New("publication repository validation failed")
	ErrSourceChanged         = errors.New("publication source no longer matches its snapshot")
	ErrNonFastForward        = errors.New("publication is not a fast-forward")
	ErrDirtyWorktree         = errors.New("publication target worktree is dirty")
	ErrRemoteConflict        = errors.New("publication remote branch or pull request conflicts")
	ErrCommandTimeout        = errors.New("publication command timed out")
	ErrCommandFailed         = errors.New("publication command failed")
	ErrTeardownUnconfirmed   = processguard.ErrTeardownUnconfirmed
	ErrCommandStartBlocked   = errors.New("publication command start was blocked")
	ErrCommandStartUncertain = errors.New(
		"publication command start result is uncertain",
	)
)

type Error struct {
	Kind        ErrorKind
	Operation   string
	Err         error
	exitCode    int
	hasExitCode bool
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	operation := strings.TrimSpace(e.Operation)
	detail := ""
	if e.Err != nil {
		detail = boundedText(e.Err.Error(), MaxErrorDetailBytes, true)
	}
	if operation == "" {
		return detail
	}
	if detail == "" {
		return operation
	}
	return boundedText(operation+": "+detail, MaxErrorDetailBytes, false)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type CommandOutput struct {
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
}

// CommandRunner is injectable so pull-request behavior can be tested without
// a network. Implementations receive argv as distinct strings; no shell is
// involved.
type CommandRunner interface {
	Run(ctx context.Context, directory, file string, args ...string) (CommandOutput, error)
}

// CommandRelease opens the one-shot fence in front of an already-started
// process guard. The boolean reports whether the target may have started even
// when release itself returns an error.
type CommandRelease func() (bool, error)

// CommandReleaseGate serializes the final target release with the caller's
// durable authorization boundary. Implementations must invoke release while
// that boundary is held and return its released state.
type CommandReleaseGate func(context.Context, CommandRelease) (bool, error)

// GatedCommandRunner supports a process-start fence. A configured ReleaseGate
// is never downgraded to CommandRunner.Run.
type GatedCommandRunner interface {
	CommandRunner
	RunGated(
		ctx context.Context,
		directory string,
		file string,
		gate CommandReleaseGate,
		args ...string,
	) (CommandOutput, error)
}

// CommandStartError preserves whether this command's target crossed its start
// fence. Released is command-scoped: callers coordinating a multi-command
// publication must separately retain attempt-wide side-effect uncertainty.
// Err is the original gate, release, abort, or teardown cause.
type CommandStartError struct {
	Released bool
	Err      error
}

func (e *CommandStartError) Error() string {
	if e == nil {
		return ""
	}
	sentinel := ErrCommandStartBlocked
	if e.Released {
		sentinel = ErrCommandStartUncertain
	}
	if e.Err == nil {
		return sentinel.Error()
	}
	return sentinel.Error() + ": " + e.Err.Error()
}

func (e *CommandStartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *CommandStartError) Is(target error) bool {
	if e == nil {
		return false
	}
	if e.Released {
		return target == ErrCommandStartUncertain
	}
	return target == ErrCommandStartBlocked
}

func newCommandStartError(released bool, cause error) *CommandStartError {
	return &CommandStartError{Released: released, Err: cause}
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

// ExecRunner is the production runner. It bounds output while the child is
// running, disables interactive Git/GitHub prompts, and waits for guarded
// descendant teardown before returning.
type ExecRunner struct{}

func (ExecRunner) Run(
	ctx context.Context,
	directory string,
	file string,
	args ...string,
) (CommandOutput, error) {
	command := processguard.NewCommandContext(
		ctx,
		MaxCommandTimeout,
		file,
		args...,
	)
	command.Dir = directory
	command.Env = commandEnvironment()
	stdout := limitedBuffer{limit: MaxCommandOutputBytes}
	stderr := limitedBuffer{limit: MaxCommandOutputBytes}
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return CommandOutput{
		Stdout: stdout.String(), Stderr: stderr.String(),
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
	}, err
}

func (ExecRunner) RunGated(
	ctx context.Context,
	directory string,
	file string,
	gate CommandReleaseGate,
	args ...string,
) (CommandOutput, error) {
	if gate == nil {
		return (ExecRunner{}).Run(ctx, directory, file, args...)
	}
	command, err := processguard.NewFencedCommandContext(
		ctx,
		MaxCommandTimeout,
		file,
		args...,
	)
	if err != nil {
		return CommandOutput{}, newCommandStartError(false, err)
	}
	defer command.Close()
	command.Command.Dir = directory
	command.Command.Env = commandEnvironment()
	stdout := limitedBuffer{limit: MaxCommandOutputBytes}
	stderr := limitedBuffer{limit: MaxCommandOutputBytes}
	command.Command.Stdout = &stdout
	command.Command.Stderr = &stderr
	output := func() CommandOutput {
		return CommandOutput{
			Stdout: stdout.String(), Stderr: stderr.String(),
			StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		}
	}
	if err := command.Start(); err != nil {
		return output(), newCommandStartError(false, err)
	}

	releaseState := struct {
		sync.Mutex
		active    bool
		called    bool
		committed bool
		err       error
	}{active: true}
	release := CommandRelease(func() (bool, error) {
		releaseState.Lock()
		defer releaseState.Unlock()
		if !releaseState.active {
			return releaseState.committed, errors.New(
				"command release callback is no longer active",
			)
		}
		if releaseState.called {
			err := errors.New(
				"command release callback was already invoked",
			)
			releaseState.err = errors.Join(releaseState.err, err)
			return releaseState.committed, err
		}
		releaseState.called = true
		released, err := command.Release()
		releaseState.committed = released
		releaseState.err = err
		return released, err
	})
	gateReleased, gateErr := gate(ctx, release)
	releaseState.Lock()
	releaseState.active = false
	releaseCalled := releaseState.called
	releaseCommitted := releaseState.committed
	releaseErr := releaseState.err
	releaseState.Unlock()
	if gateReleased && !releaseCalled {
		gateErr = errors.Join(
			gateErr,
			errors.New("command release gate reported release without invoking it"),
		)
	}
	if gateReleased != releaseCommitted {
		gateErr = errors.Join(
			gateErr,
			errors.New("command release gate returned an inconsistent release state"),
		)
	}

	if !gateReleased || !releaseCommitted {
		abortErr := command.AbortStart()
		command.Close()
		waitErr := command.Wait()
		cause := errors.Join(gateErr, releaseErr, abortErr, waitErr)
		if cause == nil {
			cause = errors.New("command release gate rejected target start")
		}
		return output(), newCommandStartError(releaseCommitted, cause)
	}

	// Waiting here, after gate has returned, prevents a long-running command
	// from retaining the caller's authorization lock.
	if gateErr != nil || releaseErr != nil {
		// Release may have crossed the execution boundary. Stop the target
		// promptly, then wait for guarded descendant teardown before reporting
		// the uncertain start.
		command.Close()
		waitErr := command.Wait()
		return output(), newCommandStartError(
			true,
			errors.Join(gateErr, releaseErr, waitErr),
		)
	}
	waitErr := command.Wait()
	return output(), waitErr
}

func commandEnvironment() []string {
	overrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "never",
		"GH_PROMPT_DISABLED":  "1",
		"GIT_MERGE_AUTOEDIT":  "no",
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if _, overridden := overrides[key]; !overridden {
			environment = append(environment, item)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

type Options struct {
	Runner         CommandRunner
	CommandTimeout time.Duration
	ReleaseGate    CommandReleaseGate
}

type Engine struct {
	runner         CommandRunner
	commandTimeout time.Duration
	releaseGate    CommandReleaseGate
}

func New(options Options) *Engine {
	runner := options.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	timeout := options.CommandTimeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	if timeout > MaxCommandTimeout {
		timeout = MaxCommandTimeout
	}
	return &Engine{
		runner: runner, commandTimeout: timeout,
		releaseGate: options.ReleaseGate,
	}
}

func Execute(
	ctx context.Context,
	publication model.Publication,
	options Options,
) (Result, error) {
	return New(options).Execute(ctx, publication)
}

type commandResult struct {
	stdout string
	stderr string
}

func (e *Engine) run(
	ctx context.Context,
	directory string,
	file string,
	args ...string,
) (commandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, e.commandTimeout)
	defer cancel()
	var output CommandOutput
	var err error
	if e.releaseGate == nil {
		output, err = e.runner.Run(runCtx, directory, file, args...)
	} else if runner, ok := e.runner.(GatedCommandRunner); ok {
		output, err = runner.RunGated(
			runCtx,
			directory,
			file,
			e.releaseGate,
			args...,
		)
	} else {
		err = newCommandStartError(
			false,
			errors.New("configured command release gate requires a gated command runner"),
		)
	}
	output = normalizeOutput(output)
	result := commandResult{stdout: output.Stdout, stderr: output.Stderr}
	if err != nil {
		if errors.Is(err, processguard.ErrTeardownUnconfirmed) {
			return result, &Error{
				Kind: ErrorTeardownUnconfirmed, Operation: "run command",
				Err: errors.Join(ErrCommandFailed, err),
			}
		}
		var startErr *CommandStartError
		if errors.As(err, &startErr) {
			kind := ErrorCommandStartBlocked
			if startErr.Released {
				kind = ErrorCommandStartUncertain
			}
			return result, &Error{
				Kind: kind, Operation: "start command", Err: err,
			}
		}
	}
	if output.StdoutTruncated {
		return commandResult{}, &Error{
			Kind: ErrorCommandFailed, Operation: "read command output",
			Err: fmt.Errorf("%w: stdout exceeded %d bytes", ErrCommandFailed, MaxCommandOutputBytes),
		}
	}
	if err == nil {
		return result, nil
	}
	if parentErr := ctx.Err(); parentErr != nil {
		return result, &Error{Kind: ErrorCanceled, Operation: "run command", Err: parentErr}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return result, &Error{
			Kind: ErrorCommandTimeout, Operation: "run command",
			Err: fmt.Errorf("%w after %s", ErrCommandTimeout, e.commandTimeout),
		}
	}
	detail := strings.TrimSpace(output.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(output.Stdout)
	}
	if detail == "" {
		detail = err.Error()
	}
	detail = boundedText(detail, MaxErrorDetailBytes, true)
	exitCode, hasExitCode := processExitCode(err)
	return result, &Error{
		Kind: ErrorCommandFailed, Operation: "run command",
		Err:      fmt.Errorf("%w: %s", ErrCommandFailed, detail),
		exitCode: exitCode, hasExitCode: hasExitCode,
	}
}

func processExitCode(err error) (int, bool) {
	type exitCoder interface {
		ExitCode() int
	}
	var value exitCoder
	if !errors.As(err, &value) {
		return 0, false
	}
	return value.ExitCode(), true
}

func normalizeOutput(output CommandOutput) CommandOutput {
	output.Stdout, output.StdoutTruncated = boundCommandOutput(
		output.Stdout, output.StdoutTruncated,
	)
	output.Stderr, output.StderrTruncated = boundCommandOutput(
		output.Stderr, output.StderrTruncated,
	)
	return output
}

func boundCommandOutput(value string, alreadyTruncated bool) (string, bool) {
	if len(value) <= MaxCommandOutputBytes {
		return value, alreadyTruncated
	}
	return boundedText(value, MaxCommandOutputBytes, false), true
}

func boundedText(value string, limit int, keepTail bool) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if keepTail {
		value = value[len(value)-limit:]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[1:]
		}
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func semanticError(kind ErrorKind, operation string, sentinel error, detail string) error {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return &Error{Kind: kind, Operation: operation, Err: sentinel}
	}
	return &Error{
		Kind: kind, Operation: operation,
		Err: fmt.Errorf("%w: %s", sentinel, boundedText(detail, MaxErrorDetailBytes, true)),
	}
}

func commandControlError(err error) error {
	var startErr *CommandStartError
	if errors.As(err, &startErr) {
		return err
	}
	var execution *Error
	if errors.As(err, &execution) &&
		(execution.Kind == ErrorCommandTimeout ||
			execution.Kind == ErrorCanceled ||
			execution.Kind == ErrorTeardownUnconfirmed ||
			execution.Kind == ErrorCommandStartBlocked ||
			execution.Kind == ErrorCommandStartUncertain) {
		return err
	}
	return nil
}
