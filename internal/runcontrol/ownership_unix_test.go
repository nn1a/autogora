//go:build !windows

package runcontrol

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

type processGroupFixture struct {
	command   *exec.Cmd
	leaderPID int
	childPID  int
	waitOnce  sync.Once
}

func startProcessGroupFixture(t *testing.T) *processGroupFixture {
	t.Helper()

	command := exec.Command(
		os.Args[0],
		"-test.run=^TestRunControlProcessGroupHelper$",
	)
	command.Env = append(
		os.Environ(),
		"AUTOGORA_RUNCONTROL_GROUP_HELPER=1",
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &processGroupFixture{
		command:   command,
		leaderPID: command.Process.Pid,
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-fixture.leaderPID, syscall.SIGKILL)
		_ = fixture.command.Process.Kill()
		fixture.wait()
	})

	result := make(chan struct {
		pid int
		err error
	}, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr != nil {
			result <- struct {
				pid int
				err error
			}{err: readErr}
			return
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
		result <- struct {
			pid int
			err error
		}{pid: pid, err: parseErr}
	}()

	select {
	case ready := <-result:
		if ready.err != nil {
			t.Fatalf("read process-group helper readiness: %v", ready.err)
		}
		fixture.childPID = ready.pid
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process-group helper")
	}

	childGroup, err := syscall.Getpgid(fixture.childPID)
	if err != nil {
		t.Fatalf("inspect helper child process group: %v", err)
	}
	if childGroup != fixture.leaderPID {
		t.Fatalf(
			"helper child process group = %d, want leader %d",
			childGroup,
			fixture.leaderPID,
		)
	}
	return fixture
}

func (fixture *processGroupFixture) wait() {
	fixture.waitOnce.Do(func() {
		_, _ = fixture.command.Process.Wait()
	})
}

func (fixture *processGroupFixture) stopLeader(t *testing.T) {
	t.Helper()
	if err := fixture.command.Process.Kill(); err != nil {
		t.Fatalf("stop process-group leader: %v", err)
	}
	fixture.wait()
}

func TestProcessMayStillBeRunningRetainsMismatchedPIDProcessGroup(t *testing.T) {
	fixture := startProcessGroupFixture(t)
	identity, err := processidentity.Capture(fixture.leaderPID)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedIdentity := identity + "-reused"
	state := processidentity.Inspect(
		fixture.leaderPID,
		&mismatchedIdentity,
	)
	if !state.Alive || !state.Verified || state.Matches {
		t.Fatalf("helper did not present a verified identity mismatch: %+v", state)
	}

	if !ProcessMayStillBeRunning(
		&fixture.leaderPID,
		&mismatchedIdentity,
	) {
		t.Fatal(
			"verified PID mismatch hid live worker process-group descendants",
		)
	}
	if !ProcessMayStillBeRunning(&fixture.leaderPID, nil) {
		t.Fatal(
			"unverifiable PID identity hid live worker process-group descendants",
		)
	}

	fixture.stopLeader(t)
	if err := syscall.Kill(fixture.childPID, 0); err != nil {
		t.Fatalf("helper descendant stopped with its leader: %v", err)
	}
	if !ProcessMayStillBeRunning(
		&fixture.leaderPID,
		&identity,
	) {
		t.Fatal("exited leader hid its live process-group descendant")
	}
}

func TestProcessMayStillBeRunningRejectsMismatchedPIDWithoutOwnedGroup(
	t *testing.T,
) {
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestRunControlProcessHelper$",
	)
	command.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()

	pid := command.Process.Pid
	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if group == pid {
		t.Fatalf("helper unexpectedly owns process group %d", group)
	}
	identity, err := processidentity.Capture(pid)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedIdentity := identity + "-reused"
	if ProcessMayStillBeRunning(&pid, &mismatchedIdentity) {
		t.Fatal("verified PID mismatch without an owned group retained ownership")
	}
}

func TestTerminateRunKeepsUnmanagedDescendantGroupPending(t *testing.T) {
	fixture := startProcessGroupFixture(t)
	ctx := context.Background()
	opened, err := store.Open(
		filepath.Join(t.TempDir(), "autogora.db"),
		"default",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "worker with surviving descendant",
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
		t.Fatalf("claim: %v %v", claim, err)
	}
	scope := store.RunScope{
		RunID:      claim.Run.ID,
		ClaimToken: claim.ClaimToken,
	}
	if _, err := opened.RecordSpawn(
		ctx,
		scope,
		fixture.leaderPID,
		filepath.Join(t.TempDir(), "worker.log"),
	); err != nil {
		t.Fatal(err)
	}

	fixture.stopLeader(t)
	if err := syscall.Kill(fixture.childPID, 0); err != nil {
		t.Fatalf("helper descendant stopped with its leader: %v", err)
	}
	termination, err := TerminateRun(
		ctx,
		opened,
		claim.Run.ID,
		"operator request",
	)
	if err != nil {
		t.Fatal(err)
	}
	if termination.Signaled ||
		!termination.Pending ||
		termination.Task.Task.Status != model.TaskStatusRunning {
		t.Fatalf(
			"surviving process-group descendant was released: %+v",
			termination,
		)
	}
}

func TestRunControlProcessGroupHelper(t *testing.T) {
	if os.Getenv("AUTOGORA_RUNCONTROL_GROUP_HELPER") != "1" {
		return
	}
	child := exec.Command(
		os.Args[0],
		"-test.run=^TestRunControlProcessHelper$",
	)
	child.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, child.Process.Pid)
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
}
