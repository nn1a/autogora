//go:build !windows

package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

func TestRecoveryKeepsOwnershipWhileWorkerDescendantLives(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "leader exits first", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}

	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	leader := exec.Command("sh", "-c", `sleep 30 & echo $! > "$1"; kill -STOP $$`, "sh", childPIDPath)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	leaderPID := leader.Process.Pid
	defer func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		_, _ = leader.Process.Wait()
	}()
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		contents, readErr := os.ReadFile(childPIDPath)
		if readErr == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(contents)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("worker descendant did not start")
	}
	identity, err := processidentity.Capture(leaderPID)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if _, err := opened.RecordSpawnWithIdentity(ctx, scope, leaderPID, filepath.Join(t.TempDir(), "worker.log"), identity); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(leaderPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := leader.Wait(); err != nil {
		t.Fatal(err)
	}

	zero := time.Duration(0)
	options := Options{CrashGrace: &zero}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusRunning || detail.Task.CurrentRunID == nil ||
		*detail.Task.CurrentRunID != claim.Run.ID || detail.Runs[0].Status != model.RunStatusRunning {
		t.Fatalf("recovery released ownership while descendant was alive: %#v", detail)
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("worker descendant is not alive: %v", err)
	}
}

func TestExecuteTurnCleansOwnedBackgroundDescendants(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "executor.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "background cleanup", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	result := ExecuteTurn(ctx, RunnerCommand{
		Command: "sh", Args: []string{"-c", `sleep 30 </dev/null >/dev/null 2>&1 & echo "child:$!"; exit 0`}, CWD: t.TempDir(),
	}, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, NewProcessSet(),
		filepath.Join(t.TempDir(), "worker.log"), nil)
	if result.SpawnError != nil || result.Code != 0 {
		t.Fatalf("execute worker: %#v", result)
	}
	childPID, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(result.Output), "child:"))
	if err != nil || childPID <= 0 {
		t.Fatalf("parse descendant PID from %q: %v", result.Output, err)
	}
	if err := syscall.Kill(childPID, 0); err == nil || err == syscall.EPERM {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("dispatcher left worker descendant %d alive", childPID)
	}
}
