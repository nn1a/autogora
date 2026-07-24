package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestReserveIntegrationResolutionIsClaimScopedIdempotentAndBounded(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "resolve integration", Assignee: &assignee, Runtime: model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer, MaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(claim *model.ClaimedTask, path string) (IntegrationResolutionReservation, error) {
		t.Helper()
		scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
		if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.BindRunWorkspace(ctx, RunScope{
			RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
		}, BindRunWorkspaceInput{
			Path: path, Kind: model.WorkspaceWorktree, Generated: true,
		}); err != nil {
			t.Fatal(err)
		}
		return opened.ReserveIntegrationResolution(ctx, RunScope{
			RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
		}, ReserveIntegrationResolutionInput{
			WorkspacePath: path, PrerequisiteID: "prerequisite",
			ChangeSetID: "changeset", ConflictFingerprint: strings.Repeat("a", 64),
			ConflictingFiles: []string{"conflict.txt"},
		})
	}
	block := func(claim *model.ClaimedTask) {
		t.Helper()
		if _, err := opened.BlockRun(ctx, RunScope{
			RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
		}, BlockInput{Reason: "resolution needs review: " + claim.Run.ID, Kind: model.BlockKindNeedsInput}); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.FinalizeRunTerminal(
			ctx,
			RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
			0,
		); err != nil {
			t.Fatal(err)
		}
	}
	claimTask := func() *model.ClaimedTask {
		t.Helper()
		claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
		if err != nil || claim == nil {
			t.Fatalf("claim: %#v, %v", claim, err)
		}
		return claim
	}

	first := claimTask()
	firstPath := filepath.Join(t.TempDir(), first.Run.ID)
	reservation, err := reserve(first, firstPath)
	if err != nil || reservation.Attempt != 1 || reservation.MaxAttempts != 2 {
		t.Fatalf("first reservation = %+v, %v", reservation, err)
	}
	if reservation.Started {
		t.Fatalf("preparation consumed an attempt: %+v", reservation)
	}
	repeated, err := opened.ReserveIntegrationResolution(ctx, RunScope{
		RunID: first.Run.ID, ClaimToken: first.ClaimToken,
	}, ReserveIntegrationResolutionInput{
		WorkspacePath: firstPath, PrerequisiteID: "prerequisite",
		ChangeSetID: "changeset", ConflictFingerprint: strings.Repeat("a", 64),
		ConflictingFiles: []string{"conflict.txt"},
	})
	if err != nil || repeated != reservation {
		t.Fatalf("same-run reservation was not idempotent: %+v, %v", repeated, err)
	}
	started, err := opened.StartIntegrationResolutionAttempt(ctx, RunScope{
		RunID: first.Run.ID, ClaimToken: first.ClaimToken,
	}, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("a", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	})
	if err != nil || !started.Started || !started.StartedNow {
		t.Fatalf("first start = %+v, %v", started, err)
	}
	repeatedStart, err := opened.StartIntegrationResolutionAttempt(ctx, RunScope{
		RunID: first.Run.ID, ClaimToken: first.ClaimToken,
	}, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("a", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	})
	if err != nil || !repeatedStart.Started || repeatedStart.StartedNow {
		t.Fatalf("same-run start was not idempotent: %+v, %v", repeatedStart, err)
	}
	block(first)
	if _, err := opened.ReserveIntegrationResolution(ctx, RunScope{
		RunID: first.Run.ID, ClaimToken: first.ClaimToken,
	}, ReserveIntegrationResolutionInput{
		WorkspacePath: firstPath, PrerequisiteID: "prerequisite", ChangeSetID: "changeset",
		ConflictFingerprint: strings.Repeat("a", 64),
	}); err == nil {
		t.Fatal("terminal claim reserved another resolution attempt")
	}

	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	second := claimTask()
	secondPath := filepath.Join(t.TempDir(), second.Run.ID)
	reservation, err = reserve(second, secondPath)
	if err != nil || reservation.Attempt != 2 || reservation.MaxAttempts != 2 {
		t.Fatalf("second reservation = %+v, %v", reservation, err)
	}
	started, err = opened.StartIntegrationResolutionAttempt(ctx, RunScope{
		RunID: second.Run.ID, ClaimToken: second.ClaimToken,
	}, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("a", 64),
		ExpectedAttempt:     2, ExpectedMaxAttempts: 2,
	})
	if err != nil || !started.StartedNow {
		t.Fatalf("second start = %+v, %v", started, err)
	}
	block(second)

	if _, err := opened.db.ExecContext(ctx, `UPDATE task_events SET created_at = '2000-01-01T00:00:00.000Z'
		WHERE task_id = ? AND kind = 'integration_resolution_started'`, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	removed, err := opened.GarbageCollectEvents(ctx, 0)
	if err != nil || removed < 2 {
		t.Fatalf("event GC removed %d rows: %v", removed, err)
	}

	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	third := claimTask()
	thirdPath := filepath.Join(t.TempDir(), third.Run.ID)
	reservation, err = reserve(third, thirdPath)
	if !errors.Is(err, ErrIntegrationResolutionExhausted) ||
		reservation.Attempt != 3 || reservation.MaxAttempts != 2 {
		t.Fatalf("exhausted reservation = %+v, %v", reservation, err)
	}
	var events, attempts int
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND kind = 'integration_resolution_started'`, task.Task.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_resolution_attempts
		WHERE task_id = ? AND conflict_fingerprint = ?`,
		task.Task.ID, strings.Repeat("a", 64)).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if events != 0 || attempts != 2 {
		t.Fatalf("GC reset authoritative attempts: events=%d attempts=%d", events, attempts)
	}

	block(third)
	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	fourth := claimTask()
	fourthPath := filepath.Join(t.TempDir(), fourth.Run.ID)
	if _, err := opened.BindRunWorkspace(ctx, RunScope{
		RunID: fourth.Run.ID, ClaimToken: fourth.ClaimToken,
	}, BindRunWorkspaceInput{
		Path: fourthPath, Kind: model.WorkspaceWorktree, Generated: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, RunScope{
		RunID: fourth.Run.ID, ClaimToken: fourth.ClaimToken,
	}, true); err != nil {
		t.Fatal(err)
	}
	reservation, err = opened.ReserveIntegrationResolution(ctx, RunScope{
		RunID: fourth.Run.ID, ClaimToken: fourth.ClaimToken,
	}, ReserveIntegrationResolutionInput{
		WorkspacePath: fourthPath, PrerequisiteID: "new-prerequisite",
		ChangeSetID: "new-changeset", ConflictFingerprint: strings.Repeat("b", 64),
	})
	if err != nil || reservation.Attempt != 1 || reservation.MaxAttempts != 2 {
		t.Fatalf("new conflict fingerprint inherited old attempts: %+v, %v", reservation, err)
	}
}

func TestIntegrationResolutionConcurrentPrepareAndStartCreatesOneAttemptAndEvent(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "concurrent resolution", Assignee: &assignee, Runtime: model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer, MaxRetries: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v, %v", claim, err)
	}
	workspacePath := filepath.Join(t.TempDir(), claim.Run.ID)
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.BindRunWorkspace(ctx, RunScope{
		RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
	}, BindRunWorkspaceInput{
		Path: workspacePath, Kind: model.WorkspaceWorktree, Generated: true,
	}); err != nil {
		t.Fatal(err)
	}
	input := ReserveIntegrationResolutionInput{
		WorkspacePath: workspacePath, PrerequisiteID: "prerequisite",
		ChangeSetID: "changeset", ConflictFingerprint: strings.Repeat("c", 64),
		ConflictingFiles: []string{"README.md"},
	}

	const callers = 16
	results := make(chan IntegrationResolutionReservation, callers)
	failures := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reservation, err := opened.ReserveIntegrationResolution(ctx, scope, input)
			if err != nil {
				failures <- err
				return
			}
			results <- reservation
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent reservation: %v", err)
	}
	for reservation := range results {
		if reservation.Attempt != 1 || reservation.MaxAttempts != 3 || reservation.Started {
			t.Errorf("concurrent reservation = %+v", reservation)
		}
	}
	var attempts, events, startedRows int
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_resolution_attempts
		WHERE run_id = ?`, claim.Run.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND run_id = ? AND kind = 'integration_resolution_started'`,
		task.Task.ID, claim.Run.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_resolution_attempts
		WHERE run_id = ? AND started_at IS NOT NULL`, claim.Run.ID).Scan(&startedRows); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || events != 0 || startedRows != 0 {
		t.Fatalf("concurrent preparation rows: attempts=%d events=%d started=%d", attempts, events, startedRows)
	}

	results = make(chan IntegrationResolutionReservation, callers)
	failures = make(chan error, callers)
	wait = sync.WaitGroup{}
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reservation, err := opened.StartIntegrationResolutionAttempt(ctx, scope, StartIntegrationResolutionInput{
				ConflictFingerprint: strings.Repeat("c", 64),
				ExpectedAttempt:     1, ExpectedMaxAttempts: 3,
			})
			if err != nil {
				failures <- err
				return
			}
			results <- reservation
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent start: %v", err)
	}
	startedNow := 0
	for reservation := range results {
		if reservation.Attempt != 1 || reservation.MaxAttempts != 3 || !reservation.Started {
			t.Errorf("concurrent start = %+v", reservation)
		}
		if reservation.StartedNow {
			startedNow++
		}
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_resolution_attempts
		WHERE run_id = ? AND started_at IS NOT NULL`, claim.Run.ID).Scan(&startedRows); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND run_id = ? AND kind = 'integration_resolution_started'`,
		task.Task.ID, claim.Run.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if startedNow != 1 || startedRows != 1 || events != 1 {
		t.Fatalf("concurrent start: transitioned=%d rows=%d events=%d", startedNow, startedRows, events)
	}
}

func TestIntegrationResolutionRequiresWritePolicyAndRefundsUnstartedProcess(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "guard resolution", Assignee: &assignee, Runtime: model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer, MaxRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	workspacePath := filepath.Join(t.TempDir(), claim.Run.ID)
	if _, err := opened.BindRunWorkspace(ctx, scope, BindRunWorkspaceInput{
		Path: workspacePath, Kind: model.WorkspaceWorktree, Generated: true,
	}); err != nil {
		t.Fatal(err)
	}
	input := ReserveIntegrationResolutionInput{
		WorkspacePath: workspacePath, PrerequisiteID: "parent", ChangeSetID: "change",
		ConflictFingerprint: strings.Repeat("d", 64),
	}
	if _, err := opened.ReserveIntegrationResolution(ctx, scope, input); err == nil ||
		!strings.Contains(err.Error(), "write permission") {
		t.Fatalf("missing-policy reservation error = %v", err)
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, false); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ReserveIntegrationResolution(ctx, scope, input); err == nil ||
		!strings.Contains(err.Error(), "write permission") {
		t.Fatalf("read-only reservation error = %v", err)
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	prepared, err := opened.ReserveIntegrationResolution(ctx, scope, input)
	if err != nil || prepared.Attempt != 1 || prepared.Started {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := opened.StartIntegrationResolutionAttempt(canceled, scope, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("d", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error = %v", err)
	}
	var startedRows, events int
	assertUnstarted := func() {
		t.Helper()
		if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_resolution_attempts
			WHERE run_id = ? AND started_at IS NOT NULL`, claim.Run.ID).Scan(&startedRows); err != nil {
			t.Fatal(err)
		}
		if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
			WHERE run_id = ? AND kind = 'integration_resolution_started'`, claim.Run.ID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if startedRows != 0 || events != 0 {
			t.Fatalf("unstarted process consumed retry budget: rows=%d events=%d", startedRows, events)
		}
	}
	assertUnstarted()
	started, err := opened.StartIntegrationResolutionAttempt(ctx, scope, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("d", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	})
	if err != nil || !started.StartedNow {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if err := opened.CompensateIntegrationResolutionStart(ctx, scope, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("d", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.CompensateIntegrationResolutionStart(ctx, scope, StartIntegrationResolutionInput{
		ConflictFingerprint: strings.Repeat("d", 64),
		ExpectedAttempt:     1, ExpectedMaxAttempts: 2,
	}); err != nil {
		t.Fatalf("idempotent compensation: %v", err)
	}
	assertUnstarted()
}

func TestLatestSchemaCreatesIntegrationResolutionLedgerForVersion20Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE integration_resolution_attempts"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 20"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version, tableCount int
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'integration_resolution_attempts'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || tableCount != 1 {
		t.Fatalf(
			"schema migration = version:%d ledger:%d, want %d and 1",
			version,
			tableCount,
			schemaVersion,
		)
	}
	var resolvedAtColumn int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM pragma_table_info('integration_resolution_attempts')
		WHERE name = 'resolved_at'`).Scan(&resolvedAtColumn); err != nil {
		t.Fatal(err)
	}
	if resolvedAtColumn != 1 {
		t.Fatalf("resolved_at columns = %d, want 1", resolvedAtColumn)
	}
}

type integrationRecoveryFixture struct {
	store       *Store
	task        model.TaskDetail
	scope       RunScope
	checkpoint  model.RecoveryCheckpoint
	workspace   model.RunWorkspace
	fingerprint string
}

func newIntegrationRecoveryFixture(
	t *testing.T,
	startResolution bool,
) integrationRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "autogora.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })

	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:        "resolve prerequisites before recovery",
		Assignee:     &assignee,
		Runtime:      model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer,
		MaxRetries:   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || source == nil {
		t.Fatalf("claim source run: claim=%+v err=%v", source, err)
	}
	countFailure := false
	checkpoint, _, err := opened.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{RunID: source.Run.ID, ClaimToken: source.ClaimToken},
		recoveryCheckpointInput(source.Run.ID, "/worktree/integration-source", 'a', 'd'),
		"source run stopped with partial work",
		FailRunOptions{
			Outcome:      model.RunStatusCrashed,
			CountFailure: &countFailure,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	claim, scope := claimRecoveryRun(t, opened, task.Task.ID)
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	repository := "/repository"
	base := recoveryCommit('a')
	workspace, err := opened.BindRunWorkspace(ctx, scope, BindRunWorkspaceInput{
		Path:           filepath.Join(t.TempDir(), claim.Run.ID),
		Kind:           model.WorkspaceWorktree,
		RepositoryPath: &repository,
		BaseCommit:     &base,
		Generated:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = reserveRecoveryCheckpoint(t, opened, scope, checkpoint)
	fingerprint := recoveryFingerprint('e')
	resolution, err := opened.ReserveIntegrationResolution(
		ctx,
		scope,
		ReserveIntegrationResolutionInput{
			WorkspacePath:       workspace.Path,
			PrerequisiteID:      "prerequisite",
			ChangeSetID:         "changeset",
			ConflictFingerprint: fingerprint,
			ConflictingFiles:    []string{"conflict.go"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if startResolution {
		started, err := opened.StartIntegrationResolutionAttempt(
			ctx,
			scope,
			StartIntegrationResolutionInput{
				ConflictFingerprint: fingerprint,
				ExpectedAttempt:     resolution.Attempt,
				ExpectedMaxAttempts: resolution.MaxAttempts,
			},
		)
		if err != nil || !started.StartedNow {
			t.Fatalf("start integration resolution: result=%+v err=%v", started, err)
		}
	}
	if _, err := opened.RequestRunCompletion(
		ctx,
		scope,
		CompletionInput{Summary: "conflicts resolved"},
	); err != nil {
		t.Fatal(err)
	}
	return integrationRecoveryFixture{
		store:       opened,
		task:        task,
		scope:       scope,
		checkpoint:  checkpoint,
		workspace:   workspace,
		fingerprint: fingerprint,
	}
}

func TestConfirmRecoveryAfterIntegrationResolutionIsAtomicAndIdempotent(t *testing.T) {
	fixture := newIntegrationRecoveryFixture(t, true)
	ctx := context.Background()
	resolvedBase := recoveryCommit('e')
	adoptedHead := recoveryCommit('f')

	confirmed, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		resolvedBase,
		adoptedHead,
	)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.State != model.RecoveryCheckpointAdopted ||
		confirmed.AdoptedOutputBaseCommit == nil ||
		*confirmed.AdoptedOutputBaseCommit != resolvedBase ||
		confirmed.AdoptedHeadCommit == nil ||
		*confirmed.AdoptedHeadCommit != adoptedHead {
		t.Fatalf("confirmed checkpoint = %+v", confirmed)
	}
	workspace, err := fixture.store.GetRunWorkspace(ctx, fixture.scope.RunID)
	if err != nil || workspace == nil || workspace.BaseCommit == nil ||
		*workspace.BaseCommit != resolvedBase {
		t.Fatalf("resolved workspace = %+v err=%v", workspace, err)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, fixture.scope.RunID)
	if err != nil || request != nil {
		t.Fatalf("resolver completion request = %+v err=%v", request, err)
	}
	var resolvedAt string
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT resolved_at
		FROM integration_resolution_attempts WHERE run_id = ?`,
		fixture.scope.RunID,
	).Scan(&resolvedAt); err != nil || strings.TrimSpace(resolvedAt) == "" {
		t.Fatalf("resolved_at = %q err=%v", resolvedAt, err)
	}
	unresolved, err := fixture.store.HasRunIntegrationResolution(
		ctx,
		fixture.scope.RunID,
	)
	if err != nil || unresolved {
		t.Fatalf("resolved handoff remains unresolved: unresolved=%t err=%v", unresolved, err)
	}
	inspection, err := fixture.store.GetRun(ctx, fixture.scope.RunID)
	if err != nil || inspection.Run.Status != model.RunStatusRunning ||
		inspection.Task.Status != model.TaskStatusRunning {
		t.Fatalf("bridge terminalized run: inspection=%+v err=%v", inspection, err)
	}

	var eventCount int
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM task_events WHERE run_id = ? AND kind IN (
			'recovery_checkpoint_adopted',
			'integration_resolution_resolved',
			'terminal_request_discarded'
		)`, fixture.scope.RunID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		resolvedBase,
		adoptedHead,
	)
	if err != nil || retried.ID != confirmed.ID || retried.AdoptedAt == nil ||
		confirmed.AdoptedAt == nil || *retried.AdoptedAt != *confirmed.AdoptedAt {
		t.Fatalf("idempotent confirmation = %+v err=%v", retried, err)
	}
	var retriedEventCount int
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM task_events WHERE run_id = ? AND kind IN (
			'recovery_checkpoint_adopted',
			'integration_resolution_resolved',
			'terminal_request_discarded'
		)`, fixture.scope.RunID).Scan(&retriedEventCount); err != nil {
		t.Fatal(err)
	}
	if retriedEventCount != eventCount {
		t.Fatalf("idempotent retry appended events: before=%d after=%d", eventCount, retriedEventCount)
	}
	if _, err := fixture.store.StartIntegrationResolutionAttempt(
		ctx,
		fixture.scope,
		StartIntegrationResolutionInput{
			ConflictFingerprint: fixture.fingerprint,
			ExpectedAttempt:     1,
			ExpectedMaxAttempts: 3,
		},
	); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("resolved integration restarted: %v", err)
	}
	if _, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		recoveryCommit('9'),
		adoptedHead,
	); err == nil || !strings.Contains(err.Error(), "different recovery state") {
		t.Fatalf("different confirmation retry error = %v", err)
	}
	if _, err := fixture.store.RequestRunCompletion(
		ctx,
		fixture.scope,
		CompletionInput{Summary: "Finalizer verified the resolved graph"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		resolvedBase,
		adoptedHead,
	); err == nil || !strings.Contains(err.Error(), "new terminal request") {
		t.Fatalf("bridge retry discarded Finalizer completion: %v", err)
	}
	if _, err := fixture.store.RecordRunChangeSet(
		ctx,
		fixture.scope,
		RecordChangeSetInput{
			RunID:          fixture.scope.RunID,
			RepositoryPath: "/repository",
			WorktreePath:   fixture.workspace.Path,
			BaseCommit:     resolvedBase,
			HeadCommit:     adoptedHead,
			DurableRef:     "refs/autogora/results/" + fixture.scope.RunID,
			State:          "ready",
		},
	); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.FinalizeRunTerminal(
		ctx,
		fixture.scope,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := fixture.store.GetRecoveryCheckpoint(
		ctx,
		fixture.checkpoint.ID,
	)
	if err != nil || consumed == nil ||
		consumed.State != model.RecoveryCheckpointConsumed ||
		completed.Task.Status != model.TaskStatusDone {
		t.Fatalf("verified completion: task=%s checkpoint=%+v err=%v", completed.Task.Status, consumed, err)
	}
}

func TestConfirmedIntegrationRecoveryBaseIsImmutableThroughFinalizeAndReopen(t *testing.T) {
	fixture := newIntegrationRecoveryFixture(t, true)
	ctx := context.Background()
	resolvedBase := recoveryCommit('e')
	adoptedHead := recoveryCommit('f')
	if _, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		resolvedBase,
		adoptedHead,
	); err != nil {
		t.Fatal(err)
	}

	unchanged, err := fixture.store.UpdateRunWorkspaceBase(
		ctx,
		fixture.scope,
		resolvedBase,
	)
	if err != nil || unchanged.BaseCommit == nil ||
		*unchanged.BaseCommit != resolvedBase {
		t.Fatalf("idempotent base update: workspace=%+v err=%v", unchanged, err)
	}
	if _, err := fixture.store.UpdateRunWorkspaceBase(
		ctx,
		fixture.scope,
		recoveryCommit('9'),
	); err == nil || !strings.Contains(
		err.Error(),
		"immutable after integration recovery confirmation",
	) {
		t.Fatalf("confirmed base application update error = %v", err)
	}
	if _, err := fixture.store.db.ExecContext(ctx, `UPDATE run_workspaces
		SET base_commit = ? WHERE run_id = ?`,
		recoveryCommit('8'),
		fixture.scope.RunID,
	); err == nil || !strings.Contains(
		err.Error(),
		"immutable after integration recovery confirmation",
	) {
		t.Fatalf("confirmed base direct update error = %v", err)
	}
	workspace, err := fixture.store.GetRunWorkspace(ctx, fixture.scope.RunID)
	if err != nil || workspace == nil || workspace.BaseCommit == nil ||
		*workspace.BaseCommit != resolvedBase {
		t.Fatalf("rejected updates changed workspace: workspace=%+v err=%v", workspace, err)
	}

	if _, err := fixture.store.RequestRunCompletion(
		ctx,
		fixture.scope,
		CompletionInput{Summary: "Finalizer verified the immutable bridge"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecordRunChangeSet(
		ctx,
		fixture.scope,
		RecordChangeSetInput{
			RunID:          fixture.scope.RunID,
			RepositoryPath: "/repository",
			WorktreePath:   fixture.workspace.Path,
			BaseCommit:     resolvedBase,
			HeadCommit:     adoptedHead,
			DurableRef:     "refs/autogora/results/" + fixture.scope.RunID,
			State:          "ready",
		},
	); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.FinalizeRunTerminal(
		ctx,
		fixture.scope,
		0,
	)
	if err != nil || completed.Task.Status != model.TaskStatusDone {
		t.Fatalf("finalize confirmed bridge: task=%s err=%v", completed.Task.Status, err)
	}

	dbPath := fixture.store.DBPath()
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatalf("reopen finalized bridge: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.db.ExecContext(ctx, `UPDATE run_workspaces
		SET base_commit = ? WHERE run_id = ?`,
		recoveryCommit('7'),
		fixture.scope.RunID,
	); err == nil || !strings.Contains(
		err.Error(),
		"immutable after integration recovery confirmation",
	) {
		t.Fatalf("reopened confirmed base direct update error = %v", err)
	}
	reopenedWorkspace, err := reopened.GetRunWorkspace(ctx, fixture.scope.RunID)
	if err != nil || reopenedWorkspace == nil ||
		reopenedWorkspace.BaseCommit == nil ||
		*reopenedWorkspace.BaseCommit != resolvedBase {
		t.Fatalf("reopened workspace: workspace=%+v err=%v", reopenedWorkspace, err)
	}
	reopenedCheckpoint, err := reopened.GetRecoveryCheckpoint(
		ctx,
		fixture.checkpoint.ID,
	)
	if err != nil || reopenedCheckpoint == nil ||
		reopenedCheckpoint.State != model.RecoveryCheckpointConsumed {
		t.Fatalf("reopened checkpoint: checkpoint=%+v err=%v", reopenedCheckpoint, err)
	}
	reopenedTask, err := reopened.GetTask(ctx, fixture.task.Task.ID)
	if err != nil || reopenedTask.Task.Status != model.TaskStatusDone {
		t.Fatalf("reopened task: task=%+v err=%v", reopenedTask.Task, err)
	}
}

func TestConfirmRecoveryAfterIntegrationResolutionRollsBackLateFailure(t *testing.T) {
	fixture := newIntegrationRecoveryFixture(t, true)
	ctx := context.Background()
	if _, err := fixture.store.db.ExecContext(ctx, `CREATE TRIGGER reject_resolution_request_delete
		BEFORE DELETE ON run_terminal_requests
		WHEN OLD.run_id = '`+fixture.scope.RunID+`'
		BEGIN
			SELECT RAISE(ABORT, 'test resolution rollback');
		END`); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
		ctx,
		fixture.scope,
		fixture.checkpoint.ID,
		fixture.checkpoint.ReservationToken,
		recoveryCommit('e'),
		recoveryCommit('f'),
	)
	if err == nil || !strings.Contains(err.Error(), "test resolution rollback") {
		t.Fatalf("forced bridge error = %v", err)
	}
	checkpoint, err := fixture.store.GetRecoveryCheckpoint(ctx, fixture.checkpoint.ID)
	if err != nil || checkpoint == nil ||
		checkpoint.State != model.RecoveryCheckpointReserved {
		t.Fatalf("checkpoint escaped rollback: checkpoint=%+v err=%v", checkpoint, err)
	}
	workspace, err := fixture.store.GetRunWorkspace(ctx, fixture.scope.RunID)
	if err != nil || workspace == nil || workspace.BaseCommit == nil ||
		*workspace.BaseCommit != *fixture.workspace.BaseCommit {
		t.Fatalf("workspace escaped rollback: workspace=%+v err=%v", workspace, err)
	}
	var resolvedAt sql.NullString
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT resolved_at
		FROM integration_resolution_attempts WHERE run_id = ?`,
		fixture.scope.RunID,
	).Scan(&resolvedAt); err != nil {
		t.Fatal(err)
	}
	request, err := fixture.store.GetRunTerminalRequest(ctx, fixture.scope.RunID)
	if err != nil || request == nil || request.Kind != "complete" ||
		resolvedAt.Valid {
		t.Fatalf("resolution escaped rollback: request=%+v resolved=%+v err=%v", request, resolvedAt, err)
	}
	var transitionEvents int
	if err := fixture.store.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM task_events WHERE run_id = ? AND kind IN (
			'recovery_checkpoint_adopted',
			'integration_resolution_resolved',
			'terminal_request_discarded'
		)`, fixture.scope.RunID).Scan(&transitionEvents); err != nil {
		t.Fatal(err)
	}
	if transitionEvents != 0 {
		t.Fatalf("rolled-back transition events = %d", transitionEvents)
	}
}

func TestConfirmRecoveryAfterIntegrationResolutionRejectsUnsafeBoundaries(t *testing.T) {
	t.Run("unstarted", func(t *testing.T) {
		fixture := newIntegrationRecoveryFixture(t, false)
		_, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
			context.Background(),
			fixture.scope,
			fixture.checkpoint.ID,
			fixture.checkpoint.ReservationToken,
			recoveryCommit('e'),
			recoveryCommit('f'),
		)
		if err == nil || !strings.Contains(err.Error(), "process-start boundary") {
			t.Fatalf("unstarted confirmation error = %v", err)
		}
	})

	t.Run("immutable change set", func(t *testing.T) {
		fixture := newIntegrationRecoveryFixture(t, true)
		if _, err := fixture.store.RecordRunChangeSet(
			context.Background(),
			fixture.scope,
			RecordChangeSetInput{
				RunID:          fixture.scope.RunID,
				RepositoryPath: "/repository",
				WorktreePath:   fixture.workspace.Path,
				BaseCommit:     recoveryCommit('a'),
				HeadCommit:     recoveryCommit('e'),
				DurableRef:     "refs/autogora/results/" + fixture.scope.RunID,
				State:          "ready",
			},
		); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.store.ConfirmRecoveryAfterIntegrationResolution(
			context.Background(),
			fixture.scope,
			fixture.checkpoint.ID,
			fixture.checkpoint.ReservationToken,
			recoveryCommit('e'),
			recoveryCommit('f'),
		)
		if err == nil || !strings.Contains(err.Error(), "immutable change set") {
			t.Fatalf("captured change set confirmation error = %v", err)
		}
	})

	t.Run("ordinary adoption API", func(t *testing.T) {
		fixture := newIntegrationRecoveryFixture(t, true)
		_, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
			context.Background(),
			fixture.scope,
			fixture.checkpoint.ID,
			fixture.checkpoint.ReservationToken,
			recoveryCommit('e'),
			recoveryCommit('f'),
		)
		if err == nil || !strings.Contains(err.Error(), "atomic resolution confirmation") {
			t.Fatalf("ordinary adoption error = %v", err)
		}
	})
}

func TestSchema24AddsResolvedAtWithoutLosingStartedIntegrationAttempt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:        "preserve integration attempt",
		Assignee:     &assignee,
		Runtime:      model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer,
		MaxRetries:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: claim=%+v err=%v", claim, err)
	}
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	workspacePath := filepath.Join(t.TempDir(), claim.Run.ID)
	if _, err := opened.BindRunWorkspace(ctx, scope, BindRunWorkspaceInput{
		Path: workspacePath, Kind: model.WorkspaceWorktree, Generated: true,
	}); err != nil {
		t.Fatal(err)
	}
	fingerprint := recoveryFingerprint('8')
	reservation, err := opened.ReserveIntegrationResolution(
		ctx,
		scope,
		ReserveIntegrationResolutionInput{
			WorkspacePath:       workspacePath,
			PrerequisiteID:      "parent",
			ChangeSetID:         "change",
			ConflictFingerprint: fingerprint,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.StartIntegrationResolutionAttempt(
		ctx,
		scope,
		StartIntegrationResolutionInput{
			ConflictFingerprint: fingerprint,
			ExpectedAttempt:     reservation.Attempt,
			ExpectedMaxAttempts: reservation.MaxAttempts,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeIntegrationResolutionSchemaToV23(t, path)

	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var attempt, version int
	var startedAt string
	var resolvedAt sql.NullString
	if err := reopened.db.QueryRowContext(ctx, `SELECT attempt, started_at, resolved_at
		FROM integration_resolution_attempts WHERE run_id = ?`,
		claim.Run.ID,
	).Scan(&attempt, &startedAt, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if attempt != 1 || strings.TrimSpace(startedAt) == "" ||
		resolvedAt.Valid || version != schemaVersion {
		t.Fatalf(
			"migrated attempt: attempt=%d started=%q resolved=%+v version=%d",
			attempt,
			startedAt,
			resolvedAt,
			version,
		)
	}
}

func TestSchema24ConcurrentOpenSerializesResolvedAtMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeIntegrationResolutionSchemaToV23(t, path)

	type openResult struct {
		store *Store
		err   error
	}
	const callers = 8
	start := make(chan struct{})
	results := make(chan openResult, callers)
	for range callers {
		go func() {
			<-start
			opened, err := Open(path, "default", "")
			results <- openResult{store: opened, err: err}
		}()
	}
	close(start)

	stores := make([]*Store, 0, callers)
	var openErr error
	for range callers {
		result := <-results
		if result.err != nil {
			openErr = errors.Join(openErr, result.err)
			continue
		}
		stores = append(stores, result.store)
	}
	defer func() {
		for _, opened := range stores {
			_ = opened.Close()
		}
	}()
	if openErr != nil {
		t.Fatalf("concurrent v23 open: %v", openErr)
	}

	ctx := context.Background()
	var version, resolvedAtColumns, invariantTriggers int
	if err := stores[0].db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM pragma_table_info('integration_resolution_attempts')
		WHERE name = 'resolved_at'`).Scan(&resolvedAtColumns); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'trigger' AND name IN (
			'integration_resolution_insert_requires_started_recovery',
			'integration_resolution_update_requires_started_recovery',
			'run_workspace_prevent_confirmed_integration_base_change'
		)`).Scan(&invariantTriggers); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion || resolvedAtColumns != 1 ||
		invariantTriggers != 3 {
		t.Fatalf(
			"concurrent migration: version=%d columns=%d triggers=%d",
			version,
			resolvedAtColumns,
			invariantTriggers,
		)
	}
}

func downgradeIntegrationResolutionSchemaToV23(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		DROP TRIGGER IF EXISTS integration_resolution_insert_requires_started_recovery;
		DROP TRIGGER IF EXISTS integration_resolution_update_requires_started_recovery;
		DROP TRIGGER IF EXISTS run_workspace_prevent_confirmed_integration_base_change;
		DROP INDEX IF EXISTS idx_integration_resolution_attempts_task;
		ALTER TABLE integration_resolution_attempts
			RENAME TO integration_resolution_attempts_v24;
		CREATE TABLE integration_resolution_attempts (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			conflict_fingerprint TEXT NOT NULL
				CHECK (length(conflict_fingerprint) = 64
					AND conflict_fingerprint NOT GLOB '*[^0-9a-f]*'),
			run_id TEXT NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
			attempt INTEGER CHECK (attempt IS NULL OR attempt >= 1),
			max_attempts INTEGER NOT NULL CHECK (max_attempts >= 1),
			workspace_path TEXT NOT NULL,
			prerequisite_id TEXT NOT NULL,
			change_set_id TEXT NOT NULL,
			conflicting_files_json TEXT NOT NULL DEFAULT '[]',
			prepared_at TEXT NOT NULL,
			started_at TEXT,
			CHECK (
				(attempt IS NULL AND started_at IS NULL)
				OR (attempt IS NOT NULL AND started_at IS NOT NULL)
			),
			CHECK (attempt IS NULL OR attempt <= max_attempts),
			PRIMARY KEY (task_id, conflict_fingerprint, run_id),
			UNIQUE (task_id, conflict_fingerprint, attempt)
		);
		INSERT INTO integration_resolution_attempts(
			task_id, conflict_fingerprint, run_id, attempt, max_attempts,
			workspace_path, prerequisite_id, change_set_id,
			conflicting_files_json, prepared_at, started_at
		)
		SELECT task_id, conflict_fingerprint, run_id, attempt, max_attempts,
			workspace_path, prerequisite_id, change_set_id,
			conflicting_files_json, prepared_at, started_at
		FROM integration_resolution_attempts_v24;
		DROP TABLE integration_resolution_attempts_v24;
		CREATE INDEX idx_integration_resolution_attempts_task
			ON integration_resolution_attempts(
				task_id, conflict_fingerprint, attempt DESC
			);
		PRAGMA user_version = 23;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}
