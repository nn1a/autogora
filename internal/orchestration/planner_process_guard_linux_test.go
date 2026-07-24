//go:build linux

package orchestration

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/processguard"
)

func TestRunPlannerProcessCleansSetsidDescendantAfterSuccess(t *testing.T) {
	stdout, stderr, err := runPlannerProcess(
		context.Background(),
		"/bin/sh",
		[]string{"-c",
			`setsid /bin/sh -c 'trap "" TERM HUP INT; while :; do sleep 1; done' autogora-planner-child </dev/null >/dev/null 2>&1 & printf "child:%d\n" "$!"; exit 0`,
		},
		t.TempDir(),
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("planner process: %v: %s", err, stderr)
	}
	childPID, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(stdout), "child:"))
	if err != nil || childPID <= 0 {
		t.Fatalf("planner child output = %q: %v", stdout, err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("setsid planner descendant %d survived success: %v", childPID, err)
	}
}

func TestRunPlannerProcessCleansSetsidDescendantAfterCancellation(t *testing.T) {
	pidPath := t.TempDir() + "/planner-child.pid"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runPlannerProcess(
			ctx,
			"/bin/sh",
			[]string{"-c",
				`setsid /bin/sh -c 'trap "" TERM HUP INT; while :; do sleep 1; done' autogora-planner-child </dev/null >/dev/null 2>&1 & printf "%d" "$!" >"$1"; while :; do sleep 1; done`,
				"autogora-planner",
				pidPath,
			},
			t.TempDir(),
			10*time.Second,
		)
		done <- err
	}()
	childPID := waitForPlannerChildPID(t, pidPath)
	defer syscall.Kill(childPID, syscall.SIGKILL)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("planner cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner guard did not return after cancellation")
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("setsid planner descendant %d survived cancellation: %v", childPID, err)
	}
}

func TestRunPlannerProcessDoesNotFallbackAfterUnconfirmedTeardown(t *testing.T) {
	t.Setenv("AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_INCOMPLETE_LINEAGE", "1")
	t.Setenv("AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_CLEANUP_LIMIT_MS", "50")
	_, _, err := runPlannerProcess(
		context.Background(),
		"/bin/true",
		nil,
		t.TempDir(),
		5*time.Second,
	)
	if !errors.Is(err, processguard.ErrTeardownUnconfirmed) {
		t.Fatalf("planner error = %v, want ErrTeardownUnconfirmed", err)
	}
	if _, retry := ClassifyPlannerFailure(err); retry {
		t.Fatalf("unconfirmed planner teardown was classified for fallback: %v", err)
	}
}
