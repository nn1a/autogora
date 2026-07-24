//go:build !windows

package processguard

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configurePrivateFencedCommand(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

func resumePrivateFencedCommand(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	err := command.Process.Signal(syscall.SIGCONT)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	// The guard can be stopped immediately after the parent's first SIGCONT
	// (for example, between publishing the starting state and its readiness
	// write). Keep it runnable briefly so a pending cancellation or closed
	// fence can reach trusted cleanup instead of forcing a proof-destroying
	// SIGKILL.
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			retryErr := command.Process.Signal(syscall.SIGCONT)
			if errors.Is(retryErr, os.ErrProcessDone) {
				return
			}
		}
	}()
	return err
}
