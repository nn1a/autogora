package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type coordinationApprovalFixture struct {
	incident model.CoordinationIncident
	proposal model.CoordinationProposal
	claimAt  time.Time
}

func approvalRevision(value int64) *int64 { return &value }

func bumpApprovalGraph(t *testing.T, opened *Store) {
	t.Helper()
	ctx := context.Background()
	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "graph parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := opened.CreateTask(ctx, CreateTaskInput{Title: "graph child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
}

func seedValidatedCoordinationProposal(
	t *testing.T,
	opened *Store,
) coordinationApprovalFixture {
	t.Helper()
	ctx := context.Background()
	incident, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger:               model.CoordinationTriggerRepeatedBlock,
		Severity:              model.CoordinationSeverityWarning,
		Summary:               "Task remains blocked",
		ExpectedGraphRevision: approvalRevision(0),
	})
	if err != nil || !created {
		t.Fatalf("create incident: created=%v value=%+v err=%v", created, incident, err)
	}
	claimAt := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	incident, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: approvalRevision(0),
		TTL:                   time.Minute,
		Current:               claimAt,
	})
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v value=%+v err=%v", claimed, incident, err)
	}
	proposal, created, err := opened.CreateCoordinationProposal(ctx, CreateCoordinationProposalInput{
		IncidentID:            incident.ID,
		CoordinatorAgent:      "coordinator",
		CoordinatorModel:      "model",
		CoordinatorProvider:   "provider",
		ExpectedGraphRevision: approvalRevision(0),
		Summary:               "Route the task to another worker",
		Rationale:             "The current worker cannot make progress.",
		Actions:               []byte(`[{"type":"set_route","taskId":"task-1","agent":"worker-2"}]`),
	})
	if err != nil || !created {
		t.Fatalf("create proposal: created=%v value=%+v err=%v", created, proposal, err)
	}
	for _, status := range []model.CoordinationProposalStatus{
		model.CoordinationProposalValidating,
		model.CoordinationProposalValidated,
	} {
		proposal, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, TransitionCoordinationProposalInput{
			ExpectedStatus:        proposal.Status,
			Status:                status,
			ExpectedGraphRevision: approvalRevision(0),
		})
		if err != nil {
			t.Fatalf("transition proposal to %s: %v", status, err)
		}
	}
	return coordinationApprovalFixture{incident: incident, proposal: proposal, claimAt: claimAt}
}

func requestFixtureApproval(
	t *testing.T,
	opened *Store,
	fixture coordinationApprovalFixture,
) CoordinationApprovalResult {
	t.Helper()
	result, err := opened.RequestCoordinationApproval(context.Background(), fixture.proposal.ID,
		RequestCoordinationApprovalInput{
			ExpectedGraphRevision: approvalRevision(fixture.proposal.ExpectedGraphRevision),
			ClaimToken:            fixture.incident.ClaimToken,
			Current:               fixture.claimAt.Add(time.Second),
		})
	if err != nil {
		t.Fatalf("request coordination approval: %v", err)
	}
	return result
}

func TestCoordinationApprovalRequestAndApprove(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedValidatedCoordinationProposal(t, opened)

	awaiting := requestFixtureApproval(t, opened, fixture)
	if awaiting.Proposal.Status != model.CoordinationProposalAwaitingApproval ||
		awaiting.Incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("awaiting pair = %+v", awaiting)
	}
	if awaiting.Incident.ClaimToken != "" || awaiting.Incident.ClaimExpiresAt != nil {
		t.Fatalf("approval request did not clear claim: %+v", awaiting.Incident)
	}

	// Retrying the same handoff after the claim was atomically cleared is safe.
	retried, err := opened.RequestCoordinationApproval(ctx, fixture.proposal.ID,
		RequestCoordinationApprovalInput{
			ExpectedGraphRevision: approvalRevision(0),
			ClaimToken:            fixture.incident.ClaimToken,
			Current:               fixture.claimAt.Add(2 * time.Second),
		})
	if err != nil {
		t.Fatalf("idempotent approval request: %v", err)
	}
	if retried.Proposal.UpdatedAt != awaiting.Proposal.UpdatedAt ||
		retried.Incident.UpdatedAt != awaiting.Incident.UpdatedAt {
		t.Fatalf("idempotent request rewrote pair: before=%+v after=%+v", awaiting, retried)
	}

	approved, err := opened.ApproveCoordinationProposal(ctx, awaiting.Proposal.ID,
		ApproveCoordinationProposalInput{
			ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
			ExpectedGraphRevision: approvalRevision(0),
		})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Proposal.Status != model.CoordinationProposalApproved ||
		approved.Incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("approved pair = %+v", approved)
	}
}

func TestCoordinationApprovalRequestRejectsWrongClaimAndMixedState(t *testing.T) {
	t.Run("wrong claim rolls back", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)

		_, err = opened.RequestCoordinationApproval(ctx, fixture.proposal.ID,
			RequestCoordinationApprovalInput{
				ExpectedGraphRevision: approvalRevision(0),
				ClaimToken:            "another-owner",
				Current:               fixture.claimAt.Add(time.Second),
			})
		if !errors.Is(err, ErrCoordinationClaimNotOwner) {
			t.Fatalf("wrong claim error = %v", err)
		}
		proposal, getErr := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		incident, getErr := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if proposal.Status != model.CoordinationProposalValidated ||
			incident.Status != model.CoordinationIncidentCoordinating ||
			incident.ClaimToken != fixture.incident.ClaimToken {
			t.Fatalf("wrong owner changed pair: proposal=%+v incident=%+v", proposal, incident)
		}
	})

	t.Run("mixed awaiting state is rejected", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		proposal, err := opened.TransitionCoordinationProposal(ctx, fixture.proposal.ID,
			TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalValidated,
				Status:                model.CoordinationProposalAwaitingApproval,
				ExpectedGraphRevision: approvalRevision(0),
			})
		if err != nil {
			t.Fatal(err)
		}
		_, err = opened.RequestCoordinationApproval(ctx, proposal.ID,
			RequestCoordinationApprovalInput{
				ExpectedGraphRevision: approvalRevision(0),
				ClaimToken:            fixture.incident.ClaimToken,
				Current:               fixture.claimAt.Add(time.Second),
			})
		if !errors.Is(err, ErrCoordinationStateConflict) {
			t.Fatalf("mixed state error = %v", err)
		}
		incident, getErr := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if incident.Status != model.CoordinationIncidentCoordinating ||
			incident.ClaimToken != fixture.incident.ClaimToken {
			t.Fatalf("mixed state changed incident: %+v", incident)
		}
	})

	t.Run("validation errors roll back", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		if _, err := opened.db.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET validation_errors_json = '["stale task version"]'
			WHERE id = ?
		`, fixture.proposal.ID); err != nil {
			t.Fatal(err)
		}

		_, err = opened.RequestCoordinationApproval(ctx, fixture.proposal.ID,
			RequestCoordinationApprovalInput{
				ExpectedGraphRevision: approvalRevision(0),
				ClaimToken:            fixture.incident.ClaimToken,
				Current:               fixture.claimAt.Add(time.Second),
			})
		if err == nil {
			t.Fatal("proposal with validation errors entered approval")
		}
		proposal, getErr := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		incident, getErr := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if proposal.Status != model.CoordinationProposalValidated ||
			incident.Status != model.CoordinationIncidentCoordinating ||
			incident.ClaimToken != fixture.incident.ClaimToken {
			t.Fatalf("invalid proposal changed pair: proposal=%+v incident=%+v", proposal, incident)
		}
	})
}

func TestCoordinationApprovalRejectDismissesIncident(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedValidatedCoordinationProposal(t, opened)
	awaiting := requestFixtureApproval(t, opened, fixture)

	rejected, err := opened.RejectCoordinationProposal(ctx, awaiting.Proposal.ID,
		RejectCoordinationProposalInput{
			ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
			ExpectedGraphRevision: approvalRevision(0),
		})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Proposal.Status != model.CoordinationProposalRejected ||
		rejected.Incident.Status != model.CoordinationIncidentDismissed {
		t.Fatalf("rejected pair = %+v", rejected)
	}
}

func TestCoordinationApprovalRejectRollsBackBothRecords(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedValidatedCoordinationProposal(t, opened)
	awaiting := requestFixtureApproval(t, opened, fixture)
	if _, err := opened.db.ExecContext(ctx, `
		CREATE TRIGGER reject_incident_failure
		BEFORE UPDATE OF status ON coordination_incidents
		WHEN NEW.status = 'dismissed'
		BEGIN
			SELECT RAISE(ABORT, 'injected incident rejection failure');
		END
	`); err != nil {
		t.Fatal(err)
	}

	_, err = opened.RejectCoordinationProposal(ctx, awaiting.Proposal.ID,
		RejectCoordinationProposalInput{
			ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
			ExpectedGraphRevision: approvalRevision(0),
		})
	if err == nil {
		t.Fatal("injected incident failure did not fail rejection")
	}
	proposal, getErr := opened.GetCoordinationProposal(ctx, awaiting.Proposal.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	incident, getErr := opened.GetCoordinationIncident(ctx, awaiting.Incident.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if proposal.Status != model.CoordinationProposalAwaitingApproval ||
		proposal.UpdatedAt != awaiting.Proposal.UpdatedAt ||
		incident.Status != model.CoordinationIncidentAwaitingApproval {
		t.Fatalf("failed rejection partially committed: proposal=%+v incident=%+v", proposal, incident)
	}
}

func TestCoordinationApprovalRejectsStaleVersionAndGraph(t *testing.T) {
	t.Run("stale proposal version", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		awaiting := requestFixtureApproval(t, opened, fixture)

		_, err = opened.ApproveCoordinationProposal(ctx, awaiting.Proposal.ID,
			ApproveCoordinationProposalInput{
				ExpectedUpdatedAt:     fixture.proposal.UpdatedAt,
				ExpectedGraphRevision: approvalRevision(0),
			})
		if !errors.Is(err, ErrCoordinationStateConflict) {
			t.Fatalf("stale proposal version error = %v", err)
		}
		proposal, getErr := opened.GetCoordinationProposal(ctx, awaiting.Proposal.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if proposal.Status != model.CoordinationProposalAwaitingApproval {
			t.Fatalf("stale version changed proposal: %+v", proposal)
		}
	})

	t.Run("stale graph revision", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		awaiting := requestFixtureApproval(t, opened, fixture)
		bumpApprovalGraph(t, opened)

		_, err = opened.ApproveCoordinationProposal(ctx, awaiting.Proposal.ID,
			ApproveCoordinationProposalInput{
				ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
				ExpectedGraphRevision: approvalRevision(0),
			})
		if !errors.Is(err, ErrGraphRevisionConflict) {
			t.Fatalf("stale graph error = %v", err)
		}
		proposal, getErr := opened.GetCoordinationProposal(ctx, awaiting.Proposal.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if proposal.Status != model.CoordinationProposalAwaitingApproval {
			t.Fatalf("stale graph changed proposal: %+v", proposal)
		}
	})
}

func TestSupersedeCoordinationProposalAllowsStaleGraph(t *testing.T) {
	t.Run("coordinator-owned validated proposal", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		bumpApprovalGraph(t, opened)

		superseded, err := opened.SupersedeCoordinationProposal(ctx, fixture.proposal.ID,
			SupersedeCoordinationProposalInput{
				ExpectedUpdatedAt: fixture.proposal.UpdatedAt,
				ClaimToken:        fixture.incident.ClaimToken,
				Current:           fixture.claimAt.Add(time.Second),
			})
		if err != nil {
			t.Fatal(err)
		}
		if superseded.Proposal.Status != model.CoordinationProposalSuperseded ||
			superseded.Incident.Status != model.CoordinationIncidentOpen ||
			superseded.Incident.ClaimToken != "" {
			t.Fatalf("superseded pair = %+v", superseded)
		}
	})

	t.Run("human-approved proposal", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)
		awaiting := requestFixtureApproval(t, opened, fixture)
		approved, err := opened.ApproveCoordinationProposal(ctx, awaiting.Proposal.ID,
			ApproveCoordinationProposalInput{
				ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
				ExpectedGraphRevision: approvalRevision(0),
			})
		if err != nil {
			t.Fatal(err)
		}
		bumpApprovalGraph(t, opened)

		superseded, err := opened.SupersedeCoordinationProposal(ctx, approved.Proposal.ID,
			SupersedeCoordinationProposalInput{ExpectedUpdatedAt: approved.Proposal.UpdatedAt})
		if err != nil {
			t.Fatal(err)
		}
		if superseded.Proposal.Status != model.CoordinationProposalSuperseded ||
			superseded.Incident.Status != model.CoordinationIncidentOpen {
			t.Fatalf("superseded approved pair = %+v", superseded)
		}
	})
}

func TestSupersedeCoordinationProposalPreventsClaimTheft(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedValidatedCoordinationProposal(t, opened)

	_, err = opened.SupersedeCoordinationProposal(ctx, fixture.proposal.ID,
		SupersedeCoordinationProposalInput{
			ExpectedUpdatedAt: fixture.proposal.UpdatedAt,
			ClaimToken:        "another-owner",
			Current:           fixture.claimAt.Add(time.Second),
		})
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("wrong supersede claim error = %v", err)
	}
	proposal, getErr := opened.GetCoordinationProposal(ctx, fixture.proposal.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	incident, getErr := opened.GetCoordinationIncident(ctx, fixture.incident.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if proposal.Status != model.CoordinationProposalValidated ||
		incident.Status != model.CoordinationIncidentCoordinating ||
		incident.ClaimToken != fixture.incident.ClaimToken {
		t.Fatalf("wrong owner superseded pair: proposal=%+v incident=%+v", proposal, incident)
	}
}
