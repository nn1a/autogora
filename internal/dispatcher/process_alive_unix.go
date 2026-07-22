//go:build !windows

package dispatcher

import "syscall"

func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
