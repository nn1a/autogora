package model

import "encoding/json"

type BoardGraphState struct {
	Board     string `json:"board"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updatedAt"`
}

type CoordinationTrigger string

const (
	CoordinationTriggerRepeatedBlock       CoordinationTrigger = "repeated_block"
	CoordinationTriggerRetryExhausted      CoordinationTrigger = "retry_exhausted"
	CoordinationTriggerGraphStalled        CoordinationTrigger = "graph_stalled"
	CoordinationTriggerIntegrationConflict CoordinationTrigger = "integration_conflict"
	CoordinationTriggerAgentExhausted      CoordinationTrigger = "agent_exhausted"
)

var CoordinationTriggers = []CoordinationTrigger{
	CoordinationTriggerRepeatedBlock,
	CoordinationTriggerRetryExhausted,
	CoordinationTriggerGraphStalled,
	CoordinationTriggerIntegrationConflict,
	CoordinationTriggerAgentExhausted,
}

type CoordinationSeverity string

const (
	CoordinationSeverityInfo     CoordinationSeverity = "info"
	CoordinationSeverityWarning  CoordinationSeverity = "warning"
	CoordinationSeverityError    CoordinationSeverity = "error"
	CoordinationSeverityCritical CoordinationSeverity = "critical"
)

var CoordinationSeverities = []CoordinationSeverity{
	CoordinationSeverityInfo,
	CoordinationSeverityWarning,
	CoordinationSeverityError,
	CoordinationSeverityCritical,
}

type CoordinationIncidentStatus string

const (
	CoordinationIncidentOpen             CoordinationIncidentStatus = "open"
	CoordinationIncidentCoordinating     CoordinationIncidentStatus = "coordinating"
	CoordinationIncidentAwaitingApproval CoordinationIncidentStatus = "awaiting_approval"
	CoordinationIncidentApplying         CoordinationIncidentStatus = "applying"
	CoordinationIncidentResolved         CoordinationIncidentStatus = "resolved"
	CoordinationIncidentDismissed        CoordinationIncidentStatus = "dismissed"
	CoordinationIncidentFailed           CoordinationIncidentStatus = "failed"
)

var CoordinationIncidentStatuses = []CoordinationIncidentStatus{
	CoordinationIncidentOpen,
	CoordinationIncidentCoordinating,
	CoordinationIncidentAwaitingApproval,
	CoordinationIncidentApplying,
	CoordinationIncidentResolved,
	CoordinationIncidentDismissed,
	CoordinationIncidentFailed,
}

type CoordinationProposalStatus string

const (
	CoordinationProposalDraft            CoordinationProposalStatus = "draft"
	CoordinationProposalValidating       CoordinationProposalStatus = "validating"
	CoordinationProposalValidated        CoordinationProposalStatus = "validated"
	CoordinationProposalAwaitingApproval CoordinationProposalStatus = "awaiting_approval"
	CoordinationProposalApproved         CoordinationProposalStatus = "approved"
	CoordinationProposalRejected         CoordinationProposalStatus = "rejected"
	CoordinationProposalSuperseded       CoordinationProposalStatus = "superseded"
	CoordinationProposalApplying         CoordinationProposalStatus = "applying"
	CoordinationProposalApplied          CoordinationProposalStatus = "applied"
	CoordinationProposalFailed           CoordinationProposalStatus = "failed"
)

var CoordinationProposalStatuses = []CoordinationProposalStatus{
	CoordinationProposalDraft,
	CoordinationProposalValidating,
	CoordinationProposalValidated,
	CoordinationProposalAwaitingApproval,
	CoordinationProposalApproved,
	CoordinationProposalRejected,
	CoordinationProposalSuperseded,
	CoordinationProposalApplying,
	CoordinationProposalApplied,
	CoordinationProposalFailed,
}

func ValidCoordinationTrigger(value CoordinationTrigger) bool {
	for _, candidate := range CoordinationTriggers {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidCoordinationSeverity(value CoordinationSeverity) bool {
	for _, candidate := range CoordinationSeverities {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidCoordinationIncidentStatus(value CoordinationIncidentStatus) bool {
	for _, candidate := range CoordinationIncidentStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

func ValidCoordinationProposalStatus(value CoordinationProposalStatus) bool {
	for _, candidate := range CoordinationProposalStatuses {
		if candidate == value {
			return true
		}
	}
	return false
}

type CoordinationIncident struct {
	ID             string                     `json:"id"`
	Board          string                     `json:"board"`
	RootTaskID     *string                    `json:"rootTaskId"`
	TaskID         *string                    `json:"taskId"`
	Trigger        CoordinationTrigger        `json:"trigger"`
	Severity       CoordinationSeverity       `json:"severity"`
	Status         CoordinationIncidentStatus `json:"status"`
	GraphRevision  int64                      `json:"graphRevision"`
	Summary        string                     `json:"summary"`
	Details        json.RawMessage            `json:"details"`
	ClaimToken     string                     `json:"-"`
	ClaimExpiresAt *string                    `json:"claimExpiresAt"`
	CreatedAt      string                     `json:"createdAt"`
	UpdatedAt      string                     `json:"updatedAt"`
}

type CoordinationProposal struct {
	ID                    string                     `json:"id"`
	IncidentID            string                     `json:"incidentId"`
	CoordinatorAgent      string                     `json:"coordinatorAgent"`
	CoordinatorModel      string                     `json:"coordinatorModel"`
	CoordinatorProvider   string                     `json:"coordinatorProvider"`
	Status                CoordinationProposalStatus `json:"status"`
	ExpectedGraphRevision int64                      `json:"expectedGraphRevision"`
	Summary               string                     `json:"summary"`
	Rationale             string                     `json:"rationale"`
	Actions               json.RawMessage            `json:"actions"`
	ValidationErrors      json.RawMessage            `json:"validationErrors"`
	CreatedAt             string                     `json:"createdAt"`
	UpdatedAt             string                     `json:"updatedAt"`
	AppliedAt             *string                    `json:"appliedAt"`
}
