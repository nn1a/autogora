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
	AutoDecomposeBackoffBase  = 5 * time.Second
	AutoDecomposeBackoffLimit = 5 * time.Minute
	AutoDecomposeMaxAttempts  = 5

	minAutoDecomposeClaimTTL = time.Second
	maxAutoDecomposeClaimTTL = 15 * time.Minute
)

var (
	ErrAutoDecomposeClaimNotOwner = errors.New("auto-decompose claim is owned by another caller")
	ErrAutoDecomposeClaimExpired  = errors.New("auto-decompose claim has expired")
	ErrAutoDecomposeTaskChanged   = errors.New("auto-decompose task changed while planning")
)

type AutoDecomposeEligibility string

const (
	AutoDecomposeClaimed     AutoDecomposeEligibility = "claimed"
	AutoDecomposeBackoff     AutoDecomposeEligibility = "backoff"
	AutoDecomposeBusy        AutoDecomposeEligibility = "busy"
	AutoDecomposeExhausted   AutoDecomposeEligibility = "exhausted"
	AutoDecomposeNotTriage   AutoDecomposeEligibility = "not_triage"
	AutoDecomposeInvalidated AutoDecomposeEligibility = "invalidated"
)

const autoDecomposeStateColumns = `
	task_id, task_updated_at, attempts, max_attempts, next_attempt_at,
	claim_token, claim_expires_at, last_error, updated_at
`

// AutoDecomposeState is the durable budget and lease for one automatically
// planned Triage task. Interactive Specify and Decompose calls do not consult
// or mutate this scheduler state.
type AutoDecomposeState struct {
	TaskID         string
	TaskUpdatedAt  string
	Attempts       int
	MaxAttempts    int
	NextAttemptAt  *string
	ClaimToken     *string
	ClaimExpiresAt *string
	LastError      *string
	UpdatedAt      string
}

type AutoDecomposeClaim struct {
	TaskID        string
	TaskUpdatedAt string
	Token         string
	Attempt       int
	MaxAttempts   int
	ExpiresAt     string
}

type AutoDecomposeDecision struct {
	Eligibility AutoDecomposeEligibility
	Attempts    int
	MaxAttempts int
	RetryAt     *string
	Claim       *AutoDecomposeClaim
}

type AutoDecomposeFailureResult struct {
	Eligibility AutoDecomposeEligibility
	Attempt     int
	MaxAttempts int
	RetryAt     *string
}

func scanAutoDecomposeState(row scanner) (AutoDecomposeState, error) {
	var value AutoDecomposeState
	var nextAttemptAt, claimToken, claimExpiresAt, lastError sql.NullString
	err := row.Scan(
		&value.TaskID, &value.TaskUpdatedAt, &value.Attempts, &value.MaxAttempts,
		&nextAttemptAt, &claimToken, &claimExpiresAt, &lastError, &value.UpdatedAt,
	)
	if nextAttemptAt.Valid {
		value.NextAttemptAt = &nextAttemptAt.String
	}
	if claimToken.Valid {
		value.ClaimToken = &claimToken.String
	}
	if claimExpiresAt.Valid {
		value.ClaimExpiresAt = &claimExpiresAt.String
	}
	if lastError.Valid {
		value.LastError = &lastError.String
	}
	return value, err
}

func autoDecomposeTimestamp(current time.Time) (string, error) {
	current = current.UTC()
	if current.IsZero() || current.Year() < 0 || current.Year() > 9999 {
		return "", errors.New("auto-decompose current time must fit RFC3339")
	}
	return current.Format(time.RFC3339Nano), nil
}

func parseAutoDecomposeTimestamp(field, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse auto-decompose %s: %w", field, err)
	}
	return parsed, nil
}

func AutoDecomposeRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := AutoDecomposeBackoffBase
	for step := 1; step < attempt && delay < AutoDecomposeBackoffLimit; step++ {
		if delay > AutoDecomposeBackoffLimit/2 {
			return AutoDecomposeBackoffLimit
		}
		delay *= 2
	}
	return min(delay, AutoDecomposeBackoffLimit)
}

func normalizeAutoDecomposeClaim(
	taskID string,
	maxAttempts int,
	claimTTL time.Duration,
	current time.Time,
) (string, string, string, error) {
	taskID = strings.TrimSpace(taskID)
	switch {
	case taskID == "":
		return "", "", "", errors.New("auto-decompose claim requires a task ID")
	case maxAttempts < 1 || maxAttempts > 32:
		return "", "", "", errors.New("auto-decompose max attempts must be between 1 and 32")
	case claimTTL < minAutoDecomposeClaimTTL || claimTTL > maxAutoDecomposeClaimTTL:
		return "", "", "", fmt.Errorf(
			"auto-decompose claim TTL must be between %s and %s",
			minAutoDecomposeClaimTTL, maxAutoDecomposeClaimTTL,
		)
	}
	timestamp, err := autoDecomposeTimestamp(current)
	if err != nil {
		return "", "", "", err
	}
	expiresAt, err := autoDecomposeTimestamp(current.Add(claimTTL))
	if err != nil {
		return "", "", "", err
	}
	return taskID, timestamp, expiresAt, nil
}

func autoDecomposeDecision(
	eligibility AutoDecomposeEligibility,
	state AutoDecomposeState,
	retryAt *string,
) AutoDecomposeDecision {
	return AutoDecomposeDecision{
		Eligibility: eligibility,
		Attempts:    state.Attempts,
		MaxAttempts: state.MaxAttempts,
		RetryAt:     retryAt,
	}
}

// ClaimAutoDecompose atomically reserves one logical planner invocation. The
// attempt is charged before the external process starts, so a crash cannot
// create an unbounded free retry loop. A newer task version starts a fresh
// budget because it represents explicit user recovery.
func (s *Store) ClaimAutoDecompose(
	ctx context.Context,
	taskID string,
	maxAttempts int,
	claimTTL time.Duration,
	current time.Time,
) (decision AutoDecomposeDecision, err error) {
	taskID, timestamp, expiresAt, err := normalizeAutoDecomposeClaim(taskID, maxAttempts, claimTTL, current)
	if err != nil {
		return AutoDecomposeDecision{}, err
	}
	current = current.UTC()

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Status != model.TaskStatusTriage {
			decision = AutoDecomposeDecision{
				Eligibility: AutoDecomposeNotTriage,
				MaxAttempts: maxAttempts,
			}
			return nil
		}
		coordinationRetryAt, blocked, err := autoDecomposeClaimBlockedByGraphStall(
			ctx, tx, task.Board, task.ID, task.UpdatedAt, current,
		)
		if err != nil {
			return err
		}
		if blocked {
			decision = AutoDecomposeDecision{
				Eligibility: AutoDecomposeBusy,
				MaxAttempts: maxAttempts,
				RetryAt:     coordinationRetryAt,
			}
			return nil
		}

		state, stateErr := scanAutoDecomposeState(tx.QueryRowContext(ctx,
			"SELECT "+autoDecomposeStateColumns+" FROM auto_decompose_state WHERE task_id = ?",
			taskID,
		))
		switch {
		case errors.Is(stateErr, sql.ErrNoRows):
			state = AutoDecomposeState{TaskID: taskID, TaskUpdatedAt: task.UpdatedAt, MaxAttempts: maxAttempts}
		case stateErr != nil:
			return fmt.Errorf("read auto-decompose state for %s: %w", taskID, stateErr)
		case state.TaskUpdatedAt != task.UpdatedAt:
			if _, err := tx.ExecContext(ctx, "DELETE FROM auto_decompose_state WHERE task_id = ?", taskID); err != nil {
				return fmt.Errorf("reset auto-decompose state for %s: %w", taskID, err)
			}
			if err := appendEvent(ctx, tx, taskID, "auto_decompose_retry_reset", map[string]any{
				"previousAttempts": state.Attempts, "reason": "task_updated",
			}, nil); err != nil {
				return err
			}
			state = AutoDecomposeState{TaskID: taskID, TaskUpdatedAt: task.UpdatedAt, MaxAttempts: maxAttempts}
		}
		state.MaxAttempts = maxAttempts

		claimExpired := false
		if state.ClaimToken != nil && state.ClaimExpiresAt != nil {
			expires, err := parseAutoDecomposeTimestamp("claim expiry", *state.ClaimExpiresAt)
			if err != nil {
				return err
			}
			if expires.After(current) {
				decision = autoDecomposeDecision(AutoDecomposeBusy, state, state.ClaimExpiresAt)
				return nil
			}
			expiredAt := *state.ClaimExpiresAt
			expiredError := "planner claim expired before completion"
			if _, err := tx.ExecContext(ctx, `
				UPDATE auto_decompose_state
				SET max_attempts = ?, claim_token = NULL, claim_expires_at = NULL,
				    last_error = ?, updated_at = ?
				WHERE task_id = ?
			`, maxAttempts, expiredError, timestamp, taskID); err != nil {
				return fmt.Errorf("expire auto-decompose claim for %s: %w", taskID, err)
			}
			if err := appendEvent(ctx, tx, taskID, "auto_decompose_claim_expired", map[string]any{
				"attempt": state.Attempts, "maxAttempts": maxAttempts, "claimExpiredAt": expiredAt,
			}, nil); err != nil {
				return err
			}
			state.ClaimToken = nil
			state.ClaimExpiresAt = nil
			state.LastError = &expiredError
			state.UpdatedAt = timestamp
			claimExpired = true
		}
		if state.Attempts >= maxAttempts {
			if claimExpired {
				if err := appendEvent(ctx, tx, taskID, "auto_decompose_exhausted", map[string]any{
					"attempts": state.Attempts, "maxAttempts": maxAttempts, "reason": "claim_expired",
				}, nil); err != nil {
					return err
				}
			}
			decision = autoDecomposeDecision(AutoDecomposeExhausted, state, nil)
			return nil
		}
		if state.NextAttemptAt != nil {
			retryAt, err := parseAutoDecomposeTimestamp("retry time", *state.NextAttemptAt)
			if err != nil {
				return err
			}
			if retryAt.After(current) {
				decision = autoDecomposeDecision(AutoDecomposeBackoff, state, state.NextAttemptAt)
				return nil
			}
		}

		attempt := state.Attempts + 1
		token := newID("plan")
		_, err = tx.ExecContext(ctx, `
			INSERT INTO auto_decompose_state(
				task_id, task_updated_at, attempts, max_attempts, next_attempt_at,
				claim_token, claim_expires_at, last_error, updated_at
			) VALUES (?, ?, ?, ?, NULL, ?, ?, NULL, ?)
			ON CONFLICT(task_id) DO UPDATE SET
				task_updated_at = excluded.task_updated_at,
				attempts = excluded.attempts,
				max_attempts = excluded.max_attempts,
				next_attempt_at = NULL,
				claim_token = excluded.claim_token,
				claim_expires_at = excluded.claim_expires_at,
				last_error = NULL,
				updated_at = excluded.updated_at
		`, taskID, task.UpdatedAt, attempt, maxAttempts, token, expiresAt, timestamp)
		if err != nil {
			return fmt.Errorf("claim auto-decompose task %s: %w", taskID, err)
		}
		if err := appendEvent(ctx, tx, taskID, "auto_decompose_claimed", map[string]any{
			"attempt": attempt, "maxAttempts": maxAttempts, "claimExpiresAt": expiresAt,
		}, nil); err != nil {
			return err
		}
		claim := AutoDecomposeClaim{
			TaskID: taskID, TaskUpdatedAt: task.UpdatedAt, Token: token,
			Attempt: attempt, MaxAttempts: maxAttempts, ExpiresAt: expiresAt,
		}
		decision = AutoDecomposeDecision{
			Eligibility: AutoDecomposeClaimed,
			Attempts:    attempt, MaxAttempts: maxAttempts, Claim: &claim,
		}
		return nil
	})
	return decision, err
}

func (s *Store) GetAutoDecomposeState(ctx context.Context, taskID string) (*AutoDecomposeState, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("auto-decompose state requires a task ID")
	}
	value, err := scanAutoDecomposeState(s.db.QueryRowContext(ctx,
		"SELECT "+autoDecomposeStateColumns+" FROM auto_decompose_state WHERE task_id = ?",
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

// RenewAutoDecomposeClaim atomically proves that the caller still owns a live
// claim for the same Triage task version, then extends that lease without
// charging another attempt. An expired claim cannot be revived because another
// scheduler may already be reclaiming it.
func (s *Store) RenewAutoDecomposeClaim(
	ctx context.Context,
	claim AutoDecomposeClaim,
	claimTTL time.Duration,
	current time.Time,
) (renewed AutoDecomposeClaim, err error) {
	claim.TaskID = strings.TrimSpace(claim.TaskID)
	claim.TaskUpdatedAt = strings.TrimSpace(claim.TaskUpdatedAt)
	claim.Token = strings.TrimSpace(claim.Token)
	switch {
	case claim.TaskID == "" || claim.Token == "":
		return AutoDecomposeClaim{}, errors.New("auto-decompose claim renewal requires a claim")
	case claim.TaskUpdatedAt == "":
		return AutoDecomposeClaim{}, errors.New("auto-decompose claim renewal requires a task version")
	case claimTTL < minAutoDecomposeClaimTTL || claimTTL > maxAutoDecomposeClaimTTL:
		return AutoDecomposeClaim{}, fmt.Errorf(
			"auto-decompose claim renewal TTL must be between %s and %s",
			minAutoDecomposeClaimTTL, maxAutoDecomposeClaimTTL,
		)
	}
	timestamp, err := autoDecomposeTimestamp(current)
	if err != nil {
		return AutoDecomposeClaim{}, err
	}
	expiresAt, err := autoDecomposeTimestamp(current.UTC().Add(claimTTL))
	if err != nil {
		return AutoDecomposeClaim{}, err
	}
	current = current.UTC()

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		state, stateErr := scanAutoDecomposeState(tx.QueryRowContext(ctx,
			"SELECT "+autoDecomposeStateColumns+" FROM auto_decompose_state WHERE task_id = ?",
			claim.TaskID,
		))
		if errors.Is(stateErr, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, claim.TaskID)
		}
		if stateErr != nil {
			return fmt.Errorf("read auto-decompose claim for %s: %w", claim.TaskID, stateErr)
		}
		if state.ClaimToken == nil || *state.ClaimToken != claim.Token {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, claim.TaskID)
		}
		if state.TaskUpdatedAt != claim.TaskUpdatedAt {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeTaskChanged, claim.TaskID)
		}
		task, taskErr := requireTask(ctx, tx, claim.TaskID)
		if taskErr != nil {
			return taskErr
		}
		if task.Status != model.TaskStatusTriage || task.UpdatedAt != claim.TaskUpdatedAt {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeTaskChanged, claim.TaskID)
		}
		if state.ClaimExpiresAt == nil {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, claim.TaskID)
		}
		previousExpiry, parseErr := parseAutoDecomposeTimestamp("claim expiry", *state.ClaimExpiresAt)
		if parseErr != nil {
			return parseErr
		}
		if !previousExpiry.After(current) {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimExpired, claim.TaskID)
		}
		// A clock adjustment must never shorten a lease that is already longer.
		if previousExpiry.After(current.Add(claimTTL)) {
			expiresAt = previousExpiry.UTC().Format(time.RFC3339Nano)
		}
		update, updateErr := tx.ExecContext(ctx, `
			UPDATE auto_decompose_state
			SET claim_expires_at = ?, updated_at = ?
			WHERE task_id = ? AND task_updated_at = ?
				AND claim_token = ? AND claim_expires_at = ?
		`, expiresAt, timestamp, claim.TaskID, claim.TaskUpdatedAt,
			claim.Token, *state.ClaimExpiresAt)
		if updateErr != nil {
			return fmt.Errorf("renew auto-decompose claim for %s: %w", claim.TaskID, updateErr)
		}
		changed, rowsErr := update.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if changed != 1 {
			return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, claim.TaskID)
		}
		renewed = AutoDecomposeClaim{
			TaskID: claim.TaskID, TaskUpdatedAt: claim.TaskUpdatedAt,
			Token: claim.Token, Attempt: state.Attempts,
			MaxAttempts: state.MaxAttempts, ExpiresAt: expiresAt,
		}
		return nil
	})
	return renewed, err
}

// requireLiveAutoDecomposeClaim fences a result mutation inside the same
// transaction that will apply it. If another scheduler reclaimed the lease,
// its replacement token is visible before any task or graph state is changed.
func requireLiveAutoDecomposeClaim(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	claim AutoDecomposeClaim,
) error {
	if strings.TrimSpace(claim.TaskID) != task.ID ||
		strings.TrimSpace(claim.TaskUpdatedAt) == "" ||
		strings.TrimSpace(claim.Token) == "" {
		return errors.New("auto-decompose result application requires its original claim")
	}
	if task.UpdatedAt != claim.TaskUpdatedAt {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeTaskChanged, task.ID)
	}
	state, err := scanAutoDecomposeState(tx.QueryRowContext(ctx,
		"SELECT "+autoDecomposeStateColumns+" FROM auto_decompose_state WHERE task_id = ?",
		task.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, task.ID)
	}
	if err != nil {
		return fmt.Errorf("read auto-decompose result fence for %s: %w", task.ID, err)
	}
	if state.TaskUpdatedAt != claim.TaskUpdatedAt {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeTaskChanged, task.ID)
	}
	if state.ClaimToken == nil || *state.ClaimToken != claim.Token ||
		state.ClaimExpiresAt == nil {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, task.ID)
	}
	expiresAt, err := parseAutoDecomposeTimestamp("claim expiry", *state.ClaimExpiresAt)
	if err != nil {
		return err
	}
	if !expiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimExpired, task.ID)
	}
	return nil
}

func consumeAutoDecomposeClaim(
	ctx context.Context,
	tx *sql.Tx,
	claim AutoDecomposeClaim,
) error {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM auto_decompose_state
		WHERE task_id = ? AND task_updated_at = ? AND claim_token = ?
	`, claim.TaskID, claim.TaskUpdatedAt, claim.Token)
	if err != nil {
		return fmt.Errorf("consume auto-decompose claim for %s: %w", claim.TaskID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrAutoDecomposeClaimNotOwner, claim.TaskID)
	}
	return nil
}

func normalizeAutoDecomposeFailure(claim AutoDecomposeClaim, failure string, current time.Time) (string, string, error) {
	if strings.TrimSpace(claim.TaskID) == "" || strings.TrimSpace(claim.Token) == "" {
		return "", "", errors.New("auto-decompose failure requires a claim")
	}
	timestamp, err := autoDecomposeTimestamp(current)
	if err != nil {
		return "", "", err
	}
	failure = strings.TrimSpace(strings.ToValidUTF8(failure, "\uFFFD"))
	if failure == "" {
		failure = "planner failed without an error"
	}
	if len(failure) > 2000 {
		failure = failure[:2000]
		for !utf8.ValidString(failure) {
			failure = failure[:len(failure)-1]
		}
	}
	return failure, timestamp, nil
}

// FailAutoDecompose releases a claim into persisted exponential backoff, or
// marks its invocation budget exhausted. A stale result never overwrites the
// state of a task that a user edited while Planner was running.
func (s *Store) FailAutoDecompose(
	ctx context.Context,
	claim AutoDecomposeClaim,
	failure string,
	current time.Time,
) (result AutoDecomposeFailureResult, err error) {
	failure, timestamp, err := normalizeAutoDecomposeFailure(claim, failure, current)
	if err != nil {
		return AutoDecomposeFailureResult{}, err
	}
	current = current.UTC()

	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		state, stateErr := scanAutoDecomposeState(tx.QueryRowContext(ctx,
			"SELECT "+autoDecomposeStateColumns+" FROM auto_decompose_state WHERE task_id = ?",
			claim.TaskID,
		))
		if errors.Is(stateErr, sql.ErrNoRows) ||
			stateErr == nil && (state.ClaimToken == nil || *state.ClaimToken != claim.Token) {
			result = AutoDecomposeFailureResult{Eligibility: AutoDecomposeInvalidated}
			return nil
		}
		if stateErr != nil {
			return fmt.Errorf("read auto-decompose claim for %s: %w", claim.TaskID, stateErr)
		}
		task, err := requireTask(ctx, tx, claim.TaskID)
		if err != nil {
			return err
		}
		if task.Status != model.TaskStatusTriage || task.UpdatedAt != state.TaskUpdatedAt {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM auto_decompose_state WHERE task_id = ? AND claim_token = ?",
				claim.TaskID, claim.Token,
			); err != nil {
				return fmt.Errorf("invalidate stale auto-decompose claim for %s: %w", claim.TaskID, err)
			}
			result = AutoDecomposeFailureResult{
				Eligibility: AutoDecomposeInvalidated,
				Attempt:     state.Attempts, MaxAttempts: state.MaxAttempts,
			}
			return nil
		}

		eligibility := AutoDecomposeBackoff
		var retryAt *string
		if state.Attempts >= state.MaxAttempts {
			eligibility = AutoDecomposeExhausted
		} else {
			value, err := autoDecomposeTimestamp(current.Add(AutoDecomposeRetryDelay(state.Attempts)))
			if err != nil {
				return err
			}
			retryAt = &value
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE auto_decompose_state
			SET next_attempt_at = ?, claim_token = NULL, claim_expires_at = NULL,
			    last_error = ?, updated_at = ?
			WHERE task_id = ? AND claim_token = ?
		`, nullableString(retryAt), failure, timestamp, claim.TaskID, claim.Token)
		if err != nil {
			return fmt.Errorf("record auto-decompose failure for %s: %w", claim.TaskID, err)
		}
		payload := map[string]any{
			"error": failure, "attempt": state.Attempts,
			"maxAttempts": state.MaxAttempts, "exhausted": eligibility == AutoDecomposeExhausted,
		}
		if retryAt != nil {
			payload["retryAt"] = *retryAt
		}
		if err := appendEvent(ctx, tx, claim.TaskID, "auto_decompose_failed", payload, nil); err != nil {
			return err
		}
		if eligibility == AutoDecomposeExhausted {
			if err := appendEvent(ctx, tx, claim.TaskID, "auto_decompose_exhausted", map[string]any{
				"attempts": state.Attempts, "maxAttempts": state.MaxAttempts,
			}, nil); err != nil {
				return err
			}
		}
		result = AutoDecomposeFailureResult{
			Eligibility: eligibility, Attempt: state.Attempts,
			MaxAttempts: state.MaxAttempts, RetryAt: retryAt,
		}
		return nil
	})
	return result, err
}
