package operatorrecovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

var (
	ErrInvalidConfirmation = errors.New(
		"invalid operator recovery confirmation",
	)
	ErrPublicationStorageConflict = errors.New(
		"publication recovery storage conflict",
	)
)

type Service struct {
	manager *boards.Manager
}

type secretSafeOperationError struct {
	message string
	cause   error
}

func (e *secretSafeOperationError) Error() string { return e.message }
func (e *secretSafeOperationError) Unwrap() error { return e.cause }

func secretSafeError(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &secretSafeOperationError{message: message, cause: cause}
}

func New(manager *boards.Manager) (*Service, error) {
	if manager == nil {
		return nil, errors.New("operator recovery requires a board manager")
	}
	return &Service{manager: manager}, nil
}

// Status reads the global default-board authority and returns only secret-safe
// recovery state. It never returns a claim/session/permit token or a local
// database, repository, worktree, archive, or attachment path.
func (s *Service) Status(
	ctx context.Context,
) (result Status, resultErr error) {
	if s == nil || s.manager == nil {
		return Status{}, errors.New("operator recovery service is not initialized")
	}
	authority, err := s.manager.OpenCoordinationStore(ctx)
	if err != nil {
		return Status{}, secretSafeError(
			"open operator recovery authority",
			err,
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, authority.Close())
	}()

	snapshot, err := authority.GetAutomationQuarantineRecoverySnapshot(ctx)
	if err != nil {
		return Status{}, fmt.Errorf(
			"read operator recovery authority: %w",
			err,
		)
	}
	result.Gate = snapshot.Gate
	result.UnacknowledgedSessionCount =
		snapshot.UnacknowledgedSessionCount
	if !snapshot.Gate.Active {
		result.Sources = []StatusSource{}
		return result, nil
	}
	result.Sources = make([]StatusSource, 0, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		result.Sources = append(
			result.Sources,
			statusSourceFromAuthority(source),
		)
	}
	sort.Slice(result.Sources, func(i, j int) bool {
		return result.Sources[i].SourceKey < result.Sources[j].SourceKey
	})

	if snapshot.Confirmation.Pending {
		pending, err := pendingConfirmation(snapshot)
		if err != nil {
			return Status{}, err
		}
		result.Pending = &pending
	}
	prepared, err := s.enrichPublicationStatus(ctx, result.Sources)
	if err != nil {
		return Status{}, err
	}
	result.Prepared = prepared
	if snapshot.Confirmation.Pending {
		publicationSourceCount := 0
		for _, source := range result.Sources {
			if source.Kind != PublicationSourceKind {
				continue
			}
			publicationSourceCount++
			_, validOutcome := publicationDisposition(source.Outcome)
			if source.Publication == nil ||
				source.Publication.MatchCount != 1 ||
				!source.Publication.HasReceipt ||
				!validOutcome {
				return Status{}, fmt.Errorf(
					"%w: pending publication source %s lacks one exact recovery receipt",
					ErrPublicationStorageConflict,
					source.SourceKey,
				)
			}
		}
		if publicationSourceCount > 0 &&
			(prepared == nil ||
				prepared.RecoveredPublicationSources != publicationSourceCount ||
				prepared.Actor != result.Pending.Actor ||
				prepared.Reason != result.Pending.Reason) {
			return Status{}, fmt.Errorf(
				"%w: pending confirmation differs from prepared receipts",
				ErrPublicationStorageConflict,
			)
		}
	}
	return result, nil
}

func statusSourceFromAuthority(
	source store.AutomationQuarantineSource,
) StatusSource {
	return StatusSource{
		SourceKey:          source.SourceKey,
		FirstGeneration:    source.Generation,
		Board:              source.Board,
		Kind:               source.Kind,
		SourceID:           source.SourceID,
		ObservedUpdatedAt:  source.ObservedUpdatedAt,
		ObservedClaimEpoch: source.ObservedClaimEpoch,
		DiagnosticCode:     source.DiagnosticCode,
		Disposition:        source.Disposition,
		ResolvedGeneration: source.ResolvedGeneration,
	}
}

func pendingConfirmation(
	snapshot store.AutomationQuarantineRecoverySnapshot,
) (PendingConfirmation, error) {
	for _, source := range snapshot.Sources {
		if source.ResolvedGeneration == nil ||
			*source.ResolvedGeneration != snapshot.Gate.Generation ||
			source.ResolvedBy == nil ||
			source.ResolutionReason == nil {
			return PendingConfirmation{}, fmt.Errorf(
				"%w: pending source lacks exact resolution evidence",
				store.ErrAutomationSourceConflict,
			)
		}
		if snapshot.Confirmation.Actor == nil ||
			snapshot.Confirmation.Reason == nil ||
			*source.ResolvedBy != *snapshot.Confirmation.Actor ||
			*source.ResolutionReason != *snapshot.Confirmation.Reason {
			return PendingConfirmation{}, fmt.Errorf(
				"%w: pending source confirmations disagree",
				store.ErrAutomationSourceConflict,
			)
		}
	}
	if snapshot.Confirmation.Actor == nil ||
		snapshot.Confirmation.Reason == nil ||
		!snapshot.Confirmation.HelpersStopped ||
		!snapshot.Confirmation.ExternalWritesStopped {
		return PendingConfirmation{}, fmt.Errorf(
			"%w: pending gate lacks complete confirmation evidence",
			store.ErrAutomationSourceConflict,
		)
	}
	return PendingConfirmation{
		ResolvedGeneration:    snapshot.Gate.Generation,
		Actor:                 *snapshot.Confirmation.Actor,
		Reason:                *snapshot.Confirmation.Reason,
		HelpersStopped:        snapshot.Confirmation.HelpersStopped,
		ExternalWritesStopped: snapshot.Confirmation.ExternalWritesStopped,
	}, nil
}

// Confirm couples board-local publication recovery to the global authority's
// Guard. The authority holds its exclusive automation lock while this service
// rescans every active/archived database, proves a unique storage location for
// every publication source, commits idempotent receipts, and only then allows
// phase one source resolution and gate clearing.
func (s *Service) Confirm(
	ctx context.Context,
	input Confirmation,
) (result ConfirmationResult, resultErr error) {
	if s == nil || s.manager == nil {
		return ConfirmationResult{}, errors.New(
			"operator recovery service is not initialized",
		)
	}
	normalized, parsedEpochs, authorityInput, err :=
		validateConfirmation(input)
	if err != nil {
		return ConfirmationResult{}, err
	}
	authority, err := s.manager.OpenCoordinationStore(ctx)
	if err != nil {
		return ConfirmationResult{}, secretSafeError(
			"open operator recovery authority",
			err,
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, authority.Close())
	}()

	var publications []PublicationResult
	authorityInput.Guard = func(
		guardContext context.Context,
		snapshot store.AutomationQuarantineSnapshot,
	) error {
		sources, err := exactGuardSources(snapshot, normalized)
		if err != nil {
			return err
		}
		plan, err := s.buildPublicationRecoveryPlan(
			guardContext,
			normalized,
			parsedEpochs,
			sources,
		)
		if err != nil {
			return err
		}
		if snapshot.RecoveryPermit == nil {
			return store.ErrAutomationRecoveryPermit
		}
		publications, err = s.applyPublicationRecoveryPlan(
			guardContext,
			snapshot.RecoveryPermit,
			plan,
		)
		return err
	}
	gate, cleared, err := authority.ConfirmAutomationQuarantine(
		ctx,
		authorityInput,
	)
	if err != nil {
		return ConfirmationResult{}, err
	}
	sort.Slice(publications, func(i, j int) bool {
		return publications[i].SourceKey < publications[j].SourceKey
	})
	return ConfirmationResult{
		Gate:         gate,
		Cleared:      cleared,
		Publications: publications,
	}, nil
}

func validateConfirmation(
	input Confirmation,
) (
	Confirmation,
	map[string]int64,
	store.AutomationQuarantineConfirmation,
	error,
) {
	if input.Generation < 1 {
		return Confirmation{}, nil, store.AutomationQuarantineConfirmation{},
			invalidConfirmation("generation must be positive")
	}
	if err := canonicalRequiredText(input.Actor, "actor"); err != nil {
		return Confirmation{}, nil, store.AutomationQuarantineConfirmation{}, err
	}
	if err := canonicalRequiredText(input.Reason, "reason"); err != nil {
		return Confirmation{}, nil, store.AutomationQuarantineConfirmation{}, err
	}
	if !input.HelpersStopped || !input.ExternalWritesStopped {
		return Confirmation{}, nil, store.AutomationQuarantineConfirmation{},
			invalidConfirmation(
				"helpersStopped and externalWritesStopped must both be true",
			)
	}
	if len(input.Sources) > 1000 {
		return Confirmation{}, nil, store.AutomationQuarantineConfirmation{},
			invalidConfirmation("source count cannot exceed 1000")
	}
	seen := make(map[string]bool, len(input.Sources))
	epochs := make(map[string]int64, len(input.Sources))
	resolutions := make(
		[]store.AutomationQuarantineSourceResolution,
		0,
		len(input.Sources),
	)
	for index := range input.Sources {
		source := &input.Sources[index]
		for field, value := range map[string]string{
			"sourceKey":      source.SourceKey,
			"board":          source.Board,
			"kind":           source.Kind,
			"sourceId":       source.SourceID,
			"diagnosticCode": source.DiagnosticCode,
		} {
			if err := canonicalRequiredText(value, field); err != nil {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					fmt.Errorf("source %d: %w", index, err)
			}
		}
		if source.ObservedUpdatedAt != "" {
			if err := canonicalRequiredText(
				source.ObservedUpdatedAt,
				"observedUpdatedAt",
			); err != nil {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					fmt.Errorf("source %d: %w", index, err)
			}
		}
		if len(source.SourceKey) != 64 ||
			strings.IndexFunc(source.SourceKey, func(value rune) bool {
				return (value < '0' || value > '9') &&
					(value < 'a' || value > 'f')
			}) >= 0 {
			return Confirmation{}, nil,
				store.AutomationQuarantineConfirmation{},
				invalidConfirmation(
					fmt.Sprintf("source %d has an invalid sourceKey", index),
				)
		}
		if seen[source.SourceKey] {
			return Confirmation{}, nil,
				store.AutomationQuarantineConfirmation{},
				invalidConfirmation(
					fmt.Sprintf("source %d duplicates sourceKey", index),
				)
		}
		seen[source.SourceKey] = true

		epoch := int64(0)
		if source.ObservedClaimEpoch != "" {
			if strings.TrimSpace(source.ObservedClaimEpoch) !=
				source.ObservedClaimEpoch {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"source %d claim epoch is not canonical",
							index,
						),
					)
			}
			parsed, err := strconv.ParseInt(
				source.ObservedClaimEpoch,
				10,
				64,
			)
			if err != nil || parsed < 1 ||
				strconv.FormatInt(parsed, 10) !=
					source.ObservedClaimEpoch {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"source %d claim epoch must be a positive canonical decimal",
							index,
						),
					)
			}
			epoch = parsed
		}
		if source.ObservedUpdatedAt == "" && epoch == 0 {
			return Confirmation{}, nil,
				store.AutomationQuarantineConfirmation{},
				invalidConfirmation(
					fmt.Sprintf(
						"source %d requires an update or claim epoch",
						index,
					),
				)
		}
		if source.Kind == PublicationSourceKind {
			if normalized, err := boards.NormalizeSlug(source.Board); err != nil ||
				normalized != source.Board {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"publication source %d has an invalid board",
							index,
						),
					)
			}
			if source.ObservedUpdatedAt == "" || epoch < 1 {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"publication source %d requires an update and claim epoch",
							index,
						),
					)
			}
			expectedDisposition, valid := publicationDisposition(
				source.Outcome,
			)
			if !valid {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"publication source %d has an invalid outcome",
							index,
						),
					)
			}
			if source.Disposition != expectedDisposition {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"publication outcome %s requires disposition %s",
							source.Outcome,
							expectedDisposition,
						),
					)
			}
			if source.Outcome != PublicationOutcomePublished &&
				source.ResultURL != nil {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"publication outcome %s cannot have resultUrl",
							source.Outcome,
						),
					)
			}
			if source.ResultURL != nil {
				if err := canonicalRequiredText(
					*source.ResultURL,
					"resultUrl",
				); err != nil {
					return Confirmation{}, nil,
						store.AutomationQuarantineConfirmation{},
						err
				}
			}
			epochs[source.SourceKey] = epoch
		} else {
			if source.Outcome != "" || source.ResultURL != nil {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"non-publication source %d accepts only a disposition",
							index,
						),
					)
			}
			if source.Disposition != store.AutomationSourceSuperseded &&
				source.Disposition != store.AutomationSourceAbandoned {
				return Confirmation{}, nil,
					store.AutomationQuarantineConfirmation{},
					invalidConfirmation(
						fmt.Sprintf(
							"non-publication source %d has an invalid disposition",
							index,
						),
					)
			}
		}
		resolutions = append(
			resolutions,
			store.AutomationQuarantineSourceResolution{
				SourceKey:          source.SourceKey,
				ObservedUpdatedAt:  source.ObservedUpdatedAt,
				ObservedClaimEpoch: source.ObservedClaimEpoch,
				Disposition:        source.Disposition,
				Outcome: store.PublicationRecoveryOutcome(
					source.Outcome,
				),
				ResultURL: cloneStringPointer(source.ResultURL),
			},
		)
	}
	return input, epochs, store.AutomationQuarantineConfirmation{
		Generation:            input.Generation,
		Actor:                 input.Actor,
		Reason:                input.Reason,
		HelpersStopped:        input.HelpersStopped,
		ExternalWritesStopped: input.ExternalWritesStopped,
		Sources:               resolutions,
	}, nil
}

func canonicalRequiredText(value, field string) error {
	if strings.TrimSpace(value) == "" {
		return invalidConfirmation(field + " cannot be empty")
	}
	if strings.TrimSpace(value) != value {
		return invalidConfirmation(field + " is not canonical")
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return invalidConfirmation(
			field + " must be valid UTF-8 without NUL",
		)
	}
	return nil
}

func invalidConfirmation(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfirmation, reason)
}

func publicationDisposition(
	outcome PublicationOutcome,
) (store.AutomationSourceDisposition, bool) {
	switch outcome {
	case PublicationOutcomePublished:
		return store.AutomationSourceSuperseded, true
	case PublicationOutcomeFailed:
		return store.AutomationSourceAbandoned, true
	case PublicationOutcomeSuperseded:
		return store.AutomationSourceSuperseded, true
	default:
		return "", false
	}
}

func exactGuardSources(
	snapshot store.AutomationQuarantineSnapshot,
	input Confirmation,
) (map[string]store.AutomationQuarantineSource, error) {
	if snapshot.Gate.Generation != input.Generation ||
		len(snapshot.Sources) != len(input.Sources) {
		return nil, fmt.Errorf(
			"%w: guard source generation or count changed",
			store.ErrAutomationSourceConflict,
		)
	}
	actual := make(
		map[string]store.AutomationQuarantineSource,
		len(snapshot.Sources),
	)
	for _, source := range snapshot.Sources {
		actual[source.SourceKey] = source
	}
	for _, expected := range input.Sources {
		source, ok := actual[expected.SourceKey]
		if !ok ||
			source.Board != expected.Board ||
			source.Kind != expected.Kind ||
			source.SourceID != expected.SourceID ||
			source.ObservedUpdatedAt != expected.ObservedUpdatedAt ||
			source.ObservedClaimEpoch != expected.ObservedClaimEpoch ||
			source.DiagnosticCode != expected.DiagnosticCode {
			return nil, fmt.Errorf(
				"%w: source %s identity changed",
				store.ErrAutomationSourceConflict,
				expected.SourceKey,
			)
		}
		if source.Disposition != "active" &&
			source.Disposition != string(expected.Disposition) {
			return nil, fmt.Errorf(
				"%w: source %s disposition changed",
				store.ErrAutomationSourceConflict,
				expected.SourceKey,
			)
		}
	}
	return actual, nil
}

type publicationInventoryMatch struct {
	metadata boards.Metadata
	current  *model.Publication
	receipt  *store.PublicationRecoveryReceipt
}

type publicationRecoveryPlanItem struct {
	source   ConfirmationSource
	metadata boards.Metadata
	input    store.PublicationRecoveryInput
}

func (s *Service) publicationInventory(
	ctx context.Context,
	sources map[string]StatusSource,
) (map[string][]publicationInventoryMatch, error) {
	inventory, err := s.manager.ListMetadata(ctx, true)
	if err != nil {
		return nil, secretSafeError(
			"list publication recovery inventory",
			err,
		)
	}
	matches := make(
		map[string][]publicationInventoryMatch,
		len(sources),
	)
	wantedBoards := make(map[string]bool)
	for _, source := range sources {
		wantedBoards[source.Board] = true
	}
	for _, metadata := range inventory {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !wantedBoards[metadata.Slug] {
			continue
		}
		reader, err := s.manager.OpenListedPublicationRecoveryReader(
			ctx,
			metadata,
		)
		if err != nil {
			return nil, secretSafeError(
				fmt.Sprintf(
					"inspect publication recovery inventory for board %s",
					metadata.Slug,
				),
				err,
			)
		}
		var boardErr error
		for sourceKey, source := range sources {
			if source.Board != metadata.Slug {
				continue
			}
			current, foundCurrent, currentErr :=
				reader.GetPublicationForRecovery(ctx, source.SourceID)
			if currentErr != nil {
				boardErr = currentErr
				break
			}
			receipt, foundReceipt, receiptErr :=
				reader.GetPublicationRecoveryReceipt(ctx, sourceKey)
			if receiptErr != nil {
				boardErr = receiptErr
				break
			}
			if foundReceipt {
				epoch, parseErr := strconv.ParseInt(
					source.ObservedClaimEpoch,
					10,
					64,
				)
				if parseErr != nil ||
					receipt.SourceKey != source.SourceKey ||
					receipt.FirstGeneration != source.FirstGeneration ||
					receipt.PublicationID != source.SourceID ||
					receipt.ObservedUpdatedAt != source.ObservedUpdatedAt ||
					receipt.ObservedClaimEpoch != epoch {
					boardErr = fmt.Errorf(
						"%w: receipt %s has incompatible source identity",
						ErrPublicationStorageConflict,
						sourceKey,
					)
					break
				}
			}
			if foundCurrent || foundReceipt {
				match := publicationInventoryMatch{metadata: metadata}
				if foundCurrent {
					value := current
					match.current = &value
				}
				if foundReceipt {
					value := receipt
					match.receipt = &value
				}
				matches[sourceKey] = append(matches[sourceKey], match)
			}
		}
		closeErr := reader.Close()
		if boardErr != nil || closeErr != nil {
			return nil, errors.Join(boardErr, closeErr)
		}
	}
	return matches, nil
}

func (s *Service) enrichPublicationStatus(
	ctx context.Context,
	sources []StatusSource,
) (*PreparedConfirmation, error) {
	wanted := make(map[string]StatusSource)
	indexes := make(map[string]int)
	for index, source := range sources {
		if source.Kind != PublicationSourceKind {
			continue
		}
		wanted[source.SourceKey] = source
		indexes[source.SourceKey] = index
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	matches, err := s.publicationInventory(ctx, wanted)
	if err != nil {
		return nil, err
	}
	var prepared *PreparedConfirmation
	for sourceKey, index := range indexes {
		values := matches[sourceKey]
		status := &PublicationStorageStatus{
			MatchCount: len(values),
		}
		sources[index].Publication = status
		if len(values) != 1 {
			continue
		}
		match := values[0]
		archived := match.metadata.Archived
		status.Archived = &archived
		status.HasReceipt = match.receipt != nil
		if match.current != nil {
			status.CurrentStatus = match.current.Status
			status.CurrentUpdatedAt = match.current.UpdatedAt
			status.CurrentClaimEpoch = match.current.ClaimEpoch
		}
		if match.receipt != nil {
			disposition := match.receipt.Disposition
			sources[index].ReceiptDisposition = &disposition
			sources[index].Outcome = PublicationOutcome(
				match.receipt.Outcome,
			)
			sources[index].ResultURL = cloneStringPointer(
				match.receipt.ResultURL,
			)
			recoveredAt := match.receipt.RecoveredAt
			resultUpdatedAt := match.receipt.ResultUpdatedAt
			sources[index].RecoveredAt = &recoveredAt
			sources[index].ResultUpdatedAt = &resultUpdatedAt
			if prepared == nil {
				prepared = &PreparedConfirmation{
					Actor:  match.receipt.Actor,
					Reason: match.receipt.Reason,
				}
			} else if prepared.Actor != match.receipt.Actor ||
				prepared.Reason != match.receipt.Reason {
				return nil, fmt.Errorf(
					"%w: prepared publication receipts disagree on actor or reason",
					ErrPublicationStorageConflict,
				)
			}
			prepared.RecoveredPublicationSources++
		}
	}
	return prepared, nil
}

func (s *Service) buildPublicationRecoveryPlan(
	ctx context.Context,
	input Confirmation,
	epochs map[string]int64,
	authoritySources map[string]store.AutomationQuarantineSource,
) ([]publicationRecoveryPlanItem, error) {
	wanted := make(map[string]StatusSource)
	confirmationByKey := make(map[string]ConfirmationSource)
	for _, source := range input.Sources {
		if source.Kind != PublicationSourceKind {
			continue
		}
		authority := authoritySources[source.SourceKey]
		wanted[source.SourceKey] = StatusSource{
			SourceKey:          source.SourceKey,
			FirstGeneration:    authority.Generation,
			Board:              source.Board,
			Kind:               source.Kind,
			SourceID:           source.SourceID,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: source.ObservedClaimEpoch,
			DiagnosticCode:     source.DiagnosticCode,
		}
		confirmationByKey[source.SourceKey] = source
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	matches, err := s.publicationInventory(ctx, wanted)
	if err != nil {
		return nil, err
	}
	plan := make([]publicationRecoveryPlanItem, 0, len(wanted))
	for _, source := range input.Sources {
		if source.Kind != PublicationSourceKind {
			continue
		}
		values := matches[source.SourceKey]
		if len(values) != 1 {
			return nil, fmt.Errorf(
				"%w: source %s matched %d board databases, expected exactly one",
				ErrPublicationStorageConflict,
				source.SourceKey,
				len(values),
			)
		}
		match := values[0]
		recoveryInput := store.PublicationRecoveryInput{
			SourceKey:          source.SourceKey,
			FirstGeneration:    authoritySources[source.SourceKey].Generation,
			PublicationID:      source.SourceID,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: epochs[source.SourceKey],
			Outcome: store.PublicationRecoveryOutcome(
				source.Outcome,
			),
			Disposition: source.Disposition,
			ResultURL:   cloneStringPointer(source.ResultURL),
			Actor:       input.Actor,
			Reason:      input.Reason,
		}
		if match.receipt != nil {
			if !receiptMatchesConfirmation(
				*match.receipt,
				recoveryInput,
			) ||
				(match.current != nil &&
					!publicationMatchesReceiptCurrent(
						*match.current,
						*match.receipt,
					)) {
				return nil, fmt.Errorf(
					"%w: source %s receipt or terminal publication differs",
					ErrPublicationStorageConflict,
					source.SourceKey,
				)
			}
		} else {
			if match.current == nil ||
				!publicationCanApplyOrAdopt(
					*match.current,
					source,
					epochs[source.SourceKey],
					input.Reason,
				) {
				return nil, fmt.Errorf(
					"%w: source %s current publication cannot satisfy outcome %s",
					ErrPublicationStorageConflict,
					source.SourceKey,
					source.Outcome,
				)
			}
		}
		plan = append(plan, publicationRecoveryPlanItem{
			source:   confirmationByKey[source.SourceKey],
			metadata: match.metadata,
			input:    recoveryInput,
		})
	}
	return plan, nil
}

func receiptMatchesConfirmation(
	receipt store.PublicationRecoveryReceipt,
	input store.PublicationRecoveryInput,
) bool {
	return receipt.SourceKey == input.SourceKey &&
		receipt.FirstGeneration == input.FirstGeneration &&
		receipt.PublicationID == input.PublicationID &&
		receipt.ObservedUpdatedAt == input.ObservedUpdatedAt &&
		receipt.ObservedClaimEpoch == input.ObservedClaimEpoch &&
		receipt.Outcome == input.Outcome &&
		receipt.Disposition == input.Disposition &&
		equalStringPointers(receipt.ResultURL, input.ResultURL) &&
		receipt.Actor == input.Actor &&
		receipt.Reason == input.Reason
}

func publicationCanApplyOrAdopt(
	current model.Publication,
	source ConfirmationSource,
	epoch int64,
	reason string,
) bool {
	exactPublishing := current.Status == model.PublicationPublishing &&
		current.UpdatedAt == source.ObservedUpdatedAt &&
		current.ClaimEpoch == epoch
	if exactPublishing {
		return true
	}
	if current.ClaimEpoch != epoch {
		return false
	}
	switch source.Outcome {
	case PublicationOutcomePublished:
		return current.Status == model.PublicationPublished &&
			equalStringPointers(current.URL, source.ResultURL) &&
			current.Error == nil &&
			current.PublishedAt != nil
	case PublicationOutcomeFailed:
		return current.Status == model.PublicationFailed &&
			current.URL == nil &&
			current.Error != nil && *current.Error == reason
	case PublicationOutcomeSuperseded:
		return current.Status == model.PublicationSuperseded &&
			current.URL == nil &&
			current.Error != nil && *current.Error == reason
	default:
		return false
	}
}

func publicationMatchesReceiptCurrent(
	current model.Publication,
	receipt store.PublicationRecoveryReceipt,
) bool {
	if current.ID != receipt.PublicationID ||
		current.ClaimEpoch != receipt.ObservedClaimEpoch ||
		current.UpdatedAt != receipt.ResultUpdatedAt {
		return false
	}
	switch receipt.Outcome {
	case store.PublicationRecoveryPublished:
		return current.Status == model.PublicationPublished &&
			equalStringPointers(current.URL, receipt.ResultURL) &&
			current.Error == nil &&
			current.PublishedAt != nil
	case store.PublicationRecoveryFailed:
		return current.Status == model.PublicationFailed &&
			current.URL == nil &&
			current.Error != nil &&
			*current.Error == receipt.Reason
	case store.PublicationRecoverySuperseded:
		return current.Status == model.PublicationSuperseded &&
			current.URL == nil &&
			current.Error != nil &&
			*current.Error == receipt.Reason
	default:
		return false
	}
}

func (s *Service) applyPublicationRecoveryPlan(
	ctx context.Context,
	permit *store.AutomationRecoveryPermit,
	plan []publicationRecoveryPlanItem,
) ([]PublicationResult, error) {
	type recoveryGroup struct {
		metadata boards.Metadata
		items    []publicationRecoveryPlanItem
	}
	groups := make([]recoveryGroup, 0)
	groupIndex := make(map[string]int)
	for _, item := range plan {
		// Active and archived clones can share a slug, so include the stable
		// inventory order's archive bit and listed DB path only as an internal
		// grouping key. It never crosses the service boundary.
		key := fmt.Sprintf(
			"%t\x00%s",
			item.metadata.Archived,
			item.metadata.DBPath,
		)
		index, exists := groupIndex[key]
		if !exists {
			index = len(groups)
			groupIndex[key] = index
			groups = append(groups, recoveryGroup{
				metadata: item.metadata,
			})
		}
		groups[index].items = append(groups[index].items, item)
	}
	results := make([]PublicationResult, 0, len(plan))
	for _, group := range groups {
		inputs := make(
			[]store.PublicationRecoveryInput,
			0,
			len(group.items),
		)
		for _, item := range group.items {
			inputs = append(inputs, item.input)
		}
		applied, err := s.manager.ApplyListedPublicationRecoveries(
			ctx,
			group.metadata,
			permit,
			inputs,
		)
		if err != nil {
			return nil, secretSafeError(
				fmt.Sprintf(
					"apply publication recovery on board %s",
					group.metadata.Slug,
				),
				err,
			)
		}
		if len(applied) != len(group.items) {
			return nil, errors.New(
				"publication recovery returned an invalid result count",
			)
		}
		for index, value := range applied {
			item := group.items[index]
			results = append(results, safePublicationResult(
				item.source,
				value,
			))
		}
	}
	return results, nil
}

func safePublicationResult(
	source ConfirmationSource,
	result store.PublicationRecoveryResult,
) PublicationResult {
	return PublicationResult{
		SourceKey:     source.SourceKey,
		Board:         source.Board,
		PublicationID: source.SourceID,
		Status:        result.Publication.Status,
		UpdatedAt:     result.Publication.UpdatedAt,
		ClaimEpoch:    result.Publication.ClaimEpoch,
		Outcome:       source.Outcome,
		Disposition:   source.Disposition,
		ResultURL:     cloneStringPointer(result.Receipt.ResultURL),
		Changed:       result.Changed,
		RecoveredAt:   result.Receipt.RecoveredAt,
		Present:       result.Publication.Present,
	}
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
