package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/coordinator"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func coordinatorRecoverySnapshot(request orchestration.PlannerRequest) (coordinator.IncidentSnapshot, error) {
	const marker = "Incident snapshot:\n\n"
	if request.Kind != orchestration.PlannerCoordinator {
		return coordinator.IncidentSnapshot{}, fmt.Errorf(
			"planner kind = %s, want %s",
			request.Kind,
			orchestration.PlannerCoordinator,
		)
	}
	offset := strings.LastIndex(request.Prompt, marker)
	if offset < 0 {
		return coordinator.IncidentSnapshot{}, errors.New("Coordinator prompt has no incident snapshot")
	}
	var snapshot coordinator.IncidentSnapshot
	if err := json.Unmarshal(
		[]byte(request.Prompt[offset+len(marker):]),
		&snapshot,
	); err != nil {
		return coordinator.IncidentSnapshot{}, fmt.Errorf("decode Coordinator snapshot: %w", err)
	}
	return snapshot, nil
}

func coordinatorRecoveryNode(
	snapshot coordinator.IncidentSnapshot,
	taskID string,
) (coordinator.NodeSnapshot, bool) {
	for _, node := range snapshot.Nodes {
		if node.ID == taskID {
			return node, true
		}
	}
	return coordinator.NodeSnapshot{}, false
}

func coordinatorRecoveryAgent(
	snapshot coordinator.IncidentSnapshot,
	agentID string,
) (coordinator.AgentSnapshot, bool) {
	for _, agent := range snapshot.AvailableAgents {
		if agent.ID == agentID {
			return agent, true
		}
	}
	return coordinator.AgentSnapshot{}, false
}

func requireCoordinatorRecoveryWorktreesClean(
	t *testing.T,
	detail model.TaskDetail,
	runCount int,
) {
	t.Helper()
	workspaces := make(map[string]model.RunWorkspace, len(detail.RunWorkspaces))
	for _, value := range detail.RunWorkspaces {
		workspaces[value.RunID] = value
	}
	if len(detail.Runs) < runCount {
		t.Fatalf("task has %d runs, want at least %d", len(detail.Runs), runCount)
	}
	for _, run := range detail.Runs[:runCount] {
		if run.Status != model.RunStatusBlocked {
			t.Fatalf("run %s status = %s, want blocked", run.ID, run.Status)
		}
		prepared, found := workspaces[run.ID]
		if !found || prepared.Kind != model.WorkspaceWorktree {
			t.Fatalf("blocked run %s workspace = %+v, found=%t", run.ID, prepared, found)
		}
		command := exec.Command(
			"git", "-C", prepared.Path,
			"status", "--porcelain=v1", "--untracked-files=all",
		)
		output, err := command.CombinedOutput()
		if err != nil || len(output) != 0 {
			t.Fatalf(
				"blocked run %s worktree is not clean: output=%q err=%v",
				run.ID,
				output,
				err,
			)
		}
	}
}

func TestLiveCoordinatorRecoversRepeatedCleanWorktreeBlockAndPublishesGraph(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	finalizerGit(t, repository, "branch", "-M", "main")

	enabled, writes, approval := true, true, false
	coordinationMode := boards.CoordinationModeAuto
	publicationMode := boards.PublicationModeLocalFF
	targetBranch, remote := "main", "origin"
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
		Orchestration: &boards.OrchestrationUpdate{
			Autopilot: &boards.AutopilotUpdate{
				Enabled:         &enabled,
				AutoExecute:     &enabled,
				WorkspaceWrites: &writes,
				Coordination: &boards.CoordinationUpdate{
					Mode: &coordinationMode,
				},
				Publication: &boards.PublicationUpdate{
					Mode: &publicationMode, TargetBranch: &targetBranch,
					Remote: &remote, RequireApproval: &approval,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	workerID := "recovery-worker"
	recovery, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "recover the clean capability block", Assignee: &workerID,
		Runtime: model.RuntimeCline, MaxRetries: 3,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	downstream, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "continue after Coordinator recovery", Assignee: &workerID,
		Runtime: model.RuntimeCline, Parents: []string{recovery.Task.ID},
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	finalizer, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "finalize and publish recovered graph", Assignee: &workerID,
		Runtime: model.RuntimeCline, WorkflowRole: model.WorkflowRoleFinalizer,
		Parents: []string{downstream.Task.ID},
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	attemptFile := filepath.Join(t.TempDir(), "recovery-attempt")
	t.Setenv("AUTOGORA_TEST_RECOVERY_ATTEMPT", attemptFile)
	worker := executableFixture(t, fmt.Sprintf(`
case "$AUTOGORA_TASK_ID" in
  %s)
    test -z "$(git -C "$AUTOGORA_WORKSPACE" status --porcelain=v1 --untracked-files=all)"
    attempt=0
    if test -r "$AUTOGORA_TEST_RECOVERY_ATTEMPT"; then
      attempt="$(cat "$AUTOGORA_TEST_RECOVERY_ATTEMPT")"
    fi
    attempt=$((attempt + 1))
    printf '%%s\n' "$attempt" > "$AUTOGORA_TEST_RECOVERY_ATTEMPT"
    if test "$attempt" -le 2; then
      "$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "required compiler is unavailable" --kind capability >/dev/null
      test -z "$(git -C "$AUTOGORA_WORKSPACE" status --porcelain=v1 --untracked-files=all)"
    else
      "$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "capability recovery succeeded" >/dev/null
    fi
    ;;
  %s)
    printf 'recovered graph continued\n' > "$AUTOGORA_WORKSPACE/game.txt"
    "$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "downstream implementation completed" >/dev/null
    ;;
  %s)
    test "$(cat "$AUTOGORA_WORKSPACE/game.txt")" = "recovered graph continued"
    printf 'finalized after Coordinator recovery\n' > "$AUTOGORA_WORKSPACE/release.txt"
    "$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "recovered graph finalized" >/dev/null
    ;;
  *)
    printf 'unexpected task %%s\n' "$AUTOGORA_TASK_ID" >&2
    exit 2
    ;;
esac`, recovery.Task.ID, downstream.Task.ID, finalizer.Task.ID))
	config := agentconfig.Normalize(agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults: agentconfig.Defaults{
			WorkerAgents:      []string{workerID},
			CoordinatorAgents: []string{"recovery-coordinator"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: workerID, Runtime: model.RuntimeCline, Command: worker,
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker},
			},
			{
				ID: "recovery-coordinator", Runtime: model.RuntimeCodex,
				Command: "/bin/true", Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleCoordinator},
			},
		},
	})
	cliPath := buildAutogora(t)
	baseTime := time.Date(2026, time.July, 24, 1, 2, 3, 0, time.UTC)
	var clockTick atomic.Int64
	currentTime := func() time.Time {
		return baseTime.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
	}
	var analyzerCalls atomic.Int32
	var analyzedSnapshot coordinator.IncidentSnapshot
	coordinatorPlanner := func(
		_ context.Context,
		request orchestration.PlannerRequest,
	) (any, error) {
		analyzerCalls.Add(1)
		snapshot, err := coordinatorRecoverySnapshot(request)
		if err != nil {
			return nil, err
		}
		if snapshot.Trigger != string(model.CoordinationTriggerRepeatedBlock) ||
			snapshot.FocusTaskID != recovery.Task.ID {
			return nil, fmt.Errorf("unexpected incident snapshot: %+v", snapshot)
		}
		node, found := coordinatorRecoveryNode(snapshot, recovery.Task.ID)
		if !found ||
			node.Status != model.TaskStatusTriage ||
			node.BlockKind == nil ||
			*node.BlockKind != model.BlockKindCapability ||
			node.BlockReason == nil ||
			*node.BlockReason != "required compiler is unavailable" ||
			node.BlockRecurrences != 2 ||
			node.PreservedWork ||
			node.WorkspaceDirty {
			return nil, fmt.Errorf("recovery node is not a clean repeated block: %+v", node)
		}
		agent, found := coordinatorRecoveryAgent(snapshot, workerID)
		if !found ||
			!agent.Enabled ||
			agent.Health != string(model.AgentHealthReady) ||
			agent.ActiveSlots != 0 ||
			agent.MaxConcurrent != 1 {
			return nil, fmt.Errorf("worker is not safely retryable: %+v", agent)
		}
		analyzedSnapshot = snapshot
		return coordinator.Proposal{
			IncidentID: snapshot.IncidentID, ExpectedGraphRevision: snapshot.GraphRevision,
			Summary:   "Retry the clean capability block",
			Rationale: "The same clean task can retry with its healthy assigned worker.",
			Actions: []coordinator.Action{{
				Kind: coordinator.ActionUnblockTask, TaskID: recovery.Task.ID,
				ExpectedUpdatedAt: node.UpdatedAt,
				Reason:            "Retry after confirming a clean workspace and healthy worker.",
			}},
		}, nil
	}
	runOnce := func(taskID string) {
		t.Helper()
		runCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := Run(runCtx, Options{
			DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: taskID,
			Once: true, Interval: 250 * time.Millisecond, MaxWorkers: 1,
			Autopilot: true, AllowWrites: true, AutoDecompose: boolValue(false),
			PlannerTimeout: 5 * time.Second, PublicationTimeout: 5 * time.Second,
			AgentConfig: &config, CoordinatorPlanner: coordinatorPlanner,
			Now: currentTime, Getenv: func(string) string { return "" },
		}); err != nil {
			t.Fatal(err)
		}
	}

	runOnce(recovery.Task.ID)
	opened, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	firstBlocked, err := opened.GetTask(ctx, recovery.Task.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if firstBlocked.Task.Status != model.TaskStatusBlocked ||
		firstBlocked.Task.BlockRecurrences != 1 {
		opened.Close()
		t.Fatalf("first live block = %+v", firstBlocked.Task)
	}
	requireCoordinatorRecoveryWorktreesClean(t, firstBlocked, 1)
	if _, err := opened.UnblockTask(ctx, recovery.Task.ID); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	runOnce(recovery.Task.ID)
	opened, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	secondBlocked, err := opened.GetTask(ctx, recovery.Task.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if secondBlocked.Task.Status != model.TaskStatusTriage ||
		secondBlocked.Task.BlockRecurrences != 2 ||
		secondBlocked.Task.BlockKind == nil ||
		*secondBlocked.Task.BlockKind != model.BlockKindCapability ||
		secondBlocked.Task.BlockReason == nil ||
		*secondBlocked.Task.BlockReason != "required compiler is unavailable" {
		opened.Close()
		t.Fatalf("second live block = %+v", secondBlocked.Task)
	}
	requireCoordinatorRecoveryWorktreesClean(t, secondBlocked, 2)
	beforeObservation, err := opened.ListCoordinationIncidents(
		ctx,
		store.CoordinationIncidentFilter{
			TaskID:  recovery.Task.ID,
			Trigger: model.CoordinationTriggerRepeatedBlock,
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if len(beforeObservation) != 0 {
		opened.Close()
		t.Fatalf("incident was inserted outside the live observer: %+v", beforeObservation)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	// No task is runnable until the production coordination queue observes the
	// repeated live block. The same one-shot pass applies the validated CAS
	// proposal, loops, and launches the recovered task's third run.
	runOnce("")
	opened, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := opened.GetTask(ctx, recovery.Task.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	incidents, err := opened.ListCoordinationIncidents(
		ctx,
		store.CoordinationIncidentFilter{
			TaskID:  recovery.Task.ID,
			Trigger: model.CoordinationTriggerRepeatedBlock,
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if len(incidents) != 1 ||
		incidents[0].Status != model.CoordinationIncidentResolved ||
		incidents[0].ID != analyzedSnapshot.IncidentID {
		opened.Close()
		t.Fatalf("resolved live incident = %+v", incidents)
	}
	proposals, err := opened.ListCoordinationProposals(
		ctx,
		store.CoordinationProposalFilter{IncidentID: incidents[0].ID},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: incidents[0].ID},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if analyzerCalls.Load() != 1 ||
		len(proposals) != 1 ||
		proposals[0].Status != model.CoordinationProposalApplied ||
		proposals[0].AppliedAt == nil ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptSucceeded ||
		attempts[0].EndedAt == nil {
		opened.Close()
		t.Fatalf(
			"coordination audit: calls=%d proposals=%+v attempts=%+v",
			analyzerCalls.Load(),
			proposals,
			attempts,
		)
	}
	var actions []coordinator.Action
	if err := json.Unmarshal(proposals[0].Actions, &actions); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	analyzedNode, found := coordinatorRecoveryNode(analyzedSnapshot, recovery.Task.ID)
	if !found ||
		len(actions) != 1 ||
		actions[0].Kind != coordinator.ActionUnblockTask ||
		actions[0].TaskID != recovery.Task.ID ||
		actions[0].ExpectedUpdatedAt != analyzedNode.UpdatedAt {
		opened.Close()
		t.Fatalf("applied CAS actions = %+v snapshotNode=%+v", actions, analyzedNode)
	}
	if recovered.Task.Status != model.TaskStatusDone ||
		recovered.Task.BlockKind != nil ||
		recovered.Task.BlockReason != nil ||
		recovered.Task.BlockRecurrences != 0 ||
		len(recovered.Runs) != 3 ||
		recovered.Runs[2].Status != model.RunStatusCompleted ||
		recovered.Runs[2].Summary == nil ||
		*recovered.Runs[2].Summary != "capability recovery succeeded" {
		opened.Close()
		t.Fatalf("recovered task = %+v", recovered)
	}
	continued, err := opened.GetTask(ctx, downstream.Task.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if continued.Task.Status != model.TaskStatusReady {
		opened.Close()
		t.Fatalf("downstream status after recovery = %s", continued.Task.Status)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	runOnce("")
	runOnce("")
	// Exercise another idle coordination pass after publication. It must reuse
	// the resolved audit state instead of making a duplicate paid analysis call.
	runOnce("")

	opened, err = manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	downstreamResult, err := opened.GetTask(ctx, downstream.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalizerResult, err := opened.GetTask(ctx, finalizer.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	publications, err := opened.ListPublications(
		ctx,
		store.PublicationFilter{TaskID: finalizer.Task.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err = opened.ListCoordinationAttempts(
		ctx,
		store.CoordinationAttemptFilter{IncidentID: incidents[0].ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if analyzerCalls.Load() != 1 || len(attempts) != 1 {
		t.Fatalf(
			"duplicate Coordinator analysis: calls=%d attempts=%+v",
			analyzerCalls.Load(),
			attempts,
		)
	}
	if downstreamResult.Task.Status != model.TaskStatusDone ||
		len(downstreamResult.ChangeSets) != 1 ||
		downstreamResult.ChangeSets[0].State != "ready" {
		t.Fatalf("downstream result = %+v", downstreamResult)
	}
	if finalizerResult.Task.Status != model.TaskStatusDone ||
		finalizerResult.Task.WorkflowRole != model.WorkflowRoleFinalizer ||
		len(finalizerResult.ChangeSets) != 1 ||
		finalizerResult.ChangeSets[0].State != "ready" {
		t.Fatalf("finalizer result = %+v", finalizerResult)
	}
	if len(publications) != 1 ||
		publications[0].Status != model.PublicationPublished ||
		publications[0].HeadCommit != finalizerResult.ChangeSets[0].HeadCommit {
		t.Fatalf("publication result = %+v", publications)
	}
	head := finalizerGit(t, repository, "rev-parse", "main^{commit}")
	if head != finalizerResult.ChangeSets[0].HeadCommit {
		t.Fatalf(
			"published main = %s, want finalizer %s",
			head,
			finalizerResult.ChangeSets[0].HeadCommit,
		)
	}
	game, gameErr := os.ReadFile(filepath.Join(repository, "game.txt"))
	release, releaseErr := os.ReadFile(filepath.Join(repository, "release.txt"))
	if gameErr != nil ||
		releaseErr != nil ||
		string(game) != "recovered graph continued\n" ||
		string(release) != "finalized after Coordinator recovery\n" {
		t.Fatalf(
			"published files: game=%q err=%v release=%q err=%v",
			game,
			gameErr,
			release,
			releaseErr,
		)
	}
}
