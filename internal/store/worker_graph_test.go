package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestWorkerRelationshipGraphKeepsOnlyLocalTopology(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	create := func(title string) string {
		detail, err := opened.CreateTask(ctx, CreateTaskInput{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return detail.Task.ID
	}
	root, branch, focus := create("root"), create("branch"), create("focus")
	child, sibling := create("child"), create("root sibling")
	prerequisite2, prerequisite1 := create("far prerequisite"), create("direct prerequisite")
	dependent1, dependent2 := create("direct dependent"), create("far dependent")

	for _, relation := range [][2]string{
		{root, branch}, {branch, focus}, {focus, child}, {root, sibling},
	} {
		if _, err := opened.SetSubtaskParent(ctx, relation[0], relation[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, relation := range [][2]string{
		{prerequisite2, prerequisite1}, {prerequisite1, focus},
		{focus, dependent1}, {dependent1, dependent2},
	} {
		if _, err := opened.LinkTasks(ctx, relation[0], relation[1]); err != nil {
			t.Fatal(err)
		}
	}

	full, err := opened.RelationshipGraph(ctx, focus)
	if err != nil {
		t.Fatal(err)
	}
	local, err := opened.WorkerRelationshipGraph(ctx, focus)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{root, branch, focus, child, prerequisite1, dependent1}
	sort.Strings(want)
	got := make([]string, 0, len(local.Nodes))
	selected := map[string]bool{}
	for _, node := range local.Nodes {
		got = append(got, node.Task.ID)
		selected[node.Task.ID] = true
		if node.ParentTaskID != nil && !selectedOrPending(*node.ParentTaskID, want) {
			t.Fatalf("node %s leaks omitted parent %s", node.Task.ID, *node.ParentTaskID)
		}
		for _, references := range [][]string{node.SubtaskIDs, node.BlockedBy, node.Unlocks} {
			for _, reference := range references {
				if !selectedOrPending(reference, want) {
					t.Fatalf("node %s leaks omitted reference %s", node.Task.ID, reference)
				}
			}
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worker node IDs = %v, want %v", got, want)
	}
	if local.FocusTaskID != full.FocusTaskID || local.RootTaskID != full.RootTaskID ||
		local.GraphRevision != full.GraphRevision || local.TotalPhases != full.TotalPhases ||
		local.TotalConnectedNodes != full.TotalConnectedNodes {
		t.Fatalf("worker graph lost full graph metadata:\nfull=%+v\nlocal=%+v", full, local)
	}
	if !local.Truncated || local.OmittedNodeCount != full.TotalConnectedNodes-len(local.Nodes) {
		t.Fatalf("worker truncation = truncated:%v omitted:%d total:%d nodes:%d",
			local.Truncated, local.OmittedNodeCount, local.TotalConnectedNodes, len(local.Nodes))
	}
	for _, edge := range local.Hierarchy {
		if !selected[edge.ParentTaskID] || !selected[edge.SubtaskID] {
			t.Fatalf("hierarchy edge leaks omitted node: %+v", edge)
		}
	}
	for _, edge := range local.Dependencies {
		if !selected[edge.PrerequisiteID] || !selected[edge.DependentID] {
			t.Fatalf("dependency edge leaks omitted node: %+v", edge)
		}
	}
}

func selectedOrPending(id string, selected []string) bool {
	for _, candidate := range selected {
		if candidate == id {
			return true
		}
	}
	return false
}

func TestWorkerTaskDetailRedactsRelatedExecutionData(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	workspace := "/private/neighbor-worktree"
	assignee := "worker"
	parent, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "parent", Body: "neighbor secret body", Workspace: &workspace,
		Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	focus, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "focus", Body: "authorized focus body", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, focus.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetSubtaskParent(ctx, parent.Task.ID, focus.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.WorkerTaskDetail(ctx, focus.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Body != "authorized focus body" || len(detail.Parents) != 1 || detail.ParentTask == nil {
		t.Fatalf("worker detail lost focus or relationships: %+v", detail)
	}
	for _, related := range []model.Task{detail.Parents[0], *detail.ParentTask} {
		if related.Body != "" || related.Workspace != nil || related.Result != nil ||
			related.BlockReason != nil || related.CurrentRunID != nil {
			t.Fatalf("worker detail leaked related execution data: %+v", related)
		}
		if related.ID != parent.Task.ID || related.Title != parent.Task.Title {
			t.Fatalf("worker detail lost routing metadata: %+v", related)
		}
	}
}

func TestBuildWorkerContextReservesFocusAndRootBeforeNodeLimit(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	root, err := opened.CreateTask(ctx, CreateTaskInput{Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	focus, err := opened.CreateTask(ctx, CreateTaskInput{Title: "focus", Body: "current work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetSubtaskParent(ctx, root.Task.ID, focus.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 55; index++ {
		prerequisite, err := opened.CreateTask(ctx, CreateTaskInput{Title: fmt.Sprintf("prerequisite %02d", index)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := opened.LinkTasks(ctx, prerequisite.Task.ID, focus.Task.ID); err != nil {
			t.Fatal(err)
		}
	}
	contextText, err := opened.BuildWorkerContext(ctx, focus.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextText, focus.Task.ID+" (subtask of "+root.Task.ID+") focus ← current") ||
		!strings.Contains(contextText, root.Task.ID+" (root) root") {
		t.Fatalf("bounded worker context omitted focus or root:\n%s", contextText)
	}
	if !strings.Contains(contextText, "7 additional related node(s) omitted") {
		t.Fatalf("bounded worker context reported the wrong omitted count:\n%s", contextText)
	}
}
