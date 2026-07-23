package store

import (
	"context"
	"sync"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestGraphRevisionTracksTopologyMutationsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := opened.CreateTask(ctx, CreateTaskInput{Title: "child"})
	if err != nil {
		t.Fatal(err)
	}
	assertRevision := func(want int64) {
		t.Helper()
		state, err := opened.GetBoardGraphState(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		if state.Revision != want {
			t.Fatalf("graph revision = %d, want %d", state.Revision, want)
		}
	}

	assertRevision(0)
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(1)
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(1)
	if _, err := opened.UnlinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(2)
	if _, err := opened.UnlinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(2)

	position := 0
	if _, err := opened.SetSubtaskParent(ctx, parent.Task.ID, child.Task.ID, &position); err != nil {
		t.Fatal(err)
	}
	assertRevision(3)
	if _, err := opened.SetSubtaskParent(ctx, parent.Task.ID, child.Task.ID, &position); err != nil {
		t.Fatal(err)
	}
	assertRevision(3)
	position = 2
	if _, err := opened.SetSubtaskParent(ctx, parent.Task.ID, child.Task.ID, &position); err != nil {
		t.Fatal(err)
	}
	assertRevision(4)
	if _, err := opened.RemoveSubtask(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(5)
	if _, err := opened.RemoveSubtask(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(5)

	graph, err := opened.RelationshipGraph(ctx, parent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if graph.GraphRevision != 5 {
		t.Fatalf("relationship graph revision = %d, want 5", graph.GraphRevision)
	}

	linkedChild, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "created with dependency", Parents: []string{parent.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevision(6)
	if err := opened.DeleteTask(ctx, linkedChild.Task.ID); err != nil {
		t.Fatal(err)
	}
	assertRevision(7)
}

func TestApplyTaskGraphBumpsRevisionOnceForWholeGraph(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	root, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "triage root", Body: "split this", Status: model.TaskStatusTriage,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := opened.ApplyTaskGraph(ctx, TaskGraphInput{
		RootTaskID: root.Task.ID, RootTitle: "planned", RootBody: "planned body",
		FinalizerAssignee: "coordinator", FinalizerRuntime: model.RuntimeCodex,
		Nodes: []TaskGraphNode{
			{Key: "a", Title: "A", Body: "A", Assignee: "worker", Runtime: model.RuntimeCodex},
			{Key: "b", Title: "B", Body: "B", Assignee: "worker", Runtime: model.RuntimeCodex},
			{Key: "review", Title: "Review", Body: "review", Assignee: "reviewer", Runtime: model.RuntimeClaude, WorkflowRole: model.WorkflowRoleReviewer},
		},
		Dependencies: []TaskGraphDependency{{Parent: "a", Child: "review"}, {Parent: "b", Child: "review"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RelationshipGraph.GraphRevision != 1 {
		t.Fatalf("ApplyTaskGraph revision = %d, want 1", result.RelationshipGraph.GraphRevision)
	}
	state, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 {
		t.Fatalf("stored graph revision = %d, want 1", state.Revision)
	}
}

func TestConcurrentTopologyMutationsKeepMonotonicExactRevision(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(t.TempDir()+"/autogora.db", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	const children = 12
	childIDs := make([]string, 0, children)
	for index := 0; index < children; index++ {
		child, err := opened.CreateTask(ctx, CreateTaskInput{Title: "child"})
		if err != nil {
			t.Fatal(err)
		}
		childIDs = append(childIDs, child.Task.ID)
	}

	runLinks := func(ids []string) {
		t.Helper()
		var wait sync.WaitGroup
		errors := make(chan error, len(ids))
		for _, childID := range ids {
			childID := childID
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := opened.LinkTasks(ctx, parent.Task.ID, childID)
				errors <- err
			}()
		}
		wait.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	runLinks(childIDs)
	state, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != children {
		t.Fatalf("concurrent graph revision = %d, want %d", state.Revision, children)
	}

	// Retrying the same idempotent mutations concurrently must not consume
	// additional revisions.
	runLinks(childIDs)
	state, err = opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != children {
		t.Fatalf("duplicate concurrent links advanced revision to %d", state.Revision)
	}
}
