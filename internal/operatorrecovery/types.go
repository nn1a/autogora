package operatorrecovery

import (
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const PublicationSourceKind = "publication"

// PublicationOutcome is the operator's observed result of an external
// publication attempt whose process ownership could not be proven.
type PublicationOutcome string

const (
	PublicationOutcomePublished  PublicationOutcome = "published"
	PublicationOutcomeFailed     PublicationOutcome = "failed"
	PublicationOutcomeSuperseded PublicationOutcome = "superseded"
)

// Confirmation is the strict, portable operator contract shared by the CLI
// and Dashboard. It contains no process, publication-claim, session, permit,
// database, or filesystem credentials.
type Confirmation struct {
	Generation            int64                `json:"generation"`
	Actor                 string               `json:"actor"`
	Reason                string               `json:"reason"`
	HelpersStopped        bool                 `json:"helpersStopped"`
	ExternalWritesStopped bool                 `json:"externalWritesStopped"`
	Sources               []ConfirmationSource `json:"sources"`
}

type ConfirmationSource struct {
	SourceKey          string                            `json:"sourceKey"`
	Board              string                            `json:"board"`
	Kind               string                            `json:"kind"`
	SourceID           string                            `json:"sourceId"`
	ObservedUpdatedAt  string                            `json:"observedUpdatedAt"`
	ObservedClaimEpoch string                            `json:"observedClaimEpoch"`
	DiagnosticCode     string                            `json:"diagnosticCode"`
	Disposition        store.AutomationSourceDisposition `json:"disposition"`
	Outcome            PublicationOutcome                `json:"outcome,omitempty"`
	ResultURL          *string                           `json:"resultUrl,omitempty"`
}

// Status is a secret-safe view of the global authority and its exact current
// recovery source set. Pending is present after confirmation phase one so a
// caller can reconstruct and replay the same confirmation after a crash.
type Status struct {
	Gate                       store.AutomationQuarantine `json:"gate"`
	Sources                    []StatusSource             `json:"sources"`
	Pending                    *PendingConfirmation       `json:"pending,omitempty"`
	Prepared                   *PreparedConfirmation      `json:"prepared,omitempty"`
	UnacknowledgedSessionCount int                        `json:"unacknowledgedSessionCount"`
}

type PendingConfirmation struct {
	ResolvedGeneration    int64  `json:"resolvedGeneration"`
	Actor                 string `json:"actor"`
	Reason                string `json:"reason"`
	HelpersStopped        bool   `json:"helpersStopped"`
	ExternalWritesStopped bool   `json:"externalWritesStopped"`
}

// PreparedConfirmation preserves the immutable actor/reason constraint when
// one or more board-local receipts committed before authority phase one.
type PreparedConfirmation struct {
	Actor                       string `json:"actor"`
	Reason                      string `json:"reason"`
	RecoveredPublicationSources int    `json:"recoveredPublicationSources"`
}

type StatusSource struct {
	SourceKey          string                             `json:"sourceKey"`
	FirstGeneration    int64                              `json:"firstGeneration"`
	Board              string                             `json:"board"`
	Kind               string                             `json:"kind"`
	SourceID           string                             `json:"sourceId"`
	ObservedUpdatedAt  string                             `json:"observedUpdatedAt"`
	ObservedClaimEpoch string                             `json:"observedClaimEpoch"`
	DiagnosticCode     string                             `json:"diagnosticCode"`
	Disposition        string                             `json:"disposition"`
	ResolvedGeneration *int64                             `json:"resolvedGeneration,omitempty"`
	Outcome            PublicationOutcome                 `json:"outcome,omitempty"`
	ResultURL          *string                            `json:"resultUrl,omitempty"`
	ReceiptDisposition *store.AutomationSourceDisposition `json:"receiptDisposition,omitempty"`
	RecoveredAt        *string                            `json:"recoveredAt,omitempty"`
	ResultUpdatedAt    *string                            `json:"resultUpdatedAt,omitempty"`
	Publication        *PublicationStorageStatus          `json:"publication,omitempty"`
}

// PublicationStorageStatus summarizes inventory evidence without disclosing a
// database, repository, worktree, archive-directory, or attachment path.
type PublicationStorageStatus struct {
	MatchCount        int                     `json:"matchCount"`
	Archived          *bool                   `json:"archived,omitempty"`
	CurrentStatus     model.PublicationStatus `json:"currentStatus,omitempty"`
	CurrentUpdatedAt  string                  `json:"currentUpdatedAt,omitempty"`
	CurrentClaimEpoch int64                   `json:"currentClaimEpoch,omitempty"`
	HasReceipt        bool                    `json:"hasReceipt"`
}

type ConfirmationResult struct {
	Gate         store.AutomationQuarantine `json:"gate"`
	Cleared      bool                       `json:"cleared"`
	Publications []PublicationResult        `json:"publications"`
}

// PublicationResult intentionally exposes only operator-recovery state. The
// underlying model.Publication also contains repository and worktree paths,
// and its in-memory form can contain a claim token, so it must not cross this
// boundary.
type PublicationResult struct {
	SourceKey     string                            `json:"sourceKey"`
	Board         string                            `json:"board"`
	PublicationID string                            `json:"publicationId"`
	Status        model.PublicationStatus           `json:"status"`
	UpdatedAt     string                            `json:"updatedAt"`
	ClaimEpoch    int64                             `json:"claimEpoch"`
	Outcome       PublicationOutcome                `json:"outcome"`
	Disposition   store.AutomationSourceDisposition `json:"disposition"`
	ResultURL     *string                           `json:"resultUrl,omitempty"`
	Changed       bool                              `json:"changed"`
	RecoveredAt   string                            `json:"recoveredAt"`
	Present       bool                              `json:"present"`
}
