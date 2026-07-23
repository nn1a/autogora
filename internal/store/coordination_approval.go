package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

// CoordinationApprovalResult keeps the proposal and its incident from the same
// transaction snapshot. Callers should use these returned versions for the next
// approval lifecycle operation instead of re-reading the records separately.
type CoordinationApprovalResult struct {
	Proposal model.CoordinationProposal `json:"proposal"`
	Incident model.CoordinationIncident `json:"incident"`
}

type RequestCoordinationApprovalInput struct {
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
}

type ApproveCoordinationProposalInput struct {
	ExpectedUpdatedAt     string
	ExpectedGraphRevision *int64
}

type RejectCoordinationProposalInput struct {
	ExpectedUpdatedAt     string
	ExpectedGraphRevision *int64
}

type SupersedeCoordinationProposalInput struct {
	ExpectedUpdatedAt string
	ClaimToken        string
	Current           time.Time
}

func requireCoordinationApprovalID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("coordination approval operation requires a proposal ID")
	}
	return id, nil
}

func requireCoordinationProposalVersion(
	proposal model.CoordinationProposal,
	expectedUpdatedAt string,
) error {
	expectedUpdatedAt = strings.TrimSpace(expectedUpdatedAt)
	if expectedUpdatedAt == "" {
		return errors.New("coordination approval operation requires the proposal updatedAt")
	}
	if proposal.UpdatedAt != expectedUpdatedAt {
		return &CoordinationStateConflictError{
			Kind:     "proposal version",
			ID:       proposal.ID,
			Expected: expectedUpdatedAt,
			Actual:   proposal.UpdatedAt,
		}
	}
	return nil
}

func requireCoordinationApprovalGraph(
	ctx context.Context,
	q querier,
	proposal model.CoordinationProposal,
	incident model.CoordinationIncident,
	expected *int64,
) error {
	if expected == nil {
		return errors.New("coordination approval operation requires an expected graph revision")
	}
	if proposal.ExpectedGraphRevision != *expected {
		return &GraphRevisionConflictError{
			Board: incident.Board, Expected: *expected, Actual: proposal.ExpectedGraphRevision,
		}
	}
	if incident.GraphRevision != *expected {
		return &GraphRevisionConflictError{
			Board: incident.Board, Expected: *expected, Actual: incident.GraphRevision,
		}
	}
	_, err := requireBoardGraphRevision(ctx, q, incident.Board, *expected)
	return err
}

func requireCoordinationApprovalState(
	kind, id string,
	actual string,
	expected ...string,
) error {
	for _, candidate := range expected {
		if actual == candidate {
			return nil
		}
	}
	return &CoordinationStateConflictError{
		Kind:     kind,
		ID:       id,
		Expected: strings.Join(expected, " or "),
		Actual:   actual,
	}
}

func requireSingleCoordinationApprovalUpdate(
	result sql.Result,
	kind, id, expected string,
) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return &CoordinationStateConflictError{
			Kind: kind, ID: id, Expected: expected, Actual: "changed concurrently",
		}
	}
	return nil
}

// RequestCoordinationApproval atomically hands a validated proposal from its
// current coordinator lease to a human approver. The claim is cleared only
// after both records enter awaiting_approval.
func (s *Store) RequestCoordinationApproval(
	ctx context.Context,
	proposalID string,
	input RequestCoordinationApprovalInput,
) (CoordinationApprovalResult, error) {
	proposalID, err := requireCoordinationApprovalID(proposalID)
	if err != nil {
		return CoordinationApprovalResult{}, err
	}
	input.ClaimToken = strings.TrimSpace(input.ClaimToken)
	if input.ClaimToken == "" {
		return CoordinationApprovalResult{}, errors.New("coordination approval request requires a claim token")
	}

	var result CoordinationApprovalResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, err := proposalWithIncident(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if err := requireCoordinationApprovalGraph(ctx, tx, proposal, incident, input.ExpectedGraphRevision); err != nil {
			return err
		}
		if !emptyJSONArray(proposal.ValidationErrors) {
			return errors.New("a coordination proposal with validation errors cannot await approval")
		}

		// A retry after the atomic transition is harmless. A mixed pair is not:
		// it signals corruption or a caller racing another lifecycle operation.
		if proposal.Status == model.CoordinationProposalAwaitingApproval &&
			incident.Status == model.CoordinationIncidentAwaitingApproval {
			if incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
				return fmt.Errorf("awaiting approval incident %s still has a claim lease", incident.ID)
			}
			result = CoordinationApprovalResult{Proposal: proposal, Incident: incident}
			return nil
		}
		if err := requireCoordinationApprovalState(
			"proposal", proposal.ID, string(proposal.Status),
			string(model.CoordinationProposalValidated),
		); err != nil {
			return err
		}
		if err := requireCoordinationApprovalState(
			"incident", incident.ID, string(incident.Status),
			string(model.CoordinationIncidentCoordinating),
		); err != nil {
			return err
		}
		if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
			return fmt.Errorf("coordinating incident %s has no claim lease", incident.ID)
		}
		if incident.ClaimToken != input.ClaimToken {
			return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
		}
		current, currentTimestamp, err := normalizeCoordinationClaimTime(input.Current)
		if err != nil {
			return err
		}
		expired, err := coordinationIncidentClaimExpired(incident, current)
		if err != nil {
			return err
		}
		if expired {
			return fmt.Errorf("%w: %s", ErrCoordinationClaimExpired, incident.ID)
		}

		timestamp := now()
		proposalUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = 'awaiting_approval', updated_at = ?
			WHERE id = ? AND incident_id = ? AND status = 'validated'
				AND expected_graph_revision = ? AND updated_at = ? AND validation_errors_json = ?
		`, timestamp, proposal.ID, incident.ID, proposal.ExpectedGraphRevision,
			proposal.UpdatedAt, string(proposal.ValidationErrors))
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			proposalUpdate, "proposal", proposal.ID, string(model.CoordinationProposalValidated),
		); err != nil {
			return err
		}
		incidentUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_incidents
			SET status = 'awaiting_approval', claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND status = 'coordinating' AND graph_revision = ?
				AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at > ?
		`, timestamp, incident.ID, incident.GraphRevision, input.ClaimToken,
			*incident.ClaimExpiresAt, currentTimestamp)
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			incidentUpdate, "incident", incident.ID, string(model.CoordinationIncidentCoordinating),
		); err != nil {
			return err
		}

		proposal.Status = model.CoordinationProposalAwaitingApproval
		proposal.UpdatedAt = timestamp
		incident.Status = model.CoordinationIncidentAwaitingApproval
		incident.ClaimToken = ""
		incident.ClaimExpiresAt = nil
		incident.UpdatedAt = timestamp
		result = CoordinationApprovalResult{Proposal: proposal, Incident: incident}
		return nil
	})
	return result, err
}

// ApproveCoordinationProposal records a human decision without mutating the
// graph. Applying the proposal remains a separate, CAS-protected operation.
func (s *Store) ApproveCoordinationProposal(
	ctx context.Context,
	proposalID string,
	input ApproveCoordinationProposalInput,
) (CoordinationApprovalResult, error) {
	proposalID, err := requireCoordinationApprovalID(proposalID)
	if err != nil {
		return CoordinationApprovalResult{}, err
	}

	var result CoordinationApprovalResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, err := proposalWithIncident(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if err := requireCoordinationProposalVersion(proposal, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requireCoordinationApprovalGraph(ctx, tx, proposal, incident, input.ExpectedGraphRevision); err != nil {
			return err
		}
		if err := requireCoordinationApprovalState(
			"proposal", proposal.ID, string(proposal.Status),
			string(model.CoordinationProposalAwaitingApproval),
		); err != nil {
			return err
		}
		if err := requireCoordinationApprovalState(
			"incident", incident.ID, string(incident.Status),
			string(model.CoordinationIncidentAwaitingApproval),
		); err != nil {
			return err
		}
		if !emptyJSONArray(proposal.ValidationErrors) {
			return errors.New("a coordination proposal with validation errors cannot be approved")
		}

		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = 'approved', updated_at = ?
			WHERE id = ? AND incident_id = ? AND status = 'awaiting_approval'
				AND expected_graph_revision = ? AND updated_at = ? AND validation_errors_json = ?
		`, timestamp, proposal.ID, incident.ID, proposal.ExpectedGraphRevision,
			proposal.UpdatedAt, string(proposal.ValidationErrors))
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			update, "proposal", proposal.ID, string(model.CoordinationProposalAwaitingApproval),
		); err != nil {
			return err
		}
		proposal.Status = model.CoordinationProposalApproved
		proposal.UpdatedAt = timestamp
		result = CoordinationApprovalResult{Proposal: proposal, Incident: incident}
		return nil
	})
	return result, err
}

// RejectCoordinationProposal dismisses the incident in the same transaction so
// a human rejection cannot be immediately reproposed by Autopilot.
func (s *Store) RejectCoordinationProposal(
	ctx context.Context,
	proposalID string,
	input RejectCoordinationProposalInput,
) (CoordinationApprovalResult, error) {
	proposalID, err := requireCoordinationApprovalID(proposalID)
	if err != nil {
		return CoordinationApprovalResult{}, err
	}

	var result CoordinationApprovalResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, err := proposalWithIncident(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if err := requireCoordinationProposalVersion(proposal, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requireCoordinationApprovalGraph(ctx, tx, proposal, incident, input.ExpectedGraphRevision); err != nil {
			return err
		}
		if err := requireCoordinationApprovalState(
			"proposal", proposal.ID, string(proposal.Status),
			string(model.CoordinationProposalAwaitingApproval),
		); err != nil {
			return err
		}
		if err := requireCoordinationApprovalState(
			"incident", incident.ID, string(incident.Status),
			string(model.CoordinationIncidentAwaitingApproval),
		); err != nil {
			return err
		}

		timestamp := now()
		proposalUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = 'rejected', updated_at = ?
			WHERE id = ? AND incident_id = ? AND status = 'awaiting_approval'
				AND expected_graph_revision = ? AND updated_at = ?
		`, timestamp, proposal.ID, incident.ID, proposal.ExpectedGraphRevision, proposal.UpdatedAt)
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			proposalUpdate, "proposal", proposal.ID, string(model.CoordinationProposalAwaitingApproval),
		); err != nil {
			return err
		}
		incidentUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_incidents
			SET status = 'dismissed', updated_at = ?
			WHERE id = ? AND status = 'awaiting_approval' AND graph_revision = ?
				AND claim_token IS NULL AND claim_expires_at IS NULL
		`, timestamp, incident.ID, incident.GraphRevision)
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			incidentUpdate, "incident", incident.ID, string(model.CoordinationIncidentAwaitingApproval),
		); err != nil {
			return err
		}

		proposal.Status = model.CoordinationProposalRejected
		proposal.UpdatedAt = timestamp
		incident.Status = model.CoordinationIncidentDismissed
		incident.UpdatedAt = timestamp
		result = CoordinationApprovalResult{Proposal: proposal, Incident: incident}
		return nil
	})
	return result, err
}

// SupersedeCoordinationProposal recovers an in-flight or approval-pending
// proposal after its snapshot becomes obsolete. It intentionally does not
// require the proposal revision to match the live board graph.
func (s *Store) SupersedeCoordinationProposal(
	ctx context.Context,
	proposalID string,
	input SupersedeCoordinationProposalInput,
) (CoordinationApprovalResult, error) {
	proposalID, err := requireCoordinationApprovalID(proposalID)
	if err != nil {
		return CoordinationApprovalResult{}, err
	}

	var result CoordinationApprovalResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, err := proposalWithIncident(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if err := requireCoordinationProposalVersion(proposal, input.ExpectedUpdatedAt); err != nil {
			return err
		}

		var claimTimestamp string
		switch incident.Status {
		case model.CoordinationIncidentCoordinating:
			if err := requireCoordinationApprovalState(
				"proposal", proposal.ID, string(proposal.Status),
				string(model.CoordinationProposalDraft),
				string(model.CoordinationProposalValidating),
				string(model.CoordinationProposalValidated),
			); err != nil {
				return err
			}
			if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
				return fmt.Errorf("coordinating incident %s has no claim lease", incident.ID)
			}
			if strings.TrimSpace(input.ClaimToken) != incident.ClaimToken {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
			}
			current, timestamp, err := normalizeCoordinationClaimTime(input.Current)
			if err != nil {
				return err
			}
			expired, err := coordinationIncidentClaimExpired(incident, current)
			if err != nil {
				return err
			}
			if expired {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimExpired, incident.ID)
			}
			claimTimestamp = timestamp
		case model.CoordinationIncidentAwaitingApproval:
			if err := requireCoordinationApprovalState(
				"proposal", proposal.ID, string(proposal.Status),
				string(model.CoordinationProposalAwaitingApproval),
				string(model.CoordinationProposalApproved),
			); err != nil {
				return err
			}
			if incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
				return fmt.Errorf("awaiting approval incident %s still has a claim lease", incident.ID)
			}
		default:
			return &CoordinationStateConflictError{
				Kind: "incident", ID: incident.ID,
				Expected: string(model.CoordinationIncidentCoordinating) + " or " +
					string(model.CoordinationIncidentAwaitingApproval),
				Actual: string(incident.Status),
			}
		}

		timestamp := now()
		proposalUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = 'superseded', updated_at = ?
			WHERE id = ? AND incident_id = ? AND status = ?
				AND expected_graph_revision = ? AND updated_at = ?
		`, timestamp, proposal.ID, incident.ID, proposal.Status,
			proposal.ExpectedGraphRevision, proposal.UpdatedAt)
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			proposalUpdate, "proposal", proposal.ID, string(proposal.Status),
		); err != nil {
			return err
		}

		statement := `
			UPDATE coordination_incidents
			SET status = 'open', claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND status = ? AND graph_revision = ?`
		arguments := []any{timestamp, incident.ID, incident.Status, incident.GraphRevision}
		if incident.Status == model.CoordinationIncidentCoordinating {
			statement += " AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at > ?"
			arguments = append(arguments, incident.ClaimToken, *incident.ClaimExpiresAt, claimTimestamp)
		} else {
			statement += " AND claim_token IS NULL AND claim_expires_at IS NULL"
		}
		incidentUpdate, err := tx.ExecContext(ctx, statement, arguments...)
		if err != nil {
			return err
		}
		if err := requireSingleCoordinationApprovalUpdate(
			incidentUpdate, "incident", incident.ID, string(incident.Status),
		); err != nil {
			return err
		}

		proposal.Status = model.CoordinationProposalSuperseded
		proposal.UpdatedAt = timestamp
		incident.Status = model.CoordinationIncidentOpen
		incident.ClaimToken = ""
		incident.ClaimExpiresAt = nil
		incident.UpdatedAt = timestamp
		result = CoordinationApprovalResult{Proposal: proposal, Incident: incident}
		return nil
	})
	return result, err
}
