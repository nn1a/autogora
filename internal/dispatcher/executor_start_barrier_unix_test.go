//go:build !windows

package dispatcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestExecuteTurnStartBarrierPreventsWorkBeforeSpawnRecord(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(
		filepath.Join(t.TempDir(), "executor.db"),
		"default",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "barrier-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "do not execute before durable spawn",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(
		ctx,
		store.ClaimOptions{TaskID: task.Task.ID},
	)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	marker := filepath.Join(t.TempDir(), "worker-started")
	result := ExecuteTurn(
		ctx,
		RunnerCommand{
			Command: "/bin/sh",
			Args:    []string{"-c", `printf started >"$1"`, "sh", marker},
			CWD:     t.TempDir(),
		},
		opened,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: "invalid-token"},
		NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"),
		nil,
	)
	if result.SpawnError == nil ||
		!strings.Contains(result.SpawnError.Error(), "record worker spawn") {
		t.Fatalf("spawn record failure = %#v", result)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker crossed the start barrier before spawn record: %v", err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.PID != nil {
		t.Fatalf("failed spawn record persisted PID %v", inspection.Run.PID)
	}
}

func TestExecuteTurnReleaseGatePreventsWorkerCode(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(
		filepath.Join(t.TempDir(), "executor.db"),
		"default",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "release-gate-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "do not execute without release authorization",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(
		ctx,
		store.ClaimOptions{TaskID: task.Task.ID},
	)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	marker := filepath.Join(t.TempDir(), "worker-started")
	releaseFailure := errors.New("injected release authorization failure")
	result := ExecuteTurn(
		ctx,
		RunnerCommand{
			Command: "/bin/sh",
			Args:    []string{"-c", `printf started >"$1"`, "sh", marker},
			CWD:     t.TempDir(),
			ReleaseGate: func(
				context.Context,
				WorkerRelease,
			) (bool, error) {
				return false, releaseFailure
			},
		},
		opened,
		store.RunScope{
			RunID:      claim.Run.ID,
			ClaimToken: claim.ClaimToken,
		},
		NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"),
		nil,
	)
	if !errors.Is(result.SpawnError, releaseFailure) {
		t.Fatalf("release gate failure = %#v", result)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker crossed the rejected release gate: %v", err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.PID == nil {
		t.Fatal("release gate ran before the durable spawn record")
	}
}

func TestWorkerStartBarrierPipeCloseDoesNotRunWorker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "worker-started")
	command, err := preflightWorkerCommand(RunnerCommand{
		Command: "/bin/sh",
		Args:    []string{"-c", `printf started >"$1"`, "sh", marker},
		CWD:     t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := newWorkerCommand(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	worker.child.Dir = command.CWD
	worker.child.Env = mergedEnvironment(command.Env)
	configureProcess(worker.child)
	if err := worker.start(); err != nil {
		worker.cleanup()
		t.Fatal(err)
	}
	// Closing the dispatcher-owned descriptors models process teardown. The
	// inherited read side sees EOF and exits without crossing the gate.
	worker.cleanup()
	if err := worker.wait(); err == nil {
		t.Fatal("barrier shell unexpectedly succeeded after parent pipe close")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker ran after parent pipe close: %v", err)
	}
}

func TestExecuteTurnRecordsEventualWorkerPID(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(
		filepath.Join(t.TempDir(), "executor.db"),
		"default",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "pid-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "record eventual worker PID",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	result := ExecuteTurn(
		ctx,
		RunnerCommand{
			Command: "/bin/sh",
			Args:    []string{"-c", `printf '%d\n' "$$"`},
			CWD:     t.TempDir(),
		},
		opened,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"),
		nil,
	)
	if result.SpawnError != nil || result.Code != 0 {
		t.Fatalf("execution = %#v", result)
	}
	actualPID, err := strconv.Atoi(strings.TrimSpace(result.Output))
	if err != nil {
		t.Fatalf("worker PID output %q: %v", result.Output, err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.PID == nil {
		t.Fatal("durable worker PID was not recorded")
	}
	if runtime.GOOS == "linux" && *inspection.Run.PID == actualPID {
		t.Fatalf("recorded PID = %d, want Linux guard PID distinct from target %d", *inspection.Run.PID, actualPID)
	}
	if runtime.GOOS != "linux" && *inspection.Run.PID != actualPID {
		t.Fatalf("recorded PID = %d, eventual worker PID = %d", *inspection.Run.PID, actualPID)
	}
	identity, err := opened.GetRunProcessIdentity(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil || *identity == "" {
		t.Fatal("eventual worker process identity was not recorded")
	}
}

func TestExecuteTurnPreflightRejectsNonExecutableAndMalformedFiles(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		mode    os.FileMode
		want    error
	}{
		{name: "permission", content: "#!/bin/sh\nexit 0\n", mode: 0o644, want: os.ErrPermission},
		{name: "format", content: "exit 0\n", mode: 0o755, want: syscall.ENOEXEC},
		{name: "missing interpreter", content: "#!/definitely/missing/interpreter\n", mode: 0o755, want: os.ErrNotExist},
	} {
		t.Run(test.name, func(t *testing.T) {
			commandPath := filepath.Join(t.TempDir(), "worker")
			if err := os.WriteFile(commandPath, []byte(test.content), test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := preflightWorkerCommand(RunnerCommand{Command: commandPath, CWD: t.TempDir()})
			if !errors.Is(err, test.want) {
				t.Fatalf("preflight error = %v, want %v", err, test.want)
			}
		})
	}
}
