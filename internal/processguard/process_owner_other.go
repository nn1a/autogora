//go:build !linux

package processguard

import "os/exec"

func startOwnedProcess(command *exec.Cmd) (<-chan error, error) {
	if err := command.Start(); err != nil {
		return nil, err
	}
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()
	return waited, nil
}
