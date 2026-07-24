package publisher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type panicRunner struct{}

func (panicRunner) Run(
	context.Context,
	string,
	string,
	...string,
) (CommandOutput, error) {
	panic("runner must not be called")
}

func TestExecuteManualRefusesWithoutRunningCommand(t *testing.T) {
	result, err := Execute(context.Background(), model.Publication{
		Mode: model.PublicationModeManual,
	}, Options{Runner: panicRunner{}})
	if !errors.Is(err, ErrManualMode) {
		t.Fatalf("manual error = %v", err)
	}
	if result.Status != ResultManualRequired ||
		!strings.Contains(result.Message, "manual publication") {
		t.Fatalf("manual result = %+v", result)
	}
	var execution *Error
	if !errors.As(err, &execution) || execution.Kind != ErrorManualMode {
		t.Fatalf("manual error kind = %#v", execution)
	}
}

type timeoutRunner struct{}

func (timeoutRunner) Run(
	ctx context.Context,
	_ string,
	_ string,
	_ ...string,
) (CommandOutput, error) {
	<-ctx.Done()
	return CommandOutput{}, ctx.Err()
}

func TestEveryCommandReceivesTimeout(t *testing.T) {
	directory := t.TempDir()
	_, err := Execute(context.Background(), model.Publication{
		Mode: model.PublicationModeLocalFF, RepositoryPath: directory,
		WorktreePath: directory,
	}, Options{Runner: timeoutRunner{}, CommandTimeout: 5 * time.Millisecond})
	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	var execution *Error
	if !errors.As(err, &execution) || execution.Kind != ErrorCommandTimeout {
		t.Fatalf("timeout kind = %#v", execution)
	}
}

type outputRunner struct {
	output CommandOutput
	err    error
}

func (r outputRunner) Run(
	context.Context,
	string,
	string,
	...string,
) (CommandOutput, error) {
	return r.output, r.err
}

type countingRunner struct {
	calls int
}

func (r *countingRunner) Run(
	context.Context,
	string,
	string,
	...string,
) (CommandOutput, error) {
	r.calls++
	return CommandOutput{}, nil
}

type gatedOutputRunner struct {
	runCalls   int
	gatedCalls int
	output     CommandOutput
	err        error
}

func (r *gatedOutputRunner) Run(
	context.Context,
	string,
	string,
	...string,
) (CommandOutput, error) {
	r.runCalls++
	return r.output, r.err
}

func (r *gatedOutputRunner) RunGated(
	context.Context,
	string,
	string,
	CommandReleaseGate,
	...string,
) (CommandOutput, error) {
	r.gatedCalls++
	return r.output, r.err
}

func TestConfiguredReleaseGateRejectsNonGatedRunner(t *testing.T) {
	runner := &countingRunner{}
	gateCalls := 0
	engine := New(Options{
		Runner: runner,
		ReleaseGate: func(
			context.Context,
			CommandRelease,
		) (bool, error) {
			gateCalls++
			return true, nil
		},
	})
	_, err := engine.run(context.Background(), ".", "ignored")
	if !errors.Is(err, ErrCommandStartBlocked) {
		t.Fatalf("unsupported gated runner error = %v", err)
	}
	var startErr *CommandStartError
	var execution *Error
	if !errors.As(err, &startErr) || startErr.Released ||
		!errors.As(err, &execution) ||
		execution.Kind != ErrorCommandStartBlocked {
		t.Fatalf("unsupported gated runner detail = %#v, %#v", startErr, execution)
	}
	if runner.calls != 0 || gateCalls != 0 {
		t.Fatalf("unsupported runner crossed gate: runs=%d gates=%d", runner.calls, gateCalls)
	}
}

func TestUngatedEngineKeepsUsingCommandRunner(t *testing.T) {
	runner := &gatedOutputRunner{
		output: CommandOutput{Stdout: "ungated"},
	}
	result, err := New(Options{Runner: runner}).run(
		context.Background(),
		".",
		"ignored",
	)
	if err != nil || result.stdout != "ungated" ||
		runner.runCalls != 1 || runner.gatedCalls != 0 {
		t.Fatalf(
			"ungated result=%+v err=%v run=%d gated=%d",
			result,
			err,
			runner.runCalls,
			runner.gatedCalls,
		)
	}
}

func TestEnginePrioritizesTeardownWhilePreservingCommandStartError(
	t *testing.T,
) {
	gateCause := errors.New("gate release response was lost")
	runner := &gatedOutputRunner{
		output: CommandOutput{
			Stdout:          strings.Repeat("x", MaxCommandOutputBytes),
			StdoutTruncated: true,
		},
		err: newCommandStartError(
			true,
			errors.Join(gateCause, ErrTeardownUnconfirmed),
		),
	}
	engine := New(Options{
		Runner: runner,
		ReleaseGate: func(
			context.Context,
			CommandRelease,
		) (bool, error) {
			return false, errors.New("unused fake gate")
		},
	})
	_, err := engine.run(context.Background(), ".", "ignored")
	var startErr *CommandStartError
	var execution *Error
	if !errors.As(err, &startErr) || !startErr.Released ||
		!errors.As(err, &execution) ||
		execution.Kind != ErrorTeardownUnconfirmed ||
		!errors.Is(err, ErrCommandStartUncertain) ||
		!errors.Is(err, ErrTeardownUnconfirmed) ||
		!errors.Is(err, gateCause) {
		t.Fatalf(
			"ordered command-start error=%v start=%#v execution=%#v",
			err,
			startErr,
			execution,
		)
	}
	if control := commandControlError(err); control == nil {
		t.Fatal("command start uncertainty was not preserved as a control error")
	}
	if runner.runCalls != 0 || runner.gatedCalls != 1 {
		t.Fatalf("runner path calls: run=%d gated=%d", runner.runCalls, runner.gatedCalls)
	}
}

func TestRunnerOutputAndErrorsAreBounded(t *testing.T) {
	engine := New(Options{Runner: outputRunner{
		output: CommandOutput{Stdout: strings.Repeat("x", MaxCommandOutputBytes+100)},
	}})
	_, err := engine.run(context.Background(), ".", "ignored")
	if !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("oversized output error = %v", err)
	}
	if len(err.Error()) > MaxErrorDetailBytes {
		t.Fatalf("error was not bounded: %d bytes", len(err.Error()))
	}

	engine = New(Options{Runner: outputRunner{
		output: CommandOutput{Stderr: strings.Repeat("실패", MaxErrorDetailBytes)},
		err:    errors.New("exit"),
	}})
	_, err = engine.run(context.Background(), ".", "ignored")
	if !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("command error = %v", err)
	}
	if len(err.Error()) > MaxErrorDetailBytes || !strings.Contains(err.Error(), "패") {
		t.Fatalf("bounded UTF-8 error = %q (%d bytes)", err, len(err.Error()))
	}
}

func TestEnginePreservesUnconfirmedTeardownForOperatorHandling(t *testing.T) {
	engine := New(Options{Runner: outputRunner{
		err: ErrTeardownUnconfirmed,
	}})
	_, err := engine.run(context.Background(), ".", "ignored")
	if !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("teardown error = %v", err)
	}
	var execution *Error
	if !errors.As(err, &execution) ||
		execution.Kind != ErrorTeardownUnconfirmed {
		t.Fatalf("teardown error kind = %#v", execution)
	}
	if control := commandControlError(err); !errors.Is(
		control,
		ErrTeardownUnconfirmed,
	) {
		t.Fatalf("teardown control error = %v", control)
	}
}

func TestParseWorktreePorcelain(t *testing.T) {
	records, err := parseWorktreePorcelain(
		"worktree /repo\x00HEAD " + strings.Repeat("a", 40) +
			"\x00branch refs/heads/main\x00\x00" +
			"worktree /repo/worker with space\x00HEAD " +
			strings.Repeat("b", 40) + "\x00detached\x00\x00",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].branch != "refs/heads/main" ||
		records[1].path != "/repo/worker with space" ||
		records[1].branch != "" {
		t.Fatalf("records = %+v", records)
	}
}

func TestSanitizedTaskID(t *testing.T) {
	value, err := sanitizedTaskID("  기능 / Unsafe..Task.lock  ")
	if err != nil {
		t.Fatal(err)
	}
	if value != "unsafe-task-lock" {
		t.Fatalf("sanitized task = %q", value)
	}
	value, err = sanitizedTaskID("한글")
	if err != nil {
		t.Fatal(err)
	}
	if value != "task" {
		t.Fatalf("non-ASCII fallback = %q", value)
	}
}
