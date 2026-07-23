//go:build !windows

package runcontrol

import (
	"errors"
	"syscall"
)

// ProcessTreeAlive conservatively reports whether the worker's process group
// still contains a process after its recorded leader exits. It never sends a
// signal. A reused process-group ID can produce a false positive, which is
// safer than releasing ownership while a descendant may still be writing.
func ProcessTreeAlive(pid *int) bool {
	if pid == nil || *pid <= 0 {
		return false
	}
	err := syscall.Kill(-*pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
