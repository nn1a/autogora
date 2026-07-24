package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/store"
)

func unavailableMutationCapability() automaticMutationCapability {
	return automaticMutationCapability{
		Available:         false,
		UnsupportedReason: "test platform has no descendant teardown proof",
	}
}

func TestAutomaticWorkerMutationPolicyAllowsOnlyContainedOrIsolatedWork(t *testing.T) {
	dir, worktree := "/project", "worktree"
	tests := []struct {
		name           string
		task           model.Task
		existing       *model.RunWorkspace
		defaultWorkdir bool
		allowWrites    bool
		wantBlocked    bool
	}{
		{
			name:        "scratch writes remain isolated",
			task:        model.Task{WorkspaceKind: model.WorkspaceScratch},
			allowWrites: true,
		},
		{
			name: "read only directory",
			task: model.Task{
				Workspace: &dir, WorkspaceKind: model.WorkspaceDir,
			},
		},
		{
			name: "writable directory",
			task: model.Task{
				Workspace: &dir, WorkspaceKind: model.WorkspaceDir,
			},
			allowWrites: true, wantBlocked: true,
		},
		{
			name: "read only worktree still needs host Git",
			task: model.Task{
				Workspace: &worktree, WorkspaceKind: model.WorkspaceWorktree,
			},
			wantBlocked: true,
		},
		{
			name: "writable worktree",
			task: model.Task{
				Workspace: &worktree, WorkspaceKind: model.WorkspaceWorktree,
			},
			allowWrites: true, wantBlocked: true,
		},
		{
			name: "existing worktree",
			task: model.Task{WorkspaceKind: model.WorkspaceScratch},
			existing: &model.RunWorkspace{
				Kind: model.WorkspaceWorktree,
			},
			wantBlocked: true,
		},
		{
			name:           "writable board default",
			task:           model.Task{WorkspaceKind: model.WorkspaceScratch},
			defaultWorkdir: true, allowWrites: true, wantBlocked: true,
		},
		{
			name:           "read only board default is resolved at prepare boundary",
			task:           model.Task{WorkspaceKind: model.WorkspaceScratch},
			defaultWorkdir: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, blocked := automaticWorkerMutationTarget(
				test.task,
				test.existing,
				test.defaultWorkdir,
				test.allowWrites,
			)
			if blocked != test.wantBlocked {
				t.Fatalf(
					"blocked=%t target=%q, want blocked=%t",
					blocked,
					target,
					test.wantBlocked,
				)
			}
			if blocked && strings.TrimSpace(target) == "" {
				t.Fatal("blocked policy returned no diagnostic target")
			}
		})
	}
}

func TestUnsupportedAutomaticClaimPersistsCapabilityBlockBeforeWorkspace(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee, requested := "worker", "worktree"
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "must not prepare a worktree", Status: model.TaskStatusReady,
		Runtime: model.RuntimeCodex, Assignee: &assignee,
		Workspace: &requested, WorkspaceKind: model.WorkspaceWorktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID, WorkerID: "capability-test",
	})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}

	blocked, err := blockUnsupportedAutomaticClaim(
		ctx,
		manager,
		opened,
		claim,
		Options{AllowWrites: false},
		unavailableMutationCapability(),
	)
	if err != nil || !blocked {
		t.Fatalf("blocked=%t err=%v", blocked, err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Task.BlockReason == nil {
		t.Fatalf("capability block has no reason: %+v", inspection)
	}
	if !capabilityBlockApplied(
		inspection,
		*inspection.Task.BlockReason,
	) ||
		!strings.Contains(
			*inspection.Task.BlockReason,
			automaticMutationCapabilityCode,
		) {
		t.Fatalf("inspection=%+v", inspection)
	}
	if workspace, err := opened.GetRunWorkspace(
		ctx,
		claim.Run.ID,
	); err != nil || workspace != nil {
		t.Fatalf("workspace=%+v err=%v", workspace, err)
	}
}

func TestCapabilityBlockFinalizesManagedClaimWithoutAWorker(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "managed capability boundary", Status: model.TaskStatusReady,
		Runtime: model.RuntimeCodex, Assignee: &assignee,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID, WorkerID: "capability-test",
	})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	scope := store.RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, false); err != nil {
		t.Fatal(err)
	}
	reason := automaticMutationCapabilityFailure(
		unavailableMutationCapability(),
		"automatic Git worktree creation",
	).Error()
	if err := persistAutomaticMutationCapabilityBlock(
		ctx,
		opened,
		scope,
		reason,
	); err != nil {
		t.Fatal(err)
	}
	inspection, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !capabilityBlockApplied(inspection, reason) ||
		inspection.Run.PID != nil {
		t.Fatalf("inspection=%+v", inspection)
	}
}

func TestUnsupportedAutomaticPublicationFailsDurablyWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t,
		manager,
		"default",
		boards.PublicationModeLocalFF,
		false,
		true,
	)
	_, changeSet := createCompletedFinalizerChangeSet(
		t,
		manager,
		"default",
		"unsupported-capability",
		"ready",
	)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	metadata, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	publications, err := ensureBoardPublications(
		ctx,
		opened,
		"default",
		metadata.Orchestration.Autopilot.Publication,
	)
	if err != nil || len(publications) != 1 {
		t.Fatalf("publications=%+v err=%v", publications, err)
	}
	calls := 0
	options := publicationTestOptions(
		current,
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			calls++
			return publisher.Result{}, nil
		},
	)
	acquired, err := executePublicationWithCapability(
		ctx,
		opened,
		publications[0],
		options,
		unavailableMutationCapability(),
	)
	if !acquired || err == nil {
		t.Fatalf("acquired=%t err=%v", acquired, err)
	}
	if !errors.Is(
		err,
		processguard.ErrAutomaticMutationContainmentUnavailable,
	) {
		t.Fatalf("publication error lost capability type: %v", err)
	}
	if calls != 0 {
		t.Fatalf("executor calls=%d", calls)
	}
	failed, err := opened.GetPublicationByChangeSet(ctx, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.PublicationFailed ||
		failed.Error == nil ||
		!strings.Contains(*failed.Error, automaticMutationCapabilityCode) {
		t.Fatalf("publication=%+v", failed)
	}

	// A failed capability diagnostic is not an automatic candidate. A later
	// pass must leave it failed instead of creating a hot retry loop.
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runPublicationPass(
		ctx,
		manager,
		[]string{"default"},
		options,
		&publicationRuntimeState{},
		current,
	); err != nil {
		t.Fatal(err)
	}
	after := publicationForChangeSet(
		t,
		manager,
		"default",
		changeSet.ID,
	)
	if after.Status != model.PublicationFailed || calls != 0 {
		t.Fatalf("publication=%+v executor calls=%d", after, calls)
	}
}

func TestManualPublicationHasNoAutomaticMutationCapabilityGate(t *testing.T) {
	err := automaticPublicationCapabilityFailure(
		model.Publication{Mode: model.PublicationModeManual},
		unavailableMutationCapability(),
	)
	if err != nil {
		t.Fatalf("manual publication was capability-gated: %v", err)
	}
}

func TestUnsupportedRecoveryBlocksOnlyWorktreeHostGit(t *testing.T) {
	for _, test := range []struct {
		name      string
		workspace *model.RunWorkspace
		wantBlock bool
	}{
		{name: "no workspace"},
		{
			name:      "scratch",
			workspace: &model.RunWorkspace{Kind: model.WorkspaceScratch},
		},
		{
			name:      "directory inspection",
			workspace: &model.RunWorkspace{Kind: model.WorkspaceDir},
		},
		{
			name:      "worktree checkpoint",
			workspace: &model.RunWorkspace{Kind: model.WorkspaceWorktree},
			wantBlock: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := automaticRecoveryCapabilityFailure(
				test.workspace,
				unavailableMutationCapability(),
			)
			if (err != nil) != test.wantBlock {
				t.Fatalf("error=%v want block=%t", err, test.wantBlock)
			}
			if err != nil && !errors.Is(
				err,
				processguard.ErrAutomaticMutationContainmentUnavailable,
			) {
				t.Fatalf("recovery error lost capability type: %v", err)
			}
		})
	}
}
