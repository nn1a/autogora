//go:build linux

package workspace

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

func installEscapingFakeGit(t *testing.T, wait bool) string {
	t.Helper()
	directory := t.TempDir()
	mode := "exit"
	if wait {
		mode = "wait"
	}
	script := `#!/bin/sh
setsid /bin/sh -c 'trap "" TERM HUP INT; printf "%d\n" "$$" >"$1"; while :; do sleep 1; done' autogora-git-descendant "$AUTOGORA_TEST_GIT_DESCENDANT_PID" </dev/null >/dev/null 2>&1 &
while [ ! -s "$AUTOGORA_TEST_GIT_DESCENDANT_PID" ]; do sleep 0.01; done
if [ "` + mode + `" = wait ]; then
  while :; do sleep 1; done
fi
`
	path := filepath.Join(directory, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func readWorkspaceGuardPID(t *testing.T, path string) int {
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
	t.Fatalf("fake Git descendant did not write PID to %s", path)
	return 0
}

func requireWorkspaceGuardPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("host Git descendant %d remained alive", pid)
}

func TestCommandOutputCleansEscapedGitDescendantAfterNormalExit(t *testing.T) {
	installEscapingFakeGit(t, false)
	pidPath := filepath.Join(t.TempDir(), "git-descendant.pid")
	t.Setenv("AUTOGORA_TEST_GIT_DESCENDANT_PID", pidPath)
	if _, err := commandOutput(context.Background(), "git", "--version"); err != nil {
		t.Fatal(err)
	}
	requireWorkspaceGuardPIDGone(t, readWorkspaceGuardPID(t, pidPath))
}

func TestCommandOutputCleansEscapedGitDescendantAfterCancellation(t *testing.T) {
	installEscapingFakeGit(t, true)
	pidPath := filepath.Join(t.TempDir(), "git-descendant.pid")
	t.Setenv("AUTOGORA_TEST_GIT_DESCENDANT_PID", pidPath)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := commandOutput(ctx, "git", "--version")
		result <- err
	}()
	descendantPID := readWorkspaceGuardPID(t, pidPath)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled host Git command unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host Git guard did not finish teardown after cancellation")
	}
	requireWorkspaceGuardPIDGone(t, descendantPID)
}
