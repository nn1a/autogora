package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

var ErrIntegrationResolutionExhausted = errors.New("integration resolution attempts exhausted")

type ReserveIntegrationResolutionInput struct {
	WorkspacePath       string
	PrerequisiteID      string
	ChangeSetID         string
	ConflictFingerprint string
	ConflictingFiles    []string
}

type StartIntegrationResolutionInput struct {
	ConflictFingerprint string
	ExpectedAttempt     int
	ExpectedMaxAttempts int
}

type IntegrationResolutionReservation struct {
	Attempt     int
	MaxAttempts int
	Started     bool
	StartedNow  bool
}

func normalizedResolutionFingerprint(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256DigestBytes {
		return "", errors.New("integration resolution requires a SHA-256 conflict fingerprint")
	}
	return value, nil
}

const sha256DigestBytes = 32

func requireIntegrationResolutionPolicy(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	workspacePath string,
) (model.RunWorkspace, error) {
	if task.WorkflowRole != model.WorkflowRoleFinalizer {
		return model.RunWorkspace{}, fmt.Errorf("integration resolution requires a finalizer task, got %s", task.WorkflowRole)
	}
	var allowWrites bool
	err := tx.QueryRowContext(ctx,
		"SELECT allow_writes FROM managed_run_policies WHERE run_id = ?", run.ID,
	).Scan(&allowWrites)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !allowWrites) {
		return model.RunWorkspace{}, errors.New("integration resolution requires persisted dispatcher write permission")
	}
	if err != nil {
		return model.RunWorkspace{}, err
	}
	workspace, err := scanRunWorkspace(tx.QueryRowContext(ctx, `SELECT run_id, task_id, path, kind,
		repository_path, base_commit, generated, prepared_at FROM run_workspaces WHERE run_id = ?`, run.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.RunWorkspace{}, errors.New("integration resolution requires a prepared worktree")
	}
	if err != nil {
		return model.RunWorkspace{}, err
	}
	if workspace.Kind != model.WorkspaceWorktree || !workspace.Generated {
		return model.RunWorkspace{}, errors.New("integration resolution requires a generated isolated worktree")
	}
	if workspacePath != "" && workspace.Path != workspacePath {
		return model.RunWorkspace{}, fmt.Errorf("integration resolution workspace changed from %s to %s", workspace.Path, workspacePath)
	}
	return workspace, nil
}

func previousStartedResolutionAttempt(
	ctx context.Context,
	tx *sql.Tx,
	taskID, fingerprint, excludedRunID string,
) (int, error) {
	query := `SELECT COALESCE(MAX(attempt), 0) FROM integration_resolution_attempts
		WHERE task_id = ? AND conflict_fingerprint = ? AND started_at IS NOT NULL`
	args := []any{taskID, fingerprint}
	if excludedRunID != "" {
		query += " AND run_id <> ?"
		args = append(args, excludedRunID)
	}
	var previous int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&previous)
	return previous, err
}

// ReserveIntegrationResolution prepares a conflict handoff without consuming
// retry budget. StartIntegrationResolutionAttempt performs the durable charge
// at the executor's final pre-spawn boundary.
func (s *Store) ReserveIntegrationResolution(
	ctx context.Context,
	scope RunScope,
	input ReserveIntegrationResolutionInput,
) (IntegrationResolutionReservation, error) {
	workspacePath, err := filepath.Abs(strings.TrimSpace(input.WorkspacePath))
	if err != nil || strings.TrimSpace(input.WorkspacePath) == "" {
		return IntegrationResolutionReservation{}, errors.New("integration resolution workspace path cannot be empty")
	}
	input.PrerequisiteID = strings.TrimSpace(input.PrerequisiteID)
	input.ChangeSetID = strings.TrimSpace(input.ChangeSetID)
	if input.PrerequisiteID == "" || input.ChangeSetID == "" {
		return IntegrationResolutionReservation{}, errors.New("integration resolution requires prerequisite and change set ids")
	}
	input.ConflictFingerprint, err = normalizedResolutionFingerprint(input.ConflictFingerprint)
	if err != nil {
		return IntegrationResolutionReservation{}, err
	}

	var reservation IntegrationResolutionReservation
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		workspace, err := requireIntegrationResolutionPolicy(ctx, tx, task, run, workspacePath)
		if err != nil {
			return err
		}
		maxAttempts := max(1, task.MaxRetries)

		var existingTaskID, existingFingerprint, existingWorkspace string
		var existingPrerequisiteID, existingChangeSetID string
		var existingAttempt sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT task_id, conflict_fingerprint, attempt, max_attempts,
			workspace_path, prerequisite_id, change_set_id
			FROM integration_resolution_attempts WHERE run_id = ?`, run.ID).Scan(
			&existingTaskID, &existingFingerprint, &existingAttempt, &reservation.MaxAttempts,
			&existingWorkspace, &existingPrerequisiteID, &existingChangeSetID,
		)
		if err == nil {
			if existingTaskID != task.ID || existingFingerprint != input.ConflictFingerprint ||
				existingWorkspace != workspace.Path || existingPrerequisiteID != input.PrerequisiteID ||
				existingChangeSetID != input.ChangeSetID {
				return errors.New("run already prepared a different integration resolution handoff")
			}
			if existingAttempt.Valid {
				reservation.Attempt = int(existingAttempt.Int64)
				reservation.Started = true
				return nil
			}
			previous, err := previousStartedResolutionAttempt(ctx, tx, task.ID, input.ConflictFingerprint, run.ID)
			if err != nil {
				return err
			}
			reservation.Attempt = previous + 1
			reservation.MaxAttempts = maxAttempts
			if previous >= maxAttempts {
				return fmt.Errorf("%w after %d attempt(s)", ErrIntegrationResolutionExhausted, previous)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		previous, err := previousStartedResolutionAttempt(ctx, tx, task.ID, input.ConflictFingerprint, "")
		if err != nil {
			return err
		}
		reservation = IntegrationResolutionReservation{Attempt: previous + 1, MaxAttempts: maxAttempts}
		if previous >= maxAttempts {
			return fmt.Errorf("%w after %d attempt(s)", ErrIntegrationResolutionExhausted, previous)
		}
		filesJSON, err := json.Marshal(boundedResolutionFiles(input.ConflictingFiles))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO integration_resolution_attempts(
			task_id, conflict_fingerprint, run_id, attempt, max_attempts, workspace_path,
			prerequisite_id, change_set_id, conflicting_files_json, prepared_at, started_at
		) VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, NULL)`,
			task.ID, input.ConflictFingerprint, run.ID, maxAttempts, workspace.Path,
			input.PrerequisiteID, input.ChangeSetID, string(filesJSON), now(),
		)
		return err
	})
	return reservation, err
}

// StartIntegrationResolutionAttempt consumes retry budget immediately before
// exec.Cmd.Start. It is claim-scoped and idempotent for one run.
func (s *Store) StartIntegrationResolutionAttempt(
	ctx context.Context,
	scope RunScope,
	input StartIntegrationResolutionInput,
) (IntegrationResolutionReservation, error) {
	fingerprint, err := normalizedResolutionFingerprint(input.ConflictFingerprint)
	if err != nil {
		return IntegrationResolutionReservation{}, err
	}
	if input.ExpectedAttempt < 1 {
		return IntegrationResolutionReservation{}, errors.New("integration resolution expected attempt must be positive")
	}
	if input.ExpectedMaxAttempts < input.ExpectedAttempt {
		return IntegrationResolutionReservation{}, errors.New("integration resolution expected maximum is invalid")
	}
	var reservation IntegrationResolutionReservation
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		workspace, err := requireIntegrationResolutionPolicy(ctx, tx, task, run, "")
		if err != nil {
			return err
		}
		var rowTaskID, rowFingerprint, rowWorkspace, prerequisiteID, changeSetID, filesJSON string
		var attempt sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT task_id, conflict_fingerprint, attempt, max_attempts,
			workspace_path, prerequisite_id, change_set_id, conflicting_files_json
			FROM integration_resolution_attempts WHERE run_id = ?`, run.ID).Scan(
			&rowTaskID, &rowFingerprint, &attempt, &reservation.MaxAttempts, &rowWorkspace,
			&prerequisiteID, &changeSetID, &filesJSON,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("integration resolution was not prepared for this run")
		}
		if err != nil {
			return err
		}
		if rowTaskID != task.ID || rowFingerprint != fingerprint || rowWorkspace != workspace.Path {
			return errors.New("prepared integration resolution no longer matches the active claim")
		}
		if attempt.Valid {
			reservation.Attempt = int(attempt.Int64)
			reservation.Started = true
			if reservation.Attempt != input.ExpectedAttempt ||
				reservation.MaxAttempts != input.ExpectedMaxAttempts {
				return fmt.Errorf("integration resolution already started as attempt %d, expected %d", reservation.Attempt, input.ExpectedAttempt)
			}
			return nil
		}

		previous, err := previousStartedResolutionAttempt(ctx, tx, task.ID, fingerprint, run.ID)
		if err != nil {
			return err
		}
		maxAttempts := max(1, task.MaxRetries)
		reservation = IntegrationResolutionReservation{
			Attempt: previous + 1, MaxAttempts: maxAttempts,
		}
		if previous >= maxAttempts {
			return fmt.Errorf("%w after %d attempt(s)", ErrIntegrationResolutionExhausted, previous)
		}
		if reservation.Attempt != input.ExpectedAttempt {
			return fmt.Errorf(
				"integration resolution attempt changed from %d to %d before launch",
				input.ExpectedAttempt, reservation.Attempt,
			)
		}
		if reservation.MaxAttempts != input.ExpectedMaxAttempts {
			return fmt.Errorf(
				"integration resolution maximum changed from %d to %d before launch",
				input.ExpectedMaxAttempts, reservation.MaxAttempts,
			)
		}
		startedAt := now()
		result, err := tx.ExecContext(ctx, `UPDATE integration_resolution_attempts
			SET attempt = ?, max_attempts = ?, started_at = ?
			WHERE run_id = ? AND attempt IS NULL AND started_at IS NULL`,
			reservation.Attempt, maxAttempts, startedAt, run.ID,
		)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return errors.New("integration resolution start lost its compare-and-swap")
		}
		var files []string
		if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, task.ID, "integration_resolution_started", map[string]any{
			"attempt": reservation.Attempt, "maxAttempts": maxAttempts,
			"workspacePath": workspace.Path, "prerequisiteId": prerequisiteID,
			"changeSetId": changeSetID, "conflictFingerprint": fingerprint,
			"conflictingFiles": files,
		}, &run.ID); err != nil {
			return err
		}
		reservation.Started = true
		reservation.StartedNow = true
		return nil
	})
	return reservation, err
}

// CompensateIntegrationResolutionStart refunds the exact attempt charged by a
// start gate when exec.Cmd.Start fails before any process exists.
func (s *Store) CompensateIntegrationResolutionStart(
	ctx context.Context,
	scope RunScope,
	input StartIntegrationResolutionInput,
) error {
	fingerprint, err := normalizedResolutionFingerprint(input.ConflictFingerprint)
	if err != nil {
		return err
	}
	if input.ExpectedAttempt < 1 {
		return errors.New("integration resolution compensated attempt must be positive")
	}
	return s.withWrite(ctx, func(tx *sql.Tx) error {
		var taskID, claimToken string
		err := tx.QueryRowContext(ctx,
			"SELECT task_id, claim_token FROM task_runs WHERE id = ?", scope.RunID,
		).Scan(&taskID, &claimToken)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run not found: %s", scope.RunID)
		}
		if err != nil {
			return err
		}
		if claimToken != scope.ClaimToken {
			return errors.New("invalid claim token")
		}
		var rowTaskID, rowFingerprint string
		var attempt sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT task_id, conflict_fingerprint, attempt
			FROM integration_resolution_attempts WHERE run_id = ?`, scope.RunID).Scan(
			&rowTaskID, &rowFingerprint, &attempt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("integration resolution was not prepared for compensation")
		}
		if err != nil {
			return err
		}
		if rowTaskID != taskID || rowFingerprint != fingerprint {
			return errors.New("integration resolution compensation does not match its prepared handoff")
		}
		if !attempt.Valid {
			return nil
		}
		if int(attempt.Int64) != input.ExpectedAttempt {
			return fmt.Errorf(
				"integration resolution compensation expected attempt %d, found %d",
				input.ExpectedAttempt, attempt.Int64,
			)
		}
		result, err := tx.ExecContext(ctx, `UPDATE integration_resolution_attempts
			SET attempt = NULL, started_at = NULL
			WHERE run_id = ? AND conflict_fingerprint = ? AND attempt = ? AND started_at IS NOT NULL`,
			scope.RunID, fingerprint, input.ExpectedAttempt,
		)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return errors.New("integration resolution compensation lost its compare-and-swap")
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM task_events
			WHERE task_id = ? AND run_id = ? AND kind = 'integration_resolution_started'`,
			taskID, scope.RunID,
		)
		return err
	})
}

func boundedResolutionFiles(values []string) []string {
	const fileLimit = 20
	const byteLimit = 512
	result := make([]string, 0, min(len(values), fileLimit))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToValidUTF8(strings.TrimSpace(value), "\uFFFD")
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		if len(value) > byteLimit {
			value = value[:byteLimit]
			for !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
		}
		result = append(result, value)
		if len(result) == fileLimit {
			break
		}
	}
	return result
}
