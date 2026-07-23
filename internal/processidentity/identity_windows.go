//go:build windows

package processidentity

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func rawProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	return windows.GetExitCodeProcess(handle, &exitCode) == nil && exitCode == 259
}

func snapshot(pid int) (string, bool, error) {
	if pid <= 0 {
		return "", false, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if !rawProcessAlive(pid) {
			return "", false, nil
		}
		return "", true, err
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return "", true, err
	}
	if exitCode != 259 {
		return "", false, nil
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", true, err
	}
	return fmt.Sprintf("windows:%d", created.Nanoseconds()), true, nil
}
