package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type finalizerConflictFixture struct {
	manager      *boards.Manager
	dbPath       string
	repository   string
	firstTaskID  string
	secondTaskID string
	finalizerID  string
	firstHead    string
	secondHead   string
}

func finalizerGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Autogora", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Autogora", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0", "GIT_MERGE_AUTOEDIT=no",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newFinalizerConflictFixture(t *testing.T, maxRetries int) finalizerConflictFixture {
	t.Helper()
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	profileName := "integration-finalizer"
	profiles := []boards.Profile{{Name: profileName, Runtime: model.RuntimeCline, Description: "Conflict resolver"}}
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
		Orchestration: &boards.OrchestrationUpdate{
			Profiles:         &profiles,
			FinalizerProfile: store.OptionalString{Set: true, Value: &profileName},
		},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	worker := "fixture-worker"
	first, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "first branch", Assignee: &worker, Runtime: model.RuntimeCline,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "second branch", Assignee: &worker, Runtime: model.RuntimeCline,
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizer, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "integrate conflicting branches", Assignee: &profileName, Runtime: model.RuntimeCline,
		WorkflowRole: model.WorkflowRoleFinalizer, MaxRetries: maxRetries,
		Parents: []string{first.Task.ID, second.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := finalizerGit(t, repository, "rev-parse", "HEAD^{commit}")
	completeBranch := func(taskID, name, contents string) string {
		t.Helper()
		worktree := filepath.Join(t.TempDir(), name)
		finalizerGit(t, repository, "worktree", "add", "-q", "--detach", worktree, base)
		if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		finalizerGit(t, worktree, "commit", "-q", "-am", name)
		head := finalizerGit(t, worktree, "rev-parse", "HEAD^{commit}")
		claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: taskID})
		if err != nil || claim == nil {
			t.Fatalf("claim prerequisite %s: %#v, %v", taskID, claim, err)
		}
		scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
		if _, err := opened.RequestRunCompletion(ctx, scope, store.CompletionInput{Summary: name + " ready"}); err != nil {
			t.Fatal(err)
		}
		ref := "refs/autogora/runs/" + claim.Run.ID
		finalizerGit(t, repository, "update-ref", ref, head)
		if _, err := opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
			RunID: claim.Run.ID, RepositoryPath: repository, WorktreePath: worktree,
			BaseCommit: base, HeadCommit: head, DurableRef: ref, State: "ready",
			ChangedFiles: []string{"README.md"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.FinalizeRunTerminal(ctx, claim.Run.ID, 0); err != nil {
			t.Fatal(err)
		}
		return head
	}
	firstHead := completeBranch(first.Task.ID, "first", "first version\n")
	secondHead := completeBranch(second.Task.ID, "second", "second version\n")
	return finalizerConflictFixture{
		manager: manager, dbPath: dbPath, repository: repository,
		firstTaskID: first.Task.ID, secondTaskID: second.Task.ID,
		finalizerID: finalizer.Task.ID, firstHead: firstHead, secondHead: secondHead,
	}
}

func runFinalizerFixture(t *testing.T, fixture finalizerConflictFixture, cliPath, workerPath string) {
	t.Helper()
	if err := Run(context.Background(), Options{
		DBPath: fixture.dbPath, CLIPath: cliPath, Board: "default",
		TaskID: fixture.finalizerID, Once: true, AllowWrites: true,
		AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return workerPath
			}
			return ""
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizerResolvesRealGitConflictAndRetainsEveryPrerequisite(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 2)
	cliPath := buildAutogora(t)
	marker := filepath.Join(t.TempDir(), "launched")
	t.Setenv("AUTOGORA_TEST_FINALIZER_MARKER", marker)
	worker := executableFixture(t, `
test "$AUTOGORA_AGENT_PROFILE" = "integration-finalizer"
test -z "${AUTOGORA_INTEGRATION_RESOLUTION:-}"
test -r "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST"
test "$(sha256sum "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST" | cut -d ' ' -f1)" = "$AUTOGORA_INTEGRATION_RESOLUTION_SHA256"
grep -q '"mergeInProgress":true' "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST"
test -n "$(git ls-files -u)"
printf 'resolved by finalizer\n' > README.md
git add README.md
git -c user.name=Autogora -c user.email=autogora@localhost commit -q --no-edit
test -z "$(git ls-files -u)"
touch "$AUTOGORA_TEST_FINALIZER_MARKER"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "conflict resolved and validated" >/dev/null`)
	runFinalizerFixture(t, fixture, cliPath, worker)

	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusDone || detail.Task.WorkflowRole != model.WorkflowRoleFinalizer ||
		len(detail.ChangeSets) != 1 || len(detail.RunAgentConfigs) != 1 ||
		detail.RunAgentConfigs[0].Profile != "integration-finalizer" {
		logOutput := ""
		if len(detail.Runs) > 0 && detail.Runs[0].LogPath != nil {
			if contents, readErr := os.ReadFile(*detail.Runs[0].LogPath); readErr == nil {
				logOutput = string(contents)
			}
		}
		t.Fatalf("finalizer did not complete through its explicit profile: status=%s reason=%v log=%q",
			detail.Task.Status, detail.Task.BlockReason, logOutput)
	}
	head := detail.ChangeSets[0].HeadCommit
	for _, prerequisiteHead := range []string{fixture.firstHead, fixture.secondHead} {
		command := exec.Command("git", "-C", fixture.repository, "merge-base", "--is-ancestor", prerequisiteHead, head)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("finalizer head dropped prerequisite %s: %v\n%s", prerequisiteHead, err, output)
		}
	}
	contents := finalizerGit(t, fixture.repository, "show", head+":README.md")
	if contents != "resolved by finalizer" {
		t.Fatalf("resolved contents = %q", contents)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("finalizer was not launched: %v", err)
	}
	resolutionEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			resolutionEvents++
		}
	}
	if resolutionEvents != 1 {
		t.Fatalf("resolution event count = %d, events=%+v", resolutionEvents, detail.Events)
	}
}

func TestFinalizerResolutionPreservesManualHandoffsAndStopsAtAttemptLimit(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 2)
	cliPath := buildAutogora(t)
	marker := filepath.Join(t.TempDir(), "attempts")
	t.Setenv("AUTOGORA_TEST_FINALIZER_ATTEMPTS", marker)
	worker := executableFixture(t, `
test -r "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST"
test "$(sha256sum "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST" | cut -d ' ' -f1)" = "$AUTOGORA_INTEGRATION_RESOLUTION_SHA256"
test -n "$(git ls-files -u)"
printf '%s\n' "$AUTOGORA_RUN_ID" >> "$AUTOGORA_TEST_FINALIZER_ATTEMPTS"
"$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "resolution needs manual review: $AUTOGORA_RUN_ID" --kind needs_input >/dev/null`)

	var preserved []string
	for attempt := 1; attempt <= 2; attempt++ {
		runFinalizerFixture(t, fixture, cliPath, worker)
		opened, err := fixture.manager.OpenStore(context.Background(), "default")
		if err != nil {
			t.Fatal(err)
		}
		detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
		if err != nil {
			opened.Close()
			t.Fatal(err)
		}
		if detail.Task.Status != model.TaskStatusBlocked || len(detail.RunWorkspaces) != attempt {
			opened.Close()
			t.Fatalf("attempt %d was not preserved as a manual block: %#v", attempt, detail)
		}
		path := detail.RunWorkspaces[len(detail.RunWorkspaces)-1].Path
		if unmerged, err := exec.Command("git", "-C", path, "ls-files", "-u").Output(); err != nil || len(unmerged) == 0 {
			opened.Close()
			t.Fatalf("attempt %d lost its conflict handoff: %q, %v", attempt, unmerged, err)
		}
		preserved = append(preserved, path)
		if attempt == 1 {
			if err := os.WriteFile(filepath.Join(fixture.repository, "unrelated.txt"), []byte("advance base\n"), 0o644); err != nil {
				opened.Close()
				t.Fatal(err)
			}
			finalizerGit(t, fixture.repository, "add", "unrelated.txt")
			finalizerGit(t, fixture.repository, "commit", "-q", "-m", "advance unrelated base")
		}
		if _, err := opened.UnblockTask(context.Background(), fixture.finalizerID); err != nil {
			opened.Close()
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}

	runFinalizerFixture(t, fixture, cliPath, worker)
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.BlockReason == nil ||
		!strings.Contains(*detail.Task.BlockReason, "exhausted its 2 attempt") ||
		len(detail.Runs) != 3 || len(detail.RunWorkspaces) != 3 || len(detail.ChangeSets) != 0 {
		t.Fatalf("exhausted finalizer lifecycle = %#v", detail)
	}
	lines, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if attempts := strings.Fields(string(lines)); len(attempts) != 2 {
		t.Fatalf("worker launch count = %d, want 2: %q", len(attempts), lines)
	}
	for _, path := range preserved {
		if unmerged, err := exec.Command("git", "-C", path, "ls-files", "-u").Output(); err != nil || len(unmerged) == 0 {
			t.Fatalf("preserved attempt was modified after exhaustion: %s, %q, %v", path, unmerged, err)
		}
	}
	exhaustedWorkspace := detail.RunWorkspaces[len(detail.RunWorkspaces)-1].Path
	if unmerged, err := exec.Command("git", "-C", exhaustedWorkspace, "ls-files", "-u").Output(); err != nil || len(unmerged) != 0 {
		t.Fatalf("exhausted pre-launch workspace was not rolled back: %q, %v", unmerged, err)
	}
	resolutionEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			resolutionEvents++
		}
	}
	if resolutionEvents != 2 {
		t.Fatalf("resolution event count = %d, want 2", resolutionEvents)
	}
	incidents, err := opened.ListCoordinationIncidents(context.Background(), store.CoordinationIncidentFilter{
		TaskID: fixture.finalizerID, Trigger: model.CoordinationTriggerIntegrationConflict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || !strings.Contains(incidents[0].Summary, "exhausted") {
		t.Fatalf("exhaustion coordination incident = %+v", incidents)
	}

	// A future manual unblock cannot bypass the durable attempt ledger.
	if _, err := opened.UnblockTask(context.Background(), fixture.finalizerID); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	runFinalizerFixture(t, fixture, cliPath, worker)
	reopened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Runs) != 4 || again.Task.Status != model.TaskStatusTriage ||
		again.Task.BlockRecurrences < 2 {
		t.Fatalf("repeated exhaustion did not escalate to triage: status=%s recurrences=%d runs=%d",
			again.Task.Status, again.Task.BlockRecurrences, len(again.Runs))
	}
	lines, err = os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if attempts := strings.Fields(string(lines)); len(attempts) != 2 {
		t.Fatalf("exhaustion relaunched worker: %q", lines)
	}
}

func TestFinalizerSpawnFailureDoesNotConsumeConflictAttempt(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 1)
	cliPath := buildAutogora(t)
	missingWorker := filepath.Join(t.TempDir(), "missing-cline")
	runFinalizerFixture(t, fixture, cliPath, missingWorker)

	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	startedEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			startedEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusBlocked || startedEvents != 0 ||
		len(detail.RunWorkspaces) != 1 {
		opened.Close()
		t.Fatalf("spawn failure consumed or requeued the conflict: %#v", detail)
	}
	if unmerged, err := exec.Command("git", "-C", detail.RunWorkspaces[0].Path, "ls-files", "-u").Output(); err != nil || len(unmerged) == 0 {
		opened.Close()
		t.Fatalf("spawn failure did not preserve conflict: %q, %v", unmerged, err)
	}
	if _, err := opened.UnblockTask(context.Background(), fixture.finalizerID); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := opened.SetAgentHealth(context.Background(), store.SetAgentHealthInput{
		AgentID: "integration-finalizer", Status: model.AgentHealthReady,
	}); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("AUTOGORA_TEST_FINALIZER_STARTED", marker)
	worker := executableFixture(t, `
test -r "$AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST"
touch "$AUTOGORA_TEST_FINALIZER_STARTED"
"$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "manual resolution required" --kind needs_input >/dev/null`)
	runFinalizerFixture(t, fixture, cliPath, worker)
	reopened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	detail, err = reopened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	startedEvents = 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			startedEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusBlocked || startedEvents != 1 {
		t.Fatalf("refunded attempt was not available to the next process: %#v", detail)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("replacement finalizer was not launched: %v", err)
	}
}

func TestFinalizerCancellationBeforeSpawnDoesNotConsumeConflictAttempt(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 1)
	cliPath := buildAutogora(t)
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("AUTOGORA_TEST_FINALIZER_CANCELED", marker)
	worker := executableFixture(t, `touch "$AUTOGORA_TEST_FINALIZER_CANCELED"`)
	ctx, cancel := context.WithCancel(context.Background())
	err := Run(ctx, Options{
		DBPath: fixture.dbPath, CLIPath: cliPath, Board: "default",
		TaskID: fixture.finalizerID, Once: true, AllowWrites: true,
		AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				cancel()
				return worker
			}
			return ""
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	startedEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			startedEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusBlocked || startedEvents != 0 {
		t.Fatalf("canceled pre-spawn finalizer consumed or requeued the conflict: %#v", detail)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled finalizer process started: %v", err)
	}
}

func TestFinalizerStartGateRejectsConflictAbortedAfterCommandConstruction(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 1)
	cliPath := buildAutogora(t)
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("AUTOGORA_TEST_FINALIZER_STALE_GATE", marker)
	worker := executableFixture(t, `touch "$AUTOGORA_TEST_FINALIZER_STALE_GATE"`)
	mutated := false
	err := Run(context.Background(), Options{
		DBPath: fixture.dbPath, CLIPath: cliPath, Board: "default",
		TaskID: fixture.finalizerID, Once: true, AllowWrites: true,
		AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name != "AUTOGORA_CLINE_BIN" {
				return ""
			}
			if !mutated {
				root, rootErr := fixture.manager.WorkspaceRoot("default")
				if rootErr != nil {
					t.Fatal(rootErr)
				}
				runDirectories, globErr := filepath.Glob(filepath.Join(root, fixture.finalizerID, "*"))
				if globErr != nil {
					t.Fatal(globErr)
				}
				for _, directory := range runDirectories {
					command := exec.Command("git", "-C", directory, "ls-files", "-u")
					if output, outputErr := command.Output(); outputErr == nil && len(output) > 0 {
						finalizerGit(t, directory, "merge", "--abort")
						mutated = true
						break
					}
				}
			}
			return worker
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mutated {
		t.Fatal("test did not mutate the prepared conflict")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale finalizer process started: %v", err)
	}
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	startedEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			startedEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusBlocked || startedEvents != 0 ||
		detail.Task.BlockReason == nil ||
		!strings.Contains(*detail.Task.BlockReason, "revalidate live integration conflict") {
		t.Fatalf("stale start gate was not preserved without charging an attempt: %#v", detail)
	}
}

func TestFinalizerGoalContinuationDoesNotChargeResolutionTwice(t *testing.T) {
	fixture := newFinalizerConflictFixture(t, 2)
	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	goalMode, goalTurns := true, 3
	if _, err := opened.UpdateTask(context.Background(), fixture.finalizerID, store.UpdateTaskInput{
		GoalMode: &goalMode, GoalMaxTurns: &goalTurns,
	}); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	cliPath := buildAutogora(t)
	turnMarker := filepath.Join(t.TempDir(), "turn")
	t.Setenv("AUTOGORA_TEST_FINALIZER_GOAL_TURN", turnMarker)
	worker := executableFixture(t, `
if [ ! -e "$AUTOGORA_TEST_FINALIZER_GOAL_TURN" ]; then
  touch "$AUTOGORA_TEST_FINALIZER_GOAL_TURN"
  exit 0
fi
printf 'resolved in continuation\n' > README.md
git add README.md
git -c user.name=Autogora -c user.email=autogora@localhost commit -q --no-edit`)
	judgments := 0
	if err := Run(context.Background(), Options{
		DBPath: fixture.dbPath, CLIPath: cliPath, Board: "default",
		TaskID: fixture.finalizerID, Once: true, AllowWrites: true,
		AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return worker
			}
			return ""
		},
		GoalJudge: func(_ context.Context, _ model.TaskDetail, _ int, _ string) (orchestration.GoalJudgment, error) {
			judgments++
			if judgments == 1 {
				return orchestration.GoalJudgment{
					Complete: false, Reason: "resolve the conflict", NextPrompt: "finish resolution",
				}, nil
			}
			return orchestration.GoalJudgment{Complete: true, Reason: "conflict resolved"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err = fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(context.Background(), fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	startedEvents, spawnEvents := 0, 0
	for _, event := range detail.Events {
		switch event.Kind {
		case "integration_resolution_started":
			startedEvents++
		case "spawned":
			spawnEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusDone || judgments != 2 ||
		startedEvents != 1 || spawnEvents != 2 {
		t.Fatalf("goal continuation charged or gated the attempt again: status=%s judgments=%d resolution=%d spawned=%d",
			detail.Task.Status, judgments, startedEvents, spawnEvents)
	}
}

func TestPreparedFinalizerConflictIsRecoveredAfterClaimTTL(t *testing.T) {
	ctx := context.Background()
	fixture := newFinalizerConflictFixture(t, 1)
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: fixture.finalizerID, ClaimTTLSeconds: 60,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.New(fixture.manager)
	workspaces.SetAllowWrites(true)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.IntegratePrerequisiteChangeSets(ctx, opened, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.IntegrationResolution == nil || prepared.Workspace == nil {
		t.Fatalf("conflict handoff was not prepared: %+v", prepared)
	}
	raw, err := sql.Open("sqlite", fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := raw.ExecContext(ctx,
		"UPDATE task_runs SET claim_expires_at = ? WHERE id = ?", expired, claim.Run.ID,
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	options := Options{}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, fixture.finalizerID)
	if err != nil {
		t.Fatal(err)
	}
	startedEvents := 0
	for _, event := range detail.Events {
		if event.Kind == "integration_resolution_started" {
			startedEvents++
		}
	}
	if detail.Task.Status != model.TaskStatusBlocked || startedEvents != 0 ||
		len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusReclaimed ||
		detail.Task.BlockReason == nil || !strings.Contains(*detail.Task.BlockReason, prepared.Workspace.Path) {
		t.Fatalf("expired prepared conflict was not durably preserved: %#v", detail)
	}
	if unmerged, err := exec.Command("git", "-C", prepared.Workspace.Path, "ls-files", "-u").Output(); err != nil || len(unmerged) == 0 {
		t.Fatalf("TTL recovery lost prepared conflict: %q, %v", unmerged, err)
	}
}

func TestIntegrationResolutionReservationRejectsNonFinalizerRole(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "ordinary worker", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v, %v", claim, err)
	}
	path := filepath.Join(t.TempDir(), claim.Run.ID)
	if _, err := opened.BindRunWorkspace(ctx, store.RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}, store.BindRunWorkspaceInput{
		Path: path, Kind: model.WorkspaceWorktree, Generated: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = opened.ReserveIntegrationResolution(ctx, store.RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}, store.ReserveIntegrationResolutionInput{
		WorkspacePath: path, PrerequisiteID: "parent", ChangeSetID: "change",
		ConflictFingerprint: strings.Repeat("a", 64),
	})
	if err == nil || errors.Is(err, store.ErrIntegrationResolutionExhausted) ||
		!strings.Contains(err.Error(), "requires a finalizer") {
		t.Fatalf("ordinary worker reservation error = %v", err)
	}
}
