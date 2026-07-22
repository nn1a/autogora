//go:build !windows

package runcontrol

import "syscall"

func signalProcessTree(pid int) bool {
	processGroup, err := syscall.Getpgid(pid)
	if err != nil {
		return false
	}
	if processGroup == pid {
		return syscall.Kill(-processGroup, syscall.SIGTERM) == nil
	}
	return syscall.Kill(pid, syscall.SIGTERM) == nil
}
