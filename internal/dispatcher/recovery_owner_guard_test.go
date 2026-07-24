package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestRecoveryOwnershipGuardRenewsWithoutInvalidatingOwnerCredential(
	t *testing.T,
) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "renew recovery owner while inspecting workspace",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID:          task.Task.ID,
		ClaimTTLSeconds: 30,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, err=%v", claim, err)
	}
	scope := store.RunScope{
		RunID:      claim.Run.ID,
		ClaimToken: claim.ClaimToken,
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FenceObservedRunRecovery(
		ctx,
		store.ObserveRunForRecovery(claim.Run, nil),
		30,
		"recovery owner heartbeat test",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	fence, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || fence == nil {
		t.Fatalf("fence = %#v, err=%v", fence, err)
	}
	acknowledged, err := opened.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	owned, acquired, err := opened.ClaimObservedRunRecovery(
		ctx,
		store.ObserveRunForRecovery(current.Run, nil, &acknowledged),
		600*time.Millisecond,
	)
	if err != nil || !acquired {
		t.Fatalf("claim recovery owner: acquired=%v err=%v", acquired, err)
	}
	guard, err := startRecoveryOwnershipGuardWithTTL(
		ctx,
		opened,
		owned,
		600*time.Millisecond,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1400 * time.Millisecond)
	if err := guard.ctx.Err(); err != nil {
		t.Fatalf("recovery owner guard lost its renewed epoch: %v", err)
	}
	// Pass the original owner observation. Its expiry is deliberately stale;
	// only the immutable local owner token/epoch is part of the recovery CAS.
	if err := opened.ValidateObservedRunRecoveryOwnership(
		ctx,
		owned,
	); err != nil {
		t.Fatalf("renewed owner credential became stale: %v", err)
	}
	if err := guard.Stop(); err != nil {
		t.Fatal(err)
	}
}
