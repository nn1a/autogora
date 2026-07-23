package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func openBoardGraphTestStore(t *testing.T) *Store {
	t.Helper()
	opened, err := Open(t.TempDir()+"/autogora.db", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

func boardGraphNodeByTitle(t *testing.T, graph model.BoardRelationshipGraph, title string) model.RelationshipNode {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.Task.Title == title {
			return node
		}
	}
	t.Fatalf("board graph is missing task %q: %+v", title, graph.Nodes)
	return model.RelationshipNode{}
}

func TestBoardRelationshipGraphEmptySnapshot(t *testing.T) {
	ctx := context.Background()
	opened := openBoardGraphTestStore(t)

	graph, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Board != "default" || graph.IncludeArchived || graph.GraphRevision != 0 ||
		graph.TotalNodes != 0 || graph.ReturnedNodes != 0 ||
		graph.NodeLimit != BoardRelationshipGraphNodeLimit ||
		graph.Truncated || graph.OmittedNodeCount != 0 || graph.TotalPhases != 0 {
		t.Fatalf("unexpected empty board graph: %+v", graph)
	}
	if graph.Nodes == nil || graph.Hierarchy == nil || graph.Dependencies == nil {
		t.Fatalf("empty board graph collections must encode as arrays: %+v", graph)
	}
}

func TestBoardRelationshipGraphIncludesDisconnectedTasksAndSeparatesEdges(t *testing.T) {
	ctx := context.Background()
	opened := openBoardGraphTestStore(t)
	create := func(title string) model.TaskDetail {
		t.Helper()
		detail, err := opened.CreateTask(ctx, CreateTaskInput{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return detail
	}

	prerequisite := create("prerequisite")
	dependent := create("dependent")
	subtask := create("subtask")
	create("isolated")
	archived := create("archived")
	if _, err := opened.LinkTasks(ctx, prerequisite.Task.ID, dependent.Task.ID); err != nil {
		t.Fatal(err)
	}
	position := 3
	if _, err := opened.SetSubtaskParent(ctx, prerequisite.Task.ID, subtask.Task.ID, &position); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ArchiveTask(ctx, archived.Task.ID); err != nil {
		t.Fatal(err)
	}

	graph, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if graph.TotalNodes != 4 || graph.ReturnedNodes != 4 || graph.Truncated ||
		graph.OmittedNodeCount != 0 || graph.GraphRevision != 2 || graph.TotalPhases != 2 {
		t.Fatalf("unexpected board graph window: %+v", graph)
	}
	if len(graph.Dependencies) != 1 ||
		graph.Dependencies[0].PrerequisiteID != prerequisite.Task.ID ||
		graph.Dependencies[0].DependentID != dependent.Task.ID {
		t.Fatalf("dependency direction is not prerequisite -> dependent: %+v", graph.Dependencies)
	}
	if len(graph.Hierarchy) != 1 ||
		graph.Hierarchy[0].ParentTaskID != prerequisite.Task.ID ||
		graph.Hierarchy[0].SubtaskID != subtask.Task.ID ||
		graph.Hierarchy[0].Position != position {
		t.Fatalf("hierarchy was not returned separately: %+v", graph.Hierarchy)
	}

	prerequisiteNode := boardGraphNodeByTitle(t, graph, "prerequisite")
	dependentNode := boardGraphNodeByTitle(t, graph, "dependent")
	subtaskNode := boardGraphNodeByTitle(t, graph, "subtask")
	isolatedNode := boardGraphNodeByTitle(t, graph, "isolated")
	if prerequisiteNode.Phase != 0 || !reflect.DeepEqual(prerequisiteNode.Unlocks, []string{dependent.Task.ID}) {
		t.Fatalf("unexpected prerequisite node: %+v", prerequisiteNode)
	}
	if dependentNode.Phase != 1 || !reflect.DeepEqual(dependentNode.BlockedBy, []string{prerequisite.Task.ID}) {
		t.Fatalf("unexpected dependent node: %+v", dependentNode)
	}
	if subtaskNode.ParentTaskID == nil || *subtaskNode.ParentTaskID != prerequisite.Task.ID ||
		subtaskNode.HierarchyDepth == nil || *subtaskNode.HierarchyDepth != 1 {
		t.Fatalf("unexpected subtask node: %+v", subtaskNode)
	}
	if isolatedNode.ParentTaskID != nil || isolatedNode.HierarchyDepth != nil || isolatedNode.Phase != 0 {
		t.Fatalf("disconnected node was not preserved: %+v", isolatedNode)
	}

	repeated, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph, repeated) {
		t.Fatalf("board graph ordering is not deterministic:\nfirst=%+v\nsecond=%+v", graph, repeated)
	}

	withArchived, err := opened.BoardRelationshipGraph(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !withArchived.IncludeArchived || withArchived.TotalNodes != 5 || withArchived.ReturnedNodes != 5 {
		t.Fatalf("archived scope is not explicit: %+v", withArchived)
	}
	if node := boardGraphNodeByTitle(t, withArchived, "archived"); node.Task.Status != model.TaskStatusArchived {
		t.Fatalf("archived task status was lost: %+v", node)
	}

	focused, err := opened.RelationshipGraph(ctx, prerequisite.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if focused.TotalConnectedNodes != 3 || len(focused.Nodes) != 3 {
		t.Fatalf("task-scoped graph unexpectedly included disconnected board nodes: %+v", focused)
	}
}

func TestBoardRelationshipGraphMarksDependencyCycles(t *testing.T) {
	ctx := context.Background()
	opened := openBoardGraphTestStore(t)
	a, err := opened.CreateTask(ctx, CreateTaskInput{Title: "cycle-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := opened.CreateTask(ctx, CreateTaskInput{Title: "cycle-b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CreateTask(ctx, CreateTaskInput{Title: "outside-cycle"}); err != nil {
		t.Fatal(err)
	}

	// Public mutations reject dependency cycles. Insert one transactionally to
	// prove the read model remains diagnostic if a damaged or imported database
	// contains a cycle.
	if err := opened.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO task_links(parent_id, child_id) VALUES (?, ?), (?, ?)",
			a.Task.ID, b.Task.ID, b.Task.ID, a.Task.ID,
		); err != nil {
			return err
		}
		_, err := bumpBoardGraphRevision(ctx, tx, "default")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	graph, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if graph.GraphRevision != 1 || len(graph.Dependencies) != 2 || graph.TotalPhases != 1 {
		t.Fatalf("unexpected cyclic graph summary: %+v", graph)
	}
	if node := boardGraphNodeByTitle(t, graph, "cycle-a"); node.Phase != -1 {
		t.Fatalf("cycle-a phase = %d, want -1", node.Phase)
	}
	if node := boardGraphNodeByTitle(t, graph, "cycle-b"); node.Phase != -1 {
		t.Fatalf("cycle-b phase = %d, want -1", node.Phase)
	}
	if node := boardGraphNodeByTitle(t, graph, "outside-cycle"); node.Phase != 0 {
		t.Fatalf("disconnected acyclic node phase = %d, want 0", node.Phase)
	}
}

func TestBoardRelationshipGraphBoundsLargeBoards(t *testing.T) {
	ctx := context.Background()
	opened := openBoardGraphTestStore(t)
	const total = BoardRelationshipGraphNodeLimit + 5
	if err := opened.withWrite(ctx, func(tx *sql.Tx) error {
		for index := 0; index < total; index++ {
			if _, err := opened.createTask(ctx, tx, CreateTaskInput{
				Title: fmt.Sprintf("task-%03d", index),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	graph, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if graph.TotalNodes != total || graph.ReturnedNodes != BoardRelationshipGraphNodeLimit ||
		len(graph.Nodes) != BoardRelationshipGraphNodeLimit ||
		!graph.Truncated || graph.OmittedNodeCount != 5 {
		t.Fatalf("large graph was not bounded: %+v", graph)
	}
	if graph.Nodes[0].Task.Title != "task-000" ||
		graph.Nodes[len(graph.Nodes)-1].Task.Title != "task-499" {
		t.Fatalf("large graph selection/order is not deterministic: first=%q last=%q",
			graph.Nodes[0].Task.Title, graph.Nodes[len(graph.Nodes)-1].Task.Title)
	}
}

func TestBoardRelationshipGraphRevisionAndTopologyShareSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opened := openBoardGraphTestStore(t)
	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	const children = 24
	childIDs := make([]string, 0, children)
	for index := 0; index < children; index++ {
		child, err := opened.CreateTask(ctx, CreateTaskInput{Title: fmt.Sprintf("child-%02d", index)})
		if err != nil {
			t.Fatal(err)
		}
		childIDs = append(childIDs, child.Task.ID)
	}

	writerDone := make(chan error, 1)
	go func() {
		for _, childID := range childIDs {
			if _, err := opened.LinkTasks(ctx, parent.Task.ID, childID); err != nil {
				writerDone <- err
				return
			}
			runtime.Gosched()
		}
		writerDone <- nil
	}()

	done := false
	for !done {
		graph, err := opened.BoardRelationshipGraph(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if int(graph.GraphRevision) != len(graph.Dependencies) {
			t.Fatalf("mixed graph snapshot: revision=%d dependencies=%d",
				graph.GraphRevision, len(graph.Dependencies))
		}
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		default:
			runtime.Gosched()
		}
	}

	final, err := opened.BoardRelationshipGraph(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if final.GraphRevision != children || len(final.Dependencies) != children {
		t.Fatalf("final graph snapshot mismatch: revision=%d dependencies=%d",
			final.GraphRevision, len(final.Dependencies))
	}
}
