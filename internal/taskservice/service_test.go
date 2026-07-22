package taskservice

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
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

func TestSharedServiceUsesBoardProfilesForExplicitDecomposition(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	profiles := []boards.Profile{{Name: "coder", Runtime: model.RuntimeCodex, Description: "implements changes"}}
	defaultProfile := "coder"
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles, DefaultProfile: store.OptionalString{Set: true, Value: &defaultProfile}}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	rootTask, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Build feature", Body: "Split the work", Status: model.TaskStatusTriage})
	if err != nil {
		t.Fatal(err)
	}
	plan := &orchestration.DecompositionPlan{
		Fanout: true, RootTitle: "Build and verify feature", RootBody: "Complete every child and synthesize the result.", Reason: "Separate implementation",
		Tasks: []orchestration.DecompositionTask{{Key: "implementation", Title: "Implement feature", Body: "Implement and test it.", Assignee: "coder", Runtime: model.RuntimeGemini, Priority: 8}},
	}
	result, err := New(opened, manager, "default").DecomposeTask(ctx, rootTask.Task.ID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Fanout || result.Graph == nil || len(result.Graph.ChildIDs) != 1 {
		t.Fatalf("decomposition graph missing: %#v", result)
	}
	childDetail, err := opened.GetTask(ctx, result.Graph.ChildIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	child := childDetail.Task
	if child.Assignee == nil || *child.Assignee != "coder" || child.Runtime != model.RuntimeCodex {
		t.Fatalf("board profile was not applied to child: %#v", child)
	}
}
