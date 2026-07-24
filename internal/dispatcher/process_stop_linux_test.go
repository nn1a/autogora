//go:build linux

package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/nn1a/autogora/internal/processguard"
)

func TestGuardedWorkerStopDoesNotUseReleasedProxyProcess(t *testing.T) {
	worker, err := newWorkerCommand(
		context.Background(),
		RunnerCommand{
			Command: "/bin/sleep",
			Args:    []string{"30"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.cleanup()
	if err := worker.start(); err != nil {
		t.Fatal(err)
	}
	if !worker.guarded {
		t.Fatal("Linux worker did not use process guard")
	}
	proxy := worker.child.Process
	if proxy == nil {
		t.Fatal("worker proxy process is nil")
	}
	if err := proxy.Release(); err != nil {
		t.Fatal(err)
	}
	if released, err := worker.release(); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := worker.stop(false); err != nil {
		t.Fatal(err)
	}
	if err := worker.wait(); errors.Is(err, processguard.ErrTeardownUnconfirmed) {
		t.Fatalf("guard stop through private identity lost proof: %v", err)
	}
}
