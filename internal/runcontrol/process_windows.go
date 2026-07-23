//go:build windows

package runcontrol

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

func signalVerifiedProcess(pid int, expectedIdentity *string, _ bool) bool {
	const stillActive = 259

	if expectedIdentity == nil || strings.TrimSpace(*expectedIdentity) == "" {
		return false
	}
	// Identity validation and termination deliberately use the same kernel
	// handle. A PID can be reused, but an open process handle cannot silently
	// change which process it references.
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if windows.GetExitCodeProcess(handle, &exitCode) != nil || exitCode != stillActive {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil {
		return false
	}
	if fmt.Sprintf("windows:%d", created.Nanoseconds()) != strings.TrimSpace(*expectedIdentity) {
		return false
	}
	return windows.TerminateProcess(handle, 1) == nil
}
