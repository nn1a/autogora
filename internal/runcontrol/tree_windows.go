//go:build windows

package runcontrol

// ProcessTreeAlive cannot inspect descendants from a persisted PID on Windows
// without retaining the original Job Object handle.
func ProcessTreeAlive(_ *int) bool {
	return false
}
