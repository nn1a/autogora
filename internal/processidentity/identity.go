package processidentity

import (
	"errors"
	"strings"
)

// State describes whether a PID is occupied and whether it still identifies
// the exact process captured when an Autogora worker was spawned.
type State struct {
	Alive    bool
	Verified bool
	Matches  bool
}

// Capture returns a stable OS process-start identity for a newly spawned
// worker. A PID alone is not an identity because operating systems reuse it.
func Capture(pid int) (string, error) {
	identity, alive, err := snapshot(pid)
	if err != nil {
		return "", err
	}
	if !alive || strings.TrimSpace(identity) == "" {
		return "", errors.New("process is no longer running")
	}
	return identity, nil
}

// Inspect never treats an occupied PID as verified unless the current OS
// process-start identity can be compared with the persisted value.
func Inspect(pid int, expected *string) State {
	identity, alive, err := snapshot(pid)
	state := State{Alive: alive}
	if !alive || err != nil || expected == nil || strings.TrimSpace(*expected) == "" || strings.TrimSpace(identity) == "" {
		return state
	}
	state.Verified = true
	state.Matches = identity == strings.TrimSpace(*expected)
	return state
}
