//go:build linux

package processidentity

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func rawProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func snapshot(pid int) (string, bool, error) {
	if pid <= 0 {
		return "", false, nil
	}
	contents, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) || !rawProcessAlive(pid) {
			return "", false, nil
		}
		return "", true, err
	}
	// The comm field is parenthesized and may itself contain spaces or closing
	// parentheses. Fields after its final ')' start with field 3; starttime is
	// field 22 and therefore index 19 in this suffix.
	closing := strings.LastIndexByte(string(contents), ')')
	if closing < 0 {
		return "", true, errors.New("invalid /proc process stat")
	}
	fields := strings.Fields(string(contents[closing+1:]))
	if len(fields) <= 19 {
		return "", true, errors.New("incomplete /proc process stat")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", true, fmt.Errorf("invalid process start tick: %w", err)
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", true, fmt.Errorf("read boot identity: %w", err)
	}
	return "linux:" + strings.TrimSpace(string(bootID)) + ":" + fields[19], true, nil
}
