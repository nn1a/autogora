package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrCoordinationStateConflict = errors.New("coordination state conflict")
	ErrCoordinationClaimNotOwner = errors.New("coordination incident claim is owned by another caller")
	ErrCoordinationClaimExpired  = errors.New("coordination incident claim has expired")
)

const (
	MinCoordinationIncidentClaimTTL = 5 * time.Second
	MaxCoordinationIncidentClaimTTL = 15 * time.Minute

	coordinationIncidentClaimTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type CoordinationStateConflictError struct {
	Kind     string
	ID       string
	Expected string
	Actual   string
}

func (e *CoordinationStateConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s %s is %s, expected %s", ErrCoordinationStateConflict, e.Kind, e.ID, e.Actual, e.Expected)
}

func (e *CoordinationStateConflictError) Unwrap() error { return ErrCoordinationStateConflict }

type CreateCoordinationIncidentInput struct {
	ID                    string
	Board                 string
	RootTaskID            *string
	TaskID                *string
	Trigger               model.CoordinationTrigger
	Severity              model.CoordinationSeverity
	Status                model.CoordinationIncidentStatus
	ExpectedGraphRevision *int64
	Summary               string
	Details               json.RawMessage
}

type CoordinationIncidentFilter struct {
	Board      string
	RootTaskID string
	TaskID     string
	Trigger    model.CoordinationTrigger
	Status     model.CoordinationIncidentStatus
	Limit      int
}

type UpdateCoordinationIncidentInput struct {
	ExpectedUpdatedAt     *string
	ExpectedGraphRevision *int64
	Severity              *model.CoordinationSeverity
	Summary               *string
	Details               *json.RawMessage
}

type TransitionCoordinationIncidentInput struct {
	ExpectedStatus        model.CoordinationIncidentStatus
	Status                model.CoordinationIncidentStatus
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
}

type ClaimCoordinationIncidentInput struct {
	ExpectedGraphRevision *int64
	TTL                   time.Duration
	Current               time.Time
}

type CreateCoordinationProposalInput struct {
	ID                    string
	IncidentID            string
	CoordinatorAgent      string
	CoordinatorModel      string
	CoordinatorProvider   string
	Status                model.CoordinationProposalStatus
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
	Summary               string
	Rationale             string
	Actions               json.RawMessage
	ValidationErrors      json.RawMessage
}

type CoordinationProposalFilter struct {
	IncidentID string
	Status     model.CoordinationProposalStatus
	Limit      int
}

type UpdateCoordinationProposalInput struct {
	ExpectedStatus        model.CoordinationProposalStatus
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
	Summary               *string
	Rationale             *string
	Actions               *json.RawMessage
	ValidationErrors      *json.RawMessage
}

type TransitionCoordinationProposalInput struct {
	ExpectedStatus        model.CoordinationProposalStatus
	Status                model.CoordinationProposalStatus
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
	ValidationErrors      *json.RawMessage
}

const incidentColumns = `id, board, root_task_id, task_id, trigger, severity, status,
	graph_revision, summary, details_json, claim_token, claim_expires_at, created_at, updated_at`

const proposalColumns = `id, incident_id, coordinator_agent, coordinator_model, coordinator_provider,
	status, expected_graph_revision, summary, rationale, actions_json, validation_errors_json,
	created_at, updated_at, applied_at`

func scanCoordinationIncident(row scanner) (model.CoordinationIncident, error) {
	var value model.CoordinationIncident
	var rootTaskID, taskID, claimToken, claimExpiresAt sql.NullString
	var details []byte
	err := row.Scan(
		&value.ID, &value.Board, &rootTaskID, &taskID, &value.Trigger, &value.Severity, &value.Status,
		&value.GraphRevision, &value.Summary, &details, &claimToken, &claimExpiresAt,
		&value.CreatedAt, &value.UpdatedAt,
	)
	value.RootTaskID = stringPointer(rootTaskID)
	value.TaskID = stringPointer(taskID)
	value.Details = append(json.RawMessage(nil), details...)
	if claimToken.Valid {
		value.ClaimToken = claimToken.String
	}
	value.ClaimExpiresAt = stringPointer(claimExpiresAt)
	return value, err
}

func scanCoordinationProposal(row scanner) (model.CoordinationProposal, error) {
	var value model.CoordinationProposal
	var actions, validationErrors []byte
	var appliedAt sql.NullString
	err := row.Scan(
		&value.ID, &value.IncidentID, &value.CoordinatorAgent, &value.CoordinatorModel, &value.CoordinatorProvider,
		&value.Status, &value.ExpectedGraphRevision, &value.Summary, &value.Rationale, &actions, &validationErrors,
		&value.CreatedAt, &value.UpdatedAt, &appliedAt,
	)
	value.Actions = append(json.RawMessage(nil), actions...)
	value.ValidationErrors = append(json.RawMessage(nil), validationErrors...)
	value.AppliedAt = stringPointer(appliedAt)
	return value, err
}

func normalizeJSON(raw json.RawMessage, fallback string, wantObject bool, field string) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		raw = json.RawMessage(fallback)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %w", field, err)
	}
	if wantObject {
		if _, ok := decoded.(map[string]any); !ok {
			return nil, fmt.Errorf("%s must be a JSON object", field)
		}
	} else if _, ok := decoded.([]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON array", field)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func validRecordID(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > 128 || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("%s must be at most 128 non-whitespace characters", field)
	}
	return value, nil
}

func requireCoordinationTaskBoard(ctx context.Context, q querier, board string, taskID *string, field string) error {
	if taskID == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*taskID)
	if trimmed == "" {
		return fmt.Errorf("%s cannot be empty", field)
	}
	task, err := requireTask(ctx, q, trimmed)
	if err != nil {
		return err
	}
	if task.Board != board {
		return fmt.Errorf("%s %s belongs to board %s, not %s", field, trimmed, task.Board, board)
	}
	*taskID = trimmed
	return nil
}

func activeIncidentStatus(status model.CoordinationIncidentStatus) bool {
	switch status {
	case model.CoordinationIncidentOpen, model.CoordinationIncidentCoordinating,
		model.CoordinationIncidentAwaitingApproval, model.CoordinationIncidentApplying:
		return true
	default:
		return false
	}
}

func normalizeCoordinationClaimTime(current time.Time) (time.Time, string, error) {
	if current.IsZero() {
		current = time.Now()
	}
	current = current.UTC()
	if current.Year() < 0 || current.Year() > 9999 {
		return time.Time{}, "", errors.New("coordination incident claim time must fit RFC3339")
	}
	return current, current.Format(coordinationIncidentClaimTimestampLayout), nil
}

func coordinationIncidentClaimExpired(incident model.CoordinationIncident, current time.Time) (bool, error) {
	if incident.ClaimExpiresAt == nil {
		return false, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, *incident.ClaimExpiresAt)
	if err != nil {
		return false, fmt.Errorf("parse coordination incident %s claim expiry: %w", incident.ID, err)
	}
	return !expires.After(current), nil
}

func coordinationIncidentHasLiveClaim(incident model.CoordinationIncident, current time.Time) (bool, error) {
	if incident.Status != model.CoordinationIncidentCoordinating ||
		incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
		return false, nil
	}
	expired, err := coordinationIncidentClaimExpired(incident, current)
	return !expired, err
}

// requireLiveCoordinationClaim binds coordinator-owned proposal writes to the
// exact incident lease that authorized them. Human approval writes happen only
// after the incident leaves coordinating, so they do not need the retired
// coordinator token.
func requireLiveCoordinationClaim(
	incident model.CoordinationIncident,
	claimToken string,
	current time.Time,
) error {
	if incident.Status != model.CoordinationIncidentCoordinating {
		return nil
	}
	if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
		return fmt.Errorf("coordinating incident %s has no claim lease", incident.ID)
	}
	if claimToken != incident.ClaimToken {
		return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
	}
	claimTime, _, err := normalizeCoordinationClaimTime(current)
	if err != nil {
		return err
	}
	expired, err := coordinationIncidentClaimExpired(incident, claimTime)
	if err != nil {
		return err
	}
	if expired {
		return fmt.Errorf("%w: %s", ErrCoordinationClaimExpired, incident.ID)
	}
	return nil
}

func (s *Store) CreateCoordinationIncident(ctx context.Context, input CreateCoordinationIncidentInput) (model.CoordinationIncident, bool, error) {
	board := normalizedBoard(input.Board, s.board)
	if !model.ValidCoordinationTrigger(input.Trigger) {
		return model.CoordinationIncident{}, false, fmt.Errorf("invalid coordination trigger: %s", input.Trigger)
	}
	if input.Severity == "" {
		input.Severity = model.CoordinationSeverityWarning
	}
	if !model.ValidCoordinationSeverity(input.Severity) {
		return model.CoordinationIncident{}, false, fmt.Errorf("invalid coordination severity: %s", input.Severity)
	}
	if input.Status == "" {
		input.Status = model.CoordinationIncidentOpen
	}
	if input.Status != model.CoordinationIncidentOpen {
		return model.CoordinationIncident{}, false, errors.New(
			"new coordination incident must be open; use ClaimCoordinationIncident to enter coordinating status",
		)
	}
	input.Summary = strings.TrimSpace(input.Summary)
	if input.Summary == "" {
		return model.CoordinationIncident{}, false, errors.New("coordination incident summary cannot be empty")
	}
	details, err := normalizeJSON(input.Details, "{}", true, "coordination incident details")
	if err != nil {
		return model.CoordinationIncident{}, false, err
	}
	id, err := validRecordID(input.ID, "coordination incident id")
	if err != nil {
		return model.CoordinationIncident{}, false, err
	}
	if id == "" {
		id = newID("ci")
	}

	var result model.CoordinationIncident
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		if err := requireCoordinationTaskBoard(ctx, tx, board, input.RootTaskID, "root task"); err != nil {
			return err
		}
		if err := requireCoordinationTaskBoard(ctx, tx, board, input.TaskID, "task"); err != nil {
			return err
		}
		state, err := readBoardGraphState(ctx, tx, board)
		if err != nil {
			return err
		}
		if input.ExpectedGraphRevision != nil {
			state, err = requireBoardGraphRevision(ctx, tx, board, *input.ExpectedGraphRevision)
			if err != nil {
				return err
			}
		}

		existing, getErr := scanCoordinationIncident(tx.QueryRowContext(ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
		if getErr == nil {
			if existing.Board != board || existing.Trigger != input.Trigger ||
				!sameString(existing.RootTaskID, input.RootTaskID) || !sameString(existing.TaskID, input.TaskID) {
				return fmt.Errorf("coordination incident id %s is already used for another incident", id)
			}
			if activeIncidentStatus(existing.Status) {
				liveClaim, err := coordinationIncidentHasLiveClaim(existing, time.Now().UTC())
				if err != nil {
					return err
				}
				if liveClaim {
					result = existing
					return nil
				}
				timestamp := now()
				if _, err := tx.ExecContext(ctx, `
					UPDATE coordination_incidents
					SET severity = ?, graph_revision = ?, summary = ?, details_json = ?, updated_at = ?
					WHERE id = ?
				`, input.Severity, state.Revision, input.Summary, string(details), timestamp, existing.ID); err != nil {
					return err
				}
				existing.Severity = input.Severity
				existing.GraphRevision = state.Revision
				existing.Summary = input.Summary
				existing.Details = details
				existing.UpdatedAt = timestamp
			}
			result = existing
			return nil
		}
		if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}

		existing, getErr = scanCoordinationIncident(tx.QueryRowContext(ctx, `
			SELECT `+incidentColumns+` FROM coordination_incidents
			WHERE board = ? AND trigger = ?
				AND IFNULL(root_task_id, '') = IFNULL(?, '')
				AND IFNULL(task_id, '') = IFNULL(?, '')
				AND status IN ('open', 'coordinating', 'awaiting_approval', 'applying')
			LIMIT 1
		`, board, input.Trigger, nullableString(input.RootTaskID), nullableString(input.TaskID)))
		if getErr == nil {
			liveClaim, err := coordinationIncidentHasLiveClaim(existing, time.Now().UTC())
			if err != nil {
				return err
			}
			if liveClaim {
				result = existing
				return nil
			}
			timestamp := now()
			if _, err := tx.ExecContext(ctx, `
				UPDATE coordination_incidents
				SET severity = ?, graph_revision = ?, summary = ?, details_json = ?, updated_at = ?
				WHERE id = ?
			`, input.Severity, state.Revision, input.Summary, string(details), timestamp, existing.ID); err != nil {
				return err
			}
			existing.Severity = input.Severity
			existing.GraphRevision = state.Revision
			existing.Summary = input.Summary
			existing.Details = details
			existing.UpdatedAt = timestamp
			result = existing
			return nil
		}
		if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}

		timestamp := now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coordination_incidents(
				id, board, root_task_id, task_id, trigger, severity, status,
				graph_revision, summary, details_json, claim_token, claim_expires_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)
		`, id, board, nullableString(input.RootTaskID), nullableString(input.TaskID), input.Trigger,
			input.Severity, input.Status, state.Revision, input.Summary, string(details), timestamp, timestamp); err != nil {
			return err
		}
		result = model.CoordinationIncident{
			ID: id, Board: board, RootTaskID: input.RootTaskID, TaskID: input.TaskID,
			Trigger: input.Trigger, Severity: input.Severity, Status: input.Status,
			GraphRevision: state.Revision, Summary: input.Summary, Details: details,
			CreatedAt: timestamp, UpdatedAt: timestamp,
		}
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) GetCoordinationIncident(ctx context.Context, id string) (model.CoordinationIncident, error) {
	value, err := scanCoordinationIncident(s.db.QueryRowContext(ctx,
		"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return model.CoordinationIncident{}, fmt.Errorf("coordination incident not found: %s", id)
	}
	return value, err
}

func (s *Store) ListCoordinationIncidents(ctx context.Context, filter CoordinationIncidentFilter) ([]model.CoordinationIncident, error) {
	clauses := []string{"board = ?"}
	values := []any{normalizedBoard(filter.Board, s.board)}
	if filter.RootTaskID != "" {
		clauses, values = append(clauses, "root_task_id = ?"), append(values, strings.TrimSpace(filter.RootTaskID))
	}
	if filter.TaskID != "" {
		clauses, values = append(clauses, "task_id = ?"), append(values, strings.TrimSpace(filter.TaskID))
	}
	if filter.Trigger != "" {
		if !model.ValidCoordinationTrigger(filter.Trigger) {
			return nil, fmt.Errorf("invalid coordination trigger: %s", filter.Trigger)
		}
		clauses, values = append(clauses, "trigger = ?"), append(values, filter.Trigger)
	}
	if filter.Status != "" {
		if !model.ValidCoordinationIncidentStatus(filter.Status) {
			return nil, fmt.Errorf("invalid coordination incident status: %s", filter.Status)
		}
		clauses, values = append(clauses, "status = ?"), append(values, filter.Status)
	}
	limit := filter.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx, "SELECT "+incidentColumns+" FROM coordination_incidents WHERE "+
		strings.Join(clauses, " AND ")+" ORDER BY updated_at DESC, id DESC LIMIT ?", values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.CoordinationIncident{}
	for rows.Next() {
		value, err := scanCoordinationIncident(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpdateCoordinationIncident(ctx context.Context, id string, input UpdateCoordinationIncidentInput) (model.CoordinationIncident, error) {
	var result model.CoordinationIncident
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", id)
		}
		if err != nil {
			return err
		}
		if input.ExpectedUpdatedAt != nil && *input.ExpectedUpdatedAt != current.UpdatedAt {
			return fmt.Errorf("coordination incident update conflict: %s changed at %s", id, current.UpdatedAt)
		}
		if input.ExpectedGraphRevision != nil {
			if *input.ExpectedGraphRevision != current.GraphRevision {
				return &GraphRevisionConflictError{
					Board: current.Board, Expected: *input.ExpectedGraphRevision, Actual: current.GraphRevision,
				}
			}
			if _, err := requireBoardGraphRevision(ctx, tx, current.Board, *input.ExpectedGraphRevision); err != nil {
				return err
			}
		}
		if !activeIncidentStatus(current.Status) {
			return fmt.Errorf("cannot edit terminal coordination incident %s in status %s", id, current.Status)
		}
		severity, summary, details := current.Severity, current.Summary, current.Details
		if input.Severity != nil {
			if !model.ValidCoordinationSeverity(*input.Severity) {
				return fmt.Errorf("invalid coordination severity: %s", *input.Severity)
			}
			severity = *input.Severity
		}
		if input.Summary != nil {
			summary = strings.TrimSpace(*input.Summary)
			if summary == "" {
				return errors.New("coordination incident summary cannot be empty")
			}
		}
		if input.Details != nil {
			details, err = normalizeJSON(*input.Details, "{}", true, "coordination incident details")
			if err != nil {
				return err
			}
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE coordination_incidents SET severity = ?, summary = ?, details_json = ?, updated_at = ? WHERE id = ?
		`, severity, summary, string(details), timestamp, id); err != nil {
			return err
		}
		current.Severity, current.Summary, current.Details, current.UpdatedAt = severity, summary, details, timestamp
		result = current
		return nil
	})
	return result, err
}

// ClaimCoordinationIncident grants one caller a bounded attempt lease. An open
// incident can be claimed once; a coordinating incident becomes claimable
// again only after its previous lease expires.
func (s *Store) ClaimCoordinationIncident(ctx context.Context, id string, input ClaimCoordinationIncidentInput) (model.CoordinationIncident, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.CoordinationIncident{}, false, errors.New("coordination incident claim requires an incident ID")
	}
	if input.ExpectedGraphRevision == nil {
		return model.CoordinationIncident{}, false, errors.New("coordination incident claim requires an expected graph revision")
	}
	if input.TTL < MinCoordinationIncidentClaimTTL || input.TTL > MaxCoordinationIncidentClaimTTL {
		return model.CoordinationIncident{}, false, fmt.Errorf(
			"coordination incident claim TTL must be between %s and %s",
			MinCoordinationIncidentClaimTTL, MaxCoordinationIncidentClaimTTL,
		)
	}
	current, timestamp, err := normalizeCoordinationClaimTime(input.Current)
	if err != nil {
		return model.CoordinationIncident{}, false, err
	}
	expires := current.Add(input.TTL)
	if expires.Year() < 0 || expires.Year() > 9999 {
		return model.CoordinationIncident{}, false, errors.New("coordination incident claim expiry must fit RFC3339")
	}
	expiresAt := expires.Format(coordinationIncidentClaimTimestampLayout)
	token, err := claimToken()
	if err != nil {
		return model.CoordinationIncident{}, false, fmt.Errorf("generate coordination incident claim token: %w", err)
	}

	var claimedIncident model.CoordinationIncident
	claimed := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		incident, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", id)
		}
		if err != nil {
			return err
		}
		expectedRevision := *input.ExpectedGraphRevision
		state, err := readBoardGraphState(ctx, tx, incident.Board)
		if err != nil {
			return err
		}
		if state.Revision != expectedRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: expectedRevision, Actual: state.Revision,
			}
		}

		var updateResult sql.Result
		updatedAt := now()
		switch incident.Status {
		case model.CoordinationIncidentOpen:
			if incident.GraphRevision != expectedRevision {
				return &GraphRevisionConflictError{
					Board: incident.Board, Expected: expectedRevision, Actual: incident.GraphRevision,
				}
			}
			if incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
				return fmt.Errorf("open coordination incident %s has an invalid claim", id)
			}
			updateResult, err = tx.ExecContext(ctx, `
				UPDATE coordination_incidents
				SET status = 'coordinating', claim_token = ?, claim_expires_at = ?, updated_at = ?
				WHERE id = ? AND status = 'open' AND graph_revision = ?
					AND claim_token IS NULL AND claim_expires_at IS NULL
			`, token, expiresAt, updatedAt, id, expectedRevision)
		case model.CoordinationIncidentCoordinating:
			if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
				return fmt.Errorf("coordinating incident %s has no claim lease", id)
			}
			expired, expiryErr := coordinationIncidentClaimExpired(incident, current)
			if expiryErr != nil {
				return expiryErr
			}
			if !expired {
				claimedIncident = incident
				return nil
			}
			updateResult, err = tx.ExecContext(ctx, `
				UPDATE coordination_incidents
				SET graph_revision = ?, claim_token = ?, claim_expires_at = ?, updated_at = ?
				WHERE id = ? AND status = 'coordinating' AND graph_revision = ?
					AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at <= ?
			`, expectedRevision, token, expiresAt, updatedAt, id, incident.GraphRevision,
				incident.ClaimToken, *incident.ClaimExpiresAt, timestamp)
		default:
			claimedIncident = incident
			return nil
		}
		if err != nil {
			return err
		}
		changed, err := updateResult.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			latest, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
				"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
			if err != nil {
				return err
			}
			if latest.GraphRevision != expectedRevision {
				return &GraphRevisionConflictError{
					Board: latest.Board, Expected: expectedRevision, Actual: latest.GraphRevision,
				}
			}
			claimedIncident = latest
			return nil
		}
		incident.Status = model.CoordinationIncidentCoordinating
		incident.GraphRevision = expectedRevision
		incident.ClaimToken = token
		incident.ClaimExpiresAt = &expiresAt
		incident.UpdatedAt = updatedAt
		claimedIncident = incident
		claimed = true
		return nil
	})
	return claimedIncident, claimed, err
}

func validIncidentTransition(from, to model.CoordinationIncidentStatus) bool {
	switch from {
	case model.CoordinationIncidentOpen:
		return to == model.CoordinationIncidentResolved || to == model.CoordinationIncidentDismissed ||
			to == model.CoordinationIncidentFailed
	case model.CoordinationIncidentCoordinating:
		return to == model.CoordinationIncidentOpen || to == model.CoordinationIncidentAwaitingApproval ||
			to == model.CoordinationIncidentApplying || to == model.CoordinationIncidentResolved ||
			to == model.CoordinationIncidentDismissed || to == model.CoordinationIncidentFailed
	case model.CoordinationIncidentAwaitingApproval:
		return to == model.CoordinationIncidentOpen || to == model.CoordinationIncidentApplying ||
			to == model.CoordinationIncidentResolved || to == model.CoordinationIncidentDismissed ||
			to == model.CoordinationIncidentFailed
	case model.CoordinationIncidentApplying:
		return to == model.CoordinationIncidentResolved || to == model.CoordinationIncidentFailed
	default:
		return false
	}
}

func incidentStatusRequiresCurrentGraph(status model.CoordinationIncidentStatus) bool {
	switch status {
	case model.CoordinationIncidentAwaitingApproval, model.CoordinationIncidentApplying:
		return true
	default:
		return false
	}
}

func (s *Store) TransitionCoordinationIncident(ctx context.Context, id string, input TransitionCoordinationIncidentInput) (model.CoordinationIncident, error) {
	if !model.ValidCoordinationIncidentStatus(input.Status) {
		return model.CoordinationIncident{}, fmt.Errorf("invalid coordination incident status: %s", input.Status)
	}
	if input.Status == model.CoordinationIncidentCoordinating {
		return model.CoordinationIncident{}, errors.New(
			"use ClaimCoordinationIncident to enter coordinating status",
		)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return model.CoordinationIncident{}, errors.New("coordination incident transition requires an incident ID")
	}
	var result model.CoordinationIncident
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", id)
		}
		if err != nil {
			return err
		}
		if input.ExpectedStatus != "" && current.Status != input.ExpectedStatus {
			return &CoordinationStateConflictError{
				Kind: "incident", ID: id, Expected: string(input.ExpectedStatus), Actual: string(current.Status),
			}
		}
		if current.Status == input.Status {
			result = current
			return nil
		}
		if !validIncidentTransition(current.Status, input.Status) {
			return fmt.Errorf("invalid coordination incident transition: %s -> %s", current.Status, input.Status)
		}
		if incidentStatusRequiresCurrentGraph(input.Status) && input.ExpectedGraphRevision == nil {
			return errors.New("coordination incident transition requires an expected graph revision")
		}
		if input.ExpectedGraphRevision != nil {
			if *input.ExpectedGraphRevision != current.GraphRevision {
				return &GraphRevisionConflictError{
					Board: current.Board, Expected: *input.ExpectedGraphRevision, Actual: current.GraphRevision,
				}
			}
			if _, err := requireBoardGraphRevision(ctx, tx, current.Board, *input.ExpectedGraphRevision); err != nil {
				return err
			}
		}

		claimTimestamp := ""
		if current.Status == model.CoordinationIncidentCoordinating {
			if current.ClaimToken == "" || current.ClaimExpiresAt == nil {
				return fmt.Errorf("coordinating incident %s has no claim lease", id)
			}
			if input.ClaimToken != current.ClaimToken {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, id)
			}
			claimTime, timestamp, err := normalizeCoordinationClaimTime(input.Current)
			if err != nil {
				return err
			}
			expired, err := coordinationIncidentClaimExpired(current, claimTime)
			if err != nil {
				return err
			}
			if expired {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimExpired, id)
			}
			claimTimestamp = timestamp
		}

		timestamp := now()
		statement := `UPDATE coordination_incidents
			SET status = ?, claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND status = ? AND graph_revision = ?`
		arguments := []any{input.Status, timestamp, id, current.Status, current.GraphRevision}
		if current.Status == model.CoordinationIncidentCoordinating {
			statement += " AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at > ?"
			arguments = append(arguments, input.ClaimToken, *current.ClaimExpiresAt, claimTimestamp)
		}
		updateResult, err := tx.ExecContext(ctx, statement, arguments...)
		if err != nil {
			return err
		}
		changed, err := updateResult.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			latest, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
				"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", id))
			if err != nil {
				return err
			}
			if current.Status == model.CoordinationIncidentCoordinating &&
				latest.ClaimToken != input.ClaimToken {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, id)
			}
			return &CoordinationStateConflictError{
				Kind: "incident", ID: id, Expected: string(current.Status), Actual: string(latest.Status),
			}
		}
		current.Status = input.Status
		current.ClaimToken = ""
		current.ClaimExpiresAt = nil
		current.UpdatedAt = timestamp
		result = current
		return nil
	})
	return result, err
}

func (s *Store) CreateCoordinationProposal(ctx context.Context, input CreateCoordinationProposalInput) (model.CoordinationProposal, bool, error) {
	input.IncidentID = strings.TrimSpace(input.IncidentID)
	input.CoordinatorAgent = strings.TrimSpace(input.CoordinatorAgent)
	input.CoordinatorModel = strings.TrimSpace(input.CoordinatorModel)
	input.CoordinatorProvider = strings.TrimSpace(input.CoordinatorProvider)
	input.Summary = strings.TrimSpace(input.Summary)
	input.Rationale = strings.TrimSpace(input.Rationale)
	if input.IncidentID == "" || input.CoordinatorAgent == "" || input.Summary == "" || input.Rationale == "" {
		return model.CoordinationProposal{}, false, errors.New("coordination proposal requires incident, coordinator agent, summary, and rationale")
	}
	if input.ExpectedGraphRevision == nil {
		return model.CoordinationProposal{}, false, errors.New("coordination proposal requires an expected graph revision")
	}
	if input.Status == "" {
		input.Status = model.CoordinationProposalDraft
	}
	if input.Status != model.CoordinationProposalDraft && input.Status != model.CoordinationProposalValidating {
		return model.CoordinationProposal{}, false, fmt.Errorf("new coordination proposal must be draft or validating: %s", input.Status)
	}
	actions, err := normalizeJSON(input.Actions, "[]", false, "coordination proposal actions")
	if err != nil {
		return model.CoordinationProposal{}, false, err
	}
	validationErrors, err := normalizeJSON(input.ValidationErrors, "[]", false, "coordination proposal validation errors")
	if err != nil {
		return model.CoordinationProposal{}, false, err
	}
	id, err := validRecordID(input.ID, "coordination proposal id")
	if err != nil {
		return model.CoordinationProposal{}, false, err
	}
	if id == "" {
		id = newID("cp")
	}

	var result model.CoordinationProposal
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		incident, err := scanCoordinationIncident(tx.QueryRowContext(ctx,
			"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", input.IncidentID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("coordination incident not found: %s", input.IncidentID)
		}
		if err != nil {
			return err
		}
		if incident.Status != model.CoordinationIncidentCoordinating {
			return &CoordinationStateConflictError{
				Kind: "incident", ID: incident.ID,
				Expected: string(model.CoordinationIncidentCoordinating),
				Actual:   string(incident.Status),
			}
		}
		if err := requireLiveCoordinationClaim(incident, input.ClaimToken, input.Current); err != nil {
			return err
		}
		if incident.GraphRevision != *input.ExpectedGraphRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: *input.ExpectedGraphRevision, Actual: incident.GraphRevision,
			}
		}
		if _, err := requireBoardGraphRevision(ctx, tx, incident.Board, *input.ExpectedGraphRevision); err != nil {
			return err
		}
		existing, getErr := scanCoordinationProposal(tx.QueryRowContext(ctx,
			"SELECT "+proposalColumns+" FROM coordination_proposals WHERE id = ?", id))
		if getErr == nil {
			if existing.IncidentID != input.IncidentID || existing.ExpectedGraphRevision != *input.ExpectedGraphRevision {
				return fmt.Errorf("coordination proposal id %s is already used for another proposal", id)
			}
			result = existing
			return nil
		}
		if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO coordination_proposals(
				id, incident_id, coordinator_agent, coordinator_model, coordinator_provider,
				status, expected_graph_revision, summary, rationale, actions_json, validation_errors_json,
				created_at, updated_at, applied_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		`, id, input.IncidentID, input.CoordinatorAgent, input.CoordinatorModel, input.CoordinatorProvider,
			input.Status, *input.ExpectedGraphRevision, input.Summary, input.Rationale, string(actions),
			string(validationErrors), timestamp, timestamp); err != nil {
			return err
		}
		result = model.CoordinationProposal{
			ID: id, IncidentID: input.IncidentID, CoordinatorAgent: input.CoordinatorAgent,
			CoordinatorModel: input.CoordinatorModel, CoordinatorProvider: input.CoordinatorProvider,
			Status: input.Status, ExpectedGraphRevision: *input.ExpectedGraphRevision,
			Summary: input.Summary, Rationale: input.Rationale, Actions: actions,
			ValidationErrors: validationErrors, CreatedAt: timestamp, UpdatedAt: timestamp,
		}
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) GetCoordinationProposal(ctx context.Context, id string) (model.CoordinationProposal, error) {
	value, err := scanCoordinationProposal(s.db.QueryRowContext(ctx,
		"SELECT "+proposalColumns+" FROM coordination_proposals WHERE id = ?", strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return model.CoordinationProposal{}, fmt.Errorf("coordination proposal not found: %s", id)
	}
	return value, err
}

func (s *Store) ListCoordinationProposals(ctx context.Context, filter CoordinationProposalFilter) ([]model.CoordinationProposal, error) {
	clauses := []string{"1 = 1"}
	values := []any{}
	if filter.IncidentID != "" {
		clauses, values = append(clauses, "incident_id = ?"), append(values, strings.TrimSpace(filter.IncidentID))
	}
	if filter.Status != "" {
		if !model.ValidCoordinationProposalStatus(filter.Status) {
			return nil, fmt.Errorf("invalid coordination proposal status: %s", filter.Status)
		}
		clauses, values = append(clauses, "status = ?"), append(values, filter.Status)
	}
	limit := filter.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx, "SELECT "+proposalColumns+" FROM coordination_proposals WHERE "+
		strings.Join(clauses, " AND ")+" ORDER BY created_at DESC, id DESC LIMIT ?", values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.CoordinationProposal{}
	for rows.Next() {
		value, err := scanCoordinationProposal(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) UpdateCoordinationProposal(ctx context.Context, id string, input UpdateCoordinationProposalInput) (model.CoordinationProposal, error) {
	if input.ExpectedGraphRevision == nil {
		return model.CoordinationProposal{}, errors.New("coordination proposal update requires an expected graph revision")
	}
	var result model.CoordinationProposal
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		current, incident, err := proposalWithIncident(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := requireLiveCoordinationClaim(incident, input.ClaimToken, input.Current); err != nil {
			return err
		}
		if input.ExpectedStatus != "" && current.Status != input.ExpectedStatus {
			return &CoordinationStateConflictError{
				Kind: "proposal", ID: id, Expected: string(input.ExpectedStatus), Actual: string(current.Status),
			}
		}
		if current.Status != model.CoordinationProposalDraft && current.Status != model.CoordinationProposalValidating {
			return fmt.Errorf("cannot edit coordination proposal %s in status %s", id, current.Status)
		}
		if *input.ExpectedGraphRevision != current.ExpectedGraphRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: *input.ExpectedGraphRevision, Actual: current.ExpectedGraphRevision,
			}
		}
		if _, err := requireBoardGraphRevision(ctx, tx, incident.Board, *input.ExpectedGraphRevision); err != nil {
			return err
		}
		summary, rationale, actions, validationErrors := current.Summary, current.Rationale, current.Actions, current.ValidationErrors
		if input.Summary != nil {
			summary = strings.TrimSpace(*input.Summary)
			if summary == "" {
				return errors.New("coordination proposal summary cannot be empty")
			}
		}
		if input.Rationale != nil {
			rationale = strings.TrimSpace(*input.Rationale)
			if rationale == "" {
				return errors.New("coordination proposal rationale cannot be empty")
			}
		}
		if input.Actions != nil {
			actions, err = normalizeJSON(*input.Actions, "[]", false, "coordination proposal actions")
			if err != nil {
				return err
			}
		}
		if input.ValidationErrors != nil {
			validationErrors, err = normalizeJSON(*input.ValidationErrors, "[]", false, "coordination proposal validation errors")
			if err != nil {
				return err
			}
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET summary = ?, rationale = ?, actions_json = ?, validation_errors_json = ?, updated_at = ?
			WHERE id = ?
		`, summary, rationale, string(actions), string(validationErrors), timestamp, id); err != nil {
			return err
		}
		current.Summary, current.Rationale, current.Actions = summary, rationale, actions
		current.ValidationErrors, current.UpdatedAt = validationErrors, timestamp
		result = current
		return nil
	})
	return result, err
}

func proposalWithIncident(ctx context.Context, q querier, id string) (model.CoordinationProposal, model.CoordinationIncident, error) {
	proposal, err := scanCoordinationProposal(q.QueryRowContext(ctx,
		"SELECT "+proposalColumns+" FROM coordination_proposals WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, fmt.Errorf("coordination proposal not found: %s", id)
	}
	if err != nil {
		return model.CoordinationProposal{}, model.CoordinationIncident{}, err
	}
	incident, err := scanCoordinationIncident(q.QueryRowContext(ctx,
		"SELECT "+incidentColumns+" FROM coordination_incidents WHERE id = ?", proposal.IncidentID))
	return proposal, incident, err
}

func validProposalTransition(from, to model.CoordinationProposalStatus) bool {
	switch from {
	case model.CoordinationProposalDraft:
		return to == model.CoordinationProposalValidating || to == model.CoordinationProposalRejected ||
			to == model.CoordinationProposalSuperseded || to == model.CoordinationProposalFailed
	case model.CoordinationProposalValidating:
		return to == model.CoordinationProposalDraft || to == model.CoordinationProposalValidated ||
			to == model.CoordinationProposalRejected || to == model.CoordinationProposalSuperseded ||
			to == model.CoordinationProposalFailed
	case model.CoordinationProposalValidated:
		return to == model.CoordinationProposalAwaitingApproval || to == model.CoordinationProposalApproved ||
			to == model.CoordinationProposalRejected || to == model.CoordinationProposalSuperseded ||
			to == model.CoordinationProposalFailed
	case model.CoordinationProposalAwaitingApproval:
		return to == model.CoordinationProposalApproved || to == model.CoordinationProposalRejected ||
			to == model.CoordinationProposalSuperseded || to == model.CoordinationProposalFailed
	case model.CoordinationProposalApproved:
		return to == model.CoordinationProposalApplying || to == model.CoordinationProposalRejected ||
			to == model.CoordinationProposalSuperseded || to == model.CoordinationProposalFailed
	case model.CoordinationProposalApplying:
		return to == model.CoordinationProposalApplied || to == model.CoordinationProposalFailed
	default:
		return false
	}
}

func statusRequiresCurrentGraph(status model.CoordinationProposalStatus) bool {
	switch status {
	case model.CoordinationProposalValidating, model.CoordinationProposalValidated,
		model.CoordinationProposalAwaitingApproval, model.CoordinationProposalApproved,
		model.CoordinationProposalApplying:
		return true
	default:
		return false
	}
}

func emptyJSONArray(raw json.RawMessage) bool {
	var values []any
	return json.Unmarshal(raw, &values) == nil && len(values) == 0
}

func validCoordinatorRuntimeProposalTransition(
	from, to model.CoordinationProposalStatus,
) bool {
	switch from {
	case model.CoordinationProposalDraft,
		model.CoordinationProposalValidating,
		model.CoordinationProposalValidated:
		switch to {
		case model.CoordinationProposalDraft,
			model.CoordinationProposalValidating,
			model.CoordinationProposalValidated,
			model.CoordinationProposalFailed:
			return from == to || validProposalTransition(from, to)
		default:
			return false
		}
	case model.CoordinationProposalFailed:
		return to == model.CoordinationProposalFailed
	default:
		return false
	}
}

func (s *Store) TransitionCoordinationProposal(ctx context.Context, id string, input TransitionCoordinationProposalInput) (model.CoordinationProposal, error) {
	if !model.ValidCoordinationProposalStatus(input.Status) {
		return model.CoordinationProposal{}, fmt.Errorf("invalid coordination proposal status: %s", input.Status)
	}
	var result model.CoordinationProposal
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		current, incident, err := proposalWithIncident(ctx, tx, id)
		if err != nil {
			return err
		}
		if incident.Status != model.CoordinationIncidentCoordinating {
			return fmt.Errorf(
				"coordination proposal transition requires incident %s to be coordinating; use an atomic approval, application, or supersede operation from %s",
				incident.ID, incident.Status,
			)
		}
		if err := requireLiveCoordinationClaim(incident, input.ClaimToken, input.Current); err != nil {
			return err
		}
		if input.ExpectedStatus != "" && current.Status != input.ExpectedStatus {
			return &CoordinationStateConflictError{
				Kind: "proposal", ID: id, Expected: string(input.ExpectedStatus), Actual: string(current.Status),
			}
		}
		if !validCoordinatorRuntimeProposalTransition(current.Status, input.Status) {
			return fmt.Errorf(
				"coordination proposal transition %s -> %s requires an atomic approval, application, or supersede operation",
				current.Status, input.Status,
			)
		}
		if current.Status == input.Status {
			result = current
			return nil
		}
		validationErrors := current.ValidationErrors
		if input.ValidationErrors != nil {
			validationErrors, err = normalizeJSON(*input.ValidationErrors, "[]", false, "coordination proposal validation errors")
			if err != nil {
				return err
			}
		}
		if (input.Status == model.CoordinationProposalValidated ||
			input.Status == model.CoordinationProposalAwaitingApproval ||
			input.Status == model.CoordinationProposalApproved ||
			input.Status == model.CoordinationProposalApplying ||
			input.Status == model.CoordinationProposalApplied) && !emptyJSONArray(validationErrors) {
			return errors.New("a validated or applicable coordination proposal cannot have validation errors")
		}
		if statusRequiresCurrentGraph(input.Status) {
			if input.ExpectedGraphRevision == nil {
				return errors.New("coordination proposal transition requires an expected graph revision")
			}
			if *input.ExpectedGraphRevision != current.ExpectedGraphRevision {
				return &GraphRevisionConflictError{
					Board: incident.Board, Expected: *input.ExpectedGraphRevision, Actual: current.ExpectedGraphRevision,
				}
			}
			if _, err := requireBoardGraphRevision(ctx, tx, incident.Board, *input.ExpectedGraphRevision); err != nil {
				return err
			}
		}
		timestamp := now()
		var appliedAt *string
		if input.Status == model.CoordinationProposalApplied {
			appliedAt = &timestamp
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = ?, validation_errors_json = ?, updated_at = ?, applied_at = ?
			WHERE id = ?
		`, input.Status, string(validationErrors), timestamp, nullableString(appliedAt), id); err != nil {
			return err
		}
		current.Status, current.ValidationErrors, current.UpdatedAt, current.AppliedAt =
			input.Status, validationErrors, timestamp, appliedAt
		result = current
		return nil
	})
	return result, err
}
