package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/store"
)

func createVersionedDashboardTask(t *testing.T, server *Server, title string) (string, string) {
	t.Helper()
	response, value := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": title})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create %q = %d %#v", title, response.StatusCode, value)
	}
	task := mapValue(t, mapValue(t, value)["task"])
	return task["id"].(string), task["updatedAt"].(string)
}

func TestWebTerminalActionsRejectStaleTaskVersions(t *testing.T) {
	server := startTestServer(t)
	tests := []struct {
		name   string
		method string
		suffix string
		body   map[string]any
	}{
		{name: "drag complete", method: http.MethodPatch, body: map[string]any{"status": "done", "summary": "finished"}},
		{name: "drawer block", method: http.MethodPost, suffix: "/block", body: map[string]any{"reason": "needs review", "kind": "needs_input"}},
		{name: "drawer archive", method: http.MethodPost, suffix: "/archive", body: map[string]any{}},
		{name: "drawer delete", method: http.MethodDelete, body: map[string]any{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			taskID, stale := createVersionedDashboardTask(t, server, test.name)
			time.Sleep(2 * time.Millisecond)
			latestResponse, latest := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{
				"expectedUpdatedAt": stale, "title": "latest " + test.name,
			})
			if latestResponse.StatusCode != http.StatusOK {
				t.Fatalf("prepare latest task = %d %#v", latestResponse.StatusCode, latest)
			}
			test.body["expectedUpdatedAt"] = stale
			response, value := apiRequest(t, server, test.method, "/api/tasks/"+taskID+test.suffix, test.body)
			if response.StatusCode != http.StatusConflict || !strings.Contains(mapValue(t, value)["error"].(string), "refresh") {
				t.Fatalf("stale %s = %d %#v, want refresh conflict", test.name, response.StatusCode, value)
			}
			getResponse, loaded := apiRequest(t, server, http.MethodGet, "/api/tasks/"+taskID, nil)
			task := mapValue(t, mapValue(t, loaded)["task"])
			if getResponse.StatusCode != http.StatusOK || task["title"] != "latest "+test.name || task["status"] != "todo" {
				t.Fatalf("stale %s changed task: %d %#v", test.name, getResponse.StatusCode, loaded)
			}
		})
	}
}

func TestWebBulkMutationReportsPerTaskVersionConflicts(t *testing.T) {
	server := startTestServer(t)
	freshID, freshVersion := createVersionedDashboardTask(t, server, "fresh bulk")
	staleID, staleVersion := createVersionedDashboardTask(t, server, "stale bulk")
	time.Sleep(2 * time.Millisecond)
	response, value := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+staleID, map[string]any{
		"expectedUpdatedAt": staleVersion, "title": "newer stale bulk",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare stale bulk = %d %#v", response.StatusCode, value)
	}

	response, value = apiRequest(t, server, http.MethodPost, "/api/tasks/bulk", map[string]any{
		"ids": []any{freshID, staleID},
		"mutation": map[string]any{
			"status": "review",
			"expectedUpdatedAt": map[string]any{
				freshID: freshVersion,
				staleID: staleVersion,
			},
		},
	})
	result := mapValue(t, value)
	if response.StatusCode != http.StatusOK || len(arrayValue(t, result["ok"])) != 1 || len(arrayValue(t, result["errors"])) != 1 {
		t.Fatalf("partial bulk response = %d %#v", response.StatusCode, value)
	}
	conflict := mapValue(t, arrayValue(t, result["errors"])[0])
	if conflict["id"] != staleID || !strings.Contains(conflict["error"].(string), "conflict") {
		t.Fatalf("bulk conflict = %#v, want %s", conflict, staleID)
	}
}

func TestWebPlannerActionsRejectStaleTaskVersions(t *testing.T) {
	server := startTestServer(t)
	tests := []struct {
		name   string
		suffix string
		body   map[string]any
	}{
		{name: "specify", suffix: "/specify", body: map[string]any{
			"title": "planned title", "body": "planned body",
		}},
		{name: "decompose", suffix: "/decompose", body: map[string]any{
			"plan": map[string]any{
				"fanout": false, "rootTitle": "planned title", "rootBody": "planned body",
				"reason": "one task", "tasks": []any{}, "dependencies": []any{},
			},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, value := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{
				"title": "rough " + test.name, "status": "triage",
			})
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("create triage = %d %#v", response.StatusCode, value)
			}
			task := mapValue(t, mapValue(t, value)["task"])
			taskID, stale := task["id"].(string), task["updatedAt"].(string)
			time.Sleep(2 * time.Millisecond)
			response, value = apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{
				"expectedUpdatedAt": stale, "title": "latest " + test.name,
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("prepare latest triage = %d %#v", response.StatusCode, value)
			}
			test.body["expectedUpdatedAt"] = stale
			response, value = apiRequest(t, server, http.MethodPost, "/api/tasks/"+taskID+test.suffix, test.body)
			if response.StatusCode != http.StatusConflict || !strings.Contains(mapValue(t, value)["error"].(string), "refresh") {
				t.Fatalf("stale planner action = %d %#v", response.StatusCode, value)
			}
		})
	}
}

func TestBoardSnapshotReportsTruncatedTaskWindow(t *testing.T) {
	server := startTestServer(t)
	opened, err := server.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	for index := 0; index < dashboardTaskLimit+1; index++ {
		if _, err := opened.CreateTask(context.Background(), store.CreateTaskInput{Title: fmt.Sprintf("window task %03d", index)}); err != nil {
			t.Fatal(err)
		}
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/board", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("board snapshot = %d %#v", response.StatusCode, value)
	}
	snapshot := mapValue(t, value)
	window := mapValue(t, snapshot["taskWindow"])
	if window["returned"] != float64(dashboardTaskLimit) || window["total"] != float64(dashboardTaskLimit+1) ||
		window["truncated"] != true || window["limit"] != float64(dashboardTaskLimit) {
		t.Fatalf("task window = %#v", window)
	}
	if len(arrayValue(t, snapshot["tasks"])) != dashboardTaskLimit {
		t.Fatalf("snapshot task count = %d, want %d", len(arrayValue(t, snapshot["tasks"])), dashboardTaskLimit)
	}
}
