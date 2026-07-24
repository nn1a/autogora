//go:build linux

package processguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	parentHelperEnvironment = "AUTOGORA_TEST_PROCESS_GUARD_PARENT_HELPER"
	parentGuardPIDPath      = "AUTOGORA_TEST_PROCESS_GUARD_PID_PATH"
	parentChildPIDPath      = "AUTOGORA_TEST_PROCESS_GUARD_CHILD_PID_PATH"
)

func escapedDescendantArguments(pidPath, mode string) []string {
	script := `setsid /bin/sh -c 'trap "" TERM HUP INT; printf "%d\n" "$$" >"$1"; while :; do sleep 1; done' autogora-descendant "$1" </dev/null >/dev/null 2>&1 & while [ ! -s "$1" ]; do sleep 0.01; done; if [ "$2" = wait ]; then while :; do sleep 1; done; fi`
	return []string{"-c", script, "autogora-main", pidPath, mode}
}

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is required for process guard containment tests")
	}
}

func readPIDEventually(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID was not written to %s", path)
	return 0
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("escaped descendant %d remained alive", pid)
}

func TestLinuxGuardCleansEscapedDescendantAfterNormalExit(t *testing.T) {
	requireSetsid(t)
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	command := NewCommandContext(
		context.Background(),
		10*time.Second,
		"/bin/sh",
		escapedDescendantArguments(pidPath, "exit")...,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guarded command: %v: %s", err, output)
	}
	requireProcessGone(t, readPIDEventually(t, pidPath))
}

func TestLinuxGuardCleansEscapedDescendantAfterCancellation(t *testing.T) {
	requireSetsid(t)
	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	ctx, cancel := context.WithCancel(context.Background())
	command := NewCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		escapedDescendantArguments(pidPath, "wait")...,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	descendantPID := readPIDEventually(t, pidPath)
	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("canceled guarded command unexpectedly succeeded")
	}
	requireProcessGone(t, descendantPID)
}

func TestLinuxFencedGuardCleansEscapedDescendantAfterCancellation(
	t *testing.T,
) {
	requireSetsid(t)
	pidPath := filepath.Join(t.TempDir(), "fenced-descendant.pid")
	ctx, cancel := context.WithCancel(context.Background())
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		escapedDescendantArguments(pidPath, "wait")...,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	descendantPID := readPIDEventually(t, pidPath)
	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("canceled fenced command unexpectedly succeeded")
	}
	requireProcessGone(t, descendantPID)
}

func TestLinuxGuardSIGKILLCannotForgeTeardownProof(t *testing.T) {
	reported := make(chan error, 1)
	ctx := WithTeardownFailureReporter(context.Background(), func(err error) {
		reported <- err
	})
	command := NewCommandContext(
		ctx,
		10*time.Second,
		"/bin/sleep",
		"30",
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(command.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	if !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("guard SIGKILL error = %v, want ErrTeardownUnconfirmed", err)
	}
	select {
	case report := <-reported:
		if !errors.Is(report, ErrTeardownUnconfirmed) {
			t.Fatalf("reported error = %v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("teardown failure reporter was not called")
	}
}

func TestLinuxGuardBoundsUnprovableCleanupWithoutAttesting(t *testing.T) {
	t.Setenv(testIncompleteLineageEnvironment, "1")
	t.Setenv(testCleanupLimitEnvironment, "50")
	reported := make(chan error, 1)
	ctx := WithTeardownFailureReporter(context.Background(), func(err error) {
		reported <- err
	})
	started := time.Now()
	command := NewCommandContext(ctx, 5*time.Second, "/bin/true")
	err := command.Run()
	if !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("unprovable cleanup error = %v, want ErrTeardownUnconfirmed", err)
	}
	if command.ProcessState == nil || !command.ProcessState.Exited() {
		t.Fatal("parent returned before the negatively attesting guard exited")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("unprovable cleanup remained blocked for %s", elapsed)
	}
	select {
	case <-reported:
	case <-time.After(time.Second):
		t.Fatal("bounded negative attestation was not reported")
	}
}

func TestLinuxFencedGuardReportsUnprovableCleanup(t *testing.T) {
	t.Setenv(testIncompleteLineageEnvironment, "1")
	t.Setenv(testCleanupLimitEnvironment, "50")
	reported := make(chan error, 1)
	ctx := WithTeardownFailureReporter(context.Background(), func(err error) {
		reported <- err
	})
	command, err := NewFencedCommandContext(
		ctx,
		5*time.Second,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	started := time.Now()
	err = command.Wait()
	if !errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf(
			"unprovable fenced cleanup error = %v, want ErrTeardownUnconfirmed",
			err,
		)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("unprovable fenced cleanup remained blocked for %s", elapsed)
	}
	select {
	case report := <-reported:
		if !errors.Is(report, ErrTeardownUnconfirmed) {
			t.Fatalf("reported error = %v", report)
		}
	case <-time.After(time.Second):
		t.Fatal("fenced teardown failure reporter was not called")
	}
}

func TestLinuxFencedWaitersDoNotReturnBeforeTeardownReporter(
	t *testing.T,
) {
	t.Setenv(testIncompleteLineageEnvironment, "1")
	t.Setenv(testCleanupLimitEnvironment, "50")
	reportEntered := make(chan struct{})
	continueReport := make(chan struct{})
	ctx := WithTeardownFailureReporter(context.Background(), func(err error) {
		if !errors.Is(err, ErrTeardownUnconfirmed) {
			t.Errorf("reported error = %v", err)
		}
		close(reportEntered)
		<-continueReport
	})
	command, err := NewFencedCommandContext(
		ctx,
		5*time.Second,
		"/bin/true",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	released, err := command.Release()
	if err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}

	first := make(chan error, 1)
	go func() {
		first <- command.Wait()
	}()
	select {
	case <-reportEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown reporter was not entered")
	}

	second := make(chan error, 1)
	go func() {
		second <- command.Wait()
	}()
	select {
	case err := <-first:
		t.Fatalf("primary Wait returned before reporter completion: %v", err)
	case err := <-second:
		t.Fatalf("concurrent Wait returned before reporter completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(continueReport)
	for label, result := range map[string]<-chan error{
		"primary": first,
		"waiter":  second,
	} {
		select {
		case err := <-result:
			if !errors.Is(err, ErrTeardownUnconfirmed) {
				t.Fatalf("%s Wait error = %v", label, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s Wait remained blocked after reporter completion", label)
		}
	}
}

// TestLinuxGuardParentDeathHelper is re-executed as the parent whose abrupt
// death must trigger the guard's PDEATHSIG cleanup.
func TestLinuxGuardParentDeathHelper(t *testing.T) {
	if os.Getenv(parentHelperEnvironment) != "1" {
		return
	}
	pidPath := os.Getenv(parentChildPIDPath)
	command := NewCommandContext(
		context.Background(),
		time.Hour,
		"/bin/sh",
		escapedDescendantArguments(pidPath, "wait")...,
	)
	if err := command.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(
		os.Getenv(parentGuardPIDPath),
		[]byte(fmt.Sprintf("%d\n", command.Process.Pid)),
		0o600,
	); err != nil {
		os.Exit(3)
	}
	select {}
}

func TestLinuxGuardCleansEscapedDescendantAfterParentDeath(t *testing.T) {
	requireSetsid(t)
	directory := t.TempDir()
	guardPIDPath := filepath.Join(directory, "guard.pid")
	childPIDPath := filepath.Join(directory, "descendant.pid")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(executable, "-test.run=^TestLinuxGuardParentDeathHelper$")
	parent.Env = append(
		os.Environ(),
		parentHelperEnvironment+"=1",
		parentGuardPIDPath+"="+guardPIDPath,
		parentChildPIDPath+"="+childPIDPath,
	)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if parent.Process != nil {
			_ = parent.Process.Kill()
		}
	}()
	_ = readPIDEventually(t, guardPIDPath)
	descendantPID := readPIDEventually(t, childPIDPath)
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("killed parent unexpectedly succeeded")
	}
	requireProcessGone(t, descendantPID)
}
