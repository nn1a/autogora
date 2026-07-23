//go:build !windows

package dispatcher

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachProcessTree(cmd *exec.Cmd) (func(), error) {
	pid := cmd.Process.Pid
	return func() {
		// This closure runs only for the exact command started by this
		// dispatcher. Unlike restart recovery, the group cannot have been
		// confused with an unrelated persisted PID, so it is safe to clean up
		// background descendants before allowing the run to become terminal.
		if !processGroupAlive(pid) {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		if waitForProcessGroup(pid, time.Second) {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = waitForProcessGroup(pid, time.Second)
	}, nil
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitForProcessGroup(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return !processGroupAlive(pid)
}

func terminateProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}
