package dispatcher_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/supervisor"
)

func e2eGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func e2eRepository(t *testing.T, root string) string {
	t.Helper()
	repository := filepath.Join(root, "project")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	e2eGit(t, repository, "init", "-b", "main")
	e2eGit(t, repository, "config", "user.name", "Autogora E2E")
	e2eGit(t, repository, "config", "user.email", "autogora-e2e@example.test")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e2eGit(t, repository, "add", "README.md")
	e2eGit(t, repository, "commit", "-m", "base")
	return repository
}

func e2eAutogoraBinary(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	repository := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(t.TempDir(), "autogora")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/autogora")
	command.Dir = repository
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build Autogora CLI: %v\n%s", err, output)
	}
	return binary
}

func e2eFakeAgent(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "fake-cline.sh")
	script := `#!/bin/sh
set -eu

prompt=
for argument do
  prompt=$argument
done

case "$prompt" in
  *"You are an Autogora graph decomposer."*)
    printf '%s\n' planner >> "$AUTOGORA_E2E_CALL_LOG"
    printf '%s\n' '{"fanout":true,"rootTitle":"Integrate the collaborative result","rootBody":"Acceptance: retain both worker changes, the review evidence, and final validation in the published main branch.","reason":"Two independent implementations can run in parallel before review and final integration.","tasks":[{"key":"worker-a","title":"Implement alpha","body":"Create the alpha implementation file and report verification evidence.","assignee":"worker-a","runtime":"cline","workflowRole":"worker","priority":20,"skills":[]},{"key":"worker-b","title":"Implement beta","body":"Create the beta implementation file and report verification evidence.","assignee":"worker-b","runtime":"cline","workflowRole":"worker","priority":20,"skills":[]},{"key":"review","title":"Review combined workers","body":"Verify both worker handoffs, add durable review evidence, and complete only when both are present.","assignee":"reviewer","runtime":"cline","workflowRole":"reviewer","priority":10,"skills":[]}],"dependencies":[{"parent":"worker-a","child":"review"},{"parent":"worker-b","child":"review"}]}'
    exit 0
    ;;
  *"You are the independent completion judge for a goal-mode Autogora worker."*)
    printf '%s\n' judge >> "$AUTOGORA_E2E_CALL_LOG"
    printf '%s\n' '{"complete":true,"reason":"The finalizer retained both worker files and review evidence, then added final validation.","nextPrompt":""}'
    exit 0
    ;;
esac

profile=${AUTOGORA_AGENT_PROFILE:-}
printf 'worker:%s\n' "$profile" >> "$AUTOGORA_E2E_CALL_LOG"
case "$profile" in
  worker-a)
    printf '%s\n' alpha > "$AUTOGORA_WORKSPACE/alpha.txt"
    : > "$AUTOGORA_E2E_BARRIER/worker-a"
    attempt=0
    while [ ! -f "$AUTOGORA_E2E_BARRIER/worker-b" ] && [ "$attempt" -lt 200 ]; do
      sleep 0.05
      attempt=$((attempt + 1))
    done
    test -f "$AUTOGORA_E2E_BARRIER/worker-b"
    ;;
  worker-b)
    printf '%s\n' beta > "$AUTOGORA_WORKSPACE/beta.txt"
    : > "$AUTOGORA_E2E_BARRIER/worker-b"
    attempt=0
    while [ ! -f "$AUTOGORA_E2E_BARRIER/worker-a" ] && [ "$attempt" -lt 200 ]; do
      sleep 0.05
      attempt=$((attempt + 1))
    done
    test -f "$AUTOGORA_E2E_BARRIER/worker-a"
    ;;
  reviewer)
    test "$(cat "$AUTOGORA_WORKSPACE/alpha.txt")" = alpha
    test "$(cat "$AUTOGORA_WORKSPACE/beta.txt")" = beta
    printf '%s\n' reviewed > "$AUTOGORA_WORKSPACE/review.txt"
    ;;
  finalizer)
    test "$(cat "$AUTOGORA_WORKSPACE/alpha.txt")" = alpha
    test "$(cat "$AUTOGORA_WORKSPACE/beta.txt")" = beta
    test "$(cat "$AUTOGORA_WORKSPACE/review.txt")" = reviewed
    printf '%s\n' validated > "$AUTOGORA_WORKSPACE/final.txt"
    printf '%s\n' '{"type":"run_result","text":"final validation passed"}'
    exit 0
    ;;
  *)
    printf 'unexpected worker profile: %s\n' "$profile" >&2
    exit 2
    ;;
esac

"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "$profile completed with verification evidence" >/dev/null
printf '%s\n' '{"type":"run_result","text":"task completed"}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func e2eAgentConfig(command string) agentconfig.Config {
	worker := func(id string) agentconfig.Agent {
		return agentconfig.Agent{
			ID: id, Runtime: model.RuntimeCline, Command: command,
			Enabled: true, MaxConcurrent: 1,
			Roles: []agentconfig.Role{agentconfig.RoleWorker},
		}
	}
	return agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor: agentconfig.Supervisor{
			AutoStart: true, MaxWorkers: 2, AllowWrites: true,
		},
		Defaults: agentconfig.Defaults{
			WorkerAgents:  []string{"worker-a", "worker-b", "reviewer", "finalizer"},
			PlannerAgents: []string{"planner-agent"},
			JudgeAgents:   []string{"planner-agent"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: "planner-agent", Runtime: model.RuntimeCline, Command: command,
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RolePlanner, agentconfig.RoleJudge},
			},
			worker("worker-a"),
			worker("worker-b"),
			worker("reviewer"),
			worker("finalizer"),
		},
	}
}

func e2eCountLines(contents string) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(contents), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			counts[line]++
		}
	}
	return counts
}

func TestSupervisorRunsPlannedGraphThroughGoalJudgeAndLocalFFPublication(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	root := t.TempDir()
	repository := e2eRepository(t, root)
	dbPath := filepath.Join(root, "autogora.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "default", boards.Update{}); err != nil {
		t.Fatal(err)
	}

	enabled, writes, noApproval := true, true, false
	autoPlan, autoExecute, autoDecompose, autoPromote := true, true, true, true
	mode, target, remote := boards.PublicationModeLocalFF, "main", "origin"
	defaultProfile, finalizerProfile := "worker-a", "finalizer"
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
		Orchestration: &boards.OrchestrationUpdate{
			AutoDecompose:       &autoDecompose,
			AutoPromoteChildren: &autoPromote,
			DefaultProfile:      store.OptionalString{Set: true, Value: &defaultProfile},
			FinalizerProfile:    store.OptionalString{Set: true, Value: &finalizerProfile},
			Autopilot: &boards.AutopilotUpdate{
				Enabled: &enabled, AutoPlan: &autoPlan, AutoExecute: &autoExecute,
				WorkspaceWrites: &writes,
				Publication: &boards.PublicationUpdate{
					Mode: &mode, TargetBranch: &target, Remote: &remote,
					RequireApproval: &noApproval,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	rootTask, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title:  "Build a collaborative feature",
		Body:   "Use two independent implementations, review their combined result, validate it, and publish it to main.",
		Status: model.TaskStatusTriage, WorkspaceKind: model.WorkspaceWorktree,
		GoalMode: true, GoalMaxTurns: 2,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	before, err := opened.ListTasks(context.Background(), store.ListTaskFilter{Limit: 10})
	closeErr := opened.Close()
	if err != nil {
		t.Fatal(fmt.Errorf("inspect initial board: %w", err))
	}
	if closeErr != nil {
		t.Fatal(fmt.Errorf("close initial board: %w", closeErr))
	}
	if len(before) != 1 || before[0].ID != rootTask.Task.ID ||
		before[0].Status != model.TaskStatusTriage {
		t.Fatalf("initial board = %+v, want one Triage card", before)
	}

	callLog := filepath.Join(root, "agent-calls.log")
	barrier := filepath.Join(root, "parallel-barrier")
	if err := os.MkdirAll(barrier, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOGORA_E2E_CALL_LOG", callLog)
	t.Setenv("AUTOGORA_E2E_BARRIER", barrier)
	t.Setenv("AUTOGORA_CLINE_BIN", "")
	t.Setenv("AUTOGORA_CLINE_MODEL", "")
	t.Setenv("AUTOGORA_CLINE_PROVIDER", "")
	fakeAgent := e2eFakeAgent(t, root)
	config := e2eAgentConfig(fakeAgent)
	if err := agentconfig.Validate(config); err != nil {
		t.Fatalf("validate E2E agent configuration: %v", err)
	}
	cliPath := e2eAutogoraBinary(t)

	var logMu sync.Mutex
	logs := make([]string, 0)
	controller := supervisor.New(supervisor.Options{
		DBPath: dbPath, CLIPath: cliPath, WorkingDirectory: repository,
		AgentConfigLoader: func() (agentconfig.Config, error) {
			return config, nil
		},
		OnLog: func(message string) {
			logMu.Lock()
			logs = append(logs, message)
			logMu.Unlock()
		},
	})
	runContext, cancelRun := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelRun()
	if !controller.Start(runContext, config) {
		t.Fatal("Supervisor did not start")
	}
	stopped := false
	defer func() {
		if stopped {
			return
		}
		stopContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = controller.Stop(stopContext)
	}()

	check, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var publication model.Publication
	published := false
	for !published {
		select {
		case <-runContext.Done():
			detail, _ := check.GetTask(context.Background(), rootTask.Task.ID)
			tasks, _ := check.ListTasks(context.Background(), store.ListTaskFilter{Limit: 20})
			publications, _ := check.ListPublications(context.Background(), store.PublicationFilter{
				TaskID: rootTask.Task.ID,
			})
			logMu.Lock()
			logSnapshot := append([]string(nil), logs...)
			logMu.Unlock()
			t.Fatalf(
				"timed out waiting for publication: supervisor=%+v root=%+v tasks=%+v publications=%+v logs=%v",
				controller.Status(), detail.Task, tasks, publications, logSnapshot,
			)
		case <-ticker.C:
			detail, readErr := check.GetTask(context.Background(), rootTask.Task.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if detail.Task.Status == model.TaskStatusBlocked {
				logMu.Lock()
				logSnapshot := append([]string(nil), logs...)
				logMu.Unlock()
				t.Fatalf("root Finalizer blocked: reason=%v events=%+v logs=%v",
					detail.Task.BlockReason, detail.Events, logSnapshot)
			}
			publications, listErr := check.ListPublications(
				context.Background(),
				store.PublicationFilter{TaskID: rootTask.Task.ID},
			)
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, candidate := range publications {
				if candidate.Status == model.PublicationFailed {
					t.Fatalf("publication failed: %+v", candidate)
				}
				if candidate.Status == model.PublicationPublished {
					publication, published = candidate, true
				}
			}
		}
	}
	stopContext, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	if err := controller.Stop(stopContext); err != nil {
		cancelStop()
		t.Fatal(err)
	}
	cancelStop()
	stopped = true

	status := controller.Status()
	if status.RestartCount != 0 || status.LastError != "" {
		t.Fatalf("Supervisor restarted during happy path: %+v", status)
	}
	rootDetail, err := check.GetTask(context.Background(), rootTask.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootDetail.Task.Status != model.TaskStatusDone ||
		rootDetail.Task.WorkflowRole != model.WorkflowRoleFinalizer ||
		rootDetail.Task.Assignee == nil || *rootDetail.Task.Assignee != "finalizer" ||
		len(rootDetail.Subtasks) != 3 {
		t.Fatalf("unexpected root Finalizer result: %+v", rootDetail)
	}
	if len(rootDetail.Runs) != 1 ||
		rootDetail.Runs[0].Status != model.RunStatusCompleted ||
		len(rootDetail.ChangeSets) != 1 {
		t.Fatalf("root Finalizer did not produce one completed run and change set: %+v", rootDetail)
	}

	expectedRoles := map[string]model.WorkflowRole{
		"worker-a": model.WorkflowRoleWorker,
		"worker-b": model.WorkflowRoleWorker,
		"reviewer": model.WorkflowRoleReviewer,
	}
	childDetails := make(map[string]model.TaskDetail, len(rootDetail.Subtasks))
	workspaces := map[string]string{}
	for _, child := range rootDetail.Subtasks {
		if child.Assignee == nil {
			t.Fatalf("planned child has no profile: %+v", child)
		}
		profile := *child.Assignee
		expectedRole, exists := expectedRoles[profile]
		if !exists || child.WorkflowRole != expectedRole {
			t.Fatalf("planned child route = %s/%s", profile, child.WorkflowRole)
		}
		detail, getErr := check.GetTask(context.Background(), child.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if detail.Task.Status != model.TaskStatusDone ||
			len(detail.Runs) != 1 ||
			detail.Runs[0].Status != model.RunStatusCompleted ||
			len(detail.ChangeSets) != 1 ||
			len(detail.RunAgentConfigs) != 1 ||
			detail.RunAgentConfigs[0].Profile != profile ||
			len(detail.RunWorkspaces) != 1 ||
			detail.RunWorkspaces[0].Kind != model.WorkspaceWorktree ||
			!detail.RunWorkspaces[0].Generated {
			t.Fatalf("child %s did not complete through its routed worktree: %+v", profile, detail)
		}
		if owner, duplicate := workspaces[detail.RunWorkspaces[0].Path]; duplicate {
			t.Fatalf("profiles %s and %s shared a worktree", owner, profile)
		}
		workspaces[detail.RunWorkspaces[0].Path] = profile
		childDetails[profile] = detail
	}
	if len(rootDetail.RunAgentConfigs) != 1 ||
		rootDetail.RunAgentConfigs[0].Profile != "finalizer" ||
		len(rootDetail.RunWorkspaces) != 1 ||
		rootDetail.RunWorkspaces[0].Kind != model.WorkspaceWorktree ||
		!rootDetail.RunWorkspaces[0].Generated {
		t.Fatalf("root did not run through the Finalizer profile and worktree: %+v", rootDetail)
	}
	if owner, duplicate := workspaces[rootDetail.RunWorkspaces[0].Path]; duplicate {
		t.Fatalf("Finalizer shared a worktree with %s", owner)
	}

	reviewer := childDetails["reviewer"]
	if len(reviewer.Prerequisites) != 2 ||
		len(rootDetail.Prerequisites) != 1 ||
		rootDetail.Prerequisites[0].ID != reviewer.Task.ID {
		t.Fatalf("planned graph lost worker → reviewer → Finalizer dependencies: reviewer=%+v root=%+v",
			reviewer.Prerequisites, rootDetail.Prerequisites)
	}

	finalHead := rootDetail.ChangeSets[0].HeadCommit
	for profile, detail := range childDetails {
		childHead := detail.ChangeSets[0].HeadCommit
		command := exec.Command(
			"git", "-C", repository, "merge-base", "--is-ancestor", childHead, finalHead,
		)
		if output, ancestorErr := command.CombinedOutput(); ancestorErr != nil {
			t.Fatalf("Finalizer dropped %s head %s: %v\n%s",
				profile, childHead, ancestorErr, output)
		}
	}

	publications, err := check.ListPublications(
		context.Background(),
		store.PublicationFilter{TaskID: rootTask.Task.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 1 ||
		publication.ID != publications[0].ID ||
		publication.Status != model.PublicationPublished ||
		publication.Mode != model.PublicationModeLocalFF ||
		publication.PublishedAt == nil ||
		publication.HeadCommit != finalHead {
		t.Fatalf("production Publisher did not record exactly one local_ff publication: %+v", publications)
	}
	if mainHead := e2eGit(t, repository, "rev-parse", "main^{commit}"); mainHead != finalHead {
		t.Fatalf("main = %s, want published Finalizer head %s", mainHead, finalHead)
	}
	for path, expected := range map[string]string{
		"alpha.txt": "alpha\n", "beta.txt": "beta\n",
		"review.txt": "reviewed\n", "final.txt": "validated\n",
	} {
		contents, readErr := os.ReadFile(filepath.Join(repository, path))
		if readErr != nil || string(contents) != expected {
			t.Fatalf("published %s = %q, err=%v", path, contents, readErr)
		}
	}
	if dirty := e2eGit(t, repository, "status", "--porcelain"); dirty != "" {
		t.Fatalf("Publisher left main dirty: %s", dirty)
	}

	callContents, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	counts := e2eCountLines(string(callContents))
	for name, expected := range map[string]int{
		"planner":          1,
		"judge":            1,
		"worker:worker-a":  1,
		"worker:worker-b":  1,
		"worker:reviewer":  1,
		"worker:finalizer": 1,
	} {
		if counts[name] != expected {
			t.Fatalf("agent call count %s = %d, want %d; calls:\n%s",
				name, counts[name], expected, callContents)
		}
	}
	goalJudgments := 0
	for _, event := range rootDetail.Events {
		if event.Kind == "goal_judged" {
			goalJudgments++
		}
	}
	if goalJudgments != 1 {
		t.Fatalf("Goal Judge event count = %d, want 1; events=%+v",
			goalJudgments, rootDetail.Events)
	}
}
