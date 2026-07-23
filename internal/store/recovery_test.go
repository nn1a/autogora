package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestRecoverRunBlockedIsAtomicIdempotentAndReleasesLease(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "recovery.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "preserve partial work", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RequestRunCompletion(ctx, scope, CompletionInput{Summary: "pending completion"}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.AcquireWorkspaceLease(ctx, scope, filepath.Join(t.TempDir(), "shared")); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(ctx, `CREATE TRIGGER reject_blocked_recovery
		BEFORE UPDATE OF status ON tasks
		WHEN OLD.status = 'running' AND NEW.status = 'blocked'
		BEGIN SELECT RAISE(ABORT, 'forced blocked-task failure'); END`); err != nil {
		t.Fatal(err)
	}
	reason := "partial changes remain; inspect them before unblocking"
	input := RecoverBlockedRunInput{Outcome: model.RunStatusCrashed, Reason: reason, Kind: model.BlockKindNeedsInput}
	if _, err := opened.RecoverRunBlocked(ctx, claim.Run.ID, input); err == nil || !strings.Contains(err.Error(), "forced blocked-task failure") {
		t.Fatalf("recovery error = %v, want injected task-update failure", err)
	}

	afterFailure, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Task.Status != model.TaskStatusRunning || afterFailure.Task.CurrentRunID == nil ||
		afterFailure.Runs[0].Status != model.RunStatusRunning || len(afterFailure.TerminalRequests) != 1 {
		t.Fatalf("failed recovery exposed a partial terminal state: %#v", afterFailure)
	}
	leases, err := opened.ListResourceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].RunID != claim.Run.ID {
		t.Fatalf("failed recovery released the workspace lease: %#v", leases)
	}
	if _, err := opened.db.ExecContext(ctx, "DROP TRIGGER reject_blocked_recovery"); err != nil {
		t.Fatal(err)
	}

	stopClaims := make(chan struct{})
	claimErrors := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			select {
			case <-stopClaims:
				return
			default:
			}
			candidate, claimErr := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
			if claimErr != nil {
				select {
				case claimErrors <- claimErr:
				default:
				}
				return
			}
			if candidate != nil {
				select {
				case claimErrors <- assertError("task was reclaimed during atomic recovery"):
				default:
				}
				return
			}
		}
	}()
	recovered, err := opened.RecoverRunBlocked(ctx, claim.Run.ID, input)
	close(stopClaims)
	wait.Wait()
	select {
	case err := <-claimErrors:
		t.Fatal(err)
	default:
	}
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Task.Status != model.TaskStatusBlocked || recovered.Task.CurrentRunID != nil ||
		recovered.Task.BlockKind == nil || *recovered.Task.BlockKind != model.BlockKindNeedsInput ||
		recovered.Task.BlockReason == nil || *recovered.Task.BlockReason != reason || recovered.Runs[0].Status != model.RunStatusCrashed ||
		len(recovered.TerminalRequests) != 0 {
		t.Fatalf("unexpected recovered state: %#v", recovered)
	}
	leases, err = opened.ListResourceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("terminal recovery retained workspace leases: %#v", leases)
	}
	eventCount := len(recovered.Events)
	again, err := opened.RecoverRunBlocked(ctx, claim.Run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != eventCount {
		t.Fatalf("idempotent recovery appended events: before=%d after=%d", eventCount, len(again.Events))
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
