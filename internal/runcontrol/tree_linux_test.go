//go:build linux

package runcontrol

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestProcessTreeAliveChecksDedicatedProcessGroup(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestRunControlProcessHelper")
	command.Env = append(os.Environ(), "AUTOGORA_RUNCONTROL_HELPER=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if !ProcessTreeAlive(&pid) {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		t.Fatal("dedicated worker process group was not detected")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := command.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	if ProcessTreeAlive(&pid) {
		t.Fatal("empty worker process group was reported alive")
	}
}
