package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
)

func connectTestServer(t *testing.T, server *mcp.Server) (*mcp.ClientSession, <-chan error) {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- server.Run(context.Background(), serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "taskcircuit-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return session, done
}

func closeTestServer(t *testing.T, session *mcp.ClientSession, done <-chan error) {
	t.Helper()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MCP server did not stop")
	}
}

func toolJSON(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any, output any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool %s failed: %+v", name, result.Content)
	}
	if output != nil {
		if len(result.Content) == 0 {
			t.Fatalf("tool %s returned no content", name)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("tool %s returned non-text content: %T", name, result.Content[0])
		}
		if err := json.Unmarshal([]byte(text.Text), output); err != nil {
			t.Fatalf("decode %s output %q: %v", name, text.Text, err)
		}
	}
	return result
}

func TestCoreMCPToolsShareBoardAndBoundedContext(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "project", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	server, _ := New(manager, "test")
	session, done := connectTestServer(t, server)
	defer closeTestServer(t, session, done)
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"kanban_create", "kanban_list", "kanban_show", "kanban_context", "kanban_graph", "kanban_diagnostics", "kanban_complete", "kanban_unlink", "kanban_schedule", "kanban_notify_subscribe", "kanban_specify", "kanban_decompose", "kanban_profile_describe_auto", "kanban_swarm"} {
		found := false
		for _, tool := range tools.Tools {
			if tool.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("MCP tool missing: %s", expected)
		}
	}
	var created struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "kanban_create", map[string]any{"board": "project", "title": "MCP task", "assignee": "worker", "runtime": "codex"}, &created)
	if created.Task.Status != model.TaskStatusReady {
		t.Fatalf("unexpected task: %+v", created.Task)
	}
	var shown struct {
		Task              model.Task              `json:"task"`
		RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
		WorkerContext     string                  `json:"workerContext"`
	}
	toolJSON(t, session, "kanban_show", map[string]any{"board": "project", "task_id": created.Task.ID}, &shown)
	if shown.RelationshipGraph.FocusTaskID != created.Task.ID || shown.WorkerContext == "" {
		t.Fatalf("show omitted execution context: %+v", shown)
	}
	var claimed struct {
		Run        model.Run `json:"run"`
		ClaimToken string    `json:"claimToken"`
	}
	toolJSON(t, session, "kanban_claim", map[string]any{"board": "project", "task_id": created.Task.ID}, &claimed)
	if claimed.Run.Status != model.RunStatusRunning || claimed.ClaimToken == "" {
		t.Fatalf("claim output mismatch: %+v", claimed)
	}

	scopedServer, scopedService := New(manager, "test")
	scopedEnvironment := map[string]string{
		"KANBAN_BOARD": "project", "KANBAN_TASK_ID": created.Task.ID,
		"KANBAN_RUN_ID": claimed.Run.ID, "KANBAN_CLAIM_TOKEN": claimed.ClaimToken,
	}
	scopedService.getenv = func(name string) string { return scopedEnvironment[name] }
	scopedSession, scopedDone := connectTestServer(t, scopedServer)
	defer closeTestServer(t, scopedSession, scopedDone)
	var scopedShow struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, scopedSession, "kanban_show", map[string]any{}, &scopedShow)
	if scopedShow.Task.ID != created.Task.ID || scopedShow.Task.Status != model.TaskStatusRunning {
		t.Fatalf("scoped worker read wrong task: %+v", scopedShow.Task)
	}
	forbidden, err := scopedSession.CallTool(ctx, &mcp.CallToolParams{Name: "kanban_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !forbidden.IsError {
		t.Fatal("scoped worker was allowed to list the board")
	}
	toolJSON(t, scopedSession, "kanban_comment", map[string]any{"body": "MCP progress", "author": "worker"}, nil)
	toolJSON(t, scopedSession, "kanban_complete", map[string]any{"summary": "MCP completed", "metadata": map[string]any{"verification": []string{"mcp"}}}, nil)
	var completed struct {
		Task model.Task  `json:"task"`
		Runs []model.Run `json:"runs"`
	}
	toolJSON(t, session, "kanban_show", map[string]any{"board": "project", "task_id": created.Task.ID}, &completed)
	if completed.Task.Status != model.TaskStatusDone || len(completed.Runs) != 1 || completed.Runs[0].Status != model.RunStatusCompleted {
		t.Fatalf("scoped MCP lifecycle did not complete shared task: %+v", completed)
	}
}

func TestMCPExplicitSpecificationAndDecomposition(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "default", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	server, _ := New(manager, "test")
	session, done := connectTestServer(t, server)
	defer closeTestServer(t, session, done)

	var rough struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "kanban_create", map[string]any{"title": "rough", "status": "triage"}, &rough)
	var specified struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "kanban_specify", map[string]any{"task_id": rough.Task.ID, "title": "Precise", "body": "Acceptance: pass"}, &specified)
	if specified.Task.Status != model.TaskStatusTodo || specified.Task.Title != "Precise" {
		t.Fatalf("unexpected specified task: %+v", specified.Task)
	}

	var root struct {
		Task model.Task `json:"task"`
	}
	toolJSON(t, session, "kanban_create", map[string]any{"title": "rough graph", "status": "triage"}, &root)
	plan := map[string]any{
		"fanout": true, "rootTitle": "Coordinate", "rootBody": "Verify output", "reason": "parallel",
		"tasks":        []any{map[string]any{"key": "child", "title": "Implement", "body": "Deliver", "assignee": "worker", "runtime": "codex", "priority": 1, "skills": []any{}}},
		"dependencies": []any{},
	}
	var decomposed struct {
		Fanout bool `json:"fanout"`
		Graph  struct {
			ChildIDs []string `json:"childIds"`
		} `json:"graph"`
	}
	toolJSON(t, session, "kanban_decompose", map[string]any{"task_id": root.Task.ID, "default_profile": map[string]any{"name": "worker", "runtime": "codex"}, "plan": plan}, &decomposed)
	if !decomposed.Fanout || len(decomposed.Graph.ChildIDs) != 1 {
		t.Fatalf("unexpected decomposition: %+v", decomposed)
	}
}
