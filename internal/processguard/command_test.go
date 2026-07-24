package processguard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

const (
	commandLateOutputHelper  = "AUTOGORA_TEST_COMMAND_LATE_OUTPUT_HELPER"
	commandLateOutputReady   = "AUTOGORA_TEST_COMMAND_LATE_OUTPUT_READY"
	commandLateOutputRelease = "AUTOGORA_TEST_COMMAND_LATE_OUTPUT_RELEASE"
	commandLateOutputDone    = "AUTOGORA_TEST_COMMAND_LATE_OUTPUT_DONE"
)

type recordingTeardownProof struct {
	afterStartCalls atomic.Int32
	confirmCalls    atomic.Int32
	closeCalls      atomic.Int32
}

func (p *recordingTeardownProof) afterStart() error {
	p.afterStartCalls.Add(1)
	return nil
}
func (p *recordingTeardownProof) confirm() error {
	p.confirmCalls.Add(1)
	return nil
}
func (p *recordingTeardownProof) close() {
	p.closeCalls.Add(1)
}

func TestCommandLateOutputHelper(t *testing.T) {
	if os.Getenv(commandLateOutputHelper) != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte("early")); err != nil {
		os.Exit(91)
	}
	if err := os.WriteFile(
		os.Getenv(commandLateOutputReady),
		[]byte("ready"),
		0o600,
	); err != nil {
		os.Exit(92)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(
			os.Getenv(commandLateOutputRelease),
		); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(93)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stdout.Write([]byte("-late")); err != nil {
		os.Exit(94)
	}
	if err := os.WriteFile(
		os.Getenv(commandLateOutputDone),
		[]byte("done"),
		0o600,
	); err != nil {
		os.Exit(95)
	}
}

type blockingAfterStartProof struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingAfterStartProof) afterStart() error {
	close(p.entered)
	<-p.release
	return nil
}

func (*blockingAfterStartProof) confirm() error { return nil }
func (*blockingAfterStartProof) close()         {}

func TestNestedTeardownFailureReportersPreserveOuterFirstOrder(t *testing.T) {
	var calls []string
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(err error) {
			if !errors.Is(err, ErrTeardownUnconfirmed) {
				t.Fatalf("outer reporter error = %v", err)
			}
			calls = append(calls, "outer")
		},
	)
	ctx = WithTeardownFailureReporter(ctx, func(err error) {
		if !errors.Is(err, ErrTeardownUnconfirmed) {
			t.Fatalf("inner reporter error = %v", err)
		}
		calls = append(calls, "inner")
	})

	ReportTeardownFailure(ctx, errors.Join(
		errors.New("fixture cleanup failed"),
		ErrTeardownUnconfirmed,
	))

	if want := []string{"outer", "inner"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("reporter order = %v, want %v", calls, want)
	}
}

func TestNilNestedTeardownFailureReporterKeepsParent(t *testing.T) {
	called := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { called = true },
	)
	ctx = WithTeardownFailureReporter(ctx, nil)

	ReportTeardownFailure(ctx, ErrTeardownUnconfirmed)
	if !called {
		t.Fatal("nil nested reporter removed the parent reporter")
	}
}

func TestCommandPreStartErrorNeverSpawnsOrReportsUnconfirmedTeardown(
	t *testing.T,
) {
	expected := errors.Join(
		ErrUnsafeProcessGuardPrivileges,
		errors.New("fixture capability failure"),
	)
	proof := &recordingTeardownProof{}
	canceled := false
	reported := false
	command := &Command{
		Cmd: exec.Command(
			"/autogora-test-command-must-not-be-started",
		),
		cancel:   func() { canceled = true },
		context:  context.Background(),
		proof:    proof,
		report:   func(error) { reported = true },
		startErr: expected,
	}

	err := command.Start()
	if !errors.Is(err, ErrUnsafeProcessGuardPrivileges) {
		t.Fatalf("Start error = %v", err)
	}
	if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("pre-start error was reported as unconfirmed teardown: %v", err)
	}
	if command.Process != nil {
		t.Fatalf("pre-start failure spawned process %d", command.Process.Pid)
	}
	if proof.closeCalls.Load() == 0 || !canceled {
		t.Fatalf(
			"pre-start cleanup = proof closes:%d context canceled:%t",
			proof.closeCalls.Load(),
			canceled,
		)
	}
	if reported {
		t.Fatal("pre-start capability failure called teardown reporter")
	}

	err = command.Wait()
	if !errors.Is(err, ErrUnsafeProcessGuardPrivileges) {
		t.Fatalf("Wait error = %v", err)
	}
	if reported {
		t.Fatal("Wait reported a deterministic pre-start capability failure")
	}
}

func TestCommandWaitBeforeStartDoesNotConsumeTeardownProof(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proof := &recordingTeardownProof{}
	reported := false
	command := &Command{
		Cmd:      exec.CommandContext(ctx, "/bin/true"),
		cancel:   cancel,
		context:  ctx,
		proof:    proof,
		report:   func(error) { reported = true },
		waitDone: make(chan struct{}),
	}

	if err := command.Wait(); !errors.Is(err, ErrCommandNotStarted) {
		t.Fatalf("Wait before Start error = %v", err)
	}
	if proof.confirmCalls.Load() != 0 ||
		proof.closeCalls.Load() != 0 ||
		reported {
		t.Fatalf(
			"Wait before Start touched proof: confirm:%d close:%d report:%t",
			proof.confirmCalls.Load(),
			proof.closeCalls.Load(),
			reported,
		)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if proof.afterStartCalls.Load() != 1 ||
		proof.confirmCalls.Load() != 1 {
		t.Fatalf(
			"normal run proof calls = after:%d confirm:%d",
			proof.afterStartCalls.Load(),
			proof.confirmCalls.Load(),
		)
	}
}

func TestCommandSecondStartDoesNotCancelRunningGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proof := &recordingTeardownProof{}
	command := &Command{
		Cmd:      exec.CommandContext(ctx, "/bin/sleep", "30"),
		cancel:   cancel,
		context:  ctx,
		proof:    proof,
		waitDone: make(chan struct{}),
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); !errors.Is(err, ErrCommandAlreadyStarted) {
		t.Fatalf("second Start error = %v", err)
	}
	if proof.closeCalls.Load() != 0 {
		t.Fatal("second Start closed the active teardown proof")
	}
	command.Close()
	if proof.confirmCalls.Load() != 1 {
		t.Fatalf(
			"Close confirmed proof %d times",
			proof.confirmCalls.Load(),
		)
	}
}

func TestCommandConcurrentWaitAndCloseShareTerminalResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proof := &recordingTeardownProof{}
	command := &Command{
		Cmd:      exec.CommandContext(ctx, "/bin/sleep", "30"),
		cancel:   cancel,
		context:  ctx,
		proof:    proof,
		waitDone: make(chan struct{}),
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	closed := make(chan struct{})
	go func() {
		command.Close()
		close(closed)
	}()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("closed command unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Wait did not finish")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close did not finish")
	}
	if proof.confirmCalls.Load() != 1 {
		t.Fatalf(
			"concurrent Wait confirmed proof %d times",
			proof.confirmCalls.Load(),
		)
	}
}

func TestCommandCancellationWaitsForAfterStartReadiness(t *testing.T) {
	bounded, boundedCancel := context.WithCancel(context.Background())
	launchContext, launchCancel := context.WithCancel(context.Background())
	proof := &blockingAfterStartProof{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	command := &Command{
		Cmd: exec.CommandContext(launchContext, "/bin/sleep", "30"),
		cancel: func() {
			boundedCancel()
			launchCancel()
		},
		launchCancel: launchCancel,
		context:      bounded,
		proof:        proof,
		waitDone:     make(chan struct{}),
	}
	defer command.Close()

	started := make(chan error, 1)
	go func() {
		started <- command.Start()
	}()
	select {
	case <-proof.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not enter its post-start readiness handoff")
	}

	boundedCancel()
	select {
	case <-launchContext.Done():
		t.Fatal("bounded cancellation reached the process before readiness")
	case <-time.After(25 * time.Millisecond):
	}
	close(proof.release)
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start after readiness handoff: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start remained blocked after readiness")
	}
	select {
	case <-launchContext.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bounded cancellation was not forwarded after readiness")
	}
	if err := command.Wait(); err == nil {
		t.Fatal("canceled command unexpectedly succeeded")
	}
}

func TestCommandOutputReturnsOwnedSnapshotBeforeLateProcessWait(
	t *testing.T,
) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	release := filepath.Join(directory, "release")
	done := filepath.Join(directory, "done")
	defer os.WriteFile(release, []byte("release"), 0o600)

	bounded, boundedCancel := context.WithCancel(context.Background())
	proof := &recordingTeardownProof{}
	command := &Command{
		Cmd: exec.Command(
			executable,
			"-test.run=^TestCommandLateOutputHelper$",
		),
		cancel:                    func() {},
		context:                   bounded,
		proof:                     proof,
		waitDone:                  make(chan struct{}),
		teardownConfirmationDelay: 20 * time.Millisecond,
	}
	command.Cmd.Env = append(
		os.Environ(),
		commandLateOutputHelper+"=1",
		commandLateOutputReady+"="+ready,
		commandLateOutputRelease+"="+release,
		commandLateOutputDone+"="+done,
	)

	canceled := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, statErr := os.Stat(ready); statErr == nil {
				boundedCancel()
				canceled <- nil
				return
			}
			if time.Now().After(deadline) {
				boundedCancel()
				canceled <- errors.New(
					"late-output helper did not publish readiness",
				)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	output, runErr := command.Output()
	if cancelErr := <-canceled; cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if !errors.Is(runErr, context.Canceled) ||
		!errors.Is(runErr, ErrTeardownUnconfirmed) {
		t.Fatalf("bounded Output error = %v", runErr)
	}
	if string(output) != "early" {
		t.Fatalf("bounded Output snapshot = %q", output)
	}
	if cap(output) != len(output) {
		t.Fatalf(
			"Output snapshot aliases writable capacity: len=%d cap=%d",
			len(output),
			cap(output),
		)
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case actualWaitErr := <-command.processWait:
		if actualWaitErr != nil {
			t.Fatalf("late underlying process Wait: %v", actualWaitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late underlying process Wait did not finish")
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("late output was not attempted: %v", err)
	}
	if string(output) != "early" {
		t.Fatalf("late output mutated returned snapshot: %q", output)
	}
	if proof.confirmCalls.Load() != 0 {
		t.Fatalf(
			"unconfirmed bounded return confirmed proof %d times",
			proof.confirmCalls.Load(),
		)
	}
}
