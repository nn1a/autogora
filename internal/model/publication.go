package model

import "encoding/json"

// PublicationMode describes the deterministic host-side operation selected
// when a finalizer's immutable change set becomes ready for publication.
type PublicationMode string

const (
	PublicationModeManual      PublicationMode = "manual"
	PublicationModeLocalFF     PublicationMode = "local_ff"
	PublicationModePullRequest PublicationMode = "pull_request"
)

var PublicationModes = []PublicationMode{
	PublicationModeManual,
	PublicationModeLocalFF,
	PublicationModePullRequest,
}

type PublicationStatus string

const (
	PublicationPending          PublicationStatus = "pending"
	PublicationAwaitingApproval PublicationStatus = "awaiting_approval"
	PublicationPublishing       PublicationStatus = "publishing"
	PublicationPublished        PublicationStatus = "published"
	PublicationNoChange         PublicationStatus = "no_change"
	PublicationFailed           PublicationStatus = "failed"
	PublicationSuperseded       PublicationStatus = "superseded"
)

var PublicationStatuses = []PublicationStatus{
	PublicationPending,
	PublicationAwaitingApproval,
	PublicationPublishing,
	PublicationPublished,
	PublicationNoChange,
	PublicationFailed,
	PublicationSuperseded,
}

func ValidPublicationMode(value PublicationMode) bool {
	for _, candidate := range PublicationModes {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidPublicationStatus(value PublicationStatus) bool {
	for _, candidate := range PublicationStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

// Publication is the durable handoff between task completion and host-side
// publication. PolicySnapshot and SourceSnapshot are immutable audit records;
// explicit fields remain convenient query and execution inputs.
type Publication struct {
	ID              string            `json:"id"`
	Board           string            `json:"board"`
	TaskID          string            `json:"taskId"`
	RunID           string            `json:"runId"`
	ChangeSetID     string            `json:"changeSetId"`
	Status          PublicationStatus `json:"status"`
	Mode            PublicationMode   `json:"mode"`
	TargetBranch    string            `json:"targetBranch"`
	Remote          string            `json:"remote"`
	RequireApproval bool              `json:"requireApproval"`
	RepositoryPath  string            `json:"repositoryPath"`
	WorktreePath    string            `json:"worktreePath"`
	BaseCommit      string            `json:"baseCommit"`
	HeadCommit      string            `json:"headCommit"`
	DurableRef      string            `json:"durableRef"`
	PolicySnapshot  json.RawMessage   `json:"policySnapshot"`
	SourceSnapshot  json.RawMessage   `json:"sourceSnapshot"`
	URL             *string           `json:"url"`
	Error           *string           `json:"error"`
	ClaimToken      string            `json:"-"`
	ClaimExpiresAt  *string           `json:"claimExpiresAt"`
	ApprovedAt      *string           `json:"approvedAt"`
	PublishedAt     *string           `json:"publishedAt"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
}
