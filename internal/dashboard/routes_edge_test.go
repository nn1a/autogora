package dashboard

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func createStoredScheduledTask(t *testing.T, server *Server, title, scheduledAt string) string {
	t.Helper()
	opened, err := server.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Board:       "default",
		Title:       title,
		Status:      model.TaskStatusScheduled,
		ScheduledAt: &scheduledAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return detail.Task.ID
}

func assertTaskStatusAndSchedule(t *testing.T, server *Server, taskID string, status model.TaskStatus) {
	t.Helper()
	response, value := apiRequest(t, server, http.MethodGet, "/api/tasks/"+taskID, nil)
	task := mapValue(t, mapValue(t, value)["task"])
	if response.StatusCode != http.StatusOK || task["status"] != string(status) || task["scheduledAt"] != nil {
		t.Fatalf("task %s = %d %#v, want status %s without scheduledAt", taskID, response.StatusCode, task, status)
	}
}

func TestWebStatusMovesClearPastAndFutureSchedules(t *testing.T) {
	server := startTestServer(t)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)

	t.Run("single patch", func(t *testing.T) {
		for _, scheduledAt := range []string{past, future} {
			taskID := createStoredScheduledTask(t, server, "single "+scheduledAt, scheduledAt)
			response, value := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"status": "todo"})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status move failed: %d %#v", response.StatusCode, value)
			}
			assertTaskStatusAndSchedule(t, server, taskID, model.TaskStatusTodo)
		}
	})

	t.Run("bulk", func(t *testing.T) {
		taskIDs := []string{
			createStoredScheduledTask(t, server, "bulk past", past),
			createStoredScheduledTask(t, server, "bulk future", future),
		}
		response, value := apiRequest(t, server, http.MethodPost, "/api/tasks/bulk", map[string]any{
			"ids": taskIDs, "mutation": map[string]any{"status": "todo"},
		})
		result := mapValue(t, value)
		if response.StatusCode != http.StatusOK || len(arrayValue(t, result["ok"])) != len(taskIDs) || len(arrayValue(t, result["errors"])) != 0 {
			t.Fatalf("bulk status move failed: %d %#v", response.StatusCode, value)
		}
		for _, taskID := range taskIDs {
			assertTaskStatusAndSchedule(t, server, taskID, model.TaskStatusTodo)
		}
	})
}

func TestOperationHistoryRetainsConcurrentRunningOperations(t *testing.T) {
	server := &Server{}
	const operationCount = 125
	operations := make([]operationRecord, operationCount)
	var wait sync.WaitGroup

	for index := range operations {
		wait.Add(1)
		go func() {
			defer wait.Done()
			operations[index] = server.beginOperation("dispatch", "default", "", "one_shot", false)
		}()
	}
	wait.Wait()

	server.operationsMu.Lock()
	if len(server.operations) != operationCount {
		server.operationsMu.Unlock()
		t.Fatalf("running operations retained = %d, want %d", len(server.operations), operationCount)
	}
	for _, operation := range server.operations {
		if operation.Status != "running" {
			server.operationsMu.Unlock()
			t.Fatalf("unexpected in-flight operation: %#v", operation)
		}
	}
	server.operationsMu.Unlock()

	for _, operation := range operations {
		wait.Add(1)
		go func() {
			defer wait.Done()
			server.finishOperation(operation.ID, nil)
		}()
	}
	wait.Wait()

	server.operationsMu.Lock()
	defer server.operationsMu.Unlock()
	if len(server.operations) != 100 {
		t.Fatalf("terminal operation history = %d, want 100", len(server.operations))
	}
	for _, operation := range server.operations {
		if operation.Status != "completed" {
			t.Fatalf("unexpected terminal operation: %#v", operation)
		}
	}
}

func TestGitHubImportRejectsInvalidJSONIssueNumbers(t *testing.T) {
	server := startTestServer(t)
	for name, issues := range map[string]any{
		"fraction":         []any{3.9},
		"zero":             []any{0},
		"negative":         []any{-4},
		"too large":        []any{1e100},
		"too large string": []any{"999999999999999999999999999999999999"},
	} {
		t.Run(name, func(t *testing.T) {
			response, value := apiRequest(t, server, http.MethodPost, "/api/github/import", map[string]any{
				"repository": "team/service",
				"issues":     issues,
			})
			if response.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, value)["error"].(string), "positive integers") {
				t.Fatalf("invalid issues response = %d %#v", response.StatusCode, value)
			}
		})
	}

	for name, number := range map[string]float64{
		"NaN":               math.NaN(),
		"positive infinity": math.Inf(1),
		"negative infinity": math.Inf(-1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := importIssueNumbers([]any{number}); err == nil || !strings.Contains(err.Error(), "positive integers") {
				t.Fatalf("non-finite issue number error = %v", err)
			}
		})
	}
}
