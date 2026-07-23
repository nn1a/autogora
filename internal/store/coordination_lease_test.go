package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestRenewCoordinationIncidentClaimExtendsOnlyLiveOwner(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	start := time.Now().UTC()
	claimed, won, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               start,
		},
	)
	if err != nil || !won {
		t.Fatalf("claim: won=%t incident=%+v error=%v", won, claimed, err)
	}
	renewAt := start.Add(MinCoordinationIncidentClaimTTL - time.Second)
	renewed, err := opened.RenewCoordinationIncidentClaim(
		ctx,
		incident.ID,
		RenewCoordinationIncidentClaimInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            claimed.ClaimToken,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               renewAt,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, *renewed.ClaimExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := renewAt.Add(MinCoordinationIncidentClaimTTL); !expiry.Equal(want) {
		t.Fatalf("renewed expiry = %s, want %s", expiry, want)
	}
	notShortened, err := opened.RenewCoordinationIncidentClaim(
		ctx,
		incident.ID,
		RenewCoordinationIncidentClaimInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            claimed.ClaimToken,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               start,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	notShortenedExpiry, err := time.Parse(
		time.RFC3339Nano,
		*notShortened.ClaimExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !notShortenedExpiry.Equal(expiry) {
		t.Fatalf(
			"backward clock shortened expiry from %s to %s",
			expiry,
			notShortenedExpiry,
		)
	}
	if _, err := opened.RenewCoordinationIncidentClaim(
		ctx,
		incident.ID,
		RenewCoordinationIncidentClaimInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            "another-owner",
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               renewAt,
		},
	); !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("wrong-owner renewal error = %v", err)
	}
	if _, err := opened.RenewCoordinationIncidentClaim(
		ctx,
		incident.ID,
		RenewCoordinationIncidentClaimInput{
			ExpectedGraphRevision: revisionPointer(incident.GraphRevision),
			ClaimToken:            claimed.ClaimToken,
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               expiry,
		},
	); !errors.Is(err, ErrCoordinationClaimExpired) {
		t.Fatalf("expired renewal error = %v", err)
	}
}

func TestCancelCoordinationAttemptReservationReleasesUnconsumedClaim(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	current := time.Now().UTC()
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		reserveAttemptInput("cancel-before-call", incident, incident.GraphRevision, current),
	)
	if err != nil || !reserved.Reserved {
		t.Fatalf("reserve: %+v, %v", reserved, err)
	}
	revision := reserved.Incident.GraphRevision
	if err := opened.CancelCoordinationAttemptReservation(
		ctx,
		reserved.Attempt.ID,
		CancelCoordinationAttemptReservationInput{
			Board: "default", IncidentID: incident.ID,
			ExpectedIncidentGraphRevision: &revision,
			ClaimToken:                    reserved.Incident.ClaimToken,
		},
	); err != nil {
		t.Fatal(err)
	}
	released, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		ctx,
		CoordinationAttemptFilter{IncidentID: incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != model.CoordinationIncidentOpen ||
		released.ClaimToken != "" || released.ClaimExpiresAt != nil ||
		len(attempts) != 0 {
		t.Fatalf("canceled reservation: incident=%+v attempts=%+v", released, attempts)
	}
}

func TestCancelCoordinationAttemptReservationRejectsDurableProposal(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	current := time.Now().UTC()
	reserved, err := opened.ReserveCoordinationAttempt(
		ctx,
		reserveAttemptInput("proposal-exists", incident, incident.GraphRevision, current),
	)
	if err != nil || !reserved.Reserved {
		t.Fatalf("reserve: %+v, %v", reserved, err)
	}
	revision := reserved.Incident.GraphRevision
	if _, _, err := opened.CreateCoordinationProposal(
		ctx,
		CreateCoordinationProposalInput{
			IncidentID: reserved.Incident.ID, AttemptID: &reserved.Attempt.ID,
			CoordinatorAgent: "coordinator", Status: model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision, ClaimToken: reserved.Incident.ClaimToken,
			Current: current.Add(time.Second), Summary: "durable result",
			Rationale: "analysis crossed the paid-call boundary",
		},
	); err != nil {
		t.Fatal(err)
	}
	err = opened.CancelCoordinationAttemptReservation(
		ctx,
		reserved.Attempt.ID,
		CancelCoordinationAttemptReservationInput{
			Board: "default", IncidentID: incident.ID,
			ExpectedIncidentGraphRevision: &revision,
			ClaimToken:                    reserved.Incident.ClaimToken,
		},
	)
	if !errors.Is(err, ErrCoordinationStateConflict) {
		t.Fatalf("proposal-backed cancellation error = %v", err)
	}
}

func TestCancelCoordinationAttemptReservationTreatsReclaimAsOwnershipLoss(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident := createAttemptTestIncident(
		t,
		opened,
		"default",
		model.CoordinationTriggerGraphStalled,
	)
	current := time.Now().UTC()
	firstInput := reserveAttemptInput(
		"first-owner",
		incident,
		incident.GraphRevision,
		current,
	)
	firstInput.TTL = MinCoordinationIncidentClaimTTL
	first, err := opened.ReserveCoordinationAttempt(ctx, firstInput)
	if err != nil || !first.Reserved {
		t.Fatalf("first reservation: %+v, %v", first, err)
	}
	reclaimAt := current.Add(MinCoordinationIncidentClaimTTL)
	secondInput := reserveAttemptInput(
		"second-owner",
		incident,
		incident.GraphRevision,
		reclaimAt,
	)
	secondInput.TTL = MinCoordinationIncidentClaimTTL
	second, err := opened.ReserveCoordinationAttempt(ctx, secondInput)
	if err != nil || !second.Reserved ||
		second.Incident.ClaimToken == first.Incident.ClaimToken {
		t.Fatalf("second reservation: %+v, %v", second, err)
	}
	revision := first.Incident.GraphRevision
	err = opened.CancelCoordinationAttemptReservation(
		ctx,
		first.Attempt.ID,
		CancelCoordinationAttemptReservationInput{
			Board: "default", IncidentID: incident.ID,
			ExpectedIncidentGraphRevision: &revision,
			ClaimToken:                    first.Incident.ClaimToken,
		},
	)
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("reclaimed reservation cancellation error = %v", err)
	}
}
