//go:build !windows

package orchestration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const plannerProcessTreeHelperEnv = "AUTOGORA_PLANNER_PROCESS_TREE_HELPER"

func TestRunPlannerProcessTerminatesDescendantsOnCancellation(t *testing.T) {
	childPIDPath := t.TempDir() + "/child.pid"
	t.Setenv(plannerProcessTreeHelperEnv, "parent")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runPlannerProcess(
			ctx,
			os.Args[0],
			[]string{
				"-test.run=TestPlannerProcessTreeHelper",
				"--",
				childPIDPath,
			},
			t.TempDir(),
			5*time.Second,
		)
		done <- err
	}()

	childPID := waitForPlannerChildPID(t, childPIDPath)
	childExited := false
	defer func() {
		if !childExited {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("planner cancellation error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner process did not return after cancellation")
	}
	childExited = waitForPlannerChildExit(childPID, 5*time.Second)
	if !childExited {
		t.Fatalf("planner descendant %d survived cancellation", childPID)
	}
}

func TestRunPlannerProcessPreservesCancellationAfterSuccessfulLeader(t *testing.T) {
	childPIDPath := t.TempDir() + "/child.pid"
	t.Setenv(plannerProcessTreeHelperEnv, "normal-parent")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := runPlannerProcess(
			ctx,
			os.Args[0],
			[]string{
				"-test.run=TestPlannerProcessTreeHelper",
				"--",
				childPIDPath,
			},
			t.TempDir(),
			5*time.Second,
		)
		done <- err
	}()

	childPID := waitForPlannerChildPID(t, childPIDPath)
	parentPID := waitForPlannerChildPID(t, childPIDPath+".parent")
	childExited := false
	defer func() {
		if !childExited {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()
	if !waitForPlannerChildExit(parentPID, 5*time.Second) {
		t.Fatalf("successful planner leader %d did not exit before cancellation", parentPID)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("planner cancellation after leader exit = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner process did not return after post-leader cancellation")
	}
	childExited = waitForPlannerChildExit(childPID, 5*time.Second)
	if !childExited {
		t.Fatalf("post-leader planner descendant %d survived cancellation", childPID)
	}
}

func TestRunPlannerProcessCleansDescendantsAfterSuccessfulLeader(t *testing.T) {
	childPIDPath := t.TempDir() + "/child.pid"
	t.Setenv(plannerProcessTreeHelperEnv, "normal-parent")
	stdout, _, err := runPlannerProcess(
		context.Background(),
		os.Args[0],
		[]string{
			"-test.run=TestPlannerProcessTreeHelper",
			"--",
			childPIDPath,
		},
		t.TempDir(),
		5*time.Second,
	)
	childPID := waitForPlannerChildPID(t, childPIDPath)
	childExited := false
	defer func() {
		if !childExited {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()
	if err != nil {
		t.Fatalf("successful planner with a background descendant failed: %v", err)
	}
	if !strings.Contains(stdout, "planner complete") {
		t.Fatalf("planner stdout = %q", stdout)
	}
	childExited = waitForPlannerChildExit(childPID, 5*time.Second)
	if !childExited {
		t.Fatalf("successful planner descendant %d survived leader exit", childPID)
	}
}

func TestPlannerProcessTreeHelper(t *testing.T) {
	role := os.Getenv(plannerProcessTreeHelperEnv)
	if role == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		os.Exit(90)
	}
	childPIDPath := args[1]
	switch role {
	case "parent", "normal-parent":
		childRole := "child"
		if role == "normal-parent" {
			childRole = "normal-child"
		}
		child := exec.Command(
			os.Args[0],
			"-test.run=TestPlannerProcessTreeHelper",
			"--",
			childPIDPath,
		)
		child.Env = plannerProcessTreeHelperEnvironment(childRole)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(
			childPIDPath,
			[]byte(strconv.Itoa(child.Process.Pid)),
			0o600,
		); err != nil {
			_ = child.Process.Kill()
			os.Exit(92)
		}
		if role == "normal-parent" {
			_, _ = os.Stdout.WriteString("planner complete\n")
			if err := os.WriteFile(
				childPIDPath+".parent",
				[]byte(strconv.Itoa(os.Getpid())),
				0o600,
			); err != nil {
				_ = child.Process.Kill()
				os.Exit(94)
			}
			return
		}
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	case "normal-child":
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(93)
	}
}

func plannerProcessTreeHelperEnvironment(role string) []string {
	prefix := plannerProcessTreeHelperEnv + "="
	environment := os.Environ()
	for index := range environment {
		if strings.HasPrefix(environment[index], prefix) {
			environment[index] = prefix + role
			return environment
		}
	}
	return append(environment, prefix+role)
}

func waitForPlannerChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(value))
			if parseErr != nil {
				t.Fatalf("parse planner child PID: %v", parseErr)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("planner child did not start")
	return 0
}

func waitForPlannerChildExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
