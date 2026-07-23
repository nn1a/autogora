//go:build linux

package runcontrol

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/processidentity"
)

func TestVerifiedProcessSignalsUsePidfd(t *testing.T) {
	tests := []struct {
		name   string
		signal func(*int, *string) bool
	}{
		{name: "graceful", signal: SignalRunProcess},
		{name: "force", signal: ForceKillRunProcess},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=TestRunControlProcessHelper")
			command.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if waited {
					return
				}
				_ = command.Process.Kill()
				_, _ = command.Process.Wait()
			}()

			identity, err := processidentity.Capture(command.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			pid := command.Process.Pid
			if !test.signal(&pid, &identity) {
				t.Fatal("verified process was not signaled through its process handle")
			}
			done := make(chan error, 1)
			go func() {
				_, err := command.Process.Wait()
				done <- err
			}()
			select {
			case <-done:
				waited = true
			case <-time.After(5 * time.Second):
				t.Fatal("signaled helper process did not exit")
			}
		})
	}
}
