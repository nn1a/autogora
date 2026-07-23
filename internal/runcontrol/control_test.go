package runcontrol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

func TestTerminateRunPersistsIntentAndReclaimsMissingProcess(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "terminate", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	termination, err := TerminateTaskRun(ctx, opened, task.Task.ID, "administrative edit")
	if err != nil {
		t.Fatal(err)
	}
	if termination.RunID != claim.Run.ID || termination.Signaled || termination.Pending || termination.Task.Task.Status != model.TaskStatusReady {
		t.Fatalf("unexpected termination: %+v", termination)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != model.RunStatusReclaimed || inspection.Run.Error == nil || *inspection.Run.Error != "administrative edit" {
		t.Fatalf("termination intent was not preserved: %+v", inspection.Run)
	}
}

func TestTerminateManagedRunWithoutProcessDefersRecovery(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "managed termination", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}

	termination, err := TerminateTaskRun(ctx, opened, task.Task.ID, "administrative edit")
	if err != nil {
		t.Fatal(err)
	}
	if termination.RunID != claim.Run.ID || termination.Signaled || !termination.Pending || termination.Task.Task.Status != model.TaskStatusRunning {
		t.Fatalf("managed termination should remain pending: %+v", termination)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != model.RunStatusRunning || inspection.Task.CurrentRunID == nil {
		t.Fatalf("managed run was released before dispatcher recovery: %+v", inspection)
	}
	intent, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || intent == nil || intent.Reason != "administrative edit" {
		t.Fatalf("termination intent was not preserved: %+v, %v", intent, err)
	}
}

func TestSignalAndForceKillRejectReusedPIDIdentity(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestRunControlProcessHelper")
	command.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()
	identity, err := processidentity.Capture(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	reusedIdentity := identity + "-different-process"
	pid := command.Process.Pid
	if SignalRunProcess(&pid, &reusedIdentity) || ForceKillRunProcess(&pid, &reusedIdentity) {
		t.Fatal("termination accepted a PID with a different process-start identity")
	}
	state := processidentity.Inspect(pid, &identity)
	if !state.Alive || !state.Verified || !state.Matches {
		t.Fatalf("unrelated process was stopped: %+v", state)
	}
}

func TestRunControlProcessHelper(t *testing.T) {
	if os.Getenv("AUTOGORA_RUNCONTROL_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}
