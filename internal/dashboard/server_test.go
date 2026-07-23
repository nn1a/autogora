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
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/model"
	setupcfg "github.com/nn1a/autogora/internal/setup"
	"github.com/nn1a/autogora/internal/store"
)

func TestErrorStatusTreatsConcurrentBoardMutationAsConflict(t *testing.T) {
	for _, message := range []string{
		"board metadata mutation is in progress: product-web",
		"workspace resource is busy",
	} {
		if status := errorStatus(errors.New(message)); status != http.StatusConflict {
			t.Fatalf("errorStatus(%q) = %d, want %d", message, status, http.StatusConflict)
		}
	}
}

const testToken = "test-dashboard-token-32-characters"

type dashboardGitHubRunner struct {
	outputs []setupcfg.CommandOutput
	calls   [][]string
}

func (runner *dashboardGitHubRunner) LookPath(string) (string, error) { return "/usr/bin/gh", nil }
func (runner *dashboardGitHubRunner) Run(_ context.Context, _ string, _ string, args ...string) (setupcfg.CommandOutput, error) {
	runner.calls = append(runner.calls, append([]string{}, args...))
	if len(runner.outputs) == 0 {
		return setupcfg.CommandOutput{}, errors.New("unexpected gh invocation")
	}
	output := runner.outputs[0]
	runner.outputs = runner.outputs[1:]
	return output, nil
}

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

func TestAgentPresetAPIPreviewsDetectedConfigurationWithoutSaving(t *testing.T) {
	calls := []string{}
	server := startTestServerWithOptions(t, func(options *Options) {
		options.AgentDetection = agentconfig.DetectOptions{
			LookPath: func(name string) (string, error) {
				if name == "codex" || name == "claude" {
					return "/tools/" + name, nil
				}
				return "", errors.New("missing")
			},
			RunVersion: func(_ context.Context, executable string) (string, string, error) {
				calls = append(calls, executable+" --version")
				return filepath.Base(executable) + " test version", "", nil
			},
		}
	})

	response, value := apiRequest(t, server, http.MethodGet, "/api/agents/presets", nil)
	if response.StatusCode != http.StatusOK || len(arrayValue(t, mapValue(t, value)["presets"])) != 4 {
		t.Fatalf("unexpected preset catalog: %d %#v", response.StatusCode, value)
	}

	response, value = apiRequest(t, server, http.MethodPost, "/api/agents/presets", map[string]any{
		"id": "codex-claude",
		"config": map[string]any{
			"schemaVersion": 1,
			"supervisor":    map[string]any{"maxWorkers": 1},
			"defaults": map[string]any{
				"workerAgents": []string{"custom"}, "plannerAgents": []string{},
				"coordinatorAgents": []string{}, "judgeAgents": []string{},
			},
			"agents": []any{map[string]any{
				"id": "custom", "runtime": "codex", "command": "/custom/codex",
				"enabled": true, "maxConcurrent": 1, "roles": []string{"worker"},
			}},
		},
	})
	preview := mapValue(t, value)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preset preview failed: %d %#v", response.StatusCode, preview)
	}
	config := mapValue(t, preview["config"])
	defaults := mapValue(t, config["defaults"])
	if len(arrayValue(t, config["agents"])) != 3 ||
		len(arrayValue(t, defaults["workerAgents"])) != 1 ||
		len(arrayValue(t, defaults["coordinatorAgents"])) != 2 ||
		len(arrayValue(t, preview["detections"])) != len(model.WorkerRuntimes) {
		t.Fatalf("unexpected preset preview: %#v", preview)
	}
	joinedCalls := strings.Join(calls, "\n")
	if len(calls) != 2 ||
		!strings.Contains(joinedCalls, "/tools/claude --version") ||
		!strings.Contains(joinedCalls, "/tools/codex --version") {
		t.Fatalf("preset preview made unexpected calls: %#v", calls)
	}
	configResponse, configValue := apiRequest(t, server, http.MethodGet, "/api/config", nil)
	if configResponse.StatusCode != http.StatusOK || mapValue(t, configValue)["exists"] != false {
		t.Fatalf("preset preview persisted configuration: %d %#v", configResponse.StatusCode, configValue)
	}
}

func TestAgentConfigAPIUsesRevisionCASAndPreservesSupervisorIntent(t *testing.T) {
	server := startTestServer(t)
	response, value := apiRequest(t, server, http.MethodGet, "/api/config", nil)
	initial := mapValue(t, value)
	revision, _ := initial["revision"].(string)
	if response.StatusCode != http.StatusOK || revision != string(agentconfig.MissingRevision) {
		t.Fatalf("unexpected initial config snapshot: %d %#v", response.StatusCode, initial)
	}
	config := mapValue(t, initial["config"])
	supervisorConfig := mapValue(t, config["supervisor"])
	supervisorConfig["autoStart"] = true
	supervisorConfig["maxWorkers"] = float64(2)
	supervisorConfig["allowWrites"] = false

	missing, missingValue := apiRequest(t, server, http.MethodPut, "/api/config", config)
	if missing.StatusCode != http.StatusBadRequest ||
		!strings.Contains(mapValue(t, missingValue)["error"].(string), "If-Match") {
		t.Fatalf("missing revision was accepted: %d %#v", missing.StatusCode, missingValue)
	}

	savedResponse, savedValue := apiRequestWithHeaders(
		t, server, http.MethodPut, "/api/config", config,
		map[string]string{"If-Match": revision},
	)
	saved := mapValue(t, savedValue)
	savedRevision, _ := saved["revision"].(string)
	if savedResponse.StatusCode != http.StatusOK || savedRevision == "" || savedRevision == revision {
		t.Fatalf("config CAS did not return a new revision: %d %#v", savedResponse.StatusCode, saved)
	}
	if status := server.supervisor.Status(); status.Desired {
		t.Fatalf("ordinary Web save changed stopped Supervisor intent: %#v", status)
	}

	staleConfig := mapValue(t, saved["config"])
	mapValue(t, staleConfig["supervisor"])["maxWorkers"] = float64(9)
	staleResponse, staleValue := apiRequestWithHeaders(
		t, server, http.MethodPut, "/api/config", staleConfig,
		map[string]string{"If-Match": revision},
	)
	if staleResponse.StatusCode != http.StatusConflict ||
		!strings.Contains(strings.ToLower(mapValue(t, staleValue)["error"].(string)), "revision conflict") {
		t.Fatalf("stale Web config overwrite was not rejected: %d %#v", staleResponse.StatusCode, staleValue)
	}
	loaded, err := agentconfig.Load(agentconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.MaxWorkers != 2 {
		t.Fatalf("stale Web save overwrote current config: %#v", loaded.Supervisor)
	}
}

func TestDashboardSupervisorPassesLiveGlobalAgentConfigLoader(t *testing.T) {
	started := make(chan dispatcher.Options, 1)
	server := startTestServerWithOptions(t, func(options *Options) {
		options.supervisorRun = func(
			ctx context.Context,
			dispatchOptions dispatcher.Options,
		) error {
			started <- dispatchOptions
			<-ctx.Done()
			return ctx.Err()
		}
	})
	config := agentconfig.Default()
	config.Supervisor.MaxWorkers = 1
	config.Agents = []agentconfig.Agent{{
		ID: "coordinator", Runtime: model.RuntimeCodex, Command: "codex",
		Model: "first-model", Enabled: true, MaxConcurrent: 1,
		Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
	}}
	config.Defaults.CoordinatorAgents = []string{"coordinator"}
	response, value := putAgentConfig(t, server, config, true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("start Dashboard Supervisor: %d %#v", response.StatusCode, value)
	}
	var dispatchOptions dispatcher.Options
	select {
	case dispatchOptions = <-started:
	case <-time.After(time.Second):
		t.Fatal("Dashboard Supervisor did not start dispatcher")
	}
	if dispatchOptions.AgentConfig == nil || dispatchOptions.AgentConfigLoader == nil ||
		dispatchOptions.AgentConfig.Agents[0].Model != "first-model" {
		t.Fatalf("Dashboard Supervisor dispatcher options = %#v", dispatchOptions)
	}
	config.Agents[0].Model = "second-model"
	if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := dispatchOptions.AgentConfigLoader()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Agents[0].Model != "second-model" ||
		dispatchOptions.AgentConfig.Agents[0].Model != "first-model" {
		t.Fatalf(
			"Dashboard Supervisor loader = snapshot:%#v live:%#v",
			dispatchOptions.AgentConfig,
			reloaded,
		)
	}
}

func putAgentConfig(
	t *testing.T,
	server *Server,
	config any,
	startSupervisor bool,
) (*http.Response, any) {
	t.Helper()
	response, value := apiRequest(t, server, http.MethodGet, "/api/config", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("load config revision: %d %#v", response.StatusCode, value)
	}
	revision, _ := mapValue(t, value)["revision"].(string)
	headers := map[string]string{"If-Match": revision}
	if startSupervisor {
		headers["X-Autogora-Supervisor-Desired"] = "start"
	}
	return apiRequestWithHeaders(
		t, server, http.MethodPut, "/api/config", config, headers,
	)
}

func apiRequest(t *testing.T, server *Server, method, path string, body any) (*http.Response, any) {
	t.Helper()
	return apiRequestWithHeaders(t, server, method, path, body, nil)
}

func apiRequestWithHeaders(
	t *testing.T,
	server *Server,
	method, path string,
	body any,
	headers map[string]string,
) (*http.Response, any) {
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
	for name, value := range headers {
		request.Header.Set(name, value)
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

func TestCloseCancelsEventStreamWithoutExhaustingShutdownDeadline(t *testing.T) {
	server := startTestServer(t)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/events/stream?token="+url.QueryEscape(testToken), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected stream response: %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatalf("close with active event stream: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("event stream delayed shutdown for %s", elapsed)
	}
}

func TestCloseBoundsWaitForDashboardOperations(t *testing.T) {
	server := startTestServer(t)
	release := make(chan struct{})
	server.workers.Add(1)
	go func() {
		defer server.workers.Done()
		<-release
	}()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := server.Close(ctx)
	cancel()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("operation wait ignored shutdown deadline for %s", elapsed)
	}
}

func TestDoneReportsUnexpectedListenerFailure(t *testing.T) {
	server := startTestServer(t)
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	case <-time.After(time.Second):
		t.Fatal("server did not report listener termination")
	}
	if err := server.Err(); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("unexpected listener failure was hidden: %v", err)
	}
}

func TestGitHubImportAPIProvidesPreviewAndEnterpriseImport(t *testing.T) {
	issue := `[{"id":"I_dashboard","number":42,"title":"Fix session retry","body":"Retry once.","url":"https://ghe.example.com/team/service/issues/42","state":"OPEN","labels":[{"name":"bug"}],"assignees":[],"author":{"login":"reporter"},"createdAt":"2026-07-01T00:00:00Z","updatedAt":"2026-07-02T00:00:00Z"}]`
	runner := &dashboardGitHubRunner{outputs: []setupcfg.CommandOutput{{Stdout: issue}, {Stdout: issue}}}
	server := startTestServerWithOptions(t, func(options *Options) { options.GitHubRunner = runner })
	body := map[string]any{
		"host": "ghe.example.com", "repository": "team/service", "labels": "bug",
		"limit": 20, "tenant": "platform", "priority": 5,
	}
	previewBody := map[string]any{}
	for key, value := range body {
		previewBody[key] = value
	}
	previewBody["dryRun"] = true
	response, previewValue := apiRequest(t, server, http.MethodPost, "/api/github/import?board=default", previewBody)
	preview := mapValue(t, previewValue)
	if response.StatusCode != http.StatusOK || preview["planned"] != float64(1) || preview["created"] != float64(0) || preview["status"] != "success" {
		t.Fatalf("unexpected import preview: %d %#v", response.StatusCode, preview)
	}
	response, importedValue := apiRequest(t, server, http.MethodPost, "/api/github/import?board=default", body)
	imported := mapValue(t, importedValue)
	if response.StatusCode != http.StatusOK || imported["created"] != float64(1) || imported["repository"] != "ghe.example.com/team/service" {
		t.Fatalf("unexpected import result: %d %#v", response.StatusCode, imported)
	}
	if len(runner.calls) != 2 || !strings.Contains(strings.Join(runner.calls[0], " "), "ghe.example.com/team/service") {
		t.Fatalf("enterprise repository not passed to gh: %#v", runner.calls)
	}
	boardResponse, boardValue := apiRequest(t, server, http.MethodGet, "/api/board?board=default", nil)
	tasks := arrayValue(t, mapValue(t, boardValue)["tasks"])
	if boardResponse.StatusCode != http.StatusOK || len(tasks) != 1 || mapValue(t, tasks[0])["status"] != "triage" {
		t.Fatalf("imported triage task missing: %d %#v", boardResponse.StatusCode, tasks)
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
	response, savedValue := putAgentConfig(t, server, config, true)
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
	rejected, rejectedValue := putAgentConfig(t, server, invalid, false)
	if rejected.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, rejectedValue)["error"].(string), "unknown field") {
		t.Fatalf("unknown config field was accepted: %d %#v", rejected.StatusCode, rejectedValue)
	}
	invalidPolicy := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 1},
		"defaults":      map[string]any{"workerAgents": []string{"unknown"}, "plannerAgents": []string{}, "judgeAgents": []string{}},
		"agents":        []any{},
	}
	rejected, rejectedValue = putAgentConfig(t, server, invalidPolicy, false)
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
	if response, value := putAgentConfig(t, server, config, false); response.StatusCode != http.StatusOK {
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

func TestEffectiveAgentsUseSharedHealthForGlobalProfiles(t *testing.T) {
	server := startTestServer(t)
	config := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"maxWorkers": 1},
		"defaults": map[string]any{
			"workerAgents": []string{"global-worker"}, "plannerAgents": []string{},
			"coordinatorAgents": []string{}, "judgeAgents": []string{},
		},
		"agents": []any{map[string]any{
			"id": "global-worker", "runtime": "codex", "command": "codex",
			"enabled": true, "maxConcurrent": 1, "roles": []string{"worker"},
		}},
	}
	if response, value := putAgentConfig(t, server, config, false); response.StatusCode != http.StatusOK {
		t.Fatalf("config save failed: %d %#v", response.StatusCode, value)
	}
	ctx := context.Background()
	if _, err := server.manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	alpha, err := server.manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}
	coordination, err := server.manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.SetAgentHealth(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthAuthRequired,
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/agents/effective?board=alpha", nil)
	effective := mapValue(t, value)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("effective agents failed: %d %#v", response.StatusCode, effective)
	}
	global := namedValue(t, arrayValue(t, effective["profiles"]), "global-worker")
	health := mapValue(t, global["health"])
	if health["status"] != string(model.AgentHealthAuthRequired) {
		t.Fatalf("effective agents used stale board-local health: %#v", global)
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
	if response, value := putAgentConfig(t, server, config, false); response.StatusCode != http.StatusOK {
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
			"autopilot": map[string]any{
				"enabled": true, "autoPlan": true, "autoExecute": true, "workspaceWrites": true,
				"coordination": map[string]any{"mode": "assist", "profile": "worker", "idleSeconds": 60},
				"publication":  map[string]any{"mode": "pull_request", "targetBranch": "develop", "requireApproval": true},
			},
			"profiles": []any{map[string]any{"name": "worker", "runtime": "gemini", "description": "general work"}},
		},
	})
	orchestration := mapValue(t, mapValue(t, updated)["orchestration"])
	autopilot := mapValue(t, orchestration["autopilot"])
	if response.StatusCode != http.StatusOK || orchestration["plannerRuntime"] != "gemini" ||
		autopilot["workspaceWrites"] != true ||
		mapValue(t, autopilot["coordination"])["mode"] != "assist" ||
		mapValue(t, autopilot["publication"])["targetBranch"] != "develop" {
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

func TestDashboardRequiresFutureScheduleAndRejectsStaleEdits(t *testing.T) {
	server := startTestServer(t)
	missing, value := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "Missing time", "status": "scheduled"})
	if missing.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, value)["error"].(string), "scheduledAt") {
		t.Fatalf("missing schedule response = %d %#v", missing.StatusCode, value)
	}
	past, _ := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "Past", "status": "scheduled", "scheduledAt": "2020-01-01T00:00:00Z"})
	if past.StatusCode != http.StatusBadRequest {
		t.Fatalf("past schedule status = %d", past.StatusCode)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	createdResponse, created := apiRequest(t, server, http.MethodPost, "/api/tasks", map[string]any{"title": "Scheduled", "status": "scheduled", "scheduledAt": future})
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf("future schedule status = %d: %#v", createdResponse.StatusCode, created)
	}
	task := mapValue(t, mapValue(t, created)["task"])
	taskID, stale := task["id"].(string), task["updatedAt"].(string)
	cleared, clearedValue := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"scheduledAt": nil})
	if cleared.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, clearedValue)["error"].(string), "scheduledAt") {
		t.Fatalf("cleared schedule response = %d %#v", cleared.StatusCode, clearedValue)
	}
	updated, latest := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"expectedUpdatedAt": stale, "title": "Latest title"})
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("fresh edit failed: %d %#v", updated.StatusCode, latest)
	}
	conflict, conflictValue := apiRequest(t, server, http.MethodPatch, "/api/tasks/"+taskID, map[string]any{"expectedUpdatedAt": stale, "title": "Stale title"})
	if conflict.StatusCode != http.StatusConflict || !strings.Contains(mapValue(t, conflictValue)["error"].(string), "refresh") {
		t.Fatalf("stale edit response = %d %#v", conflict.StatusCode, conflictValue)
	}
	_, loaded := apiRequest(t, server, http.MethodGet, "/api/tasks/"+taskID, nil)
	if mapValue(t, mapValue(t, loaded)["task"])["title"] != "Latest title" {
		t.Fatalf("stale edit overwrote task: %#v", loaded)
	}

	bulkMissing, bulkMissingValue := apiRequest(t, server, http.MethodPost, "/api/tasks/bulk", map[string]any{
		"ids": []any{taskID}, "mutation": map[string]any{"status": "scheduled"},
	})
	if bulkMissing.StatusCode != http.StatusBadRequest || !strings.Contains(mapValue(t, bulkMissingValue)["error"].(string), "scheduledAt") {
		t.Fatalf("bulk missing schedule response = %d %#v", bulkMissing.StatusCode, bulkMissingValue)
	}
	bulkFuture := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	bulkResponse, bulkValue := apiRequest(t, server, http.MethodPost, "/api/tasks/bulk", map[string]any{
		"ids": []any{taskID}, "mutation": map[string]any{"status": "scheduled", "scheduledAt": bulkFuture},
	})
	bulkResult := mapValue(t, bulkValue)
	if bulkResponse.StatusCode != http.StatusOK || len(arrayValue(t, bulkResult["ok"])) != 1 || len(arrayValue(t, bulkResult["errors"])) != 0 {
		t.Fatalf("bulk future schedule response = %d %#v", bulkResponse.StatusCode, bulkValue)
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
	config := map[string]any{
		"schemaVersion": 1,
		"supervisor":    map[string]any{"autoStart": false, "maxWorkers": 1, "allowWrites": true},
		"defaults":      map[string]any{"workerAgents": []string{"worker"}, "plannerAgents": []string{}, "judgeAgents": []string{}},
		"agents": []any{map[string]any{"id": "worker", "runtime": "cline", "command": "/missing/cline", "enabled": true,
			"maxConcurrent": 1, "roles": []string{"worker"}}},
	}
	if response, value := putAgentConfig(t, server, config, false); response.StatusCode != http.StatusOK {
		t.Fatalf("config save failed: %d %#v", response.StatusCode, value)
	}
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
	acceptedValue := mapValue(t, value)
	if accepted.StatusCode != http.StatusAccepted || acceptedValue["taskId"] != runnableID || acceptedValue["allowWrites"] != true || acceptedValue["mode"] != "one_shot" {
		t.Fatalf("targeted dispatch response = %d %#v", accepted.StatusCode, value)
	}
	operationsResponse, operationsValue := apiRequest(t, server, http.MethodGet, "/api/operations?board=default", nil)
	operations := arrayValue(t, operationsValue)
	if operationsResponse.StatusCode != http.StatusOK || len(operations) != 1 || mapValue(t, operations[0])["taskId"] != runnableID {
		t.Fatalf("dispatch operation not observable: %d %#v", operationsResponse.StatusCode, operations)
	}
}
