//go:build !windows

package orchestration

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func configurePlannerProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachPlannerProcessTree(cmd *exec.Cmd) (func(), error) {
	if cmd.Process == nil {
		return nil, fmt.Errorf("planner process has not started")
	}
	pid := cmd.Process.Pid
	var once sync.Once
	return func() {
		once.Do(func() {
			if !plannerProcessGroupAlive(pid) {
				return
			}
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			if waitForPlannerProcessGroup(pid, plannerProcessTerminationGrace) {
				return
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = waitForPlannerProcessGroup(pid, plannerProcessTerminationGrace)
		})
	}, nil
}

func plannerProcessGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitForPlannerProcessGroup(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for plannerProcessGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return !plannerProcessGroupAlive(pid)
}
