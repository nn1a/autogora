package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/store"
)

func TestBoardGraphEndpointUsesExplicitBoardAndArchiveScope(t *testing.T) {
	ctx := context.Background()
	server := startTestServer(t)
	for _, board := range []string{"graph-alpha", "graph-beta"} {
		if _, err := server.manager.Create(ctx, board, boards.Update{}); err != nil {
			t.Fatal(err)
		}
	}

	alpha, err := server.manager.OpenStore(ctx, "graph-alpha")
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := alpha.CreateTask(ctx, store.CreateTaskInput{Title: "alpha prerequisite"})
	if err != nil {
		alpha.Close()
		t.Fatal(err)
	}
	dependent, err := alpha.CreateTask(ctx, store.CreateTaskInput{Title: "alpha dependent"})
	if err != nil {
		alpha.Close()
		t.Fatal(err)
	}
	archived, err := alpha.CreateTask(ctx, store.CreateTaskInput{Title: "alpha archived"})
	if err != nil {
		alpha.Close()
		t.Fatal(err)
	}
	if _, err := alpha.LinkTasks(ctx, prerequisite.Task.ID, dependent.Task.ID); err != nil {
		alpha.Close()
		t.Fatal(err)
	}
	if _, err := alpha.ArchiveTask(ctx, archived.Task.ID); err != nil {
		alpha.Close()
		t.Fatal(err)
	}
	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}

	beta, err := server.manager.OpenStore(ctx, "graph-beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.CreateTask(ctx, store.CreateTaskInput{Title: "beta only"}); err != nil {
		beta.Close()
		t.Fatal(err)
	}
	if err := beta.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.manager.Switch("graph-beta"); err != nil {
		t.Fatal(err)
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/graph?board=graph-alpha", nil)
	graph := mapValue(t, value)
	if response.StatusCode != http.StatusOK || graph["board"] != "graph-alpha" ||
		graph["includeArchived"] != false || graph["totalNodes"] != float64(2) ||
		graph["returnedNodes"] != float64(2) || graph["nodeLimit"] != float64(store.BoardRelationshipGraphNodeLimit) {
		t.Fatalf("unexpected alpha graph response: %d %#v", response.StatusCode, graph)
	}
	dependencies := arrayValue(t, graph["dependencies"])
	if len(dependencies) != 1 {
		t.Fatalf("alpha dependency missing: %#v", graph)
	}
	dependency := mapValue(t, dependencies[0])
	if dependency["prerequisiteId"] != prerequisite.Task.ID || dependency["dependentId"] != dependent.Task.ID {
		t.Fatalf("dependency direction mismatch: %#v", dependency)
	}
	for _, raw := range arrayValue(t, graph["nodes"]) {
		node := mapValue(t, raw)
		task := mapValue(t, node["task"])
		if task["board"] != "graph-alpha" || task["title"] == "beta only" {
			t.Fatalf("current board leaked into explicit graph: %#v", graph)
		}
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/graph?board=graph-alpha&includeArchived=true", nil)
	withArchived := mapValue(t, value)
	if response.StatusCode != http.StatusOK || withArchived["includeArchived"] != true ||
		withArchived["totalNodes"] != float64(3) {
		t.Fatalf("archived graph scope mismatch: %d %#v", response.StatusCode, withArchived)
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/graph", nil)
	current := mapValue(t, value)
	if response.StatusCode != http.StatusOK || current["board"] != "graph-beta" ||
		current["totalNodes"] != float64(1) {
		t.Fatalf("current board graph mismatch: %d %#v", response.StatusCode, current)
	}

	unauthorized, err := http.Get(server.URL + "/api/graph?board=" + url.QueryEscape("graph-alpha"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, unauthorized.Body)
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized graph status = %d, want %d", unauthorized.StatusCode, http.StatusUnauthorized)
	}

	methodResponse, _ := apiRequest(t, server, http.MethodPost, "/api/graph?board=graph-alpha", map[string]any{})
	if methodResponse.StatusCode != http.StatusMethodNotAllowed ||
		methodResponse.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("graph POST response = %d allow=%q",
			methodResponse.StatusCode, methodResponse.Header.Get("Allow"))
	}
	notFound, _ := apiRequest(t, server, http.MethodGet, "/api/graph/extra?board=graph-alpha", nil)
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("nested graph endpoint status = %d, want %d", notFound.StatusCode, http.StatusNotFound)
	}
}
