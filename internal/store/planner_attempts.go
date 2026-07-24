package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrPlannerAttemptNotFound         = errors.New("planner attempt not found")
	ErrPlannerAttemptConflict         = errors.New("planner attempt idempotency conflict")
	ErrPlannerAttemptIdentity         = errors.New("planner attempt identity conflict")
	ErrPlannerProposalConflict        = errors.New("planner attempt already has a different proposal")
	ErrPlannerProposalPayloadTooLarge = errors.New("planner proposal payload exceeds the durable limit")
	ErrPlannerResponseTooLarge        = errors.New("planner response exceeds the processing limit")
)

const (
	MaxPlannerProposalPayloadBytes = 256 * 1024
	MaxPlannerProposalErrorBytes   = 4 * 1024
	MaxPlannerResponseParseBytes   = 1024 * 1024
	MaxPlannerResponseBytes        = 2 * 1024 * 1024
	MaxPlannerAttemptListLimit     = 100

	maxPlannerIdempotencyKeyBytes = 256
	plannerLedgerTimestampLayout  = "2006-01-02T15:04:05.000000000Z"
)

type PlannerAttemptKind string

const (
	PlannerAttemptSpecify   PlannerAttemptKind = "specify"
	PlannerAttemptDecompose PlannerAttemptKind = "decompose"
)

type PlannerProposalValidationStatus string

const (
	PlannerProposalValid   PlannerProposalValidationStatus = "valid"
	PlannerProposalInvalid PlannerProposalValidationStatus = "invalid"
)

// PlannerAttemptIntent is immutable evidence that the Dispatcher committed an
// exact Triage task, graph, prompt snapshot, and agent configuration before it
// invoked an external Planner.
type PlannerAttemptIntent struct {
	ID             string             `json:"id"`
	Board          string             `json:"board"`
	TaskID         string             `json:"taskId"`
	TaskUpdatedAt  string             `json:"taskUpdatedAt"`
	GraphRevision  int64              `json:"graphRevision"`
	Kind           PlannerAttemptKind `json:"kind"`
	SchemaVersion  int64              `json:"schemaVersion"`
	SnapshotHash   string             `json:"snapshotHash"`
	ConfigHash     string             `json:"configHash"`
	IdempotencyKey string             `json:"idempotencyKey"`
	Attempt        int64              `json:"attempt"`
	StartedAt      string             `json:"startedAt"`
}

// PlannerProposalRecord is an immutable receipt for the exact bytes returned
// by a Planner. Payload is the canonical JSON form when one can be retained
// within the durable bound. A malformed or oversized invalid response keeps
// only its response hash and bounded validation error.
type PlannerProposalRecord struct {
	AttemptID           string                          `json:"attemptId"`
	Board               string                          `json:"board"`
	TaskID              string                          `json:"taskId"`
	TaskUpdatedAt       string                          `json:"taskUpdatedAt"`
	GraphRevision       int64                           `json:"graphRevision"`
	Kind                PlannerAttemptKind              `json:"kind"`
	SchemaVersion       int64                           `json:"schemaVersion"`
	SnapshotHash        string                          `json:"snapshotHash"`
	ConfigHash          string                          `json:"configHash"`
	IdempotencyKey      string                          `json:"idempotencyKey"`
	Attempt             int64                           `json:"attempt"`
	StartedAt           string                          `json:"startedAt"`
	ResponseHash        string                          `json:"responseHash"`
	Payload             json.RawMessage                 `json:"payload,omitempty"`
	PayloadHash         *string                         `json:"payloadHash,omitempty"`
	ValidationStatus    PlannerProposalValidationStatus `json:"validationStatus"`
	ValidationError     *string                         `json:"validationError,omitempty"`
	ValidationErrorHash *string                         `json:"validationErrorHash,omitempty"`
	RecordedAt          string                          `json:"recordedAt"`
}

type PlannerAttemptRecord struct {
	Intent   PlannerAttemptIntent   `json:"intent"`
	Proposal *PlannerProposalRecord `json:"proposal,omitempty"`
}

type BeginPlannerAttemptInput struct {
	TaskID                string
	Board                 string
	ExpectedTaskUpdatedAt string
	ExpectedGraphRevision int64
	Kind                  PlannerAttemptKind
	SchemaVersion         int64
	SnapshotHash          string
	ConfigHash            string
	IdempotencyKey        string
	Attempt               int64
}

type RecordPlannerProposalInput struct {
	Response         []byte
	ValidationStatus PlannerProposalValidationStatus
	ValidationError  string
}

type PlannerAttemptFilter struct {
	Board          string
	AfterStartedAt string
	AfterID        string
	Limit          int
}

type PlannerAttemptConflictError struct {
	Board             string
	IdempotencyKey    string
	ExistingAttemptID string
}

func (e *PlannerAttemptConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(
		"%s: board %s key %s belongs to attempt %s with a different identity",
		ErrPlannerAttemptConflict,
		e.Board,
		e.IdempotencyKey,
		e.ExistingAttemptID,
	)
}

func (e *PlannerAttemptConflictError) Unwrap() error {
	return ErrPlannerAttemptConflict
}

type PlannerAttemptIdentityError struct {
	AttemptID string
}

func (e *PlannerAttemptIdentityError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(
		"%s: attempt %s does not match the durable intent",
		ErrPlannerAttemptIdentity,
		e.AttemptID,
	)
}

func (e *PlannerAttemptIdentityError) Unwrap() error {
	return ErrPlannerAttemptIdentity
}

type PlannerProposalConflictError struct {
	AttemptID string
}

func (e *PlannerProposalConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(
		"%s: attempt %s",
		ErrPlannerProposalConflict,
		e.AttemptID,
	)
}

func (e *PlannerProposalConflictError) Unwrap() error {
	return ErrPlannerProposalConflict
}

const plannerAttemptIntentColumns = `i.id, i.board, i.task_id,
	i.task_updated_at, i.graph_revision, i.kind, i.schema_version,
	i.snapshot_hash, i.config_hash, i.idempotency_key, i.attempt, i.started_at`

const plannerProposalColumns = `p.attempt_id, p.board, p.task_id,
	p.task_updated_at, p.graph_revision, p.kind, p.schema_version,
	p.snapshot_hash, p.config_hash, p.idempotency_key, p.attempt, p.started_at,
	p.response_hash, p.payload_json, p.payload_hash, p.validation_status,
	p.validation_error, p.validation_error_hash, p.recorded_at`

func validPlannerAttemptKind(value PlannerAttemptKind) bool {
	return value == PlannerAttemptSpecify || value == PlannerAttemptDecompose
}

func validPlannerProposalStatus(value PlannerProposalValidationStatus) bool {
	return value == PlannerProposalValid || value == PlannerProposalInvalid
}

func normalizePlannerText(
	value string,
	field string,
	maxBytes int,
	required bool,
) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%s cannot contain NUL", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("%s must be at most %d bytes", field, maxBytes)
	}
	return value, nil
}

func normalizePlannerBoard(value, fallback string) (string, error) {
	board, err := normalizePlannerText(
		normalizedBoard(value, fallback),
		"planner attempt board",
		128,
		true,
	)
	if err != nil {
		return "", err
	}
	if board != normalizedBoard("", fallback) {
		return "", fmt.Errorf(
			"planner attempt board %s does not match store board %s",
			board,
			fallback,
		)
	}
	return board, nil
}

func normalizePlannerTaskTimestamp(value, field string) (string, error) {
	original := value
	value, err := normalizePlannerText(value, field, 128, true)
	if err != nil {
		return "", err
	}
	if original != value {
		return "", fmt.Errorf("%s is not canonical RFC3339Nano", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return "", fmt.Errorf("%s must fit RFC3339", field)
	}
	canonical := parsed.UTC().Format(time.RFC3339Nano)
	if value != canonical {
		return "", fmt.Errorf("%s is not canonical RFC3339Nano", field)
	}
	return value, nil
}

func normalizePlannerLedgerTimestamp(value, field string) (string, error) {
	original := value
	value, err := normalizePlannerText(value, field, 128, true)
	if err != nil {
		return "", err
	}
	if original != value {
		return "", fmt.Errorf(
			"%s is not a fixed-width UTC ledger timestamp",
			field,
		)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return "", fmt.Errorf("%s must fit RFC3339", field)
	}
	canonical := parsed.UTC().Format(plannerLedgerTimestampLayout)
	if value != canonical {
		return "", fmt.Errorf(
			"%s is not a fixed-width UTC ledger timestamp",
			field,
		)
	}
	return value, nil
}

func plannerAttemptNow() string {
	value := now()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(plannerLedgerTimestampLayout)
}

func normalizePlannerHash(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("%s must be a lowercase SHA-256 hash", field)
	}
	if value != strings.ToLower(value) {
		return "", fmt.Errorf("%s must be a lowercase SHA-256 hash", field)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%s must be a lowercase SHA-256 hash", field)
	}
	return value, nil
}

func plannerHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func normalizeBeginPlannerAttempt(
	raw BeginPlannerAttemptInput,
	fallbackBoard string,
) (BeginPlannerAttemptInput, error) {
	var err error
	raw.TaskID, err = validRecordID(raw.TaskID, "planner attempt task ID")
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	raw.TaskID, err = normalizePlannerText(
		raw.TaskID,
		"planner attempt task ID",
		128,
		true,
	)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	raw.Board, err = normalizePlannerBoard(raw.Board, fallbackBoard)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	raw.ExpectedTaskUpdatedAt, err = normalizePlannerTaskTimestamp(
		raw.ExpectedTaskUpdatedAt,
		"planner attempt expected task updatedAt",
	)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	if raw.ExpectedGraphRevision < 0 {
		return BeginPlannerAttemptInput{}, errors.New(
			"planner attempt expected graph revision cannot be negative",
		)
	}
	if !validPlannerAttemptKind(raw.Kind) {
		return BeginPlannerAttemptInput{}, fmt.Errorf(
			"invalid planner attempt kind: %s",
			raw.Kind,
		)
	}
	if raw.SchemaVersion < 1 || raw.SchemaVersion > 2147483647 {
		return BeginPlannerAttemptInput{}, errors.New(
			"planner attempt schema version must be between 1 and 2147483647",
		)
	}
	raw.SnapshotHash, err = normalizePlannerHash(
		raw.SnapshotHash,
		"planner attempt snapshot hash",
	)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	raw.ConfigHash, err = normalizePlannerHash(
		raw.ConfigHash,
		"planner attempt config hash",
	)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	raw.IdempotencyKey, err = normalizePlannerText(
		raw.IdempotencyKey,
		"planner attempt idempotency key",
		maxPlannerIdempotencyKeyBytes,
		true,
	)
	if err != nil {
		return BeginPlannerAttemptInput{}, err
	}
	if raw.Attempt < 1 || raw.Attempt > 2147483647 {
		return BeginPlannerAttemptInput{}, errors.New(
			"planner attempt number must be between 1 and 2147483647",
		)
	}
	return raw, nil
}

func plannerIntentFromInput(
	input BeginPlannerAttemptInput,
	id string,
	startedAt string,
) PlannerAttemptIntent {
	return PlannerAttemptIntent{
		ID:             id,
		Board:          input.Board,
		TaskID:         input.TaskID,
		TaskUpdatedAt:  input.ExpectedTaskUpdatedAt,
		GraphRevision:  input.ExpectedGraphRevision,
		Kind:           input.Kind,
		SchemaVersion:  input.SchemaVersion,
		SnapshotHash:   input.SnapshotHash,
		ConfigHash:     input.ConfigHash,
		IdempotencyKey: input.IdempotencyKey,
		Attempt:        input.Attempt,
		StartedAt:      startedAt,
	}
}

func samePlannerAttemptInput(
	intent PlannerAttemptIntent,
	input BeginPlannerAttemptInput,
) bool {
	return intent.Board == input.Board &&
		intent.TaskID == input.TaskID &&
		intent.TaskUpdatedAt == input.ExpectedTaskUpdatedAt &&
		intent.GraphRevision == input.ExpectedGraphRevision &&
		intent.Kind == input.Kind &&
		intent.SchemaVersion == input.SchemaVersion &&
		intent.SnapshotHash == input.SnapshotHash &&
		intent.ConfigHash == input.ConfigHash &&
		intent.IdempotencyKey == input.IdempotencyKey &&
		intent.Attempt == input.Attempt
}

func samePlannerAttemptIntent(
	left PlannerAttemptIntent,
	right PlannerAttemptIntent,
) bool {
	return left == right
}

func validatePlannerAttemptIntent(value PlannerAttemptIntent) error {
	input, err := normalizeBeginPlannerAttempt(BeginPlannerAttemptInput{
		TaskID:                value.TaskID,
		Board:                 value.Board,
		ExpectedTaskUpdatedAt: value.TaskUpdatedAt,
		ExpectedGraphRevision: value.GraphRevision,
		Kind:                  value.Kind,
		SchemaVersion:         value.SchemaVersion,
		SnapshotHash:          value.SnapshotHash,
		ConfigHash:            value.ConfigHash,
		IdempotencyKey:        value.IdempotencyKey,
		Attempt:               value.Attempt,
	}, value.Board)
	if err != nil {
		return err
	}
	id, err := validRecordID(value.ID, "planner attempt ID")
	if err != nil || id == "" || id != value.ID {
		if err != nil {
			return err
		}
		return errors.New("planner attempt ID is not stored canonically")
	}
	if !samePlannerAttemptInput(value, input) {
		return errors.New("planner attempt identity is not stored canonically")
	}
	startedAt, err := normalizePlannerLedgerTimestamp(
		value.StartedAt,
		"planner attempt startedAt",
	)
	if err != nil {
		return err
	}
	if startedAt != value.StartedAt {
		return errors.New("planner attempt startedAt is not stored canonically")
	}
	return nil
}

func scanPlannerAttemptIntent(row scanner) (PlannerAttemptIntent, error) {
	var value PlannerAttemptIntent
	err := row.Scan(
		&value.ID,
		&value.Board,
		&value.TaskID,
		&value.TaskUpdatedAt,
		&value.GraphRevision,
		&value.Kind,
		&value.SchemaVersion,
		&value.SnapshotHash,
		&value.ConfigHash,
		&value.IdempotencyKey,
		&value.Attempt,
		&value.StartedAt,
	)
	if err != nil {
		return PlannerAttemptIntent{}, err
	}
	if err := validatePlannerAttemptIntent(value); err != nil {
		return PlannerAttemptIntent{}, fmt.Errorf(
			"invalid planner attempt intent: %w",
			err,
		)
	}
	return value, nil
}

func plannerAttemptIntentByID(
	ctx context.Context,
	q querier,
	board string,
	id string,
) (PlannerAttemptIntent, error) {
	value, err := scanPlannerAttemptIntent(q.QueryRowContext(
		ctx,
		`SELECT `+plannerAttemptIntentColumns+`
		 FROM planner_attempt_intents i
		 WHERE i.board = ? AND i.id = ?`,
		board,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PlannerAttemptIntent{}, fmt.Errorf(
			"%w: %s",
			ErrPlannerAttemptNotFound,
			id,
		)
	}
	return value, err
}

func plannerAttemptIntentByIdempotencyKey(
	ctx context.Context,
	q querier,
	board string,
	key string,
) (PlannerAttemptIntent, bool, error) {
	value, err := scanPlannerAttemptIntent(q.QueryRowContext(
		ctx,
		`SELECT `+plannerAttemptIntentColumns+`
		 FROM planner_attempt_intents i
		 WHERE i.board = ? AND i.idempotency_key = ?`,
		board,
		key,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PlannerAttemptIntent{}, false, nil
	}
	return value, err == nil, err
}

func canonicalPlannerPayload(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("planner response is empty")
	}
	if len(raw) > MaxPlannerResponseParseBytes {
		return nil, fmt.Errorf(
			"%w: response exceeds the %d-byte canonical parse limit",
			ErrPlannerProposalPayloadTooLarge,
			MaxPlannerResponseParseBytes,
		)
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("planner response must be valid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("planner response must be valid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New(
				"planner response must contain exactly one JSON value",
			)
		}
		return nil, fmt.Errorf(
			"planner response must contain exactly one JSON value: %w",
			err,
		)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize planner response: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > MaxPlannerProposalPayloadBytes {
		return nil, fmt.Errorf(
			"%w: canonical payload must be between 1 and %d bytes",
			ErrPlannerProposalPayloadTooLarge,
			MaxPlannerProposalPayloadBytes,
		)
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func boundedPlannerValidationError(value string) (*string, error) {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "\uFFFD"))
	if value == "" {
		return nil, nil
	}
	if strings.ContainsRune(value, '\x00') {
		return nil, errors.New(
			"planner proposal validation error cannot contain NUL",
		)
	}
	if len(value) <= MaxPlannerProposalErrorBytes {
		return &value, nil
	}
	value = value[:MaxPlannerProposalErrorBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return &value, nil
}

func normalizePlannerProposal(
	intent PlannerAttemptIntent,
	input RecordPlannerProposalInput,
) (PlannerProposalRecord, error) {
	if err := validatePlannerAttemptIntent(intent); err != nil {
		return PlannerProposalRecord{}, err
	}
	if !validPlannerProposalStatus(input.ValidationStatus) {
		return PlannerProposalRecord{}, fmt.Errorf(
			"invalid planner proposal validation status: %s",
			input.ValidationStatus,
		)
	}
	if len(input.Response) > MaxPlannerResponseBytes {
		return PlannerProposalRecord{}, fmt.Errorf(
			"%w: maximum is %d bytes",
			ErrPlannerResponseTooLarge,
			MaxPlannerResponseBytes,
		)
	}
	validationError, err := boundedPlannerValidationError(
		input.ValidationError,
	)
	if err != nil {
		return PlannerProposalRecord{}, err
	}
	var validationErrorHash *string
	if input.ValidationError != "" {
		hash := plannerHash([]byte(input.ValidationError))
		validationErrorHash = &hash
	}
	value := PlannerProposalRecord{
		AttemptID:           intent.ID,
		Board:               intent.Board,
		TaskID:              intent.TaskID,
		TaskUpdatedAt:       intent.TaskUpdatedAt,
		GraphRevision:       intent.GraphRevision,
		Kind:                intent.Kind,
		SchemaVersion:       intent.SchemaVersion,
		SnapshotHash:        intent.SnapshotHash,
		ConfigHash:          intent.ConfigHash,
		IdempotencyKey:      intent.IdempotencyKey,
		Attempt:             intent.Attempt,
		StartedAt:           intent.StartedAt,
		ResponseHash:        plannerHash(input.Response),
		ValidationStatus:    input.ValidationStatus,
		ValidationError:     validationError,
		ValidationErrorHash: validationErrorHash,
	}
	payload, payloadErr := canonicalPlannerPayload(input.Response)
	if payloadErr == nil {
		value.Payload = payload
		hash := plannerHash(payload)
		value.PayloadHash = &hash
	}
	switch input.ValidationStatus {
	case PlannerProposalValid:
		if value.ValidationError != nil ||
			value.ValidationErrorHash != nil {
			return PlannerProposalRecord{}, errors.New(
				"valid planner proposal cannot include a validation error",
			)
		}
		if payloadErr != nil {
			return PlannerProposalRecord{}, fmt.Errorf(
				"valid planner proposal requires a canonical bounded payload: %w",
				payloadErr,
			)
		}
	case PlannerProposalInvalid:
		if value.ValidationError == nil ||
			value.ValidationErrorHash == nil {
			return PlannerProposalRecord{}, errors.New(
				"invalid planner proposal requires a validation error",
			)
		}
	}
	return value, nil
}

func samePlannerProposal(
	left PlannerProposalRecord,
	right PlannerProposalRecord,
) bool {
	return left.AttemptID == right.AttemptID &&
		left.Board == right.Board &&
		left.TaskID == right.TaskID &&
		left.TaskUpdatedAt == right.TaskUpdatedAt &&
		left.GraphRevision == right.GraphRevision &&
		left.Kind == right.Kind &&
		left.SchemaVersion == right.SchemaVersion &&
		left.SnapshotHash == right.SnapshotHash &&
		left.ConfigHash == right.ConfigHash &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.Attempt == right.Attempt &&
		left.StartedAt == right.StartedAt &&
		left.ResponseHash == right.ResponseHash &&
		bytes.Equal(left.Payload, right.Payload) &&
		sameOptionalString(left.PayloadHash, right.PayloadHash) &&
		left.ValidationStatus == right.ValidationStatus &&
		sameOptionalString(left.ValidationError, right.ValidationError) &&
		sameOptionalString(
			left.ValidationErrorHash,
			right.ValidationErrorHash,
		)
}

func validatePlannerProposal(value PlannerProposalRecord) error {
	intent := PlannerAttemptIntent{
		ID:             value.AttemptID,
		Board:          value.Board,
		TaskID:         value.TaskID,
		TaskUpdatedAt:  value.TaskUpdatedAt,
		GraphRevision:  value.GraphRevision,
		Kind:           value.Kind,
		SchemaVersion:  value.SchemaVersion,
		SnapshotHash:   value.SnapshotHash,
		ConfigHash:     value.ConfigHash,
		IdempotencyKey: value.IdempotencyKey,
		Attempt:        value.Attempt,
		StartedAt:      value.StartedAt,
	}
	if err := validatePlannerAttemptIntent(intent); err != nil {
		return err
	}
	if _, err := normalizePlannerHash(
		value.ResponseHash,
		"planner proposal response hash",
	); err != nil {
		return err
	}
	if !validPlannerProposalStatus(value.ValidationStatus) {
		return errors.New("planner proposal has an invalid validation status")
	}
	if len(value.Payload) == 0 {
		if value.PayloadHash != nil {
			return errors.New(
				"planner proposal payload hash requires a payload",
			)
		}
		if value.ValidationStatus == PlannerProposalValid {
			return errors.New("valid planner proposal is missing its payload")
		}
	} else {
		canonical, err := canonicalPlannerPayload(value.Payload)
		if err != nil {
			return err
		}
		if !bytes.Equal(canonical, value.Payload) {
			return errors.New(
				"planner proposal payload is not stored canonically",
			)
		}
		if value.PayloadHash == nil ||
			*value.PayloadHash != plannerHash(value.Payload) {
			return errors.New("planner proposal payload hash is invalid")
		}
	}
	switch value.ValidationStatus {
	case PlannerProposalValid:
		if value.ValidationError != nil ||
			value.ValidationErrorHash != nil {
			return errors.New(
				"valid planner proposal has a validation error",
			)
		}
	case PlannerProposalInvalid:
		if value.ValidationError == nil ||
			value.ValidationErrorHash == nil {
			return errors.New(
				"invalid planner proposal error is not stored canonically",
			)
		}
		if _, err := normalizePlannerHash(
			*value.ValidationErrorHash,
			"planner proposal validation error hash",
		); err != nil {
			return err
		}
		canonicalError, err := boundedPlannerValidationError(
			*value.ValidationError,
		)
		if err != nil ||
			canonicalError == nil ||
			*canonicalError != *value.ValidationError {
			return errors.New(
				"invalid planner proposal error is not stored canonically",
			)
		}
	}
	recordedAt, err := normalizePlannerLedgerTimestamp(
		value.RecordedAt,
		"planner proposal recordedAt",
	)
	if err != nil {
		return err
	}
	if recordedAt != value.RecordedAt {
		return errors.New(
			"planner proposal recordedAt is not stored canonically",
		)
	}
	return nil
}

func scanPlannerProposal(row scanner) (PlannerProposalRecord, error) {
	var value PlannerProposalRecord
	var payload, payloadHash, validationError, validationErrorHash sql.NullString
	err := row.Scan(
		&value.AttemptID,
		&value.Board,
		&value.TaskID,
		&value.TaskUpdatedAt,
		&value.GraphRevision,
		&value.Kind,
		&value.SchemaVersion,
		&value.SnapshotHash,
		&value.ConfigHash,
		&value.IdempotencyKey,
		&value.Attempt,
		&value.StartedAt,
		&value.ResponseHash,
		&payload,
		&payloadHash,
		&value.ValidationStatus,
		&validationError,
		&validationErrorHash,
		&value.RecordedAt,
	)
	if err != nil {
		return PlannerProposalRecord{}, err
	}
	if payload.Valid {
		value.Payload = json.RawMessage(payload.String)
	}
	value.PayloadHash = stringPointer(payloadHash)
	value.ValidationError = stringPointer(validationError)
	value.ValidationErrorHash = stringPointer(validationErrorHash)
	if err := validatePlannerProposal(value); err != nil {
		return PlannerProposalRecord{}, fmt.Errorf(
			"invalid planner proposal receipt: %w",
			err,
		)
	}
	return value, nil
}

func plannerProposal(
	ctx context.Context,
	q querier,
	attemptID string,
) (PlannerProposalRecord, bool, error) {
	value, err := scanPlannerProposal(q.QueryRowContext(
		ctx,
		`SELECT `+plannerProposalColumns+`
		 FROM planner_attempt_proposals p
		 WHERE p.attempt_id = ?`,
		attemptID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PlannerProposalRecord{}, false, nil
	}
	return value, err == nil, err
}

// BeginPlannerAttempt records the pre-call boundary in the same write
// transaction that verifies the current Triage task and graph revision. An
// exact idempotency replay returns the original intent even when the task has
// since changed or been deleted.
func (s *Store) BeginPlannerAttempt(
	ctx context.Context,
	raw BeginPlannerAttemptInput,
) (PlannerAttemptIntent, bool, error) {
	input, err := normalizeBeginPlannerAttempt(raw, s.board)
	if err != nil {
		return PlannerAttemptIntent{}, false, err
	}
	var result PlannerAttemptIntent
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		existing, found, err := plannerAttemptIntentByIdempotencyKey(
			ctx,
			tx,
			input.Board,
			input.IdempotencyKey,
		)
		if err != nil {
			return err
		}
		if found {
			if !samePlannerAttemptInput(existing, input) {
				return &PlannerAttemptConflictError{
					Board:             input.Board,
					IdempotencyKey:    input.IdempotencyKey,
					ExistingAttemptID: existing.ID,
				}
			}
			result = existing
			return nil
		}

		task, err := requireTask(ctx, tx, input.TaskID)
		if err != nil {
			return err
		}
		if task.Board != input.Board {
			return fmt.Errorf(
				"planner attempt task %s belongs to board %s, not %s",
				task.ID,
				task.Board,
				input.Board,
			)
		}
		if task.Status != model.TaskStatusTriage {
			return fmt.Errorf(
				"planner attempt task %s is %s, expected triage",
				task.ID,
				task.Status,
			)
		}
		if task.UpdatedAt != input.ExpectedTaskUpdatedAt {
			return fmt.Errorf(
				"planner attempt task %s changed at %s, expected %s",
				task.ID,
				task.UpdatedAt,
				input.ExpectedTaskUpdatedAt,
			)
		}
		if _, err := requireBoardGraphRevision(
			ctx,
			tx,
			input.Board,
			input.ExpectedGraphRevision,
		); err != nil {
			return err
		}

		result = plannerIntentFromInput(
			input,
			newID("pla"),
			plannerAttemptNow(),
		)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO planner_attempt_intents(
				id, board, task_id, task_updated_at, graph_revision, kind,
				schema_version, snapshot_hash, config_hash, idempotency_key,
				attempt, started_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			result.ID,
			result.Board,
			result.TaskID,
			result.TaskUpdatedAt,
			result.GraphRevision,
			result.Kind,
			result.SchemaVersion,
			result.SnapshotHash,
			result.ConfigHash,
			result.IdempotencyKey,
			result.Attempt,
			result.StartedAt,
		); err != nil {
			return fmt.Errorf("record planner attempt intent: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return PlannerAttemptIntent{}, false, err
	}
	return result, created, nil
}

// RecordPlannerProposal appends the immutable receipt for one exact intent.
// Repeating the exact bytes and validation result is an idempotent replay;
// any other second result is a typed conflict.
func (s *Store) RecordPlannerProposal(
	ctx context.Context,
	intent PlannerAttemptIntent,
	input RecordPlannerProposalInput,
) (PlannerProposalRecord, bool, error) {
	proposed, err := normalizePlannerProposal(intent, input)
	if err != nil {
		return PlannerProposalRecord{}, false, err
	}
	var result PlannerProposalRecord
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		stored, err := plannerAttemptIntentByID(
			ctx,
			tx,
			s.board,
			intent.ID,
		)
		if err != nil {
			return err
		}
		if !samePlannerAttemptIntent(stored, intent) {
			return &PlannerAttemptIdentityError{AttemptID: intent.ID}
		}
		existing, found, err := plannerProposal(ctx, tx, intent.ID)
		if err != nil {
			return err
		}
		if found {
			if !samePlannerProposal(existing, proposed) {
				return &PlannerProposalConflictError{
					AttemptID: intent.ID,
				}
			}
			result = existing
			return nil
		}
		proposed.RecordedAt = plannerAttemptNow()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO planner_attempt_proposals(
				attempt_id, board, task_id, task_updated_at, graph_revision,
				kind, schema_version, snapshot_hash, config_hash,
				idempotency_key, attempt, started_at, response_hash,
				payload_json, payload_hash, validation_status,
				validation_error, validation_error_hash, recorded_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			proposed.AttemptID,
			proposed.Board,
			proposed.TaskID,
			proposed.TaskUpdatedAt,
			proposed.GraphRevision,
			proposed.Kind,
			proposed.SchemaVersion,
			proposed.SnapshotHash,
			proposed.ConfigHash,
			proposed.IdempotencyKey,
			proposed.Attempt,
			proposed.StartedAt,
			proposed.ResponseHash,
			nullablePlannerPayload(proposed.Payload),
			nullableString(proposed.PayloadHash),
			proposed.ValidationStatus,
			nullableString(proposed.ValidationError),
			nullableString(proposed.ValidationErrorHash),
			proposed.RecordedAt,
		); err != nil {
			return fmt.Errorf("record planner proposal: %w", err)
		}
		result = proposed
		created = true
		return nil
	})
	if err != nil {
		return PlannerProposalRecord{}, false, err
	}
	return result, created, nil
}

func nullablePlannerPayload(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func plannerAttemptRecord(
	ctx context.Context,
	q querier,
	intent PlannerAttemptIntent,
) (PlannerAttemptRecord, error) {
	proposal, found, err := plannerProposal(ctx, q, intent.ID)
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	if found {
		proposalIntent := PlannerAttemptIntent{
			ID:             proposal.AttemptID,
			Board:          proposal.Board,
			TaskID:         proposal.TaskID,
			TaskUpdatedAt:  proposal.TaskUpdatedAt,
			GraphRevision:  proposal.GraphRevision,
			Kind:           proposal.Kind,
			SchemaVersion:  proposal.SchemaVersion,
			SnapshotHash:   proposal.SnapshotHash,
			ConfigHash:     proposal.ConfigHash,
			IdempotencyKey: proposal.IdempotencyKey,
			Attempt:        proposal.Attempt,
			StartedAt:      proposal.StartedAt,
		}
		if !samePlannerAttemptIntent(intent, proposalIntent) {
			return PlannerAttemptRecord{}, errors.New(
				"planner proposal receipt identity does not match its intent",
			)
		}
		return PlannerAttemptRecord{
			Intent:   intent,
			Proposal: &proposal,
		}, nil
	}
	return PlannerAttemptRecord{Intent: intent}, nil
}

func (s *Store) GetPlannerAttempt(
	ctx context.Context,
	id string,
) (PlannerAttemptRecord, error) {
	id, err := validRecordID(id, "planner attempt ID")
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	if id == "" {
		return PlannerAttemptRecord{}, errors.New(
			"planner attempt ID is required",
		)
	}
	intent, err := plannerAttemptIntentByID(ctx, s.db, s.board, id)
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	return plannerAttemptRecord(ctx, s.db, intent)
}

// GetPlannerAttemptByIdempotencyKey is the restart/replay lookup. It returns
// the immutable intent and any already-recorded proposal without consulting
// mutable task state.
func (s *Store) GetPlannerAttemptByIdempotencyKey(
	ctx context.Context,
	board string,
	key string,
) (PlannerAttemptRecord, error) {
	board, err := normalizePlannerBoard(board, s.board)
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	key, err = normalizePlannerText(
		key,
		"planner attempt idempotency key",
		maxPlannerIdempotencyKeyBytes,
		true,
	)
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	intent, found, err := plannerAttemptIntentByIdempotencyKey(
		ctx,
		s.db,
		board,
		key,
	)
	if err != nil {
		return PlannerAttemptRecord{}, err
	}
	if !found {
		return PlannerAttemptRecord{}, fmt.Errorf(
			"%w: idempotency key %s",
			ErrPlannerAttemptNotFound,
			key,
		)
	}
	return plannerAttemptRecord(ctx, s.db, intent)
}

func normalizePlannerAttemptFilter(
	raw PlannerAttemptFilter,
	fallbackBoard string,
) (PlannerAttemptFilter, error) {
	var err error
	raw.Board, err = normalizePlannerBoard(raw.Board, fallbackBoard)
	if err != nil {
		return PlannerAttemptFilter{}, err
	}
	if raw.Limit == 0 {
		raw.Limit = MaxPlannerAttemptListLimit
	}
	if raw.Limit < 1 || raw.Limit > MaxPlannerAttemptListLimit {
		return PlannerAttemptFilter{}, fmt.Errorf(
			"planner attempt list limit must be between 1 and %d",
			MaxPlannerAttemptListLimit,
		)
	}
	if (strings.TrimSpace(raw.AfterStartedAt) == "") !=
		(strings.TrimSpace(raw.AfterID) == "") {
		return PlannerAttemptFilter{}, errors.New(
			"planner attempt cursor requires both startedAt and ID",
		)
	}
	if strings.TrimSpace(raw.AfterStartedAt) != "" {
		raw.AfterStartedAt, err = normalizePlannerLedgerTimestamp(
			raw.AfterStartedAt,
			"planner attempt cursor startedAt",
		)
		if err != nil {
			return PlannerAttemptFilter{}, err
		}
		raw.AfterID, err = validRecordID(
			raw.AfterID,
			"planner attempt cursor ID",
		)
		if err != nil || raw.AfterID == "" {
			if err != nil {
				return PlannerAttemptFilter{}, err
			}
			return PlannerAttemptFilter{}, errors.New(
				"planner attempt cursor ID is required",
			)
		}
	}
	return raw, nil
}

// ListUnresolvedPlannerAttempts returns intents without a proposal receipt in
// stable oldest-first keyset order.
func (s *Store) ListUnresolvedPlannerAttempts(
	ctx context.Context,
	raw PlannerAttemptFilter,
) ([]PlannerAttemptIntent, error) {
	filter, err := normalizePlannerAttemptFilter(raw, s.board)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+plannerAttemptIntentColumns+`
		FROM planner_attempt_intents i
		LEFT JOIN planner_attempt_proposals p ON p.attempt_id = i.id
		WHERE i.board = ?
			AND p.attempt_id IS NULL
			AND (
				? = ''
				OR i.started_at > ?
				OR (i.started_at = ? AND i.id > ?)
			)
		ORDER BY i.started_at, i.id
		LIMIT ?
	`,
		filter.Board,
		filter.AfterStartedAt,
		filter.AfterStartedAt,
		filter.AfterStartedAt,
		filter.AfterID,
		filter.Limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]PlannerAttemptIntent, 0, filter.Limit)
	for rows.Next() {
		intent, err := scanPlannerAttemptIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
