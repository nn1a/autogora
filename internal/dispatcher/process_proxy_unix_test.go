//go:build !windows

package dispatcher

import (
	"context"
	"testing"
)

func TestWorkerCommandCleanupReleasesProxyProcess(t *testing.T) {
	worker, err := newWorkerCommand(
		context.Background(),
		RunnerCommand{Command: "/bin/true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.cleanup()
	if !worker.guarded {
		t.Fatal("Unix FencedCommand worker enabled numeric process-tree cleanup")
	}
	if err := worker.start(); err != nil {
		t.Fatal(err)
	}
	if released, err := worker.release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := worker.wait(); err != nil {
		t.Fatal(err)
	}
	proxy := worker.child.Process
	if proxy == nil || proxy.Pid <= 0 {
		t.Fatalf("worker proxy process = %#v", proxy)
	}

	worker.cleanup()
	worker.cleanup()
	if proxy.Pid != -1 {
		t.Fatalf("worker proxy PID after cleanup = %d, want -1", proxy.Pid)
	}
}
