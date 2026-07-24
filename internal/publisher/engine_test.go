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
