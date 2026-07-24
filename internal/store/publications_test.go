package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func TestPublicationExplicitRetryAndManualCompletion(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	baseTime := time.Date(2026, 7, 24, 6, 0, 0, 0, time.UTC)
	publicationNow := baseTime
	opened.publicationClock = func() time.Time { return publicationNow }

	_, retrySource := createPublicationSource(
		t, opened, "explicit_retry", model.WorkflowRoleFinalizer,
		model.TaskStatusDone, model.RunStatusCompleted, "ready",
	)
	retryPublication, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(retrySource.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, claimed, err := opened.ClaimPublication(
		ctx, retryPublication.ID, ClaimPublicationInput{
			ExpectedUpdatedAt: retryPublication.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if err != nil || !claimed {
		t.Fatalf("claim retry publication: claimed=%v err=%v", claimed, err)
	}
	publicationNow = baseTime.Add(time.Second)
	failed, err := opened.FailPublication(ctx, retryPublication.ID, FailPublicationInput{
		ExpectedUpdatedAt: claim.UpdatedAt, ClaimToken: claim.ClaimToken,
		ClaimEpoch: claim.ClaimEpoch,
		Error:      "review the remote before retrying",
	})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := opened.RetryPublication(ctx, failed.ID, RetryPublicationInput{
		ExpectedUpdatedAt: failed.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != model.PublicationPending || retried.Error != nil {
		t.Fatalf("retried publication = %+v", retried)
	}
	if _, err := opened.RetryPublication(ctx, failed.ID, RetryPublicationInput{
		ExpectedUpdatedAt: failed.UpdatedAt,
	}); !errors.Is(err, ErrPublicationUpdateConflict) {
		t.Fatalf("stale retry error = %v", err)
	}

	_, manualSource := createPublicationSource(
		t, opened, "manual_complete", model.WorkflowRoleFinalizer,
		model.TaskStatusDone, model.RunStatusCompleted, "ready",
	)
	manualInput := publicationPolicyInput(manualSource.ID, true)
	manualInput.Mode = model.PublicationModeManual
	manual, _, err := opened.EnsurePublication(ctx, manualInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CompleteManualPublication(
		ctx, manual.ID, CompleteManualPublicationInput{
			ExpectedUpdatedAt: manual.UpdatedAt,
		},
	); !errors.Is(err, ErrPublicationStateConflict) {
		t.Fatalf("unapproved manual completion error = %v", err)
	}
	manual, err = opened.ApprovePublication(ctx, manual.ID, ApprovePublicationInput{
		ExpectedUpdatedAt: manual.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	url := "https://example.test/manual-release"
	manual, err = opened.CompleteManualPublication(
		ctx, manual.ID, CompleteManualPublicationInput{
			ExpectedUpdatedAt: manual.UpdatedAt, URL: &url,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != model.PublicationPublished ||
		manual.URL == nil || *manual.URL != url ||
		manual.PublishedAt == nil {
		t.Fatalf("manual publication = %+v", manual)
	}

	if _, err := opened.CompleteManualPublication(
		ctx, retried.ID, CompleteManualPublicationInput{
			ExpectedUpdatedAt: retried.UpdatedAt,
		},
	); err == nil || !strings.Contains(err.Error(), "requires manual publication mode") {
		t.Fatalf("automated publication manual completion error = %v", err)
	}
}

func TestPublicationClaimEpochCompleteFailAndNoExpiredTakeover(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	baseTime := time.Date(2026, 7, 24, 4, 0, 0, 0, time.UTC)
	publicationNow := baseTime
	opened.publicationClock = func() time.Time { return publicationNow }

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
		ExpectedUpdatedAt: pending.UpdatedAt, TTL: time.Minute,
	})
	if err != nil || !acquired || claimed.Status != model.PublicationPublishing ||
		claimed.ClaimToken == "" || claimed.ClaimExpiresAt == nil ||
		claimed.ClaimEpoch != 1 {
		t.Fatalf("claim publication: value=%+v acquired=%v err=%v", claimed, acquired, err)
	}
	visible, err := opened.GetPublication(ctx, claimed.ID)
	if err != nil || visible.ClaimToken != "" || visible.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf("public claim leaked token: value=%+v err=%v", visible, err)
	}
	encoded, err := json.Marshal(claimed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), claimed.ClaimToken) {
		t.Fatalf("JSON leaked claim token: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"claimEpoch":1`) {
		t.Fatalf("JSON omitted public claim epoch: %s", encoded)
	}
	formatted := fmt.Sprintf("%+v\n%#v", claimed, claimed)
	if strings.Contains(formatted, claimed.ClaimToken) ||
		!strings.Contains(formatted, "[REDACTED]") {
		t.Fatalf("formatted publication exposed its claim token: %s", formatted)
	}
	events, err := opened.ListEvents(ctx, EventFilter{
		TaskID: completedSource.TaskID,
		Kinds:  []string{"publication_claimed"},
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("claim events = %#v, err=%v", events, err)
	}
	var claimPayload struct {
		ClaimEpoch int64 `json:"claimEpoch"`
	}
	if err := json.Unmarshal(events[0].Payload, &claimPayload); err != nil {
		t.Fatal(err)
	}
	if claimPayload.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf("claim event epoch = %d, want %d", claimPayload.ClaimEpoch, claimed.ClaimEpoch)
	}
	contended, acquired, err := opened.ClaimPublication(ctx, claimed.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: pending.UpdatedAt, TTL: time.Minute,
	})
	if err != nil || acquired || contended.ClaimToken != "" ||
		contended.Status != model.PublicationPublishing ||
		contended.ClaimEpoch != claimed.ClaimEpoch ||
		contended.UpdatedAt != claimed.UpdatedAt {
		t.Fatalf("contended claim: value=%+v acquired=%v err=%v", contended, acquired, err)
	}
	publicationNow = baseTime.Add(2 * time.Second)
	if _, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: claimed.ClaimToken,
	}); err == nil || !strings.Contains(err.Error(), "claim epoch must be positive") {
		t.Fatalf("missing claim epoch completion error = %v", err)
	}
	if _, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: "wrong-token",
		ClaimEpoch: claimed.ClaimEpoch,
	}); !errors.Is(err, ErrPublicationClaimNotOwner) {
		t.Fatalf("wrong owner completion error = %v", err)
	}
	if _, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: pending.UpdatedAt, ClaimToken: claimed.ClaimToken,
		ClaimEpoch: claimed.ClaimEpoch,
	}); !errors.Is(err, ErrPublicationUpdateConflict) {
		t.Fatalf("stale completion timestamp error = %v", err)
	}
	rawURL := "https://example.test/pull/17"
	completed, err := opened.CompletePublication(ctx, claimed.ID, CompletePublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: claimed.ClaimToken,
		ClaimEpoch: claimed.ClaimEpoch,
		URL:        &rawURL,
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
	publicationNow = baseTime
	retryClaim, acquired, err := opened.ClaimPublication(ctx, retry.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: retry.UpdatedAt, TTL: time.Minute,
	})
	if err != nil || !acquired {
		t.Fatalf("claim retry source: acquired=%v err=%v", acquired, err)
	}
	publicationNow = baseTime.Add(time.Second)
	failed, err := opened.FailPublication(ctx, retry.ID, FailPublicationInput{
		ExpectedUpdatedAt: retryClaim.UpdatedAt, ClaimToken: retryClaim.ClaimToken,
		ClaimEpoch: retryClaim.ClaimEpoch,
		Error:      "remote temporarily unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.PublicationFailed || failed.Error == nil ||
		*failed.Error != "remote temporarily unavailable" {
		t.Fatalf("failed publication = %+v", failed)
	}
	if _, acquired, err := opened.ClaimPublication(ctx, retry.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: failed.UpdatedAt, TTL: time.Minute,
	}); !errors.Is(err, ErrPublicationStateConflict) || acquired {
		t.Fatalf("automatic failed retry: acquired=%v err=%v", acquired, err)
	}
	retried, err := opened.RetryPublication(ctx, retry.ID, RetryPublicationInput{
		ExpectedUpdatedAt: failed.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	publicationNow = baseTime.Add(2 * time.Second)
	retryClaim, acquired, err = opened.ClaimPublication(ctx, retry.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: retried.UpdatedAt, TTL: time.Minute,
	})
	if err != nil || !acquired || retryClaim.Error != nil || retryClaim.ClaimEpoch != 2 {
		t.Fatalf("retry claim: value=%+v acquired=%v err=%v", retryClaim, acquired, err)
	}
	publicationNow = baseTime.Add(3 * time.Second)
	if _, err := opened.FailPublication(ctx, retry.ID, FailPublicationInput{
		ExpectedUpdatedAt: retryClaim.UpdatedAt, ClaimToken: retryClaim.ClaimToken,
		ClaimEpoch: retryClaim.ClaimEpoch - 1,
		Error:      "stale claim epoch",
	}); !errors.Is(err, ErrPublicationClaimNotOwner) {
		t.Fatalf("stale claim epoch error = %v", err)
	}
	if _, err := opened.FailPublication(ctx, retry.ID, FailPublicationInput{
		ExpectedUpdatedAt: retried.UpdatedAt, ClaimToken: retryClaim.ClaimToken,
		ClaimEpoch: retryClaim.ClaimEpoch,
		Error:      "stale publication version",
	}); !errors.Is(err, ErrPublicationUpdateConflict) {
		t.Fatalf("stale failure timestamp error = %v", err)
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
	publicationNow = baseTime
	oldClaim, acquired, err := opened.ClaimPublication(ctx, takeover.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: takeover.UpdatedAt, TTL: MinPublicationClaimTTL,
	})
	if err != nil || !acquired {
		t.Fatalf("old claim: acquired=%v err=%v", acquired, err)
	}
	publicationNow = baseTime.Add(MinPublicationClaimTTL)
	observed, acquired, err := opened.ClaimPublication(ctx, takeover.ID, ClaimPublicationInput{
		ExpectedUpdatedAt: takeover.UpdatedAt, TTL: time.Minute,
	})
	if err != nil || acquired || observed.ClaimToken != "" ||
		observed.ClaimEpoch != oldClaim.ClaimEpoch ||
		observed.UpdatedAt != oldClaim.UpdatedAt ||
		observed.ClaimExpiresAt == nil ||
		*observed.ClaimExpiresAt != *oldClaim.ClaimExpiresAt {
		t.Fatalf("expired publishing observation: value=%+v acquired=%v err=%v", observed, acquired, err)
	}
	publicationNow = baseTime.Add(MinPublicationClaimTTL + time.Second)
	if _, err := opened.FailPublication(ctx, takeover.ID, FailPublicationInput{
		ExpectedUpdatedAt: oldClaim.UpdatedAt, ClaimToken: oldClaim.ClaimToken,
		ClaimEpoch: oldClaim.ClaimEpoch,
		Error:      "expired worker",
	}); !errors.Is(err, ErrPublicationClaimExpired) {
		t.Fatalf("expired claimant error = %v", err)
	}
}

func TestDeleteTaskRejectsPublishingPublicationAndPreservesEvidence(
	t *testing.T,
) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, changeSet := createPublicationSource(
		t,
		opened,
		"delete_guard",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	)
	if err != nil || !acquired {
		t.Fatalf(
			"claim publication = %+v, acquired=%v, err=%v",
			claimed,
			acquired,
			err,
		)
	}

	if err := opened.DeleteTask(
		ctx,
		task.ID,
	); !errors.Is(err, ErrPublicationStateConflict) {
		t.Fatalf("delete task with publishing publication error = %v", err)
	}
	if _, err := opened.GetTask(ctx, task.ID); err != nil {
		t.Fatalf("publishing task was not preserved: %v", err)
	}
	preserved, err := opened.GetPublication(ctx, claimed.ID)
	if err != nil || preserved.Status != model.PublicationPublishing ||
		preserved.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf(
			"publishing evidence = %+v, err=%v",
			preserved,
			err,
		)
	}

	published, err := opened.CompletePublication(
		ctx,
		claimed.ID,
		CompletePublicationInput{
			ExpectedUpdatedAt: claimed.UpdatedAt,
			ClaimToken:        claimed.ClaimToken,
			ClaimEpoch:        claimed.ClaimEpoch,
		},
	)
	if err != nil || published.Status != model.PublicationPublished {
		t.Fatalf("complete publication = %+v, err=%v", published, err)
	}
	if err := opened.DeleteTask(ctx, task.ID); err != nil {
		t.Fatalf("delete task with terminal publication: %v", err)
	}
	if _, err := opened.GetPublication(
		ctx,
		claimed.ID,
	); !errors.Is(err, ErrPublicationNotFound) {
		t.Fatalf("terminal publication after task deletion error = %v", err)
	}
}

func TestPublicationClaimRejectsExhaustedEpoch(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	opened.publicationClock = func() time.Time {
		return time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	}

	_, source := createPublicationSource(
		t, opened, "exhausted_epoch", model.WorkflowRoleFinalizer,
		model.TaskStatusDone, model.RunStatusCompleted, "ready",
	)
	pending, _, err := opened.EnsurePublication(
		ctx, publicationPolicyInput(source.ID, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(
		ctx,
		"UPDATE publications SET claim_epoch = ? WHERE id = ?",
		int64(math.MaxInt64),
		pending.ID,
	); err != nil {
		t.Fatal(err)
	}
	pending, err = opened.GetPublication(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := opened.ClaimPublication(
		ctx,
		pending.ID,
		ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               time.Minute,
		},
	); err == nil || !strings.Contains(err.Error(), "epoch is exhausted") || acquired {
		t.Fatalf("exhausted claim: acquired=%v err=%v", acquired, err)
	}
	var status model.PublicationStatus
	var epoch int64
	var token, expiry sql.NullString
	if err := opened.db.QueryRowContext(
		ctx,
		`SELECT status, claim_epoch, claim_token, claim_expires_at
		 FROM publications WHERE id = ?`,
		pending.ID,
	).Scan(&status, &epoch, &token, &expiry); err != nil {
		t.Fatal(err)
	}
	if status != model.PublicationPending || epoch != math.MaxInt64 ||
		token.Valid || expiry.Valid {
		t.Fatalf(
			"exhausted claim mutated row: status=%s epoch=%d token=%v expiry=%v",
			status,
			epoch,
			token.Valid,
			expiry.Valid,
		)
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

func TestExistingPublicationSchemaAddsClaimEpoch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "claim-epoch.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	_, changeSet := createPublicationSource(
		t,
		opened,
		"claim_epoch_migration",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	publication, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
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
	if _, err := raw.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS publications_claim_epoch_insert_guard;
		DROP TRIGGER IF EXISTS publications_claim_epoch_update_guard;
		ALTER TABLE publications DROP COLUMN claim_epoch;
		PRAGMA user_version = 25;
	`); err != nil {
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
	preserved, err := reopened.GetPublication(ctx, publication.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.ClaimEpoch != 0 || preserved.Status != publication.Status {
		t.Fatalf("migrated publication = %+v", preserved)
	}
	var version, notNull int
	var columnType, defaultValue string
	if err := reopened.db.QueryRowContext(
		ctx,
		`SELECT type, "notnull", CAST(dflt_value AS TEXT)
		 FROM pragma_table_info('publications')
		 WHERE name = 'claim_epoch'`,
	).Scan(&columnType, &notNull, &defaultValue); err != nil {
		t.Fatal(err)
	}
	if versionErr := reopened.db.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); versionErr != nil {
		t.Fatal(versionErr)
	}
	if version != schemaVersion || schemaVersion != 29 ||
		columnType != "INTEGER" || notNull != 1 || defaultValue != "0" {
		t.Fatalf(
			"claim epoch migration: version=%d constant=%d type=%q notNull=%d default=%q",
			version,
			schemaVersion,
			columnType,
			notNull,
			defaultValue,
		)
	}
	if _, err := reopened.db.ExecContext(
		ctx,
		"UPDATE publications SET claim_epoch = -1 WHERE id = ?",
		publication.ID,
	); err == nil {
		t.Fatal("negative claim epoch bypassed database constraint")
	}
	if _, err := reopened.db.ExecContext(
		ctx,
		"UPDATE publications SET claim_epoch = 1.5 WHERE id = ?",
		publication.ID,
	); err == nil {
		t.Fatal("non-integer claim epoch bypassed database constraint")
	}
}

func TestExistingPublicationSchemaRejectsStoredNonIntegerClaimEpoch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "invalid-claim-epoch.db")
	opened, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	_, changeSet := createPublicationSource(
		t,
		opened,
		"invalid_claim_epoch",
		model.WorkflowRoleFinalizer,
		model.TaskStatusDone,
		model.RunStatusCompleted,
		"ready",
	)
	publication, _, err := opened.EnsurePublication(
		ctx,
		publicationPolicyInput(changeSet.ID, false),
	)
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
	if _, err := raw.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS publications_claim_epoch_insert_guard;
		DROP TRIGGER IF EXISTS publications_claim_epoch_update_guard;
		PRAGMA ignore_check_constraints = ON;
		UPDATE publications SET claim_epoch = 1.5 WHERE id = ?;
	`, publication.ID); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, "default", "")
	if err == nil {
		reopened.Close()
		t.Fatal("stored non-integer claim epoch passed schema validation")
	}
	if !strings.Contains(err.Error(), "invalid claim epochs") {
		t.Fatalf("stored non-integer claim epoch error = %v", err)
	}
}

func TestPublicationRecoveryReaderRejectsNonExactPublishingEvidence(
	t *testing.T,
) {
	for _, test := range []struct {
		name   string
		mutate func(model.Publication) (string, []any)
	}{
		{
			name: "non-canonical updatedAt",
			mutate: func(value model.Publication) (string, []any) {
				return "UPDATE publications SET updated_at = ? WHERE id = ?",
					[]any{" " + value.UpdatedAt, value.ID}
			},
		},
		{
			name: "non-positive claim epoch",
			mutate: func(value model.Publication) (string, []any) {
				return "UPDATE publications SET claim_epoch = 0 WHERE id = ?",
					[]any{value.ID}
			},
		},
		{
			name: "board mismatch",
			mutate: func(value model.Publication) (string, []any) {
				return "UPDATE publications SET board = 'other' WHERE id = ?",
					[]any{value.ID}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "recovery-evidence.db")
			opened, err := Open(dbPath, "default", "")
			if err != nil {
				t.Fatal(err)
			}
			_, changeSet := createPublicationSource(
				t,
				opened,
				"recovery_evidence",
				model.WorkflowRoleFinalizer,
				model.TaskStatusDone,
				model.RunStatusCompleted,
				"ready",
			)
			pending, _, err := opened.EnsurePublication(
				ctx,
				publicationPolicyInput(changeSet.ID, false),
			)
			if err != nil {
				opened.Close()
				t.Fatal(err)
			}
			claimed, acquired, err := opened.ClaimPublication(
				ctx,
				pending.ID,
				ClaimPublicationInput{
					ExpectedUpdatedAt: pending.UpdatedAt,
					TTL:               time.Minute,
				},
			)
			if err != nil || !acquired {
				opened.Close()
				t.Fatalf("claim publication: acquired=%t err=%v", acquired, err)
			}
			query, arguments := test.mutate(claimed)
			if _, err := opened.db.ExecContext(
				ctx,
				query,
				arguments...,
			); err != nil {
				opened.Close()
				t.Fatal(err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := OpenPublicationRecoveryReader(
				ctx,
				dbPath,
				"default",
			)
			if err != nil {
				t.Fatal(err)
			}
			_, _, readErr := reader.ListPublishingAfter(ctx, "")
			closeErr := reader.Close()
			if readErr == nil {
				t.Fatal("corrupt publishing evidence passed recovery scan")
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
		})
	}
}
