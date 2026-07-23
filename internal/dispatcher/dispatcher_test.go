package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func boolValue(value bool) *bool                       { return &value }
func durationValue(value time.Duration) *time.Duration { return &value }

func testManager(t *testing.T) (*boards.Manager, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "default", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	return manager, dbPath
}

func executableFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+content+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildAutogora(t *testing.T) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	repository := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	path := filepath.Join(t.TempDir(), "autogora")
	command := exec.Command("go", "build", "-o", path, "./cmd/autogora")
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Autogora: %v\n%s", err, output)
	}
	return path
}

func gitRepositoryFixture(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		process := exec.Command("git", append([]string{"-C", repository}, args...)...)
		process.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
		output, err := process.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	gitInit := exec.Command("git", "init", repository)
	if output, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command("add", "README.md")
	command("commit", "-m", "base")
	return repository
}

func TestDispatcherRateLimitDoesNotConsumeRetry(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "rate limited", Assignee: &assignee, Runtime: model.RuntimeCodex})
	opened.Close()
	fixture := executableFixture(t, "exit 75")
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true, MaxWorkers: 1,
		RateLimitCooldown: durationValue(0), AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CODEX_BIN" {
				return fixture
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusReady || detail.Task.FailureCount != 0 || detail.Runs[0].Status != model.RunStatusRateLimited {
		t.Fatalf("unexpected retry state: %#v", detail)
	}
}

func TestRecoveryRequeuesRecordedDeadWorker(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	defer opened.Close()
	assignee := "worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "crashed", Assignee: &assignee, Runtime: model.RuntimeCodex})
	claim, _ := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	deadPID := 99999999
	if _, err := opened.RecordSpawn(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, deadPID, filepath.Join(t.TempDir(), "dead.log")); err != nil {
		t.Fatal(err)
	}
	options := Options{CrashGrace: durationValue(0)}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, _ := opened.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusReady || detail.Task.FailureCount != 1 || detail.Runs[0].Status != model.RunStatusCrashed {
		t.Fatalf("dead worker was not recovered: %#v", detail)
	}
}

func TestDispatcherAutoSpecifiesTriageWithInjectedPlanner(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "rough operational idea", Status: model.TaskStatusTriage})
	opened.Close()
	kinds := []orchestration.PlannerKind{}
	planner := func(_ context.Context, request orchestration.PlannerRequest) (any, error) {
		kinds = append(kinds, request.Kind)
		return map[string]any{"fanout": false, "rootTitle": "Audit backups", "rootBody": "Acceptance: record restore evidence.", "reason": "one specialist", "tasks": []any{}, "dependencies": []any{}}, nil
	}
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "automatic specification completed" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	profile := orchestration.ProfileRoute{Name: "operator", Runtime: model.RuntimeCline}
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(true), DecompositionPlanner: planner,
		DefaultProfile: &profile, Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusDone || detail.Task.Assignee == nil || *detail.Task.Assignee != "operator" || !strings.Contains(detail.Task.Body, "Acceptance") || len(kinds) != 1 || kinds[0] != orchestration.PlannerDecompose {
		t.Fatalf("unexpected auto specification: %#v, kinds=%v", detail, kinds)
	}
}

func TestOneShotDispatcherReclaimsHungWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	manager, dbPath := testManager(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "hung-worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "hung", Assignee: &assignee, Runtime: model.RuntimeCodex})
	opened.Close()
	fixture := executableFixture(t, `while :; do sleep 1; done`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true, MaxWorkers: 1,
		Interval: 250 * time.Millisecond, ClaimTTLSeconds: 1, AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CODEX_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(context.Background(), "default")
	defer check.Close()
	detail, _ := check.GetTask(context.Background(), task.Task.ID)
	if detail.Task.Status == model.TaskStatusRunning || detail.Task.CurrentRunID != nil || len(detail.Runs) != 1 || detail.Runs[0].Status == model.RunStatusRunning {
		t.Fatalf("one-shot watchdog left a stranded run: %#v", detail)
	}
}

func TestTargetedDispatcherRunsOnlyRequestedTask(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	first, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first", Assignee: &assignee, Runtime: model.RuntimeCline, Priority: 100})
	second, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "target complete" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: second.Task.ID, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CLINE_BIN" {
			return fixture
		}
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	firstDetail, _ := check.GetTask(ctx, first.Task.ID)
	secondDetail, _ := check.GetTask(ctx, second.Task.ID)
	if firstDetail.Task.Status != model.TaskStatusReady || secondDetail.Task.Status != model.TaskStatusDone {
		t.Fatalf("target dispatch changed the wrong task: first=%s second=%s", firstDetail.Task.Status, secondDetail.Task.Status)
	}
}

func TestWritableSharedDirectoryRunsAreSerializedWithoutRetryPenalty(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "writer"
	directory := t.TempDir()
	first, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first writer", Assignee: &assignee, Runtime: model.RuntimeCline, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	second, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second writer", Assignee: &assignee, Runtime: model.RuntimeCline, Workspace: &directory, WorkspaceKind: model.WorkspaceDir})
	opened.Close()
	fixture := executableFixture(t, `
sleep 1
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "serialized writer completed" >/dev/null`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, MaxWorkers: 2, Interval: 250 * time.Millisecond,
		AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	firstDetail, _ := check.GetTask(ctx, first.Task.ID)
	secondDetail, _ := check.GetTask(ctx, second.Task.ID)
	done, waiting := firstDetail, secondDetail
	if secondDetail.Task.Status == model.TaskStatusDone {
		done, waiting = secondDetail, firstDetail
	}
	if done.Task.Status != model.TaskStatusDone || (waiting.Task.Status != model.TaskStatusReady && waiting.Task.Status != model.TaskStatusScheduled) || waiting.Task.FailureCount != 0 {
		t.Fatalf("shared writers were not serialized cleanly: first=%#v second=%#v", firstDetail, secondDetail)
	}
}

func TestDispatcherCapturesWorktreeChangesWithoutMovingUserCheckout(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "writer"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "isolated change", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
printf '%s\n' 'before terminal request' > "$AUTOGORA_WORKSPACE/feature.txt"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "change ready" >/dev/null
printf '%s\n' 'after terminal request' >> "$AUTOGORA_WORKSPACE/feature.txt"`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CLINE_BIN" {
			return fixture
		}
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusDone || len(detail.ChangeSets) != 1 || detail.ChangeSets[0].State != "ready" {
		t.Fatalf("worktree completion lacks a durable change set: %#v", detail)
	}
	change := detail.ChangeSets[0]
	show := exec.Command("git", "-C", repository, "show", change.HeadCommit+":feature.txt")
	contents, err := show.Output()
	if err != nil || string(contents) != "before terminal request\nafter terminal request\n" {
		t.Fatalf("snapshot omitted final worker changes: %q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(repository, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("worker changed the user checkout: %v", err)
	}
	status := exec.Command("git", "-C", repository, "status", "--porcelain")
	if output, err := status.Output(); err != nil || len(output) != 0 {
		t.Fatalf("snapshot dirtied the user checkout: %q err=%v", output, err)
	}
	ref := exec.Command("git", "-C", repository, "rev-parse", change.DurableRef)
	refHead, err := ref.Output()
	if err != nil || strings.TrimSpace(string(refHead)) != change.HeadCommit {
		t.Fatalf("durable ref mismatch: %q want %s err=%v", refHead, change.HeadCommit, err)
	}
}

func TestDispatcherIntegratesPinnedPrerequisiteChangeSetsBeforeWorkerLaunch(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "writer"
	first, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first prerequisite", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second prerequisite", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	child, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "fan-in implementation", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, first.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, second.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	opened.Close()

	cliPath := buildAutogora(t)
	fixture := executableFixture(t, fmt.Sprintf(`
case "$AUTOGORA_TASK_ID" in
  %s) printf 'first prerequisite\n' > "$AUTOGORA_WORKSPACE/first.txt" ;;
  %s) printf 'second prerequisite\n' > "$AUTOGORA_WORKSPACE/second.txt" ;;
  %s)
    test "$(cat "$AUTOGORA_WORKSPACE/first.txt")" = "first prerequisite"
    test "$(cat "$AUTOGORA_WORKSPACE/second.txt")" = "second prerequisite"
    printf 'fan-in complete\n' > "$AUTOGORA_WORKSPACE/child.txt"
    ;;
esac
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "work completed" >/dev/null`, first.Task.ID, second.Task.ID, child.Task.ID))
	runTarget := func(taskID string) {
		t.Helper()
		if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: taskID, Once: true,
			AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
				if name == "AUTOGORA_CLINE_BIN" {
					return fixture
				}
				return ""
			}}); err != nil {
			t.Fatal(err)
		}
	}
	runTarget(first.Task.ID)
	runTarget(second.Task.ID)
	runTarget(child.Task.ID)

	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	firstDetail, _ := check.GetTask(ctx, first.Task.ID)
	secondDetail, _ := check.GetTask(ctx, second.Task.ID)
	childDetail, _ := check.GetTask(ctx, child.Task.ID)
	if childDetail.Task.Status != model.TaskStatusDone || len(firstDetail.ChangeSets) != 1 || len(secondDetail.ChangeSets) != 1 || len(childDetail.ChangeSets) != 1 {
		t.Fatalf("fan-in did not complete with durable change sets: first=%#v second=%#v child=%#v", firstDetail, secondDetail, childDetail)
	}
	childChange := childDetail.ChangeSets[0]
	if len(childChange.ChangedFiles) != 1 || childChange.ChangedFiles[0] != "child.txt" {
		t.Fatalf("child change set includes inherited prerequisite files: %#v", childChange)
	}
	for _, parentHead := range []string{firstDetail.ChangeSets[0].HeadCommit, secondDetail.ChangeSets[0].HeadCommit} {
		command := exec.Command("git", "-C", repository, "merge-base", "--is-ancestor", parentHead, childChange.HeadCommit)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("child head dropped prerequisite %s: %v\n%s", parentHead, err, output)
		}
	}
}

func TestDispatcherBlocksConflictingPrerequisiteChangeSetsWithoutLaunchingWorker(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "writer"
	first, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "first conflicting prerequisite", Assignee: &assignee, Runtime: model.RuntimeCline})
	second, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "second conflicting prerequisite", Assignee: &assignee, Runtime: model.RuntimeCline})
	child, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "conflicting fan-in", Assignee: &assignee, Runtime: model.RuntimeCline})
	if _, err := opened.LinkTasks(ctx, first.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, second.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	opened.Close()
	marker := filepath.Join(t.TempDir(), "child-worker-launched")
	t.Setenv("AUTOGORA_TEST_CHILD_MARKER", marker)
	cliPath := buildAutogora(t)
	fixture := executableFixture(t, fmt.Sprintf(`
case "$AUTOGORA_TASK_ID" in
  %s) printf 'first version\n' > "$AUTOGORA_WORKSPACE/README.md" ;;
  %s) printf 'second version\n' > "$AUTOGORA_WORKSPACE/README.md" ;;
  %s) touch "$AUTOGORA_TEST_CHILD_MARKER" ;;
esac
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "work completed" >/dev/null`, first.Task.ID, second.Task.ID, child.Task.ID))
	runTarget := func(taskID string) {
		t.Helper()
		if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: taskID, Once: true,
			AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
				if name == "AUTOGORA_CLINE_BIN" {
					return fixture
				}
				return ""
			}}); err != nil {
			t.Fatal(err)
		}
	}
	runTarget(first.Task.ID)
	runTarget(second.Task.ID)
	runTarget(child.Task.ID)

	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, child.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.FailureCount != 0 || detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindNeedsInput || detail.Task.BlockReason == nil || !strings.Contains(*detail.Task.BlockReason, "conflicts in README.md") {
		t.Fatalf("conflicting fan-in was not blocked for review: %#v", detail)
	}
	if len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusBlocked {
		t.Fatalf("conflicting integration left an invalid run history: %#v", detail.Runs)
	}
	if len(detail.ChangeSets) != 0 || len(detail.RunWorkspaces) != 1 {
		t.Fatalf("conflicting integration recorded a child change set: %#v", detail)
	}
	if unmerged, err := exec.Command("git", "-C", detail.RunWorkspaces[0].Path, "ls-files", "-u").Output(); err != nil || len(unmerged) != 0 {
		t.Fatalf("conflicting integration left unmerged files: %q err=%v", unmerged, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child worker launched despite prerequisite conflict: %v", err)
	}
}

func TestDispatcherRunsClineThroughGoCLIBridge(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	profiles := []boards.Profile{{Name: "cline-worker", Runtime: model.RuntimeCline, Model: "cline-test-model", Provider: "test-provider"}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
		t.Fatal(err)
	}
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "cline-worker"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "Cline bridge", Assignee: &assignee, Runtime: model.RuntimeCline})
	opened.Close()
	fixture := executableFixture(t, `
test "$AUTOGORA_AGENT_PROFILE" = "cline-worker"
test "$AUTOGORA_MODEL" = "cline-test-model"
test "$AUTOGORA_PROVIDER" = "test-provider"
"$AUTOGORA_CLI" show "$AUTOGORA_TASK_ID" >/dev/null
"$AUTOGORA_CLI" heartbeat "$AUTOGORA_TASK_ID" --note "running" >/dev/null
"$AUTOGORA_CLI" comment "$AUTOGORA_TASK_ID" "Cline used the Go CLI bridge" --author cline >/dev/null
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "completed through Go CLI" --metadata '{"verification":["go-cli"]}' >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CLINE_BIN" {
			return fixture
		}
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusDone || len(detail.Comments) != 1 || detail.Runs[0].Summary == nil || *detail.Runs[0].Summary != "completed through Go CLI" ||
		len(detail.RunAgentConfigs) != 1 || detail.RunAgentConfigs[0].Profile != "cline-worker" || detail.RunAgentConfigs[0].Model != "cline-test-model" ||
		detail.RunAgentConfigs[0].Provider != "test-provider" || detail.RunAgentConfigs[0].Source != "board_profile" {
		t.Fatalf("unexpected Cline result: %#v", detail)
	}
}

func TestDispatcherSkipsDisabledProfiles(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	profiles := []boards.Profile{{Name: "paused", Runtime: model.RuntimeCodex, Disabled: true}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
		t.Fatal(err)
	}
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "paused"
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "must stay queued", Assignee: &assignee, Runtime: model.RuntimeCodex})
	opened.Close()
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true, AutoDecompose: boolValue(false), Getenv: func(string) string {
		return ""
	}}); err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	if detail.Task.Status != model.TaskStatusReady || len(detail.Runs) != 0 {
		t.Fatalf("disabled profile was dispatched: %#v", detail)
	}
}

func TestDispatcherResumesGoalUntilTerminalCall(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	opened, _ := manager.OpenStore(ctx, "default")
	assignee := "worker"
	workspacePath := t.TempDir()
	task, _ := opened.CreateTask(ctx, store.CreateTaskInput{Title: "goal", Body: "Acceptance: finish second turn", Assignee: &assignee, Runtime: model.RuntimeCodex, Workspace: &workspacePath, GoalMode: true, GoalMaxTurns: 3})
	opened.Close()
	fixture := executableFixture(t, `
marker="$AUTOGORA_WORKSPACE/.goal-turn"
if [ ! -f "$marker" ]; then
  touch "$marker"
  printf '%s\n' '{"thread_id":"session-1"}'
else
  "$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "goal complete" --metadata '{"turns":2}' >/dev/null
fi`)
	judged := 0
	err := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Once: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
		if name == "AUTOGORA_CODEX_BIN" {
			return fixture
		}
		return ""
	}, GoalJudge: func(_ context.Context, _ model.TaskDetail, turn int, _ string) (orchestration.GoalJudgment, error) {
		judged++
		if judged == 2 {
			return orchestration.GoalJudgment{Complete: true, Reason: "acceptance verified"}, nil
		}
		return orchestration.GoalJudgment{Complete: false, Reason: "one gap", NextPrompt: "finish the gap"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	check, _ := manager.OpenStore(ctx, "default")
	defer check.Close()
	detail, _ := check.GetTask(ctx, task.Task.ID)
	spawned := 0
	for _, event := range detail.Events {
		if event.Kind == "spawned" {
			spawned++
		}
	}
	if detail.Task.Status != model.TaskStatusDone || judged != 2 || spawned != 2 {
		t.Fatalf("unexpected goal result: status=%s judged=%d spawned=%d detail=%#v", detail.Task.Status, judged, spawned, detail)
	}
}

func TestDispatcherUsesConfiguredFallbackWithoutConsumingRetry(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	cliPath := buildAutogora(t)
	primaryCommand := executableFixture(t, `
printf '%s\n' 'quota exceeded' >&2
exit 75`)
	fallbackCommand := executableFixture(t, `
test "$AUTOGORA_AGENT_PROFILE" = "claude-backup"
test "$AUTOGORA_MODEL" = "claude-fallback-model"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "fallback completed" >/dev/null`)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"codex-primary"}},
		Agents: []agentconfig.Agent{
			{ID: "codex-primary", Runtime: model.RuntimeCodex, Command: primaryCommand, Model: "codex-primary-model", Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker}, Fallbacks: []string{"claude-backup"}},
			{ID: "claude-backup", Runtime: model.RuntimeClaude, Command: fallbackCommand, Model: "claude-fallback-model", Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker}},
		},
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "codex-primary"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "fallback work", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()

	options := Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", Once: true, MaxWorkers: 1,
		RateLimitCooldown: durationValue(time.Hour), AutoDecompose: boolValue(false), AgentConfig: &config, Getenv: func(string) string { return "" }}
	if err := Run(ctx, options); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	first, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	primaryHealth, err := check.GetAgentHealth(ctx, "codex-primary")
	if err != nil {
		t.Fatal(err)
	}
	check.Close()
	if first.Task.Status != model.TaskStatusReady || first.Task.FailureCount != 0 || len(first.Runs) != 1 || first.Runs[0].Status != model.RunStatusRateLimited {
		t.Fatalf("primary availability failure consumed retry or stranded task: %#v", first)
	}
	if primaryHealth.Status != model.AgentHealthRateLimited || primaryHealth.CooldownUntil == nil || primaryHealth.LastRunID == nil || *primaryHealth.LastRunID != first.Runs[0].ID {
		t.Fatalf("primary health was not quarantined: %#v", primaryHealth)
	}
	if len(first.RunAgentConfigs) != 1 || first.RunAgentConfigs[0].Profile != "codex-primary" || first.RunAgentConfigs[0].Runtime != model.RuntimeCodex ||
		first.RunAgentConfigs[0].Model != "codex-primary-model" || first.RunAgentConfigs[0].Source != "global_profile" || first.RunAgentConfigs[0].FallbackFrom != nil {
		t.Fatalf("primary run configuration was not pinned: %#v", first.RunAgentConfigs)
	}

	if err := Run(ctx, options); err != nil {
		t.Fatal(err)
	}
	check, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	completed, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fallbackHealth, err := check.GetAgentHealth(ctx, "claude-backup")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone || completed.Task.FailureCount != 0 || len(completed.Runs) != 2 {
		t.Fatalf("fallback did not complete cleanly: %#v", completed)
	}
	var fallbackConfig *model.RunAgentConfig
	for index := range completed.RunAgentConfigs {
		if completed.RunAgentConfigs[index].Profile == "claude-backup" {
			fallbackConfig = &completed.RunAgentConfigs[index]
			break
		}
	}
	if fallbackConfig == nil || fallbackConfig.Runtime != model.RuntimeClaude || fallbackConfig.Model != "claude-fallback-model" ||
		fallbackConfig.Source != "fallback" || fallbackConfig.FallbackFrom == nil || *fallbackConfig.FallbackFrom != "codex-primary" {
		t.Fatalf("fallback run configuration was not audited: %#v", completed.RunAgentConfigs)
	}
	if fallbackHealth.Status != model.AgentHealthReady || fallbackHealth.LastRunID == nil || *fallbackHealth.LastRunID != fallbackConfig.RunID {
		t.Fatalf("successful fallback health was not recorded: %#v", fallbackHealth)
	}
}

func TestDispatcherQuarantinesMissingConfiguredCommandWithoutConsumingRetry(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	missingCommand := filepath.Join(t.TempDir(), "missing", "codex")
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"missing-codex"}},
		Agents: []agentconfig.Agent{{ID: "missing-codex", Runtime: model.RuntimeCodex, Command: missingCommand, Model: "configured-model",
			Enabled: true, MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleWorker}}},
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "missing-codex"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "missing command", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()

	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Board: "default", Once: true, MaxWorkers: 1,
		AutoDecompose: boolValue(false), AgentConfig: &config, Getenv: func(string) string { return "" }}); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	health, err := check.GetAgentHealth(ctx, "missing-codex")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusReady || detail.Task.FailureCount != 0 || len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusSpawnFailed {
		t.Fatalf("missing executable consumed retry or stranded task: %#v", detail)
	}
	if health.Status != model.AgentHealthMissing || health.LastRunID == nil || *health.LastRunID != detail.Runs[0].ID || health.LastError == nil {
		t.Fatalf("missing executable health was not recorded: %#v", health)
	}
	if len(detail.RunAgentConfigs) != 1 || detail.RunAgentConfigs[0].Profile != "missing-codex" || detail.RunAgentConfigs[0].Source != "global_profile" {
		t.Fatalf("missing agent run was not auditable: %#v", detail.RunAgentConfigs)
	}
}

func TestDispatcherPreservesPartialWorktreeAndDoesNotRunFallback(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "fallback-ran")
	t.Setenv("AUTOGORA_TEST_FALLBACK_MARKER", marker)
	primaryCommand := executableFixture(t, `
printf '%s\n' 'unfinished change' > "$AUTOGORA_WORKSPACE/partial.txt"
exit 75`)
	fallbackCommand := executableFixture(t, `
touch "$AUTOGORA_TEST_FALLBACK_MARKER"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "fallback should not run" >/dev/null`)
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1, AllowWrites: true},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"primary-writer"}},
		Agents: []agentconfig.Agent{
			{ID: "primary-writer", Runtime: model.RuntimeCodex, Command: primaryCommand, Model: "writer-model", Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker}, Fallbacks: []string{"fallback-writer"}},
			{ID: "fallback-writer", Runtime: model.RuntimeClaude, Command: fallbackCommand, Model: "fallback-model", Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker}},
		},
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "primary-writer"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "preserve partial edits", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	options := Options{DBPath: dbPath, CLIPath: buildAutogora(t), Board: "default", Once: true, MaxWorkers: 1, AllowWrites: true,
		RateLimitCooldown: durationValue(time.Hour), AutoDecompose: boolValue(false), AgentConfig: &config, Getenv: func(string) string { return "" }}

	if err := Run(ctx, options); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, options); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.FailureCount != 0 || detail.Task.BlockReason == nil ||
		!strings.Contains(*detail.Task.BlockReason, "partial changes remain") {
		t.Fatalf("partial availability failure did not block for review: %#v", detail)
	}
	if len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusBlocked || len(detail.RunWorkspaces) != 1 {
		t.Fatalf("partial run history is incomplete: %#v", detail)
	}
	workspace := detail.RunWorkspaces[0]
	if workspace.Kind != model.WorkspaceWorktree || !workspace.Generated {
		t.Fatalf("expected an isolated generated worktree: %#v", workspace)
	}
	contents, err := os.ReadFile(filepath.Join(workspace.Path, "partial.txt"))
	if err != nil || string(contents) != "unfinished change\n" {
		t.Fatalf("partial worktree was not preserved: contents=%q err=%v workspace=%#v", contents, err, workspace)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fallback ran after partial edits were detected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository, "partial.txt")); !os.IsNotExist(err) {
		t.Fatalf("partial edit leaked into the user checkout: %v", err)
	}
}

func TestDispatcherPreservesCleanCommittedWorkAfterNonzeroExit(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{DefaultWorkdir: store.OptionalString{Set: true, Value: &repository}}); err != nil {
		t.Fatal(err)
	}
	fixture := executableFixture(t, `
printf '%s\n' 'committed before failure' > "$AUTOGORA_WORKSPACE/committed.txt"
git -C "$AUTOGORA_WORKSPACE" add committed.txt
git -C "$AUTOGORA_WORKSPACE" -c user.name=Autogora -c user.email=autogora@localhost commit -m 'partial worker commit' >/dev/null
exit 2`)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "committing-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "preserve committed failure", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	if err := Run(ctx, Options{DBPath: dbPath, CLIPath: buildAutogora(t), Board: "default", Once: true, MaxWorkers: 1,
		AllowWrites: true, AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CODEX_BIN" {
				return fixture
			}
			return ""
		}}); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.FailureCount != 0 || len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusBlocked {
		t.Fatalf("committed failure was not preserved without retry: %#v", detail)
	}
	if detail.Task.BlockReason == nil || !strings.Contains(*detail.Task.BlockReason, "partial changes remain") || len(detail.RunWorkspaces) != 1 {
		t.Fatalf("preserved work was not explained: %#v", detail)
	}
	workspace := detail.RunWorkspaces[0]
	if output, err := exec.Command("git", "-C", workspace.Path, "status", "--porcelain").CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("fixture worktree is not clean: %q %v", output, err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace.Path, "committed.txt"))
	if err != nil || string(contents) != "committed before failure\n" {
		t.Fatalf("committed work was not preserved: contents=%q err=%v", contents, err)
	}
}

func TestDecompositionProfileOverridesCannotRelaxConfiguredPolicy(t *testing.T) {
	manager, _ := testManager(t)
	profiles := []boards.Profile{{Name: "guarded", Runtime: model.RuntimeCodex, Model: "board-model", Disabled: true, MaxConcurrent: 2}}
	if _, err := manager.Update("default", boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
		t.Fatal(err)
	}
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults:      agentconfig.Defaults{WorkerAgents: []string{"guarded"}},
		Agents: []agentconfig.Agent{{ID: "guarded", Runtime: model.RuntimeCodex, Command: "/configured/codex", Model: "global-model",
			Enabled: true, MaxConcurrent: 4, Roles: []agentconfig.Role{agentconfig.RoleWorker}}},
	}
	configured, err := configuredProfiles(manager, "default", Options{AgentConfig: &config, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeDecompositionProfiles(configured, []orchestration.ProfileRoute{{
		Name: "guarded", Runtime: model.RuntimeClaude, Model: "cli-model", Disabled: false, MaxConcurrent: 20,
	}})
	if len(merged) != 1 {
		t.Fatalf("unexpected merged profiles: %#v", merged)
	}
	profile := merged[0]
	if profile.Runtime != model.RuntimeCodex || profile.Model != "board-model" || !profile.Disabled || profile.MaxConcurrent != 2 {
		t.Fatalf("CLI decomposition override relaxed configured execution policy: %#v", profile)
	}
}

func TestWatchDispatcherHoldsSingleSupervisorLease(t *testing.T) {
	manager, dbPath := testManager(t)
	config := agentconfig.Default()
	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", MaxWorkers: 1,
			Interval: 250 * time.Millisecond, AutoDecompose: boolValue(false), AgentConfig: &config, Getenv: func(string) string { return "" }})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		opened, err := manager.OpenStore(context.Background(), "default")
		if err != nil {
			t.Fatal(err)
		}
		_, leaseErr := opened.GetServiceLease(context.Background(), dispatcherLeaseName)
		opened.Close()
		if leaseErr == nil {
			break
		}
		if !errors.Is(leaseErr, store.ErrServiceLeaseNotFound) || time.Now().After(deadline) {
			t.Fatalf("dispatcher lease did not appear: %v", leaseErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	secondErr := Run(context.Background(), Options{DBPath: dbPath, CLIPath: "/tmp/autogora", MaxWorkers: 1,
		AutoDecompose: boolValue(false), AgentConfig: &config, Getenv: func(string) string { return "" }})
	if !errors.Is(secondErr, ErrDispatcherAlreadyRunning) {
		t.Fatalf("second dispatcher error = %v, want ErrDispatcherAlreadyRunning", secondErr)
	}
	cancel()
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("first dispatcher shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first dispatcher did not stop")
	}
}
