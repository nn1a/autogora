//go:build linux

package publisher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func publisherChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(value)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publisher child did not write PID to %s", path)
	return 0
}

func requirePublisherChildGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("publisher descendant %d remained alive", pid)
}

func publisherEscapingScript(t *testing.T, wait bool) string {
	t.Helper()
	mode := "exit"
	if wait {
		mode = "wait"
	}
	path := filepath.Join(t.TempDir(), "publisher-command")
	content := `#!/bin/sh
setsid /bin/sh -c 'trap "" TERM HUP INT; printf "%d\n" "$$" >"$1"; while :; do sleep 1; done' autogora-publisher-descendant "$AUTOGORA_TEST_PUBLISHER_CHILD_PID" </dev/null >/dev/null 2>&1 &
while [ ! -s "$AUTOGORA_TEST_PUBLISHER_CHILD_PID" ]; do sleep 0.01; done
if [ "` + mode + `" = wait ]; then
  while :; do sleep 1; done
fi
`
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunnerCleansSetsidDescendantAfterSuccess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "publisher-child.pid")
	t.Setenv("AUTOGORA_TEST_PUBLISHER_CHILD_PID", pidPath)
	if _, err := (ExecRunner{}).Run(
		context.Background(),
		t.TempDir(),
		publisherEscapingScript(t, false),
	); err != nil {
		t.Fatal(err)
	}
	requirePublisherChildGone(t, publisherChildPID(t, pidPath))
}

func TestExecRunnerCleansSetsidDescendantAfterCancellation(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "publisher-child.pid")
	t.Setenv("AUTOGORA_TEST_PUBLISHER_CHILD_PID", pidPath)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	directory := t.TempDir()
	script := publisherEscapingScript(t, true)
	go func() {
		_, err := (ExecRunner{}).Run(
			ctx,
			directory,
			script,
		)
		result <- err
	}()
	childPID := publisherChildPID(t, pidPath)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled Publisher command unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publisher command did not finish guarded cancellation")
	}
	requirePublisherChildGone(t, childPID)
}

func TestExecRunnerReleaseGateDenialNeverRunsTarget(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-ran")
	gateCause := errors.New("automatic publication authorization was revoked")
	_, err := (ExecRunner{}).RunGated(
		context.Background(),
		t.TempDir(),
		"/bin/sh",
		func(context.Context, CommandRelease) (bool, error) {
			return false, gateCause
		},
		"-c",
		`printf executed >"$1"`,
		"autogora-publisher-gate",
		marker,
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || startErr.Released ||
		!errors.Is(err, ErrCommandStartBlocked) ||
		!errors.Is(err, gateCause) {
		t.Fatalf("denied command start error=%v detail=%#v", err, startErr)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target crossed denied start fence: %v", statErr)
	}
}

func TestExecRunnerReleasedGateErrorIsUncertain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-ran")
	gateCause := errors.New("release result could not be persisted")
	_, err := (ExecRunner{}).RunGated(
		context.Background(),
		t.TempDir(),
		"/bin/sh",
		func(
			_ context.Context,
			release CommandRelease,
		) (bool, error) {
			released, releaseErr := release()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if value, readErr := os.ReadFile(marker); readErr == nil &&
					string(value) == "executed" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			return released, errors.Join(releaseErr, gateCause)
		},
		"-c",
		`printf executed >"$1"`,
		"autogora-publisher-gate",
		marker,
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || !startErr.Released ||
		!errors.Is(err, ErrCommandStartUncertain) ||
		!errors.Is(err, gateCause) {
		t.Fatalf("uncertain command start error=%v detail=%#v", err, startErr)
	}
	value, readErr := os.ReadFile(marker)
	if readErr != nil || string(value) != "executed" {
		t.Fatalf("released target result=%q err=%v", value, readErr)
	}
}

func TestExecRunnerReleasedGateErrorPromptlyContainsTarget(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "publisher-child.pid")
	t.Setenv("AUTOGORA_TEST_PUBLISHER_CHILD_PID", pidPath)
	gateCause := errors.New("release result could not be persisted")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	directory := t.TempDir()
	script := publisherEscapingScript(t, true)
	go func() {
		_, err := (ExecRunner{}).RunGated(
			ctx,
			directory,
			script,
			func(
				_ context.Context,
				release CommandRelease,
			) (bool, error) {
				released, releaseErr := release()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if info, statErr := os.Stat(pidPath); statErr == nil &&
						info.Size() > 0 {
						return released, errors.Join(releaseErr, gateCause)
					}
					time.Sleep(10 * time.Millisecond)
				}
				return released, errors.Join(
					releaseErr,
					gateCause,
					errors.New("target did not expose its child before gate failure"),
				)
			},
		)
		result <- err
	}()

	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-result:
		case <-time.After(10 * time.Second):
		}
		t.Fatal("uncertain released command was not promptly contained")
	}
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || !startErr.Released ||
		!errors.Is(err, gateCause) ||
		time.Since(started) >= 5*time.Second {
		t.Fatalf(
			"contained start error=%v detail=%#v elapsed=%s",
			err,
			startErr,
			time.Since(started),
		)
	}
	requirePublisherChildGone(t, publisherChildPID(t, pidPath))
}

func TestExecRunnerRejectsReleaseCallbackAfterGateReturns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-ran")
	var lateRelease CommandRelease
	_, err := (ExecRunner{}).RunGated(
		context.Background(),
		t.TempDir(),
		"/bin/sh",
		func(_ context.Context, release CommandRelease) (bool, error) {
			lateRelease = release
			return false, nil
		},
		"-c",
		`printf executed >"$1"`,
		"autogora-publisher-gate",
		marker,
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || startErr.Released {
		t.Fatalf("blocked command start error=%v detail=%#v", err, startErr)
	}
	if lateRelease == nil {
		t.Fatal("gate did not receive a release callback")
	}
	released, lateErr := lateRelease()
	if released || lateErr == nil ||
		!strings.Contains(lateErr.Error(), "no longer active") {
		t.Fatalf("late release result=%v err=%v", released, lateErr)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("late callback released target: %v", statErr)
	}
}

func TestExecRunnerRejectsGateClaimWithoutReleaseCallback(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "target-ran")
	_, err := (ExecRunner{}).RunGated(
		context.Background(),
		t.TempDir(),
		"/bin/sh",
		func(context.Context, CommandRelease) (bool, error) {
			return true, nil
		},
		"-c",
		`printf executed >"$1"`,
		"autogora-publisher-gate",
		marker,
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || startErr.Released ||
		!strings.Contains(err.Error(), "without invoking") {
		t.Fatalf("lying gate error=%v detail=%#v", err, startErr)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("lying gate released target: %v", statErr)
	}
}

func TestExecRunnerRetainsDuplicateReleaseError(t *testing.T) {
	_, err := (ExecRunner{}).RunGated(
		context.Background(),
		t.TempDir(),
		"/bin/true",
		func(
			_ context.Context,
			release CommandRelease,
		) (bool, error) {
			released, releaseErr := release()
			_, _ = release()
			return released, releaseErr
		},
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || !startErr.Released ||
		!strings.Contains(err.Error(), "already invoked") {
		t.Fatalf("duplicate release error=%v detail=%#v", err, startErr)
	}
}
