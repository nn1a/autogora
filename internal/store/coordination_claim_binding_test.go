package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type coordinationClaimBindingFixture struct {
	incident model.CoordinationIncident
	claimAt  time.Time
}

func claimBindingRevision(value int64) *int64 { return &value }

func seedClaimedCoordinationIncident(t *testing.T, opened *Store) coordinationClaimBindingFixture {
	t.Helper()
	ctx := context.Background()
	incident, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger:               model.CoordinationTriggerGraphStalled,
		Severity:              model.CoordinationSeverityWarning,
		ExpectedGraphRevision: claimBindingRevision(0),
		Summary:               "The graph has no runnable task",
	})
	if err != nil || !created {
		t.Fatalf("create incident: created=%v incident=%+v err=%v", created, incident, err)
	}
	claimAt := time.Date(2032, time.March, 4, 5, 6, 7, 0, time.UTC)
	incident, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: claimBindingRevision(0),
		TTL:                   time.Minute,
		Current:               claimAt,
	})
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v incident=%+v err=%v", claimed, incident, err)
	}
	return coordinationClaimBindingFixture{incident: incident, claimAt: claimAt}
}

func claimBoundProposalInput(
	fixture coordinationClaimBindingFixture,
	id, claimToken string,
	current time.Time,
) CreateCoordinationProposalInput {
	return CreateCoordinationProposalInput{
		ID:                    id,
		IncidentID:            fixture.incident.ID,
		CoordinatorAgent:      "coordinator",
		CoordinatorModel:      "test-model",
		CoordinatorProvider:   "test-provider",
		ExpectedGraphRevision: claimBindingRevision(0),
		ClaimToken:            claimToken,
		Current:               current,
		Summary:               "Restore a runnable route",
		Rationale:             "Deterministic recovery was exhausted.",
		Actions:               []byte(`[]`),
	}
}

func TestCreateCoordinationProposalRejectsUnclaimedIncident(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger:               model.CoordinationTriggerAgentExhausted,
		Severity:              model.CoordinationSeverityWarning,
		ExpectedGraphRevision: claimBindingRevision(0),
		Summary:               "No configured agent is available",
	})
	if err != nil || !created {
		t.Fatalf("create incident: created=%v incident=%+v err=%v", created, incident, err)
	}
	fixture := coordinationClaimBindingFixture{incident: incident, claimAt: time.Now().UTC()}
	_, created, err = opened.CreateCoordinationProposal(ctx,
		claimBoundProposalInput(fixture, "proposal-without-lease", "", fixture.claimAt))
	if !errors.Is(err, ErrCoordinationStateConflict) || created {
		t.Fatalf("proposal created before incident claim: created=%v err=%v", created, err)
	}
}

func TestCreateCoordinationProposalRequiresCurrentClaimOwner(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedClaimedCoordinationIncident(t, opened)

	for name, token := range map[string]string{
		"missing": "",
		"wrong":   "another-owner",
	} {
		t.Run(name, func(t *testing.T) {
			_, created, err := opened.CreateCoordinationProposal(ctx,
				claimBoundProposalInput(fixture, "proposal-"+name, token, fixture.claimAt.Add(time.Second)))
			if !errors.Is(err, ErrCoordinationClaimNotOwner) || created {
				t.Fatalf("create with %s claim: created=%v err=%v", name, created, err)
			}
		})
	}

	_, created, err := opened.CreateCoordinationProposal(ctx,
		claimBoundProposalInput(
			fixture, "proposal-expired", fixture.incident.ClaimToken, fixture.claimAt.Add(time.Minute),
		))
	if !errors.Is(err, ErrCoordinationClaimExpired) || created {
		t.Fatalf("create with expired claim: created=%v err=%v", created, err)
	}

	reclaimed, claimed, err := opened.ClaimCoordinationIncident(ctx, fixture.incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: claimBindingRevision(0),
			TTL:                   time.Minute,
			Current:               fixture.claimAt.Add(time.Minute),
		})
	if err != nil || !claimed || reclaimed.ClaimToken == fixture.incident.ClaimToken {
		t.Fatalf("reclaim incident: claimed=%v incident=%+v err=%v", claimed, reclaimed, err)
	}
	_, created, err = opened.CreateCoordinationProposal(ctx,
		claimBoundProposalInput(
			fixture, "proposal-stale-owner", fixture.incident.ClaimToken,
			fixture.claimAt.Add(time.Minute+time.Second),
		))
	if !errors.Is(err, ErrCoordinationClaimNotOwner) || created {
		t.Fatalf("stale owner create: created=%v err=%v", created, err)
	}

	input := claimBoundProposalInput(
		fixture, "proposal-current-owner", reclaimed.ClaimToken,
		fixture.claimAt.Add(time.Minute+time.Second),
	)
	proposal, created, err := opened.CreateCoordinationProposal(ctx, input)
	if err != nil || !created {
		t.Fatalf("current owner create: created=%v proposal=%+v err=%v", created, proposal, err)
	}

	// Idempotent ID handling must not let a retired owner observe or reuse the
	// current owner's proposal through the create path.
	input.ClaimToken = fixture.incident.ClaimToken
	_, created, err = opened.CreateCoordinationProposal(ctx, input)
	if !errors.Is(err, ErrCoordinationClaimNotOwner) || created {
		t.Fatalf("stale owner duplicate create: created=%v err=%v", created, err)
	}
}

func TestGenericProposalTransitionCannotBypassAtomicLifecycle(t *testing.T) {
	t.Run("approval and application", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)

		_, err = opened.TransitionCoordinationProposal(ctx, fixture.proposal.ID,
			TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalValidated,
				Status:                model.CoordinationProposalAwaitingApproval,
				ExpectedGraphRevision: claimBindingRevision(0),
				ClaimToken:            fixture.incident.ClaimToken,
				Current:               fixture.claimAt.Add(time.Second),
			})
		if err == nil {
			t.Fatal("generic transition entered approval")
		}
		awaiting, err := opened.RequestCoordinationApproval(ctx, fixture.proposal.ID,
			RequestCoordinationApprovalInput{
				ExpectedGraphRevision: claimBindingRevision(0),
				ClaimToken:            fixture.incident.ClaimToken,
				Current:               fixture.claimAt.Add(time.Second),
			})
		if err != nil {
			t.Fatal(err)
		}
		_, err = opened.TransitionCoordinationProposal(ctx, fixture.proposal.ID,
			TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalAwaitingApproval,
				Status:                model.CoordinationProposalApproved,
				ExpectedGraphRevision: claimBindingRevision(0),
			})
		if err == nil {
			t.Fatal("generic transition approved proposal")
		}
		approved, err := opened.ApproveCoordinationProposal(ctx, fixture.proposal.ID,
			ApproveCoordinationProposalInput{
				ExpectedUpdatedAt:     awaiting.Proposal.UpdatedAt,
				ExpectedGraphRevision: claimBindingRevision(0),
			})
		if err != nil {
			t.Fatal(err)
		}
		_, err = opened.TransitionCoordinationProposal(ctx, fixture.proposal.ID,
			TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalApproved,
				Status:                model.CoordinationProposalApplying,
				ExpectedGraphRevision: claimBindingRevision(0),
			})
		if err == nil {
			t.Fatal("generic transition entered application")
		}
		applied, err := opened.ApplyCoordinationProposal(ctx, fixture.proposal.ID,
			ApplyCoordinationProposalInput{
				Authorization:         CoordinationApplyApproved,
				ExpectedGraphRevision: claimBindingRevision(0),
				Current:               fixture.claimAt.Add(2 * time.Second),
			})
		if err != nil {
			t.Fatal(err)
		}
		if approved.Proposal.Status != model.CoordinationProposalApproved ||
			applied.Proposal.Status != model.CoordinationProposalApplied ||
			applied.Incident.Status != model.CoordinationIncidentResolved {
			t.Fatalf("atomic lifecycle = approved:%+v applied:%+v", approved, applied)
		}
	})

	t.Run("supersede", func(t *testing.T) {
		ctx := context.Background()
		opened, err := Open(":memory:", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		fixture := seedValidatedCoordinationProposal(t, opened)

		_, err = opened.TransitionCoordinationProposal(ctx, fixture.proposal.ID,
			TransitionCoordinationProposalInput{
				ExpectedStatus:        model.CoordinationProposalValidated,
				Status:                model.CoordinationProposalSuperseded,
				ExpectedGraphRevision: claimBindingRevision(0),
				ClaimToken:            fixture.incident.ClaimToken,
				Current:               fixture.claimAt.Add(time.Second),
			})
		if err == nil {
			t.Fatal("generic transition superseded proposal")
		}
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
			superseded.Incident.Status != model.CoordinationIncidentOpen {
			t.Fatalf("atomic supersede = %+v", superseded)
		}
	})
}

func TestCoordinationProposalMutationsFollowReclaimedLease(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	fixture := seedClaimedCoordinationIncident(t, opened)
	proposal, created, err := opened.CreateCoordinationProposal(ctx,
		claimBoundProposalInput(
			fixture, "proposal-mutation", fixture.incident.ClaimToken, fixture.claimAt.Add(time.Second),
		))
	if err != nil || !created {
		t.Fatalf("create proposal: created=%v proposal=%+v err=%v", created, proposal, err)
	}
	reclaimed, claimed, err := opened.ClaimCoordinationIncident(ctx, fixture.incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: claimBindingRevision(0),
			TTL:                   time.Minute,
			Current:               fixture.claimAt.Add(time.Minute),
		})
	if err != nil || !claimed {
		t.Fatalf("reclaim incident: claimed=%v incident=%+v err=%v", claimed, reclaimed, err)
	}
	mutationTime := fixture.claimAt.Add(time.Minute + time.Second)
	summary := "Retired owner must not edit this proposal"
	_, err = opened.UpdateCoordinationProposal(ctx, proposal.ID, UpdateCoordinationProposalInput{
		ExpectedStatus:        model.CoordinationProposalDraft,
		ExpectedGraphRevision: claimBindingRevision(0),
		ClaimToken:            fixture.incident.ClaimToken,
		Current:               mutationTime,
		Summary:               &summary,
	})
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("stale owner update error = %v", err)
	}
	_, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, TransitionCoordinationProposalInput{
		ExpectedStatus:        model.CoordinationProposalDraft,
		Status:                model.CoordinationProposalDraft,
		ExpectedGraphRevision: claimBindingRevision(0),
		ClaimToken:            fixture.incident.ClaimToken,
		Current:               mutationTime,
	})
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("stale owner idempotent transition error = %v", err)
	}
	stored, err := opened.GetCoordinationProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary != proposal.Summary || stored.Status != model.CoordinationProposalDraft {
		t.Fatalf("stale owner changed proposal: %+v", stored)
	}

	summary = "Current owner may edit this proposal"
	updated, err := opened.UpdateCoordinationProposal(ctx, proposal.ID, UpdateCoordinationProposalInput{
		ExpectedStatus:        model.CoordinationProposalDraft,
		ExpectedGraphRevision: claimBindingRevision(0),
		ClaimToken:            reclaimed.ClaimToken,
		Current:               mutationTime,
		Summary:               &summary,
	})
	if err != nil || updated.Summary != summary {
		t.Fatalf("current owner update: proposal=%+v err=%v", updated, err)
	}
	transitioned, err := opened.TransitionCoordinationProposal(ctx, proposal.ID,
		TransitionCoordinationProposalInput{
			ExpectedStatus:        model.CoordinationProposalDraft,
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: claimBindingRevision(0),
			ClaimToken:            reclaimed.ClaimToken,
			Current:               mutationTime,
		})
	if err != nil || transitioned.Status != model.CoordinationProposalValidating {
		t.Fatalf("current owner transition: proposal=%+v err=%v", transitioned, err)
	}

	_, err = opened.TransitionCoordinationProposal(ctx, proposal.ID,
		TransitionCoordinationProposalInput{
			ExpectedStatus:        model.CoordinationProposalValidating,
			Status:                model.CoordinationProposalValidated,
			ExpectedGraphRevision: claimBindingRevision(0),
			ClaimToken:            reclaimed.ClaimToken,
			Current:               fixture.claimAt.Add(2 * time.Minute),
		})
	if !errors.Is(err, ErrCoordinationClaimExpired) {
		t.Fatalf("expired current owner transition error = %v", err)
	}
	stored, err = opened.GetCoordinationProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.CoordinationProposalValidating {
		t.Fatalf("expired owner changed proposal: %+v", stored)
	}
}
