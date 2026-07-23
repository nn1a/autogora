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
		if _, err := opened.FinalizeRunTerminal(ctx, claim.Run.ID, 0); err != nil {
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

func TestSchema21CreatesIntegrationResolutionLedgerForVersion20Database(t *testing.T) {
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
	if version != 21 || tableCount != 1 {
		t.Fatalf("schema migration = version:%d ledger:%d, want 21 and 1", version, tableCount)
	}
}
