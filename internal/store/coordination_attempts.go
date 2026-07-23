package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

const (
	MaxCoordinationAttemptErrorBytes = 4 * 1024
	MaxCoordinationAttemptCalls      = 100

	maxCoordinationAttemptBoardBytes    = 128
	maxCoordinationAttemptAgentBytes    = 128
	maxCoordinationAttemptModelBytes    = 256
	maxCoordinationAttemptProviderBytes = 128
	maxCoordinationAttemptSourceBytes   = 128

	coordinationAttemptTimestampLayout = "2006-01-02T15:04:05.000000000Z"
	coordinationLeaseExpiredError      = "coordination lease expired"
)

type StartCoordinationAttemptInput struct {
	ID               string
	IncidentID       string
	Board            string
	SelectedAgent    string
	SelectedRuntime  model.Runtime
	SelectedModel    string
	SelectedProvider string
	SelectedSource   string
}

type FinishCoordinationAttemptInput struct {
	Board            string
	ExpectedStatus   model.CoordinationAttemptStatus
	Status           model.CoordinationAttemptStatus
	SelectedAgent    string
	SelectedRuntime  model.Runtime
	SelectedModel    string
	SelectedProvider string
	SelectedSource   string
	Error            *string
}

// RecoverCoordinationAttemptInput closes the one logical analysis call that
// produced a durable proposal before its normal attempt-finish write
// completed. The live incident lease and both graph revisions bind recovery
// to the Supervisor that currently owns that exact proposal.
type RecoverCoordinationAttemptInput struct {
	Board                         string
	ProposalID                    string
	ExpectedProposalStatus        model.CoordinationProposalStatus
	ExpectedProposalGraphRevision *int64
	ExpectedIncidentGraphRevision *int64
	ClaimToken                    string
	Current                       time.Time
	Status                        model.CoordinationAttemptStatus
	Error                         *string
}

type ReserveCoordinationAttemptInput struct {
	ID                    string
	IncidentID            string
	Board                 string
	ExpectedGraphRevision *int64
	Since                 time.Time
	Current               time.Time
	MaxCalls              int
	TTL                   time.Duration
}

type ReserveCoordinationAttemptResult struct {
	Incident        model.CoordinationIncident `json:"incident"`
	Attempt         model.CoordinationAttempt  `json:"attempt"`
	Reserved        bool                       `json:"reserved"`
	BudgetExhausted bool                       `json:"budgetExhausted"`
	RetryAt         *string                    `json:"retryAt"`
}

// CancelCoordinationAttemptReservationInput identifies an analysis reservation
// that has not crossed the external-call boundary. Cancellation removes that
// unconsumed budget record and releases the exact incident claim atomically.
type CancelCoordinationAttemptReservationInput struct {
	Board                         string
	IncidentID                    string
	ExpectedIncidentGraphRevision *int64
	ClaimToken                    string
}

type CoordinationAttemptFilter struct {
	Board      string
	IncidentID string
	Status     model.CoordinationAttemptStatus
	Limit      int
}

const coordinationAttemptColumns = `id, incident_id, board, status,
	selected_agent, selected_runtime, selected_model, selected_provider, selected_source,
	error, started_at, ended_at`

func scanCoordinationAttempt(row scanner) (model.CoordinationAttempt, error) {
	var value model.CoordinationAttempt
	var attemptError, endedAt sql.NullString
	err := row.Scan(
		&value.ID, &value.IncidentID, &value.Board, &value.Status,
		&value.SelectedAgent, &value.SelectedRuntime, &value.SelectedModel,
		&value.SelectedProvider, &value.SelectedSource,
		&attemptError, &value.StartedAt, &endedAt,
	)
	value.Error = stringPointer(attemptError)
	value.EndedAt = stringPointer(endedAt)
	return value, err
}

func normalizedCoordinationAttemptText(value, field string, maxBytes int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return "", fmt.Errorf("%s cannot be empty", field)
		}
		return "", nil
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("%s cannot contain NUL", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("%s must be at most %d bytes", field, maxBytes)
	}
	return value, nil
}

func normalizeCoordinationAttemptBoard(value, fallback string) (string, error) {
	return normalizedCoordinationAttemptText(
		normalizedBoard(value, fallback),
		"coordination attempt board",
		maxCoordinationAttemptBoardBytes,
		true,
	)
}

func boundedCoordinationAttemptError(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(strings.ToValidUTF8(*value, "\uFFFD"))
	if trimmed == "" {
		return nil
	}
	if len(trimmed) <= MaxCoordinationAttemptErrorBytes {
		return &trimmed
	}
	bounded := trimmed[:MaxCoordinationAttemptErrorBytes]
	for !utf8.ValidString(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	return &bounded
}

func validCoordinationAttemptID(value, field string) (string, error) {
	value, err := validRecordID(value, field)
	if err != nil || value == "" {
		return value, err
	}
	return normalizedCoordinationAttemptText(value, field, 128, false)
}

func normalizeStartCoordinationAttempt(
	raw StartCoordinationAttemptInput,
	fallbackBoard string,
) (StartCoordinationAttemptInput, error) {
	var err error
	raw.ID, err = validCoordinationAttemptID(raw.ID, "coordination attempt id")
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	raw.IncidentID, err = validCoordinationAttemptID(raw.IncidentID, "coordination attempt incident id")
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	if raw.IncidentID == "" {
		return StartCoordinationAttemptInput{}, errors.New("coordination attempt requires an incident ID")
	}
	raw.Board, err = normalizeCoordinationAttemptBoard(raw.Board, fallbackBoard)
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	raw.SelectedAgent, err = normalizedCoordinationAttemptText(
		raw.SelectedAgent, "coordination attempt selected agent", maxCoordinationAttemptAgentBytes, false,
	)
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	if raw.SelectedRuntime != "" && !model.ValidRuntime(raw.SelectedRuntime) {
		return StartCoordinationAttemptInput{}, fmt.Errorf(
			"invalid coordination attempt selected runtime: %s",
			raw.SelectedRuntime,
		)
	}
	raw.SelectedModel, err = normalizedCoordinationAttemptText(
		raw.SelectedModel, "coordination attempt selected model", maxCoordinationAttemptModelBytes, false,
	)
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	raw.SelectedProvider, err = normalizedCoordinationAttemptText(
		raw.SelectedProvider, "coordination attempt selected provider", maxCoordinationAttemptProviderBytes, false,
	)
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	raw.SelectedSource, err = normalizedCoordinationAttemptText(
		raw.SelectedSource, "coordination attempt selected source", maxCoordinationAttemptSourceBytes, false,
	)
	if err != nil {
		return StartCoordinationAttemptInput{}, err
	}
	return raw, nil
}

func sameCoordinationAttemptStart(
	existing model.CoordinationAttempt,
	input StartCoordinationAttemptInput,
) bool {
	return existing.IncidentID == input.IncidentID &&
		existing.Board == input.Board &&
		(input.SelectedAgent == "" || existing.SelectedAgent == input.SelectedAgent) &&
		(input.SelectedRuntime == "" || existing.SelectedRuntime == input.SelectedRuntime) &&
		(input.SelectedModel == "" || existing.SelectedModel == input.SelectedModel) &&
		(input.SelectedProvider == "" || existing.SelectedProvider == input.SelectedProvider) &&
		(input.SelectedSource == "" || existing.SelectedSource == input.SelectedSource)
}

func coordinationAttemptNow() string {
	value := now()
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// now is generated in this package and is always RFC3339Nano. Retain
		// its value rather than turning an audit write into a process panic if
		// that invariant is ever changed.
		return value
	}
	return timestamp.Format(coordinationAttemptTimestampLayout)
}

func normalizeReserveCoordinationAttempt(
	raw ReserveCoordinationAttemptInput,
	fallbackBoard string,
) (ReserveCoordinationAttemptInput, time.Time, string, string, string, error) {
	start, err := normalizeStartCoordinationAttempt(StartCoordinationAttemptInput{
		ID: raw.ID, IncidentID: raw.IncidentID, Board: raw.Board,
	}, fallbackBoard)
	if err != nil {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", err
	}
	raw.ID, raw.IncidentID, raw.Board = start.ID, start.IncidentID, start.Board
	if raw.ID == "" {
		raw.ID = newID("ca")
	}
	if raw.ExpectedGraphRevision == nil {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt reservation requires an expected graph revision",
		)
	}
	if *raw.ExpectedGraphRevision < 0 {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt graph revision cannot be negative",
		)
	}
	if raw.MaxCalls < 1 || raw.MaxCalls > MaxCoordinationAttemptCalls {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", fmt.Errorf(
			"coordination attempt max calls must be between 1 and %d",
			MaxCoordinationAttemptCalls,
		)
	}
	if raw.TTL < MinCoordinationIncidentClaimTTL || raw.TTL > MaxCoordinationIncidentClaimTTL {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", fmt.Errorf(
			"coordination attempt reservation TTL must be between %s and %s",
			MinCoordinationIncidentClaimTTL,
			MaxCoordinationIncidentClaimTTL,
		)
	}
	if raw.Current.IsZero() {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt reservation requires a current time",
		)
	}
	current, currentTimestamp, err := normalizeCoordinationClaimTime(raw.Current)
	if err != nil {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", err
	}
	if raw.Since.IsZero() {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt reservation requires a budget start time",
		)
	}
	since := raw.Since.UTC()
	if since.Year() < 0 || since.Year() > 9999 {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt budget start time must fit RFC3339",
		)
	}
	if !since.Before(current) {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt budget start time must be before current time",
		)
	}
	expires := current.Add(raw.TTL)
	if expires.Year() < 0 || expires.Year() > 9999 {
		return ReserveCoordinationAttemptInput{}, time.Time{}, "", "", "", errors.New(
			"coordination attempt reservation expiry must fit RFC3339",
		)
	}
	raw.Current = current
	raw.Since = since
	return raw, current, currentTimestamp,
		since.Format(coordinationAttemptTimestampLayout),
		expires.Format(coordinationIncidentClaimTimestampLayout),
		nil
}

func coordinationAttemptBudget(
	ctx context.Context,
	q querier,
	board, since string,
) (int, *string, error) {
	var count int
	var oldest sql.NullString
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(started_at)
		FROM coordination_attempts
		WHERE board = ? AND started_at >= ?
	`, board, since).Scan(&count, &oldest); err != nil {
		return 0, nil, err
	}
	return count, stringPointer(oldest), nil
}

func coordinationAttemptRetryAt(
	oldest *string,
	window time.Duration,
) (*string, error) {
	if oldest == nil {
		return nil, nil
	}
	started, err := time.Parse(time.RFC3339Nano, *oldest)
	if err != nil {
		return nil, fmt.Errorf("parse oldest coordination attempt start: %w", err)
	}
	// Budget counting is inclusive at Since, so retry one nanosecond after
	// the oldest call leaves the same rolling window.
	retryAt := started.UTC().Add(window).Add(time.Nanosecond).
		Format(coordinationAttemptTimestampLayout)
	return &retryAt, nil
}

// ReserveCoordinationAttempt atomically enforces the rolling call budget,
// acquires an incident lease, expires abandoned attempts, and records the new
// logical analysis call. A live claim is observed but never stolen.
func (s *Store) ReserveCoordinationAttempt(
	ctx context.Context,
	raw ReserveCoordinationAttemptInput,
) (ReserveCoordinationAttemptResult, error) {
	input, current, currentTimestamp, sinceTimestamp, expiresAt, err :=
		normalizeReserveCoordinationAttempt(raw, s.board)
	if err != nil {
		return ReserveCoordinationAttemptResult{}, err
	}

	var result ReserveCoordinationAttemptResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		incident, getErr := scanCoordinationIncident(tx.QueryRowContext(
			ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?",
			input.IncidentID,
		))
		if errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", input.IncidentID)
		}
		if getErr != nil {
			return getErr
		}
		if incident.Board != input.Board {
			return fmt.Errorf(
				"coordination incident %s belongs to board %s, not %s",
				incident.ID,
				incident.Board,
				input.Board,
			)
		}
		state, stateErr := readBoardGraphState(ctx, tx, incident.Board)
		if stateErr != nil {
			return stateErr
		}
		expectedRevision := *input.ExpectedGraphRevision
		if state.Revision != expectedRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: expectedRevision, Actual: state.Revision,
			}
		}
		// A non-winning reservation may expose incident state, but never the
		// current owner's capability token. Exact retries and new winners
		// overwrite this scrubbed value below.
		result.Incident = incident
		result.Incident.ClaimToken = ""

		existing, existingErr := scanCoordinationAttempt(tx.QueryRowContext(
			ctx,
			"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
			input.ID,
		))
		hasExisting := existingErr == nil
		if hasExisting && !sameCoordinationAttemptStart(existing, StartCoordinationAttemptInput{
			ID: input.ID, IncidentID: input.IncidentID, Board: input.Board,
		}) {
			return fmt.Errorf(
				"coordination attempt id %s is already used for another attempt",
				input.ID,
			)
		}
		if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}

		expiredClaim := false
		switch incident.Status {
		case model.CoordinationIncidentOpen:
			if incident.GraphRevision != expectedRevision {
				return &GraphRevisionConflictError{
					Board: incident.Board, Expected: expectedRevision, Actual: incident.GraphRevision,
				}
			}
			if incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
				return fmt.Errorf("open coordination incident %s has an invalid claim", incident.ID)
			}
		case model.CoordinationIncidentCoordinating:
			if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
				return fmt.Errorf("coordinating incident %s has no claim lease", incident.ID)
			}
			var expiryErr error
			expiredClaim, expiryErr = coordinationIncidentClaimExpired(incident, current)
			if expiryErr != nil {
				return expiryErr
			}
			if !expiredClaim {
				if hasExisting && existing.Status == model.CoordinationAttemptStarted &&
					incident.GraphRevision == expectedRevision {
					result.Incident = incident
					result.Attempt = existing
					result.Reserved = true
				}
				return nil
			}
		default:
			return nil
		}
		blocked, blockErr := graphStalledClaimBlockedByAutoDecompose(
			ctx, tx, incident, current,
		)
		if blockErr != nil {
			return blockErr
		}
		if blocked {
			return nil
		}
		if hasExisting {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: input.ID,
				Expected: "unused reservation id", Actual: string(existing.Status),
			}
		}

		count, oldest, countErr := coordinationAttemptBudget(
			ctx,
			tx,
			input.Board,
			sinceTimestamp,
		)
		if countErr != nil {
			return countErr
		}
		if count >= input.MaxCalls {
			retryAt, retryErr := coordinationAttemptRetryAt(
				oldest,
				current.Sub(input.Since),
			)
			if retryErr != nil {
				return retryErr
			}
			result.BudgetExhausted = true
			result.RetryAt = retryAt
			return nil
		}

		token, tokenErr := claimToken()
		if tokenErr != nil {
			return fmt.Errorf("generate coordination incident claim token: %w", tokenErr)
		}
		updatedAt := now()
		var claimResult sql.Result
		var claimErr error
		if incident.Status == model.CoordinationIncidentOpen {
			claimResult, claimErr = tx.ExecContext(ctx, `
				UPDATE coordination_incidents
				SET status = 'coordinating', claim_token = ?, claim_expires_at = ?, updated_at = ?
				WHERE id = ? AND board = ? AND status = 'open' AND graph_revision = ?
					AND claim_token IS NULL AND claim_expires_at IS NULL
			`, token, expiresAt, updatedAt, incident.ID, incident.Board, expectedRevision)
		} else if expiredClaim {
			claimResult, claimErr = tx.ExecContext(ctx, `
				UPDATE coordination_incidents
				SET graph_revision = ?, claim_token = ?, claim_expires_at = ?, updated_at = ?
				WHERE id = ? AND board = ? AND status = 'coordinating' AND graph_revision = ?
					AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at <= ?
			`, expectedRevision, token, expiresAt, updatedAt,
				incident.ID, incident.Board, incident.GraphRevision,
				incident.ClaimToken, *incident.ClaimExpiresAt, currentTimestamp)
		}
		if claimErr != nil {
			return claimErr
		}
		if claimResult == nil {
			return fmt.Errorf(
				"coordination incident %s is not claimable in status %s",
				incident.ID,
				incident.Status,
			)
		}
		changed, err := claimResult.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			latest, latestErr := scanCoordinationIncident(tx.QueryRowContext(
				ctx,
				"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?",
				incident.ID,
			))
			if latestErr != nil {
				return latestErr
			}
			result.Incident = latest
			result.Incident.ClaimToken = ""
			return nil
		}

		expiredError := coordinationLeaseExpiredError
		if _, err := tx.ExecContext(ctx, `
			UPDATE coordination_attempts
			SET status = 'failed', error = ?, ended_at = ?
			WHERE incident_id = ? AND board = ? AND status = 'started'
				AND error IS NULL AND ended_at IS NULL
		`, expiredError, currentTimestamp, incident.ID, incident.Board); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coordination_attempts(
				id, incident_id, board, status,
				selected_agent, selected_runtime, selected_model, selected_provider, selected_source,
				error, started_at, ended_at
			) VALUES (?, ?, ?, 'started', '', '', '', '', '', NULL, ?, NULL)
		`, input.ID, incident.ID, incident.Board, currentTimestamp); err != nil {
			return err
		}

		incident.Status = model.CoordinationIncidentCoordinating
		incident.GraphRevision = expectedRevision
		incident.ClaimToken = token
		incident.ClaimExpiresAt = &expiresAt
		incident.UpdatedAt = updatedAt
		attempt := model.CoordinationAttempt{
			ID: input.ID, IncidentID: incident.ID, Board: incident.Board,
			Status: model.CoordinationAttemptStarted, StartedAt: currentTimestamp,
		}
		result.Incident = incident
		result.Attempt = attempt
		result.Reserved = true
		return nil
	})
	return result, err
}

// CancelCoordinationAttemptReservation releases a claim only while its started
// attempt has produced no proposal. Once a proposal exists, the durable
// attempt/proposal recovery path owns the record and cancellation is rejected.
func (s *Store) CancelCoordinationAttemptReservation(
	ctx context.Context,
	attemptID string,
	raw CancelCoordinationAttemptReservationInput,
) error {
	attemptID = strings.TrimSpace(attemptID)
	if attemptID == "" {
		return errors.New("coordination attempt cancellation requires an attempt ID")
	}
	input := raw
	var err error
	input.Board, err = normalizeCoordinationAttemptBoard(input.Board, s.board)
	if err != nil {
		return err
	}
	input.IncidentID, err = validCoordinationAttemptID(
		input.IncidentID,
		"coordination attempt cancellation incident id",
	)
	if err != nil {
		return err
	}
	if input.IncidentID == "" {
		return errors.New("coordination attempt cancellation requires an incident ID")
	}
	if input.ExpectedIncidentGraphRevision == nil {
		return errors.New(
			"coordination attempt cancellation requires an expected incident graph revision",
		)
	}
	input.ClaimToken = strings.TrimSpace(input.ClaimToken)
	if input.ClaimToken == "" {
		return errors.New("coordination attempt cancellation requires a claim token")
	}

	return s.withWrite(ctx, func(tx *sql.Tx) error {
		// Check the capability first. If another process already reclaimed the
		// incident, its transaction may also have terminalized or removed this
		// reservation; both cases are simply loss of ownership to this caller.
		incident, incidentErr := scanCoordinationIncident(tx.QueryRowContext(
			ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?",
			input.IncidentID,
		))
		if errors.Is(incidentErr, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", input.IncidentID)
		}
		if incidentErr != nil {
			return incidentErr
		}
		if incident.Board != input.Board ||
			incident.Status != model.CoordinationIncidentCoordinating ||
			incident.ClaimToken != input.ClaimToken {
			return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
		}
		expectedRevision := *input.ExpectedIncidentGraphRevision
		if incident.GraphRevision != expectedRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: expectedRevision,
				Actual: incident.GraphRevision,
			}
		}
		attempt, getErr := scanCoordinationAttempt(tx.QueryRowContext(
			ctx,
			"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
			attemptID,
		))
		if errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("coordination attempt not found: %s", attemptID)
		}
		if getErr != nil {
			return getErr
		}
		if attempt.IncidentID != input.IncidentID || attempt.Board != input.Board {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: attempt.ID,
				Expected: input.Board + "/" + input.IncidentID,
				Actual:   attempt.Board + "/" + attempt.IncidentID,
			}
		}
		if attempt.Status != model.CoordinationAttemptStarted ||
			attempt.EndedAt != nil {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: attempt.ID,
				Expected: string(model.CoordinationAttemptStarted),
				Actual:   string(attempt.Status),
			}
		}
		var proposalCount int
		if err := tx.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM coordination_proposals WHERE attempt_id = ?",
			attempt.ID,
		).Scan(&proposalCount); err != nil {
			return err
		}
		if proposalCount != 0 {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: attempt.ID,
				Expected: "no durable proposal", Actual: "proposal created",
			}
		}
		updatedAt := now()
		released, err := tx.ExecContext(ctx, `
			UPDATE coordination_incidents
			SET status = 'open', claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'coordinating'
				AND graph_revision = ? AND claim_token = ?
		`, updatedAt, incident.ID, incident.Board, expectedRevision, input.ClaimToken)
		if err != nil {
			return err
		}
		changed, err := released.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
		}
		deleted, err := tx.ExecContext(ctx, `
			DELETE FROM coordination_attempts
			WHERE id = ? AND incident_id = ? AND board = ?
				AND status = 'started' AND ended_at IS NULL
		`, attempt.ID, incident.ID, incident.Board)
		if err != nil {
			return err
		}
		changed, err = deleted.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: attempt.ID,
				Expected: string(model.CoordinationAttemptStarted),
				Actual:   "changed while canceling",
			}
		}
		return nil
	})
}

// StartCoordinationAttempt persists one logical Coordinator analysis call.
// Retrying an explicit ID with the same immutable selection is idempotent.
func (s *Store) StartCoordinationAttempt(
	ctx context.Context,
	raw StartCoordinationAttemptInput,
) (model.CoordinationAttempt, bool, error) {
	input, err := normalizeStartCoordinationAttempt(raw, s.board)
	if err != nil {
		return model.CoordinationAttempt{}, false, err
	}
	if input.ID == "" {
		input.ID = newID("ca")
	}

	var result model.CoordinationAttempt
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		existing, getErr := scanCoordinationAttempt(tx.QueryRowContext(
			ctx,
			"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
			input.ID,
		))
		if getErr == nil {
			if !sameCoordinationAttemptStart(existing, input) {
				return fmt.Errorf(
					"coordination attempt id %s is already used for another attempt",
					input.ID,
				)
			}
			result = existing
			return nil
		}
		if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}

		incident, getErr := scanCoordinationIncident(tx.QueryRowContext(
			ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?",
			input.IncidentID,
		))
		if errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", input.IncidentID)
		}
		if getErr != nil {
			return getErr
		}
		if incident.Board != input.Board {
			return fmt.Errorf(
				"coordination incident %s belongs to board %s, not %s",
				incident.ID,
				incident.Board,
				input.Board,
			)
		}
		if !activeIncidentStatus(incident.Status) {
			return fmt.Errorf(
				"cannot start coordination attempt for terminal incident %s in status %s",
				incident.ID,
				incident.Status,
			)
		}

		startedAt := coordinationAttemptNow()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coordination_attempts(
				id, incident_id, board, status,
				selected_agent, selected_runtime, selected_model, selected_provider, selected_source,
				error, started_at, ended_at
			) VALUES (?, ?, ?, 'started', ?, ?, ?, ?, ?, NULL, ?, NULL)
		`, input.ID, input.IncidentID, input.Board,
			input.SelectedAgent, input.SelectedRuntime, input.SelectedModel,
			input.SelectedProvider, input.SelectedSource, startedAt); err != nil {
			return err
		}
		result = model.CoordinationAttempt{
			ID: input.ID, IncidentID: input.IncidentID, Board: input.Board,
			Status:        model.CoordinationAttemptStarted,
			SelectedAgent: input.SelectedAgent, SelectedRuntime: input.SelectedRuntime,
			SelectedModel: input.SelectedModel, SelectedProvider: input.SelectedProvider,
			SelectedSource: input.SelectedSource, StartedAt: startedAt,
		}
		created = true
		return nil
	})
	return result, created, err
}

func normalizeFinishCoordinationAttempt(
	raw FinishCoordinationAttemptInput,
	fallbackBoard string,
) (FinishCoordinationAttemptInput, error) {
	var err error
	raw.Board, err = normalizeCoordinationAttemptBoard(raw.Board, fallbackBoard)
	if err != nil {
		return FinishCoordinationAttemptInput{}, err
	}
	if raw.ExpectedStatus == "" {
		raw.ExpectedStatus = model.CoordinationAttemptStarted
	}
	if raw.ExpectedStatus != model.CoordinationAttemptStarted {
		return FinishCoordinationAttemptInput{}, errors.New(
			"coordination attempt finish must expect started status",
		)
	}
	switch raw.Status {
	case model.CoordinationAttemptSucceeded, model.CoordinationAttemptFailed:
	default:
		return FinishCoordinationAttemptInput{}, fmt.Errorf(
			"coordination attempt finish status must be succeeded or failed: %s",
			raw.Status,
		)
	}
	raw.SelectedAgent, err = normalizedCoordinationAttemptText(
		raw.SelectedAgent, "coordination attempt selected agent", maxCoordinationAttemptAgentBytes, false,
	)
	if err != nil {
		return FinishCoordinationAttemptInput{}, err
	}
	if raw.SelectedRuntime != "" && !model.ValidRuntime(raw.SelectedRuntime) {
		return FinishCoordinationAttemptInput{}, fmt.Errorf(
			"invalid coordination attempt selected runtime: %s",
			raw.SelectedRuntime,
		)
	}
	raw.SelectedModel, err = normalizedCoordinationAttemptText(
		raw.SelectedModel, "coordination attempt selected model", maxCoordinationAttemptModelBytes, false,
	)
	if err != nil {
		return FinishCoordinationAttemptInput{}, err
	}
	raw.SelectedProvider, err = normalizedCoordinationAttemptText(
		raw.SelectedProvider, "coordination attempt selected provider", maxCoordinationAttemptProviderBytes, false,
	)
	if err != nil {
		return FinishCoordinationAttemptInput{}, err
	}
	raw.SelectedSource, err = normalizedCoordinationAttemptText(
		raw.SelectedSource, "coordination attempt selected source", maxCoordinationAttemptSourceBytes, false,
	)
	if err != nil {
		return FinishCoordinationAttemptInput{}, err
	}
	raw.Error = boundedCoordinationAttemptError(raw.Error)
	if raw.Status == model.CoordinationAttemptSucceeded && raw.Error != nil {
		return FinishCoordinationAttemptInput{}, errors.New(
			"succeeded coordination attempt cannot record an error",
		)
	}
	return raw, nil
}

func sameCoordinationAttemptFinish(
	existing model.CoordinationAttempt,
	input FinishCoordinationAttemptInput,
) bool {
	if existing.Status != input.Status ||
		(input.SelectedAgent != "" && existing.SelectedAgent != input.SelectedAgent) ||
		(input.SelectedRuntime != "" && existing.SelectedRuntime != input.SelectedRuntime) ||
		(input.SelectedModel != "" && existing.SelectedModel != input.SelectedModel) ||
		(input.SelectedProvider != "" && existing.SelectedProvider != input.SelectedProvider) ||
		(input.SelectedSource != "" && existing.SelectedSource != input.SelectedSource) {
		return false
	}
	if existing.Error == nil || input.Error == nil {
		return existing.Error == nil && input.Error == nil
	}
	return *existing.Error == *input.Error
}

func sameRecoveredCoordinationAttemptFinish(
	existing model.CoordinationAttempt,
	input FinishCoordinationAttemptInput,
) bool {
	if existing.Status != input.Status ||
		existing.SelectedAgent != input.SelectedAgent ||
		existing.SelectedModel != input.SelectedModel ||
		existing.SelectedProvider != input.SelectedProvider {
		return false
	}
	if existing.Error == nil || input.Error == nil {
		return existing.Error == nil && input.Error == nil
	}
	return *existing.Error == *input.Error
}

func fillCoordinationAttemptSelection(
	current model.CoordinationAttempt,
	input FinishCoordinationAttemptInput,
) (model.CoordinationAttempt, error) {
	type selectionField struct {
		name     string
		current  *string
		selected string
	}
	runtime := string(current.SelectedRuntime)
	fields := []selectionField{
		{name: "agent", current: &current.SelectedAgent, selected: input.SelectedAgent},
		{name: "runtime", current: &runtime, selected: string(input.SelectedRuntime)},
		{name: "model", current: &current.SelectedModel, selected: input.SelectedModel},
		{name: "provider", current: &current.SelectedProvider, selected: input.SelectedProvider},
		{name: "source", current: &current.SelectedSource, selected: input.SelectedSource},
	}
	for _, field := range fields {
		if field.selected == "" {
			continue
		}
		if *field.current != "" && *field.current != field.selected {
			return model.CoordinationAttempt{}, fmt.Errorf(
				"%w: coordination attempt %s selected %s is %q, expected %q",
				ErrCoordinationStateConflict,
				current.ID,
				field.name,
				*field.current,
				field.selected,
			)
		}
		*field.current = field.selected
	}
	current.SelectedRuntime = model.Runtime(runtime)
	return current, nil
}

// FinishCoordinationAttempt completes exactly one started attempt. Repeating
// the same terminal outcome is idempotent; any competing outcome is rejected.
func (s *Store) FinishCoordinationAttempt(
	ctx context.Context,
	id string,
	raw FinishCoordinationAttemptInput,
) (model.CoordinationAttempt, error) {
	id, err := validCoordinationAttemptID(id, "coordination attempt id")
	if err != nil {
		return model.CoordinationAttempt{}, err
	}
	if id == "" {
		return model.CoordinationAttempt{}, errors.New("coordination attempt finish requires an attempt ID")
	}
	input, err := normalizeFinishCoordinationAttempt(raw, s.board)
	if err != nil {
		return model.CoordinationAttempt{}, err
	}

	var result model.CoordinationAttempt
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, getErr := scanCoordinationAttempt(tx.QueryRowContext(
			ctx,
			"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
			id,
		))
		if errors.Is(getErr, sql.ErrNoRows) {
			return fmt.Errorf("coordination attempt not found: %s", id)
		}
		if getErr != nil {
			return getErr
		}
		if current.Board != input.Board {
			return fmt.Errorf(
				"coordination attempt %s belongs to board %s, not %s",
				id,
				current.Board,
				input.Board,
			)
		}
		if current.Status != input.ExpectedStatus {
			if sameCoordinationAttemptFinish(current, input) {
				result = current
				return nil
			}
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: id,
				Expected: string(input.ExpectedStatus), Actual: string(current.Status),
			}
		}
		selected, selectionErr := fillCoordinationAttemptSelection(current, input)
		if selectionErr != nil {
			return selectionErr
		}

		endedAt := coordinationAttemptNow()
		updateResult, updateErr := tx.ExecContext(ctx, `
			UPDATE coordination_attempts
			SET status = ?,
				selected_agent = ?, selected_runtime = ?, selected_model = ?,
				selected_provider = ?, selected_source = ?,
				error = ?, ended_at = ?
			WHERE id = ? AND board = ? AND status = 'started'
				AND selected_agent = ? AND selected_runtime = ? AND selected_model = ?
				AND selected_provider = ? AND selected_source = ?
				AND error IS NULL AND ended_at IS NULL
		`, input.Status,
			selected.SelectedAgent, selected.SelectedRuntime, selected.SelectedModel,
			selected.SelectedProvider, selected.SelectedSource,
			nullableString(input.Error), endedAt, id, input.Board,
			current.SelectedAgent, current.SelectedRuntime, current.SelectedModel,
			current.SelectedProvider, current.SelectedSource)
		if updateErr != nil {
			return updateErr
		}
		changed, updateErr := updateResult.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if changed != 1 {
			latest, latestErr := scanCoordinationAttempt(tx.QueryRowContext(
				ctx,
				"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
				id,
			))
			if latestErr != nil {
				return latestErr
			}
			if sameCoordinationAttemptFinish(latest, input) {
				result = latest
				return nil
			}
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: id,
				Expected: string(input.ExpectedStatus), Actual: string(latest.Status),
			}
		}
		selected.Status = input.Status
		selected.Error = input.Error
		selected.EndedAt = &endedAt
		result = selected
		return nil
	})
	return result, err
}

// RecoverCoordinationAttemptForProposal atomically finishes only the exact
// attempt durably bound to proposalID. Terminal attempts are never rewritten,
// and proposals without a paid-attempt binding are a no-op.
func (s *Store) RecoverCoordinationAttemptForProposal(
	ctx context.Context,
	raw RecoverCoordinationAttemptInput,
) (model.CoordinationAttempt, bool, error) {
	proposalID, err := validRecordID(raw.ProposalID, "coordination proposal id")
	if err != nil {
		return model.CoordinationAttempt{}, false, err
	}
	if proposalID == "" {
		return model.CoordinationAttempt{}, false, errors.New(
			"coordination attempt recovery requires a proposal ID",
		)
	}
	if !model.ValidCoordinationProposalStatus(raw.ExpectedProposalStatus) {
		return model.CoordinationAttempt{}, false, fmt.Errorf(
			"invalid expected coordination proposal status: %s",
			raw.ExpectedProposalStatus,
		)
	}
	if raw.ExpectedProposalGraphRevision == nil ||
		raw.ExpectedIncidentGraphRevision == nil {
		return model.CoordinationAttempt{}, false, errors.New(
			"coordination attempt recovery requires proposal and incident graph revisions",
		)
	}
	if *raw.ExpectedProposalGraphRevision < 0 ||
		*raw.ExpectedIncidentGraphRevision < 0 {
		return model.CoordinationAttempt{}, false, errors.New(
			"coordination attempt recovery graph revisions cannot be negative",
		)
	}
	finish, err := normalizeFinishCoordinationAttempt(FinishCoordinationAttemptInput{
		Board: raw.Board, Status: raw.Status, Error: raw.Error,
	}, s.board)
	if err != nil {
		return model.CoordinationAttempt{}, false, err
	}
	if raw.Current.IsZero() {
		return model.CoordinationAttempt{}, false, errors.New(
			"coordination attempt recovery requires a current time",
		)
	}

	var recovered model.CoordinationAttempt
	changed := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, getErr := proposalWithIncident(ctx, tx, proposalID)
		if getErr != nil {
			return getErr
		}
		if incident.Board != finish.Board {
			return fmt.Errorf(
				"coordination proposal %s belongs to board %s, not %s",
				proposal.ID,
				incident.Board,
				finish.Board,
			)
		}
		if incident.Status != model.CoordinationIncidentCoordinating {
			return &CoordinationStateConflictError{
				Kind: "incident", ID: incident.ID,
				Expected: string(model.CoordinationIncidentCoordinating),
				Actual:   string(incident.Status),
			}
		}
		if err := requireLiveCoordinationClaim(
			incident,
			raw.ClaimToken,
			raw.Current,
		); err != nil {
			return err
		}
		if incident.GraphRevision != *raw.ExpectedIncidentGraphRevision {
			return &GraphRevisionConflictError{
				Board:    incident.Board,
				Expected: *raw.ExpectedIncidentGraphRevision,
				Actual:   incident.GraphRevision,
			}
		}
		if _, err := requireBoardGraphRevision(
			ctx,
			tx,
			incident.Board,
			*raw.ExpectedIncidentGraphRevision,
		); err != nil {
			return err
		}
		if proposal.Status != raw.ExpectedProposalStatus {
			return &CoordinationStateConflictError{
				Kind: "proposal", ID: proposal.ID,
				Expected: string(raw.ExpectedProposalStatus),
				Actual:   string(proposal.Status),
			}
		}
		if proposal.ExpectedGraphRevision != *raw.ExpectedProposalGraphRevision {
			return &GraphRevisionConflictError{
				Board:    incident.Board,
				Expected: *raw.ExpectedProposalGraphRevision,
				Actual:   proposal.ExpectedGraphRevision,
			}
		}
		if proposal.AttemptID == nil {
			return nil
		}
		attempt, attemptErr := scanCoordinationAttempt(tx.QueryRowContext(
			ctx,
			"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE id = ?",
			*proposal.AttemptID,
		))
		if errors.Is(attemptErr, sql.ErrNoRows) {
			return fmt.Errorf(
				"bound coordination attempt not found: %s",
				*proposal.AttemptID,
			)
		}
		if attemptErr != nil {
			return attemptErr
		}
		if attempt.IncidentID != proposal.IncidentID || attempt.Board != incident.Board {
			return fmt.Errorf(
				"%w: proposal %s binds attempt %s for incident %s on board %s",
				ErrCoordinationStateConflict,
				proposal.ID,
				attempt.ID,
				attempt.IncidentID,
				attempt.Board,
			)
		}
		finish.SelectedAgent = proposal.CoordinatorAgent
		finish.SelectedModel = proposal.CoordinatorModel
		finish.SelectedProvider = proposal.CoordinatorProvider
		if attempt.Status != model.CoordinationAttemptStarted {
			if sameRecoveredCoordinationAttemptFinish(attempt, finish) {
				recovered = attempt
				return nil
			}
			return &CoordinationStateConflictError{
				Kind: "attempt recovery", ID: attempt.ID,
				Expected: string(finish.Status) + " with matching selection and error",
				Actual:   string(attempt.Status) + " with a different terminal result",
			}
		}

		selected, selectionErr := fillCoordinationAttemptSelection(attempt, finish)
		if selectionErr != nil {
			return selectionErr
		}
		endedAt := coordinationAttemptNow()
		updateResult, updateErr := tx.ExecContext(ctx, `
			UPDATE coordination_attempts
			SET status = ?,
				selected_agent = ?, selected_runtime = ?, selected_model = ?,
				selected_provider = ?, selected_source = ?,
				error = ?, ended_at = ?
			WHERE id = ? AND incident_id = ? AND board = ? AND status = 'started'
				AND selected_agent = ? AND selected_runtime = ? AND selected_model = ?
				AND selected_provider = ? AND selected_source = ?
				AND error IS NULL AND ended_at IS NULL
		`, finish.Status,
			selected.SelectedAgent, selected.SelectedRuntime, selected.SelectedModel,
			selected.SelectedProvider, selected.SelectedSource,
			nullableString(finish.Error), endedAt,
			selected.ID, proposal.IncidentID, incident.Board,
			attempt.SelectedAgent, attempt.SelectedRuntime, attempt.SelectedModel,
			attempt.SelectedProvider, attempt.SelectedSource)
		if updateErr != nil {
			return updateErr
		}
		affected, updateErr := updateResult.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if affected != 1 {
			return &CoordinationStateConflictError{
				Kind: "attempt", ID: selected.ID,
				Expected: string(model.CoordinationAttemptStarted),
				Actual:   "changed concurrently",
			}
		}
		selected.Status = finish.Status
		selected.Error = finish.Error
		selected.EndedAt = &endedAt
		recovered = selected
		changed = true
		return nil
	})
	return recovered, changed, err
}

// CountCoordinationAttemptsSince counts logical analysis calls when they
// started, independent of whether they later succeeded or failed.
func (s *Store) CountCoordinationAttemptsSince(
	ctx context.Context,
	board string,
	since time.Time,
) (int, error) {
	board, err := normalizeCoordinationAttemptBoard(board, s.board)
	if err != nil {
		return 0, err
	}
	if since.IsZero() {
		return 0, errors.New("coordination attempt count requires a since time")
	}
	since = since.UTC()
	if since.Year() < 0 || since.Year() > 9999 {
		return 0, errors.New("coordination attempt since time must fit RFC3339")
	}
	var count int
	err = s.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM coordination_attempts WHERE board = ? AND started_at >= ?",
		board,
		since.Format(coordinationAttemptTimestampLayout),
	).Scan(&count)
	return count, err
}

// ListCoordinationAttempts returns board-scoped immutable audit records.
func (s *Store) ListCoordinationAttempts(
	ctx context.Context,
	raw CoordinationAttemptFilter,
) ([]model.CoordinationAttempt, error) {
	board, err := normalizeCoordinationAttemptBoard(raw.Board, s.board)
	if err != nil {
		return nil, err
	}
	clauses := []string{"board = ?"}
	values := []any{board}
	if strings.TrimSpace(raw.IncidentID) != "" {
		incidentID, err := validCoordinationAttemptID(raw.IncidentID, "coordination attempt incident id")
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, "incident_id = ?")
		values = append(values, incidentID)
	}
	if raw.Status != "" {
		if !model.ValidCoordinationAttemptStatus(raw.Status) {
			return nil, fmt.Errorf("invalid coordination attempt status: %s", raw.Status)
		}
		clauses = append(clauses, "status = ?")
		values = append(values, raw.Status)
	}
	limit := raw.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return nil, errors.New("coordination attempt limit must be between 1 and 500")
	}
	values = append(values, limit)
	rows, err := s.db.QueryContext(
		ctx,
		"SELECT "+coordinationAttemptColumns+" FROM coordination_attempts WHERE "+
			strings.Join(clauses, " AND ")+" ORDER BY started_at DESC, id DESC LIMIT ?",
		values...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.CoordinationAttempt, 0)
	for rows.Next() {
		value, err := scanCoordinationAttempt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
