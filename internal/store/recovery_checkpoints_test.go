package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type recoveryCheckpointFixture struct {
	store *Store
	task  model.TaskDetail
	claim *model.ClaimedTask
}

func newRecoveryCheckpointFixture(t *testing.T, maxRetries int) recoveryCheckpointFixture {
	t.Helper()
	opened, err := Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	assignee := "recovery-worker"
	task, err := opened.CreateTask(context.Background(), CreateTaskInput{
		Title:      "recover durable partial work",
		Assignee:   &assignee,
		Runtime:    model.RuntimeCodex,
		MaxRetries: maxRetries,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim source run: claim=%v err=%v", claim, err)
	}
	return recoveryCheckpointFixture{store: opened, task: task, claim: claim}
}

func recoveryCommit(character byte) string {
	return strings.Repeat(string(character), 40)
}

func recoveryFingerprint(character byte) string {
	return strings.Repeat(string(character), 64)
}

func recoveryCheckpointInput(
	runID string,
	worktree string,
	startCharacter byte,
	headCharacter byte,
) RegisterRecoveryCheckpointInput {
	return RegisterRecoveryCheckpointInput{
		RepositoryPath:          "/repository",
		WorktreePath:            worktree,
		OutputBaseCommit:        recoveryCommit('a'),
		StartCommit:             recoveryCommit(startCharacter),
		HeadCommit:              recoveryCommit(headCharacter),
		DurableRef:              "refs/autogora/checkpoints/" + runID,
		ChangedFiles:            []string{"src/main.go", " web/ spaced name.js "},
		TaskSpecFingerprint:     recoveryFingerprint('b'),
		PrerequisiteFingerprint: recoveryFingerprint('c'),
	}
}

func registerFailedRecoverySource(
	t *testing.T,
	fixture recoveryCheckpointFixture,
) model.RecoveryCheckpoint {
	t.Helper()
	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		context.Background(),
		RunScope{RunID: fixture.claim.Run.ID, ClaimToken: fixture.claim.ClaimToken},
		recoveryCheckpointInput(fixture.claim.Run.ID, "/worktree/source", 'a', 'd'),
		"worker process crashed",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != model.RecoveryCheckpointPending ||
		checkpoint.SourceRunID != fixture.claim.Run.ID ||
		checkpoint.TaskUpdatedAt != fixture.claim.Task.Task.UpdatedAt ||
		detail.Task.Status != model.TaskStatusReady {
		t.Fatalf("registered checkpoint=%+v task=%+v", checkpoint, detail.Task)
	}
	return checkpoint
}

func TestRecoveryCheckpointFailureRetryRequiresExactTerminalEffect(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	input := recoveryCheckpointInput(
		fixture.claim.Run.ID,
		"/worktree/exact-retry",
		'a',
		'd',
	)
	options := FailRunOptions{Outcome: model.RunStatusCrashed}
	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		input,
		"worker process crashed",
		options,
	)
	if err != nil {
		t.Fatal(err)
	}
	retried, retriedDetail, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		input,
		"worker process crashed",
		options,
	)
	if err != nil ||
		retried.ID != checkpoint.ID ||
		retriedDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"exact response-loss retry checkpoint=%+v task=%+v err=%v",
			retried,
			retriedDetail.Task,
			err,
		)
	}
	countFailure := false
	mismatches := []struct {
		name    string
		runErr  string
		options FailRunOptions
	}{
		{
			name:    "different error",
			runErr:  "another failure",
			options: options,
		},
		{
			name:   "different outcome",
			runErr: "worker process crashed",
			options: FailRunOptions{
				Outcome: model.RunStatusTimedOut,
			},
		},
		{
			name:   "different failure accounting",
			runErr: "worker process crashed",
			options: FailRunOptions{
				Outcome:      model.RunStatusCrashed,
				CountFailure: &countFailure,
			},
		},
	}
	for _, test := range mismatches {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
				ctx,
				scope,
				input,
				test.runErr,
				test.options,
			); err == nil || !strings.Contains(
				err.Error(),
				"does not match terminal effect",
			) {
				t.Fatalf("mismatched response-loss retry error = %v", err)
			}
		})
	}
}

func claimRecoveryRun(
	t *testing.T,
	opened *Store,
	taskID string,
) (*model.ClaimedTask, RunScope) {
	t.Helper()
	claim, err := opened.ClaimTask(context.Background(), ClaimOptions{TaskID: taskID})
	if err != nil || claim == nil {
		t.Fatalf("claim recovery run: claim=%v err=%v", claim, err)
	}
	return claim, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
}

func observedRecoveryOwnerForCheckpointTest(
	t *testing.T,
	opened *Store,
	scope RunScope,
) RunRecoveryObservation {
	t.Helper()
	ctx := context.Background()
	managed, err := opened.IsRunManaged(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !managed {
		if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
			t.Fatal(err)
		}
	}
	inspection, err := opened.GetRun(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := opened.GetRunProcessIdentity(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FenceObservedRunRecovery(
		ctx,
		ObserveRunForRecovery(inspection.Run, identity),
		30,
		"checkpoint recovery test fence",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	fence, err := opened.GetDeferredReclaim(ctx, scope.RunID)
	if err != nil || fence == nil {
		t.Fatalf("checkpoint recovery fence = %#v, err=%v", fence, err)
	}
	acknowledged, err := opened.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = opened.GetRun(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	owned, acquired, err := opened.ClaimObservedRunRecovery(
		ctx,
		ObserveRunForRecovery(inspection.Run, identity, &acknowledged),
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("checkpoint recovery owner acquired=%v err=%v", acquired, err)
	}
	return owned
}

func reserveRecoveryCheckpoint(
	t *testing.T,
	opened *Store,
	scope RunScope,
	checkpoint model.RecoveryCheckpoint,
) model.RecoveryCheckpoint {
	t.Helper()
	reserved, ok, err := opened.ReserveRecoveryCheckpoint(
		context.Background(),
		scope,
		ReserveRecoveryCheckpointInput{
			CheckpointID:            checkpoint.ID,
			TaskSpecFingerprint:     checkpoint.TaskSpecFingerprint,
			PrerequisiteFingerprint: checkpoint.PrerequisiteFingerprint,
		},
	)
	if err != nil || !ok || reserved == nil {
		t.Fatalf("reserve checkpoint: value=%+v reserved=%t err=%v", reserved, ok, err)
	}
	return *reserved
}

func TestRecoveryCheckpointLifecycleIsHiddenAndCompletionConsumptionIsAtomic(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	dependentAssignee := "dependent"
	dependent, err := fixture.store.CreateTask(ctx, CreateTaskInput{
		Title:    "must not unlock from checkpoint",
		Assignee: &dependentAssignee,
		Runtime:  model.RuntimeClaude,
		Parents:  []string{fixture.task.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := registerFailedRecoverySource(t, fixture)

	var changeSets, satisfied int
	if err := fixture.store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_change_sets WHERE task_id = ?",
		fixture.task.Task.ID,
	).Scan(&changeSets); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_links WHERE parent_id = ? AND child_id = ? AND satisfied_at IS NOT NULL",
		fixture.task.Task.ID,
		dependent.Task.ID,
	).Scan(&satisfied); err != nil {
		t.Fatal(err)
	}
	if changeSets != 0 || satisfied != 0 {
		t.Fatalf("checkpoint leaked into completion provenance: changeSets=%d satisfied=%d", changeSets, satisfied)
	}

	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	if checkpoint.TaskUpdatedAt == claim.Task.Task.UpdatedAt {
		t.Fatal("claim should update task audit version")
	}
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, checkpoint)
	if reserved.ReservationToken == "" || reserved.ReservedRunID == nil ||
		*reserved.ReservedRunID != claim.Run.ID {
		t.Fatalf("reservation missing ownership: %+v", reserved)
	}

	again, ok, err := fixture.store.ReserveRecoveryCheckpoint(ctx, scope, ReserveRecoveryCheckpointInput{
		CheckpointID:            checkpoint.ID,
		TaskSpecFingerprint:     checkpoint.TaskSpecFingerprint,
		PrerequisiteFingerprint: checkpoint.PrerequisiteFingerprint,
	})
	if err != nil || !ok || again == nil || again.ReservationToken != reserved.ReservationToken {
		t.Fatalf("idempotent reservation changed token: value=%+v ok=%t err=%v", again, ok, err)
	}
	adopted, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		checkpoint.OutputBaseCommit,
		recoveryCommit('e'),
	)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.State != model.RecoveryCheckpointAdopted ||
		adopted.AdoptedOutputBaseCommit == nil ||
		*adopted.AdoptedOutputBaseCommit != checkpoint.OutputBaseCommit ||
		adopted.AdoptedHeadCommit == nil ||
		*adopted.AdoptedHeadCommit != recoveryCommit('e') {
		t.Fatalf("adoption = %+v", adopted)
	}
	if _, err := fixture.store.ReleaseRecoveryCheckpointReservation(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
	); err == nil || !strings.Contains(err.Error(), "unadopted") {
		t.Fatalf("adopted reservation release error = %v", err)
	}

	if _, err := fixture.store.db.ExecContext(ctx, `CREATE TRIGGER reject_test_recovery_completion
		BEFORE UPDATE OF status ON tasks
		WHEN OLD.id = '`+fixture.task.Task.ID+`' AND NEW.status = 'done'
		BEGIN
			SELECT RAISE(ABORT, 'test completion rollback');
		END`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CompleteRun(
		ctx,
		scope,
		CompletionInput{Summary: "recovered result"},
	); err == nil || !strings.Contains(err.Error(), "test completion rollback") {
		t.Fatalf("forced completion error = %v", err)
	}
	rolledBack, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || rolledBack == nil || rolledBack.State != model.RecoveryCheckpointAdopted {
		t.Fatalf("rolled-back consumption persisted: checkpoint=%+v err=%v", rolledBack, err)
	}
	if _, err := fixture.store.db.ExecContext(ctx, "DROP TRIGGER reject_test_recovery_completion"); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.CompleteRun(
		ctx,
		scope,
		CompletionInput{Summary: "recovered result"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone {
		t.Fatalf("recovery completion status = %s", completed.Task.Status)
	}
	consumed, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || consumed == nil || consumed.State != model.RecoveryCheckpointConsumed ||
		consumed.ConsumedAt == nil {
		t.Fatalf("consumed checkpoint=%+v err=%v", consumed, err)
	}
	dependentAfter, err := fixture.store.GetTask(ctx, dependent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dependentAfter.Task.Status != model.TaskStatusReady {
		t.Fatalf("successful task completion did not unlock dependent: %s", dependentAfter.Task.Status)
	}
}

func TestActiveRecoveryCheckpointPreventsEveryDoneTransition(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)

	if _, err := fixture.store.CompleteTask(
		ctx,
		fixture.task.Task.ID,
		CompletionInput{Summary: "administrative completion"},
	); err == nil || !strings.Contains(err.Error(), "active recovery checkpoint") {
		t.Fatalf("administrative completion error = %v", err)
	}
	active, err := fixture.store.GetActiveRecoveryCheckpoint(
		ctx,
		fixture.task.Task.ID,
	)
	if err != nil || active == nil || active.ID != checkpoint.ID ||
		active.State != model.RecoveryCheckpointPending {
		t.Fatalf("checkpoint after administrative rejection = %+v err=%v", active, err)
	}

	if _, err := fixture.store.db.ExecContext(
		ctx,
		"UPDATE tasks SET status = 'done' WHERE id = ?",
		fixture.task.Task.ID,
	); err == nil || !strings.Contains(
		err.Error(),
		"task cannot be done with an active recovery checkpoint",
	) {
		t.Fatalf("raw Done transition error = %v", err)
	}

	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	if err := fixture.store.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RequestRunCompletion(
		ctx,
		scope,
		CompletionInput{Summary: "ordinary run ignored pending recovery"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinalizeRunTerminal(
		ctx,
		scope,
		0,
	); err == nil || !strings.Contains(err.Error(), "active recovery checkpoint") {
		t.Fatalf("worker completion error = %v", err)
	}
	inspection, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil || inspection.Run.Status != model.RunStatusRunning ||
		inspection.Task.Status != model.TaskStatusRunning {
		t.Fatalf("rejected run completion = %+v err=%v", inspection, err)
	}
	active, err = fixture.store.GetActiveRecoveryCheckpoint(
		ctx,
		fixture.task.Task.ID,
	)
	if err != nil || active == nil ||
		active.State != model.RecoveryCheckpointPending {
		t.Fatalf("checkpoint after worker rejection = %+v err=%v", active, err)
	}
}

func TestOpenRejectsDoneTaskWithActiveRecoveryCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "recovery-worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:      "corrupt completion state",
		Assignee:   &assignee,
		Runtime:    model.RuntimeCodex,
		MaxRetries: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim source run: claim=%+v err=%v", claim, err)
	}
	countFailure := false
	if _, _, err := opened.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		recoveryCheckpointInput(claim.Run.ID, "/worktree/corrupt", 'a', 'd'),
		"source run stopped",
		FailRunOptions{
			Outcome:      model.RunStatusCrashed,
			CountFailure: &countFailure,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"DROP TRIGGER recovery_checkpoint_prevent_done_with_active",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE tasks SET status = 'done' WHERE id = ?",
		task.Task.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if reopened != nil {
		_ = reopened.Close()
	}
	if err == nil || !strings.Contains(
		err.Error(),
		"done task "+task.Task.ID+" has active checkpoint",
	) {
		t.Fatalf("corrupt recovery open error = %v", err)
	}
}

func TestRecoveryCheckpointFingerprintCASIgnoresAuditVersionAndSupersedesStaleWork(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)

	stale, reserved, err := fixture.store.ReserveRecoveryCheckpoint(ctx, scope, ReserveRecoveryCheckpointInput{
		CheckpointID:            checkpoint.ID,
		TaskSpecFingerprint:     recoveryFingerprint('e'),
		PrerequisiteFingerprint: checkpoint.PrerequisiteFingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved || stale == nil || stale.State != model.RecoveryCheckpointSuperseded ||
		stale.SupersedeReason == nil ||
		!strings.Contains(*stale.SupersedeReason, "specification") ||
		stale.ReservationToken != "" {
		t.Fatalf("stale fingerprint result=%+v reserved=%t", stale, reserved)
	}
	retriedStale, retriedReserved, err := fixture.store.ReserveRecoveryCheckpoint(
		ctx,
		scope,
		ReserveRecoveryCheckpointInput{
			CheckpointID:            checkpoint.ID,
			TaskSpecFingerprint:     recoveryFingerprint('e'),
			PrerequisiteFingerprint: checkpoint.PrerequisiteFingerprint,
		},
	)
	if err != nil || retriedReserved || retriedStale == nil ||
		retriedStale.State != model.RecoveryCheckpointSuperseded {
		t.Fatalf(
			"idempotent stale fingerprint retry=%+v reserved=%t err=%v",
			retriedStale,
			retriedReserved,
			err,
		)
	}
	if _, err := fixture.store.db.ExecContext(ctx,
		"UPDATE recovery_checkpoints SET superseded_by_id = id WHERE id = ?",
		checkpoint.ID,
	); err == nil || !strings.Contains(err.Error(), "invalid recovery checkpoint replacement") {
		t.Fatalf("invalid superseded_by update error = %v", err)
	}
	run, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil || run.Run.Status != model.RunStatusRunning {
		t.Fatalf("fingerprint rejection terminalized fresh run: %+v err=%v", run, err)
	}

	newInput := recoveryCheckpointInput(claim.Run.ID, "/worktree/fresh", 'a', 'e')
	newInput.TaskSpecFingerprint = recoveryFingerprint('e')
	newCheckpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		newInput,
		"fresh run also failed",
		FailRunOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if newCheckpoint.State != model.RecoveryCheckpointPending ||
		newCheckpoint.ID == checkpoint.ID ||
		detail.Task.Status != model.TaskStatusReady {
		t.Fatalf("replacement checkpoint=%+v task=%+v", newCheckpoint, detail.Task)
	}
	listed, err := fixture.store.ListRecoveryCheckpoints(ctx, fixture.task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 ||
		listed[0].State != model.RecoveryCheckpointSuperseded ||
		listed[1].State != model.RecoveryCheckpointPending {
		t.Fatalf("checkpoint history = %+v", listed)
	}
}

func TestRecoveryCheckpointReservationSerializesAndSetupFailureReleasesAtomically(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)

	const callers = 8
	type result struct {
		checkpoint *model.RecoveryCheckpoint
		reserved   bool
		err        error
	}
	results := make(chan result, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, reserved, err := fixture.store.ReserveRecoveryCheckpoint(
				ctx,
				scope,
				ReserveRecoveryCheckpointInput{
					CheckpointID:            checkpoint.ID,
					TaskSpecFingerprint:     checkpoint.TaskSpecFingerprint,
					PrerequisiteFingerprint: checkpoint.PrerequisiteFingerprint,
				},
			)
			results <- result{checkpoint: value, reserved: reserved, err: err}
		}()
	}
	wait.Wait()
	close(results)
	token := ""
	for value := range results {
		if value.err != nil || !value.reserved || value.checkpoint == nil {
			t.Fatalf(
				"concurrent reserve result: checkpoint=%+v reserved=%t err=%v",
				value.checkpoint,
				value.reserved,
				value.err,
			)
		}
		if token == "" {
			token = value.checkpoint.ReservationToken
		} else if token != value.checkpoint.ReservationToken {
			t.Fatalf("reservation token changed: %q != %q", token, value.checkpoint.ReservationToken)
		}
	}
	var reserveEvents int
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND run_id = ? AND kind = 'recovery_checkpoint_reserved'`,
		fixture.task.Task.ID,
		claim.Run.ID,
	).Scan(&reserveEvents); err != nil {
		t.Fatal(err)
	}
	if reserveEvents != 1 {
		t.Fatalf("reservation events = %d, want 1", reserveEvents)
	}

	if _, _, err := fixture.store.ReleaseRecoveryCheckpointReservationAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		"wrong-token",
		"runner setup failed",
		FailRunOptions{},
	); err == nil {
		t.Fatal("wrong reservation token released checkpoint")
	}
	stillRunning, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil || stillRunning.Run.Status != model.RunStatusRunning {
		t.Fatalf("failed release terminalized run: %+v err=%v", stillRunning, err)
	}

	released, detail, err := fixture.store.ReleaseRecoveryCheckpointReservationAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		token,
		"runner setup failed",
		FailRunOptions{Outcome: model.RunStatusSpawnFailed},
	)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != model.RecoveryCheckpointPending ||
		released.ReservedRunID != nil ||
		released.ReservationToken != "" ||
		released.LastReleasedRunID == nil ||
		*released.LastReleasedRunID != claim.Run.ID ||
		detail.Task.Status != model.TaskStatusReady {
		t.Fatalf("atomic release=%+v task=%+v", released, detail.Task)
	}
	retriedRelease, retriedDetail, err := fixture.store.ReleaseRecoveryCheckpointReservationAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		token,
		"runner setup failed",
		FailRunOptions{Outcome: model.RunStatusSpawnFailed},
	)
	if err != nil || retriedRelease.ID != released.ID ||
		retriedDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent release retry: checkpoint=%+v task=%+v err=%v",
			retriedRelease,
			retriedDetail.Task,
			err,
		)
	}

	next, nextScope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reused := reserveRecoveryCheckpoint(t, fixture.store, nextScope, released)
	if reused.ReservedRunID == nil || *reused.ReservedRunID != next.Run.ID {
		t.Fatalf("released checkpoint was not reusable: %+v", reused)
	}
}

func TestGenericRunTerminalizationReleasesReservedAndRejectsAdoptedCheckpoint(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	firstRecovery, firstScope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, firstScope, checkpoint)
	if _, err := fixture.store.FailRun(
		ctx,
		firstScope,
		"recovery process exited before adoption",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	); err != nil {
		t.Fatal(err)
	}
	released, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || released == nil ||
		released.State != model.RecoveryCheckpointPending ||
		released.LastReleasedRunID == nil ||
		*released.LastReleasedRunID != firstRecovery.Run.ID ||
		released.LastReleaseToken != reserved.ReservationToken {
		t.Fatalf("generic terminal release=%+v err=%v", released, err)
	}
	var automaticEvents int
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND run_id = ? AND kind = 'recovery_checkpoint_released'
			AND json_extract(payload_json, '$.automatic') = 1`,
		fixture.task.Task.ID,
		firstRecovery.Run.ID,
	).Scan(&automaticEvents); err != nil {
		t.Fatal(err)
	}
	if automaticEvents != 1 {
		t.Fatalf("automatic release events = %d, want 1", automaticEvents)
	}

	secondRecovery, secondScope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved = reserveRecoveryCheckpoint(t, fixture.store, secondScope, *released)
	if _, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		secondScope,
		checkpoint.ID,
		reserved.ReservationToken,
		checkpoint.OutputBaseCommit,
		recoveryCommit('e'),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FailRun(
		ctx,
		secondScope,
		"must preserve adopted work",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	); err == nil || !strings.Contains(err.Error(), "consumed or superseded") {
		t.Fatalf("generic adopted failure error = %v", err)
	}
	stillActive, err := fixture.store.GetRun(ctx, secondRecovery.Run.ID)
	if err != nil || stillActive.Run.Status != model.RunStatusRunning {
		t.Fatalf("adopted failure escaped transaction: run=%+v err=%v", stillActive, err)
	}
	stillAdopted, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || stillAdopted == nil ||
		stillAdopted.State != model.RecoveryCheckpointAdopted {
		t.Fatalf("adopted checkpoint changed: %+v err=%v", stillAdopted, err)
	}
}

func TestRecoveryCheckpointAdoptionSupersedesWithCumulativeSnapshot(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, checkpoint)
	adoptedBase := recoveryCommit('e')
	adoptedHead := recoveryCommit('f')
	if _, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		adoptedBase,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}

	cumulative := recoveryCheckpointInput(claim.Run.ID, "/worktree/recovery", 'd', '1')
	cumulative.OutputBaseCommit = adoptedBase
	cumulative.StartCommit = recoveryCommit('d')
	if _, _, err := fixture.store.SupersedeRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		cumulative,
		"recovery failed",
		FailRunOptions{},
	); err == nil || !strings.Contains(err.Error(), "adopted head") {
		t.Fatalf("wrong cumulative start error = %v", err)
	}
	activeRun, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil || activeRun.Run.Status != model.RunStatusRunning {
		t.Fatalf("rejected cumulative checkpoint changed run: %+v err=%v", activeRun, err)
	}
	old, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || old == nil || old.State != model.RecoveryCheckpointAdopted {
		t.Fatalf("rejected cumulative checkpoint changed old row: %+v err=%v", old, err)
	}

	cumulative.StartCommit = adoptedHead
	cumulative.ChangedFiles = []string{"only-current-output.txt"}
	replacement, detail, err := fixture.store.SupersedeRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		cumulative,
		"recovery process crashed",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.State != model.RecoveryCheckpointPending ||
		replacement.SourceRunID != claim.Run.ID ||
		replacement.StartCommit != adoptedHead ||
		replacement.OutputBaseCommit != adoptedBase ||
		len(replacement.ChangedFiles) != 1 ||
		replacement.ChangedFiles[0] != "only-current-output.txt" ||
		detail.Task.Status != model.TaskStatusReady {
		t.Fatalf("cumulative replacement=%+v task=%+v", replacement, detail.Task)
	}
	old, err = fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID)
	if err != nil || old == nil ||
		old.State != model.RecoveryCheckpointSuperseded ||
		old.SupersededByID == nil ||
		*old.SupersededByID != replacement.ID {
		t.Fatalf("superseded checkpoint=%+v err=%v", old, err)
	}

	again, _, err := fixture.store.SupersedeRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		cumulative,
		"recovery process crashed",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	)
	if err != nil || again.ID != replacement.ID {
		t.Fatalf("idempotent cumulative retry=%+v err=%v", again, err)
	}
}

func TestAdoptedRecoveryCheckpointBlockFinalizesWithCumulativeSnapshotAtomically(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	original := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, original)
	adoptedBase, adoptedHead := recoveryCommit('e'), recoveryCommit('f')
	if _, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		adoptedBase,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	requested, err := fixture.store.BlockRun(ctx, scope, BlockInput{
		Reason: "human approval is required",
		Kind:   model.BlockKindNeedsInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested.Task.Status != model.TaskStatusRunning {
		t.Fatalf("managed block finalized before checkpoint capture: %s", requested.Task.Status)
	}

	cumulative := recoveryCheckpointInput(
		claim.Run.ID,
		"/worktree/recovery-block",
		'f',
		'1',
	)
	cumulative.OutputBaseCommit = adoptedBase
	if _, err := fixture.store.db.ExecContext(ctx, `CREATE TRIGGER reject_recovery_block_test
		BEFORE UPDATE OF status ON tasks
		WHEN NEW.status = 'blocked'
		BEGIN
			SELECT RAISE(ABORT, 'forced recovery block failure');
		END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.SupersedeRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		cumulative,
		75,
	); err == nil || !strings.Contains(err.Error(), "forced recovery block failure") {
		t.Fatalf("atomic block rollback error = %v", err)
	}
	if _, err := fixture.store.db.ExecContext(ctx, "DROP TRIGGER reject_recovery_block_test"); err != nil {
		t.Fatal(err)
	}
	stillAdopted, err := fixture.store.GetRecoveryCheckpoint(ctx, original.ID)
	if err != nil || stillAdopted == nil ||
		stillAdopted.State != model.RecoveryCheckpointAdopted {
		t.Fatalf("failed block changed adopted checkpoint: %+v err=%v", stillAdopted, err)
	}
	if existing, err := getRecoveryCheckpointBySourceRun(ctx, fixture.store.db, claim.Run.ID); err != nil || existing != nil {
		t.Fatalf("failed block left replacement: %+v err=%v", existing, err)
	}
	inspection, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil || inspection.Run.Status != model.RunStatusRunning {
		t.Fatalf("failed block terminalized run: %+v err=%v", inspection, err)
	}
	pendingRequest, err := fixture.store.GetRunTerminalRequest(ctx, claim.Run.ID)
	if err != nil || pendingRequest == nil || pendingRequest.FinalizedAt != nil {
		t.Fatalf("failed block changed terminal request: %+v err=%v", pendingRequest, err)
	}

	replacement, detail, err := fixture.store.SupersedeRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		cumulative,
		75,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.State != model.RecoveryCheckpointPending ||
		replacement.SourceRunID != claim.Run.ID ||
		replacement.ReservationToken != "" ||
		replacement.StartCommit != adoptedHead ||
		replacement.OutputBaseCommit != adoptedBase ||
		detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.CurrentRunID != nil {
		t.Fatalf("blocked replacement=%+v task=%+v", replacement, detail.Task)
	}
	inspection, err = fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil ||
		inspection.Run.Status != model.RunStatusBlocked ||
		inspection.Run.ExitCode == nil ||
		*inspection.Run.ExitCode != 75 {
		t.Fatalf("blocked run=%+v err=%v", inspection.Run, err)
	}
	finalizedRequest, err := fixture.store.GetRunTerminalRequest(ctx, claim.Run.ID)
	if err != nil || finalizedRequest == nil || finalizedRequest.FinalizedAt == nil {
		t.Fatalf("block request was not finalized: %+v err=%v", finalizedRequest, err)
	}
	for _, event := range detail.Events {
		if strings.Contains(string(event.Payload), reserved.ReservationToken) {
			t.Fatalf("reservation token leaked into event %s: %s", event.Kind, event.Payload)
		}
	}

	again, againDetail, err := fixture.store.SupersedeRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		cumulative,
		75,
	)
	if err != nil ||
		again.ID != replacement.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent checkpoint block retry=%+v task=%+v err=%v",
			again,
			againDetail.Task,
			err,
		)
	}
}

func TestOrdinaryRecoveryCheckpointBlockRegistersSnapshotAtomically(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BlockRun(ctx, scope, BlockInput{
		Reason: "worker needs an API decision",
		Kind:   model.BlockKindNeedsInput,
	}); err != nil {
		t.Fatal(err)
	}
	input := recoveryCheckpointInput(
		fixture.claim.Run.ID,
		"/worktree/ordinary-block",
		'a',
		'd',
	)

	if _, err := fixture.store.db.ExecContext(ctx, `CREATE TRIGGER reject_ordinary_recovery_block_test
		BEFORE UPDATE OF status ON tasks
		WHEN NEW.status = 'blocked'
		BEGIN
			SELECT RAISE(ABORT, 'forced ordinary recovery block failure');
		END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.RegisterRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		input,
		75,
	); err == nil || !strings.Contains(err.Error(), "forced ordinary recovery block failure") {
		t.Fatalf("atomic ordinary block rollback error = %v", err)
	}
	if _, err := fixture.store.db.ExecContext(
		ctx,
		"DROP TRIGGER reject_ordinary_recovery_block_test",
	); err != nil {
		t.Fatal(err)
	}
	if existing, err := getRecoveryCheckpointBySourceRun(
		ctx,
		fixture.store.db,
		fixture.claim.Run.ID,
	); err != nil || existing != nil {
		t.Fatalf("failed ordinary block left checkpoint: %+v err=%v", existing, err)
	}
	inspection, err := fixture.store.GetRun(ctx, fixture.claim.Run.ID)
	if err != nil || inspection.Run.Status != model.RunStatusRunning {
		t.Fatalf("failed ordinary block terminalized run: %+v err=%v", inspection, err)
	}

	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		input,
		75,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != model.RecoveryCheckpointPending ||
		checkpoint.SourceRunID != fixture.claim.Run.ID ||
		checkpoint.ReservationToken != "" ||
		detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.CurrentRunID != nil {
		t.Fatalf("ordinary blocked checkpoint=%+v task=%+v", checkpoint, detail.Task)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, fixture.claim.Run.ID)
	if err != nil || request == nil || request.FinalizedAt == nil {
		t.Fatalf("ordinary block request was not finalized: %+v err=%v", request, err)
	}
	inspection, err = fixture.store.GetRun(ctx, fixture.claim.Run.ID)
	if err != nil ||
		inspection.Run.Status != model.RunStatusBlocked ||
		inspection.Run.ExitCode == nil ||
		*inspection.Run.ExitCode != 75 {
		t.Fatalf("ordinary blocked run=%+v err=%v", inspection.Run, err)
	}

	again, againDetail, err := fixture.store.RegisterRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		input,
		75,
	)
	if err != nil ||
		again.ID != checkpoint.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent ordinary checkpoint block=%+v task=%+v err=%v",
			again,
			againDetail.Task,
			err,
		)
	}
}

func TestSupervisorAtomicallyRegistersCheckpointAndRecoversOrdinaryBlockedRun(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BlockRun(ctx, scope, BlockInput{
		Reason: "worker requested input before its process exited",
		Kind:   model.BlockKindNeedsInput,
	}); err != nil {
		t.Fatal(err)
	}
	input := recoveryCheckpointInput(
		fixture.claim.Run.ID,
		"/worktree/supervisor-ordinary-block",
		'a',
		'd',
	)
	exitCode := 124
	blocked := RecoverBlockedRunInput{
		Outcome:  model.RunStatusTimedOut,
		Reason:   "supervisor preserved the stopped worker's partial work",
		Kind:     model.BlockKindNeedsInput,
		ExitCode: &exitCode,
	}
	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope,
		input,
		blocked,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != model.RecoveryCheckpointPending ||
		checkpoint.SourceRunID != fixture.claim.Run.ID ||
		checkpoint.ReservationToken != "" ||
		detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.CurrentRunID != nil {
		t.Fatalf("supervisor ordinary block checkpoint=%+v task=%+v", checkpoint, detail.Task)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, fixture.claim.Run.ID)
	if err != nil || request != nil {
		t.Fatalf("supervisor ordinary block left terminal request: %+v err=%v", request, err)
	}
	again, againDetail, err := fixture.store.RegisterRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope,
		input,
		blocked,
	)
	if err != nil ||
		again.ID != checkpoint.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent supervisor ordinary block=%+v task=%+v err=%v",
			again,
			againDetail.Task,
			err,
		)
	}
}

func TestActiveHostCheckpointRecoveryBridgeRejectsConcurrentFence(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	if err := fixture.store.MarkRunManagedWithPolicy(
		ctx,
		scope,
		true,
	); err != nil {
		t.Fatal(err)
	}
	inspection, err := fixture.store.GetRun(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FenceObservedRunRecovery(
		ctx,
		ObserveRunForRecovery(inspection.Run, nil),
		30,
		"Supervisor won before active-host checkpoint fallback",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	exitCode := 1
	if _, _, err := fixture.store.RegisterRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope,
		recoveryCheckpointInput(
			scope.RunID,
			"/worktree/fenced-active-host",
			'a',
			'd',
		),
		RecoverBlockedRunInput{
			Outcome:  model.RunStatusBlocked,
			Reason:   "late active-host fallback",
			Kind:     model.BlockKindNeedsInput,
			ExitCode: &exitCode,
		},
	); !errors.Is(err, ErrRunTerminationPending) {
		t.Fatalf(
			"fenced active-host checkpoint error = %v, want ErrRunTerminationPending",
			err,
		)
	}
	if checkpoint, err := getRecoveryCheckpointBySourceRun(
		ctx,
		fixture.store.db,
		scope.RunID,
	); err != nil {
		t.Fatal(err)
	} else if checkpoint != nil {
		t.Fatalf("fenced active host registered checkpoint: %#v", checkpoint)
	}
	inspection, err = fixture.store.GetRun(ctx, scope.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != model.RunStatusRunning ||
		inspection.Task.Status != model.TaskStatusRunning {
		t.Fatalf("fenced active host terminalized run: %#v", inspection)
	}
}

func TestSupervisorStoppedBlockFinalizerPreservesRequestedReasonAndKind(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	const reason = "wait for an external prerequisite"
	if _, err := fixture.store.BlockRun(ctx, scope, BlockInput{
		Reason: reason,
		Kind:   model.BlockKindDependency,
	}); err != nil {
		t.Fatal(err)
	}
	input := recoveryCheckpointInput(
		fixture.claim.Run.ID,
		"/worktree/stopped-ordinary-block",
		'a',
		'd',
	)
	owned := observedRecoveryOwnerForCheckpointTest(
		t,
		fixture.store,
		scope,
	)
	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFinalizeObservedStoppedBlock(
		ctx,
		owned,
		input,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != model.RecoveryCheckpointPending ||
		detail.Task.Status != model.TaskStatusTodo ||
		detail.Task.BlockReason == nil ||
		*detail.Task.BlockReason != reason ||
		detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindDependency {
		t.Fatalf("stopped block checkpoint=%+v task=%+v", checkpoint, detail.Task)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, fixture.claim.Run.ID)
	if err != nil || request == nil || request.FinalizedAt == nil {
		t.Fatalf("stopped block request was not finalized: %+v err=%v", request, err)
	}
	again, againDetail, err := fixture.store.RegisterRecoveryCheckpointAndFinalizeObservedStoppedBlock(
		ctx,
		owned,
		input,
		0,
	)
	if err != nil ||
		again.ID != checkpoint.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent stopped block=%+v task=%+v err=%v",
			again,
			againDetail.Task,
			err,
		)
	}
}

func TestSupervisorAtomicallySupersedesCheckpointAndRecoversBlockedRun(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	original := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, original)
	adoptedBase, adoptedHead := recoveryCommit('e'), recoveryCommit('f')
	if _, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		adoptedBase,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.BlockRun(ctx, scope, BlockInput{
		Reason: "worker requested operator input",
		Kind:   model.BlockKindNeedsInput,
	}); err != nil {
		t.Fatal(err)
	}

	cumulative := recoveryCheckpointInput(
		claim.Run.ID,
		"/worktree/supervisor-block",
		'f',
		'1',
	)
	cumulative.OutputBaseCommit = adoptedBase
	exitCode := 124
	blocked := RecoverBlockedRunInput{
		Outcome:  model.RunStatusTimedOut,
		Reason:   "supervisor observed timeout after block request",
		Kind:     model.BlockKindNeedsInput,
		ExitCode: &exitCode,
	}
	replacement, detail, err := fixture.store.SupersedeRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope,
		cumulative,
		blocked,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.State != model.RecoveryCheckpointPending ||
		replacement.ReservationToken != "" ||
		replacement.StartCommit != adoptedHead ||
		replacement.OutputBaseCommit != adoptedBase ||
		detail.Task.Status != model.TaskStatusBlocked ||
		detail.Task.CurrentRunID != nil {
		t.Fatalf("supervisor blocked replacement=%+v task=%+v", replacement, detail.Task)
	}
	inspection, err := fixture.store.GetRun(ctx, claim.Run.ID)
	if err != nil ||
		inspection.Run.Status != model.RunStatusTimedOut ||
		inspection.Run.ExitCode == nil ||
		*inspection.Run.ExitCode != exitCode {
		t.Fatalf("supervisor blocked run=%+v err=%v", inspection.Run, err)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, claim.Run.ID)
	if err != nil || request != nil {
		t.Fatalf("supervisor left terminal request: %+v err=%v", request, err)
	}
	for _, event := range detail.Events {
		if strings.Contains(string(event.Payload), reserved.ReservationToken) {
			t.Fatalf("reservation token leaked into event %s: %s", event.Kind, event.Payload)
		}
	}

	again, againDetail, err := fixture.store.SupersedeRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope,
		cumulative,
		blocked,
	)
	if err != nil ||
		again.ID != replacement.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf(
			"idempotent supervisor block retry=%+v task=%+v err=%v",
			again,
			againDetail.Task,
			err,
		)
	}
}

func TestSupervisorAtomicallyCheckpointsAbandonedOrdinaryAndRecoveryRuns(t *testing.T) {
	ctx := context.Background()
	ordinary := newRecoveryCheckpointFixture(t, 6)
	ordinaryInput := recoveryCheckpointInput(
		ordinary.claim.Run.ID,
		"/worktree/supervisor-source",
		'a',
		'd',
	)
	ordinaryScope := RunScope{
		RunID:      ordinary.claim.Run.ID,
		ClaimToken: ordinary.claim.ClaimToken,
	}
	ordinaryOwner := observedRecoveryOwnerForCheckpointTest(
		t,
		ordinary.store,
		ordinaryScope,
	)
	checkpoint, detail, err := ordinary.store.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
		ctx,
		ordinaryOwner,
		ordinaryInput,
		model.RunStatusCrashed,
		"supervisor observed process exit",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != model.RecoveryCheckpointPending ||
		checkpoint.SourceRunID != ordinary.claim.Run.ID ||
		detail.Task.Status != model.TaskStatusReady {
		t.Fatalf("supervisor ordinary checkpoint=%+v task=%+v", checkpoint, detail.Task)
	}
	again, againDetail, err := ordinary.store.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
		ctx,
		ordinaryOwner,
		ordinaryInput,
		model.RunStatusCrashed,
		"supervisor observed process exit",
		true,
	)
	if err != nil || again.ID != checkpoint.ID ||
		againDetail.Task.Status != detail.Task.Status {
		t.Fatalf("idempotent supervisor ordinary retry=%+v task=%+v err=%v", again, againDetail.Task, err)
	}
	for _, mismatch := range []struct {
		name         string
		outcome      model.RunStatus
		runError     string
		countFailure bool
	}{
		{
			name:         "outcome",
			outcome:      model.RunStatusTimedOut,
			runError:     "supervisor observed process exit",
			countFailure: true,
		},
		{
			name:         "error",
			outcome:      model.RunStatusCrashed,
			runError:     "different process failure",
			countFailure: true,
		},
		{
			name:         "failure accounting",
			outcome:      model.RunStatusCrashed,
			runError:     "supervisor observed process exit",
			countFailure: false,
		},
	} {
		t.Run("ordinary retry rejects "+mismatch.name, func(t *testing.T) {
			if _, _, err := ordinary.store.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
				ctx,
				ordinaryOwner,
				ordinaryInput,
				mismatch.outcome,
				mismatch.runError,
				mismatch.countFailure,
			); err == nil || !strings.Contains(
				err.Error(),
				"does not match terminal effect",
			) {
				t.Fatalf("mismatched abandoned retry error = %v", err)
			}
		})
	}

	recovery := newRecoveryCheckpointFixture(t, 6)
	original := registerFailedRecoverySource(t, recovery)
	recoveryClaim, recoveryScope := claimRecoveryRun(t, recovery.store, recovery.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, recovery.store, recoveryScope, original)
	adoptedBase, adoptedHead := recoveryCommit('e'), recoveryCommit('f')
	if _, err := recovery.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		recoveryScope,
		original.ID,
		reserved.ReservationToken,
		adoptedBase,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}
	cumulative := recoveryCheckpointInput(
		recoveryClaim.Run.ID,
		"/worktree/supervisor-recovery",
		'f',
		'1',
	)
	cumulative.OutputBaseCommit = adoptedBase
	recoveryOwner := observedRecoveryOwnerForCheckpointTest(
		t,
		recovery.store,
		recoveryScope,
	)
	replacement, recoveredDetail, err := recovery.store.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
		ctx,
		recoveryOwner,
		cumulative,
		model.RunStatusTimedOut,
		"supervisor observed recovery timeout",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.State != model.RecoveryCheckpointPending ||
		replacement.SourceRunID != recoveryClaim.Run.ID ||
		replacement.OutputBaseCommit != adoptedBase ||
		replacement.StartCommit != adoptedHead ||
		recoveredDetail.Task.Status != model.TaskStatusReady {
		t.Fatalf("supervisor cumulative checkpoint=%+v task=%+v", replacement, recoveredDetail.Task)
	}
	superseded, err := recovery.store.GetRecoveryCheckpoint(ctx, original.ID)
	if err != nil || superseded == nil ||
		superseded.State != model.RecoveryCheckpointSuperseded ||
		superseded.SupersededByID == nil ||
		*superseded.SupersededByID != replacement.ID {
		t.Fatalf("supervisor superseded checkpoint=%+v err=%v", superseded, err)
	}
	repeatedReplacement, repeatedDetail, err := recovery.store.RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
		ctx,
		recoveryOwner,
		cumulative,
		model.RunStatusTimedOut,
		"supervisor observed recovery timeout",
		true,
	)
	if err != nil || repeatedReplacement.ID != replacement.ID ||
		repeatedDetail.Task.Status != recoveredDetail.Task.Status {
		t.Fatalf(
			"idempotent supervisor recovery retry=%+v task=%+v err=%v",
			repeatedReplacement,
			repeatedDetail.Task,
			err,
		)
	}
}

func TestRecoveryCheckpointSupersessionChainCascadesWithTaskDeletion(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 6)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	_, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, checkpoint)
	adoptedHead := recoveryCommit('e')
	if _, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		checkpoint.OutputBaseCommit,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}
	cumulative := recoveryCheckpointInput(scope.RunID, "/worktree/delete-chain", 'e', 'f')
	if _, _, err := fixture.store.SupersedeRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		checkpoint.ID,
		reserved.ReservationToken,
		cumulative,
		"recovery failed",
		FailRunOptions{},
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.DeleteTask(ctx, fixture.task.Task.ID); err != nil {
		t.Fatalf("delete task with checkpoint chain: %v", err)
	}
	var checkpoints, runs int
	if err := fixture.store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM recovery_checkpoints WHERE task_id = ?",
		fixture.task.Task.ID,
	).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_runs WHERE task_id = ?",
		fixture.task.Task.ID,
	).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 || runs != 0 {
		t.Fatalf("task delete left checkpoint provenance: checkpoints=%d runs=%d", checkpoints, runs)
	}
}

func TestRecoveryCheckpointRegistrationAndRunFailureRollbackTogether(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	if _, err := fixture.store.db.ExecContext(ctx, `CREATE TRIGGER reject_test_run_failure
		BEFORE UPDATE OF status ON task_runs
		WHEN OLD.id = '`+fixture.claim.Run.ID+`'
		BEGIN
			SELECT RAISE(ABORT, 'test terminalization failure');
		END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: fixture.claim.Run.ID, ClaimToken: fixture.claim.ClaimToken},
		recoveryCheckpointInput(fixture.claim.Run.ID, "/worktree/rollback", 'a', 'd'),
		"worker failed",
		FailRunOptions{},
	); err == nil || !strings.Contains(err.Error(), "test terminalization failure") {
		t.Fatalf("terminalization error = %v", err)
	}
	checkpoint, err := getRecoveryCheckpointBySourceRun(ctx, fixture.store.db, fixture.claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint != nil {
		t.Fatalf("checkpoint escaped rolled-back run failure: %+v", checkpoint)
	}
	run, err := fixture.store.GetRun(ctx, fixture.claim.Run.ID)
	if err != nil || run.Run.Status != model.RunStatusRunning {
		t.Fatalf("source run did not roll back: %+v err=%v", run, err)
	}
}

func TestRecoveryCheckpointSchemaProtectsProvenanceAndSingleActiveRow(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	checkpoint := registerFailedRecoverySource(t, fixture)
	if _, err := fixture.store.db.ExecContext(
		ctx,
		"DELETE FROM task_runs WHERE id = ?",
		fixture.claim.Run.ID,
	); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("schema allowed deleting checkpoint source run: %v", err)
	}
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)

	if _, err := fixture.store.db.ExecContext(ctx,
		"UPDATE recovery_checkpoints SET head_commit = ? WHERE id = ?",
		recoveryCommit('e'),
		checkpoint.ID,
	); err == nil || !strings.Contains(err.Error(), "provenance is immutable") {
		t.Fatalf("immutable provenance update error = %v", err)
	}
	if _, err := fixture.store.db.ExecContext(ctx,
		"UPDATE recovery_checkpoints SET state = 'consumed', consumed_at = ? WHERE id = ?",
		now(),
		checkpoint.ID,
	); err == nil {
		t.Fatal("pending checkpoint jumped directly to consumed")
	}
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, checkpoint)
	if _, err := fixture.store.db.ExecContext(ctx,
		"UPDATE recovery_checkpoints SET reservation_token = ? WHERE id = ?",
		"different-token",
		checkpoint.ID,
	); err == nil || !strings.Contains(err.Error(), "reservation ownership is immutable") {
		t.Fatalf("reservation ownership update error = %v", err)
	}
	if loaded, err := fixture.store.GetRecoveryCheckpoint(ctx, checkpoint.ID); err != nil ||
		loaded == nil || loaded.ReservationToken != reserved.ReservationToken {
		t.Fatalf("reservation token changed: checkpoint=%+v err=%v", loaded, err)
	}
	if _, err := fixture.store.db.ExecContext(ctx, `INSERT INTO recovery_checkpoints(
		id, task_id, source_run_id, repository_path, worktree_path,
		output_base_commit, start_commit, head_commit, durable_ref,
		changed_files_json, task_updated_at, task_spec_fingerprint,
		prerequisite_fingerprint, state, created_at, updated_at
	) SELECT 'rcp_duplicate', task_id, ?, repository_path, worktree_path,
		output_base_commit, start_commit, head_commit, durable_ref,
		changed_files_json, task_updated_at, task_spec_fingerprint,
		prerequisite_fingerprint, 'pending', ?, ?
	  FROM recovery_checkpoints WHERE id = ?`,
		claim.Run.ID,
		now(),
		now(),
		checkpoint.ID,
	); err == nil {
		t.Fatal("schema allowed two active checkpoints for one task")
	}
	var changeSets int
	if err := fixture.store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_change_sets WHERE task_id = ?",
		fixture.task.Task.ID,
	).Scan(&changeSets); err != nil {
		t.Fatal(err)
	}
	if changeSets != 0 {
		t.Fatalf("recovery checkpoint created %d task change sets", changeSets)
	}
}

func TestRecoveryCheckpointInputValidationPreservesExactFileNames(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	input := recoveryCheckpointInput(fixture.claim.Run.ID, "/worktree/files", 'A', 'D')
	input.RepositoryPath = " /repository with edge spaces "
	input.WorktreePath = " /worktree/with edge spaces "
	input.ChangedFiles = []string{" leading and trailing ", " leading and trailing ", "normal.go"}
	wrongRef := input
	wrongRef.DurableRef = "refs/autogora/checkpoints/not-the-source-run"
	if _, _, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: fixture.claim.Run.ID, ClaimToken: fixture.claim.ClaimToken},
		wrongRef,
		"worker failed",
		FailRunOptions{},
	); err == nil || !strings.Contains(err.Error(), "for source run") {
		t.Fatalf("wrong durable ref error = %v", err)
	}
	checkpoint, _, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: fixture.claim.Run.ID, ClaimToken: fixture.claim.ClaimToken},
		input,
		"worker failed",
		FailRunOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.StartCommit != recoveryCommit('a') ||
		checkpoint.HeadCommit != recoveryCommit('d') ||
		checkpoint.RepositoryPath != input.RepositoryPath ||
		checkpoint.WorktreePath != input.WorktreePath ||
		len(checkpoint.ChangedFiles) != 2 ||
		checkpoint.ChangedFiles[0] != " leading and trailing " {
		t.Fatalf("normalized checkpoint = %+v", checkpoint)
	}
	active, err := fixture.store.GetActiveRecoveryCheckpoint(
		ctx,
		"  "+fixture.task.Task.ID+"  ",
	)
	if err != nil || active == nil || active.ID != checkpoint.ID {
		t.Fatalf("trimmed task lookup checkpoint=%+v err=%v", active, err)
	}

	invalid := newRecoveryCheckpointFixture(t, 5)
	invalidInput := recoveryCheckpointInput(invalid.claim.Run.ID, "/worktree/invalid", 'a', 'd')
	invalidInput.ChangedFiles = []string{string([]byte{0xff})}
	if _, _, err := invalid.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: invalid.claim.Run.ID, ClaimToken: invalid.claim.ClaimToken},
		invalidInput,
		"worker failed",
		FailRunOptions{},
	); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("invalid file name error = %v", err)
	}
	run, err := invalid.store.GetRun(ctx, invalid.claim.Run.ID)
	if err != nil || run.Run.Status != model.RunStatusRunning {
		t.Fatalf("validation error terminalized source run: %+v err=%v", run, err)
	}
}

func TestRecoveryCheckpointSchemaRejectsInvalidChangedFilesJSON(t *testing.T) {
	for name, changedFilesJSON := range map[string]string{
		"malformed": "{",
		"object":    `{"path":"main.go"}`,
		"number":    `[1]`,
		"null":      `[null]`,
		"entry":     `[{}]`,
		"empty":     `[""]`,
		"nul":       `["bad\u0000path"]`,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRecoveryCheckpointFixture(t, 5)
			ctx := context.Background()
			input := recoveryCheckpointInput(
				fixture.claim.Run.ID,
				"/worktree/schema-json",
				'a',
				'd',
			)
			if _, err := fixture.store.db.ExecContext(ctx, `INSERT INTO recovery_checkpoints(
				id, task_id, source_run_id, repository_path, worktree_path,
				output_base_commit, start_commit, head_commit, durable_ref,
				changed_files_json, task_updated_at, task_spec_fingerprint,
				prerequisite_fingerprint, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
				"rcp_invalid_json_"+name,
				fixture.task.Task.ID,
				fixture.claim.Run.ID,
				input.RepositoryPath,
				input.WorktreePath,
				input.OutputBaseCommit,
				input.StartCommit,
				input.HeadCommit,
				input.DurableRef,
				changedFilesJSON,
				fixture.task.Task.UpdatedAt,
				input.TaskSpecFingerprint,
				input.PrerequisiteFingerprint,
				now(),
				now(),
			); err == nil {
				t.Fatalf("schema accepted changed_files_json %q", changedFilesJSON)
			}
		})
	}
}

func TestSchema24UpgradesVersion22WithRecoveryCheckpoints(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "version-22.db")
	initial, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := initial.CreateTask(ctx, CreateTaskInput{Title: "preserved v22 task"})
	if err != nil {
		initial.Close()
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		DROP TABLE recovery_checkpoints;
		PRAGMA user_version = 22;
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	preserved, err := upgraded.GetTask(ctx, created.Task.ID)
	if err != nil || preserved.Task.Title != created.Task.Title {
		t.Fatalf("v22 migration lost task: %+v err=%v", preserved, err)
	}
	var version, tableCount, triggerCount int
	if err := upgraded.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'recovery_checkpoints'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'trigger' AND name LIKE 'recovery_checkpoint_%'`).Scan(&triggerCount); err != nil {
		t.Fatal(err)
	}
	if version != 24 || schemaVersion != 24 || tableCount != 1 || triggerCount != 12 {
		t.Fatalf(
			"v22 migration version=%d constant=%d tables=%d triggers=%d",
			version,
			schemaVersion,
			tableCount,
			triggerCount,
		)
	}
}

func TestRecoveryCheckpointNotFoundIsNilAndOwnedReservationRejectsStaleScope(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	missing, err := fixture.store.GetRecoveryCheckpoint(ctx, "rcp_missing")
	if err != nil || missing != nil {
		t.Fatalf("missing checkpoint=%+v err=%v", missing, err)
	}
	checkpoint := registerFailedRecoverySource(t, fixture)
	_, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, checkpoint)
	_, err = fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		RunScope{RunID: scope.RunID, ClaimToken: "stale"},
		checkpoint.ID,
		reserved.ReservationToken,
		checkpoint.OutputBaseCommit,
		recoveryCommit('e'),
	)
	if err == nil || !strings.Contains(err.Error(), "invalid claim token") {
		t.Fatalf("stale scope error = %v", err)
	}
}
