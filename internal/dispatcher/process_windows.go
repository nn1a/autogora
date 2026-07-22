//go:build windows

package dispatcher

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd, _ bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
