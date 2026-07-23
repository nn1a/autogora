package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const testToken = "test-dashboard-token-32-characters"

func startTestServer(t *testing.T) *Server {
	return startTestServerWithOptions(t, func(*Options) {})
}

func startTestServerWithOptions(t *testing.T, configure func(*Options)) *Server {
	t.Helper()
	t.Setenv("AUTOGORA_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	options := Options{DBPath: filepath.Join(t.TempDir(), "autogora.db"), CLIPath: "/tmp/autogora", Token: testToken}
	configure(&options)
	server, err := Start(context.Background(), options)
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

func TestAgentDetectionAPIUsesBoundedVersionChecks(t *testing.T) {
	calls := []string{}
	server := startTestServerWithOptions(t, func(options *Options) {
		options.AgentDetection = agentconfig.DetectOptions{
			LookPath: func(name string) (string, error) {
				if name == "codex" || name == "cline" {
					return "/tools/" + name, nil
				}
				return "", errors.New("missing")
			},
			RunVersion: func(_ context.Context, executable string) (string, string, error) {
				calls = append(calls, executable+" --version")
				if strings.HasSuffix(executable, "cline") {
					return "", "cline test version", errors.New("version exit")
				}
				return "codex test version", "", nil
			},
		}
	})

	response, value := apiRequest(t, server, http.MethodPost, "/api/agents/detect", map[string]any{})
	detected := mapValue(t, value)
	if response.StatusCode != http.StatusOK || detected["exists"] != false || len(arrayValue(t, detected["agents"])) != len(model.WorkerRuntimes) {
		t.Fatalf("unexpected detection response: %d %#v", response.StatusCode, detected)
	}
	if len(calls) != 2 || calls[0] != "/tools/codex --version" || calls[1] != "/tools/cline --version" {
		t.Fatalf("detection made unexpected calls: %#v", calls)
	}
	getResponse, _ := apiRequest(t, server, http.MethodGet, "/api/agents/detect", nil)
	if getResponse.StatusCode != http.StatusMethodNotAllowed || getResponse.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("unexpected GET detection response: %d %q", getResponse.StatusCode, getResponse.Header.Get("Allow"))
	}
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

func arrayValue(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("expected array, got %#v", value)
	}
	return result
}

func namedValue(t *testing.T, values []any, name string) map[string]any {
	t.Helper()
	for _, value := range values {
		candidate := mapValue(t, value)
		if candidate["name"] == name {
			return candidate
		}
	}
	t.Fatalf("missing named value %q in %#v", name, values)
	return nil
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
	if bootstrap.StatusCode != http.StatusFound || !strings.Contains(bootstrap.Header.Get("Set-Cookie"), "autogora_session=") {
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
	if html.StatusCode != http.StatusOK || !strings.Contains(string(contents), "<title>Autogora</title>") || !strings.Contains(string(contents), `class="dialog-wide"`) {
		t.Fatalf("embedded dashboard mismatch: %d %s", html.StatusCode, contents)
	}
}

func TestGlobalAgentConfigAPIStartsEmptyAndPersistsValidatedConfig(t *testing.T) {
	server := startTestServer(t)
	response, initialValue := apiRequest(t, server, http.MethodGet, "/api/config", nil)
	initial := mapValue(t, initialValue)
	if response.StatusCode != http.StatusOK || initial["exists"] != false || initial["path"] == "" {
		t.Fatalf("unexpected initial config response: %d %#v", response.StatusCode, initial)
	}
	if mapValue(t, initial["config"])["schemaVersion"] != float64(1) {
		t.Fatalf("default config missing schema version: %#v", initial)
	}

	config := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"autoStart": true, "maxWorkers": 3, "allowWrites": false},
		"defaults": map[string]any{
			"workerAgents": []string{"primary"}, "plannerAgents": []string{"backup"}, "judgeAgents": []string{},
		},
		"agents": []any{
			map[string]any{"id": "primary", "runtime": "codex", "command": "/opt/codex", "model": "codex-model", "enabled": true,
				"maxConcurrent": 2, "roles": []string{"worker"}, "fallbacks": []string{"backup"}},
			map[string]any{"id": "backup", "runtime": "claude", "command": "/opt/claude", "model": "claude-model", "enabled": true,
				"maxConcurrent": 1, "roles": []string{"worker", "planner"}},
		},
	}
	response, savedValue := apiRequest(t, server, http.MethodPut, "/api/config", config)
	saved := mapValue(t, savedValue)
	if response.StatusCode != http.StatusOK || saved["exists"] != true || saved["path"] != initial["path"] {
		t.Fatalf("config save failed: %d %#v", response.StatusCode, saved)
	}
	savedConfig := mapValue(t, saved["config"])
	if mapValue(t, savedConfig["supervisor"])["maxWorkers"] != float64(3) || len(arrayValue(t, savedConfig["agents"])) != 2 {
		t.Fatalf("saved config was not returned: %#v", savedConfig)
	}
	statusResponse, statusValue := apiRequest(t, server, http.MethodGet, "/api/supervisor", nil)
	status := mapValue(t, statusValue)
	if statusResponse.StatusCode != http.StatusOK || status["running"] != true || status["desired"] != true || status["maxWorkers"] != float64(3) {
		t.Fatalf("saved auto-start config did not start supervisor: %d %#v", statusResponse.StatusCode, status)
	}
	stoppedResponse, stoppedValue := apiRequest(t, server, http.MethodPost, "/api/supervisor/stop", map[string]any{})
	if stoppedResponse.StatusCode != http.StatusOK || mapValue(t, stoppedValue)["running"] != false {
		t.Fatalf("supervisor did not stop: %d %#v", stoppedResponse.StatusCode, stoppedValue)
	}

	invalid := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 1},
		"defaults":      map[string]any{"workerAgents": []string{}, "plannerAgents": []string{}, "judgeAgents": []string{}},
		"agents": []any{map[string]any{
			"id": "leaky", "runtime": "codex", "command": "codex", "enabled": true, "maxConcurrent": 1,
			"roles": []string{"worker"}, "apiKey": "must-not-be-accepted",
		}},
	}
	rejected, rejectedValue := apiRequest(t, server, http.MethodPut, "/api/config", invalid)
	if rejected.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, rejectedValue)["error"].(string), "unknown field") {
		t.Fatalf("unknown config field was accepted: %d %#v", rejected.StatusCode, rejectedValue)
	}
	invalidPolicy := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 1},
		"defaults":      map[string]any{"workerAgents": []string{"unknown"}, "plannerAgents": []string{}, "judgeAgents": []string{}},
		"agents":        []any{},
	}
	rejected, rejectedValue = apiRequest(t, server, http.MethodPut, "/api/config", invalidPolicy)
	if rejected.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, rejectedValue)["error"].(string), "unknown agent") {
		t.Fatalf("invalid config references were accepted: %d %#v", rejected.StatusCode, rejectedValue)
	}

	response, loadedValue := apiRequest(t, server, http.MethodGet, "/api/config", nil)
	loaded := mapValue(t, loadedValue)
	if response.StatusCode != http.StatusOK || loaded["exists"] != true || len(arrayValue(t, mapValue(t, loaded["config"])["agents"])) != 2 {
		t.Fatalf("invalid update changed persisted config: %d %#v", response.StatusCode, loaded)
	}
}

func TestEffectiveAgentsIncludeBoardProfilesHealthAndActiveRuns(t *testing.T) {
	server := startTestServer(t)
	config := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 2},
		"defaults":      map[string]any{"workerAgents": []string{"global-worker"}, "plannerAgents": []string{"planner-only"}, "judgeAgents": []string{}},
		"agents": []any{
			map[string]any{"id": "global-worker", "runtime": "codex", "command": "codex", "model": "global-model", "enabled": true,
				"maxConcurrent": 2, "roles": []string{"worker"}},
			map[string]any{"id": "planner-only", "runtime": "gemini", "command": "gemini", "enabled": true,
				"maxConcurrent": 1, "roles": []string{"planner"}},
		},
	}
	if response, value := apiRequest(t, server, http.MethodPut, "/api/config", config); response.StatusCode != http.StatusOK {
		t.Fatalf("config save failed: %d %#v", response.StatusCode, value)
	}
	if response, value := apiRequest(t, server, http.MethodPatch, "/api/boards/default", map[string]any{
		"orchestration": map[string]any{"profiles": []any{
			map[string]any{"name": "global-worker", "runtime": "claude", "model": "board-model", "maxConcurrent": 9},
			map[string]any{"name": "board-worker", "runtime": "cline", "model": "board-only-model"},
		}},
	}); response.StatusCode != http.StatusOK {
		t.Fatalf("board update failed: %d %#v", response.StatusCode, value)
	}

	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "global-worker"
	activeTask, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "active global worker", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: activeTask.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim active task: claim=%#v err=%v", claim, err)
	}
	if _, err := opened.RecordRunAgentConfig(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.RecordRunAgentConfigInput{
		Profile: "global-worker", Runtime: model.RuntimeCodex, Model: "board-model", Source: "board_profile",
	}); err != nil {
		t.Fatal(err)
	}
	lastRunID := claim.Run.ID
	if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{AgentID: "global-worker", Status: model.AgentHealthReady, LastRunID: &lastRunID}); err != nil {
		t.Fatal(err)
	}
	taskOnly := "task-worker"
	if _, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "task route", Assignee: &taskOnly, Runtime: model.RuntimeGemini}); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/agents/effective?board=default", nil)
	effective := mapValue(t, value)
	if response.StatusCode != http.StatusOK || mapValue(t, effective["metadata"])["slug"] != "default" {
		t.Fatalf("effective agent response failed: %d %#v", response.StatusCode, effective)
	}
	if len(arrayValue(t, mapValue(t, effective["config"])["agents"])) != 2 {
		t.Fatalf("global config missing from effective response: %#v", effective)
	}
	profiles := arrayValue(t, effective["profiles"])
	global := namedValue(t, profiles, "global-worker")
	if global["runtime"] != "codex" || global["model"] != "board-model" || global["maxConcurrent"] != float64(2) || global["activeRuns"] != float64(1) {
		t.Fatalf("global/board policy or active count mismatch: %#v", global)
	}
	health := mapValue(t, global["health"])
	if health["status"] != "ready" || health["lastRunId"] != claim.Run.ID {
		t.Fatalf("agent health missing from effective profile: %#v", global)
	}
	boardWorker := namedValue(t, profiles, "board-worker")
	if mapValue(t, boardWorker["health"])["status"] != "unknown" || boardWorker["activeRuns"] != float64(0) {
		t.Fatalf("unknown board agent state mismatch: %#v", boardWorker)
	}
	_ = namedValue(t, profiles, "task-worker")
	for _, raw := range profiles {
		if mapValue(t, raw)["name"] == "planner-only" {
			t.Fatalf("planner-only agent leaked into worker profiles: %#v", profiles)
		}
	}
}

func TestBoardSnapshotKeepsMetadataProfilesSeparateFromEffectiveProfiles(t *testing.T) {
	server := startTestServer(t)
	config := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 1},
		"defaults":      map[string]any{"workerAgents": []string{"global-worker"}, "plannerAgents": []string{}, "judgeAgents": []string{}},
		"agents": []any{map[string]any{
			"id": "global-worker", "runtime": "claude", "command": "claude", "model": "global-model", "enabled": true,
			"maxConcurrent": 1, "roles": []string{"worker"},
		}},
	}
	if response, value := apiRequest(t, server, http.MethodPut, "/api/config", config); response.StatusCode != http.StatusOK {
		t.Fatalf("config save failed: %d %#v", response.StatusCode, value)
	}
	if response, value := apiRequest(t, server, http.MethodPatch, "/api/boards/default", map[string]any{
		"orchestration": map[string]any{"profiles": []any{map[string]any{"name": "board-worker", "runtime": "cline"}}},
	}); response.StatusCode != http.StatusOK {
		t.Fatalf("board update failed: %d %#v", response.StatusCode, value)
	}
	if response, value := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{
		"title": "task-derived route", "assignee": "task-worker", "runtime": "gemini",
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("task create failed: %d %#v", response.StatusCode, value)
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/board", nil)
	snapshot := mapValue(t, value)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("board snapshot failed: %d %#v", response.StatusCode, snapshot)
	}
	storedProfiles := arrayValue(t, mapValue(t, mapValue(t, snapshot["board"])["orchestration"])["profiles"])
	if len(storedProfiles) != 1 || mapValue(t, storedProfiles[0])["name"] != "board-worker" {
		t.Fatalf("global profiles leaked into board metadata: %#v", snapshot["board"])
	}
	effectiveProfiles := arrayValue(t, snapshot["profiles"])
	_ = namedValue(t, effectiveProfiles, "global-worker")
	_ = namedValue(t, effectiveProfiles, "board-worker")
	_ = namedValue(t, effectiveProfiles, "task-worker")
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

func TestTargetedDispatchRejectsStrandedRoutesAndAcceptsRunnableTask(t *testing.T) {
	server := startTestServer(t)
	_, stranded := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{
		"title": "No worker route", "status": "ready", "runtime": "manual",
	})
	strandedID := mapValue(t, mapValue(t, stranded)["task"])["id"].(string)
	rejected, _ := apiRequest(t, server, http.MethodPost, "/api/dispatch", map[string]any{"taskId": strandedID})
	if rejected.StatusCode != http.StatusConflict {
		t.Fatalf("stranded targeted dispatch status = %d", rejected.StatusCode)
	}

	_, runnable := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{
		"title": "Runnable", "status": "ready", "assignee": "worker", "runtime": "cline",
	})
	runnableID := mapValue(t, mapValue(t, runnable)["task"])["id"].(string)
	accepted, value := apiRequest(t, server, http.MethodPost, "/api/dispatch", map[string]any{"taskId": runnableID})
	if accepted.StatusCode != http.StatusAccepted || mapValue(t, value)["taskId"] != runnableID {
		t.Fatalf("targeted dispatch response = %d %#v", accepted.StatusCode, value)
	}
}
