package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

const (
	maxRecoveryCheckpointChangedFiles     = 10_000
	maxRecoveryCheckpointChangedFileBytes = 4_096
	maxRecoveryCheckpointChangedJSONBytes = 16 << 20
)

var errRecoveryCheckpointNotFound = errors.New("recovery checkpoint not found")

// RegisterRecoveryCheckpointInput is the immutable Git snapshot captured from
// an unsuccessful run. TaskUpdatedAt is intentionally read inside the store
// transaction: it is audit provenance, not a recovery-compatibility token.
type RegisterRecoveryCheckpointInput struct {
	RepositoryPath          string
	WorktreePath            string
	OutputBaseCommit        string
	StartCommit             string
	HeadCommit              string
	DurableRef              string
	ChangedFiles            []string
	TaskSpecFingerprint     string
	PrerequisiteFingerprint string
}

// ReserveRecoveryCheckpointInput carries caller-computed fingerprints for the
// newly active run. A claim or heartbeat changes tasks.updated_at, so only
// these immutable semantic fingerprints decide whether partial work is safe to
// adopt.
type ReserveRecoveryCheckpointInput struct {
	CheckpointID            string
	TaskSpecFingerprint     string
	PrerequisiteFingerprint string
}

type normalizedRecoveryCheckpointInput struct {
	RegisterRecoveryCheckpointInput
	changedFilesJSON string
}

const recoveryCheckpointColumns = `id, task_id, source_run_id, repository_path, worktree_path,
	output_base_commit, start_commit, head_commit, durable_ref, changed_files_json,
	task_updated_at, task_spec_fingerprint, prerequisite_fingerprint, state,
	reserved_run_id, reservation_token, reserved_at, last_released_run_id,
	last_release_token, last_released_at, adopted_output_base_commit,
	adopted_head_commit, adopted_at, consumed_at, superseded_at,
	superseded_by_id, supersede_reason, created_at, updated_at`

func scanRecoveryCheckpoint(row scanner) (model.RecoveryCheckpoint, error) {
	var value model.RecoveryCheckpoint
	var changedFilesJSON string
	var state string
	var reservedRunID, reservationToken, reservedAt sql.NullString
	var lastReleasedRunID, lastReleaseToken, lastReleasedAt sql.NullString
	var adoptedOutputBaseCommit, adoptedHeadCommit, adoptedAt, consumedAt, supersededAt sql.NullString
	var supersededByID, supersedeReason sql.NullString
	err := row.Scan(
		&value.ID, &value.TaskID, &value.SourceRunID, &value.RepositoryPath,
		&value.WorktreePath, &value.OutputBaseCommit, &value.StartCommit,
		&value.HeadCommit, &value.DurableRef, &changedFilesJSON,
		&value.TaskUpdatedAt, &value.TaskSpecFingerprint,
		&value.PrerequisiteFingerprint, &state, &reservedRunID,
		&reservationToken, &reservedAt, &lastReleasedRunID, &lastReleaseToken,
		&lastReleasedAt, &adoptedOutputBaseCommit, &adoptedHeadCommit,
		&adoptedAt, &consumedAt, &supersededAt, &supersededByID,
		&supersedeReason, &value.CreatedAt, &value.UpdatedAt,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if decodeErr := json.Unmarshal([]byte(changedFilesJSON), &value.ChangedFiles); decodeErr != nil {
		return model.RecoveryCheckpoint{}, fmt.Errorf("decode recovery checkpoint changed files: %w", decodeErr)
	}
	if value.ChangedFiles == nil {
		value.ChangedFiles = []string{}
	}
	value.State = model.RecoveryCheckpointState(state)
	value.ReservedRunID = stringPointer(reservedRunID)
	if reservationToken.Valid {
		value.ReservationToken = reservationToken.String
	}
	value.ReservedAt = stringPointer(reservedAt)
	value.LastReleasedRunID = stringPointer(lastReleasedRunID)
	if lastReleaseToken.Valid {
		value.LastReleaseToken = lastReleaseToken.String
	}
	value.LastReleasedAt = stringPointer(lastReleasedAt)
	value.AdoptedOutputBaseCommit = stringPointer(adoptedOutputBaseCommit)
	value.AdoptedHeadCommit = stringPointer(adoptedHeadCommit)
	value.AdoptedAt = stringPointer(adoptedAt)
	value.ConsumedAt = stringPointer(consumedAt)
	value.SupersededAt = stringPointer(supersededAt)
	value.SupersededByID = stringPointer(supersededByID)
	value.SupersedeReason = stringPointer(supersedeReason)
	return value, nil
}

func normalizeRecoveryCheckpointText(value, field string, maxBytes int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("recovery checkpoint %s is required", field)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("recovery checkpoint %s contains invalid text", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("recovery checkpoint %s exceeds %d bytes", field, maxBytes)
	}
	return value, nil
}

func normalizeRecoveryCheckpointPath(value, field string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("recovery checkpoint %s is required", field)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("recovery checkpoint %s contains invalid text", field)
	}
	if len(value) > 4096 {
		return "", fmt.Errorf("recovery checkpoint %s exceeds %d bytes", field, 4096)
	}
	return value, nil
}

func normalizeRecoveryObjectID(value, field string) (string, error) {
	value, err := normalizeRecoveryCheckpointText(value, field, 128)
	if err != nil {
		return "", err
	}
	value = strings.ToLower(value)
	if len(value) != 40 && len(value) != 64 {
		return "", fmt.Errorf("recovery checkpoint %s must be a full Git object ID", field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("recovery checkpoint %s must be a full Git object ID", field)
	}
	return value, nil
}

func normalizeRecoveryFingerprint(value, field string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return "", fmt.Errorf("recovery checkpoint %s must be a SHA-256 fingerprint", field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("recovery checkpoint %s must be a SHA-256 fingerprint", field)
	}
	return value, nil
}

func normalizeRecoveryChangedFiles(files []string) ([]string, string, error) {
	if len(files) > maxRecoveryCheckpointChangedFiles {
		return nil, "", fmt.Errorf(
			"recovery checkpoint changed files exceed %d entries",
			maxRecoveryCheckpointChangedFiles,
		)
	}
	seen := make(map[string]struct{}, len(files))
	normalized := make([]string, 0, len(files))
	for _, file := range files {
		if file == "" || !utf8.ValidString(file) || strings.ContainsRune(file, '\x00') {
			return nil, "", errors.New("recovery checkpoint changed file contains an invalid path")
		}
		if len(file) > maxRecoveryCheckpointChangedFileBytes {
			return nil, "", fmt.Errorf(
				"recovery checkpoint changed file exceeds %d bytes",
				maxRecoveryCheckpointChangedFileBytes,
			)
		}
		if _, exists := seen[file]; exists {
			continue
		}
		seen[file] = struct{}{}
		normalized = append(normalized, file)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", err
	}
	if len(encoded) > maxRecoveryCheckpointChangedJSONBytes {
		return nil, "", fmt.Errorf(
			"recovery checkpoint changed files exceed %d bytes",
			maxRecoveryCheckpointChangedJSONBytes,
		)
	}
	return normalized, string(encoded), nil
}

func normalizeRecoveryCheckpointInput(
	raw RegisterRecoveryCheckpointInput,
) (normalizedRecoveryCheckpointInput, error) {
	var result normalizedRecoveryCheckpointInput
	var err error
	if result.RepositoryPath, err = normalizeRecoveryCheckpointPath(raw.RepositoryPath, "repository path"); err != nil {
		return result, err
	}
	if result.WorktreePath, err = normalizeRecoveryCheckpointPath(raw.WorktreePath, "worktree path"); err != nil {
		return result, err
	}
	if result.OutputBaseCommit, err = normalizeRecoveryObjectID(raw.OutputBaseCommit, "output base commit"); err != nil {
		return result, err
	}
	if result.StartCommit, err = normalizeRecoveryObjectID(raw.StartCommit, "start commit"); err != nil {
		return result, err
	}
	if result.HeadCommit, err = normalizeRecoveryObjectID(raw.HeadCommit, "head commit"); err != nil {
		return result, err
	}
	if result.DurableRef, err = normalizeRecoveryCheckpointText(raw.DurableRef, "durable ref", 4096); err != nil {
		return result, err
	}
	if !strings.HasPrefix(result.DurableRef, "refs/") {
		return result, errors.New("recovery checkpoint durable ref must start with refs/")
	}
	if result.TaskSpecFingerprint, err = normalizeRecoveryFingerprint(raw.TaskSpecFingerprint, "task spec fingerprint"); err != nil {
		return result, err
	}
	if result.PrerequisiteFingerprint, err = normalizeRecoveryFingerprint(raw.PrerequisiteFingerprint, "prerequisite fingerprint"); err != nil {
		return result, err
	}
	result.ChangedFiles, result.changedFilesJSON, err = normalizeRecoveryChangedFiles(raw.ChangedFiles)
	return result, err
}

func sameRecoverySnapshot(
	checkpoint model.RecoveryCheckpoint,
	input normalizedRecoveryCheckpointInput,
) bool {
	if checkpoint.RepositoryPath != input.RepositoryPath ||
		checkpoint.WorktreePath != input.WorktreePath ||
		checkpoint.OutputBaseCommit != input.OutputBaseCommit ||
		checkpoint.StartCommit != input.StartCommit ||
		checkpoint.HeadCommit != input.HeadCommit ||
		checkpoint.DurableRef != input.DurableRef ||
		checkpoint.TaskSpecFingerprint != input.TaskSpecFingerprint ||
		checkpoint.PrerequisiteFingerprint != input.PrerequisiteFingerprint ||
		len(checkpoint.ChangedFiles) != len(input.ChangedFiles) {
		return false
	}
	for index := range checkpoint.ChangedFiles {
		if checkpoint.ChangedFiles[index] != input.ChangedFiles[index] {
			return false
		}
	}
	return true
}

func requireRecoveryCheckpointRunRef(
	input normalizedRecoveryCheckpointInput,
	runID string,
) error {
	expected := "refs/autogora/checkpoints/" + runID
	if input.DurableRef != expected {
		return fmt.Errorf(
			"recovery checkpoint durable ref must be %s for source run %s",
			expected,
			runID,
		)
	}
	return nil
}

func getRecoveryCheckpoint(
	ctx context.Context,
	q querier,
	checkpointID string,
) (model.RecoveryCheckpoint, error) {
	value, err := scanRecoveryCheckpoint(q.QueryRowContext(
		ctx,
		"SELECT "+recoveryCheckpointColumns+" FROM recovery_checkpoints WHERE id = ?",
		checkpointID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return model.RecoveryCheckpoint{}, fmt.Errorf("%w: %s", errRecoveryCheckpointNotFound, checkpointID)
	}
	return value, err
}

func getRecoveryCheckpointBySourceRun(
	ctx context.Context,
	q querier,
	runID string,
) (*model.RecoveryCheckpoint, error) {
	value, err := scanRecoveryCheckpoint(q.QueryRowContext(
		ctx,
		"SELECT "+recoveryCheckpointColumns+" FROM recovery_checkpoints WHERE source_run_id = ?",
		runID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func getRecoveryCheckpointByReservedRun(
	ctx context.Context,
	q querier,
	runID string,
) (*model.RecoveryCheckpoint, error) {
	value, err := scanRecoveryCheckpoint(q.QueryRowContext(
		ctx,
		"SELECT "+recoveryCheckpointColumns+" FROM recovery_checkpoints WHERE reserved_run_id = ?",
		runID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) GetRecoveryCheckpoint(
	ctx context.Context,
	checkpointID string,
) (*model.RecoveryCheckpoint, error) {
	checkpointID = strings.TrimSpace(checkpointID)
	if checkpointID == "" {
		return nil, errors.New("recovery checkpoint ID is required")
	}
	value, err := getRecoveryCheckpoint(ctx, s.db, checkpointID)
	if errors.Is(err, errRecoveryCheckpointNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) GetActiveRecoveryCheckpoint(
	ctx context.Context,
	taskID string,
) (*model.RecoveryCheckpoint, error) {
	taskID = strings.TrimSpace(taskID)
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	value, err := scanRecoveryCheckpoint(s.db.QueryRowContext(
		ctx,
		`SELECT `+recoveryCheckpointColumns+` FROM recovery_checkpoints
		 WHERE task_id = ? AND state IN ('pending', 'reserved', 'adopted')`,
		taskID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) GetRunRecoveryCheckpoint(
	ctx context.Context,
	runID string,
) (*model.RecoveryCheckpoint, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("run ID is required")
	}
	return getRecoveryCheckpointByReservedRun(ctx, s.db, runID)
}

func (s *Store) ListRecoveryCheckpoints(
	ctx context.Context,
	taskID string,
) ([]model.RecoveryCheckpoint, error) {
	taskID = strings.TrimSpace(taskID)
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(
		ctx,
		"SELECT "+recoveryCheckpointColumns+" FROM recovery_checkpoints WHERE task_id = ? ORDER BY created_at, id",
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.RecoveryCheckpoint, 0)
	for rows.Next() {
		value, err := scanRecoveryCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func requireRunScope(
	ctx context.Context,
	q querier,
	scope RunScope,
) (model.Run, error) {
	run, err := getRun(ctx, q, strings.TrimSpace(scope.RunID))
	if err != nil {
		return model.Run{}, err
	}
	var token string
	if err := q.QueryRowContext(ctx, "SELECT claim_token FROM task_runs WHERE id = ?", run.ID).Scan(&token); err != nil {
		return model.Run{}, err
	}
	if strings.TrimSpace(scope.ClaimToken) == "" || token != scope.ClaimToken {
		return model.Run{}, errors.New("invalid claim token")
	}
	return run, nil
}

func insertRecoveryCheckpoint(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	input normalizedRecoveryCheckpointInput,
) (model.RecoveryCheckpoint, error) {
	if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	active, err := scanRecoveryCheckpoint(tx.QueryRowContext(
		ctx,
		`SELECT `+recoveryCheckpointColumns+` FROM recovery_checkpoints
		 WHERE task_id = ? AND state IN ('pending', 'reserved', 'adopted')`,
		task.ID,
	))
	switch {
	case err == nil:
		return model.RecoveryCheckpoint{}, fmt.Errorf(
			"task already has active recovery checkpoint %s in state %s",
			active.ID,
			active.State,
		)
	case !errors.Is(err, sql.ErrNoRows):
		return model.RecoveryCheckpoint{}, err
	}

	id, timestamp := newID("rcp"), now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_checkpoints(
		id, task_id, source_run_id, repository_path, worktree_path,
		output_base_commit, start_commit, head_commit, durable_ref,
		changed_files_json, task_updated_at, task_spec_fingerprint,
		prerequisite_fingerprint, state, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		id, task.ID, run.ID, input.RepositoryPath, input.WorktreePath,
		input.OutputBaseCommit, input.StartCommit, input.HeadCommit,
		input.DurableRef, input.changedFilesJSON, task.UpdatedAt,
		input.TaskSpecFingerprint, input.PrerequisiteFingerprint,
		timestamp, timestamp,
	); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_registered", map[string]any{
		"checkpointId":             id,
		"outputBaseCommit":         input.OutputBaseCommit,
		"startCommit":              input.StartCommit,
		"headCommit":               input.HeadCommit,
		"durableRef":               input.DurableRef,
		"changedFileCount":         len(input.ChangedFiles),
		"taskUpdatedAt":            task.UpdatedAt,
		"taskSpecFingerprint":      input.TaskSpecFingerprint,
		"prerequisiteFingerprint":  input.PrerequisiteFingerprint,
		"dependencySatisfaction":   false,
		"hiddenPartialWorkHandoff": true,
	}, &run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	return getRecoveryCheckpoint(ctx, tx, id)
}

func validRecoveryFailureOutcome(outcome model.RunStatus) bool {
	if outcome == "" {
		return true
	}
	switch outcome {
	case model.RunStatusFailed, model.RunStatusReclaimed, model.RunStatusCrashed,
		model.RunStatusTimedOut, model.RunStatusRateLimited,
		model.RunStatusSpawnFailed, model.RunStatusProtocolViolation:
		return true
	default:
		return false
	}
}

// RegisterRecoveryCheckpointAndFailRun atomically records a partial-work
// snapshot while the source claim still owns the task, then terminalizes that
// run. A dispatcher can never observe a retryable task without its checkpoint.
func (s *Store) RegisterRecoveryCheckpointAndFailRun(
	ctx context.Context,
	scope RunScope,
	raw RegisterRecoveryCheckpointInput,
	runError string,
	options FailRunOptions,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	input, err := normalizeRecoveryCheckpointInput(raw)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	runError = strings.TrimSpace(runError)
	if runError == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New("recovery checkpoint failure requires an error")
	}
	if !validRecoveryFailureOutcome(options.Outcome) {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, fmt.Errorf(
			"invalid recovery checkpoint failure outcome: %s",
			options.Outcome,
		)
	}

	var checkpoint model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		scopedRun, err := requireRunScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, scopedRun.ID); err != nil {
			return err
		}
		taskID = scopedRun.TaskID
		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, scopedRun.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New("source run already registered a different recovery checkpoint")
			}
			checkpoint = *existing
			if scopedRun.Status != model.RunStatusRunning {
				return nil
			}
		}
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if existing == nil {
			checkpoint, err = insertRecoveryCheckpoint(ctx, tx, task, run, input)
			if err != nil {
				return err
			}
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, options)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return checkpoint, detail, err
}

func recoveryFingerprintMismatchReason(
	checkpoint model.RecoveryCheckpoint,
	taskFingerprint string,
	prerequisiteFingerprint string,
) string {
	switch {
	case checkpoint.TaskSpecFingerprint != taskFingerprint &&
		checkpoint.PrerequisiteFingerprint != prerequisiteFingerprint:
		return "task specification and prerequisites changed"
	case checkpoint.TaskSpecFingerprint != taskFingerprint:
		return "task specification changed"
	default:
		return "prerequisites changed"
	}
}

// ReserveRecoveryCheckpoint fences one pending checkpoint to the current run.
// If semantic fingerprints changed, the stale checkpoint is terminally
// superseded in the same transaction and the method returns it with reserved
// false so the run can continue from a fresh workspace.
func (s *Store) ReserveRecoveryCheckpoint(
	ctx context.Context,
	scope RunScope,
	raw ReserveRecoveryCheckpointInput,
) (*model.RecoveryCheckpoint, bool, error) {
	checkpointID := strings.TrimSpace(raw.CheckpointID)
	if checkpointID == "" {
		return nil, false, errors.New("recovery checkpoint ID is required")
	}
	taskFingerprint, err := normalizeRecoveryFingerprint(raw.TaskSpecFingerprint, "task spec fingerprint")
	if err != nil {
		return nil, false, err
	}
	prerequisiteFingerprint, err := normalizeRecoveryFingerprint(raw.PrerequisiteFingerprint, "prerequisite fingerprint")
	if err != nil {
		return nil, false, err
	}

	var checkpoint model.RecoveryCheckpoint
	reserved := false
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		checkpoint, err = getRecoveryCheckpoint(ctx, tx, checkpointID)
		if err != nil {
			return err
		}
		if checkpoint.TaskID != task.ID {
			return errors.New("recovery checkpoint does not belong to the active task")
		}
		if checkpoint.State == model.RecoveryCheckpointReserved ||
			checkpoint.State == model.RecoveryCheckpointAdopted {
			if checkpoint.ReservedRunID != nil && *checkpoint.ReservedRunID == run.ID &&
				checkpoint.TaskSpecFingerprint == taskFingerprint &&
				checkpoint.PrerequisiteFingerprint == prerequisiteFingerprint {
				reserved = true
				return nil
			}
			return fmt.Errorf("recovery checkpoint is already %s by another run", checkpoint.State)
		}
		if checkpoint.State == model.RecoveryCheckpointSuperseded &&
			checkpoint.ReservedRunID == nil &&
			checkpoint.SupersededByID == nil &&
			checkpoint.SupersedeReason != nil &&
			(checkpoint.TaskSpecFingerprint != taskFingerprint ||
				checkpoint.PrerequisiteFingerprint != prerequisiteFingerprint) &&
			*checkpoint.SupersedeReason == recoveryFingerprintMismatchReason(
				checkpoint,
				taskFingerprint,
				prerequisiteFingerprint,
			) {
			return nil
		}
		if checkpoint.State != model.RecoveryCheckpointPending {
			return fmt.Errorf("recovery checkpoint is not pending: %s", checkpoint.State)
		}
		if checkpoint.TaskSpecFingerprint != taskFingerprint ||
			checkpoint.PrerequisiteFingerprint != prerequisiteFingerprint {
			reason := recoveryFingerprintMismatchReason(
				checkpoint,
				taskFingerprint,
				prerequisiteFingerprint,
			)
			timestamp := now()
			result, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
				SET state = 'superseded', superseded_at = ?, supersede_reason = ?, updated_at = ?
				WHERE id = ? AND task_id = ? AND state = 'pending'`,
				timestamp, reason, timestamp, checkpoint.ID, task.ID,
			)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return errors.New("recovery checkpoint changed before fingerprint validation")
			}
			if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_superseded", map[string]any{
				"checkpointId":            checkpoint.ID,
				"reason":                  reason,
				"taskSpecFingerprint":     taskFingerprint,
				"prerequisiteFingerprint": prerequisiteFingerprint,
			}, &run.ID); err != nil {
				return err
			}
			checkpoint, err = getRecoveryCheckpoint(ctx, tx, checkpoint.ID)
			return err
		}

		token, err := claimToken()
		if err != nil {
			return err
		}
		timestamp := now()
		result, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
			SET state = 'reserved', reserved_run_id = ?, reservation_token = ?,
				reserved_at = ?, last_released_run_id = NULL,
				last_release_token = NULL, last_released_at = NULL, updated_at = ?
			WHERE id = ? AND task_id = ? AND state = 'pending'`,
			run.ID, token, timestamp, timestamp, checkpoint.ID, task.ID,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("recovery checkpoint changed before reservation")
		}
		if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_reserved", map[string]any{
			"checkpointId": checkpoint.ID,
			"sourceRunId":  checkpoint.SourceRunID,
			"headCommit":   checkpoint.HeadCommit,
		}, &run.ID); err != nil {
			return err
		}
		checkpoint, err = getRecoveryCheckpoint(ctx, tx, checkpoint.ID)
		if err == nil {
			reserved = true
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return &checkpoint, reserved, nil
}

func requireOwnedRecoveryCheckpoint(
	ctx context.Context,
	q querier,
	task model.Task,
	run model.Run,
	checkpointID string,
	reservationToken string,
) (model.RecoveryCheckpoint, error) {
	checkpointID = strings.TrimSpace(checkpointID)
	reservationToken = strings.TrimSpace(reservationToken)
	if checkpointID == "" || reservationToken == "" {
		return model.RecoveryCheckpoint{}, errors.New("recovery checkpoint ID and reservation token are required")
	}
	checkpoint, err := getRecoveryCheckpoint(ctx, q, checkpointID)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if checkpoint.TaskID != task.ID || checkpoint.ReservedRunID == nil ||
		*checkpoint.ReservedRunID != run.ID ||
		checkpoint.ReservationToken != reservationToken {
		return model.RecoveryCheckpoint{}, errors.New("invalid recovery checkpoint reservation")
	}
	return checkpoint, nil
}

func releaseRecoveryCheckpointReservation(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	checkpointID string,
	reservationToken string,
) (model.RecoveryCheckpoint, error) {
	checkpointID = strings.TrimSpace(checkpointID)
	reservationToken = strings.TrimSpace(reservationToken)
	checkpoint, err := getRecoveryCheckpoint(ctx, tx, checkpointID)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if checkpoint.TaskID != task.ID {
		return model.RecoveryCheckpoint{}, errors.New(
			"recovery checkpoint does not belong to the active task",
		)
	}
	if checkpoint.State == model.RecoveryCheckpointPending &&
		checkpoint.LastReleasedRunID != nil &&
		*checkpoint.LastReleasedRunID == run.ID &&
		checkpoint.LastReleaseToken == reservationToken {
		return checkpoint, nil
	}
	checkpoint, err = requireOwnedRecoveryCheckpoint(
		ctx,
		tx,
		task,
		run,
		checkpointID,
		reservationToken,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if checkpoint.State != model.RecoveryCheckpointReserved {
		return model.RecoveryCheckpoint{}, fmt.Errorf(
			"only an unadopted recovery checkpoint reservation can be released: %s",
			checkpoint.State,
		)
	}
	timestamp := now()
	result, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
		SET state = 'pending', last_released_run_id = reserved_run_id,
			last_release_token = reservation_token, last_released_at = ?,
			reserved_run_id = NULL, reservation_token = NULL,
			reserved_at = NULL, updated_at = ?
		WHERE id = ? AND state = 'reserved' AND reserved_run_id = ? AND reservation_token = ?`,
		timestamp, timestamp, checkpoint.ID, run.ID, reservationToken,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if changed != 1 {
		return model.RecoveryCheckpoint{}, errors.New("recovery checkpoint reservation changed before release")
	}
	if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_released", map[string]any{
		"checkpointId": checkpoint.ID,
		"reason":       "launch or setup did not adopt the checkpoint",
	}, &run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	return getRecoveryCheckpoint(ctx, tx, checkpoint.ID)
}

func (s *Store) ReleaseRecoveryCheckpointReservation(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
) (model.RecoveryCheckpoint, error) {
	var checkpoint model.RecoveryCheckpoint
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		checkpoint, err = releaseRecoveryCheckpointReservation(
			ctx,
			tx,
			task,
			run,
			checkpointID,
			reservationToken,
		)
		return err
	})
	return checkpoint, err
}

// ReleaseRecoveryCheckpointReservationAndFailRun is the setup-failure path:
// the prior checkpoint becomes available again in the same transaction that
// terminalizes the run which could not launch.
func (s *Store) ReleaseRecoveryCheckpointReservationAndFailRun(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
	runError string,
	options FailRunOptions,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	runError = strings.TrimSpace(runError)
	if runError == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New("recovery launch failure requires an error")
	}
	if !validRecoveryFailureOutcome(options.Outcome) {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, fmt.Errorf(
			"invalid recovery checkpoint failure outcome: %s",
			options.Outcome,
		)
	}
	var checkpoint model.RecoveryCheckpoint
	taskID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		scopedRun, err := requireRunScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		taskID = scopedRun.TaskID
		if scopedRun.Status != model.RunStatusRunning {
			task, err := requireTask(ctx, tx, scopedRun.TaskID)
			if err != nil {
				return err
			}
			checkpoint, err = releaseRecoveryCheckpointReservation(
				ctx,
				tx,
				task,
				scopedRun,
				checkpointID,
				reservationToken,
			)
			return err
		}
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		checkpoint, err = releaseRecoveryCheckpointReservation(
			ctx,
			tx,
			task,
			run,
			checkpointID,
			reservationToken,
		)
		if err != nil {
			return err
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, options)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return checkpoint, detail, err
}

// ConfirmRecoveryCheckpointAdoption records the exact workspace HEAD after the
// host has incorporated the checkpoint. A later dirty check must use this
// immutable commit as the recovery run's start boundary.
func (s *Store) ConfirmRecoveryCheckpointAdoption(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
	adoptedOutputBaseCommit string,
	adoptedHeadCommit string,
) (model.RecoveryCheckpoint, error) {
	adoptedOutputBaseCommit, err := normalizeRecoveryObjectID(
		adoptedOutputBaseCommit,
		"adopted output base commit",
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	adoptedHeadCommit, err = normalizeRecoveryObjectID(adoptedHeadCommit, "adopted head commit")
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	var checkpoint model.RecoveryCheckpoint
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		checkpoint, err = requireOwnedRecoveryCheckpoint(
			ctx,
			tx,
			task,
			run,
			checkpointID,
			reservationToken,
		)
		if err != nil {
			return err
		}
		if checkpoint.State == model.RecoveryCheckpointAdopted {
			if checkpoint.AdoptedOutputBaseCommit != nil &&
				*checkpoint.AdoptedOutputBaseCommit == adoptedOutputBaseCommit &&
				checkpoint.AdoptedHeadCommit != nil &&
				*checkpoint.AdoptedHeadCommit == adoptedHeadCommit {
				return nil
			}
			return errors.New("recovery checkpoint was already adopted at a different base or head")
		}
		if checkpoint.State != model.RecoveryCheckpointReserved {
			return fmt.Errorf("recovery checkpoint cannot be adopted from state %s", checkpoint.State)
		}
		timestamp := now()
		result, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
			SET state = 'adopted', adopted_output_base_commit = ?,
				adopted_head_commit = ?, adopted_at = ?, updated_at = ?
			WHERE id = ? AND state = 'reserved' AND reserved_run_id = ? AND reservation_token = ?`,
			adoptedOutputBaseCommit, adoptedHeadCommit, timestamp, timestamp,
			checkpoint.ID, run.ID, reservationToken,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("recovery checkpoint changed before adoption")
		}
		if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_adopted", map[string]any{
			"checkpointId":            checkpoint.ID,
			"sourceHeadCommit":        checkpoint.HeadCommit,
			"adoptedOutputBaseCommit": adoptedOutputBaseCommit,
			"adoptedHeadCommit":       adoptedHeadCommit,
		}, &run.ID); err != nil {
			return err
		}
		checkpoint, err = getRecoveryCheckpoint(ctx, tx, checkpoint.ID)
		return err
	})
	return checkpoint, err
}

func supersedeAdoptedRecoveryCheckpoint(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	checkpoint model.RecoveryCheckpoint,
	input normalizedRecoveryCheckpointInput,
	reason string,
) (model.RecoveryCheckpoint, error) {
	if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if checkpoint.State != model.RecoveryCheckpointAdopted ||
		checkpoint.ReservedRunID == nil ||
		*checkpoint.ReservedRunID != run.ID ||
		checkpoint.AdoptedOutputBaseCommit == nil ||
		checkpoint.AdoptedHeadCommit == nil {
		return model.RecoveryCheckpoint{}, errors.New(
			"only the current run's adopted recovery checkpoint can be superseded by cumulative work",
		)
	}
	if input.RepositoryPath != checkpoint.RepositoryPath {
		return model.RecoveryCheckpoint{}, errors.New("cumulative recovery checkpoint changed repository")
	}
	if input.OutputBaseCommit != *checkpoint.AdoptedOutputBaseCommit {
		return model.RecoveryCheckpoint{}, errors.New(
			"cumulative recovery checkpoint does not use the adopted output base",
		)
	}
	if input.StartCommit != *checkpoint.AdoptedHeadCommit {
		return model.RecoveryCheckpoint{}, errors.New(
			"cumulative recovery checkpoint must start at the adopted head",
		)
	}
	if input.TaskSpecFingerprint != checkpoint.TaskSpecFingerprint ||
		input.PrerequisiteFingerprint != checkpoint.PrerequisiteFingerprint {
		return model.RecoveryCheckpoint{}, errors.New(
			"cumulative recovery checkpoint changed semantic fingerprints",
		)
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.RecoveryCheckpoint{}, errors.New(
			"recovery checkpoint supersession requires a reason",
		)
	}
	replacementID, timestamp := newID("rcp"), now()
	result, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
		SET state = 'superseded', superseded_at = ?, supersede_reason = ?, updated_at = ?
		WHERE id = ? AND state = 'adopted' AND reserved_run_id = ? AND reservation_token = ?`,
		timestamp, reason, timestamp, checkpoint.ID, run.ID, checkpoint.ReservationToken,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if changed != 1 {
		return model.RecoveryCheckpoint{}, errors.New(
			"recovery checkpoint changed before cumulative supersession",
		)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_checkpoints(
		id, task_id, source_run_id, repository_path, worktree_path,
		output_base_commit, start_commit, head_commit, durable_ref,
		changed_files_json, task_updated_at, task_spec_fingerprint,
		prerequisite_fingerprint, state, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		replacementID, task.ID, run.ID, input.RepositoryPath, input.WorktreePath,
		input.OutputBaseCommit, input.StartCommit, input.HeadCommit,
		input.DurableRef, input.changedFilesJSON, task.UpdatedAt,
		input.TaskSpecFingerprint, input.PrerequisiteFingerprint,
		timestamp, timestamp,
	); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_checkpoints
		SET superseded_by_id = ? WHERE id = ? AND state = 'superseded'
			AND superseded_by_id IS NULL`,
		replacementID, checkpoint.ID,
	); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if err := appendEvent(ctx, tx, task.ID, "recovery_checkpoint_superseded", map[string]any{
		"checkpointId":     checkpoint.ID,
		"replacementId":    replacementID,
		"reason":           reason,
		"outputBaseCommit": input.OutputBaseCommit,
		"startCommit":      input.StartCommit,
		"headCommit":       input.HeadCommit,
		"changedFileCount": len(input.ChangedFiles),
	}, &run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	return getRecoveryCheckpoint(ctx, tx, replacementID)
}

// SupersedeRecoveryCheckpointAndFailRun replaces an adopted checkpoint with
// the cumulative snapshot from the unsuccessful recovery run. The new start
// must be the immutable adopted HEAD. Its output base advances to the target
// base recorded at adoption, while semantic fingerprints remain anchored to
// the same task requirements.
func (s *Store) SupersedeRecoveryCheckpointAndFailRun(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
	raw RegisterRecoveryCheckpointInput,
	runError string,
	options FailRunOptions,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	input, err := normalizeRecoveryCheckpointInput(raw)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	runError = strings.TrimSpace(runError)
	if runError == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New("recovery checkpoint failure requires an error")
	}
	if !validRecoveryFailureOutcome(options.Outcome) {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, fmt.Errorf(
			"invalid recovery checkpoint failure outcome: %s",
			options.Outcome,
		)
	}

	var replacement model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		scopedRun, err := requireRunScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, scopedRun.ID); err != nil {
			return err
		}
		taskID = scopedRun.TaskID
		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, scopedRun.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New("recovery run already registered a different cumulative checkpoint")
			}
			previous, err := getRecoveryCheckpoint(ctx, tx, checkpointID)
			if err != nil {
				return err
			}
			if previous.State != model.RecoveryCheckpointSuperseded ||
				previous.SupersededByID == nil ||
				*previous.SupersededByID != existing.ID ||
				previous.ReservedRunID == nil ||
				*previous.ReservedRunID != scopedRun.ID ||
				previous.ReservationToken != strings.TrimSpace(reservationToken) {
				return errors.New("cumulative recovery checkpoint has inconsistent supersession provenance")
			}
			replacement = *existing
			if scopedRun.Status != model.RunStatusRunning {
				return nil
			}
		}
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if existing != nil {
			return finishUnsuccessful(ctx, tx, task, run, runError, options)
		}
		checkpoint, err := requireOwnedRecoveryCheckpoint(
			ctx,
			tx,
			task,
			run,
			checkpointID,
			reservationToken,
		)
		if err != nil {
			return err
		}
		replacement, err = supersedeAdoptedRecoveryCheckpoint(
			ctx,
			tx,
			task,
			run,
			checkpoint,
			input,
			"recovery run failed after adopting partial work",
		)
		if err != nil {
			return err
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, options)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return replacement, detail, err
}

func getSupersededRecoveryCheckpointByReplacement(
	ctx context.Context,
	q querier,
	replacementID string,
) (*model.RecoveryCheckpoint, error) {
	value, err := scanRecoveryCheckpoint(q.QueryRowContext(
		ctx,
		`SELECT `+recoveryCheckpointColumns+` FROM recovery_checkpoints
		 WHERE superseded_by_id = ?`,
		replacementID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func requireRecoveryReplacementProvenance(
	ctx context.Context,
	q querier,
	replacement model.RecoveryCheckpoint,
	sourceRunID string,
) (model.RecoveryCheckpoint, error) {
	previous, err := getSupersededRecoveryCheckpointByReplacement(
		ctx,
		q,
		replacement.ID,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if previous == nil ||
		previous.State != model.RecoveryCheckpointSuperseded ||
		previous.SupersededByID == nil ||
		*previous.SupersededByID != replacement.ID ||
		previous.ReservedRunID == nil ||
		*previous.ReservedRunID != sourceRunID ||
		previous.ReservationToken == "" {
		return model.RecoveryCheckpoint{}, errors.New(
			"cumulative recovery checkpoint has inconsistent supersession provenance",
		)
	}
	return *previous, nil
}

func finalizedBlockMatches(
	run model.Run,
	task model.Task,
	request *model.TerminalRequest,
	exitCode int,
) bool {
	if request == nil || request.Kind != "block" || request.FinalizedAt == nil ||
		run.Status != model.RunStatusBlocked ||
		run.ExitCode == nil || *run.ExitCode != exitCode ||
		task.CurrentRunID != nil {
		return false
	}
	reason := ""
	if request.Reason != nil {
		reason = *request.Reason
	}
	if !sameStringPointer(run.Error, reason) ||
		!sameStringPointer(task.BlockReason, reason) {
		return false
	}
	if request.BlockKind == nil {
		return task.BlockKind == nil
	}
	return sameBlockKindPointer(task.BlockKind, *request.BlockKind)
}

func finalizeRequestedBlockRecords(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	run model.Run,
	request *model.TerminalRequest,
	exitCode int,
) error {
	if request == nil || request.Kind != "block" {
		return errors.New("recovery checkpoint block requires a block terminal request")
	}
	if request.FinalizedAt != nil {
		return errors.New("block terminal request is already finalized")
	}
	reason := ""
	if request.Reason != nil {
		reason = *request.Reason
	}
	kind := model.BlockKind("")
	if request.BlockKind != nil {
		kind = *request.BlockKind
	}
	timestamp := now()
	result, err := tx.ExecContext(ctx, `UPDATE task_runs
		SET status = 'blocked', ended_at = ?, heartbeat_at = ?,
			exit_code = ?, error = ?
		WHERE id = ? AND status = 'running'`,
		timestamp,
		timestamp,
		exitCode,
		reason,
		run.ID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("run changed before block finalization")
	}
	if err := blockTaskRecord(
		ctx,
		tx,
		task,
		BlockInput{Reason: reason, Kind: kind},
		&run.ID,
	); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE run_terminal_requests
		SET finalized_at = ? WHERE run_id = ? AND finalized_at IS NULL`,
		timestamp,
		run.ID,
	)
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("block terminal request changed before finalization")
	}
	return nil
}

// RegisterRecoveryCheckpointAndFinalizeBlock preserves the first managed
// attempt's partial work and finalizes its already-requested block in one
// transaction. A later unblock can therefore adopt the checkpoint in a fresh
// worktree instead of silently starting over.
func (s *Store) RegisterRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	input, err := normalizeRecoveryCheckpointInput(raw)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}

	var checkpoint model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := requireRunScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
			return err
		}
		taskID = run.TaskID
		request, err := getTerminalRequest(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if request == nil || request.Kind != "block" {
			return errors.New(
				"recovery checkpoint block requires a block terminal request",
			)
		}

		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New(
					"blocked run already registered a different recovery checkpoint",
				)
			}
			checkpoint = *existing
			if run.Status != model.RunStatusRunning {
				task, err := requireTask(ctx, tx, run.TaskID)
				if err != nil {
					return err
				}
				if checkpoint.State != model.RecoveryCheckpointPending ||
					!finalizedBlockMatches(run, task, request, exitCode) {
					return errors.New(
						"recovery checkpoint block retry does not match terminal state",
					)
				}
				return nil
			}
		}

		task, activeRun, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if request.FinalizedAt != nil {
			return errors.New("block terminal request is already finalized")
		}
		if existing == nil {
			checkpoint, err = insertRecoveryCheckpoint(ctx, tx, task, activeRun, input)
			if err != nil {
				return err
			}
		}
		return finalizeRequestedBlockRecords(
			ctx,
			tx,
			task,
			activeRun,
			request,
			exitCode,
		)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return checkpoint, detail, err
}

// SupersedeRecoveryCheckpointAndFinalizeBlock preserves cumulative work from
// an adopted checkpoint and finalizes an already-requested worker block in one
// transaction. The returned replacement is pending and contains no reservation
// token. Repeating the same call after a committed-but-lost response is safe.
func (s *Store) SupersedeRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	input, err := normalizeRecoveryCheckpointInput(raw)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}

	var replacement model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := requireRunScope(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
			return err
		}
		taskID = run.TaskID
		request, err := getTerminalRequest(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if request == nil || request.Kind != "block" {
			return errors.New(
				"cumulative recovery checkpoint block requires a block terminal request",
			)
		}

		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New(
					"recovery run already registered a different cumulative checkpoint",
				)
			}
			previous, err := requireRecoveryReplacementProvenance(
				ctx,
				tx,
				*existing,
				run.ID,
			)
			if err != nil {
				return err
			}
			if previous.ID != strings.TrimSpace(checkpointID) ||
				previous.ReservationToken != strings.TrimSpace(reservationToken) {
				return errors.New(
					"cumulative recovery checkpoint has inconsistent reservation provenance",
				)
			}
			replacement = *existing
			if run.Status != model.RunStatusRunning {
				task, err := requireTask(ctx, tx, run.TaskID)
				if err != nil {
					return err
				}
				if replacement.State != model.RecoveryCheckpointPending ||
					!finalizedBlockMatches(run, task, request, exitCode) {
					return errors.New(
						"cumulative recovery checkpoint block retry does not match terminal state",
					)
				}
				return nil
			}
		}

		task, activeRun, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if request.FinalizedAt != nil {
			return errors.New("block terminal request is already finalized")
		}
		if existing == nil {
			checkpoint, err := requireOwnedRecoveryCheckpoint(
				ctx,
				tx,
				task,
				activeRun,
				checkpointID,
				reservationToken,
			)
			if err != nil {
				return err
			}
			replacement, err = supersedeAdoptedRecoveryCheckpoint(
				ctx,
				tx,
				task,
				activeRun,
				checkpoint,
				input,
				"recovery run blocked after adopting partial work",
			)
			if err != nil {
				return err
			}
		}

		return finalizeRequestedBlockRecords(
			ctx,
			tx,
			task,
			activeRun,
			request,
			exitCode,
		)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return replacement, detail, err
}

// RegisterRecoveryCheckpointAndRecoverRunBlocked is the tokenless Supervisor
// path for a first attempt whose process is proven dead. It preserves the
// stopped run's partial work and blocks the task atomically.
func (s *Store) RegisterRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	runID string,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New(
			"run ID is required",
		)
	}
	input, err := normalizeRecoveryCheckpointInput(rawCheckpoint)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	blocked, err := normalizeRecoverBlockedRunInput(rawBlocked)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}

	var checkpoint model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
			return err
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		taskID = task.ID

		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New(
					"blocked run already registered a different recovery checkpoint",
				)
			}
			checkpoint = *existing
			if run.Status != model.RunStatusRunning {
				if checkpoint.State == model.RecoveryCheckpointPending &&
					blockedRecoveryAlreadyApplied(run, task, blocked) {
					return nil
				}
				return fmt.Errorf(
					"cannot recover terminal run with checkpoint as blocked: %s",
					run.Status,
				)
			}
		}

		if run.Status != model.RunStatusRunning ||
			task.Status != model.TaskStatusRunning ||
			task.CurrentRunID == nil ||
			*task.CurrentRunID != run.ID {
			return errors.New("run no longer owns this task")
		}
		if existing == nil {
			checkpoint, err = insertRecoveryCheckpoint(ctx, tx, task, run, input)
			if err != nil {
				return err
			}
		}
		return recoverRunBlockedRecords(ctx, tx, run, task, blocked)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return checkpoint, detail, err
}

// SupersedeRecoveryCheckpointAndRecoverRunBlocked is the tokenless Supervisor
// counterpart for a process already proven dead. It atomically replaces the
// adopted checkpoint with cumulative work and applies RecoverRunBlocked's
// terminal state. The replacement never exposes the worker reservation token.
func (s *Store) SupersedeRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	runID string,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New(
			"run ID is required",
		)
	}
	input, err := normalizeRecoveryCheckpointInput(rawCheckpoint)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	blocked, err := normalizeRecoverBlockedRunInput(rawBlocked)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}

	var replacement model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
			return err
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		taskID = task.ID

		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New(
					"recovery run already registered a different cumulative checkpoint",
				)
			}
			if _, err := requireRecoveryReplacementProvenance(
				ctx,
				tx,
				*existing,
				run.ID,
			); err != nil {
				return err
			}
			replacement = *existing
			if run.Status != model.RunStatusRunning {
				if blockedRecoveryAlreadyApplied(run, task, blocked) {
					return nil
				}
				return fmt.Errorf(
					"cannot recover terminal run with cumulative checkpoint as blocked: %s",
					run.Status,
				)
			}
		}

		if run.Status != model.RunStatusRunning ||
			task.Status != model.TaskStatusRunning ||
			task.CurrentRunID == nil ||
			*task.CurrentRunID != run.ID {
			return errors.New("run no longer owns this task")
		}
		if existing == nil {
			checkpoint, err := getRecoveryCheckpointByReservedRun(ctx, tx, run.ID)
			if err != nil {
				return err
			}
			if checkpoint == nil {
				return errors.New(
					"recovery run has no adopted checkpoint to preserve",
				)
			}
			replacement, err = supersedeAdoptedRecoveryCheckpoint(
				ctx,
				tx,
				task,
				run,
				*checkpoint,
				input,
				"supervisor blocked recovery run after adopting partial work",
			)
			if err != nil {
				return err
			}
		}
		return recoverRunBlockedRecords(ctx, tx, run, task, blocked)
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return replacement, detail, err
}

// RegisterRecoveryCheckpointAndRecoverAbandonedRun is the supervisor path for
// a process that is known to have exited. It does not require a worker claim
// token, but still requires the run to be the task's active owner. A normal
// run registers its first checkpoint; a recovery run atomically replaces the
// checkpoint it adopted. Repeating an already-committed call is idempotent.
func (s *Store) RegisterRecoveryCheckpointAndRecoverAbandonedRun(
	ctx context.Context,
	runID string,
	raw RegisterRecoveryCheckpointInput,
	outcome model.RunStatus,
	runError string,
	countFailure bool,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New("run ID is required")
	}
	input, err := normalizeRecoveryCheckpointInput(raw)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	if outcome == "" {
		outcome = model.RunStatusReclaimed
	}
	if !validRecoveryFailureOutcome(outcome) {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, fmt.Errorf(
			"invalid recovery checkpoint abandonment outcome: %s",
			outcome,
		)
	}
	runError = strings.TrimSpace(runError)
	if runError == "" {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New(
			"recovery checkpoint abandonment requires an error",
		)
	}

	var checkpoint model.RecoveryCheckpoint
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := requireRecoveryCheckpointRunRef(input, run.ID); err != nil {
			return err
		}
		taskID = run.TaskID
		existing, err := getRecoveryCheckpointBySourceRun(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if !sameRecoverySnapshot(*existing, input) {
				return errors.New(
					"abandoned run already registered a different recovery checkpoint",
				)
			}
			checkpoint = *existing
			if run.Status != model.RunStatusRunning {
				return nil
			}
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		if run.Status != model.RunStatusRunning ||
			task.Status != model.TaskStatusRunning ||
			task.CurrentRunID == nil ||
			*task.CurrentRunID != run.ID {
			return errors.New("run no longer owns this task")
		}
		if existing == nil {
			reserved, err := getRecoveryCheckpointByReservedRun(ctx, tx, run.ID)
			if err != nil {
				return err
			}
			switch {
			case reserved == nil:
				checkpoint, err = insertRecoveryCheckpoint(ctx, tx, task, run, input)
			case reserved.State == model.RecoveryCheckpointAdopted:
				checkpoint, err = supersedeAdoptedRecoveryCheckpoint(
					ctx,
					tx,
					task,
					run,
					*reserved,
					input,
					"abandoned recovery run ended after adopting partial work",
				)
			default:
				return errors.New(
					"abandoned recovery run did not adopt its reserved checkpoint; release the reservation without replacing it",
				)
			}
			if err != nil {
				return err
			}
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, FailRunOptions{
			Outcome:      outcome,
			CountFailure: &countFailure,
		})
	})
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return checkpoint, detail, err
}

// consumeRecoveryCheckpointForSuccessfulCompletion is deliberately
// transaction-only. FinalizeRunTerminal calls it after validating a successful
// completion and before committing the task/run terminal transition. There is
// no public standalone consume method, so an adopted checkpoint cannot be lost
// if final completion later rolls back.
func consumeRecoveryCheckpointForSuccessfulCompletion(
	ctx context.Context,
	q querier,
	taskID string,
	runID string,
	timestamp string,
) error {
	checkpoint, err := getRecoveryCheckpointByReservedRun(ctx, q, runID)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		return nil
	}
	if checkpoint.TaskID != taskID {
		return errors.New("recovery checkpoint reservation belongs to a different task")
	}
	if checkpoint.State != model.RecoveryCheckpointAdopted {
		return fmt.Errorf(
			"successful recovery completion requires an adopted checkpoint: %s",
			checkpoint.State,
		)
	}
	result, err := q.ExecContext(ctx, `UPDATE recovery_checkpoints
		SET state = 'consumed', consumed_at = ?, updated_at = ?
		WHERE id = ? AND task_id = ? AND reserved_run_id = ? AND state = 'adopted'`,
		timestamp, timestamp, checkpoint.ID, taskID, runID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("recovery checkpoint changed before successful completion")
	}
	return appendEvent(ctx, q, taskID, "recovery_checkpoint_consumed", map[string]any{
		"checkpointId":            checkpoint.ID,
		"sourceRunId":             checkpoint.SourceRunID,
		"adoptedOutputBaseCommit": checkpoint.AdoptedOutputBaseCommit,
		"adoptedHeadCommit":       checkpoint.AdoptedHeadCommit,
	}, &runID)
}
