package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

type TurnExecution struct {
	Code       int
	Signal     string
	SpawnError error
	TimedOut   bool
	Canceled   bool
	Output     string
	SessionID  string
}

var ErrProcessSetStopping = errors.New("dispatcher process set is stopping")

// TurnStartCompensation reverses durable state reserved by a TurnStartGate
// while the platform fence still guarantees that no coding-agent code has
// been released. ExecuteTurn calls it with a fresh, bounded context that is
// independent of the turn context.
type TurnStartCompensation func(context.Context) error

// TurnStartGate performs the final durable transition after the fenced worker
// process and its identity are durable, immediately before the fence releases
// coding-agent code. Its compensation remains valid until that release.
type TurnStartGate func(context.Context) (TurnStartCompensation, error)

// workerCommand holds the platform-specific start fence around the coding
// agent. release is the only operation allowed to let user code run.
type workerCommand struct {
	child   *exec.Cmd
	guarded bool
	start   func() error
	wait    func() error
	release func() (bool, error)
	stop    func(bool) error
	cleanup func()
}

type tailBuffer struct {
	mu    sync.Mutex
	value []byte
	limit int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = append(b.value, value...)
	if len(b.value) > b.limit {
		b.value = append([]byte(nil), b.value[len(b.value)-b.limit:]...)
	}
	return len(value), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.value)
}

type ProcessSet struct {
	mu        sync.Mutex
	processes map[int]trackedProcess
	stopping  bool
}

type trackedProcess struct {
	command *exec.Cmd
	stop    func(bool) error
}

func NewProcessSet() *ProcessSet {
	return &ProcessSet{processes: map[int]trackedProcess{}}
}

// add registers one live process unless shutdown has sealed the set. A
// rejected registration receives its graceful stop request before add returns,
// so its caller cannot cross the worker release boundary.
func (p *ProcessSet) add(command *exec.Cmd, stop func(bool) error) bool {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		if stop != nil {
			_ = stop(false)
		}
		return false
	}
	p.processes[command.Process.Pid] = trackedProcess{
		command: command,
		stop:    stop,
	}
	p.mu.Unlock()
	return true
}

func (p *ProcessSet) remove(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pid := command.Process.Pid
	if tracked, ok := p.processes[pid]; ok &&
		tracked.command == command {
		delete(p.processes, pid)
	}
}

func (p *ProcessSet) StopAll() {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	processes := make([]trackedProcess, 0, len(p.processes))
	for _, process := range p.processes {
		processes = append(processes, process)
	}
	clear(p.processes)
	p.mu.Unlock()
	for _, process := range processes {
		if process.stop != nil {
			_ = process.stop(false)
		}
	}
}

func mergedEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, item := range os.Environ() {
		if split := strings.IndexByte(item, '='); split >= 0 {
			values[item[:split]] = item[split+1:]
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func exitDetails(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		code := exitError.ExitCode()
		return code, exitError.ProcessState.String()
	}
	return -1, err.Error()
}

func stopAndWait(
	ctx context.Context,
	stop func(bool) error,
	done <-chan error,
) error {
	if stop != nil {
		_ = stop(false)
	}
	return waitAfterStop(ctx, stop, done)
}

// waitAfterStop bounds teardown after the caller has already issued the
// graceful stop. It avoids sending a duplicate initial stop when ProcessSet.add
// rejects a registration during shutdown.
func waitAfterStop(
	ctx context.Context,
	stop func(bool) error,
	done <-chan error,
) error {
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if stop != nil {
			_ = stop(true)
		}
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			err := processguard.ErrTeardownUnconfirmed
			processguard.ReportTeardownFailure(ctx, err)
			return err
		}
	}
}

func compensateUnstartedTurn(compensate TurnStartCompensation, cause error) error {
	if compensate == nil {
		return cause
	}
	durable, cancel := durableContext()
	defer cancel()
	if err := compensate(durable); err != nil {
		return errors.Join(cause, fmt.Errorf("compensate unstarted turn: %w", err))
	}
	return cause
}

func canceledTurnExecution(err error) TurnExecution {
	return TurnExecution{Code: -1, SpawnError: err, Canceled: true}
}

func ExecuteTurn(ctx context.Context, command RunnerCommand, opened *store.Store, scope store.RunScope, processes *ProcessSet, logPath string, runtimeLimit *time.Duration, startGate ...TurnStartGate) TurnExecution {
	if processes == nil {
		processes = NewProcessSet()
	}
	if len(startGate) > 1 {
		return TurnExecution{Code: -1, SpawnError: errors.New("execute turn accepts at most one start gate")}
	}
	if command.PolicyFile != nil {
		if err := os.MkdirAll(filepath.Dir(command.PolicyFile.Path), 0o755); err != nil {
			return TurnExecution{Code: -1, SpawnError: err}
		}
		if err := os.WriteFile(command.PolicyFile.Path, []byte(command.PolicyFile.Content), 0o600); err != nil {
			return TurnExecution{Code: -1, SpawnError: err}
		}
		defer os.Remove(command.PolicyFile.Path)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return TurnExecution{Code: -1, SpawnError: err}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return TurnExecution{Code: -1, SpawnError: err}
	}
	defer logFile.Close()

	deferred, err := opened.GetDeferredReclaim(ctx, scope.RunID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return canceledTurnExecution(ctxErr)
		}
		return TurnExecution{Code: -1, SpawnError: fmt.Errorf("check pending run termination: %w", err)}
	}
	if deferred != nil {
		return TurnExecution{Code: -1, SpawnError: fmt.Errorf("%w: %s", store.ErrRunTerminationPending, scope.RunID)}
	}
	if err := ctx.Err(); err != nil {
		return canceledTurnExecution(err)
	}
	command, err = preflightWorkerCommand(command)
	if err != nil {
		return TurnExecution{Code: -1, SpawnError: err}
	}
	worker, err := newWorkerCommand(ctx, command)
	if err != nil {
		return TurnExecution{Code: -1, SpawnError: err}
	}
	defer worker.cleanup()
	child := worker.child
	child.Dir, child.Env = command.CWD, mergedEnvironment(command.Env)
	configureProcess(child)
	tail := &tailBuffer{limit: 128 * 1024}
	writer := io.MultiWriter(logFile, tail)
	child.Stdout, child.Stderr = writer, writer
	if err := ctx.Err(); err != nil {
		return canceledTurnExecution(err)
	}
	if err := worker.start(); err != nil {
		return TurnExecution{Code: -1, SpawnError: err}
	}
	releaseProcessTree, err := attachProcessTree(child, worker.guarded)
	if err != nil {
		_ = worker.stop(true)
		_ = worker.wait()
		return TurnExecution{
			Code:       -1,
			SpawnError: fmt.Errorf("protect worker process tree: %w", err),
			Output:     tail.String(),
		}
	}
	defer releaseProcessTree()
	if !processes.add(child, worker.stop) {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		waitErr := waitAfterStop(ctx, worker.stop, done)
		return TurnExecution{
			Code:       -1,
			SpawnError: errors.Join(ErrProcessSetStopping, waitErr),
			Output:     tail.String(),
		}
	}
	defer processes.remove(child)
	processIdentity, err := processidentity.Capture(child.Process.Pid)
	if err != nil {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		_ = stopAndWait(ctx, worker.stop, done)
		return TurnExecution{
			Code:       -1,
			SpawnError: fmt.Errorf("capture worker process identity: %w", err),
			Output:     tail.String(),
		}
	}
	if _, err := opened.RecordSpawnWithIdentity(ctx, scope, child.Process.Pid, logPath, processIdentity); err != nil {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		_ = stopAndWait(ctx, worker.stop, done)
		return TurnExecution{
			Code:       -1,
			SpawnError: fmt.Errorf("record worker spawn: %w", err),
			Output:     tail.String(),
		}
	}
	var stopBroker func()
	if command.ToolApproval != nil {
		stopBroker, err = startToolApprovalBroker(ctx, *command.ToolApproval)
		if err != nil {
			done := make(chan error, 1)
			go func() { done <- worker.wait() }()
			_ = stopAndWait(ctx, worker.stop, done)
			return TurnExecution{Code: -1, SpawnError: err, Output: tail.String()}
		}
		defer stopBroker()
	}
	var compensate TurnStartCompensation
	if err := ctx.Err(); err != nil {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		_ = stopAndWait(ctx, worker.stop, done)
		return canceledTurnExecution(err)
	}
	if len(startGate) == 1 && startGate[0] != nil {
		compensate, err = startGate[0](ctx)
		if err != nil {
			done := make(chan error, 1)
			go func() { done <- worker.wait() }()
			_ = stopAndWait(ctx, worker.stop, done)
			return TurnExecution{
				Code: -1,
				SpawnError: compensateUnstartedTurn(
					compensate,
					fmt.Errorf("run turn start gate: %w", err),
				),
				Output: tail.String(),
			}
		}
	}
	if err := ctx.Err(); err != nil {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		_ = stopAndWait(ctx, worker.stop, done)
		return canceledTurnExecution(
			compensateUnstartedTurn(compensate, err),
		)
	}
	release := WorkerRelease(worker.release)
	released := false
	if command.ReleaseGate != nil {
		released, err = command.ReleaseGate(ctx, release)
	} else {
		released, err = release()
	}
	if err != nil {
		done := make(chan error, 1)
		go func() { done <- worker.wait() }()
		_ = stopAndWait(ctx, worker.stop, done)
		if !released {
			err = compensateUnstartedTurn(compensate, fmt.Errorf("release worker start barrier: %w", err))
		} else {
			err = fmt.Errorf("release worker start barrier after worker release: %w", err)
		}
		return TurnExecution{
			Code:       -1,
			SpawnError: err,
			Output:     tail.String(),
		}
	}
	compensate = nil

	done := make(chan error, 1)
	go func() { done <- worker.wait() }()
	var waitErr error
	result := TurnExecution{Code: -1}
	if runtimeLimit == nil {
		select {
		case waitErr = <-done:
		case <-ctx.Done():
			result.Canceled = true
			waitErr = stopAndWait(ctx, worker.stop, done)
		}
	} else {
		limit := *runtimeLimit
		if limit < time.Millisecond {
			limit = time.Millisecond
		}
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case waitErr = <-done:
		case <-ctx.Done():
			result.Canceled = true
			waitErr = stopAndWait(ctx, worker.stop, done)
		case <-timer.C:
			result.TimedOut = true
			waitErr = stopAndWait(ctx, worker.stop, done)
		}
	}
	result.Code, result.Signal = exitDetails(waitErr)
	if errors.Is(waitErr, processguard.ErrTeardownUnconfirmed) {
		result.SpawnError = waitErr
	}
	result.Output = tail.String()
	result.SessionID = SessionIDFromOutput(result.Output)
	return result
}

func (r TurnExecution) ExitDescription() string {
	signal := r.Signal
	if signal == "" {
		signal = "none"
	}
	return "code=" + strconv.Itoa(r.Code) + ", signal=" + signal
}
