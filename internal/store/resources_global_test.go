package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGlobalWorkspaceLeaseUsesExactOwnerAndToken(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "coordination.db")
	first, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	boardLocal, err := Open(filepath.Join(t.TempDir(), "alpha.db"), "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer boardLocal.Close()
	path := filepath.Join(t.TempDir(), "workspace")
	if _, _, err := boardLocal.AcquireGlobalWorkspaceLease(ctx, "alpha", "run-a", path); err == nil {
		t.Fatal("board-local store accepted a global coordination lease")
	}

	initial, acquired, err := first.AcquireGlobalWorkspaceLease(ctx, "alpha", "run-a", path)
	if err != nil || !acquired || initial.LeaseToken == "" {
		t.Fatalf("initial lease = %+v, acquired=%v, err=%v", initial, acquired, err)
	}
	repeated, acquired, err := second.AcquireGlobalWorkspaceLease(ctx, "alpha", "run-a", path)
	if err != nil || !acquired || repeated.LeaseToken != initial.LeaseToken {
		t.Fatalf("same owner reacquire = %+v, acquired=%v, err=%v", repeated, acquired, err)
	}
	busy, acquired, err := second.AcquireGlobalWorkspaceLease(ctx, "beta", "run-b", path)
	if err != nil || acquired || busy.Board != "alpha" || busy.RunID != "run-a" {
		t.Fatalf("conflicting lease = %+v, acquired=%v, err=%v", busy, acquired, err)
	}
	wrong := initial
	wrong.LeaseToken = "stale-token"
	if released, err := first.ReleaseGlobalWorkspaceLease(ctx, wrong); err != nil || released {
		t.Fatalf("inexact owner release = %v, %v", released, err)
	}
	if released, err := first.ReleaseGlobalWorkspaceLease(ctx, initial); err != nil || !released {
		t.Fatalf("exact owner release = %v, %v", released, err)
	}
	replacement, acquired, err := second.AcquireGlobalWorkspaceLease(ctx, "beta", "run-b", path)
	if err != nil || !acquired || replacement.LeaseToken == initial.LeaseToken {
		t.Fatalf("replacement lease = %+v, acquired=%v, err=%v", replacement, acquired, err)
	}
	if released, err := first.ReleaseGlobalWorkspaceLease(ctx, initial); err != nil || released {
		t.Fatalf("stale exact owner deleted replacement = %v, %v", released, err)
	}
	leases, err := second.ListGlobalWorkspaceLeases(ctx)
	if err != nil || len(leases) != 1 || leases[0].LeaseToken != replacement.LeaseToken {
		t.Fatalf("replacement ownership was not preserved: %+v, %v", leases, err)
	}
}
