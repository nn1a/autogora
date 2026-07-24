//go:build linux

package dispatcher

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/store"
)

func TestExecuteTurnCleansSetsidEscapedDescendant(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "escaped background cleanup", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "/bin/sh",
		Args: []string{"-c",
			`setsid /bin/sh -c 'trap "" TERM HUP INT; while :; do sleep 1; done' autogora-descendant </dev/null >/dev/null 2>&1 & echo "child:$!"; exit 0`,
		},
		CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"), nil)
	if result.SpawnError != nil || result.Code != 0 {
		t.Fatalf("execute worker: %#v", result)
	}
	childPID, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(result.Output), "child:"))
	if err != nil || childPID <= 0 {
		t.Fatalf("parse descendant PID from %q: %v", result.Output, err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("dispatcher left setsid descendant %d alive: %v", childPID, err)
	}
}

func TestExecuteTurnReturnsUnconfirmedTeardownWithoutRuntimeLimit(t *testing.T) {
	t.Setenv("AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_INCOMPLETE_LINEAGE", "1")
	t.Setenv("AUTOGORA_INTERNAL_PROCESS_GUARD_TEST_CLEANUP_LIMIT_MS", "50")
	var teardownReports atomic.Int32
	ctx := processguard.WithTeardownFailureReporter(
		context.Background(),
		func(error) {
			teardownReports.Add(1)
		},
	)
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "unconfirmed cleanup", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	started := time.Now()
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "/bin/true",
		CWD:     t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"), nil)
	if !errors.Is(result.SpawnError, processguard.ErrTeardownUnconfirmed) {
		t.Fatalf("execution = %#v, want unconfirmed teardown", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("unconfirmed worker cleanup blocked for %s", elapsed)
	}
	if reports := teardownReports.Load(); reports != 1 {
		t.Fatalf("teardown reporter calls = %d, want 1", reports)
	}
}
