package dispatcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type boundaryPendingCheckpoint struct {
	manager    *boards.Manager
	dbPath     string
	repository string
	taskID     string
	checkpoint model.RecoveryCheckpoint
}

func createBoundaryPendingCheckpoint(t *testing.T) boundaryPendingCheckpoint {
	t.Helper()

	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "boundary-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "boundary recovery fixture", Assignee: &assignee,
		Runtime: model.RuntimeCline,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim fixture task: %+v, %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.New(manager)
	workspaces.SetAllowWrites(true)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree {
		t.Fatalf("expected generated worktree: %#v", prepared.Workspace)
	}
	if err := os.WriteFile(
		filepath.Join(prepared.Workspace.Path, "partial.txt"),
		[]byte("confirmed partial\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	countFailure := false
	checkpointed, err := checkpointManagedRunFailure(
		ctx,
		opened,
		workspaces,
		prepared,
		scope,
		"fixture worker stopped with partial work",
		store.FailRunOptions{
			Outcome: model.RunStatusRateLimited, CountFailure: &countFailure,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointed {
		t.Fatal("fixture partial work was not checkpointed")
	}
	checkpoint, err := opened.GetActiveRecoveryCheckpoint(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil ||
		checkpoint.State != model.RecoveryCheckpointPending {
		t.Fatalf("fixture checkpoint is not pending: %#v", checkpoint)
	}
	return boundaryPendingCheckpoint{
		manager:    manager,
		dbPath:     dbPath,
		repository: repository,
		taskID:     task.Task.ID,
		checkpoint: *checkpoint,
	}
}

func TestDispatcherBlockedPartialWorkResumesAfterUnblock(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "block-resume-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "resume explicitly blocked partial work", Assignee: &assignee,
		Runtime: model.RuntimeCline,
	})
	if closeErr := opened.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}

	worker := executableFixture(t, `
if [ ! -f "$AUTOGORA_WORKSPACE/partial.txt" ]; then
  printf '%s\n' 'phase one' > "$AUTOGORA_WORKSPACE/partial.txt"
  "$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "temporary external approval" --kind transient >/dev/null
  exit 0
fi
test "$(cat "$AUTOGORA_WORKSPACE/partial.txt")" = "phase one"
printf '%s\n' 'phase two' >> "$AUTOGORA_WORKSPACE/partial.txt"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "resumed blocked checkpoint" >/dev/null`)
	options := Options{
		DBPath: dbPath, CLIPath: buildAutogora(t), Board: "default",
		Once: true, MaxWorkers: 1, AllowWrites: true,
		AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return worker
			}
			return ""
		},
	}
	if err := Run(ctx, options); err != nil {
		t.Fatal(err)
	}
	firstCheck, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstCheck.GetTask(ctx, task.Task.ID)
	if err != nil {
		firstCheck.Close()
		t.Fatal(err)
	}
	pending, err := firstCheck.GetActiveRecoveryCheckpoint(ctx, task.Task.ID)
	if err != nil {
		firstCheck.Close()
		t.Fatal(err)
	}
	if first.Task.Status != model.TaskStatusBlocked ||
		first.Task.BlockKind == nil ||
		*first.Task.BlockKind != model.BlockKindTransient ||
		first.Task.BlockReason == nil ||
		*first.Task.BlockReason != "temporary external approval" ||
		len(first.Runs) != 1 ||
		first.Runs[0].Status != model.RunStatusBlocked ||
		len(first.RunWorkspaces) != 1 ||
		pending == nil ||
		pending.State != model.RecoveryCheckpointPending ||
		pending.SourceRunID != first.Runs[0].ID ||
		len(pending.ChangedFiles) != 1 ||
		pending.ChangedFiles[0] != "partial.txt" {
		firstCheck.Close()
		t.Fatalf("first block did not preserve a retryable checkpoint: detail=%#v checkpoint=%#v", first, pending)
	}
	if len(first.TerminalRequests) != 1 ||
		first.TerminalRequests[0].FinalizedAt == nil {
		firstCheck.Close()
		t.Fatalf("block terminal request was not finalized: %#v", first.TerminalRequests)
	}
	if _, err := firstCheck.UnblockTask(ctx, task.Task.ID); err != nil {
		firstCheck.Close()
		t.Fatal(err)
	}
	if err := firstCheck.Close(); err != nil {
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
	if detail.Task.Status != model.TaskStatusDone ||
		len(detail.Runs) != 2 ||
		detail.Runs[1].Status != model.RunStatusCompleted ||
		len(detail.RunWorkspaces) != 2 ||
		detail.RunWorkspaces[0].Path == detail.RunWorkspaces[1].Path ||
		len(detail.ChangeSets) != 1 {
		t.Fatalf("unblocked checkpoint was not completed in a distinct run: %#v", detail)
	}
	recoveredBody, err := os.ReadFile(
		filepath.Join(detail.RunWorkspaces[1].Path, "partial.txt"),
	)
	if err != nil || string(recoveredBody) != "phase one\nphase two\n" {
		t.Fatalf("recovered worktree content = %q, err=%v", recoveredBody, err)
	}
	sourceBody, err := os.ReadFile(
		filepath.Join(detail.RunWorkspaces[0].Path, "partial.txt"),
	)
	if err != nil || string(sourceBody) != "phase one\n" {
		t.Fatalf("source checkpoint worktree changed: %q, err=%v", sourceBody, err)
	}
	checkpoints, err := check.ListRecoveryCheckpoints(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 ||
		checkpoints[0].ID != pending.ID ||
		checkpoints[0].State != model.RecoveryCheckpointConsumed ||
		checkpoints[0].ReservedRunID == nil ||
		*checkpoints[0].ReservedRunID != detail.Runs[1].ID ||
		checkpoints[0].ConsumedAt == nil {
		t.Fatalf("completed checkpoint lifecycle = %#v", checkpoints)
	}
	changeBody, err := exec.Command(
		"git", "-C", repository, "show",
		detail.ChangeSets[0].HeadCommit+":partial.txt",
	).Output()
	if err != nil || string(changeBody) != "phase one\nphase two\n" {
		t.Fatalf("completed change set content = %q, err=%v", changeBody, err)
	}
}

func TestDispatcherReadOnlyRunBlocksWithoutAdoptingPendingCheckpoint(t *testing.T) {
	ctx := context.Background()
	fixture := createBoundaryPendingCheckpoint(t)
	marker := filepath.Join(t.TempDir(), "worker-started")
	t.Setenv("AUTOGORA_BOUNDARY_MARKER", marker)
	worker := executableFixture(t, `
touch "$AUTOGORA_BOUNDARY_MARKER"
exit 99`)

	if err := Run(ctx, Options{
		DBPath: fixture.dbPath, CLIPath: filepath.Join(t.TempDir(), "unused-autogora"),
		Board: "default", TaskID: fixture.taskID, Once: true, MaxWorkers: 1,
		AllowWrites: false, AutoDecompose: boolValue(false),
		Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return worker
			}
			return ""
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only worker unexpectedly started: %v", err)
	}

	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.GetTask(ctx, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindCapability ||
		detail.Task.BlockReason == nil ||
		*detail.Task.BlockReason != "Recovery checkpoint contains partial workspace changes; enable workspace writes before resuming this task" ||
		len(detail.Runs) != 2 ||
		detail.Runs[1].Status != model.RunStatusBlocked {
		t.Fatalf("read-only recovery was not capability-blocked: %#v", detail)
	}
	active, err := opened.GetActiveRecoveryCheckpoint(ctx, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil ||
		active.ID != fixture.checkpoint.ID ||
		active.State != model.RecoveryCheckpointPending ||
		active.ReservedRunID != nil ||
		active.ReservationToken != "" ||
		active.AdoptedAt != nil {
		t.Fatalf("read-only run mutated the pending checkpoint: %#v", active)
	}
	if len(detail.TerminalRequests) != 1 ||
		detail.TerminalRequests[0].RunID != detail.Runs[1].ID ||
		detail.TerminalRequests[0].FinalizedAt == nil {
		t.Fatalf("read-only capability block was not finalized: %#v", detail.TerminalRequests)
	}
}

func TestSupervisorPreservesPendingNoChangeBlockMetadata(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "stopped-block-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "preserve stopped block metadata", Assignee: &assignee,
		Runtime: model.RuntimeCline,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.New(manager)
	workspaces.SetAllowWrites(true)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workspace == nil ||
		prepared.Workspace.Kind != model.WorkspaceWorktree {
		t.Fatalf("expected clean worktree: %#v", prepared.Workspace)
	}
	if _, err := opened.RecordSpawn(
		ctx,
		scope,
		99999999,
		filepath.Join(t.TempDir(), "stopped-worker.log"),
	); err != nil {
		t.Fatal(err)
	}
	const reason = "generated API client requires an unavailable schema"
	if _, err := opened.BlockRun(ctx, scope, store.BlockInput{
		Reason: reason, Kind: model.BlockKindCapability,
	}); err != nil {
		t.Fatal(err)
	}

	zero := time.Duration(0)
	options := Options{CrashGrace: &zero}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	acknowledgeCurrentRunRecoveryFence(t, ctx, opened, scope)
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindCapability ||
		detail.Task.BlockReason == nil ||
		*detail.Task.BlockReason != reason ||
		len(detail.Runs) != 1 ||
		detail.Runs[0].Status != model.RunStatusBlocked {
		t.Fatalf("Supervisor changed the worker's block metadata: %#v", detail)
	}
	if len(detail.TerminalRequests) != 1 ||
		detail.TerminalRequests[0].FinalizedAt == nil ||
		detail.TerminalRequests[0].BlockKind == nil ||
		*detail.TerminalRequests[0].BlockKind != model.BlockKindCapability ||
		detail.TerminalRequests[0].Reason == nil ||
		*detail.TerminalRequests[0].Reason != reason {
		t.Fatalf("pending block request was not finalized exactly: %#v", detail.TerminalRequests)
	}
	active, err := opened.GetActiveRecoveryCheckpoint(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active != nil {
		t.Fatalf("clean stopped block created a recovery checkpoint: %#v", active)
	}
}

func TestSupervisorReissuesConfirmedAdoptionWhenTargetWorktreeDisappears(t *testing.T) {
	ctx := context.Background()
	fixture := createBoundaryPendingCheckpoint(t)
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: fixture.taskID,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim recovery run: %+v, %v", claim, err)
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
	adopted, err := reserveAndAdoptRecoveryCheckpoint(
		ctx,
		opened,
		workspaces,
		prepared,
	)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil ||
		adopted.State != model.RecoveryCheckpointAdopted ||
		adopted.AdoptedOutputBaseCommit == nil ||
		adopted.AdoptedHeadCommit == nil ||
		adopted.ReservedRunID == nil ||
		*adopted.ReservedRunID != claim.Run.ID {
		t.Fatalf("checkpoint adoption was not durably confirmed: %#v", adopted)
	}
	confirmedOutputBase := *adopted.AdoptedOutputBaseCommit
	confirmedHead := *adopted.AdoptedHeadCommit
	missingWorktree := prepared.Workspace.Path
	if _, err := opened.RecordSpawn(
		ctx,
		scope,
		99999999,
		filepath.Join(t.TempDir(), "lost-worktree-worker.log"),
	); err != nil {
		t.Fatal(err)
	}
	remove := exec.Command(
		"git", "-C", fixture.repository,
		"worktree", "remove", "--force", missingWorktree,
	)
	if output, err := remove.CombinedOutput(); err != nil {
		t.Fatalf("remove adopted worktree: %v\n%s", err, output)
	}
	if _, err := os.Stat(missingWorktree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adopted target worktree still exists: %v", err)
	}

	zero := time.Duration(0)
	options := Options{CrashGrace: &zero}
	options.normalize()
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	acknowledgeCurrentRunRecoveryFence(t, ctx, opened, scope)
	if err := recoverAbandonedRuns(ctx, opened, "default", options); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusReady ||
		detail.Task.CurrentRunID != nil ||
		len(detail.Runs) != 2 ||
		detail.Runs[1].ID != claim.Run.ID ||
		detail.Runs[1].Status != model.RunStatusCrashed ||
		detail.Runs[1].Error == nil ||
		!strings.Contains(
			*detail.Runs[1].Error,
			"preserved the last confirmed adopted checkpoint",
		) {
		t.Fatalf("lost adopted worktree was not safely requeued: %#v", detail)
	}
	checkpoints, err := opened.ListRecoveryCheckpoints(ctx, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoint supersession count = %d: %#v", len(checkpoints), checkpoints)
	}
	var previous, replacement *model.RecoveryCheckpoint
	for index := range checkpoints {
		checkpoint := &checkpoints[index]
		switch checkpoint.ID {
		case fixture.checkpoint.ID:
			previous = checkpoint
		default:
			if checkpoint.SourceRunID == claim.Run.ID {
				replacement = checkpoint
			}
		}
	}
	if previous == nil ||
		previous.State != model.RecoveryCheckpointSuperseded ||
		previous.SupersededByID == nil ||
		replacement == nil ||
		*previous.SupersededByID != replacement.ID {
		t.Fatalf("adopted checkpoint was not superseded exactly: %#v", checkpoints)
	}
	if replacement.State != model.RecoveryCheckpointPending ||
		replacement.SourceRunID != claim.Run.ID ||
		replacement.WorktreePath != missingWorktree ||
		replacement.OutputBaseCommit != confirmedOutputBase ||
		replacement.StartCommit != confirmedHead ||
		replacement.HeadCommit != confirmedHead ||
		replacement.DurableRef != "refs/autogora/checkpoints/"+claim.Run.ID ||
		replacement.ReservedRunID != nil ||
		replacement.ReservationToken != "" ||
		len(replacement.ChangedFiles) != 1 ||
		replacement.ChangedFiles[0] != "partial.txt" {
		t.Fatalf("unexpected reissued checkpoint: %#v", replacement)
	}
	refHead, err := exec.Command(
		"git", "-C", fixture.repository,
		"rev-parse", replacement.DurableRef,
	).Output()
	if err != nil ||
		strings.TrimSpace(string(refHead)) != confirmedHead {
		t.Fatalf("reissued ref = %q, want %s, err=%v", refHead, confirmedHead, err)
	}
	snapshotBody, err := exec.Command(
		"git", "-C", fixture.repository,
		"show", replacement.HeadCommit+":partial.txt",
	).Output()
	if err != nil || string(snapshotBody) != "confirmed partial\n" {
		t.Fatalf("reissued checkpoint content = %q, err=%v", snapshotBody, err)
	}
}

func TestSupervisorRegistersPublishedCheckpointAfterDatabaseGap(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update("default", boards.Update{
		DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "published-gap-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title:    "recover a published checkpoint before its database write",
		Assignee: &assignee, Runtime: model.RuntimeCline,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: task.Task.ID,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := store.RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.New(manager)
	workspaces.SetAllowWrites(true)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(prepared.Workspace.Path, "published.txt"),
		[]byte("published before database registration\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	published, err := workspaces.CaptureRecoveryCheckpoint(
		ctx,
		*prepared.Workspace,
		*prepared.Workspace.BaseCommit,
		task.Task.ID,
		task.Task.Title,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RecordSpawn(
		ctx,
		scope,
		99999999,
		filepath.Join(t.TempDir(), "published-gap.log"),
	); err != nil {
		t.Fatal(err)
	}
	remove := exec.Command(
		"git", "-C", repository,
		"worktree", "remove", "--force", prepared.Workspace.Path,
	)
	if output, err := remove.CombinedOutput(); err != nil {
		t.Fatalf("remove source worktree: %v\n%s", err, output)
	}

	zero := time.Duration(0)
	options := Options{CrashGrace: &zero}
	options.normalize()
	if err := recoverAbandonedRuns(
		ctx,
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	acknowledgeCurrentRunRecoveryFence(t, ctx, opened, scope)
	if err := recoverAbandonedRuns(
		ctx,
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusReady ||
		len(detail.Runs) != 1 ||
		detail.Runs[0].Status != model.RunStatusCrashed ||
		detail.Runs[0].Error == nil ||
		!strings.Contains(
			*detail.Runs[0].Error,
			"already-published immutable checkpoint",
		) {
		t.Fatalf("published checkpoint gap was not recovered: %#v", detail)
	}
	checkpoint, err := opened.GetActiveRecoveryCheckpoint(
		ctx,
		task.Task.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == nil ||
		checkpoint.State != model.RecoveryCheckpointPending ||
		checkpoint.SourceRunID != claim.Run.ID ||
		checkpoint.HeadCommit != published.HeadCommit ||
		checkpoint.DurableRef != published.DurableRef ||
		checkpoint.WorktreePath != prepared.Workspace.Path {
		t.Fatalf("registered published checkpoint = %#v, want %#v", checkpoint, published)
	}
	body, err := exec.Command(
		"git", "-C", repository,
		"show", checkpoint.HeadCommit+":published.txt",
	).Output()
	if err != nil ||
		string(body) != "published before database registration\n" {
		t.Fatalf("published checkpoint body = %q, err=%v", body, err)
	}
}

func TestSupervisorKeepsPublishedCumulativeRecoveryAfterTargetDisappears(t *testing.T) {
	ctx := context.Background()
	fixture := createBoundaryPendingCheckpoint(t)
	opened, err := fixture.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: fixture.taskID,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim recovery run: %+v, %v", claim, err)
	}
	scope := store.RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.New(fixture.manager)
	workspaces.SetAllowWrites(true)
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := reserveAndAdoptRecoveryCheckpoint(
		ctx,
		opened,
		workspaces,
		prepared,
	)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil ||
		adopted.AdoptedHeadCommit == nil {
		t.Fatalf("checkpoint was not adopted: %#v", adopted)
	}
	if err := os.WriteFile(
		filepath.Join(prepared.Workspace.Path, "partial.txt"),
		[]byte("confirmed partial\ncontinued before crash\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	published, err := workspaces.CaptureRecoveryCheckpoint(
		ctx,
		*prepared.Workspace,
		*adopted.AdoptedHeadCommit,
		fixture.taskID,
		prepared.Task.Task.Title,
	)
	if err != nil {
		t.Fatal(err)
	}
	if published.HeadCommit == *adopted.AdoptedHeadCommit {
		t.Fatal("cumulative checkpoint did not capture the continued work")
	}
	if _, err := opened.RecordSpawn(
		ctx,
		scope,
		99999999,
		filepath.Join(t.TempDir(), "published-cumulative.log"),
	); err != nil {
		t.Fatal(err)
	}
	remove := exec.Command(
		"git", "-C", fixture.repository,
		"worktree", "remove", "--force", prepared.Workspace.Path,
	)
	if output, err := remove.CombinedOutput(); err != nil {
		t.Fatalf("remove recovery target: %v\n%s", err, output)
	}

	zero := time.Duration(0)
	options := Options{CrashGrace: &zero}
	options.normalize()
	if err := recoverAbandonedRuns(
		ctx,
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	fence, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fence == nil || fence.RequiresOperator ||
		fence.HostAcknowledgedAt != nil {
		t.Fatalf("managed recovery fence before host acknowledgment = %#v", fence)
	}
	if _, err := opened.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if err := recoverAbandonedRuns(
		ctx,
		opened,
		"default",
		options,
	); err != nil {
		t.Fatal(err)
	}
	detail, err := opened.GetTask(ctx, fixture.taskID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusReady ||
		len(detail.Runs) != 2 ||
		detail.Runs[1].Status != model.RunStatusCrashed {
		t.Fatalf("cumulative checkpoint was not requeued: %#v", detail)
	}
	checkpoints, err := opened.ListRecoveryCheckpoints(
		ctx,
		fixture.taskID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("cumulative checkpoint count = %d: %#v", len(checkpoints), checkpoints)
	}
	var replacement *model.RecoveryCheckpoint
	for index := range checkpoints {
		if checkpoints[index].SourceRunID == claim.Run.ID {
			replacement = &checkpoints[index]
		}
	}
	if replacement == nil ||
		replacement.State != model.RecoveryCheckpointPending ||
		replacement.HeadCommit != published.HeadCommit ||
		replacement.StartCommit != *adopted.AdoptedHeadCommit ||
		replacement.DurableRef != published.DurableRef {
		t.Fatalf("published cumulative checkpoint was replaced incorrectly: %#v", checkpoints)
	}
	body, err := exec.Command(
		"git", "-C", fixture.repository,
		"show", replacement.HeadCommit+":partial.txt",
	).Output()
	if err != nil ||
		string(body) != "confirmed partial\ncontinued before crash\n" {
		t.Fatalf("cumulative checkpoint body = %q, err=%v", body, err)
	}
}
