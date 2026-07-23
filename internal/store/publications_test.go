package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func createPublicationSource(
	t *testing.T,
	opened *Store,
	suffix string,
	role model.WorkflowRole,
	status model.TaskStatus,
	runStatus model.RunStatus,
	changeState string,
) (model.Task, model.ChangeSet) {
	t.Helper()
	ctx := context.Background()
	detail, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "publication source " + suffix, WorkflowRole: role, Status: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := detail.Task
	runID := "r_pub_" + suffix
	timestamp := now()
	_, err = opened.db.ExecContext(ctx, `
		INSERT INTO task_runs(
			id, task_id, worker_id, runtime, status, claim_token, claimed_at,
			claim_expires_at, heartbeat_at, ended_at, metadata_json
		) VALUES (?, ?, 'publisher-test', 'manual', ?, 'retired-token', ?, ?, ?, ?, '{}')
	`, runID, task.ID, runStatus, timestamp, timestamp, timestamp, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	changeSet := model.ChangeSet{
		ID: "cs_pub_" + suffix, RunID: runID, TaskID: task.ID,
		RepositoryPath: "/repo/" + suffix, WorktreePath: "/worktree/" + suffix,
		BaseCommit: "base-" + suffix, HeadCommit: "head-" + suffix,
		DurableRef: "refs/autogora/runs/" + runID, State: changeState,
		ChangedFiles: []string{"a-" + suffix + ".go", "b-" + suffix + ".md"},
		CreatedAt:    timestamp,
	}
	changedFiles, err := json.Marshal(changeSet.ChangedFiles)
	if err != nil {
		t.Fatal(err)
	}
	_, err = opened.db.ExecContext(ctx, `
		INSERT INTO task_change_sets(
			id, run_id, task_id, repository_path, worktree_path, base_commit,
			head_commit, durable_ref, state, changed_files_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, changeSet.ID, changeSet.RunID, changeSet.TaskID, changeSet.RepositoryPath,
		changeSet.WorktreePath, changeSet.BaseCommit, changeSet.HeadCommit,
		changeSet.DurableRef, changeSet.State, string(changedFiles), changeSet.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	return task, changeSet
}

func publicationPolicyInput(changeSetID string, approval bool) EnsurePublicationInput {
	return EnsurePublicationInput{
		ChangeSetID: changeSetID, Mode: model.PublicationModePullRequest,
		TargetBranch: "main", Remote: "origin", RequireApproval: approval,
		PolicySnapshot: json.RawMessage(`{
			"revision":"initial",
			"autopilot":{"publication":{"mode":"pull_request","targetBranch":"main","remote":"origin"}}
		}`),
	}
}

func TestEnsurePublicationPreservesPolicyAndSourceSnapshots(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, changeSet := createPublicationSource(
		t, opened, "snapshot", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)

	input := publicationPolicyInput(changeSet.ID, false)
	first, created, err := opened.EnsurePublication(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.Status != model.PublicationPending ||
		first.TaskID != task.ID || first.RunID != changeSet.RunID ||
		first.ChangeSetID != changeSet.ID || first.Mode != model.PublicationModePullRequest ||
		first.TargetBranch != "main" || first.Remote != "origin" ||
		first.BaseCommit != changeSet.BaseCommit || first.HeadCommit != changeSet.HeadCommit ||
		first.DurableRef != changeSet.DurableRef {
		t.Fatalf("unexpected publication: %+v", first)
	}
	var policy map[string]any
	if err := json.Unmarshal(first.PolicySnapshot, &policy); err != nil {
		t.Fatal(err)
	}
	if policy["revision"] != "initial" {
		t.Fatalf("policy snapshot = %s", first.PolicySnapshot)
	}
	var source struct {
		Board        string             `json:"board"`
		WorkflowRole model.WorkflowRole `json:"workflowRole"`
		TaskStatus   model.TaskStatus   `json:"taskStatus"`
		RunStatus    model.RunStatus    `json:"runStatus"`
		ChangeSet    model.ChangeSet    `json:"changeSet"`
	}
	if err := json.Unmarshal(first.SourceSnapshot, &source); err != nil {
		t.Fatal(err)
	}
	if source.Board != "default" || source.WorkflowRole != model.WorkflowRoleFinalizer ||
		source.TaskStatus != model.TaskStatusDone || source.RunStatus != model.RunStatusCompleted ||
		source.ChangeSet.ID != changeSet.ID ||
		len(source.ChangeSet.ChangedFiles) != len(changeSet.ChangedFiles) {
		t.Fatalf("source snapshot = %+v", source)
	}

	changed := EnsurePublicationInput{
		ID: "pub_reconfigured", ChangeSetID: changeSet.ID,
		Mode: model.PublicationModeLocalFF, TargetBranch: "release",
		Remote: "upstream", RequireApproval: true,
		PolicySnapshot: json.RawMessage(`{"revision":"changed"}`),
	}
	second, created, err := opened.EnsurePublication(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID || second.Mode != first.Mode ||
		second.TargetBranch != first.TargetBranch || second.Remote != first.Remote ||
		second.RequireApproval != first.RequireApproval ||
		string(second.PolicySnapshot) != string(first.PolicySnapshot) ||
		string(second.SourceSnapshot) != string(first.SourceSnapshot) {
		t.Fatalf("idempotent ensure replaced snapshots:\nfirst=%+v\nsecond=%+v", first, second)
	}

	byChangeSet, err := opened.GetPublicationByChangeSet(ctx, changeSet.ID)
	if err != nil || byChangeSet.ID != first.ID {
		t.Fatalf("get by change set: value=%+v err=%v", byChangeSet, err)
	}
	listed, err := opened.ListPublications(ctx, PublicationFilter{
		TaskID: task.ID, RunID: changeSet.RunID, Status: model.PublicationPending,
	})
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("list publications: values=%+v err=%v", listed, err)
	}
}

func TestEnsurePublicationNoChangeIsTerminal(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, changeSet := createPublicationSource(
		t, opened, "no_change", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "no_change",
	)

	value, created, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(changeSet.ID, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || value.Status != model.PublicationNoChange ||
		value.PublishedAt == nil || value.ClaimExpiresAt != nil {
		t.Fatalf("no-change publication = %+v", value)
	}
	if _, claimed, err := opened.ClaimPublication(ctx, value.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: value.UpdatedAt, TTL: time.Minute,
	}); !errors.Is(err, ErrPublicationStateConflict) || claimed {
		t.Fatalf("claim no-change: claimed=%v err=%v", claimed, err)
	}
}

func TestPublicationApprovalAndSupersedeUseUpdatedAtCAS(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, approvedSource := createPublicationSource(
		t, opened, "approval", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	awaiting, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(approvedSource.ID, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if awaiting.Status != model.PublicationAwaitingApproval {
		t.Fatalf("awaiting publication = %+v", awaiting)
	}
	approved, err := opened.ApprovePublication(ctx, awaiting.ID, ApprovePublicationInput{
		ExpectedUpdatedAt: awaiting.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != model.PublicationPending || approved.ApprovedAt == nil ||
		approved.UpdatedAt == awaiting.UpdatedAt {
		t.Fatalf("approved publication = %+v", approved)
	}
	if _, err := opened.ApprovePublication(ctx, awaiting.ID, ApprovePublicationInput{
		ExpectedUpdatedAt: awaiting.UpdatedAt,
	}); !errors.Is(err, ErrPublicationUpdateConflict) {
		t.Fatalf("stale approval error = %v", err)
	}

	_, rejectedSource := createPublicationSource(
		t, opened, "rejected", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	rejected, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(rejectedSource.ID, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err = opened.SupersedePublication(ctx, rejected.ID, SupersedePublicationInput{
		ExpectedUpdatedAt: rejected.UpdatedAt, Reason: "operator rejected publication",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != model.PublicationSuperseded || rejected.Error == nil ||
		*rejected.Error != "operator rejected publication" {
		t.Fatalf("superseded publication = %+v", rejected)
	}
	if _, err := opened.ApprovePublication(ctx, rejected.ID, ApprovePublicationInput{
		ExpectedUpdatedAt: rejected.UpdatedAt,
	}); !errors.Is(err, ErrPublicationStateConflict) {
		t.Fatalf("approve superseded error = %v", err)
	}
}

func TestPublicationClaimCompleteFailAndExpiredTakeover(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	baseTime := time.Date(2026, 7, 24, 4, 0, 0, 0, time.UTC)

	_, completedSource := createPublicationSource(
		t, opened, "complete", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(completedSource.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, acquired, err := opened.ClaimPublication(ctx, pending.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: pending.UpdatedAt, TTL: time.Minute, Current: baseTime,
	})
	if err != nil || !acquired || claimed.Status != model.PublicationPublishing ||
		claimed.ClaimToken == "" || claimed.ClaimExpiresAt == nil {
		t.Fatalf("claim publication: value=%+v acquired=%v err=%v", claimed, acquired, err)
	}
	visible, err := opened.GetPublication(ctx, claimed.ID)
	if err != nil || visible.ClaimToken != "" {
		t.Fatalf("public claim leaked token: value=%+v err=%v", visible, err)
	}
	encoded, err := json.Marshal(claimed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), claimed.ClaimToken) {
		t.Fatalf("JSON leaked claim token: %s", encoded)
	}
	contended, acquired, err := opened.ClaimPublication(ctx, claimed.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, TTL: time.Minute,
		Current: baseTime.Add(time.Second),
	})
	if err != nil || acquired || contended.ClaimToken != "" ||
		contended.Status != model.PublicationPublishing {
		t.Fatalf("contended claim: value=%+v acquired=%v err=%v", contended, acquired, err)
	}
	if _, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: "wrong-token",
		Current: baseTime.Add(2 * time.Second),
	}); !errors.Is(err, ErrPublicationClaimNotOwner) {
		t.Fatalf("wrong owner completion error = %v", err)
	}
	rawURL := "https://example.test/pull/17"
	completed, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: claimed.ClaimToken,
		Current: baseTime.Add(2 * time.Second), URL: &rawURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != model.PublicationPublished || completed.URL == nil ||
		*completed.URL != rawURL || completed.PublishedAt == nil ||
		completed.ClaimToken != "" || completed.ClaimExpiresAt != nil {
		t.Fatalf("completed publication = %+v", completed)
	}

	_, retrySource := createPublicationSource(
		t, opened, "retry", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	retry, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(retrySource.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	retryClaim, acquired, err := opened.ClaimPublication(ctx, retry.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: retry.UpdatedAt, TTL: time.Minute, Current: baseTime,
	})
	if err != nil || !acquired {
		t.Fatalf("claim retry source: acquired=%v err=%v", acquired, err)
	}
	failed, err := opened.FailPublication(ctx, retry.ID, FailPublicationInput{
		ExpectedUpdatedAt: retryClaim.UpdatedAt, ClaimToken: retryClaim.ClaimToken,
		Current: baseTime.Add(time.Second), Error: "remote temporarily unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.PublicationFailed || failed.Error == nil ||
		*failed.Error != "remote temporarily unavailable" {
		t.Fatalf("failed publication = %+v", failed)
	}
	retryClaim, acquired, err = opened.ClaimPublication(ctx, retry.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: failed.UpdatedAt, TTL: time.Minute,
		Current: baseTime.Add(2 * time.Second),
	})
	if err != nil || !acquired || retryClaim.Error != nil {
		t.Fatalf("retry claim: value=%+v acquired=%v err=%v", retryClaim, acquired, err)
	}

	_, takeoverSource := createPublicationSource(
		t, opened, "takeover", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	takeover, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(takeoverSource.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim, acquired, err := opened.ClaimPublication(ctx, takeover.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: takeover.UpdatedAt, TTL: MinPublicationClaimTTL,
		Current: baseTime,
	})
	if err != nil || !acquired {
		t.Fatalf("old claim: acquired=%v err=%v", acquired, err)
	}
	newClaim, acquired, err := opened.ClaimPublication(ctx, takeover.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: oldClaim.UpdatedAt, TTL: time.Minute,
		Current: baseTime.Add(MinPublicationClaimTTL),
	})
	if err != nil || !acquired || newClaim.ClaimToken == oldClaim.ClaimToken {
		t.Fatalf("takeover claim: value=%+v acquired=%v err=%v", newClaim, acquired, err)
	}
	if _, err := opened.FailPublication(ctx, takeover.ID, FailPublicationInput{
		ExpectedUpdatedAt: newClaim.UpdatedAt, ClaimToken: oldClaim.ClaimToken,
		Current: baseTime.Add(MinPublicationClaimTTL + time.Second),
		Error:   "stale worker",
	}); !errors.Is(err, ErrPublicationClaimNotOwner) {
		t.Fatalf("stale claimant error = %v", err)
	}
}

func TestEnsurePublicationValidatesFinalizerCompletionAndBoardScope(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	_, workerChange := createPublicationSource(
		t, opened, "worker", model.WorkflowRoleWorker, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	if _, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(workerChange.ID, false),
	); err == nil || !strings.Contains(err.Error(), "is not a finalizer") {
		t.Fatalf("worker publication error = %v", err)
	}
	_, openChange := createPublicationSource(
		t, opened, "open", model.WorkflowRoleFinalizer, model.TaskStatusTodo,
		model.RunStatusCompleted, "ready",
	)
	if _, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(openChange.ID, false),
	); err == nil || !strings.Contains(err.Error(), "expected done") {
		t.Fatalf("unfinished finalizer error = %v", err)
	}
	_, failedChange := createPublicationSource(
		t, opened, "failed_run", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusFailed, "ready",
	)
	if _, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(failedChange.ID, false),
	); err == nil || !strings.Contains(err.Error(), "completed finalizer run") {
		t.Fatalf("failed finalizer run error = %v", err)
	}
	_, validChange := createPublicationSource(
		t, opened, "valid", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)
	wrongBoard := publicationPolicyInput(validChange.ID, false)
	wrongBoard.Board = "another"
	if _, _, err := opened.EnsurePublication(ctx, wrongBoard); err == nil ||
		!strings.Contains(err.Error(), "does not match store board") {
		t.Fatalf("cross-board ensure error = %v", err)
	}
	if _, err := opened.ListPublications(ctx, PublicationFilter{Board: "another"}); err == nil ||
		!strings.Contains(err.Error(), "does not match store board") {
		t.Fatalf("cross-board list error = %v", err)
	}
	invalidPolicy := publicationPolicyInput(validChange.ID, false)
	invalidPolicy.PolicySnapshot = json.RawMessage(`[]`)
	if _, _, err := opened.EnsurePublication(ctx, invalidPolicy); err == nil ||
		!strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("invalid policy snapshot error = %v", err)
	}
}

func TestEnsurePublicationIsConcurrentAndIdempotent(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "concurrent.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	_, changeSet := createPublicationSource(
		t, opened, "concurrent", model.WorkflowRoleFinalizer, model.TaskStatusDone,
		model.RunStatusCompleted, "ready",
	)

	const callers = 12
	var created atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	ids := make(chan string, callers)
	for index := 0; index < callers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			input := publicationPolicyInput(changeSet.ID, false)
			input.ID = fmt.Sprintf("pub_concurrent_%02d", index)
			value, wasCreated, err := opened.EnsurePublication(ctx, input)
			if err != nil {
				errs <- err
				return
			}
			if wasCreated {
				created.Add(1)
			}
			ids <- value.ID
		}(index)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d, want 1", created.Load())
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("idempotent ensure returned IDs %s and %s", first, id)
		}
	}
}

func TestExistingDatabaseEnsuresPublicationSchema(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "preserve existing task"})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TABLE publications"); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetTask(ctx, task.Task.ID); err != nil {
		t.Fatalf("existing task was not preserved: %v", err)
	}
	var tableName string
	if err := reopened.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'publications'",
	).Scan(&tableName); err != nil {
		t.Fatalf("publication table was not ensured: %v", err)
	}
}
