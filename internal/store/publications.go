package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrPublicationNotFound       = errors.New("publication not found")
	ErrPublicationStateConflict  = errors.New("publication state conflict")
	ErrPublicationUpdateConflict = errors.New("publication update conflict")
	ErrPublicationClaimNotOwner  = errors.New("publication claim is owned by another caller")
	ErrPublicationClaimExpired   = errors.New("publication claim has expired")
)

const (
	MinPublicationClaimTTL = 5 * time.Second
	MaxPublicationClaimTTL = 15 * time.Minute

	MaxPublicationErrorBytes          = 4 * 1024
	MaxPublicationPolicySnapshotBytes = 64 * 1024

	maxPublicationBoardBytes   = 128
	maxPublicationBranchBytes  = 512
	maxPublicationRemoteBytes  = 256
	maxPublicationURLBytes     = 8 * 1024
	publicationTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type PublicationStateConflictError struct {
	ID       string
	Expected string
	Actual   model.PublicationStatus
}

func (e *PublicationStateConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: publication %s is %s, expected %s",
		ErrPublicationStateConflict, e.ID, e.Actual, e.Expected)
}

func (e *PublicationStateConflictError) Unwrap() error { return ErrPublicationStateConflict }

type PublicationUpdateConflictError struct {
	ID       string
	Expected string
	Actual   string
}

func (e *PublicationUpdateConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: publication %s changed at %s, expected %s",
		ErrPublicationUpdateConflict, e.ID, e.Actual, e.Expected)
}

func (e *PublicationUpdateConflictError) Unwrap() error { return ErrPublicationUpdateConflict }

type EnsurePublicationInput struct {
	ID              string
	Board           string
	ChangeSetID     string
	Mode            model.PublicationMode
	TargetBranch    string
	Remote          string
	RequireApproval bool
	PolicySnapshot  json.RawMessage
}

type PublicationFilter struct {
	Board       string
	TaskID      string
	RunID       string
	ChangeSetID string
	Status      model.PublicationStatus
	Limit       int
}

type ApprovePublicationInput struct {
	ExpectedUpdatedAt string
}

type RetryPublicationInput struct {
	ExpectedUpdatedAt string
}

type CompleteManualPublicationInput struct {
	ExpectedUpdatedAt string
	URL               *string
}

type SupersedePublicationInput struct {
	ExpectedUpdatedAt string
	Reason            string
}

type ClaimPublicationInput struct {
	ExpectedUpdatedAt string
	TTL               time.Duration
}

type CompletePublicationInput struct {
	ExpectedUpdatedAt string
	ClaimToken        string
	ClaimEpoch        int64
	URL               *string
}

type FailPublicationInput struct {
	ExpectedUpdatedAt string
	ClaimToken        string
	ClaimEpoch        int64
	Error             string
}

const publicationColumns = `id, board, task_id, run_id, change_set_id, status, mode,
	target_branch, remote, require_approval, repository_path, worktree_path,
	base_commit, head_commit, durable_ref, policy_snapshot_json, source_snapshot_json,
	url, error, claim_epoch, claim_token, claim_expires_at, approved_at, published_at,
	created_at, updated_at`

func scanPublication(row scanner) (model.Publication, error) {
	var value model.Publication
	var requireApproval int
	var policySnapshot, sourceSnapshot []byte
	var rawURL, publicationError, claimToken sql.NullString
	var claimExpiresAt, approvedAt, publishedAt sql.NullString
	err := row.Scan(
		&value.ID, &value.Board, &value.TaskID, &value.RunID, &value.ChangeSetID,
		&value.Status, &value.Mode, &value.TargetBranch, &value.Remote, &requireApproval,
		&value.RepositoryPath, &value.WorktreePath, &value.BaseCommit, &value.HeadCommit,
		&value.DurableRef, &policySnapshot, &sourceSnapshot, &rawURL, &publicationError,
		&value.ClaimEpoch, &claimToken, &claimExpiresAt, &approvedAt, &publishedAt,
		&value.CreatedAt, &value.UpdatedAt,
	)
	value.RequireApproval = requireApproval == 1
	value.PolicySnapshot = append(json.RawMessage(nil), policySnapshot...)
	value.SourceSnapshot = append(json.RawMessage(nil), sourceSnapshot...)
	value.URL = stringPointer(rawURL)
	value.Error = stringPointer(publicationError)
	if claimToken.Valid {
		value.ClaimToken = claimToken.String
	}
	value.ClaimExpiresAt = stringPointer(claimExpiresAt)
	value.ApprovedAt = stringPointer(approvedAt)
	value.PublishedAt = stringPointer(publishedAt)
	return value, err
}

func normalizedPublicationText(value, field string, maxBytes int, required bool) (string, error) {
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

func boundedPublicationError(value string) (string, error) {
	value, err := normalizedPublicationText(value, "publication error", MaxPublicationErrorBytes, true)
	if err != nil {
		return "", err
	}
	return value, nil
}

func normalizePublicationBoard(value, fallback string) (string, error) {
	board, err := normalizedPublicationText(
		normalizedBoard(value, fallback),
		"publication board",
		maxPublicationBoardBytes,
		true,
	)
	if err != nil {
		return "", err
	}
	if board != normalizedBoard("", fallback) {
		return "", fmt.Errorf("publication board %s does not match store board %s", board, fallback)
	}
	return board, nil
}

func normalizePublicationPolicy(input EnsurePublicationInput) (EnsurePublicationInput, error) {
	var err error
	input.ID, err = validRecordID(input.ID, "publication id")
	if err != nil {
		return EnsurePublicationInput{}, err
	}
	input.ChangeSetID, err = validRecordID(input.ChangeSetID, "publication change set id")
	if err != nil {
		return EnsurePublicationInput{}, err
	}
	if input.ChangeSetID == "" {
		return EnsurePublicationInput{}, errors.New("publication requires a change set ID")
	}
	if !model.ValidPublicationMode(input.Mode) {
		return EnsurePublicationInput{}, fmt.Errorf("invalid publication mode: %s", input.Mode)
	}
	input.TargetBranch, err = normalizedPublicationText(
		input.TargetBranch, "publication target branch", maxPublicationBranchBytes, true,
	)
	if err != nil {
		return EnsurePublicationInput{}, err
	}
	input.Remote, err = normalizedPublicationText(
		input.Remote, "publication remote", maxPublicationRemoteBytes, true,
	)
	if err != nil {
		return EnsurePublicationInput{}, err
	}
	policy := bytes.TrimSpace(input.PolicySnapshot)
	if len(policy) == 0 {
		policy, err = json.Marshal(map[string]any{
			"publication": map[string]any{
				"mode": input.Mode, "targetBranch": input.TargetBranch,
				"remote": input.Remote, "requireApproval": input.RequireApproval,
			},
		})
		if err != nil {
			return EnsurePublicationInput{}, err
		}
	}
	if len(policy) > MaxPublicationPolicySnapshotBytes {
		return EnsurePublicationInput{}, fmt.Errorf(
			"publication policy snapshot must be at most %d bytes",
			MaxPublicationPolicySnapshotBytes,
		)
	}
	normalized, err := normalizeJSON(policy, "{}", true, "publication policy snapshot")
	if err != nil {
		return EnsurePublicationInput{}, err
	}
	input.PolicySnapshot = normalized
	return input, nil
}

func (s *Store) publicationCurrent() (time.Time, string, error) {
	clock := s.publicationClock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	current := clock().UTC()
	if current.Year() < 0 || current.Year() > 9999 {
		return time.Time{}, "", errors.New("publication claim time must fit RFC3339")
	}
	return current, current.Format(publicationTimestampLayout), nil
}

func publicationClaimExpired(value model.Publication, current time.Time) (bool, error) {
	if value.ClaimExpiresAt == nil {
		return false, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, *value.ClaimExpiresAt)
	if err != nil {
		return false, fmt.Errorf("parse publication %s claim expiry: %w", value.ID, err)
	}
	return !expires.After(current), nil
}

func requirePublicationVersion(value model.Publication, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return errors.New("publication mutation requires expected updatedAt")
	}
	if value.UpdatedAt != expected {
		return &PublicationUpdateConflictError{
			ID: value.ID, Expected: expected, Actual: value.UpdatedAt,
		}
	}
	return nil
}

func requirePublicationState(value model.Publication, expected ...model.PublicationStatus) error {
	for _, candidate := range expected {
		if value.Status == candidate {
			return nil
		}
	}
	values := make([]string, 0, len(expected))
	for _, candidate := range expected {
		values = append(values, string(candidate))
	}
	return &PublicationStateConflictError{
		ID: value.ID, Expected: strings.Join(values, " or "), Actual: value.Status,
	}
}

func publicationForBoard(
	ctx context.Context,
	q querier,
	id string,
	board string,
) (model.Publication, error) {
	value, err := scanPublication(q.QueryRowContext(ctx,
		"SELECT "+publicationColumns+" FROM publications WHERE id = ? AND board = ?",
		strings.TrimSpace(id), board))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Publication{}, fmt.Errorf("%w: %s", ErrPublicationNotFound, strings.TrimSpace(id))
	}
	return value, err
}

func publicationByChangeSetForBoard(
	ctx context.Context,
	q querier,
	changeSetID string,
	board string,
) (model.Publication, error) {
	value, err := scanPublication(q.QueryRowContext(ctx,
		"SELECT "+publicationColumns+" FROM publications WHERE change_set_id = ? AND board = ?",
		strings.TrimSpace(changeSetID), board))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Publication{}, fmt.Errorf("%w for change set: %s", ErrPublicationNotFound,
			strings.TrimSpace(changeSetID))
	}
	return value, err
}

func publicPublication(value model.Publication) model.Publication {
	value.ClaimToken = ""
	return value
}

func publicationSource(
	ctx context.Context,
	q querier,
	changeSetID string,
	board string,
) (model.ChangeSet, json.RawMessage, error) {
	changeSet, err := scanChangeSet(q.QueryRowContext(ctx,
		"SELECT "+changeSetColumns+" FROM task_change_sets WHERE id = ?",
		changeSetID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.ChangeSet{}, nil, fmt.Errorf("change set not found: %s", changeSetID)
	}
	if err != nil {
		return model.ChangeSet{}, nil, err
	}
	task, err := requireTask(ctx, q, changeSet.TaskID)
	if err != nil {
		return model.ChangeSet{}, nil, err
	}
	if task.Board != board {
		return model.ChangeSet{}, nil, fmt.Errorf(
			"change set %s belongs to board %s, not %s",
			changeSetID, task.Board, board,
		)
	}
	if task.WorkflowRole != model.WorkflowRoleFinalizer {
		return model.ChangeSet{}, nil, fmt.Errorf(
			"change set %s task %s is not a finalizer",
			changeSetID, task.ID,
		)
	}
	if task.Status != model.TaskStatusDone {
		return model.ChangeSet{}, nil, fmt.Errorf(
			"change set %s finalizer task %s is %s, expected done",
			changeSetID, task.ID, task.Status,
		)
	}
	run, err := getRun(ctx, q, changeSet.RunID)
	if err != nil {
		return model.ChangeSet{}, nil, err
	}
	if run.TaskID != task.ID || run.Status != model.RunStatusCompleted {
		return model.ChangeSet{}, nil, fmt.Errorf(
			"change set %s does not belong to a completed finalizer run",
			changeSetID,
		)
	}
	source, err := json.Marshal(struct {
		Board        string             `json:"board"`
		WorkflowRole model.WorkflowRole `json:"workflowRole"`
		TaskStatus   model.TaskStatus   `json:"taskStatus"`
		RunStatus    model.RunStatus    `json:"runStatus"`
		ChangeSet    model.ChangeSet    `json:"changeSet"`
	}{
		Board: board, WorkflowRole: task.WorkflowRole, TaskStatus: task.Status,
		RunStatus: run.Status, ChangeSet: changeSet,
	})
	if err != nil {
		return model.ChangeSet{}, nil, err
	}
	return changeSet, source, nil
}

// EnsurePublication creates at most one immutable publication handoff for a
// finalizer's completed change set. A repeated call returns the first policy
// and source snapshots even if board settings have changed since creation.
func (s *Store) EnsurePublication(
	ctx context.Context,
	raw EnsurePublicationInput,
) (model.Publication, bool, error) {
	input, err := normalizePublicationPolicy(raw)
	if err != nil {
		return model.Publication{}, false, err
	}
	board, err := normalizePublicationBoard(input.Board, s.board)
	if err != nil {
		return model.Publication{}, false, err
	}
	if input.ID == "" {
		input.ID = newID("pub")
	}

	var value model.Publication
	created := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		existing, getErr := publicationByChangeSetForBoard(
			ctx, tx, input.ChangeSetID, board,
		)
		if getErr == nil {
			value = publicPublication(existing)
			return nil
		}
		if !errors.Is(getErr, ErrPublicationNotFound) {
			return getErr
		}

		var existingID string
		idErr := tx.QueryRowContext(ctx,
			"SELECT id FROM publications WHERE id = ?", input.ID).Scan(&existingID)
		if idErr == nil {
			return fmt.Errorf("publication id %s is already used", input.ID)
		}
		if !errors.Is(idErr, sql.ErrNoRows) {
			return idErr
		}
		changeSet, sourceSnapshot, err := publicationSource(
			ctx, tx, input.ChangeSetID, board,
		)
		if err != nil {
			return err
		}
		status := model.PublicationPending
		var publishedAt *string
		timestamp := now()
		if changeSet.State == "no_change" {
			status = model.PublicationNoChange
			publishedAt = &timestamp
		} else if input.RequireApproval {
			status = model.PublicationAwaitingApproval
		}
		requireApproval := 0
		if input.RequireApproval {
			requireApproval = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO publications(
				id, board, task_id, run_id, change_set_id, status, mode,
				target_branch, remote, require_approval, repository_path, worktree_path,
				base_commit, head_commit, durable_ref, policy_snapshot_json,
				source_snapshot_json, url, error, claim_token, claim_expires_at,
				approved_at, published_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
				NULL, NULL, NULL, NULL, NULL, ?, ?, ?)
		`, input.ID, board, changeSet.TaskID, changeSet.RunID, changeSet.ID, status,
			input.Mode, input.TargetBranch, input.Remote, requireApproval,
			changeSet.RepositoryPath, changeSet.WorktreePath, changeSet.BaseCommit,
			changeSet.HeadCommit, changeSet.DurableRef, string(input.PolicySnapshot),
			string(sourceSnapshot), nullableString(publishedAt), timestamp, timestamp); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, changeSet.TaskID, "publication_created", map[string]any{
			"publicationId": input.ID, "changeSetId": changeSet.ID,
			"status": status, "mode": input.Mode,
		}, &changeSet.RunID); err != nil {
			return err
		}
		value = model.Publication{
			ID: input.ID, Board: board, TaskID: changeSet.TaskID,
			RunID: changeSet.RunID, ChangeSetID: changeSet.ID, Status: status,
			Mode: input.Mode, TargetBranch: input.TargetBranch, Remote: input.Remote,
			RequireApproval: input.RequireApproval,
			RepositoryPath:  changeSet.RepositoryPath, WorktreePath: changeSet.WorktreePath,
			BaseCommit: changeSet.BaseCommit, HeadCommit: changeSet.HeadCommit,
			DurableRef:     changeSet.DurableRef,
			PolicySnapshot: append(json.RawMessage(nil), input.PolicySnapshot...),
			SourceSnapshot: append(json.RawMessage(nil), sourceSnapshot...),
			PublishedAt:    publishedAt, CreatedAt: timestamp, UpdatedAt: timestamp,
		}
		created = true
		return nil
	})
	return value, created, err
}

func (s *Store) GetPublication(ctx context.Context, id string) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	value, err := publicationForBoard(ctx, s.db, id, board)
	return publicPublication(value), err
}

func (s *Store) GetPublicationByChangeSet(
	ctx context.Context,
	changeSetID string,
) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	value, err := publicationByChangeSetForBoard(ctx, s.db, changeSetID, board)
	return publicPublication(value), err
}

func (s *Store) ListPublications(
	ctx context.Context,
	filter PublicationFilter,
) ([]model.Publication, error) {
	board, err := normalizePublicationBoard(filter.Board, s.board)
	if err != nil {
		return nil, err
	}
	clauses := []string{"board = ?"}
	values := []any{board}
	if filter.TaskID != "" {
		clauses = append(clauses, "task_id = ?")
		values = append(values, strings.TrimSpace(filter.TaskID))
	}
	if filter.RunID != "" {
		clauses = append(clauses, "run_id = ?")
		values = append(values, strings.TrimSpace(filter.RunID))
	}
	if filter.ChangeSetID != "" {
		clauses = append(clauses, "change_set_id = ?")
		values = append(values, strings.TrimSpace(filter.ChangeSetID))
	}
	if filter.Status != "" {
		if !model.ValidPublicationStatus(filter.Status) {
			return nil, fmt.Errorf("invalid publication status: %s", filter.Status)
		}
		clauses = append(clauses, "status = ?")
		values = append(values, filter.Status)
	}
	limit := filter.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+publicationColumns+" FROM publications WHERE "+
			strings.Join(clauses, " AND ")+" ORDER BY created_at DESC, id DESC LIMIT ?",
		values...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Publication, 0)
	for rows.Next() {
		value, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, publicPublication(value))
	}
	return result, rows.Err()
}

func (s *Store) ApprovePublication(
	ctx context.Context,
	id string,
	input ApprovePublicationInput,
) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requirePublicationState(current, model.PublicationAwaitingApproval); err != nil {
			return err
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'pending', approved_at = ?, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'awaiting_approval' AND updated_at = ?
		`, timestamp, timestamp, current.ID, board, current.UpdatedAt)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_approved",
			map[string]any{"publicationId": current.ID}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationPending
		current.ApprovedAt = &timestamp
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}

// RetryPublication explicitly returns one failed publication to the pending
// lane. The Publisher never retries Failed records on its own, so an operator
// must acknowledge the previous error through this CAS-protected transition.
func (s *Store) RetryPublication(
	ctx context.Context,
	id string,
	input RetryPublicationInput,
) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requirePublicationState(current, model.PublicationFailed); err != nil {
			return err
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'pending', error = NULL, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'failed' AND updated_at = ?
				AND claim_token IS NULL AND claim_expires_at IS NULL
		`, timestamp, current.ID, board, current.UpdatedAt)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_retry_requested",
			map[string]any{"publicationId": current.ID}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationPending
		current.Error = nil
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}

// CompleteManualPublication records a human-controlled publication without
// granting a host-side Git lease. Automated modes must complete through
// ClaimPublication and CompletePublication instead.
func (s *Store) CompleteManualPublication(
	ctx context.Context,
	id string,
	input CompleteManualPublicationInput,
) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	rawURL, err := normalizePublicationURL(input.URL)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if current.Mode != model.PublicationModeManual {
			return fmt.Errorf(
				"manual completion requires manual publication mode, got %s",
				current.Mode,
			)
		}
		if err := requirePublicationState(current, model.PublicationPending); err != nil {
			return err
		}
		if current.RequireApproval && current.ApprovedAt == nil {
			return errors.New("manual publication requires approval before completion")
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'published', url = ?, error = NULL, published_at = ?,
				updated_at = ?
			WHERE id = ? AND board = ? AND mode = 'manual' AND status = 'pending'
				AND updated_at = ? AND claim_token IS NULL AND claim_expires_at IS NULL
		`, nullableString(rawURL), timestamp, timestamp, current.ID, board,
			current.UpdatedAt)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_completed", map[string]any{
			"publicationId": current.ID, "mode": current.Mode, "url": rawURL,
		}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationPublished
		current.URL = rawURL
		current.Error = nil
		current.PublishedAt = &timestamp
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}

func (s *Store) SupersedePublication(
	ctx context.Context,
	id string,
	input SupersedePublicationInput,
) (model.Publication, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	reason, err := boundedPublicationError(input.Reason)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requirePublicationState(
			current,
			model.PublicationAwaitingApproval,
			model.PublicationPending,
			model.PublicationFailed,
		); err != nil {
			return err
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'superseded', error = ?, updated_at = ?
			WHERE id = ? AND board = ? AND status = ? AND updated_at = ?
		`, reason, timestamp, current.ID, board, current.Status, current.UpdatedAt)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_superseded", map[string]any{
			"publicationId": current.ID, "reason": reason,
		}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationSuperseded
		current.Error = &reason
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}

// ClaimPublication grants a bounded lease for one host-side publication
// attempt. Every successful claim advances ClaimEpoch. Publishing records are
// never taken over automatically, even after their lease expires.
func (s *Store) ClaimPublication(
	ctx context.Context,
	id string,
	input ClaimPublicationInput,
) (model.Publication, bool, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, false, err
	}
	if input.TTL < MinPublicationClaimTTL || input.TTL > MaxPublicationClaimTTL {
		return model.Publication{}, false, fmt.Errorf(
			"publication claim TTL must be between %s and %s",
			MinPublicationClaimTTL, MaxPublicationClaimTTL,
		)
	}
	var value model.Publication
	claimed := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if current.Status == model.PublicationPublishing {
			value = publicPublication(current)
			return nil
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if err := requirePublicationState(current, model.PublicationPending); err != nil {
			return err
		}
		if current.ClaimEpoch == math.MaxInt64 {
			return errors.New("publication claim epoch is exhausted")
		}
		currentTime, _, err := s.publicationCurrent()
		if err != nil {
			return err
		}
		expires := currentTime.Add(input.TTL)
		if expires.Year() < 0 || expires.Year() > 9999 {
			return errors.New("publication claim expiry must fit RFC3339")
		}
		expiresAt := expires.Format(publicationTimestampLayout)
		token, err := claimToken()
		if err != nil {
			return fmt.Errorf("generate publication claim token: %w", err)
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'publishing', error = NULL, claim_epoch = claim_epoch + 1,
				claim_token = ?, claim_expires_at = ?, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'pending' AND updated_at = ?
				AND claim_epoch = ? AND claim_epoch < ?
				AND claim_token IS NULL AND claim_expires_at IS NULL
		`, token, expiresAt, timestamp, current.ID, board, current.UpdatedAt,
			current.ClaimEpoch, int64(math.MaxInt64))
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		current.ClaimEpoch++
		if err := appendEvent(ctx, tx, current.TaskID, "publication_claimed", map[string]any{
			"publicationId": current.ID, "claimEpoch": current.ClaimEpoch,
			"claimExpiresAt": expiresAt,
		}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationPublishing
		current.Error = nil
		current.ClaimToken = token
		current.ClaimExpiresAt = &expiresAt
		current.UpdatedAt = timestamp
		value = current
		claimed = true
		return nil
	})
	if !claimed {
		value = publicPublication(value)
	}
	return value, claimed, err
}

func requireLivePublicationClaim(
	value model.Publication,
	token string,
	epoch int64,
	current time.Time,
) error {
	token = strings.TrimSpace(token)
	if value.Status != model.PublicationPublishing {
		return requirePublicationState(value, model.PublicationPublishing)
	}
	if value.ClaimToken == "" || value.ClaimExpiresAt == nil {
		return fmt.Errorf("publishing publication %s has no claim lease", value.ID)
	}
	if token == "" || token != value.ClaimToken || epoch != value.ClaimEpoch {
		return fmt.Errorf("%w: %s", ErrPublicationClaimNotOwner, value.ID)
	}
	expired, err := publicationClaimExpired(value, current)
	if err != nil {
		return err
	}
	if expired {
		return fmt.Errorf("%w: %s", ErrPublicationClaimExpired, value.ID)
	}
	return nil
}

func normalizePublicationURL(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	value, err := normalizedPublicationText(*raw, "publication URL", maxPublicationURLBytes, false)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	return &value, nil
}

func (s *Store) CompletePublication(
	ctx context.Context,
	id string,
	input CompletePublicationInput,
) (model.Publication, error) {
	if input.ClaimEpoch <= 0 {
		return model.Publication{}, errors.New("publication claim epoch must be positive")
	}
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	rawURL, err := normalizePublicationURL(input.URL)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		currentTime, currentTimestamp, err := s.publicationCurrent()
		if err != nil {
			return err
		}
		if err := requireLivePublicationClaim(
			current,
			input.ClaimToken,
			input.ClaimEpoch,
			currentTime,
		); err != nil {
			return err
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'published', url = ?, error = NULL, claim_token = NULL,
				claim_expires_at = NULL, published_at = ?, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'publishing' AND updated_at = ?
				AND claim_epoch = ? AND claim_token = ? AND claim_expires_at = ?
				AND claim_expires_at > ?
		`, nullableString(rawURL), timestamp, timestamp, current.ID, board,
			current.UpdatedAt, current.ClaimEpoch, current.ClaimToken,
			*current.ClaimExpiresAt, currentTimestamp)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_completed", map[string]any{
			"publicationId": current.ID, "claimEpoch": current.ClaimEpoch,
			"mode": current.Mode, "url": rawURL,
		}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationPublished
		current.URL = rawURL
		current.Error = nil
		current.ClaimToken = ""
		current.ClaimExpiresAt = nil
		current.PublishedAt = &timestamp
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}

func (s *Store) FailPublication(
	ctx context.Context,
	id string,
	input FailPublicationInput,
) (model.Publication, error) {
	if input.ClaimEpoch <= 0 {
		return model.Publication{}, errors.New("publication claim epoch must be positive")
	}
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, err
	}
	failure, err := boundedPublicationError(input.Error)
	if err != nil {
		return model.Publication{}, err
	}
	var value model.Publication
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		current, err := publicationForBoard(ctx, tx, id, board)
		if err != nil {
			return err
		}
		if err := requirePublicationVersion(current, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		currentTime, currentTimestamp, err := s.publicationCurrent()
		if err != nil {
			return err
		}
		if err := requireLivePublicationClaim(
			current,
			input.ClaimToken,
			input.ClaimEpoch,
			currentTime,
		); err != nil {
			return err
		}
		timestamp := now()
		update, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET status = 'failed', error = ?, claim_token = NULL,
				claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND board = ? AND status = 'publishing' AND updated_at = ?
				AND claim_epoch = ? AND claim_token = ? AND claim_expires_at = ?
				AND claim_expires_at > ?
		`, failure, timestamp, current.ID, board, current.UpdatedAt,
			current.ClaimEpoch, current.ClaimToken, *current.ClaimExpiresAt,
			currentTimestamp)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &PublicationUpdateConflictError{
				ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
			}
		}
		if err := appendEvent(ctx, tx, current.TaskID, "publication_failed", map[string]any{
			"publicationId": current.ID, "claimEpoch": current.ClaimEpoch,
			"error": failure,
		}, &current.RunID); err != nil {
			return err
		}
		current.Status = model.PublicationFailed
		current.Error = &failure
		current.ClaimToken = ""
		current.ClaimExpiresAt = nil
		current.UpdatedAt = timestamp
		value = publicPublication(current)
		return nil
	})
	return value, err
}
