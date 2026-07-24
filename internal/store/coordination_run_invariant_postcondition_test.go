package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func runInvariantIncident(
	t *testing.T,
	ctx context.Context,
	opened *Store,
	task model.Task,
	details map[string]any,
) model.CoordinationIncident {
	t.Helper()
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	incident, created, err := opened.CreateCoordinationIncident(
		ctx,
		CreateCoordinationIncidentInput{
			TaskID: &task.ID, Trigger: model.CoordinationTriggerRunInvariant,
			Severity: model.CoordinationSeverityCritical,
			Summary:  "Run recovery requires deterministic intervention",
			Details:  encoded,
		},
	)
	if err != nil || !created {
		t.Fatalf(
			"create run-invariant incident: created=%t value=%+v error=%v",
			created,
			incident,
			err,
		)
	}
	return incident
}

func TestCoordinationProposalCannotDismissOperatorRecoveryFence(t *testing.T) {
	for _, authorization := range []CoordinationApplyAuthorization{
		CoordinationApplyValidatedAuto,
		CoordinationApplyApproved,
	} {
		t.Run(string(authorization), func(t *testing.T) {
			ctx := context.Background()
			opened, err := Open(":memory:", "default", "")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			assignee := "worker"
			task, err := opened.CreateTask(ctx, CreateTaskInput{
				Title: "operator-owned recovery", Assignee: &assignee,
				Runtime: model.RuntimeCodex,
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := opened.ClaimTask(
				ctx,
				ClaimOptions{TaskID: task.Task.ID},
			)
			if err != nil || claim == nil {
				t.Fatalf("claim task: claim=%+v err=%v", claim, err)
			}
			auxiliary, err := opened.CreateTask(ctx, CreateTaskInput{
				Title: "unrelated visible recovery note",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := opened.RequireObservedRunRecoveryIntervention(
				ctx,
				ObserveRunForRecovery(claim.Run, nil),
				30,
				"process containment is unavailable",
				model.RunStatusReclaimed,
				false,
			); err != nil {
				t.Fatal(err)
			}
			reclaim, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
			if err != nil || reclaim == nil {
				t.Fatalf("operator fence: value=%+v err=%v", reclaim, err)
			}
			diagnosticCode := ""
			if reclaim.DiagnosticCode != nil {
				diagnosticCode = *reclaim.DiagnosticCode
			}
			incident := runInvariantIncident(
				t,
				ctx,
				opened,
				claim.Task.Task,
				map[string]any{
					"reason":          "operator_recovery_required",
					"currentRunId":    claim.Run.ID,
					"diagnosticCode":  diagnosticCode,
					"fenceGeneration": reclaim.FenceGeneration,
				},
			)
			priority := 99
			fixture := applicableProposalForIncident(
				t,
				ctx,
				opened,
				incident,
				[]coordinationAction{{
					Kind: coordinationActionUpdatePriority, TaskID: auxiliary.Task.ID,
					ExpectedUpdatedAt: auxiliary.Task.UpdatedAt, Priority: &priority,
					Reason: "make the unsafe run visible",
				}},
				authorization,
			)
			_, err = opened.ApplyCoordinationProposal(
				ctx,
				fixture.proposal.ID,
				applyFixtureInput(fixture, authorization),
			)
			if err == nil || !strings.Contains(err.Error(), "retains operator recovery fence") {
				t.Fatalf("operator fence postcondition error = %v", err)
			}
			currentTask, taskErr := opened.GetTask(ctx, auxiliary.Task.ID)
			currentIncident, incidentErr := opened.GetCoordinationIncident(ctx, incident.ID)
			currentProposal, proposalErr := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
			if taskErr != nil || incidentErr != nil || proposalErr != nil {
				t.Fatalf(
					"read rollback: task=%v incident=%v proposal=%v",
					taskErr,
					incidentErr,
					proposalErr,
				)
			}
			expectedIncidentStatus := model.CoordinationIncidentCoordinating
			expectedProposalStatus := model.CoordinationProposalValidated
			if authorization == CoordinationApplyApproved {
				expectedIncidentStatus = model.CoordinationIncidentAwaitingApproval
				expectedProposalStatus = model.CoordinationProposalApproved
			}
			if currentTask.Task.Priority == priority ||
				currentIncident.Status != expectedIncidentStatus ||
				currentProposal.Status != expectedProposalStatus {
				t.Fatalf(
					"operator fence apply was not atomic: task=%+v incident=%+v proposal=%+v",
					currentTask.Task,
					currentIncident,
					currentProposal,
				)
			}
		})
	}
}

func TestCoordinationProposalCannotDismissPendingRecoveryCheckpoint(t *testing.T) {
	ctx := context.Background()
	fixture := newRecoveryCheckpointFixture(t, 3)
	checkpoint, detail, err := fixture.store.RegisterRecoveryCheckpointAndFailRun(
		ctx,
		RunScope{
			RunID:      fixture.claim.Run.ID,
			ClaimToken: fixture.claim.ClaimToken,
		},
		recoveryCheckpointInput(
			fixture.claim.Run.ID,
			"/worktree/coordinator-postcondition",
			'a',
			'd',
		),
		"worker failed before checkpoint adoption",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	)
	if err != nil {
		t.Fatal(err)
	}
	detail, err = fixture.store.BlockTask(ctx, detail.Task.ID, BlockInput{
		Kind:   model.BlockKindNeedsInput,
		Reason: "Recovery checkpoint adoption failed: manual inspection required",
	})
	if err != nil {
		t.Fatal(err)
	}
	incident := runInvariantIncident(
		t,
		ctx,
		fixture.store,
		detail.Task,
		map[string]any{
			"reason":          "recovery_checkpoint_adoption_exhausted",
			"checkpointId":    checkpoint.ID,
			"checkpointState": checkpoint.State,
		},
	)
	priority := 98
	proposal := applicableProposalForIncident(
		t,
		ctx,
		fixture.store,
		incident,
		[]coordinationAction{{
			Kind: coordinationActionUpdatePriority, TaskID: detail.Task.ID,
			ExpectedUpdatedAt: detail.Task.UpdatedAt, Priority: &priority,
			Reason: "prioritize manual checkpoint inspection",
		}},
		CoordinationApplyValidatedAuto,
	)
	_, err = fixture.store.ApplyCoordinationProposal(
		ctx,
		proposal.proposal.ID,
		applyFixtureInput(proposal, CoordinationApplyValidatedAuto),
	)
	if err == nil || !strings.Contains(err.Error(), "retains pending recovery checkpoint") {
		t.Fatalf("checkpoint postcondition error = %v", err)
	}
}
