package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func testManager(t *testing.T) (*boards.Manager, *store.Store) {
	t.Helper()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	return manager, opened
}

func TestScratchWorkspaceIsBoundAndCleanedOnlyAtTrustedPath(t *testing.T) {
	ctx := context.Background()
	manager, opened := testManager(t)
	defer opened.Close()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "scratch", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	prepared, err := New(manager).Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workspace == nil || prepared.Workspace.Kind != model.WorkspaceScratch {
		t.Fatalf("scratch workspace not bound: %+v", prepared.Workspace)
	}
	if prepared.Task.Task.Workspace != nil {
		t.Fatalf("run path overwrote task workspace intent: %+v", prepared.Task.Task)
	}
	if info, err := os.Stat(prepared.Workspace.Path); err != nil || !info.IsDir() {
		t.Fatalf("scratch workspace missing: %v", err)
	}
	if changed, err := New(manager).HasChanges(ctx, *prepared.Workspace); err != nil || changed {
		t.Fatalf("fresh scratch workspace reported changes: changed=%v err=%v", changed, err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "result.txt"), []byte("result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := New(manager).HasChanges(ctx, *prepared.Workspace); err != nil || !changed {
		t.Fatalf("scratch changes were not detected: changed=%v err=%v", changed, err)
	}
	if err := opened.MarkRunManaged(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}); err != nil {
		t.Fatal(err)
	}
	completed, err := opened.CompleteRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.CompletionInput{Summary: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusRunning {
		t.Fatalf("prepared run completed before process-exit finalization: %s", completed.Task.Status)
	}
	completed, err = opened.FinalizeRunTerminal(ctx, claim.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := New(manager).Cleanup(completed.Task.Board, *prepared.Workspace)
	if err != nil || !removed {
		t.Fatalf("cleanup: removed=%v err=%v", removed, err)
	}
	outside := t.TempDir()
	prepared.Workspace.Path = outside
	if _, err := New(manager).Cleanup(completed.Task.Board, *prepared.Workspace); err == nil {
		t.Fatal("cleanup accepted an untrusted path")
	}
}

func TestDirWorkspaceRejectsAmbiguousRelativePath(t *testing.T) {
	ctx := context.Background()
	manager, opened := testManager(t)
	defer opened.Close()
	relative := "relative/project"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "dir", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex, Workspace: &relative, WorkspaceKind: model.WorkspaceDir})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := New(manager).Prepare(ctx, opened, claim); err == nil {
		t.Fatal("relative dir workspace was accepted")
	}
}

func TestWritableDirWorkspaceIsLeasedUntilOwningRunEnds(t *testing.T) {
	ctx := context.Background()
	manager, opened := testManager(t)
	defer opened.Close()
	directory := t.TempDir()
	assignee := "worker"
	first, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first writer", Assignee: &assignee, Runtime: model.RuntimeCodex, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second writer", Assignee: &assignee, Runtime: model.RuntimeCodex, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: first.Task.ID})
	secondClaim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: second.Task.ID})
	workspaces := New(manager)
	workspaces.SetAllowWrites(true)
	if _, err := workspaces.Prepare(ctx, opened, firstClaim); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.Prepare(ctx, opened, secondClaim); !errors.Is(err, store.ErrResourceBusy) {
		t.Fatalf("concurrent writer was not rejected: %v", err)
	}
	if _, err := opened.FailRun(ctx, store.RunScope{RunID: firstClaim.Run.ID, ClaimToken: firstClaim.ClaimToken}, "finished", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.Prepare(ctx, opened, secondClaim); err != nil {
		t.Fatalf("released workspace lease was not reusable: %v", err)
	}
	leases, err := opened.ListResourceLeases(ctx)
	if err != nil || len(leases) != 1 || leases[0].RunID != secondClaim.Run.ID {
		t.Fatalf("unexpected workspace leases: %+v err=%v", leases, err)
	}
}

func TestGitBoardCreatesPreservedWorktreeWithBranch(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Autogora", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Autogora", "GIT_COMMITTER_EMAIL=test@example.com")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-m", "fixture")
	manager, err := boards.NewManager(filepath.Join(directory, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	branch := "autogora/test-worktree"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "worktree", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex, WorkspaceKind: model.WorkspaceWorktree, Branch: &branch})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	prepared, err := New(manager).Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workspace == nil || prepared.Workspace.Kind != model.WorkspaceWorktree || prepared.Workspace.BaseCommit == nil || prepared.Workspace.RepositoryPath == nil {
		t.Fatalf("worktree not bound: %+v", prepared.Workspace)
	}
	if prepared.Task.Task.Workspace != nil {
		t.Fatalf("generated path overwrote task workspace intent: %+v", prepared.Task.Task)
	}
	if _, err := os.Stat(filepath.Join(prepared.Workspace.Path, "README.md")); err != nil {
		t.Fatalf("worktree content missing: %v", err)
	}
	if changed, err := New(manager).HasChanges(ctx, *prepared.Workspace); err != nil || changed {
		t.Fatalf("fresh worktree reported changes: changed=%v err=%v", changed, err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Workspace.Path, "partial.txt"), []byte("partial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := New(manager).HasChanges(ctx, *prepared.Workspace); err != nil || !changed {
		t.Fatalf("worker changes were not detected: changed=%v err=%v", changed, err)
	}
	if command := exec.Command("git", "-C", prepared.Workspace.Path, "symbolic-ref", "-q", "HEAD"); command.Run() == nil {
		t.Fatal("worker worktree checked out a shared branch instead of a detached commit")
	}
	reprepared, err := New(manager).Prepare(ctx, opened, claim)
	if err != nil || reprepared.Workspace == nil || reprepared.Workspace.Path != prepared.Workspace.Path {
		t.Fatalf("same run did not reuse its persisted workspace: %+v err=%v", reprepared, err)
	}
	if _, err := opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "retry", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || secondClaim == nil {
		t.Fatalf("second claim: %v %v", secondClaim, err)
	}
	second, err := New(manager).Prepare(ctx, opened, secondClaim)
	if err != nil {
		t.Fatal(err)
	}
	if second.Workspace == nil || second.Workspace.Kind != model.WorkspaceWorktree || second.Workspace.Path == prepared.Workspace.Path || second.Task.Task.Workspace != nil {
		t.Fatalf("retry did not receive a distinct worktree while preserving task intent: %+v", second)
	}
	if removed, err := New(manager).Cleanup(prepared.Task.Task.Board, *prepared.Workspace); err != nil || removed {
		t.Fatalf("worktree must be preserved: removed=%v err=%v", removed, err)
	}
}

func TestExplicitWorktreeRootAllocatesOneDirectoryPerRun(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Autogora", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Autogora", "GIT_COMMITTER_EMAIL=test@example.com")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-m", "fixture")

	manager, err := boards.NewManager(filepath.Join(directory, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	root := filepath.Join(directory, "explicit-worktrees")
	intent := "worktree:" + root
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "retryable explicit worktree", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Workspace: &intent, WorkspaceKind: model.WorkspaceWorktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || firstClaim == nil {
		t.Fatalf("first claim: %#v %v", firstClaim, err)
	}
	first, err := New(manager).Prepare(ctx, opened, firstClaim)
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace == nil || first.Workspace.Path != filepath.Join(root, firstClaim.Run.ID) {
		t.Fatalf("first explicit worktree = %#v", first.Workspace)
	}
	if err := os.WriteFile(filepath.Join(first.Workspace.Path, "preserved.txt"), []byte("first attempt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FailRun(ctx, store.RunScope{RunID: firstClaim.Run.ID, ClaimToken: firstClaim.ClaimToken}, "retry", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || secondClaim == nil {
		t.Fatalf("second claim: %#v %v", secondClaim, err)
	}
	second, err := New(manager).Prepare(ctx, opened, secondClaim)
	if err != nil {
		t.Fatal(err)
	}
	if second.Workspace == nil || second.Workspace.Path != filepath.Join(root, secondClaim.Run.ID) || second.Workspace.Path == first.Workspace.Path {
		t.Fatalf("second explicit worktree = %#v", second.Workspace)
	}
	if _, err := os.Stat(filepath.Join(first.Workspace.Path, "preserved.txt")); err != nil {
		t.Fatalf("first attempt was not preserved: %v", err)
	}
}

func stringValue(value string) *string { return &value }
