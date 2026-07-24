package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func claimManagedLeaseFixture(
	t *testing.T,
	opened *Store,
	ttlSeconds int,
) *model.ClaimedTask {
	t.Helper()
	assignee := "worker"
	task, err := opened.CreateTask(context.Background(), CreateTaskInput{
		Title: "managed lease", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), ClaimOptions{
		TaskID: task.Task.ID, ClaimTTLSeconds: ttlSeconds,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	return claim
}

func TestRenewManagedRunLeaseExtendsScopedClaimWithoutHeartbeatEvent(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "managed-lease.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 1)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	originalExpiry, err := time.Parse(time.RFC3339Nano, claim.Run.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	renewed, err := opened.RenewManagedRunLease(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	renewedExpiry, err := time.Parse(time.RFC3339Nano, renewed.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !renewedExpiry.After(originalExpiry) ||
		renewed.HeartbeatAt == claim.Run.HeartbeatAt {
		t.Fatalf("renewed lease = %#v, original expiry=%s", renewed, originalExpiry)
	}
	var heartbeatEvents int
	if err := opened.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM task_events
		 WHERE task_id = ? AND kind = 'heartbeat'`,
		claim.Task.Task.ID,
	).Scan(&heartbeatEvents); err != nil {
		t.Fatal(err)
	}
	if heartbeatEvents != 0 {
		t.Fatalf("managed lease emitted %d heartbeat events", heartbeatEvents)
	}
}

func TestRenewManagedRunLeaseRejectsUnmanagedAndStaleScopes(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "managed-lease-scope.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimManagedLeaseFixture(t, opened, 60)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if _, err := opened.RenewManagedRunLease(ctx, scope); err == nil ||
		!strings.Contains(err.Error(), "not managed") {
		t.Fatalf("unmanaged renewal error = %v", err)
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, false); err != nil {
		t.Fatal(err)
	}
	countFailure := false
	if _, err := opened.FailRun(
		ctx,
		scope,
		"fixture reclaim",
		FailRunOptions{
			Outcome:      model.RunStatusReclaimed,
			CountFailure: &countFailure,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RenewManagedRunLease(ctx, scope); err == nil ||
		!strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal renewal error = %v", err)
	}
}
