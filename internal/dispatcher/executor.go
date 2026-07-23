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

// TurnStartCompensation reverses durable state reserved by a TurnStartGate when
// the worker process could not be created. ExecuteTurn calls it with a fresh,
// bounded context that is independent of the turn context.
type TurnStartCompensation func(context.Context) error

// TurnStartGate performs the final durable transition immediately before a
// worker process is created. The returned compensation is used only while no
// worker process exists.
type TurnStartGate func(context.Context) (TurnStartCompensation, error)

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
	processes map[int]*exec.Cmd
}

func NewProcessSet() *ProcessSet { return &ProcessSet{processes: map[int]*exec.Cmd{}} }

func (p *ProcessSet) add(command *exec.Cmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.processes[command.Process.Pid] = command
}

func (p *ProcessSet) remove(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.processes, command.Process.Pid)
}

func (p *ProcessSet) StopAll() {
	p.mu.Lock()
	commands := make([]*exec.Cmd, 0, len(p.processes))
	for _, command := range p.processes {
		commands = append(commands, command)
	}
	p.mu.Unlock()
	for _, command := range commands {
		_ = terminateProcess(command, false)
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

func stopAndWait(command *exec.Cmd, done <-chan error) error {
	_ = terminateProcess(command, false)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = terminateProcess(command, true)
		return <-done
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

	child := exec.Command(command.Command, command.Args...)
	child.Dir, child.Env = command.CWD, mergedEnvironment(command.Env)
	configureProcess(child)
	tail := &tailBuffer{limit: 128 * 1024}
	writer := io.MultiWriter(logFile, tail)
	child.Stdout, child.Stderr = writer, writer
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
	var compensate TurnStartCompensation
	if len(startGate) == 1 && startGate[0] != nil {
		compensate, err = startGate[0](ctx)
		if err != nil {
			return TurnExecution{
				Code:       -1,
				SpawnError: compensateUnstartedTurn(compensate, fmt.Errorf("run turn start gate: %w", err)),
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledTurnExecution(compensateUnstartedTurn(compensate, err))
	}
	if err := child.Start(); err != nil {
		if child.Process == nil {
			err = compensateUnstartedTurn(compensate, err)
		}
		return TurnExecution{Code: -1, SpawnError: err}
	}
	releaseProcessTree, err := attachProcessTree(child)
	if err != nil {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
		return TurnExecution{Code: -1, SpawnError: fmt.Errorf("protect worker process tree: %w", err), Output: tail.String()}
	}
	defer releaseProcessTree()
	processes.add(child)
	defer processes.remove(child)
	processIdentity, err := processidentity.Capture(child.Process.Pid)
	if err != nil {
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		_ = stopAndWait(child, done)
		return TurnExecution{Code: -1, SpawnError: fmt.Errorf("capture worker process identity: %w", err), Output: tail.String()}
	}
	if _, err := opened.RecordSpawnWithIdentity(ctx, scope, child.Process.Pid, logPath, processIdentity); err != nil {
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		_ = stopAndWait(child, done)
		return TurnExecution{Code: -1, SpawnError: fmt.Errorf("record worker spawn: %w", err), Output: tail.String()}
	}
	var stopBroker func()
	if command.ToolApproval != nil {
		stopBroker, err = startToolApprovalBroker(ctx, *command.ToolApproval)
		if err != nil {
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			_ = stopAndWait(child, done)
			return TurnExecution{Code: -1, SpawnError: err, Output: tail.String()}
		}
		defer stopBroker()
	}

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	var waitErr error
	result := TurnExecution{Code: -1}
	if runtimeLimit == nil {
		select {
		case waitErr = <-done:
		case <-ctx.Done():
			result.Canceled = true
			waitErr = stopAndWait(child, done)
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
			waitErr = stopAndWait(child, done)
		case <-timer.C:
			result.TimedOut = true
			waitErr = stopAndWait(child, done)
		}
	}
	result.Code, result.Signal = exitDetails(waitErr)
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
