package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

func TestCollectDeletesOnlyExpiredKnownScratchData(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "kanban.db"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "terminal scratch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteTask(ctx, scratch.Task.ID, store.CompletionInput{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	worktree, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "preserved worktree", WorkspaceKind: model.WorkspaceWorktree})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteTask(ctx, worktree.Task.ID, store.CompletionInput{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, _ := manager.WorkspaceRoot("default")
	logsRoot, _ := manager.LogsRoot("default")
	for _, path := range []string{filepath.Join(workspaceRoot, scratch.Task.ID), filepath.Join(workspaceRoot, worktree.Task.ID), filepath.Join(workspaceRoot, "unknown"), logsRoot} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldLog, newLog := filepath.Join(logsRoot, "old.log"), filepath.Join(logsRoot, "new.log")
	if err := os.WriteFile(oldLog, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newLog, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, path := range []string{filepath.Join(workspaceRoot, scratch.Task.ID), filepath.Join(workspaceRoot, worktree.Task.ID), filepath.Join(workspaceRoot, "unknown"), oldLog} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Collect(ctx, manager, "default", Options{EventRetentionDays: 0, LogRetentionDays: 7, WorkspaceRetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.LogsDeleted) != 1 || result.LogsDeleted[0] != oldLog || len(result.WorkspacesDeleted) != 1 || result.WorkspacesDeleted[0] != filepath.Join(workspaceRoot, scratch.Task.ID) {
		t.Fatalf("unexpected GC result: %+v", result)
	}
	for _, preserved := range []string{newLog, filepath.Join(workspaceRoot, worktree.Task.ID), filepath.Join(workspaceRoot, "unknown")} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatalf("GC removed preserved path %s: %v", preserved, err)
		}
	}
}
