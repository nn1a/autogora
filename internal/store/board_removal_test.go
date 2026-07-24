package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestBoardRemovalGuardsBlockNewClaimsAndGlobalLeases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	local, err := Open(filepath.Join(root, "alpha.db"), "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := local.CreateTask(ctx, CreateTaskInput{
		Title: "wait behind removal guard", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	localGuard, err := local.AcquireBoardRemovalGuard(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if claim, err := local.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID}); claim != nil || !errors.Is(err, ErrBoardRemovalInProgress) {
		t.Fatalf("claim behind removal guard = %+v, %v", claim, err)
	}
	if released, err := local.ReleaseBoardRemovalGuard(ctx, localGuard); err != nil || !released {
		t.Fatalf("release local guard = %v, %v", released, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	coordination, err := Open(filepath.Join(root, "coordination.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	coordinationGuard, err := coordination.AcquireBoardRemovalGuard(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	_, _, agentErr := coordination.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "planner", Limit: 1, OwnerKind: AgentSlotOwnerPlanner,
		Board: "alpha", OwnerID: "planner:test", TTL: time.Minute, Current: time.Now(),
	})
	if !errors.Is(agentErr, ErrBoardRemovalInProgress) {
		t.Fatalf("agent lease behind removal guard error = %v", agentErr)
	}
	_, _, workspaceErr := coordination.AcquireGlobalWorkspaceLease(
		ctx, "alpha", "run-test", filepath.Join(root, "workspace"),
	)
	if !errors.Is(workspaceErr, ErrBoardRemovalInProgress) {
		t.Fatalf("workspace lease behind removal guard error = %v", workspaceErr)
	}
	if released, err := coordination.ReleaseBoardRemovalGuard(ctx, coordinationGuard); err != nil || !released {
		t.Fatalf("release coordination guard = %v, %v", released, err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinationRemovalGuardAndLeaseAcquisitionAreAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "coordination.db")
	guardStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer guardStore.Close()
	leaseStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()

	for index := 0; index < 20; index++ {
		board := "race-" + string(rune('a'+index))
		agent := "planner-" + board
		start := make(chan struct{})
		guardResult := make(chan struct {
			guard BoardRemovalGuard
			err   error
		}, 1)
		leaseResult := make(chan struct {
			slot     GlobalAgentSlot
			acquired bool
			err      error
		}, 1)
		go func() {
			<-start
			guard, err := guardStore.AcquireBoardRemovalGuard(ctx, board)
			guardResult <- struct {
				guard BoardRemovalGuard
				err   error
			}{guard: guard, err: err}
		}()
		go func() {
			<-start
			slot, acquired, err := leaseStore.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
				AgentID: agent, Limit: 1, OwnerKind: AgentSlotOwnerPlanner,
				Board: board, OwnerID: "owner-" + board, TTL: time.Minute, Current: time.Now(),
			})
			leaseResult <- struct {
				slot     GlobalAgentSlot
				acquired bool
				err      error
			}{slot: slot, acquired: acquired, err: err}
		}()
		close(start)
		guardOutcome := <-guardResult
		leaseOutcome := <-leaseResult
		guardWon := guardOutcome.err == nil
		leaseWon := leaseOutcome.err == nil && leaseOutcome.acquired
		if guardWon == leaseWon {
			t.Fatalf("iteration %d: guard=%+v/%v lease=%+v/%v/%v",
				index, guardOutcome.guard, guardOutcome.err, leaseOutcome.slot, leaseOutcome.acquired, leaseOutcome.err)
		}
		if guardWon {
			if !errors.Is(leaseOutcome.err, ErrBoardRemovalInProgress) {
				t.Fatalf("iteration %d: losing lease error = %v", index, leaseOutcome.err)
			}
			if released, err := guardStore.ReleaseBoardRemovalGuard(ctx, guardOutcome.guard); err != nil || !released {
				t.Fatalf("iteration %d: release guard = %v, %v", index, released, err)
			}
		} else {
			if !errors.Is(guardOutcome.err, ErrBoardBusy) {
				t.Fatalf("iteration %d: losing guard error = %v", index, guardOutcome.err)
			}
			if released, err := leaseStore.ReleaseGlobalAgentSlot(ctx, leaseOutcome.slot); err != nil || !released {
				t.Fatalf("iteration %d: release lease = %v, %v", index, released, err)
			}
		}
	}
}

func TestLocalRemovalGuardBlocksOrdinaryStoreWrites(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "alpha.db"), "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := opened.SetAgentHealth(ctx, SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthRateLimited, CooldownUntil: &expired,
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := opened.AcquireBoardRemovalGuard(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := opened.CreateTask(ctx, CreateTaskInput{Title: "late write"}); !errors.Is(err, ErrBoardRemovalInProgress) {
		t.Fatalf("create behind guard error = %v", err)
	}
	if _, err := opened.SetAgentHealth(ctx, SetAgentHealthInput{
		AgentID: "worker", Status: model.AgentHealthReady,
	}); !errors.Is(err, ErrBoardRemovalInProgress) {
		t.Fatalf("agent health write behind guard error = %v", err)
	}
	if _, err := opened.ClearExpiredAgentCooldowns(ctx, time.Now()); !errors.Is(err, ErrBoardRemovalInProgress) {
		t.Fatalf("cooldown cleanup behind guard error = %v", err)
	}

	released, err := opened.ReleaseBoardRemovalGuard(ctx, guard)
	if err != nil || !released {
		t.Fatalf("release exact local guard = %v, %v", released, err)
	}
	if _, err := opened.CreateTask(ctx, CreateTaskInput{Title: "write after release"}); err != nil {
		t.Fatalf("write after guard release: %v", err)
	}
}

func TestLocalRemovalGuardCountsWholeDatabase(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "alpha.db"), "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "mismatched legacy task", Board: "beta", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID, Board: "beta"})
	if err != nil || claim == nil {
		t.Fatalf("claim mismatched task: %+v, %v", claim, err)
	}
	if _, err := opened.AcquireBoardRemovalGuard(ctx, "alpha"); !errors.Is(err, ErrBoardBusy) {
		t.Fatalf("guard ignored active run with mismatched board value: %v", err)
	}
}

func TestLocalRemovalGuardRejectsPublishingPublication(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "alpha.db"), "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, changeSet := createPublicationSource(
		t,
		opened,
		"board_removal",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if err != nil || !acquired {
		t.Fatalf(
			"claim publication = %+v, acquired=%v, err=%v",
			claimed,
			acquired,
			err,
		)
	}

	_, err = opened.AcquireBoardRemovalGuard(ctx, "alpha")
	var busy *BoardBusyError
	if !errors.As(err, &busy) ||
		busy.PublishingPublications != 1 ||
		!strings.Contains(err.Error(), "1 publishing publication(s)") {
		t.Fatalf("publishing board removal error = %#v", err)
	}
	preserved, err := opened.GetPublication(ctx, claimed.ID)
	if err != nil || preserved.Status != model.PublicationPublishing {
		t.Fatalf(
			"publishing evidence = %+v, err=%v",
			preserved,
			err,
		)
	}

	if _, err := opened.FailPublication(
		ctx,
		claimed.ID,
		FailPublicationInput{
			ExpectedUpdatedAt: claimed.UpdatedAt,
			ClaimToken:        claimed.ClaimToken,
			ClaimEpoch:        claimed.ClaimEpoch,
			Error:             "terminal for removal",
		},
	); err != nil {
		t.Fatal(err)
	}
	guard, err := opened.AcquireBoardRemovalGuard(ctx, "alpha")
	if err != nil {
		t.Fatalf("guard after terminal publication: %v", err)
	}
	if released, err := opened.ReleaseBoardRemovalGuard(
		ctx,
		guard,
	); err != nil || !released {
		t.Fatalf(
			"release guard after terminal publication = %v, %v",
			released,
			err,
		)
	}
}
