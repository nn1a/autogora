//go:build darwin

package runcontrol

// Darwin does not expose a stable process handle suitable for signaling an
// arbitrary persisted PID. Refuse the signal instead of introducing a
// check-then-kill race that could terminate a process which reused the PID.
func signalVerifiedProcess(_ int, _ *string, _ bool) bool {
	return false
}
