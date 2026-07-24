//go:build linux

package processguard

import (
	"os/exec"
	"runtime"
)

// startOwnedProcess keeps the OS thread that created the guard alive until the
// guard exits. Linux delivers a parent-death signal when that creating thread
// terminates, so a short-lived Go runtime launcher thread cannot become a
// false parent-lifetime boundary.
func startOwnedProcess(command *exec.Cmd) (<-chan error, error) {
	started := make(chan error, 1)
	waited := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		err := command.Start()
		started <- err
		if err != nil {
			close(waited)
			return
		}
		waited <- command.Wait()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return waited, nil
}
