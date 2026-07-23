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

	maxCoordinationAttemptBoardBytes    = 128
	maxCoordinationAttemptAgentBytes    = 128
	maxCoordinationAttemptModelBytes    = 256
	maxCoordinationAttemptProviderBytes = 128
	maxCoordinationAttemptSourceBytes   = 128

	coordinationAttemptTimestampLayout = "2006-01-02T15:04:05.000000000Z"
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
