package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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
	return getActiveRecoveryCheckpoint(ctx, s.db, taskID)
}

func getActiveRecoveryCheckpoint(
	ctx context.Context,
	q querier,
	taskID string,
) (*model.RecoveryCheckpoint, error) {
	value, err := scanRecoveryCheckpoint(q.QueryRowContext(
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

func requireNoActiveRecoveryCheckpointForCompletion(
	ctx context.Context,
	q querier,
	taskID string,
) error {
	checkpoint, err := getActiveRecoveryCheckpoint(ctx, q, taskID)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		return nil
	}
	return fmt.Errorf(
		"task cannot be completed with active recovery checkpoint %s in state %s",
		checkpoint.ID,
		checkpoint.State,
	)
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

func terminalFailureRetryMatches(
	ctx context.Context,
	q querier,
	run model.Run,
	task model.Task,
	runError string,
	options FailRunOptions,
) (bool, error) {
	outcome := options.Outcome
	if outcome == "" {
		outcome = model.RunStatusFailed
	}
	countFailure := true
	if options.CountFailure != nil {
		countFailure = *options.CountFailure
	}
	limit := options.FailureLimit
	if limit <= 0 {
		limit = task.MaxRetries
	}
	limit = max(1, limit)
	if run.Status != outcome ||
		run.Error == nil ||
		*run.Error != runError ||
		run.EndedAt == nil ||
		task.CurrentRunID != nil {
		return false, nil
	}

	rows, err := q.QueryContext(
		ctx,
		`SELECT kind, payload_json, created_at
		 FROM task_events
		 WHERE run_id = ?
		 ORDER BY id DESC`,
		run.ID,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, createdAt string
		var payloadBytes []byte
		if err := rows.Scan(&kind, &payloadBytes, &createdAt); err != nil {
			return false, err
		}
		if kind != string(outcome) &&
			!(outcome == model.RunStatusFailed &&
				(kind == "requeued" || kind == "gave_up")) {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			continue
		}
		var eventError string
		var eventOutcome model.RunStatus
		var eventCountFailure bool
		var failures, effectiveLimit int
		if json.Unmarshal(payload["error"], &eventError) != nil ||
			json.Unmarshal(payload["outcome"], &eventOutcome) != nil ||
			json.Unmarshal(payload["countFailure"], &eventCountFailure) != nil ||
			json.Unmarshal(payload["failures"], &failures) != nil ||
			json.Unmarshal(payload["effectiveLimit"], &effectiveLimit) != nil ||
			eventError != runError ||
			eventOutcome != outcome ||
			eventCountFailure != countFailure ||
			effectiveLimit != limit ||
			task.FailureCount != failures {
			continue
		}
		exhausted := countFailure && failures >= effectiveLimit
		expectedKind := string(outcome)
		if outcome == model.RunStatusFailed {
			if exhausted {
				expectedKind = "gave_up"
			} else {
				expectedKind = "requeued"
			}
		}
		if kind != expectedKind {
			continue
		}
		var scheduledAt *string
		rawScheduledAt, exists := payload["scheduledAt"]
		if !exists || json.Unmarshal(rawScheduledAt, &scheduledAt) != nil {
			continue
		}
		if !sameRecoveryOptionalString(task.ScheduledAt, scheduledAt) {
			continue
		}
		switch {
		case exhausted:
			if task.Status != model.TaskStatusBlocked ||
				task.BlockReason == nil ||
				*task.BlockReason != runError ||
				scheduledAt != nil {
				continue
			}
		case options.CooldownSeconds > 0:
			if task.Status != model.TaskStatusScheduled ||
				scheduledAt == nil ||
				!scheduledDelayMatches(
					createdAt,
					*scheduledAt,
					options.CooldownSeconds,
				) {
				continue
			}
		default:
			if scheduledAt != nil {
				continue
			}
			open, err := hasOpenParents(ctx, q, task.ID)
			if err != nil {
				return false, err
			}
			expectedStatus := model.TaskStatusReady
			if open || task.Assignee == nil ||
				task.Runtime == model.RuntimeManual {
				expectedStatus = model.TaskStatusTodo
			}
			if task.Status != expectedStatus {
				continue
			}
		}
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func scheduledDelayMatches(createdAt, scheduledAt string, seconds int) bool {
	created, createdErr := time.Parse(time.RFC3339Nano, createdAt)
	scheduled, scheduledErr := time.Parse(time.RFC3339Nano, scheduledAt)
	if createdErr != nil || scheduledErr != nil {
		return false
	}
	expected := time.Duration(seconds) * time.Second
	difference := scheduled.Sub(created) - expected
	if difference < 0 {
		difference = -difference
	}
	return difference <= time.Second
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
				task, err := requireTask(ctx, tx, scopedRun.TaskID)
				if err != nil {
					return err
				}
				matches, err := terminalFailureRetryMatches(
					ctx,
					tx,
					scopedRun,
					task,
					runError,
					options,
				)
				if err != nil {
					return err
				}
				if !matches {
					return errors.New(
						"recovery checkpoint failure retry does not match terminal effect",
					)
				}
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
		var unresolvedIntegration bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM integration_resolution_attempts
			WHERE run_id = ? AND resolved_at IS NULL
		)`, run.ID).Scan(&unresolvedIntegration); err != nil {
			return err
		}
		if unresolvedIntegration {
			return errors.New(
				"integration resolution recovery must use atomic resolution confirmation",
			)
		}
		checkpoint, err = confirmRecoveryCheckpointAdoption(
			ctx,
			tx,
			task,
			run,
			checkpointID,
			reservationToken,
			adoptedOutputBaseCommit,
			adoptedHeadCommit,
			now(),
		)
		return err
	})
	return checkpoint, err
}

func confirmRecoveryCheckpointAdoption(
	ctx context.Context,
	q querier,
	task model.Task,
	run model.Run,
	checkpointID string,
	reservationToken string,
	adoptedOutputBaseCommit string,
	adoptedHeadCommit string,
	timestamp string,
) (model.RecoveryCheckpoint, error) {
	checkpoint, err := requireOwnedRecoveryCheckpoint(
		ctx,
		q,
		task,
		run,
		checkpointID,
		reservationToken,
	)
	if err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	if checkpoint.State == model.RecoveryCheckpointAdopted {
		if checkpoint.AdoptedOutputBaseCommit != nil &&
			*checkpoint.AdoptedOutputBaseCommit == adoptedOutputBaseCommit &&
			checkpoint.AdoptedHeadCommit != nil &&
			*checkpoint.AdoptedHeadCommit == adoptedHeadCommit {
			return checkpoint, nil
		}
		return model.RecoveryCheckpoint{}, errors.New(
			"recovery checkpoint was already adopted at a different base or head",
		)
	}
	if checkpoint.State != model.RecoveryCheckpointReserved {
		return model.RecoveryCheckpoint{}, fmt.Errorf(
			"recovery checkpoint cannot be adopted from state %s",
			checkpoint.State,
		)
	}
	result, err := q.ExecContext(ctx, `UPDATE recovery_checkpoints
		SET state = 'adopted', adopted_output_base_commit = ?,
			adopted_head_commit = ?, adopted_at = ?, updated_at = ?
		WHERE id = ? AND state = 'reserved' AND reserved_run_id = ? AND reservation_token = ?`,
		adoptedOutputBaseCommit, adoptedHeadCommit, timestamp, timestamp,
		checkpoint.ID, run.ID, reservationToken,
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
			"recovery checkpoint changed before adoption",
		)
	}
	if err := appendEvent(ctx, q, task.ID, "recovery_checkpoint_adopted", map[string]any{
		"checkpointId":            checkpoint.ID,
		"sourceHeadCommit":        checkpoint.HeadCommit,
		"adoptedOutputBaseCommit": adoptedOutputBaseCommit,
		"adoptedHeadCommit":       adoptedHeadCommit,
	}, &run.ID); err != nil {
		return model.RecoveryCheckpoint{}, err
	}
	return getRecoveryCheckpoint(ctx, q, checkpoint.ID)
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
func (s *Store) registerRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	observation *RunRecoveryObservation,
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

		activeRunLoader := requireActiveRun
		if observation != nil {
			activeRunLoader = requireActiveRunForRecovery
		}
		task, activeRun, err := activeRunLoader(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireOptionalSupervisorRunRecoveryFence(ctx, tx, activeRun, observation); err != nil {
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

func (s *Store) RegisterRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.registerRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		nil,
		raw,
		exitCode,
	)
}

func (s *Store) stoppedManagedRunScope(
	ctx context.Context,
	runID string,
) (RunScope, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunScope{}, errors.New("run ID is required")
	}
	var claimToken string
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT runs.claim_token
		 FROM task_runs runs
		 JOIN managed_runs managed ON managed.run_id = runs.id
		 WHERE runs.id = ?`,
		runID,
	).Scan(&claimToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunScope{}, errors.New(
				"stopped recovery block requires a managed run",
			)
		}
		return RunScope{}, err
	}
	return RunScope{RunID: runID, ClaimToken: claimToken}, nil
}

// RegisterRecoveryCheckpointAndFinalizeObservedStoppedBlock is the
// compare-and-swap protected Supervisor counterpart.
func (s *Store) RegisterRecoveryCheckpointAndFinalizeObservedStoppedBlock(
	ctx context.Context,
	observation RunRecoveryObservation,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	scope, err := s.stoppedManagedRunScope(ctx, observation.RunID)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	return s.registerRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		&observation,
		raw,
		exitCode,
	)
}

// SupersedeRecoveryCheckpointAndFinalizeBlock preserves cumulative work from
// an adopted checkpoint and finalizes an already-requested worker block in one
// transaction. The returned replacement is pending and contains no reservation
// token. Repeating the same call after a committed-but-lost response is safe.
func (s *Store) supersedeRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	observation *RunRecoveryObservation,
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

		activeRunLoader := requireActiveRun
		if observation != nil {
			activeRunLoader = requireActiveRunForRecovery
		}
		task, activeRun, err := activeRunLoader(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := requireOptionalSupervisorRunRecoveryFence(ctx, tx, activeRun, observation); err != nil {
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

func (s *Store) SupersedeRecoveryCheckpointAndFinalizeBlock(
	ctx context.Context,
	scope RunScope,
	checkpointID string,
	reservationToken string,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.supersedeRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		nil,
		checkpointID,
		reservationToken,
		raw,
		exitCode,
	)
}

// SupersedeRecoveryCheckpointAndFinalizeObservedStoppedBlock is the
// compare-and-swap protected Supervisor counterpart for a stopped recovery
// run.
func (s *Store) SupersedeRecoveryCheckpointAndFinalizeObservedStoppedBlock(
	ctx context.Context,
	observation RunRecoveryObservation,
	raw RegisterRecoveryCheckpointInput,
	exitCode int,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	scope, err := s.stoppedManagedRunScope(ctx, observation.RunID)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	checkpoint, err := getRecoveryCheckpointByReservedRun(ctx, s.db, scope.RunID)
	if err != nil {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, err
	}
	if checkpoint == nil ||
		(checkpoint.State != model.RecoveryCheckpointAdopted &&
			checkpoint.State != model.RecoveryCheckpointSuperseded) {
		return model.RecoveryCheckpoint{}, model.TaskDetail{}, errors.New(
			"stopped recovery block requires an adopted checkpoint or its committed supersession",
		)
	}
	return s.supersedeRecoveryCheckpointAndFinalizeBlock(
		ctx,
		scope,
		&observation,
		checkpoint.ID,
		checkpoint.ReservationToken,
		raw,
		exitCode,
	)
}

// registerRecoveryCheckpointAndRecoverRunBlocked serves either an exact active
// host claim or an observed Supervisor recovery owner. It never accepts an
// unauthenticated run ID.
func (s *Store) registerRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	runID string,
	scope *RunScope,
	observation *RunRecoveryObservation,
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
		var run model.Run
		var err error
		if scope != nil {
			run, err = requireRunScope(ctx, tx, *scope)
		} else {
			run, err = getRun(ctx, tx, runID)
		}
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

		if scope != nil {
			task, run, err = requireActiveRun(ctx, tx, *scope)
			if err != nil {
				return err
			}
		} else {
			if run.Status != model.RunStatusRunning ||
				task.Status != model.TaskStatusRunning ||
				task.CurrentRunID == nil ||
				*task.CurrentRunID != run.ID {
				return errors.New("run no longer owns this task")
			}
			if err := requireSupervisorRunRecoveryFence(
				ctx,
				tx,
				run,
				observation,
			); err != nil {
				return err
			}
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

func (s *Store) RegisterRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	scope RunScope,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.registerRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope.RunID,
		&scope,
		nil,
		rawCheckpoint,
		rawBlocked,
	)
}

func (s *Store) RegisterRecoveryCheckpointAndRecoverObservedRunBlocked(
	ctx context.Context,
	observation RunRecoveryObservation,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.registerRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		observation.RunID,
		nil,
		&observation,
		rawCheckpoint,
		rawBlocked,
	)
}

// supersedeRecoveryCheckpointAndRecoverRunBlocked serves either an exact
// active host claim or an observed Supervisor recovery owner. The replacement
// never exposes the worker reservation token.
func (s *Store) supersedeRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	runID string,
	scope *RunScope,
	observation *RunRecoveryObservation,
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
		var run model.Run
		var err error
		if scope != nil {
			run, err = requireRunScope(ctx, tx, *scope)
		} else {
			run, err = getRun(ctx, tx, runID)
		}
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

		if scope != nil {
			task, run, err = requireActiveRun(ctx, tx, *scope)
			if err != nil {
				return err
			}
		} else {
			if run.Status != model.RunStatusRunning ||
				task.Status != model.TaskStatusRunning ||
				task.CurrentRunID == nil ||
				*task.CurrentRunID != run.ID {
				return errors.New("run no longer owns this task")
			}
			if err := requireSupervisorRunRecoveryFence(
				ctx,
				tx,
				run,
				observation,
			); err != nil {
				return err
			}
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

func (s *Store) SupersedeRecoveryCheckpointAndRecoverRunBlocked(
	ctx context.Context,
	scope RunScope,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.supersedeRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		scope.RunID,
		&scope,
		nil,
		rawCheckpoint,
		rawBlocked,
	)
}

func (s *Store) SupersedeRecoveryCheckpointAndRecoverObservedRunBlocked(
	ctx context.Context,
	observation RunRecoveryObservation,
	rawCheckpoint RegisterRecoveryCheckpointInput,
	rawBlocked RecoverBlockedRunInput,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.supersedeRecoveryCheckpointAndRecoverRunBlocked(
		ctx,
		observation.RunID,
		nil,
		&observation,
		rawCheckpoint,
		rawBlocked,
	)
}

// registerRecoveryCheckpointAndRecoverAbandonedRun is the observed Supervisor
// path for a process whose quiescence has been proven. A normal run registers
// its first checkpoint; a recovery run atomically replaces the checkpoint it
// adopted. Repeating the same committed terminal effect is idempotent.
func (s *Store) registerRecoveryCheckpointAndRecoverAbandonedRun(
	ctx context.Context,
	runID string,
	observation RunRecoveryObservation,
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
				task, err := requireTask(ctx, tx, run.TaskID)
				if err != nil {
					return err
				}
				matches, err := terminalFailureRetryMatches(
					ctx,
					tx,
					run,
					task,
					runError,
					FailRunOptions{
						Outcome:      outcome,
						CountFailure: &countFailure,
					},
				)
				if err != nil {
					return err
				}
				if !matches {
					return errors.New(
						"abandoned recovery checkpoint retry does not match terminal effect",
					)
				}
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
		if err := requireSupervisorRunRecoveryFence(
			ctx,
			tx,
			run,
			&observation,
		); err != nil {
			return err
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

func (s *Store) RegisterRecoveryCheckpointAndRecoverObservedAbandonedRun(
	ctx context.Context,
	observation RunRecoveryObservation,
	raw RegisterRecoveryCheckpointInput,
	outcome model.RunStatus,
	runError string,
	countFailure bool,
) (model.RecoveryCheckpoint, model.TaskDetail, error) {
	return s.registerRecoveryCheckpointAndRecoverAbandonedRun(
		ctx,
		observation.RunID,
		observation,
		raw,
		outcome,
		runError,
		countFailure,
	)
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
	checkpoint, err := getActiveRecoveryCheckpoint(ctx, q, taskID)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		return nil
	}
	if checkpoint.TaskID != taskID ||
		checkpoint.ReservedRunID == nil ||
		*checkpoint.ReservedRunID != runID ||
		checkpoint.State != model.RecoveryCheckpointAdopted {
		return fmt.Errorf(
			"task cannot be completed with active recovery checkpoint %s in state %s",
			checkpoint.ID,
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
