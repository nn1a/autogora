package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

func testManager(t *testing.T) (*boards.Manager, *store.Store) {
	t.Helper()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "taskcircuit.db"))
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
	if prepared.Task.Task.WorkspaceKind != model.WorkspaceScratch || prepared.Task.Task.Workspace == nil {
		t.Fatalf("scratch workspace not bound: %+v", prepared.Task.Task)
	}
	if info, err := os.Stat(*prepared.Task.Task.Workspace); err != nil || !info.IsDir() {
		t.Fatalf("scratch workspace missing: %v", err)
	}
	completed, err := opened.CompleteRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.CompletionInput{Summary: "done"})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := New(manager).Cleanup(completed.Task)
	if err != nil || !removed {
		t.Fatalf("cleanup: removed=%v err=%v", removed, err)
	}
	outside := t.TempDir()
	completed.Task.Workspace = &outside
	if _, err := New(manager).Cleanup(completed.Task); err == nil {
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

func TestGitBoardCreatesPreservedWorktreeWithBranch(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=TaskCircuit", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=TaskCircuit", "GIT_COMMITTER_EMAIL=test@example.com")
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
	manager, err := boards.NewManager(filepath.Join(directory, "taskcircuit.db"))
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
	branch := "taskcircuit/test-worktree"
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
	if prepared.Task.Task.WorkspaceKind != model.WorkspaceWorktree || prepared.Task.Task.Workspace == nil {
		t.Fatalf("worktree not bound: %+v", prepared.Task.Task)
	}
	if _, err := os.Stat(filepath.Join(*prepared.Task.Task.Workspace, "README.md")); err != nil {
		t.Fatalf("worktree content missing: %v", err)
	}
	if removed, err := New(manager).Cleanup(prepared.Task.Task); err != nil || removed {
		t.Fatalf("worktree must be preserved: removed=%v err=%v", removed, err)
	}
}

func stringValue(value string) *string { return &value }
