package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/store"
)

func createRunnableDashboardTask(t *testing.T, server *Server, title string) (string, string) {
	t.Helper()
	response, value := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{
		"title": title, "status": "ready", "assignee": "worker", "runtime": "codex",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create runnable task = %d %#v", response.StatusCode, value)
	}
	task := mapValue(t, mapValue(t, value)["task"])
	return task["id"].(string), task["updatedAt"].(string)
}

func TestTargetedDispatchRejectsAlreadyStaleVersion(t *testing.T) {
	server := startTestServer(t)
	taskID, stale := createRunnableDashboardTask(t, server, "stale request")
	response, value := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{
		"expectedUpdatedAt": stale,
		"title":             "newer request",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prepare newer version = %d %#v", response.StatusCode, value)
	}

	response, value = apiRequest(t, server, http.MethodPost, "/api/dispatch", map[string]any{
		"taskId": taskID, "expectedUpdatedAt": stale,
	})
	message, _ := mapValue(t, value)["error"].(string)
	if response.StatusCode != http.StatusConflict || !strings.Contains(message, "refresh before dispatching") {
		t.Fatalf("stale dispatch = %d %#v, want refresh conflict", response.StatusCode, value)
	}
	server.operationsMu.Lock()
	operationCount := len(server.operations)
	server.operationsMu.Unlock()
	if operationCount != 0 {
		t.Fatalf("stale request created %d async operation(s)", operationCount)
	}
}

func TestTargetedDispatchRechecksVersionAtAsyncClaimBoundary(t *testing.T) {
	server := startTestServer(t)
	taskID, expected := createRunnableDashboardTask(t, server, "claim boundary")
	dispatchStarted := make(chan struct{})
	continueClaim := make(chan struct{})
	claimResult := make(chan bool, 1)
	receivedVersion := ""

	server.runDispatcher = func(ctx context.Context, options dispatcher.Options) error {
		if options.ExpectedUpdatedAt != nil {
			receivedVersion = *options.ExpectedUpdatedAt
		}
		close(dispatchStarted)
		<-continueClaim
		opened, err := server.manager.OpenStore(ctx, options.Board)
		if err != nil {
			claimResult <- false
			return err
		}
		defer opened.Close()
		claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
			TaskID: options.TaskID, Board: options.Board,
			ExpectedUpdatedAt: options.ExpectedUpdatedAt,
			WorkerID:          "dashboard-race-test", ExcludeManual: true,
		})
		claimResult <- claim != nil
		return err
	}

	response, value := apiRequest(t, server, http.MethodPost, "/api/dispatch", map[string]any{
		"taskId": taskID, "expectedUpdatedAt": expected,
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("initial dispatch = %d %#v", response.StatusCode, value)
	}
	operationID := mapValue(t, mapValue(t, value)["operation"])["id"].(string)
	select {
	case <-dispatchStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher hook did not start")
	}
	if receivedVersion != expected {
		t.Fatalf("dispatcher expectedUpdatedAt = %q, want %q", receivedVersion, expected)
	}

	response, value = apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{
		"expectedUpdatedAt": expected,
		"title":             "changed after request validation",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("concurrent task edit = %d %#v", response.StatusCode, value)
	}
	close(continueClaim)
	select {
	case claimed := <-claimResult:
		if claimed {
			t.Fatal("stale asynchronous dispatch claimed the changed task")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher claim did not finish")
	}

	deadline := time.Now().Add(3 * time.Second)
	var operation operationRecord
	for time.Now().Before(deadline) {
		server.operationsMu.Lock()
		for _, candidate := range server.operations {
			if candidate.ID == operationID {
				operation = candidate
			}
		}
		server.operationsMu.Unlock()
		if operation.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if operation.Status != "failed" || !strings.Contains(operation.Error, "conflict") || !strings.Contains(operation.Error, "refresh") {
		t.Fatalf("dispatch operation = %#v, want visible refresh conflict", operation)
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/tasks/"+taskID, nil)
	detail := mapValue(t, value)
	task := mapValue(t, detail["task"])
	if response.StatusCode != http.StatusOK || task["status"] != "ready" ||
		task["title"] != "changed after request validation" || len(arrayValue(t, detail["runs"])) != 0 {
		t.Fatalf("stale async claim changed task: %d %#v", response.StatusCode, value)
	}
}
