package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const dashboardWorkerHelperEnvironment = "AUTOGORA_DASHBOARD_WORKER_HELPER"

// TestDashboardSupervisorWorkerHelperProcess is launched as the configured
// Cline executable by TestDashboardSupervisorRunsProductionDispatcherFromAPI.
// The subprocess only replaces the external coding agent: its completion
// request still crosses the managed-run boundary that dispatcher.Run owns.
func TestDashboardSupervisorWorkerHelperProcess(t *testing.T) {
	if os.Getenv(dashboardWorkerHelperEnvironment) != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened, err := store.Open(
		os.Getenv("AUTOGORA_DB"),
		os.Getenv("AUTOGORA_BOARD"),
		os.Getenv("AUTOGORA_ATTACHMENTS_ROOT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := opened.CompleteRun(ctx, store.RunScope{
		RunID: os.Getenv("AUTOGORA_RUN_ID"), ClaimToken: os.Getenv("AUTOGORA_CLAIM_TOKEN"),
	}, store.CompletionInput{Summary: "production Dashboard Supervisor smoke completed"}); err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("AUTOGORA_DASHBOARD_WORKER_MARKER")
	if marker == "" {
		t.Fatal("worker marker path is missing")
	}
	value := strings.Join([]string{
		os.Getenv("AUTOGORA_TASK_ID"),
		os.Getenv("AUTOGORA_AGENT_PROFILE"),
		os.Getenv("AUTOGORA_MODEL"),
		os.Getenv("AUTOGORA_WORKSPACE"),
	}, "\n")
	if err := os.WriteFile(marker, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dashboardSmokeExecutable(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+contents+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func dashboardSmokeTask(t *testing.T, server *Server, taskID string) map[string]any {
	t.Helper()
	response, value := apiRequest(t, server, http.MethodGet, "/api/tasks/"+taskID+"?board=default", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("load smoke task: %d %#v", response.StatusCode, value)
	}
	return mapValue(t, value)
}

func waitForDashboardSmokeTask(
	t *testing.T,
	server *Server,
	taskID string,
	status model.TaskStatus,
	timeout time.Duration,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var detail map[string]any
	for time.Now().Before(deadline) {
		detail = dashboardSmokeTask(t, server, taskID)
		if mapValue(t, detail["task"])["status"] == string(status) {
			return detail
		}
		time.Sleep(25 * time.Millisecond)
	}
	supervisorResponse, supervisorValue := apiRequest(t, server, http.MethodGet, "/api/supervisor", nil)
	t.Fatalf(
		"task %s did not reach %s; task=%#v supervisor=%d %#v",
		taskID, status, detail, supervisorResponse.StatusCode, supervisorValue,
	)
	return nil
}

func TestDashboardSupervisorRunsProductionDispatcherFromAPI(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "worker-ran")
	t.Setenv(dashboardWorkerHelperEnvironment, "1")
	t.Setenv("AUTOGORA_DASHBOARD_TEST_BINARY", executable)
	t.Setenv("AUTOGORA_DASHBOARD_WORKER_MARKER", marker)

	planner := dashboardSmokeExecutable(t, "planner.sh", `
case " $* " in *" --auto-approve false "*) ;; *) exit 91 ;; esac
case " $* " in *" --model planner-smoke "*) ;; *) exit 92 ;; esac
case " $* " in *" --provider fixture "*) ;; *) exit 93 ;; esac
printf '%s\n' '{"type":"run_result","text":"{\"fanout\":false,\"rootTitle\":\"Planned production smoke\",\"rootBody\":\"Acceptance: the managed worker completes through the production Dispatcher.\",\"reason\":\"One worker is sufficient.\",\"tasks\":[],\"dependencies\":[]}"}'
`)
	worker := dashboardSmokeExecutable(t, "worker.sh", `
case " $* " in *" --auto-approve true "*) ;; *) exit 94 ;; esac
case " $* " in *" --model worker-smoke "*) ;; *) exit 95 ;; esac
case " $* " in *" --provider fixture "*) ;; *) exit 96 ;; esac
exec "$AUTOGORA_DASHBOARD_TEST_BINARY" -test.run '^TestDashboardSupervisorWorkerHelperProcess$'
`)
	workspace := t.TempDir()
	server := startTestServer(t)

	config := agentconfig.Default()
	config.Supervisor = agentconfig.Supervisor{MaxWorkers: 1, AllowWrites: true}
	config.Defaults.WorkerAgents = []string{"worker"}
	config.Defaults.PlannerAgents = []string{"planner"}
	config.Agents = []agentconfig.Agent{
		{
			ID: "worker", Runtime: model.RuntimeCline, Command: worker,
			Model: "worker-smoke", Provider: "fixture", Enabled: true, MaxConcurrent: 1,
			Roles: []agentconfig.Role{agentconfig.RoleWorker},
		},
		{
			ID: "planner", Runtime: model.RuntimeCline, Command: planner,
			Model: "planner-smoke", Provider: "fixture", Enabled: true, MaxConcurrent: 1,
			Roles: []agentconfig.Role{agentconfig.RolePlanner},
		},
	}
	if response, value := putAgentConfig(t, server, config, false); response.StatusCode != http.StatusOK {
		t.Fatalf("save production Supervisor config: %d %#v", response.StatusCode, value)
	}

	response, value := apiRequest(t, server, http.MethodPatch, "/api/boards/default", map[string]any{
		"defaultWorkdir": workspace,
		"orchestration": map[string]any{
			"autoDecompose": true, "autoDecomposePerTick": 1, "autoPromoteChildren": true,
			"defaultProfile": "worker",
			"autopilot": map[string]any{
				"enabled": true, "autoPlan": true, "autoExecute": true, "workspaceWrites": true,
			},
		},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enable board Autopilot: %d %#v", response.StatusCode, value)
	}
	board := mapValue(t, value)
	autopilot := mapValue(t, mapValue(t, board["orchestration"])["autopilot"])
	if autopilot["enabled"] != true || autopilot["autoPlan"] != true ||
		autopilot["autoExecute"] != true || autopilot["workspaceWrites"] != true {
		t.Fatalf("board Autopilot gates were not persisted: %#v", autopilot)
	}
	startResponse, startValue := apiRequest(t, server, http.MethodPost, "/api/supervisor/start", map[string]any{})
	started := mapValue(t, startValue)
	if startResponse.StatusCode != http.StatusOK || started["running"] != true || started["desired"] != true ||
		started["maxWorkers"] != float64(1) || started["allowWrites"] != true {
		t.Fatalf("production Supervisor did not expose applied config: %d %#v", startResponse.StatusCode, started)
	}
	statusResponse, statusValue := apiRequest(t, server, http.MethodGet, "/api/supervisor", nil)
	status := mapValue(t, statusValue)
	if statusResponse.StatusCode != http.StatusOK || status["running"] != true || status["desired"] != true ||
		status["maxWorkers"] != float64(1) || status["allowWrites"] != true {
		t.Fatalf("production Supervisor status changed unexpectedly: %d %#v", statusResponse.StatusCode, status)
	}

	response, value = apiRequest(t, server, http.MethodPost, "/api/tasks?board=default", map[string]any{
		"title": "rough Dashboard production smoke", "body": "Plan and execute this task.",
		"status": "triage",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create Triage task: %d %#v", response.StatusCode, value)
	}
	taskID, _ := mapValue(t, mapValue(t, value)["task"])["id"].(string)
	if taskID == "" {
		t.Fatalf("created task has no id: %#v", value)
	}
	detail := waitForDashboardSmokeTask(t, server, taskID, model.TaskStatusDone, 15*time.Second)
	task := mapValue(t, detail["task"])
	if task["title"] != "Planned production smoke" || !strings.Contains(task["body"].(string), "production Dispatcher") {
		t.Fatalf("production Planner did not specify the Triage task: %#v", task)
	}
	events := arrayValue(t, detail["events"])
	kinds := make(map[string]bool, len(events))
	for _, raw := range events {
		kinds[mapValue(t, raw)["kind"].(string)] = true
	}
	for _, kind := range []string{"specified", "run_managed", "completion_requested"} {
		if !kinds[kind] {
			t.Fatalf("production lifecycle is missing %q: %#v", kind, events)
		}
	}
	runs := arrayValue(t, detail["runs"])
	configs := arrayValue(t, detail["runAgentConfigs"])
	if len(runs) != 1 || len(configs) != 1 {
		t.Fatalf("production Dispatcher run audit is incomplete: runs=%#v configs=%#v", runs, configs)
	}
	run := mapValue(t, runs[0])
	runID, _ := run["id"].(string)
	if run["status"] != string(model.RunStatusCompleted) || run["exitCode"] != float64(0) {
		t.Fatalf("production Dispatcher did not finalize the managed run: %#v", run)
	}
	runConfig := mapValue(t, configs[0])
	if runConfig["profile"] != "worker" || runConfig["runtime"] != "cline" ||
		runConfig["model"] != "worker-smoke" || runConfig["provider"] != "fixture" {
		t.Fatalf("production Dispatcher selected the wrong worker: %#v", runConfig)
	}
	opened, err := server.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	writePolicy, policyErr := opened.GetManagedRunWritePolicy(context.Background(), runID)
	closeErr := opened.Close()
	if policyErr != nil || closeErr != nil {
		t.Fatal(errors.Join(policyErr, closeErr))
	}
	if writePolicy == nil || !*writePolicy {
		t.Fatalf(
			"global allowWrites and board workspaceWrites did not reach dispatcher.Run: %#v",
			writePolicy,
		)
	}
	markerContents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	markerLines := strings.Split(string(markerContents), "\n")
	if len(markerLines) != 4 || markerLines[0] != taskID || markerLines[1] != "worker" ||
		markerLines[2] != "worker-smoke" || markerLines[3] != workspace {
		t.Fatalf("configured worker did not receive scoped production environment: %q", markerContents)
	}

	stopResponse, stopValue := apiRequest(t, server, http.MethodPost, "/api/supervisor/stop", map[string]any{})
	stopped := mapValue(t, stopValue)
	if stopResponse.StatusCode != http.StatusOK || stopped["running"] != false || stopped["desired"] != false {
		t.Fatalf("production Supervisor did not stop cleanly: %d %#v", stopResponse.StatusCode, stopped)
	}
}
