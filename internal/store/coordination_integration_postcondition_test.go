package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func integrationPostconditionIncident(
	t *testing.T,
	ctx context.Context,
	opened *Store,
	task model.Task,
) model.CoordinationIncident {
	t.Helper()
	details, err := json.Marshal(map[string]any{
		"code":      "resolution_exhausted",
		"blockKind": model.BlockKindNeedsInput,
		"reason":    "finalizer resolution attempts exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	incident, created, err := opened.CreateCoordinationIncident(
		ctx,
		CreateCoordinationIncidentInput{
			TaskID: &task.ID, Trigger: model.CoordinationTriggerIntegrationConflict,
			Severity: model.CoordinationSeverityError,
			Summary:  "Finalizer integration resolution exhausted",
			Details:  details,
		},
	)
	if err != nil || !created {
		t.Fatalf("create integration incident: created=%t value=%+v error=%v", created, incident, err)
	}
	return incident
}

func TestApplyCoordinationProposalKeepsUnresolvedIntegrationIncident(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "exhausted finalizer",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err = opened.BlockTask(ctx, task.Task.ID, BlockInput{
		Kind:   model.BlockKindNeedsInput,
		Reason: "finalizer resolution attempts exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	incident := integrationPostconditionIncident(t, ctx, opened, task.Task)
	priority := 9
	fixture := applicableProposalForIncident(
		t,
		ctx,
		opened,
		incident,
		[]coordinationAction{{
			Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
			ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority,
			Reason: "prioritize the same finalizer",
		}},
		CoordinationApplyValidatedAuto,
	)
	_, err = opened.ApplyCoordinationProposal(
		ctx,
		fixture.proposal.ID,
		applyFixtureInput(fixture, CoordinationApplyValidatedAuto),
	)
	if err == nil || !strings.Contains(err.Error(), "retains its integration block") {
		t.Fatalf("route-only integration apply error = %v", err)
	}
	currentTask, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentIncident, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentProposal, err := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentTask.Task.Priority == priority ||
		currentTask.Task.BlockKind == nil ||
		currentIncident.Status != model.CoordinationIncidentCoordinating ||
		currentProposal.Status != model.CoordinationProposalValidated {
		t.Fatalf(
			"failed postcondition was not atomic: task=%+v incident=%+v proposal=%+v",
			currentTask.Task,
			currentIncident,
			currentProposal,
		)
	}
}

func TestApprovedCoordinationProposalKeepsUnresolvedIntegrationIncident(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "exhausted finalizer"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = opened.BlockTask(ctx, task.Task.ID, BlockInput{
		Kind:   model.BlockKindNeedsInput,
		Reason: "finalizer resolution attempts exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	incident := integrationPostconditionIncident(t, ctx, opened, task.Task)
	priority := 9
	fixture := applicableProposalForIncident(
		t,
		ctx,
		opened,
		incident,
		[]coordinationAction{{
			Kind: coordinationActionUpdatePriority, TaskID: task.Task.ID,
			ExpectedUpdatedAt: task.Task.UpdatedAt, Priority: &priority,
			Reason: "prioritize the same finalizer",
		}},
		CoordinationApplyApproved,
	)
	_, err = opened.ApplyCoordinationProposal(
		ctx,
		fixture.proposal.ID,
		applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err == nil || !strings.Contains(err.Error(), "retains its integration block") {
		t.Fatalf("approved route-only integration apply error = %v", err)
	}
	currentIncident, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentProposal, err := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentIncident.Status != model.CoordinationIncidentAwaitingApproval ||
		currentProposal.Status != model.CoordinationProposalApproved {
		t.Fatalf(
			"approved postcondition rollback: incident=%+v proposal=%+v",
			currentIncident,
			currentProposal,
		)
	}
}

func TestApplyCoordinationProposalResolvesIntegrationAfterClearingBlock(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "exhausted finalizer"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = opened.BlockTask(ctx, task.Task.ID, BlockInput{
		Kind:   model.BlockKindNeedsInput,
		Reason: "finalizer resolution attempts exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	incident := integrationPostconditionIncident(t, ctx, opened, task.Task)
	fixture := applicableProposalForIncident(
		t,
		ctx,
		opened,
		incident,
		[]coordinationAction{{
			Kind: coordinationActionMoveToTriage, TaskID: task.Task.ID,
			ExpectedUpdatedAt: task.Task.UpdatedAt,
			Reason:            "return exhausted integration to explicit replanning",
		}},
		CoordinationApplyApproved,
	)
	result, err := opened.ApplyCoordinationProposal(
		ctx,
		fixture.proposal.ID,
		applyFixtureInput(fixture, CoordinationApplyApproved),
	)
	if err != nil {
		t.Fatal(err)
	}
	currentTask, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Incident.Status != model.CoordinationIncidentResolved ||
		currentTask.Task.Status != model.TaskStatusTriage ||
		currentTask.Task.BlockKind != nil || currentTask.Task.BlockReason != nil {
		t.Fatalf("cleared integration result: result=%+v task=%+v", result, currentTask.Task)
	}
}
