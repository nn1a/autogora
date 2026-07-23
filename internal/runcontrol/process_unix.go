//go:build !linux && !darwin && !windows

package runcontrol

// Platforms without a kernel process handle must not signal a persisted PID.
// Checking identity and then signaling by PID would leave a reuse race.
func signalVerifiedProcess(_ int, _ *string, _ bool) bool {
	return false
}
