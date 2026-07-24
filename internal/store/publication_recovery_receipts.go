package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrPublicationRecoveryReceiptNotFound = errors.New(
		"publication recovery receipt not found",
	)
	ErrPublicationRecoveryConflict = errors.New("publication recovery conflict")
)

type PublicationRecoveryOutcome string

const (
	PublicationRecoveryPublished  PublicationRecoveryOutcome = "published"
	PublicationRecoveryFailed     PublicationRecoveryOutcome = "failed"
	PublicationRecoverySuperseded PublicationRecoveryOutcome = "superseded"
)

// PublicationRecoveryInput contains operator-confirmed, token-free evidence
// for resolving one quarantined publication side effect.
type PublicationRecoveryInput struct {
	SourceKey          string                      `json:"sourceKey"`
	FirstGeneration    int64                       `json:"firstGeneration"`
	PublicationID      string                      `json:"publicationId"`
	ObservedUpdatedAt  string                      `json:"observedUpdatedAt"`
	ObservedClaimEpoch int64                       `json:"observedClaimEpoch"`
	Outcome            PublicationRecoveryOutcome  `json:"outcome"`
	Disposition        AutomationSourceDisposition `json:"disposition"`
	ResultURL          *string                     `json:"resultUrl,omitempty"`
	Actor              string                      `json:"actor"`
	Reason             string                      `json:"reason"`
}

// PublicationRecoveryReceipt is immutable audit evidence. It never contains a
// publication claim token or any other execution credential.
type PublicationRecoveryReceipt struct {
	SourceKey          string                      `json:"sourceKey"`
	FirstGeneration    int64                       `json:"firstGeneration"`
	PublicationID      string                      `json:"publicationId"`
	ObservedUpdatedAt  string                      `json:"observedUpdatedAt"`
	ObservedClaimEpoch int64                       `json:"observedClaimEpoch"`
	Outcome            PublicationRecoveryOutcome  `json:"outcome"`
	Disposition        AutomationSourceDisposition `json:"disposition"`
	ResultURL          *string                     `json:"resultUrl,omitempty"`
	Actor              string                      `json:"actor"`
	Reason             string                      `json:"reason"`
	RecoveredAt        string                      `json:"recoveredAt"`
	ResultUpdatedAt    string                      `json:"resultUpdatedAt"`
}

// PublicationRecoveryState is the token-free, path-free state returned across
// the operator-recovery boundary. It deliberately omits repository, worktree,
// policy, source snapshot, ref, branch, remote, and claim-lease fields.
type PublicationRecoveryState struct {
	ID          string                  `json:"id"`
	Board       string                  `json:"board"`
	Status      model.PublicationStatus `json:"status"`
	URL         *string                 `json:"url,omitempty"`
	Error       *string                 `json:"error,omitempty"`
	ClaimEpoch  int64                   `json:"claimEpoch"`
	PublishedAt *string                 `json:"publishedAt,omitempty"`
	UpdatedAt   string                  `json:"updatedAt"`
	Present     bool                    `json:"present"`
}

type PublicationRecoveryResult struct {
	Receipt     PublicationRecoveryReceipt `json:"receipt"`
	Publication PublicationRecoveryState   `json:"publication"`
	Changed     bool                       `json:"changed"`
}

type PublicationRecoveryConflictError struct {
	SourceKey string
	Reason    string
}

func (e *PublicationRecoveryConflictError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Reason) == "" {
		return fmt.Sprintf("%s: source %s", ErrPublicationRecoveryConflict, e.SourceKey)
	}
	return fmt.Sprintf(
		"%s: source %s: %s",
		ErrPublicationRecoveryConflict,
		e.SourceKey,
		e.Reason,
	)
}

func (e *PublicationRecoveryConflictError) Unwrap() error {
	return ErrPublicationRecoveryConflict
}

const publicationRecoveryReceiptColumns = `source_key, first_generation,
	publication_id, observed_updated_at, observed_claim_epoch, outcome,
	disposition, result_url, actor, reason, recovered_at, result_updated_at`

func scanPublicationRecoveryReceipt(row scanner) (PublicationRecoveryReceipt, error) {
	var value PublicationRecoveryReceipt
	var resultURL sql.NullString
	err := row.Scan(
		&value.SourceKey,
		&value.FirstGeneration,
		&value.PublicationID,
		&value.ObservedUpdatedAt,
		&value.ObservedClaimEpoch,
		&value.Outcome,
		&value.Disposition,
		&resultURL,
		&value.Actor,
		&value.Reason,
		&value.RecoveredAt,
		&value.ResultUpdatedAt,
	)
	value.ResultURL = stringPointer(resultURL)
	if err != nil {
		return PublicationRecoveryReceipt{}, err
	}
	normalized, err := normalizePublicationRecoveryInput(PublicationRecoveryInput{
		SourceKey:          value.SourceKey,
		FirstGeneration:    value.FirstGeneration,
		PublicationID:      value.PublicationID,
		ObservedUpdatedAt:  value.ObservedUpdatedAt,
		ObservedClaimEpoch: value.ObservedClaimEpoch,
		Outcome:            value.Outcome,
		Disposition:        value.Disposition,
		ResultURL:          value.ResultURL,
		Actor:              value.Actor,
		Reason:             value.Reason,
	})
	if err != nil {
		return PublicationRecoveryReceipt{}, fmt.Errorf(
			"invalid publication recovery receipt: %w",
			err,
		)
	}
	if !publicationRecoveryReceiptMatches(value, normalized) {
		return PublicationRecoveryReceipt{}, errors.New(
			"publication recovery receipt is not stored canonically",
		)
	}
	for _, field := range []struct {
		name      string
		timestamp string
	}{
		{name: "recoveredAt", timestamp: value.RecoveredAt},
		{name: "resultUpdatedAt", timestamp: value.ResultUpdatedAt},
	} {
		normalizedTimestamp, err := normalizedPublicationText(
			field.timestamp,
			"publication recovery "+field.name,
			128,
			true,
		)
		if err != nil {
			return PublicationRecoveryReceipt{}, err
		}
		if normalizedTimestamp != field.timestamp {
			return PublicationRecoveryReceipt{}, errors.New(
				"publication recovery receipt timestamp is not stored canonically",
			)
		}
	}
	return value, nil
}

func normalizePublicationRecoveryInput(
	input PublicationRecoveryInput,
) (PublicationRecoveryInput, error) {
	var err error
	input.SourceKey, err = normalizePublicationRecoverySourceKey(input.SourceKey)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	if input.FirstGeneration < 1 {
		return PublicationRecoveryInput{}, errors.New(
			"publication recovery first generation must be positive",
		)
	}
	input.PublicationID, err = validRecordID(
		input.PublicationID,
		"publication recovery publication id",
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	if input.PublicationID == "" {
		return PublicationRecoveryInput{}, errors.New(
			"publication recovery publication id cannot be empty",
		)
	}
	input.ObservedUpdatedAt, err = normalizedPublicationText(
		input.ObservedUpdatedAt,
		"publication recovery observed updatedAt",
		128,
		true,
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	if input.ObservedClaimEpoch < 1 {
		return PublicationRecoveryInput{}, errors.New(
			"publication recovery observed claim epoch must be positive",
		)
	}
	input.Actor, err = boundedAutomationText(
		input.Actor,
		"publication recovery actor",
		maxAutomationActorBytes,
		true,
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	input.Reason, err = boundedAutomationText(
		input.Reason,
		"publication recovery reason",
		maxAutomationReasonBytes,
		true,
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	input.ResultURL, err = normalizePublicationURL(input.ResultURL)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	rawOutcome, err := boundedAutomationText(
		string(input.Outcome),
		"publication recovery outcome",
		32,
		true,
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	input.Outcome = PublicationRecoveryOutcome(rawOutcome)
	rawDisposition, err := boundedAutomationText(
		string(input.Disposition),
		"publication recovery disposition",
		32,
		true,
	)
	if err != nil {
		return PublicationRecoveryInput{}, err
	}
	input.Disposition = AutomationSourceDisposition(rawDisposition)

	expectedDisposition := AutomationSourceSuperseded
	switch input.Outcome {
	case PublicationRecoveryPublished:
		expectedDisposition = AutomationSourceSuperseded
	case PublicationRecoveryFailed:
		expectedDisposition = AutomationSourceAbandoned
	case PublicationRecoverySuperseded:
		expectedDisposition = AutomationSourceSuperseded
	default:
		return PublicationRecoveryInput{}, fmt.Errorf(
			"invalid publication recovery outcome: %s",
			input.Outcome,
		)
	}
	if input.Disposition != expectedDisposition {
		return PublicationRecoveryInput{}, fmt.Errorf(
			"publication recovery outcome %s requires %s disposition",
			input.Outcome,
			expectedDisposition,
		)
	}
	if input.Outcome != PublicationRecoveryPublished && input.ResultURL != nil {
		return PublicationRecoveryInput{}, fmt.Errorf(
			"publication recovery outcome %s cannot have a result URL",
			input.Outcome,
		)
	}
	return input, nil
}

func normalizePublicationRecoverySourceKey(value string) (string, error) {
	value, err := boundedAutomationText(
		value,
		"publication recovery source key",
		64,
		true,
	)
	if err != nil {
		return "", err
	}
	if len(value) != 64 {
		return "", errors.New("publication recovery source key is invalid")
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return "", errors.New(
				"publication recovery source key must be lowercase hexadecimal",
			)
		}
	}
	return value, nil
}

func publicationRecoveryReceiptMatches(
	receipt PublicationRecoveryReceipt,
	input PublicationRecoveryInput,
) bool {
	return receipt.SourceKey == input.SourceKey &&
		receipt.FirstGeneration == input.FirstGeneration &&
		receipt.PublicationID == input.PublicationID &&
		receipt.ObservedUpdatedAt == input.ObservedUpdatedAt &&
		receipt.ObservedClaimEpoch == input.ObservedClaimEpoch &&
		receipt.Outcome == input.Outcome &&
		receipt.Disposition == input.Disposition &&
		sameOptionalString(receipt.ResultURL, input.ResultURL) &&
		receipt.Actor == input.Actor &&
		receipt.Reason == input.Reason
}

func publicationRecoveryReceiptForSource(
	ctx context.Context,
	q querier,
	sourceKey string,
) (PublicationRecoveryReceipt, error) {
	value, err := scanPublicationRecoveryReceipt(q.QueryRowContext(
		ctx,
		"SELECT "+publicationRecoveryReceiptColumns+
			" FROM publication_recovery_receipts WHERE source_key = ?",
		sourceKey,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationRecoveryReceipt{}, fmt.Errorf(
			"%w: %s",
			ErrPublicationRecoveryReceiptNotFound,
			sourceKey,
		)
	}
	return value, err
}

func (s *Store) GetPublicationRecoveryReceipt(
	ctx context.Context,
	sourceKey string,
) (PublicationRecoveryReceipt, bool, error) {
	sourceKey, err := normalizePublicationRecoverySourceKey(sourceKey)
	if err != nil {
		return PublicationRecoveryReceipt{}, false, err
	}
	value, err := publicationRecoveryReceiptForSource(ctx, s.db, sourceKey)
	if errors.Is(err, ErrPublicationRecoveryReceiptNotFound) {
		return PublicationRecoveryReceipt{}, false, nil
	}
	return value, err == nil, err
}

func publicationRecoveryConflict(
	sourceKey string,
	reason string,
) error {
	return &PublicationRecoveryConflictError{
		SourceKey: sourceKey,
		Reason:    reason,
	}
}

func validatePublicationRecoveryReplay(
	receipt PublicationRecoveryReceipt,
	current model.Publication,
) error {
	if current.ClaimEpoch != receipt.ObservedClaimEpoch {
		return publicationRecoveryConflict(
			receipt.SourceKey,
			"publication claim epoch differs from the recovery receipt",
		)
	}
	if current.UpdatedAt != receipt.ResultUpdatedAt {
		return publicationRecoveryConflict(
			receipt.SourceKey,
			"publication changed after the recovery receipt was recorded",
		)
	}
	switch receipt.Outcome {
	case PublicationRecoveryPublished:
		if current.Status != model.PublicationPublished ||
			!sameOptionalString(current.URL, receipt.ResultURL) ||
			current.Error != nil ||
			current.PublishedAt == nil {
			return publicationRecoveryConflict(
				receipt.SourceKey,
				"publication no longer matches the recorded published result",
			)
		}
	case PublicationRecoveryFailed:
		if current.Status != model.PublicationFailed ||
			current.Error == nil || *current.Error != receipt.Reason ||
			current.URL != nil {
			return publicationRecoveryConflict(
				receipt.SourceKey,
				"publication no longer matches the recorded failed result",
			)
		}
	case PublicationRecoverySuperseded:
		if current.Status != model.PublicationSuperseded ||
			current.Error == nil || *current.Error != receipt.Reason ||
			current.URL != nil {
			return publicationRecoveryConflict(
				receipt.SourceKey,
				"publication no longer matches the recorded superseded result",
			)
		}
	default:
		return publicationRecoveryConflict(
			receipt.SourceKey,
			"receipt contains an invalid recovery outcome",
		)
	}
	if current.ClaimToken != "" || current.ClaimExpiresAt != nil {
		return publicationRecoveryConflict(
			receipt.SourceKey,
			"publication retained an unexpected claim lease after recovery",
		)
	}
	return nil
}

func publicationMatchesRecoveryTerminal(
	current model.Publication,
	input PublicationRecoveryInput,
) bool {
	if current.ClaimEpoch != input.ObservedClaimEpoch ||
		current.ClaimToken != "" ||
		current.ClaimExpiresAt != nil {
		return false
	}
	switch input.Outcome {
	case PublicationRecoveryPublished:
		return current.Status == model.PublicationPublished &&
			sameOptionalString(current.URL, input.ResultURL) &&
			current.Error == nil &&
			current.PublishedAt != nil
	case PublicationRecoveryFailed:
		return current.Status == model.PublicationFailed &&
			current.URL == nil &&
			current.Error != nil &&
			*current.Error == input.Reason
	case PublicationRecoverySuperseded:
		return current.Status == model.PublicationSuperseded &&
			current.URL == nil &&
			current.Error != nil &&
			*current.Error == input.Reason
	default:
		return false
	}
}

func clonePublicationRecoveryString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func publicationRecoveryStateFromPublication(
	current model.Publication,
) PublicationRecoveryState {
	return PublicationRecoveryState{
		ID:          current.ID,
		Board:       current.Board,
		Status:      current.Status,
		URL:         clonePublicationRecoveryString(current.URL),
		Error:       clonePublicationRecoveryString(current.Error),
		ClaimEpoch:  current.ClaimEpoch,
		PublishedAt: clonePublicationRecoveryString(current.PublishedAt),
		UpdatedAt:   current.UpdatedAt,
		Present:     true,
	}
}

func publicationRecoveryStateFromReceipt(
	receipt PublicationRecoveryReceipt,
	board string,
) PublicationRecoveryState {
	status := model.PublicationSuperseded
	var publicationError *string
	switch receipt.Outcome {
	case PublicationRecoveryPublished:
		status = model.PublicationPublished
	case PublicationRecoveryFailed:
		status = model.PublicationFailed
		publicationError = clonePublicationRecoveryString(&receipt.Reason)
	case PublicationRecoverySuperseded:
		publicationError = clonePublicationRecoveryString(&receipt.Reason)
	}
	return PublicationRecoveryState{
		ID:         receipt.PublicationID,
		Board:      board,
		Status:     status,
		URL:        clonePublicationRecoveryString(receipt.ResultURL),
		Error:      publicationError,
		ClaimEpoch: receipt.ObservedClaimEpoch,
		UpdatedAt:  receipt.ResultUpdatedAt,
		Present:    false,
	}
}

// ApplyPublicationRecovery atomically records an immutable receipt and either
// terminalizes the exact Publishing tuple or adopts its identical terminal
// result. It requires the callback-scoped authority permit and intentionally
// does not accept, persist, or return a claim token or local path.
func (s *Store) ApplyPublicationRecovery(
	ctx context.Context,
	permit *AutomationRecoveryPermit,
	raw PublicationRecoveryInput,
) (PublicationRecoveryResult, error) {
	input, err := normalizePublicationRecoveryInput(raw)
	if err != nil {
		return PublicationRecoveryResult{}, err
	}
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return PublicationRecoveryResult{}, err
	}
	releasePermit, err := permit.acquirePublicationRecovery(board, input)
	if err != nil {
		return PublicationRecoveryResult{}, err
	}
	defer releasePermit()

	var result PublicationRecoveryResult
	// Archived boards retain their local removal barrier. This narrow operator
	// repair is the sole board-local mutation allowed through that barrier;
	// callers must still hold the global recovery guard and a verified board
	// lifecycle identity.
	err = s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
		existing, existingErr := publicationRecoveryReceiptForSource(
			ctx,
			tx,
			input.SourceKey,
		)
		switch {
		case existingErr == nil:
			if !publicationRecoveryReceiptMatches(existing, input) {
				return publicationRecoveryConflict(
					input.SourceKey,
					"an immutable receipt already records different recovery evidence",
				)
			}
			result.Receipt = existing
			current, getErr := publicationForBoard(
				ctx,
				tx,
				existing.PublicationID,
				board,
			)
			if errors.Is(getErr, ErrPublicationNotFound) {
				result.Publication = publicationRecoveryStateFromReceipt(
					existing,
					board,
				)
				result.Changed = false
				return nil
			}
			if getErr != nil {
				return getErr
			}
			if err := validatePublicationRecoveryReplay(existing, current); err != nil {
				return err
			}
			result.Publication = publicationRecoveryStateFromPublication(current)
			result.Changed = false
			return nil
		case !errors.Is(existingErr, ErrPublicationRecoveryReceiptNotFound):
			return existingErr
		}

		current, err := publicationForBoard(ctx, tx, input.PublicationID, board)
		if err != nil {
			return err
		}
		exactPublishingTuple := current.Status == model.PublicationPublishing &&
			current.UpdatedAt == input.ObservedUpdatedAt &&
			current.ClaimEpoch == input.ObservedClaimEpoch

		adoptingTerminal := false
		if current.Status == model.PublicationPublishing {
			if !exactPublishingTuple {
				return publicationRecoveryConflict(
					input.SourceKey,
					"the current Publishing tuple differs from the quarantined observation",
				)
			}
		} else if publicationMatchesRecoveryTerminal(current, input) {
			adoptingTerminal = true
		} else {
			return publicationRecoveryConflict(
				input.SourceKey,
				"publication is neither the exact Publishing tuple nor the requested terminal result",
			)
		}

		recoveredAt := now()
		resultUpdatedAt := current.UpdatedAt
		switch {
		case adoptingTerminal:
			// Another exact operator attempt may have reached the durable
			// terminal state before its response was observed. Adopt only the
			// identical token-free result and add the missing receipt.
		case input.Outcome == PublicationRecoveryPublished:
			update, err := tx.ExecContext(ctx, `
				UPDATE publications
				SET status = 'published', url = ?, error = NULL,
					claim_token = NULL, claim_expires_at = NULL,
					published_at = ?, updated_at = ?
				WHERE id = ? AND board = ? AND status = 'publishing'
					AND updated_at = ? AND claim_epoch = ?
			`, nullableString(input.ResultURL), recoveredAt, recoveredAt,
				current.ID, board, input.ObservedUpdatedAt,
				input.ObservedClaimEpoch)
			if err != nil {
				return err
			}
			changed, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return publicationRecoveryConflict(
					input.SourceKey,
					"the quarantined Publishing tuple changed before recovery",
				)
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				"publication_completed",
				map[string]any{
					"publicationId": current.ID,
					"claimEpoch":    current.ClaimEpoch,
					"mode":          current.Mode,
					"url":           input.ResultURL,
					"recovery":      true,
					"sourceKey":     input.SourceKey,
					"actor":         input.Actor,
					"reason":        input.Reason,
				},
				&current.RunID,
			); err != nil {
				return err
			}
			current.Status = model.PublicationPublished
			current.URL = input.ResultURL
			current.Error = nil
			current.ClaimToken = ""
			current.ClaimExpiresAt = nil
			current.PublishedAt = &recoveredAt
			current.UpdatedAt = recoveredAt
			resultUpdatedAt = recoveredAt
		case input.Outcome == PublicationRecoveryFailed:
			update, err := tx.ExecContext(ctx, `
				UPDATE publications
				SET status = 'failed', url = NULL, error = ?,
					claim_token = NULL, claim_expires_at = NULL,
					updated_at = ?
				WHERE id = ? AND board = ? AND status = 'publishing'
					AND updated_at = ? AND claim_epoch = ?
			`, input.Reason, recoveredAt, current.ID, board,
				input.ObservedUpdatedAt, input.ObservedClaimEpoch)
			if err != nil {
				return err
			}
			changed, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return publicationRecoveryConflict(
					input.SourceKey,
					"the quarantined Publishing tuple changed before recovery",
				)
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				"publication_failed",
				map[string]any{
					"publicationId": current.ID,
					"claimEpoch":    current.ClaimEpoch,
					"error":         input.Reason,
					"recovery":      true,
					"sourceKey":     input.SourceKey,
					"actor":         input.Actor,
				},
				&current.RunID,
			); err != nil {
				return err
			}
			current.Status = model.PublicationFailed
			current.URL = nil
			current.Error = &input.Reason
			current.ClaimToken = ""
			current.ClaimExpiresAt = nil
			current.UpdatedAt = recoveredAt
			resultUpdatedAt = recoveredAt
		case input.Outcome == PublicationRecoverySuperseded:
			update, err := tx.ExecContext(ctx, `
				UPDATE publications
				SET status = 'superseded', url = NULL, error = ?,
					claim_token = NULL, claim_expires_at = NULL,
					updated_at = ?
				WHERE id = ? AND board = ? AND status = 'publishing'
					AND updated_at = ? AND claim_epoch = ?
			`, input.Reason, recoveredAt, current.ID, board,
				input.ObservedUpdatedAt, input.ObservedClaimEpoch)
			if err != nil {
				return err
			}
			changed, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return publicationRecoveryConflict(
					input.SourceKey,
					"the quarantined Publishing tuple changed before recovery",
				)
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				"publication_superseded",
				map[string]any{
					"publicationId": current.ID,
					"claimEpoch":    current.ClaimEpoch,
					"reason":        input.Reason,
					"recovery":      true,
					"sourceKey":     input.SourceKey,
					"actor":         input.Actor,
				},
				&current.RunID,
			); err != nil {
				return err
			}
			current.Status = model.PublicationSuperseded
			current.URL = nil
			current.Error = &input.Reason
			current.ClaimToken = ""
			current.ClaimExpiresAt = nil
			current.UpdatedAt = recoveredAt
			resultUpdatedAt = recoveredAt
		}

		receipt := PublicationRecoveryReceipt{
			SourceKey:          input.SourceKey,
			FirstGeneration:    input.FirstGeneration,
			PublicationID:      input.PublicationID,
			ObservedUpdatedAt:  input.ObservedUpdatedAt,
			ObservedClaimEpoch: input.ObservedClaimEpoch,
			Outcome:            input.Outcome,
			Disposition:        input.Disposition,
			ResultURL:          input.ResultURL,
			Actor:              input.Actor,
			Reason:             input.Reason,
			RecoveredAt:        recoveredAt,
			ResultUpdatedAt:    resultUpdatedAt,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO publication_recovery_receipts(
				source_key, first_generation, publication_id,
				observed_updated_at, observed_claim_epoch, outcome,
				disposition, result_url, actor, reason, recovered_at,
				result_updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, receipt.SourceKey, receipt.FirstGeneration, receipt.PublicationID,
			receipt.ObservedUpdatedAt, receipt.ObservedClaimEpoch,
			receipt.Outcome, receipt.Disposition,
			nullableString(receipt.ResultURL), receipt.Actor, receipt.Reason,
			receipt.RecoveredAt, receipt.ResultUpdatedAt); err != nil {
			return err
		}
		result.Receipt = receipt
		result.Publication = publicationRecoveryStateFromPublication(current)
		result.Changed = true
		return nil
	})
	return result, err
}
