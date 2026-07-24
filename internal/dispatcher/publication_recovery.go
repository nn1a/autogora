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
	publicationStartupOwnershipScanTimeout    = 30 * time.Second
)

func publicationQuarantineSource(
	value model.Publication,
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
		DiagnosticCode:     publicationOwnershipUnconfirmedDiagnostic,
		ValidateCurrent:    validate,
	}
}

func (s *automationDispatcherSession) persistPublicationQuarantine(
	ctx context.Context,
	value model.Publication,
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
	gate, _, err := s.authority.ActivateAutomationQuarantine(
		ctx,
		publicationQuarantineSource(value, validate),
	)
	if err != nil {
		return store.AutomationQuarantine{}, fmt.Errorf(
			"persist publication %s quarantine source: %w",
			value.ID,
			err,
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

// quarantinePublicationTeardown records the exact publication attempt before
// the caller can transition it to Failed. The session latch withholds its ACK,
// and observeGate cancels every remaining dispatcher queue.
func (s *automationDispatcherSession) quarantinePublicationTeardown(
	opened *store.Store,
	value model.Publication,
) error {
	if s == nil {
		return errors.New(
			"automation dispatcher session is required after unconfirmed publication teardown",
		)
	}
	if opened == nil {
		return errors.New(
			"publication Store is required after unconfirmed publication teardown",
		)
	}
	s.unconfirmed.Store(true)
	operationContext, cancel := context.WithTimeout(
		context.Background(),
		automationSessionOperationLimit,
	)
	gate, err := s.persistPublicationQuarantine(
		operationContext,
		value,
		opened.ValidatePublishingAutomationSource,
	)
	cancel()
	if errors.Is(err, store.ErrAutomationSourceStale) {
		return errors.Join(
			err,
			s.activateUnconfirmedQuarantine(automationTeardownDiagnostic),
		)
	}
	if err != nil {
		s.recordFailure(err)
		return err
	}
	s.sourceSaved.Store(true)
	return s.observeGate(gate)
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

func persistCurrentPublishingQuarantine(
	ctx context.Context,
	session *automationDispatcherSession,
	reader *store.PublicationRecoveryReader,
	value model.Publication,
) (store.AutomationQuarantine, bool, error) {
	const maxTupleAttempts = 3
	for attempt := 0; attempt < maxTupleAttempts; attempt++ {
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

// quarantineUnconfirmedPublishingOwnership scans every active and archived
// board with a bounded ID keyset. Any Publishing row belongs to an older
// process because the current global session has not started a queue yet.
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
	found := false
	for _, board := range metadata {
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
		cursor := ""
		for {
			page, next, pageErr := opened.ListPublishingAfter(
				scanContext,
				cursor,
			)
			if pageErr != nil {
				_ = opened.Close()
				return failPublishingOwnershipScan(
					session,
					fmt.Errorf(
						"list board %s publishing records: %w",
						board.Slug,
						pageErr,
					),
				)
			}
			for _, publication := range page {
				if publication.Board != board.Slug {
					_ = opened.Close()
					return failPublishingOwnershipScan(
						session,
						fmt.Errorf(
							"board %s returned publication %s for board %s",
							board.Slug,
							publication.ID,
							publication.Board,
						),
					)
				}
				// Quarantine identity intentionally follows the durable
				// publication tuple required by operator recovery. If an
				// archived database was cloned byte-for-byte, both incarnations
				// conservatively collapse to the same source key. Recovery must
				// scan every active and archived database and reject the source
				// unless this tuple has exactly one storage match.
				var quarantined bool
				latestGate, quarantined, err = persistCurrentPublishingQuarantine(
					scanContext,
					session,
					opened,
					publication,
				)
				if err != nil {
					_ = opened.Close()
					return failPublishingOwnershipScan(session, err)
				}
				found = found || quarantined
			}
			if next == "" {
				break
			}
			if next <= cursor {
				_ = opened.Close()
				return failPublishingOwnershipScan(
					session,
					fmt.Errorf(
						"board %s publication cursor did not advance",
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
	if found {
		return session.observeGate(latestGate)
	}
	return nil
}
