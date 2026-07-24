package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

const recoveryFingerprintVersion = 1

type recoveryAttachmentFingerprint struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	MediaType *string `json:"mediaType"`
	Size      *int64  `json:"size"`
	SHA256    *string `json:"sha256"`
	Path      *string `json:"path"`
	URL       *string `json:"url"`
}

type recoveryTaskFingerprint struct {
	Version            int                             `json:"version"`
	ID                 string                          `json:"id"`
	Board              string                          `json:"board"`
	Tenant             *string                         `json:"tenant"`
	IdempotencyKey     *string                         `json:"idempotencyKey"`
	Title              string                          `json:"title"`
	Body               string                          `json:"body"`
	WorkflowRole       model.WorkflowRole              `json:"workflowRole"`
	Workspace          *string                         `json:"workspace"`
	WorkspaceKind      model.WorkspaceKind             `json:"workspaceKind"`
	Branch             *string                         `json:"branch"`
	MaxRuntimeSeconds  *int                            `json:"maxRuntimeSeconds"`
	Skills             []string                        `json:"skills"`
	GoalMode           bool                            `json:"goalMode"`
	GoalMaxTurns       int                             `json:"goalMaxTurns"`
	WorkflowTemplateID *string                         `json:"workflowTemplateId"`
	CurrentStepKey     *string                         `json:"currentStepKey"`
	Attachments        []recoveryAttachmentFingerprint `json:"attachments"`
}

type recoveryChangeSetFingerprint struct {
	ID             string   `json:"id"`
	RunID          string   `json:"runId"`
	TaskID         string   `json:"taskId"`
	RepositoryPath string   `json:"repositoryPath"`
	BaseCommit     string   `json:"baseCommit"`
	HeadCommit     string   `json:"headCommit"`
	DurableRef     string   `json:"durableRef"`
	State          string   `json:"state"`
	ChangedFiles   []string `json:"changedFiles"`
}

type recoveryPrerequisiteItemFingerprint struct {
	PrerequisiteID string                        `json:"prerequisiteId"`
	DependentID    string                        `json:"dependentId"`
	SatisfiedAt    string                        `json:"satisfiedAt"`
	SatisfiedRunID *string                       `json:"satisfiedRunId"`
	ChangeSet      *recoveryChangeSetFingerprint `json:"changeSet"`
}

type recoveryPrerequisitesFingerprint struct {
	Version int                                   `json:"version"`
	Items   []recoveryPrerequisiteItemFingerprint `json:"items"`
}

func normalizedFingerprintStrings(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := unique[value]; exists {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	if result == nil {
		return []string{}
	}
	return result
}

func recoveryFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("recovery fingerprint contains a non-JSON value: " + err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// recoveryTaskSpecFingerprint deliberately excludes routing and lifecycle
// fields such as assignee, runtime, priority, status, claim, heartbeat, and
// updated_at. Those may change while a fallback run still performs the exact
// same work. Requirement, workspace, attachment, and goal changes invalidate
// automatic adoption.
func recoveryTaskSpecFingerprint(detail model.TaskDetail) string {
	task := detail.Task
	attachments := make([]recoveryAttachmentFingerprint, 0, len(detail.Attachments))
	for _, attachment := range detail.Attachments {
		attachments = append(attachments, recoveryAttachmentFingerprint{
			ID: attachment.ID, Kind: attachment.Kind, Name: attachment.Name,
			MediaType: attachment.MediaType, Size: attachment.Size,
			SHA256: attachment.SHA256, Path: attachment.Path, URL: attachment.URL,
		})
	}
	sort.Slice(attachments, func(left, right int) bool {
		if attachments[left].ID != attachments[right].ID {
			return attachments[left].ID < attachments[right].ID
		}
		if attachments[left].Kind != attachments[right].Kind {
			return attachments[left].Kind < attachments[right].Kind
		}
		return attachments[left].Name < attachments[right].Name
	})
	return recoveryFingerprint(recoveryTaskFingerprint{
		Version: recoveryFingerprintVersion,
		ID:      task.ID, Board: task.Board, Tenant: task.Tenant,
		IdempotencyKey: task.IdempotencyKey, Title: task.Title, Body: task.Body,
		WorkflowRole: task.WorkflowRole, Workspace: task.Workspace,
		WorkspaceKind: task.WorkspaceKind, Branch: task.Branch,
		MaxRuntimeSeconds: task.MaxRuntimeSeconds,
		Skills:            normalizedFingerprintStrings(task.Skills),
		GoalMode:          task.GoalMode, GoalMaxTurns: task.GoalMaxTurns,
		WorkflowTemplateID: task.WorkflowTemplateID,
		CurrentStepKey:     task.CurrentStepKey, Attachments: attachments,
	})
}

func recoveryPrerequisiteFingerprint(handoffs []model.PrerequisiteHandoff) string {
	items := make([]recoveryPrerequisiteItemFingerprint, 0, len(handoffs))
	for _, handoff := range handoffs {
		item := recoveryPrerequisiteItemFingerprint{
			PrerequisiteID: handoff.PrerequisiteID,
			DependentID:    handoff.DependentID,
			SatisfiedAt:    handoff.SatisfiedAt,
			SatisfiedRunID: handoff.SatisfiedRunID,
		}
		if change := handoff.ChangeSet; change != nil {
			item.ChangeSet = &recoveryChangeSetFingerprint{
				ID: change.ID, RunID: change.RunID, TaskID: change.TaskID,
				RepositoryPath: change.RepositoryPath,
				BaseCommit:     change.BaseCommit, HeadCommit: change.HeadCommit,
				DurableRef: change.DurableRef, State: change.State,
				ChangedFiles: normalizedFingerprintStrings(change.ChangedFiles),
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].PrerequisiteID != items[right].PrerequisiteID {
			return items[left].PrerequisiteID < items[right].PrerequisiteID
		}
		if items[left].DependentID != items[right].DependentID {
			return items[left].DependentID < items[right].DependentID
		}
		leftRun, rightRun := "", ""
		if items[left].SatisfiedRunID != nil {
			leftRun = *items[left].SatisfiedRunID
		}
		if items[right].SatisfiedRunID != nil {
			rightRun = *items[right].SatisfiedRunID
		}
		if leftRun != rightRun {
			return leftRun < rightRun
		}
		return items[left].SatisfiedAt < items[right].SatisfiedAt
	})
	return recoveryFingerprint(recoveryPrerequisitesFingerprint{
		Version: recoveryFingerprintVersion,
		Items:   items,
	})
}

func recoverySemanticFingerprints(detail model.TaskDetail) (string, string) {
	return recoveryTaskSpecFingerprint(detail),
		recoveryPrerequisiteFingerprint(detail.PrerequisiteHandoffs)
}
