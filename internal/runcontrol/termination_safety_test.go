package runcontrol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestTerminateUnmanagedLiveUnverifiedProcessStaysPending(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestRunControlProcessHelper")
	command.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()

	ctx := context.Background()
	opened, err := store.Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "unverified worker",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if _, err := opened.RecordSpawn(ctx, scope, command.Process.Pid, filepath.Join(t.TempDir(), "worker.log")); err != nil {
		t.Fatal(err)
	}

	termination, err := TerminateRun(ctx, opened, claim.Run.ID, "operator request")
	if err != nil {
		t.Fatal(err)
	}
	if termination.Signaled || !termination.Pending || termination.Task.Task.Status != model.TaskStatusRunning {
		t.Fatalf("live process without a verifiable identity was released: %+v", termination)
	}
	if !ProcessMayStillBeRunning(&command.Process.Pid, nil) {
		t.Fatal("lease safety check did not retain a live unverified process")
	}
}
