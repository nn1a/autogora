package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type coordinationApplyFixture struct {
	incident  model.CoordinationIncident
	proposal  model.CoordinationProposal
	claimTime time.Time
}

func applicableCoordinationProposal(
	t *testing.T,
	ctx context.Context,
	opened *Store,
	actions []coordinationAction,
	authorization CoordinationApplyAuthorization,
) coordinationApplyFixture {
	t.Helper()
	state, err := opened.GetBoardGraphState(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Severity: model.CoordinationSeverityWarning,
		ExpectedGraphRevision: revisionPointer(state.Revision), Summary: "Coordinator recovery is required",
	})
	if err != nil {
		t.Fatal(err)
	}
	return applicableProposalForIncident(t, ctx, opened, incident, actions, authorization)
}

func applicableProposalForIncident(
	t *testing.T,
	ctx context.Context,
	opened *Store,
	incident model.CoordinationIncident,
	actions []coordinationAction,
	authorization CoordinationApplyAuthorization,
) coordinationApplyFixture {
	t.Helper()
	claimTime := time.Now().UTC()
	claimed, won, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !won {
		t.Fatalf("claim incident: won=%v incident=%+v err=%v", won, claimed, err)
	}
	incident = claimed
	encoded, err := json.Marshal(actions)
	if err != nil {
		t.Fatal(err)
	}
	proposal, _, err := opened.CreateCoordinationProposal(ctx, CreateCoordinationProposalInput{
		IncidentID: incident.ID, CoordinatorAgent: "coordinator",
		CoordinatorModel: "test-model", CoordinatorProvider: "test-provider",
		ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
		ClaimToken:            incident.ClaimToken,
		Current:               claimTime.Add(time.Second),
		Summary:               "Apply bounded recovery", Rationale: "The deterministic recovery path is exhausted.",
		Actions: encoded,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.CoordinationProposalStatus{
		model.CoordinationProposalValidating,
		model.CoordinationProposalValidated,
	} {
		proposal, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, TransitionCoordinationProposalInput{
			ExpectedStatus: proposal.Status, Status: status,
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            incident.ClaimToken,
			Current:               claimTime.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	switch authorization {
	case CoordinationApplyValidatedAuto:
	case CoordinationApplyApproved:
		approval, err := opened.RequestCoordinationApproval(ctx, proposal.ID, RequestCoordinationApprovalInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            incident.ClaimToken,
			Current:               claimTime.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		approval, err = opened.ApproveCoordinationProposal(ctx, proposal.ID, ApproveCoordinationProposalInput{
			ExpectedUpdatedAt:     approval.Proposal.UpdatedAt,
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
		})
		if err != nil {
			t.Fatal(err)
		}
		proposal, incident = approval.Proposal, approval.Incident
	default:
		t.Fatalf("unsupported test authorization %s", authorization)
	}
	return coordinationApplyFixture{incident: incident, proposal: proposal, claimTime: claimTime}
}

func applyFixtureInput(
	fixture coordinationApplyFixture,
	authorization CoordinationApplyAuthorization,
) ApplyCoordinationProposalInput {
	input := ApplyCoordinationProposalInput{
		Authorization: authorization, ExpectedGraphRevision: revisionPointer(fixture.incident.GraphRevision),
		Current: fixture.claimTime.Add(2 * time.Second),
	}
	if authorization == CoordinationApplyValidatedAuto {
		input.ClaimToken = fixture.incident.ClaimToken
	}
	return input
}

func TestApplyCoordinationProposalRouteAndUnblockAtomically(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee := "codex"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "blocked implementation", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := opened.BlockTask(ctx, task.Task.ID, BlockInput{
		Kind: model.BlockKindCapability, Reason: "primary agent is unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := []coordinationAction{
		{
			Kind: coordinationActionSetRoute, TaskID: task.Task.ID,
			ExpectedUpdatedAt: blocked.Task.UpdatedAt, Assignee: "claude",
			Runtime: model.RuntimeClaude, Reason: "use an available agent",
		},
		{
			Kind: coordinationActionUnblockTask, TaskID: task.Task.ID,
			ExpectedUpdatedAt: blocked.Task.UpdatedAt, Reason: "the replacement route is ready",
		},
	}
	fixture := applicableCoordinationProposal(
		t, ctx, opened, actions, CoordinationApplyApproved,
	)
	result, err := opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Task.Assignee == nil || *updated.Task.Assignee != "claude" ||
		updated.Task.Runtime != model.RuntimeClaude || updated.Task.Status != model.TaskStatusReady ||
		updated.Task.BlockKind != nil || updated.Task.BlockReason != nil ||
		updated.Task.BlockRecurrences != 0 {
		t.Fatalf("route + unblock result = %+v", updated.Task)
	}
	if result.Proposal.Status != model.CoordinationProposalApplied ||
		result.Proposal.AppliedAt == nil ||
		result.Incident.Status != model.CoordinationIncidentResolved {
		t.Fatalf("application lifecycle = %+v / %+v", result.Proposal, result.Incident)
	}
}

func TestApplyCoordinationProposalMoveToTriageAndRemoveDependency(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	prerequisite, err := opened.CreateTask(ctx, CreateTaskInput{Title: "obsolete prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "needs replanning"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, prerequisite.Task.ID, dependent.Task.ID); err != nil {
		t.Fatal(err)
	}
	prerequisite, _ = opened.GetTask(ctx, prerequisite.Task.ID)
	dependent, _ = opened.GetTask(ctx, dependent.Task.ID)
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{
		{
			Kind: coordinationActionMoveToTriage, TaskID: dependent.Task.ID,
			ExpectedUpdatedAt: dependent.Task.UpdatedAt, Reason: "the task needs a new plan",
		},
		{
			Kind:           coordinationActionRemoveDependency,
			PrerequisiteID: prerequisite.Task.ID, ExpectedPrerequisiteUpdatedAt: prerequisite.Task.UpdatedAt,
			DependentID: dependent.Task.ID, ExpectedDependentUpdatedAt: dependent.Task.UpdatedAt,
			Reason: "the old dependency no longer applies",
		},
	}, CoordinationApplyApproved)
	result, err := opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := opened.GetTask(ctx, dependent.Task.ID)
	var linked int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_links WHERE parent_id = ? AND child_id = ?",
		prerequisite.Task.ID, dependent.Task.ID,
	).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if updated.Task.Status != model.TaskStatusTriage || linked != 0 ||
		result.GraphRevision != fixture.incident.GraphRevision+1 {
		t.Fatalf("move/remove result task=%+v linked=%d graph=%d", updated.Task, linked, result.GraphRevision)
	}
}

func TestApplyCoordinationProposalMoveToTriageClearsBlockLoopForPlanner(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "codex"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "repeatedly blocked task", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := BlockInput{Kind: model.BlockKindCapability, Reason: "required capability is unavailable"}
	if _, err := opened.BlockTask(ctx, task.Task.ID, block); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	looped, err := opened.BlockTask(ctx, task.Task.ID, block)
	if err != nil {
		t.Fatal(err)
	}
	if looped.Task.Status != model.TaskStatusTriage || looped.Task.BlockReason == nil ||
		looped.Task.BlockRecurrences < blockRecurrenceLimit {
		t.Fatalf("test did not create block-loop triage: %+v", looped.Task)
	}
	previousScheduledAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := opened.db.ExecContext(ctx, `
		UPDATE tasks SET scheduled_at = ?, failure_count = 3, updated_at = ? WHERE id = ?
	`, previousScheduledAt, now(), task.Task.ID); err != nil {
		t.Fatal(err)
	}
	looped, err = opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionMoveToTriage, TaskID: task.Task.ID,
		ExpectedUpdatedAt: looped.Task.UpdatedAt, Reason: "reset diagnostics and let Planner create a new plan",
	}}, CoordinationApplyApproved)
	if _, err := opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	); err != nil {
		t.Fatal(err)
	}
	updated, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This is the exact state predicate that keeps repeated-block Triage tasks
	// out of Planner. Clearing it makes the task eligible for decomposition.
	if updated.Task.Status != model.TaskStatusTriage || updated.Task.BlockReason != nil ||
		updated.Task.BlockKind != nil || updated.Task.BlockRecurrences != 0 ||
		updated.Task.FailureCount != 0 || updated.Task.ScheduledAt != nil {
		t.Fatalf("task remains in Planner-skipped triage state: %+v", updated.Task)
	}
	events, err := opened.ListEvents(ctx, EventFilter{
		TaskID: task.Task.ID, Kinds: []string{"coordination_moved_to_triage"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("move-to-triage events = %d, want 1", len(events))
	}
	payload := map[string]any{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["previousBlockKind"] != string(model.BlockKindCapability) ||
		payload["previousBlockReason"] != block.Reason ||
		payload["previousBlockRecurrences"] != float64(blockRecurrenceLimit) ||
		payload["previousFailureCount"] != float64(3) ||
		payload["previousScheduledAt"] != previousScheduledAt {
		t.Fatalf("previous triage diagnostics were not preserved: %#v", payload)
	}
}

func TestApplyCoordinationProposalAuthorizationRequiresDurableState(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "priority target", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	priority := 7
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
		ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority, Reason: "raise recovery priority",
	}}, CoordinationApplyValidatedAuto)
	input := applyFixtureInput(fixture, CoordinationApplyApproved)
	_, err = opened.ApplyCoordinationProposal(ctx, fixture.proposal.ID, input)
	if !errors.Is(err, ErrCoordinationStateConflict) {
		t.Fatalf("approved authorization over validated state error = %v", err)
	}
	updated, _ := opened.GetTask(ctx, task.Task.ID)
	if updated.Task.Priority != 1 {
		t.Fatalf("authorization mismatch changed priority to %d", updated.Task.Priority)
	}
}

func TestApplyCoordinationProposalCompoundCycleRollsBack(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	first, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "first"})
	second, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "second"})
	third, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "third"})
	if _, err := opened.LinkTasks(ctx, first.Task.ID, second.Task.ID); err != nil {
		t.Fatal(err)
	}
	first, _ = opened.GetTask(ctx, first.Task.ID)
	second, _ = opened.GetTask(ctx, second.Task.ID)
	third, _ = opened.GetTask(ctx, third.Task.ID)
	actions := []coordinationAction{
		{
			Kind:           coordinationActionAddDependency,
			PrerequisiteID: second.Task.ID, ExpectedPrerequisiteUpdatedAt: second.Task.UpdatedAt,
			DependentID: third.Task.ID, ExpectedDependentUpdatedAt: third.Task.UpdatedAt,
			Reason: "continue the chain",
		},
		{
			Kind:           coordinationActionAddDependency,
			PrerequisiteID: third.Task.ID, ExpectedPrerequisiteUpdatedAt: third.Task.UpdatedAt,
			DependentID: first.Task.ID, ExpectedDependentUpdatedAt: first.Task.UpdatedAt,
			Reason: "invalid cycle",
		},
	}
	fixture := applicableCoordinationProposal(
		t, ctx, opened, actions, CoordinationApplyApproved,
	)
	_, err = opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err == nil {
		t.Fatal("compound dependency cycle was applied")
	}
	var linked int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_links WHERE parent_id = ? AND child_id = ?",
		second.Task.ID, third.Task.ID,
	).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 0 {
		t.Fatal("first dependency action survived cycle rollback")
	}
	state, _ := opened.GetBoardGraphState(ctx, "")
	proposal, _ := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if state.Revision != fixture.incident.GraphRevision ||
		proposal.Status != model.CoordinationProposalApproved ||
		incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("rollback state: graph=%+v proposal=%+v incident=%+v", state, proposal, incident)
	}
}

func TestApplyCoordinationProposalRejectsStaleTaskAndGraphWithoutPartialWrites(t *testing.T) {
	t.Run("task", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		task, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "priority target", Priority: 1})
		priority := 9
		fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
			Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
			ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority, Reason: "make recovery urgent",
		}}, CoordinationApplyValidatedAuto)
		newerPriority := 3
		if _, err := opened.UpdateTask(ctx, task.Task.ID, UpdateTaskInput{Priority: &newerPriority}); err != nil {
			t.Fatal(err)
		}
		if _, err := opened.ApplyCoordinationProposal(
			ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyValidatedAuto),
		); err == nil {
			t.Fatal("stale task proposal was applied")
		}
		updated, _ := opened.GetTask(ctx, task.Task.ID)
		if updated.Task.Priority != newerPriority {
			t.Fatalf("stale apply changed priority to %d", updated.Task.Priority)
		}
	})

	t.Run("graph", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		task, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "priority target", Priority: 1})
		first, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "unrelated first"})
		second, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "unrelated second"})
		priority := 9
		fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
			Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
			ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority, Reason: "make recovery urgent",
		}}, CoordinationApplyValidatedAuto)
		if _, err := opened.LinkTasks(ctx, first.Task.ID, second.Task.ID); err != nil {
			t.Fatal(err)
		}
		_, err = opened.ApplyCoordinationProposal(
			ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyValidatedAuto),
		)
		if !errors.Is(err, ErrGraphRevisionConflict) {
			t.Fatalf("stale graph error = %v", err)
		}
		updated, _ := opened.GetTask(ctx, task.Task.ID)
		if updated.Task.Priority != 1 {
			t.Fatalf("stale graph apply changed priority to %d", updated.Task.Priority)
		}
	})
}

func TestApplyCoordinationProposalReusesIncidentScopedRecoveryTask(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger:               model.CoordinationTriggerRetryExhausted,
		ExpectedGraphRevision: revisionPointer(0), Summary: "Retry recovery needs work",
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := &coordinationTaskDraft{
		Key: "recovery", Title: "Recover failed work", Body: "Preserve and repair the partial change.",
		Assignee: "codex", Runtime: model.RuntimeCodex, WorkflowRole: model.WorkflowRoleWorker,
		Priority: 4, Prerequisites: []string{}, Dependents: []string{},
	}
	key := coordinationRecoveryIdempotencyKey(incident.ID, draft.Key)
	assignee := draft.Assignee
	existing, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: draft.Title, Body: draft.Body, IdempotencyKey: &key, Assignee: &assignee,
		Runtime: draft.Runtime, WorkflowRole: draft.WorkflowRole, Priority: draft.Priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := applicableProposalForIncident(t, ctx, opened, incident, []coordinationAction{{
		Kind: coordinationActionCreateTask, Task: draft,
		ExpectedTaskVersions: map[string]string{}, Reason: "resume with bounded recovery work",
	}}, CoordinationApplyApproved)
	result, err := opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.CreatedTaskIDs[draft.Key] != existing.Task.ID {
		t.Fatalf("resolved recovery task = %q, want %q", result.CreatedTaskIDs[draft.Key], existing.Task.ID)
	}
	var count int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tasks WHERE board = ? AND idempotency_key = ?",
		"default", key,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || result.GraphRevision != 0 {
		t.Fatalf("idempotent create count=%d graphRevision=%d", count, result.GraphRevision)
	}
}

func TestApplyCoordinationProposalBumpsTopologyRevisionOnce(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	prerequisite, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "prerequisite"})
	dependent, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "dependent"})
	parent, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "parent"})
	draft := &coordinationTaskDraft{
		Key: "bridge", Title: "Bridge recovery work", Body: "Repair the dependency chain.",
		Assignee: "codex", Runtime: model.RuntimeCodex, WorkflowRole: model.WorkflowRoleReviewer,
		Priority: 6, Prerequisites: []string{prerequisite.Task.ID},
		Dependents: []string{dependent.Task.ID}, ParentTaskID: parent.Task.ID,
	}
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionCreateTask, Task: draft,
		ExpectedTaskVersions: map[string]string{
			prerequisite.Task.ID: prerequisite.Task.UpdatedAt,
			dependent.Task.ID:    dependent.Task.UpdatedAt,
			parent.Task.ID:       parent.Task.UpdatedAt,
		},
		Reason: "insert one bounded recovery step",
	}}, CoordinationApplyApproved)
	result, err := opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.GraphRevision != fixture.incident.GraphRevision+1 {
		t.Fatalf("graph revision = %d, want %d", result.GraphRevision, fixture.incident.GraphRevision+1)
	}
	taskID := result.CreatedTaskIDs[draft.Key]
	var dependencyCount, hierarchyCount int
	if err := opened.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_links
		WHERE (parent_id = ? AND child_id = ?) OR (parent_id = ? AND child_id = ?)
	`, prerequisite.Task.ID, taskID, taskID, dependent.Task.ID).Scan(&dependencyCount); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_hierarchy WHERE parent_id = ? AND child_id = ?",
		parent.Task.ID, taskID,
	).Scan(&hierarchyCount); err != nil {
		t.Fatal(err)
	}
	if dependencyCount != 2 || hierarchyCount != 1 {
		t.Fatalf("created topology dependencies=%d hierarchy=%d", dependencyCount, hierarchyCount)
	}
}

func TestApplyCoordinationProposalRejectsControlParentBeforeCreate(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	control, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "swarm control", WorkflowRole: model.WorkflowRoleControl,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := &coordinationTaskDraft{
		Key: "unsafe-control-child", Title: "Recovery work", Body: "Do not attach this to control.",
		Assignee: "codex", Runtime: model.RuntimeCodex, WorkflowRole: model.WorkflowRoleWorker,
		Prerequisites: []string{}, Dependents: []string{}, ParentTaskID: control.Task.ID,
	}
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionCreateTask, Task: draft,
		ExpectedTaskVersions: map[string]string{
			control.Task.ID: control.Task.UpdatedAt,
		},
		Reason: "invalid control topology mutation",
	}}, CoordinationApplyApproved)
	_, err = opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err == nil || !strings.Contains(err.Error(), "control task") {
		t.Fatalf("control parent apply error = %v", err)
	}
	key := coordinationRecoveryIdempotencyKey(fixture.incident.ID, draft.Key)
	var created int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tasks WHERE board = ? AND idempotency_key = ?",
		"default", key,
	).Scan(&created); err != nil {
		t.Fatal(err)
	}
	state, _ := opened.GetBoardGraphState(ctx, "")
	proposal, _ := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if created != 0 || state.Revision != fixture.incident.GraphRevision ||
		proposal.Status != model.CoordinationProposalApproved ||
		incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf(
			"control parent rejection was not atomic: created=%d graph=%+v proposal=%+v incident=%+v",
			created, state, proposal, incident,
		)
	}
}

func TestApplyCoordinationProposalRejectsArchivedParentBeforeCreate(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "retired parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = opened.ArchiveTask(ctx, parent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft := &coordinationTaskDraft{
		Key: "unsafe-archived-child", Title: "Recovery work", Body: "Do not attach this to archived work.",
		Assignee: "codex", Runtime: model.RuntimeCodex, WorkflowRole: model.WorkflowRoleWorker,
		Prerequisites: []string{}, Dependents: []string{}, ParentTaskID: parent.Task.ID,
	}
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionCreateTask, Task: draft,
		ExpectedTaskVersions: map[string]string{
			parent.Task.ID: parent.Task.UpdatedAt,
		},
		Reason: "invalid archived topology mutation",
	}}, CoordinationApplyApproved)
	_, err = opened.ApplyCoordinationProposal(
		ctx, fixture.proposal.ID, applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err == nil || !strings.Contains(err.Error(), "archived task") {
		t.Fatalf("archived parent apply error = %v", err)
	}
	key := coordinationRecoveryIdempotencyKey(fixture.incident.ID, draft.Key)
	var created int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tasks WHERE board = ? AND idempotency_key = ?",
		"default", key,
	).Scan(&created); err != nil {
		t.Fatal(err)
	}
	state, _ := opened.GetBoardGraphState(ctx, "")
	proposal, _ := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	incident, _ := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if created != 0 || state.Revision != fixture.incident.GraphRevision ||
		proposal.Status != model.CoordinationProposalApproved ||
		incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf(
			"archived parent rejection was not atomic: created=%d graph=%+v proposal=%+v incident=%+v",
			created, state, proposal, incident,
		)
	}
}

func TestApplyCoordinationProposalRejectsStaleClaimOwner(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "priority target", Priority: 1})
	priority := 8
	fixture := applicableCoordinationProposal(t, ctx, opened, []coordinationAction{{
		Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
		ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority, Reason: "raise recovery priority",
	}}, CoordinationApplyValidatedAuto)
	staleToken := fixture.incident.ClaimToken
	reclaimed, won, err := opened.ClaimCoordinationIncident(ctx, fixture.incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(fixture.incident.GraphRevision),
		TTL:                   time.Minute,
		Current:               fixture.claimTime.Add(time.Minute),
	})
	if err != nil || !won || reclaimed.ClaimToken == staleToken {
		t.Fatalf("reclaim: won=%v incident=%+v err=%v", won, reclaimed, err)
	}
	input := applyFixtureInput(fixture, CoordinationApplyValidatedAuto)
	input.Current = fixture.claimTime.Add(time.Minute + time.Second)
	_, err = opened.ApplyCoordinationProposal(ctx, fixture.proposal.ID, input)
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("stale claim apply error = %v", err)
	}
	updated, _ := opened.GetTask(ctx, task.Task.ID)
	proposal, _ := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	if updated.Task.Priority != 1 || proposal.Status != model.CoordinationProposalValidated {
		t.Fatalf("stale owner changed state: task=%+v proposal=%+v", updated.Task, proposal)
	}
}
