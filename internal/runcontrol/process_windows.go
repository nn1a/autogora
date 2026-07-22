//go:build windows

package runcontrol

import "os"

func signalProcessTree(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Kill() == nil
}
