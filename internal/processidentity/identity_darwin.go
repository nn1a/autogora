//go:build darwin

package processidentity

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func rawProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func snapshot(pid int) (string, bool, error) {
	if pid <= 0 {
		return "", false, nil
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if !rawProcessAlive(pid) {
			return "", false, nil
		}
		return "", true, err
	}
	if process == nil || process.Proc.P_pid != int32(pid) || process.Proc.P_stat == 0 {
		return "", false, nil
	}
	started := process.Proc.P_starttime
	return fmt.Sprintf("darwin:%d:%d", started.Sec, started.Usec), true, nil
}
