//go:build !windows

package processguard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	fencedTargetHelperEnvironment = "AUTOGORA_TEST_FENCED_TARGET_HELPER"
	fencedTargetModeEnvironment   = "AUTOGORA_TEST_FENCED_TARGET_MODE"
	fencedTargetMarkerEnvironment = "AUTOGORA_TEST_FENCED_TARGET_MARKER"
	fencedTestWaitLimit           = 5 * time.Second
)

func TestFencedCommandTargetHelper(t *testing.T) {
	if os.Getenv(fencedTargetHelperEnvironment) != "1" {
		return
	}
	marker := os.Getenv(fencedTargetMarkerEnvironment)
	if marker != "" {
		if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
			os.Exit(91)
		}
	}
	switch os.Getenv(fencedTargetModeEnvironment) {
	case "wait":
		for {
			time.Sleep(time.Second)
		}
	case "delay":
		time.Sleep(100 * time.Millisecond)
	}
}

type fenceTestProof struct{}

func (fenceTestProof) afterStart() error { return nil }
func (fenceTestProof) confirm() error    { return nil }
func (fenceTestProof) close()            {}

type fenceTestLease struct {
	closed chan struct{}
}

func (l *fenceTestLease) close() error {
	close(l.closed)
	return nil
}

func TestFencedCommandLateProcessWaitStillClosesLifetimeLease(t *testing.T) {
	finished := exec.Command("/bin/true")
	if err := finished.Run(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := &FencedCommand{
		command:                   &exec.Cmd{ProcessState: finished.ProcessState},
		context:                   ctx,
		cancel:                    func() {},
		proof:                     fenceTestProof{},
		teardownConfirmationDelay: 10 * time.Millisecond,
		waitDone:                  make(chan struct{}),
		state:                     fencedCommandReleased,
	}
	waited := make(chan error, 1)
	lease := &fenceTestLease{closed: make(chan struct{})}
	command.processWait = command.observeProcessWait(waited, lease)

	if err := command.waitForProcess(); !errors.Is(
		err,
		ErrTeardownUnconfirmed,
	) {
		t.Fatalf("bounded wait error = %v", err)
	}
	waited <- nil
	select {
	case <-lease.closed:
	case <-time.After(time.Second):
		t.Fatal("late process Wait did not close the lifetime lease")
	}
	deadline := time.Now().Add(time.Second)
	for command.ProcessState() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if command.ProcessState() != finished.ProcessState {
		t.Fatal("late process Wait did not publish ProcessState")
	}
}

func eofIgnoringFencedCommand(
	t *testing.T,
) (*FencedCommand, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "eof-ignoring-guard-started")
	t.Setenv(fencedTargetHelperEnvironment, "1")
	t.Setenv(fencedTargetModeEnvironment, "wait")
	t.Setenv(fencedTargetMarkerEnvironment, marker)
	commandContext, cancel := boundedContext(context.Background(), 0)
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command := exec.CommandContext(
		commandContext,
		executable,
		"-test.run=^TestFencedCommandTargetHelper$",
	)
	guarded := newFencedCommand(
		context.Background(),
		commandContext,
		cancel,
		command,
		reader,
		writer,
		fenceTestProof{},
	)
	guarded.closeCancelDelay = 50 * time.Millisecond
	return guarded, marker
}

func fencedTargetCommand(
	t *testing.T,
	ctx context.Context,
	maximum time.Duration,
	mode string,
) (*FencedCommand, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "target-started")
	t.Setenv(fencedTargetHelperEnvironment, "1")
	t.Setenv(fencedTargetModeEnvironment, mode)
	t.Setenv(fencedTargetMarkerEnvironment, marker)
	command, err := NewFencedCommandContext(
		ctx,
		maximum,
		executable,
		"-test.run=^TestFencedCommandTargetHelper$",
	)
	if err != nil {
		t.Fatal(err)
	}
	return command, marker
}

func requireFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target marker exists after rejected start: %v", err)
	}
}

func requireFileEventually(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(fencedTestWaitLimit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("target marker was not created at %s", path)
}

func requireFencedWaitStarted(t *testing.T, command *FencedCommand) {
	t.Helper()
	deadline := time.Now().Add(fencedTestWaitLimit)
	for time.Now().Before(deadline) {
		command.mu.Lock()
		started := command.waitStarted
		command.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fenced command Wait did not start")
}

func TestFencedCommandAbortStartNeverRunsTarget(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"marker",
	)
	defer command.Close()
	if err := command.AbortStart(); !errors.Is(
		err,
		ErrFencedCommandNotStarted,
	) {
		t.Fatalf("abort before Start error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); released ||
		!errors.Is(err, ErrFencedCommandStartAborted) {
		t.Fatalf("release after abort = %t, %v", released, err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("aborted guard unexpectedly exited successfully")
	}
	requireFileAbsent(t, marker)
}

func TestFencedCommandWaitBeforeReleaseAllowsRelease(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"marker",
	)
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	requireFencedWaitStarted(t, command)
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release after Wait = %t, %v", released, err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait after release error = %v", err)
		}
	case <-time.After(fencedTestWaitLimit):
		t.Fatal("Wait remained blocked after release")
	}
	requireFileEventually(t, marker)
}

func TestFencedCommandWaitBeforeAbortAllowsAbort(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"marker",
	)
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	requireFencedWaitStarted(t, command)
	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("aborted guard unexpectedly exited successfully")
		}
	case <-time.After(fencedTestWaitLimit):
		t.Fatal("Wait remained blocked after abort")
	}
	requireFileAbsent(t, marker)
}

func TestFencedCommandCloseReapsUnreleasedGuard(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"marker",
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	command.Close()
	if command.ProcessState() == nil {
		t.Fatalf(
			"Close returned without reaping the unreleased guard: state=%d waitStarted=%t waitComplete=%t waitErr=%v",
			command.state,
			command.waitStarted,
			command.waitComplete,
			command.waitErr,
		)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("closed unreleased guard unexpectedly exited successfully")
	} else if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("Close destroyed the unreleased guard's teardown proof: %v", err)
	}
	requireFileAbsent(t, marker)
}

func TestFencedCommandCloseBoundsExistingWaitOnEOFIgnoringGuard(
	t *testing.T,
) {
	command, marker := eofIgnoringFencedCommand(t)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	requireFileEventually(t, marker)
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	requireFencedWaitStarted(t, command)

	started := time.Now()
	command.Close()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("canceled EOF-ignoring guard unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close left the existing Wait blocked on an EOF-ignoring guard")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("EOF-ignoring guard cancellation took %s", elapsed)
	}
	if command.ProcessState() == nil {
		t.Fatal("EOF-ignoring guard was not reaped")
	}
}

func TestFencedCommandReleaseIsOneShotAndWaitIsShareable(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"delay",
	)
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); !errors.Is(err, ErrFencedCommandAlreadyStarted) {
		t.Fatalf("second Start error = %v", err)
	}

	type releaseResult struct {
		released bool
		err      error
	}
	releases := make(chan releaseResult, 2)
	var releaseWait sync.WaitGroup
	releaseWait.Add(2)
	for index := 0; index < 2; index++ {
		go func() {
			defer releaseWait.Done()
			released, err := command.Release()
			releases <- releaseResult{released: released, err: err}
		}()
	}
	releaseWait.Wait()
	close(releases)
	successes := 0
	alreadyReleased := 0
	for result := range releases {
		if !result.released {
			t.Fatalf("release did not preserve committed=true: %+v", result)
		}
		switch {
		case result.err == nil:
			successes++
		case errors.Is(result.err, ErrFencedCommandAlreadyReleased):
			alreadyReleased++
		default:
			t.Fatalf("release error = %v", result.err)
		}
	}
	if successes != 1 || alreadyReleased != 1 {
		t.Fatalf(
			"release outcomes: success=%d already=%d",
			successes,
			alreadyReleased,
		)
	}

	waits := make(chan error, 2)
	var waiters sync.WaitGroup
	waiters.Add(2)
	for index := 0; index < 2; index++ {
		go func() {
			defer waiters.Done()
			waits <- command.Wait()
		}()
	}
	waiters.Wait()
	close(waits)
	for err := range waits {
		if err != nil {
			t.Fatalf("shared Wait error = %v", err)
		}
	}
	requireFileEventually(t, marker)
}

func TestFencedCommandReleaseReportsCommittedWhenDescriptorCloseFails(
	t *testing.T,
) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		5*time.Second,
		"marker",
	)
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.reader.Close(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if !released || err == nil {
		t.Fatalf("release with close failure = %t, %v", released, err)
	}
	if waitErr := command.Wait(); waitErr == nil {
		t.Fatal("descriptor close failure was not retained by Wait")
	}
	requireFileEventually(t, marker)
}

func TestFencedCommandMaximumCancelsGuardBeforeRelease(t *testing.T) {
	command, marker := fencedTargetCommand(
		t,
		context.Background(),
		50*time.Millisecond,
		"marker",
	)
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := command.Wait(); err == nil {
		t.Fatal("expired fenced command unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("fenced command timeout remained blocked for %s", elapsed)
	}
	requireFileAbsent(t, marker)
}

func TestFencedCommandContextCancellationAfterReleaseStopsTarget(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	command, marker := fencedTargetCommand(t, ctx, 0, "wait")
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	requireFileEventually(t, marker)
	cancel()
	started := time.Now()
	if err := command.Wait(); err == nil {
		t.Fatal("canceled target unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled fenced target remained blocked for %s", elapsed)
	}
}
