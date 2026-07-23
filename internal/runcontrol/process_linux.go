//go:build linux

package runcontrol

import (
	"strings"

	"github.com/nn1a/autogora/internal/processidentity"
	"golang.org/x/sys/unix"
)

func signalVerifiedProcess(pid int, expectedIdentity *string, force bool) bool {
	if expectedIdentity == nil || strings.TrimSpace(*expectedIdentity) == "" {
		return false
	}
	// Open the pidfd before validating the identity, then signal through that
	// same descriptor. Even if the numeric PID is reused after validation, the
	// descriptor continues to refer only to the process opened here.
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer unix.Close(pidfd)

	state := processidentity.Inspect(pid, expectedIdentity)
	if !state.Alive || !state.Verified || !state.Matches {
		return false
	}
	signal := unix.SIGTERM
	if force {
		signal = unix.SIGKILL
	}
	return unix.PidfdSendSignal(pidfd, signal, nil, 0) == nil
}
