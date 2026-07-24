//go:build linux

package publisher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func publisherChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(value)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publisher child did not write PID to %s", path)
	return 0
}

func requirePublisherChildGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("publisher descendant %d remained alive", pid)
}

func publisherEscapingScript(t *testing.T, wait bool) string {
	t.Helper()
	mode := "exit"
	if wait {
		mode = "wait"
	}
	path := filepath.Join(t.TempDir(), "publisher-command")
	content := `#!/bin/sh
setsid /bin/sh -c 'trap "" TERM HUP INT; printf "%d\n" "$$" >"$1"; while :; do sleep 1; done' autogora-publisher-descendant "$AUTOGORA_TEST_PUBLISHER_CHILD_PID" </dev/null >/dev/null 2>&1 &
while [ ! -s "$AUTOGORA_TEST_PUBLISHER_CHILD_PID" ]; do sleep 0.01; done
if [ "` + mode + `" = wait ]; then
  while :; do sleep 1; done
fi
`
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunnerCleansSetsidDescendantAfterSuccess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "publisher-child.pid")
	t.Setenv("AUTOGORA_TEST_PUBLISHER_CHILD_PID", pidPath)
	if _, err := (ExecRunner{}).Run(
		context.Background(),
		t.TempDir(),
		publisherEscapingScript(t, false),
	); err != nil {
		t.Fatal(err)
	}
	requirePublisherChildGone(t, publisherChildPID(t, pidPath))
}

func TestExecRunnerCleansSetsidDescendantAfterCancellation(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "publisher-child.pid")
	t.Setenv("AUTOGORA_TEST_PUBLISHER_CHILD_PID", pidPath)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	directory := t.TempDir()
	script := publisherEscapingScript(t, true)
	go func() {
		_, err := (ExecRunner{}).Run(
			ctx,
			directory,
			script,
		)
		result <- err
	}()
	childPID := publisherChildPID(t, pidPath)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled Publisher command unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Publisher command did not finish guarded cancellation")
	}
	requirePublisherChildGone(t, childPID)
}
