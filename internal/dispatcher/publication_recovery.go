package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const (
	publicationQuarantineKind                 = "publication"
	publicationOwnershipUnconfirmedDiagnostic = "publishing_ownership_unconfirmed"
	publicationOwnershipScanFailureDiagnostic = "publication_ownership_scan_failed"
	publicationAttemptMissingDiagnostic       = "publication_attempt_result_missing"
	publicationStartupOwnershipScanTimeout    = 30 * time.Second
	publicationAttemptActivationRetries       = 3
)

var errPublicationQuarantineSourceResolved = errors.New(
	"exact publication quarantine source was already resolved",
)

func publicationQuarantineSource(
	value model.Publication,
	validate store.AutomationQuarantineSourceValidator,
) store.AutomationQuarantineSourceInput {
	return publicationQuarantineSourceWithDiagnostic(
		value,
		publicationOwnershipUnconfirmedDiagnostic,
		validate,
	)
}

func publicationQuarantineSourceWithDiagnostic(
	value model.Publication,
	diagnostic string,
	validate store.AutomationQuarantineSourceValidator,
) store.AutomationQuarantineSourceInput {
	claimEpoch := ""
	if value.ClaimEpoch > 0 {
		claimEpoch = strconv.FormatInt(value.ClaimEpoch, 10)
	}
	return store.AutomationQuarantineSourceInput{
		Board:              value.Board,
		Kind:               publicationQuarantineKind,
		SourceID:           value.ID,
		ObservedUpdatedAt:  value.UpdatedAt,
		ObservedClaimEpoch: claimEpoch,
		DiagnosticCode:     diagnostic,
		ValidateCurrent:    validate,
	}
}

func (s *automationDispatcherSession) persistPublicationQuarantine(
	ctx context.Context,
	value model.Publication,
	validate store.AutomationQuarantineSourceValidator,
) (store.AutomationQuarantine, error) {
	return s.persistPublicationQuarantineWithDiagnostic(
		ctx,
		value,
		publicationOwnershipUnconfirmedDiagnostic,
		validate,
	)
}

func (s *automationDispatcherSession) persistPublicationQuarantineWithDiagnostic(
	ctx context.Context,
	value model.Publication,
	diagnostic string,
	validate store.AutomationQuarantineSourceValidator,
) (store.AutomationQuarantine, error) {
	if s == nil {
		return store.AutomationQuarantine{}, errors.New(
			"automation dispatcher session is required for publication quarantine",
		)
	}
	if value.Status != model.PublicationPublishing {
		return store.AutomationQuarantine{}, fmt.Errorf(
			"publication %s is %s, expected publishing",
			value.ID,
			value.Status,
		)
	}
	gate, outcome, err := s.authority.EnsureAutomationQuarantineSource(
		ctx,
		publicationQuarantineSourceWithDiagnostic(
			value,
			diagnostic,
			validate,
		),
	)
	if err != nil {
		return store.AutomationQuarantine{}, fmt.Errorf(
			"persist publication %s quarantine source: %w",
			value.ID,
			err,
		)
	}
	switch outcome {
	case store.AutomationQuarantineSourceCreated,
		store.AutomationQuarantineSourceExistingActive:
	case store.AutomationQuarantineSourceExistingResolved:
		return gate, fmt.Errorf(
			"%w: publication %s",
			errPublicationQuarantineSourceResolved,
			value.ID,
		)
	default:
		return gate, fmt.Errorf(
			"publication %s quarantine source returned invalid lifecycle outcome %q",
			value.ID,
			outcome,
		)
	}
	if !gate.Active {
		return gate, fmt.Errorf(
			"publication %s quarantine source did not activate the gate",
			value.ID,
		)
	}
	return gate, nil
}

func failPublishingOwnershipScan(
	session *automationDispatcherSession,
	cause error,
) error {
	scanErr := fmt.Errorf("scan publishing ownership at dispatcher startup: %w", cause)
	if session == nil {
		return scanErr
	}
	activationErr := session.activateUnconfirmedQuarantine(
		publicationOwnershipScanFailureDiagnostic,
	)
	if errors.Is(activationErr, store.ErrAutomationQuarantined) {
		return errors.Join(scanErr, activationErr)
	}
	operationContext, cancel := context.WithTimeout(
		context.Background(),
		automationSessionOperationLimit,
	)
	gate, inspectErr := session.authority.GetAutomationQuarantine(
		operationContext,
	)
	cancel()
	var observeErr error
	if inspectErr == nil && gate.Active {
		observeErr = session.observeGate(gate)
	}
	return errors.Join(scanErr, activationErr, inspectErr, observeErr)
}

type publicationAttemptRecoveryState uint8

const (
	publicationAttemptRecoveryResolved publicationAttemptRecoveryState = iota + 1
	publicationAttemptRecoveryUnresolved
)

type publicationAttemptRecoveryDecision struct {
	Intent     store.PublicationAttemptIntent
	State      publicationAttemptRecoveryState
	Diagnostic string
}

func publicationAttemptRecoveryIntegrity(
	intent store.PublicationAttemptIntent,
	reason string,
) error {
	return fmt.Errorf(
		"publication attempt %s recovery integrity failure: %s",
		intent.ID,
		reason,
	)
}

func validatePublicationAttemptObservationIdentity(
	board string,
	expected *store.PublicationAttemptIntent,
	observation store.PublicationAttemptRecoveryObservation,
) error {
	intent := observation.Attempt.Intent
	if intent.ID == "" || intent.PublicationID == "" ||
		intent.SourceKey == "" || intent.EffectFingerprint == "" ||
		intent.Board != board {
		return publicationAttemptRecoveryIntegrity(
			intent,
			fmt.Sprintf(
				"reader returned incompatible intent identity for board %s",
				board,
			),
		)
	}
	if expected != nil && intent != *expected {
		return publicationAttemptRecoveryIntegrity(
			*expected,
			"exact re-observation changed the immutable intent identity",
		)
	}
	if result := observation.Attempt.Result; result != nil &&
		(result.AttemptID != intent.ID ||
			result.Board != intent.Board ||
			result.PublicationID != intent.PublicationID ||
			result.ClaimEpoch != intent.ClaimEpoch) {
		return publicationAttemptRecoveryIntegrity(
			intent,
			"result identity does not match its intent",
		)
	}
	if current := observation.Publication; current.Present &&
		(current.ID != intent.PublicationID ||
			current.Board != intent.Board) {
		return publicationAttemptRecoveryIntegrity(
			intent,
			"current publication identity does not match its intent",
		)
	}
	return nil
}

func publicationAttemptReceiptMatchesIntent(
	receipt store.PublicationRecoveryReceipt,
	intent store.PublicationAttemptIntent,
) bool {
	return receipt.SourceKey == intent.SourceKey &&
		receipt.PublicationID == intent.PublicationID &&
		receipt.ObservedUpdatedAt == intent.PublicationUpdatedAt &&
		receipt.ObservedClaimEpoch == intent.ClaimEpoch
}

func publicationAttemptCurrentIsOriginal(
	current store.PublicationAttemptRecoveryPublication,
	intent store.PublicationAttemptIntent,
) bool {
	return publicationAttemptCurrentHasOriginalTupleIdentity(
		current,
		intent,
	) &&
		current.ChangeSetID == intent.ChangeSetID &&
		current.Mode == intent.Mode &&
		current.TargetBranch == intent.TargetBranch &&
		current.Remote == intent.Remote &&
		current.BaseCommit == intent.BaseCommit &&
		current.HeadCommit == intent.HeadCommit &&
		current.DurableRef == intent.DurableRef &&
		current.ExecutionProvenanceFingerprint ==
			intent.ExecutionProvenanceFingerprint &&
		current.ClaimExpiresAt != nil &&
		*current.ClaimExpiresAt == intent.ClaimExpiresAt
}

func publicationAttemptCurrentHasOriginalTupleIdentity(
	current store.PublicationAttemptRecoveryPublication,
	intent store.PublicationAttemptIntent,
) bool {
	return current.Present &&
		current.ID == intent.PublicationID &&
		current.Board == intent.Board &&
		current.Status == model.PublicationPublishing &&
		current.ClaimEpoch == intent.ClaimEpoch &&
		current.UpdatedAt == intent.PublicationUpdatedAt
}

func classifyPublicationAttemptRecovery(
	observation store.PublicationAttemptRecoveryObservation,
) (
	publicationAttemptRecoveryState,
	string,
	error,
) {
	intent := observation.Attempt.Intent
	original := publicationAttemptCurrentIsOriginal(
		observation.Publication,
		intent,
	)
	if !original && publicationAttemptCurrentHasOriginalTupleIdentity(
		observation.Publication,
		intent,
	) {
		return 0, "", publicationAttemptRecoveryIntegrity(
			intent,
			"original Publishing tuple has incompatible effect identity",
		)
	}
	if receipt := observation.Attempt.RecoveryReceipt; receipt != nil {
		if !publicationAttemptReceiptMatchesIntent(*receipt, intent) {
			return 0, "", publicationAttemptRecoveryIntegrity(
				intent,
				"recovery receipt identity does not match its intent",
			)
		}
		if original {
			return 0, "", publicationAttemptRecoveryIntegrity(
				intent,
				"recovery receipt exists beside the original Publishing tuple",
			)
		}
		return publicationAttemptRecoveryResolved, "", nil
	}
	result := observation.Attempt.Result
	if result != nil && result.Outcome != store.PublicationAttemptUnknown {
		if original {
			return 0, "", publicationAttemptRecoveryIntegrity(
				intent,
				"known result exists beside the original Publishing tuple",
			)
		}
		return publicationAttemptRecoveryResolved, "", nil
	}
	if !original {
		reason := "unresolved attempt no longer has its original Publishing tuple"
		switch {
		case !observation.Publication.Present:
			reason = "unresolved attempt publication is missing"
		case observation.Publication.Status != model.PublicationPublishing:
			reason = "unresolved attempt publication is terminal"
		default:
			reason = "unresolved attempt publication has a newer or incompatible tuple"
		}
		return 0, "", publicationAttemptRecoveryIntegrity(intent, reason)
	}
	diagnostic := publicationAttemptMissingDiagnostic
	if result != nil {
		diagnostic = publicationAttemptUnknownDiagnostic
	}
	return publicationAttemptRecoveryUnresolved, diagnostic, nil
}

func publicationAttemptQuarantineValue(
	intent store.PublicationAttemptIntent,
) model.Publication {
	return model.Publication{
		ID:             intent.PublicationID,
		Board:          intent.Board,
		ChangeSetID:    intent.ChangeSetID,
		Status:         model.PublicationPublishing,
		Mode:           intent.Mode,
		TargetBranch:   intent.TargetBranch,
		Remote:         intent.Remote,
		BaseCommit:     intent.BaseCommit,
		HeadCommit:     intent.HeadCommit,
		DurableRef:     intent.DurableRef,
		ClaimEpoch:     intent.ClaimEpoch,
		ClaimExpiresAt: &intent.ClaimExpiresAt,
		UpdatedAt:      intent.PublicationUpdatedAt,
	}
}

func observeExactPublicationAttempt(
	ctx context.Context,
	reader *store.PublicationRecoveryReader,
	board string,
	expected store.PublicationAttemptIntent,
) (store.PublicationAttemptRecoveryObservation, error) {
	observation, found, supported, err :=
		reader.GetPublicationAttemptRecoveryObservation(
			ctx,
			expected.ID,
		)
	if err != nil {
		return store.PublicationAttemptRecoveryObservation{}, err
	}
	if !supported {
		return store.PublicationAttemptRecoveryObservation{},
			publicationAttemptRecoveryIntegrity(
				expected,
				"attempt schema disappeared during startup recovery",
			)
	}
	if !found {
		return store.PublicationAttemptRecoveryObservation{},
			publicationAttemptRecoveryIntegrity(
				expected,
				"immutable attempt disappeared during startup recovery",
			)
	}
	if err := validatePublicationAttemptObservationIdentity(
		board,
		&expected,
		observation,
	); err != nil {
		return store.PublicationAttemptRecoveryObservation{}, err
	}
	return observation, nil
}

func exactPublicationAttemptRecoveryValidator(
	reader *store.PublicationRecoveryReader,
	board string,
	expected store.PublicationAttemptIntent,
	diagnostic string,
) store.AutomationQuarantineSourceValidator {
	return func(
		ctx context.Context,
		input store.AutomationQuarantineSourceInput,
	) (bool, error) {
		value := publicationAttemptQuarantineValue(expected)
		source := publicationQuarantineSourceWithDiagnostic(
			value,
			diagnostic,
			nil,
		)
		if input.Board != source.Board ||
			input.Kind != source.Kind ||
			input.SourceID != source.SourceID ||
			input.ObservedUpdatedAt != source.ObservedUpdatedAt ||
			input.ObservedClaimEpoch != source.ObservedClaimEpoch ||
			input.DiagnosticCode != source.DiagnosticCode {
			return false, publicationAttemptRecoveryIntegrity(
				expected,
				"quarantine validator received a different source identity",
			)
		}
		observation, err := observeExactPublicationAttempt(
			ctx,
			reader,
			board,
			expected,
		)
		if err != nil {
			return false, err
		}
		state, currentDiagnostic, err :=
			classifyPublicationAttemptRecovery(observation)
		if err != nil {
			return false, err
		}
		return state == publicationAttemptRecoveryUnresolved &&
			currentDiagnostic == diagnostic, nil
	}
}

func persistPublicationAttemptRecovery(
	ctx context.Context,
	session *automationDispatcherSession,
	reader *store.PublicationRecoveryReader,
	board string,
	initial store.PublicationAttemptRecoveryObservation,
) (
	store.AutomationQuarantine,
	publicationAttemptRecoveryDecision,
	bool,
	error,
) {
	expected := initial.Attempt.Intent
	current := initial
	for attempt := 0; attempt < publicationAttemptActivationRetries; attempt++ {
		if err := validatePublicationAttemptObservationIdentity(
			board,
			&expected,
			current,
		); err != nil {
			return store.AutomationQuarantine{},
				publicationAttemptRecoveryDecision{}, false, err
		}
		state, diagnostic, err :=
			classifyPublicationAttemptRecovery(current)
		if err != nil {
			return store.AutomationQuarantine{},
				publicationAttemptRecoveryDecision{}, false, err
		}
		decision := publicationAttemptRecoveryDecision{
			Intent:     expected,
			State:      state,
			Diagnostic: diagnostic,
		}
		if state == publicationAttemptRecoveryResolved {
			return store.AutomationQuarantine{}, decision, false, nil
		}
		gate, err := session.persistPublicationQuarantineWithDiagnostic(
			ctx,
			publicationAttemptQuarantineValue(expected),
			diagnostic,
			exactPublicationAttemptRecoveryValidator(
				reader,
				board,
				expected,
				diagnostic,
			),
		)
		if err == nil {
			return gate, decision, true, nil
		}
		if !errors.Is(err, store.ErrAutomationSourceStale) {
			return store.AutomationQuarantine{},
				publicationAttemptRecoveryDecision{}, false, err
		}
		current, err = observeExactPublicationAttempt(
			ctx,
			reader,
			board,
			expected,
		)
		if err != nil {
			return store.AutomationQuarantine{},
				publicationAttemptRecoveryDecision{}, false, err
		}
	}
	return store.AutomationQuarantine{},
		publicationAttemptRecoveryDecision{},
		false,
		publicationAttemptRecoveryIntegrity(
			expected,
			"attempt ownership tuple did not stabilize",
		)
}

func persistCurrentPublishingQuarantine(
	ctx context.Context,
	session *automationDispatcherSession,
	reader *store.PublicationRecoveryReader,
	value model.Publication,
) (store.AutomationQuarantine, bool, error) {
	const maxTupleAttempts = 3
	for attempt := 0; attempt < maxTupleAttempts; attempt++ {
		observation, attemptBacked, _, lookupErr :=
			reader.GetPublicationAttemptRecoveryObservationForPublication(
				ctx,
				value.ID,
				value.UpdatedAt,
				value.ClaimEpoch,
			)
		if lookupErr != nil {
			return store.AutomationQuarantine{}, false, fmt.Errorf(
				"check publication %s attempt ownership: %w",
				value.ID,
				lookupErr,
			)
		}
		if attemptBacked {
			if err := validatePublicationAttemptObservationIdentity(
				value.Board,
				nil,
				observation,
			); err != nil {
				return store.AutomationQuarantine{}, false, err
			}
			return store.AutomationQuarantine{}, false,
				publicationAttemptRecoveryIntegrity(
					observation.Attempt.Intent,
					"legacy refresh encountered an attempt-backed tuple",
				)
		}
		gate, err := session.persistPublicationQuarantine(
			ctx,
			value,
			reader.ValidatePublishingAutomationSource,
		)
		if err == nil {
			return gate, true, nil
		}
		if !errors.Is(err, store.ErrAutomationSourceStale) {
			return store.AutomationQuarantine{}, false, err
		}
		current, found, refreshErr := reader.GetPublicationForRecovery(
			ctx,
			value.ID,
		)
		if refreshErr != nil {
			return store.AutomationQuarantine{}, false, fmt.Errorf(
				"refresh stale publication %s: %w",
				value.ID,
				refreshErr,
			)
		}
		if !found || current.Status != model.PublicationPublishing {
			return store.AutomationQuarantine{}, false, nil
		}
		if current.Board != value.Board {
			return store.AutomationQuarantine{}, false, fmt.Errorf(
				"publication %s changed board from %s to %s",
				value.ID,
				value.Board,
				current.Board,
			)
		}
		value = current
	}
	return store.AutomationQuarantine{}, false, fmt.Errorf(
		"publication %s ownership tuple did not stabilize",
		value.ID,
	)
}

func publicationAttemptCursorAdvances(
	previous store.PublicationAttemptRecoveryCursor,
	next store.PublicationAttemptRecoveryCursor,
) bool {
	return next.StartedAt > previous.StartedAt ||
		next.StartedAt == previous.StartedAt && next.ID > previous.ID
}

func reconcileAttemptBackedLegacyPublishing(
	board string,
	observation store.PublicationAttemptRecoveryObservation,
	previous publicationAttemptRecoveryDecision,
	seen bool,
) error {
	var expected *store.PublicationAttemptIntent
	if seen {
		expected = &previous.Intent
	}
	if err := validatePublicationAttemptObservationIdentity(
		board,
		expected,
		observation,
	); err != nil {
		return err
	}
	state, diagnostic, err :=
		classifyPublicationAttemptRecovery(observation)
	if err != nil {
		return err
	}
	result := observation.Attempt.Result
	known := result != nil &&
		result.Outcome != store.PublicationAttemptUnknown
	if !seen {
		if state == publicationAttemptRecoveryResolved && known {
			// Known results are intentionally absent from the raw unresolved
			// attempt pass. A concurrent known Finish that moved the
			// publication away is the only safe unobserved transition.
			return nil
		}
		return publicationAttemptRecoveryIntegrity(
			observation.Attempt.Intent,
			"legacy pass found an attempt absent from the attempt pass",
		)
	}
	switch state {
	case publicationAttemptRecoveryUnresolved:
		if previous.State != publicationAttemptRecoveryUnresolved ||
			previous.Diagnostic != diagnostic {
			return publicationAttemptRecoveryIntegrity(
				observation.Attempt.Intent,
				"legacy pass disagrees with the unresolved attempt decision",
			)
		}
	case publicationAttemptRecoveryResolved:
		if previous.State == publicationAttemptRecoveryResolved {
			return nil
		}
		if previous.State == publicationAttemptRecoveryUnresolved &&
			(known || observation.Attempt.RecoveryReceipt != nil) {
			// Activation and the legacy scan use separate snapshots. A known
			// Finish or exact operator receipt may legitimately resolve the
			// source between those snapshots.
			return nil
		}
		return publicationAttemptRecoveryIntegrity(
			observation.Attempt.Intent,
			"legacy pass observed an unexplained attempt resolution",
		)
	default:
		return publicationAttemptRecoveryIntegrity(
			observation.Attempt.Intent,
			"legacy pass returned an invalid attempt decision",
		)
	}
	return nil
}

// quarantineUnconfirmedPublishingOwnership scans every active and archived
// board twice. The first bounded keyset pass adjudicates the immutable attempt
// ledger. Only then may the legacy Publishing pass quarantine rows without an
// exact attempt.
func quarantineUnconfirmedPublishingOwnership(
	manager *boards.Manager,
	session *automationDispatcherSession,
) error {
	if manager == nil {
		return failPublishingOwnershipScan(
			session,
			errors.New("board manager is required"),
		)
	}
	if session == nil {
		return errors.New("automation dispatcher session is required")
	}
	scanContext, cancel := context.WithTimeout(
		context.Background(),
		publicationStartupOwnershipScanTimeout,
	)
	defer cancel()
	metadata, err := manager.ListMetadata(scanContext, true)
	if err != nil {
		return failPublishingOwnershipScan(session, err)
	}
	var latestGate store.AutomationQuarantine
	quarantined := false
	decisions := make(
		[]map[string]publicationAttemptRecoveryDecision,
		len(metadata),
	)

	// Pass one: consume every raw unresolved attempt page before inspecting
	// legacy Publishing rows in any database.
	for boardIndex, board := range metadata {
		if err := scanContext.Err(); err != nil {
			return failPublishingOwnershipScan(session, err)
		}
		opened, err := manager.OpenListedPublicationRecoveryReader(
			scanContext,
			board,
		)
		if err != nil {
			return failPublishingOwnershipScan(
				session,
				fmt.Errorf("open board %s publication store: %w", board.Slug, err),
			)
		}
		failReader := func(cause error) error {
			return failPublishingOwnershipScan(
				session,
				errors.Join(cause, opened.Close()),
			)
		}
		decisions[boardIndex] = make(
			map[string]publicationAttemptRecoveryDecision,
		)
		cursor := store.PublicationAttemptRecoveryCursor{}
		for {
			page, next, supported, pageErr :=
				opened.ListPublicationAttemptRecoveryPage(
					scanContext,
					store.PublicationAttemptFilter{
						AfterStartedAt: cursor.StartedAt,
						AfterID:        cursor.ID,
						Limit:          100,
					},
				)
			if pageErr != nil {
				return failReader(
					fmt.Errorf(
						"list board %s publication attempt records: %w",
						board.Slug,
						pageErr,
					),
				)
			}
			if !supported {
				if len(page) != 0 ||
					next != (store.PublicationAttemptRecoveryCursor{}) ||
					cursor != (store.PublicationAttemptRecoveryCursor{}) {
					return failReader(errors.New(
						"unsupported publication attempt reader returned a partial page",
					))
				}
				break
			}
			for _, snapshot := range page {
				if err := validatePublicationAttemptObservationIdentity(
					board.Slug,
					nil,
					snapshot,
				); err != nil {
					return failReader(err)
				}
				exact, err := observeExactPublicationAttempt(
					scanContext,
					opened,
					board.Slug,
					snapshot.Attempt.Intent,
				)
				if err != nil {
					return failReader(err)
				}
				gate, decision, persisted, err :=
					persistPublicationAttemptRecovery(
						scanContext,
						session,
						opened,
						board.Slug,
						exact,
					)
				if err != nil {
					return failReader(err)
				}
				if _, duplicate := decisions[boardIndex][decision.Intent.ID]; duplicate {
					return failReader(
						publicationAttemptRecoveryIntegrity(
							decision.Intent,
							"attempt appeared more than once in its raw keyset",
						),
					)
				}
				decisions[boardIndex][decision.Intent.ID] = decision
				if persisted {
					latestGate = gate
					quarantined = true
				}
			}
			if next == (store.PublicationAttemptRecoveryCursor{}) {
				break
			}
			if len(page) == 0 ||
				!publicationAttemptCursorAdvances(cursor, next) ||
				next.StartedAt !=
					page[len(page)-1].Attempt.Intent.StartedAt ||
				next.ID != page[len(page)-1].Attempt.Intent.ID {
				return failReader(
					fmt.Errorf(
						"board %s publication attempt cursor did not advance",
						board.Slug,
					),
				)
			}
			cursor = next
		}
		if err := opened.Close(); err != nil {
			return failPublishingOwnershipScan(
				session,
				fmt.Errorf("close board %s publication store: %w", board.Slug, err),
			)
		}
	}

	// Pass two: inspect legacy Publishing rows. An exact attempt lookup includes
	// known results and must agree with the first pass before the row is skipped.
	for boardIndex, board := range metadata {
		if err := scanContext.Err(); err != nil {
			return failPublishingOwnershipScan(session, err)
		}
		opened, err := manager.OpenListedPublicationRecoveryReader(
			scanContext,
			board,
		)
		if err != nil {
			return failPublishingOwnershipScan(
				session,
				fmt.Errorf("open board %s publication store: %w", board.Slug, err),
			)
		}
		failReader := func(cause error) error {
			return failPublishingOwnershipScan(
				session,
				errors.Join(cause, opened.Close()),
			)
		}
		cursor := ""
		for {
			page, next, pageErr := opened.ListPublishingAfter(
				scanContext,
				cursor,
			)
			if pageErr != nil {
				return failReader(
					fmt.Errorf(
						"list board %s publishing records: %w",
						board.Slug,
						pageErr,
					),
				)
			}
			for _, publication := range page {
				if publication.Board != board.Slug {
					return failReader(fmt.Errorf(
						"board %s returned publication %s for board %s",
						board.Slug,
						publication.ID,
						publication.Board,
					))
				}
				observation, attemptBacked, _, lookupErr :=
					opened.GetPublicationAttemptRecoveryObservationForPublication(
						scanContext,
						publication.ID,
						publication.UpdatedAt,
						publication.ClaimEpoch,
					)
				if lookupErr != nil {
					return failReader(fmt.Errorf(
						"lookup board %s publication %s attempt: %w",
						board.Slug,
						publication.ID,
						lookupErr,
					))
				}
				if attemptBacked {
					previous, seen := decisions[boardIndex][observation.Attempt.Intent.ID]
					if err := reconcileAttemptBackedLegacyPublishing(
						board.Slug,
						observation,
						previous,
						seen,
					); err != nil {
						return failReader(err)
					}
					continue
				}
				gate, persisted, err :=
					persistCurrentPublishingQuarantine(
						scanContext,
						session,
						opened,
						publication,
					)
				if err != nil {
					return failReader(err)
				}
				if persisted {
					latestGate = gate
					quarantined = true
				}
			}
			if next == "" {
				break
			}
			if next <= cursor {
				return failReader(fmt.Errorf(
					"board %s publication cursor did not advance",
					board.Slug,
				))
			}
			cursor = next
		}
		if err := opened.Close(); err != nil {
			return failPublishingOwnershipScan(
				session,
				fmt.Errorf("close board %s publication store: %w", board.Slug, err),
			)
		}
	}
	if quarantined {
		return session.observeGate(latestGate)
	}
	return nil
}
