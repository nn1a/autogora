//go:build linux

package processguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxFencedRequestStopUsesExactDurableIdentity(t *testing.T) {
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sleep",
		"30",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := command.RequestStop(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	if err == nil {
		t.Fatal("stopped guard unexpectedly succeeded")
	}
	if errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("exact graceful stop lost teardown proof: %v", err)
	}
	if reported {
		t.Fatal("exact graceful stop called teardown failure reporter")
	}
}

func TestLinuxExactStopRejectsChangedDurableIdentity(t *testing.T) {
	command, err := NewFencedCommandContext(
		context.Background(),
		10*time.Second,
		"/bin/sleep",
		"30",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	changed := identity
	changed.StartTimeTicks++
	if err := requestExactFencedProcessStop(changed); !errors.Is(
		err,
		ErrDurableProcessIdentityChanged,
	) {
		t.Fatalf("changed identity stop error = %v", err)
	}

	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("guard was signaled through changed identity: %t, %v", released, err)
	}
	if err := command.RequestStop(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("cleanup after rejected identity lost teardown proof: %v", err)
	}
}

func TestLinuxFencedExactStopFailureUsesBoundedFallback(t *testing.T) {
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sleep",
		"30",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	command.mu.Lock()
	command.identity.StartTimeTicks++
	command.closeCancelDelay = 20 * time.Millisecond
	command.mu.Unlock()
	if err := command.RequestStop(); !errors.Is(
		err,
		ErrDurableProcessIdentityChanged,
	) {
		t.Fatalf("changed exact stop error = %v", err)
	}
	started := time.Now()
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("bounded exact-stop fallback lost proof: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded exact-stop fallback took %s", elapsed)
	}
	if reported {
		t.Fatal("bounded exact-stop fallback reported unconfirmed teardown")
	}
}

func TestLinuxFencedRequestStopBeforeReleasePreventsTarget(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-started")
	command, err := NewFencedCommandContext(
		context.Background(),
		10*time.Second,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.RequestStop(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); released ||
		!errors.Is(err, ErrFencedCommandStartAborted) {
		t.Fatalf("release after stop = %t, %v", released, err)
	}
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("unreleased exact stop lost teardown proof: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed an aborted start fence: %v", err)
	}
}

func TestLinuxFencedRequestStopResumesStoppedReleasedGuard(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "guard-stopped")
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		"-c",
		`kill -STOP "$PPID"; printf stopped >"$1"; sleep 30`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target did not stop its guard")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.RequestStop(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("stopped exact guard lost teardown proof: %v", err)
	}
	if reported {
		t.Fatal("stopped exact guard called teardown failure reporter")
	}
}

func TestLinuxFencedCloseResumesStoppedReleasedGuard(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "guard-stopped")
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		"-c",
		`kill -STOP "$PPID"; printf stopped >"$1"; sleep 30`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if released, err := command.Release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target did not stop its guard")
		}
		time.Sleep(10 * time.Millisecond)
	}
	command.Close()
	if reported {
		t.Fatal("Close reported an unconfirmed stopped guard")
	}
	if command.ProcessState() == nil {
		t.Fatal("Close did not reap the stopped guard")
	}
}

func TestLinuxFencedAbortResumesStoppedReadyGuard(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-started")
	command, err := NewFencedCommandContext(
		context.Background(),
		10*time.Second,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := withExactFencedProcess(identity, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := command.AbortStart(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("aborted stopped guard lost teardown proof: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed an aborted stopped fence: %v", err)
	}
}

func TestLinuxFencedCloseResumesStoppedUnreleasedGuard(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-started")
	reported := false
	ctx := WithTeardownFailureReporter(
		context.Background(),
		func(error) { reported = true },
	)
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := withExactFencedProcess(identity, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	command.Close()
	if reported {
		t.Fatal("Close reported an unconfirmed stopped unreleased guard")
	}
	if command.ProcessState() == nil {
		t.Fatal("Close did not reap the stopped unreleased guard")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed a closed start fence: %v", err)
	}
}

func TestLinuxFencedCanceledReleaseResumesStoppedGuard(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	reported := false
	ctx := WithTeardownFailureReporter(parent, func(error) { reported = true })
	marker := filepath.Join(t.TempDir(), "target-started")
	command, err := NewFencedCommandContext(
		ctx,
		10*time.Second,
		"/bin/sh",
		"-c",
		`printf started >"$1"`,
		"autogora-target",
		marker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := command.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := withExactFencedProcess(identity, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, _ = command.Release()
	if err := command.Wait(); errors.Is(err, ErrTeardownUnconfirmed) {
		t.Fatalf("canceled release lost teardown proof: %v", err)
	}
	if reported {
		t.Fatal("canceled release reported unconfirmed teardown")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target crossed a canceled start fence: %v", err)
	}
}
