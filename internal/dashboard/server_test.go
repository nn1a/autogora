package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "test-dashboard-token-32-characters"

func startTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := Start(context.Background(), Options{DBPath: filepath.Join(t.TempDir(), "kanban.db"), CLIPath: "/tmp/taskcircuit", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server
}

func apiRequest(t *testing.T, server *Server, method, path string, body any) (*http.Response, any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if len(contents) > 0 && json.Unmarshal(contents, &value) != nil {
		t.Fatalf("non-JSON response %s: %s", path, contents)
	}
	return response, value
}

func mapValue(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %#v", value)
	}
	return result
}

func TestAuthenticationAndEmbeddedAssets(t *testing.T) {
	server := startTestServer(t)
	unauthorized, err := http.Get(server.URL + "/api/boards")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	bootstrap, err := client.Get(server.URL + "/?token=" + url.QueryEscape(testToken))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.Body.Close()
	if bootstrap.StatusCode != http.StatusFound || !strings.Contains(bootstrap.Header.Get("Set-Cookie"), "kanban_session=") {
		t.Fatalf("bootstrap response: %d %s", bootstrap.StatusCode, bootstrap.Header.Get("Set-Cookie"))
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	request.Header.Set("Cookie", strings.Split(bootstrap.Header.Get("Set-Cookie"), ";")[0])
	html, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(html.Body)
	html.Body.Close()
	if html.StatusCode != http.StatusOK || !strings.Contains(string(contents), "<title>TaskCircuit</title>") || !strings.Contains(string(contents), `class="dialog-wide"`) {
		t.Fatalf("embedded dashboard mismatch: %d %s", html.StatusCode, contents)
	}
}

func TestBoardTaskHierarchyAndAttachmentAPI(t *testing.T) {
	server := startTestServer(t)
	response, updated := apiRequest(t, server, http.MethodPatch, "/api/boards/default", map[string]any{
		"name": "Default Project", "orchestration": map[string]any{
			"autoDecompose": true, "autoDecomposePerTick": 2, "autoPromoteChildren": false,
			"plannerRuntime": "gemini", "defaultProfile": "worker",
			"profiles": []any{map[string]any{"name": "worker", "runtime": "gemini", "description": "general work"}},
		},
	})
	if response.StatusCode != http.StatusOK || mapValue(t, mapValue(t, updated)["orchestration"])["plannerRuntime"] != "gemini" {
		t.Fatalf("board update failed: %d %#v", response.StatusCode, updated)
	}
	invalid, _ := apiRequest(t, server, http.MethodGet, "/api/tasks?sort=drop-table", nil)
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid sort status = %d", invalid.StatusCode)
	}
	createdResponse, created := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "HTTP task", "body": "exercise API", "status": "triage"})
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %#v", createdResponse.StatusCode, created)
	}
	taskID := mapValue(t, mapValue(t, created)["task"])["id"].(string)
	_, specified := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"title": "Updated HTTP task", "body": "Acceptance: durable", "status": "todo"})
	if mapValue(t, mapValue(t, specified)["task"])["title"] != "Updated HTTP task" {
		t.Fatalf("task update failed: %#v", specified)
	}
	commentResponse, _ := apiRequest(t, server, http.MethodPost, "/api/tasks/"+taskID+"/comments", map[string]any{"author": "test", "body": "durable comment"})
	if commentResponse.StatusCode != http.StatusCreated {
		t.Fatalf("comment status = %d", commentResponse.StatusCode)
	}
	_, child := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "child"})
	childID := mapValue(t, mapValue(t, child)["task"])["id"].(string)
	_, hierarchy := apiRequest(t, server, http.MethodPost, "/api/hierarchy", map[string]any{"parentTaskId": taskID, "subtaskId": childID, "position": 0})
	if mapValue(t, mapValue(t, hierarchy)["graph"])["rootTaskId"] != taskID {
		t.Fatalf("hierarchy failed: %#v", hierarchy)
	}

	uploadRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/tasks/"+taskID+"/attachments?name=brief.txt", strings.NewReader("attachment body"))
	uploadRequest.Header.Set("Authorization", "Bearer "+testToken)
	uploadRequest.Header.Set("Content-Type", "text/plain")
	upload, err := http.DefaultClient.Do(uploadRequest)
	if err != nil {
		t.Fatal(err)
	}
	var attachment map[string]any
	if err := json.NewDecoder(upload.Body).Decode(&attachment); err != nil {
		t.Fatal(err)
	}
	upload.Body.Close()
	if upload.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d: %#v", upload.StatusCode, attachment)
	}
	downloadRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/api/attachments/"+attachment["id"].(string)+"/download?taskId="+taskID, nil)
	downloadRequest.Header.Set("Authorization", "Bearer "+testToken)
	download, err := http.DefaultClient.Do(downloadRequest)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(download.Body)
	download.Body.Close()
	if string(downloaded) != "attachment body" {
		t.Fatalf("download mismatch: %q", downloaded)
	}
	_, snapshot := apiRequest(t, server, http.MethodGet, "/api/board?includeArchived=true", nil)
	tasks := mapValue(t, snapshot)["tasks"].([]any)
	for _, raw := range tasks {
		task := mapValue(t, raw)
		if task["id"] == taskID && (task["commentsCount"] != float64(1) || task["subtasksTotal"] != float64(1)) {
			t.Fatalf("enriched counters mismatch: %#v", task)
		}
	}
}

func TestSSEStreamsTaskEvents(t *testing.T) {
	server := startTestServer(t)
	_, existing := apiRequest(t, server, http.MethodGet, "/api/events?since=0", nil)
	cursor := int64(0)
	events := existing.([]any)
	if len(events) > 0 {
		cursor = int64(mapValue(t, events[len(events)-1])["id"].(float64))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/events/stream?since="+strconv.FormatInt(cursor, 10), nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	message := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				message <- strings.TrimPrefix(scanner.Text(), "data: ")
				return
			}
		}
	}()
	_, created := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "streamed task"})
	taskID := mapValue(t, mapValue(t, created)["task"])["id"].(string)
	select {
	case text := <-message:
		if !strings.Contains(text, taskID) {
			t.Fatalf("SSE did not contain task event: %s", text)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("SSE event timeout")
	}
}
