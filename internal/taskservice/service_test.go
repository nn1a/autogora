package taskservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestBoardContextMergesStoredAndConfiguredProfiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "autogora.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	configured := []boards.Profile{{Name: "reviewer", Runtime: model.RuntimeGemini, Description: "reviews changes"}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &configured}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	assignee := "reviewer"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Review", Assignee: &assignee, Runtime: model.RuntimeCodex}); err != nil {
		t.Fatal(err)
	}
	worker := "implementer"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Implement", Assignee: &worker, Runtime: model.RuntimeClaude}); err != nil {
		t.Fatal(err)
	}

	board, err := New(opened, manager, "default").BoardContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Profiles) != 2 {
		t.Fatalf("expected two profiles, got %#v", board.Profiles)
	}
	if board.Profiles[0].Name != "reviewer" || board.Profiles[0].Runtime != model.RuntimeGemini || board.Profiles[0].Description == "" {
		t.Fatalf("configured profile did not override task-derived route: %#v", board.Profiles[0])
	}
	if board.Profiles[1].Name != "implementer" || board.Profiles[1].Runtime != model.RuntimeClaude {
		t.Fatalf("task-derived profile missing: %#v", board.Profiles[1])
	}
}
