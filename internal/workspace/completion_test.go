package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func directCompletionFixture(t *testing.T) (*Manager, *store.Store, *model.ClaimedTask, string) {
	t.Helper()
	ctx := context.Background()
	manager, opened := testManager(t)
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "init", "-q")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "add", "README.md")
	runWorkspaceGit(t, repository, "commit", "-q", "-m", "base")
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	assignee := "external-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "direct worktree completion", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	workspaces := New(manager)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	return workspaces, opened, prepared, repository
}

func TestDirectWorktreeCompletionCapturesChangeSetBeforeDone(t *testing.T) {
	ctx := context.Background()
	workspaces, opened, prepared, repository := directCompletionFixture(t)
	defer opened.Close()
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "direct.txt"), []byte("captured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completed, err := workspaces.CompleteRun(ctx, opened, store.RunScope{RunID: prepared.Run.ID, ClaimToken: prepared.ClaimToken}, store.CompletionInput{Summary: "direct completion"})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone || len(completed.ChangeSets) != 1 || completed.ChangeSets[0].State != "ready" {
		t.Fatalf("direct completion omitted its change set: %#v", completed)
	}
	changeSet := completed.ChangeSets[0]
	contents := runWorkspaceGit(t, repository, "show", changeSet.HeadCommit+":direct.txt")
	if contents != "captured" {
		t.Fatalf("captured contents = %q", contents)
	}
}

func TestManagedWorktreeCompletionWaitsForProcessExit(t *testing.T) {
	ctx := context.Background()
	workspaces, opened, prepared, _ := directCompletionFixture(t)
	defer opened.Close()
	scope := store.RunScope{RunID: prepared.Run.ID, ClaimToken: prepared.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "before.txt"), []byte("before request\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	requested, err := workspaces.CompleteRun(ctx, opened, scope, store.CompletionInput{Summary: "managed completion"})
	if err != nil {
		t.Fatal(err)
	}
	if requested.Task.Status != model.TaskStatusRunning {
		t.Fatalf("managed completion finalized before process exit: %#v", requested.Task)
	}
	if changeSet, err := opened.GetRunChangeSet(ctx, prepared.Run.ID); err != nil || changeSet != nil {
		t.Fatalf("managed completion captured before process exit: %#v err=%v", changeSet, err)
	}
}

func TestDirectCompletionCaptureFailureBlocksWithoutConsumingRetry(t *testing.T) {
	ctx := context.Background()
	workspaces, opened, prepared, _ := directCompletionFixture(t)
	defer opened.Close()
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "partial.txt"), []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(prepared.Workspace.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	_, err := workspaces.CompleteRun(ctx, opened, store.RunScope{RunID: prepared.Run.ID, ClaimToken: prepared.ClaimToken}, store.CompletionInput{Summary: "cannot capture"})
	if err == nil {
		t.Fatal("invalid worktree completion unexpectedly succeeded")
	}
	detail, getErr := opened.GetTask(ctx, prepared.Task.Task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.FailureCount != 0 || detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindNeedsInput || detail.Task.BlockReason == nil || !strings.Contains(*detail.Task.BlockReason, prepared.Workspace.Path) {
		t.Fatalf("capture failure did not preserve and block the run: %#v", detail)
	}
	if contents, readErr := os.ReadFile(filepath.Join(prepared.Workspace.Path, "partial.txt")); readErr != nil || string(contents) != "preserve me\n" {
		t.Fatalf("partial workspace was not preserved: %q err=%v", contents, readErr)
	}
}
