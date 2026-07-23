package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

type ActionKind string

const (
	ActionSetRoute         ActionKind = "set_route"
	ActionUpdatePriority   ActionKind = "update_priority"
	ActionUnblockTask      ActionKind = "unblock_task"
	ActionMoveToTriage     ActionKind = "move_to_triage"
	ActionAddDependency    ActionKind = "add_dependency"
	ActionRemoveDependency ActionKind = "remove_dependency"
	ActionCreateTask       ActionKind = "create_task"
)

type ActionRisk string

const (
	ActionRiskConditional ActionRisk = "conditional"
	ActionRiskApproval    ActionRisk = "approval_required"
)

type TaskDraft struct {
	Key           string             `json:"key"`
	Title         string             `json:"title"`
	Body          string             `json:"body"`
	Assignee      string             `json:"assignee"`
	Runtime       model.Runtime      `json:"runtime"`
	WorkflowRole  model.WorkflowRole `json:"workflowRole"`
	Priority      int                `json:"priority"`
	Prerequisites []string           `json:"prerequisites"`
	Dependents    []string           `json:"dependents"`
	ParentTaskID  string             `json:"parentTaskId,omitempty"`
}

type TaskVersionMap map[string]string

type taskVersionExpectation struct {
	TaskID    string `json:"taskId"`
	UpdatedAt string `json:"updatedAt"`
}

func (m *TaskVersionMap) UnmarshalJSON(value []byte) error {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" {
		*m = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var versions map[string]string
		if err := json.Unmarshal(value, &versions); err != nil {
			return err
		}
		*m = versions
		return nil
	}
	var entries []taskVersionExpectation
	if err := json.Unmarshal(value, &entries); err != nil {
		return err
	}
	versions := make(TaskVersionMap, len(entries))
	for _, entry := range entries {
		entry.TaskID = strings.TrimSpace(entry.TaskID)
		entry.UpdatedAt = strings.TrimSpace(entry.UpdatedAt)
		if entry.TaskID == "" || entry.UpdatedAt == "" {
			return errors.New("expectedTaskVersions entries require taskId and updatedAt")
		}
		if _, exists := versions[entry.TaskID]; exists {
			return fmt.Errorf("expectedTaskVersions contains duplicate taskId %s", entry.TaskID)
		}
		versions[entry.TaskID] = entry.UpdatedAt
	}
	*m = versions
	return nil
}

type Action struct {
	Kind                          ActionKind     `json:"kind"`
	TaskID                        string         `json:"taskId,omitempty"`
	ExpectedUpdatedAt             string         `json:"expectedUpdatedAt,omitempty"`
	Assignee                      string         `json:"assignee,omitempty"`
	Runtime                       model.Runtime  `json:"runtime,omitempty"`
	Priority                      *int           `json:"priority,omitempty"`
	PrerequisiteID                string         `json:"prerequisiteId,omitempty"`
	ExpectedPrerequisiteUpdatedAt string         `json:"expectedPrerequisiteUpdatedAt,omitempty"`
	DependentID                   string         `json:"dependentId,omitempty"`
	ExpectedDependentUpdatedAt    string         `json:"expectedDependentUpdatedAt,omitempty"`
	Task                          *TaskDraft     `json:"task,omitempty"`
	ExpectedTaskVersions          TaskVersionMap `json:"expectedTaskVersions,omitempty"`
	Reason                        string         `json:"reason"`
}

func (a Action) Risk() ActionRisk {
	switch a.Kind {
	case ActionSetRoute, ActionUpdatePriority:
		return ActionRiskConditional
	default:
		return ActionRiskApproval
	}
}

type Proposal struct {
	IncidentID            string   `json:"incidentId"`
	ExpectedGraphRevision int64    `json:"expectedGraphRevision"`
	Summary               string   `json:"summary"`
	Rationale             string   `json:"rationale"`
	Actions               []Action `json:"actions"`
}

type IncidentSnapshot struct {
	IncidentID      string               `json:"incidentId"`
	Trigger         string               `json:"trigger"`
	Severity        string               `json:"severity"`
	Summary         string               `json:"summary"`
	Details         map[string]any       `json:"details,omitempty"`
	GraphRevision   int64                `json:"graphRevision"`
	FocusTaskID     string               `json:"focusTaskId,omitempty"`
	Nodes           []NodeSnapshot       `json:"nodes"`
	Dependencies    []DependencySnapshot `json:"dependencies"`
	Diagnostics     []IssueSnapshot      `json:"diagnostics"`
	AvailableAgents []AgentSnapshot      `json:"availableAgents"`
}

type DependencySnapshot struct {
	PrerequisiteID string `json:"prerequisiteId"`
	DependentID    string `json:"dependentId"`
	Satisfied      bool   `json:"satisfied"`
}

type NodeSnapshot struct {
	ID               string             `json:"id"`
	Title            string             `json:"title"`
	Status           model.TaskStatus   `json:"status"`
	WorkflowRole     model.WorkflowRole `json:"workflowRole"`
	Assignee         *string            `json:"assignee,omitempty"`
	Runtime          model.Runtime      `json:"runtime"`
	Priority         int                `json:"priority"`
	UpdatedAt        string             `json:"updatedAt"`
	CurrentRunID     *string            `json:"currentRunId,omitempty"`
	PreservedWork    bool               `json:"preservedWork"`
	WorkspaceDirty   bool               `json:"workspaceDirty"`
	BlockKind        *model.BlockKind   `json:"blockKind,omitempty"`
	BlockReason      *string            `json:"blockReason,omitempty"`
	BlockRecurrences int                `json:"blockRecurrences"`
	FailureCount     int                `json:"failureCount"`
	BlockedBy        []string           `json:"blockedBy"`
	Unlocks          []string           `json:"unlocks"`
}

type IssueSnapshot struct {
	Kind   string `json:"kind"`
	TaskID string `json:"taskId,omitempty"`
	Detail string `json:"detail"`
}

type AgentSnapshot struct {
	ID            string        `json:"id"`
	Runtime       model.Runtime `json:"runtime"`
	Model         string        `json:"model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Enabled       bool          `json:"enabled"`
	Roles         []string      `json:"roles"`
	Health        string        `json:"health"`
	MaxConcurrent int           `json:"maxConcurrent"`
	ActiveSlots   int           `json:"activeSlots"`
	CooldownUntil *string       `json:"cooldownUntil,omitempty"`
	Fallbacks     []string      `json:"fallbacks"`
}

type Analyzer struct {
	Planner    orchestration.Planner
	MaxActions int
}

func (a Analyzer) Analyze(ctx context.Context, snapshot IncidentSnapshot) (Proposal, error) {
	if a.Planner == nil {
		return Proposal{}, errors.New("coordinator analyzer requires a planner")
	}
	if strings.TrimSpace(snapshot.IncidentID) == "" {
		return Proposal{}, errors.New("coordinator snapshot requires an incident ID")
	}
	maxActions := a.MaxActions
	if maxActions < 1 {
		maxActions = 3
	}
	if maxActions > 20 {
		maxActions = 20
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return Proposal{}, err
	}
	if len(snapshot.Nodes) > 200 || len(snapshot.Dependencies) > 800 || len(snapshot.AvailableAgents) > 100 ||
		len(encoded) > 512*1024 {
		return Proposal{}, errors.New("coordinator snapshot exceeds the bounded analysis limit")
	}
	prompt := strings.Join([]string{
		"You are the Autogora Coordinator. Analyze an exceptional workflow incident; normal scheduling belongs to the deterministic Supervisor and Dispatcher.",
		"Do not call tools, run shell commands, edit files, access Git, or invent task state. Return only a proposal that conforms to the supplied JSON schema.",
		"Prefer no actions when deterministic recovery or a user decision is required. Never complete, delete, archive, publish, approve, or change global/board configuration.",
		"Use only task IDs, versions, and agent IDs present in the snapshot. Keep the proposal to at most " + fmt.Sprint(maxActions) + " actions.",
		"Incident snapshot:",
		string(encoded),
	}, "\n\n")
	raw, err := a.Planner(ctx, orchestration.PlannerRequest{
		TaskID: snapshot.FocusTaskID, Kind: orchestration.PlannerCoordinator,
		Prompt: prompt, Schema: proposalSchema(maxActions),
	})
	if err != nil {
		return Proposal{}, err
	}
	encodedProposal, err := json.Marshal(raw)
	if err != nil {
		return Proposal{}, fmt.Errorf("encode coordinator response: %w", err)
	}
	var proposal Proposal
	if err := json.Unmarshal(encodedProposal, &proposal); err != nil {
		return Proposal{}, fmt.Errorf("decode coordinator response: %w", err)
	}
	if proposal.IncidentID != snapshot.IncidentID {
		return Proposal{}, errors.New("coordinator proposal incident does not match the snapshot")
	}
	if proposal.ExpectedGraphRevision != snapshot.GraphRevision {
		return Proposal{}, errors.New("coordinator proposal graph revision does not match the snapshot")
	}
	if err := ValidateProposal(proposal, maxActions); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

func ValidateProposal(proposal Proposal, maxActions int) error {
	if strings.TrimSpace(proposal.IncidentID) == "" {
		return errors.New("coordinator proposal incidentId cannot be empty")
	}
	if proposal.ExpectedGraphRevision < 0 {
		return errors.New("coordinator proposal expectedGraphRevision cannot be negative")
	}
	if strings.TrimSpace(proposal.Summary) == "" {
		return errors.New("coordinator proposal summary cannot be empty")
	}
	if strings.TrimSpace(proposal.Rationale) == "" {
		return errors.New("coordinator proposal rationale cannot be empty")
	}
	if maxActions < 1 {
		maxActions = 3
	}
	if len(proposal.Actions) > maxActions {
		return fmt.Errorf("coordinator proposal has %d actions; maximum is %d", len(proposal.Actions), maxActions)
	}
	createdKeys := map[string]bool{}
	for index, action := range proposal.Actions {
		if err := validateAction(action, createdKeys); err != nil {
			return fmt.Errorf("coordinator action %d: %w", index+1, err)
		}
		if action.Kind == ActionCreateTask {
			createdKeys[action.Task.Key] = true
		}
	}
	return nil
}

func validateAction(action Action, createdKeys map[string]bool) error {
	if strings.TrimSpace(action.Reason) == "" {
		return errors.New("reason cannot be empty")
	}
	requireExistingTask := func() error {
		if strings.TrimSpace(action.TaskID) == "" || strings.TrimSpace(action.ExpectedUpdatedAt) == "" {
			return errors.New("taskId and expectedUpdatedAt are required")
		}
		return nil
	}
	switch action.Kind {
	case ActionSetRoute:
		if err := requireExistingTask(); err != nil {
			return err
		}
		if strings.TrimSpace(action.Assignee) == "" || action.Runtime == model.RuntimeManual || !model.ValidRuntime(action.Runtime) {
			return errors.New("set_route requires an assignee and coding-agent runtime")
		}
		if err := rejectActionFields(action, "priority", "dependency", "task"); err != nil {
			return err
		}
	case ActionUpdatePriority:
		if err := requireExistingTask(); err != nil {
			return err
		}
		if action.Priority == nil {
			return errors.New("update_priority requires priority")
		}
		if err := rejectActionFields(action, "route", "dependency", "task"); err != nil {
			return err
		}
	case ActionUnblockTask, ActionMoveToTriage:
		if err := requireExistingTask(); err != nil {
			return err
		}
		if err := rejectActionFields(action, "route", "priority", "dependency", "task"); err != nil {
			return err
		}
	case ActionAddDependency, ActionRemoveDependency:
		if strings.TrimSpace(action.PrerequisiteID) == "" || strings.TrimSpace(action.DependentID) == "" ||
			action.PrerequisiteID == action.DependentID {
			return errors.New("dependency action requires two distinct task IDs")
		}
		if strings.TrimSpace(action.ExpectedPrerequisiteUpdatedAt) == "" ||
			strings.TrimSpace(action.ExpectedDependentUpdatedAt) == "" {
			return errors.New("dependency action requires expected versions for both tasks")
		}
		if err := rejectActionFields(action, "existing_task", "route", "priority", "task"); err != nil {
			return err
		}
	case ActionCreateTask:
		if action.Task == nil {
			return errors.New("create_task requires task")
		}
		task := action.Task
		task.Key = strings.TrimSpace(task.Key)
		if task.Key == "" || createdKeys[task.Key] {
			return errors.New("create_task requires a unique key")
		}
		if strings.TrimSpace(task.Title) == "" || strings.TrimSpace(task.Body) == "" || strings.TrimSpace(task.Assignee) == "" {
			return errors.New("create_task requires title, body, and assignee")
		}
		if task.Runtime == model.RuntimeManual || !model.ValidRuntime(task.Runtime) {
			return errors.New("create_task requires a coding-agent runtime")
		}
		if task.WorkflowRole != model.WorkflowRoleWorker && task.WorkflowRole != model.WorkflowRoleReviewer {
			return errors.New("create_task workflowRole must be worker or reviewer")
		}
		if duplicateString(task.Prerequisites) || duplicateString(task.Dependents) {
			return errors.New("create_task relationship IDs must be unique")
		}
		prerequisites := uniqueStrings(task.Prerequisites)
		for dependent := range uniqueStrings(task.Dependents) {
			if prerequisites[dependent] {
				return fmt.Errorf("create_task cannot use %s as both prerequisite and dependent", dependent)
			}
		}
		related := append(append([]string{}, task.Prerequisites...), task.Dependents...)
		if task.ParentTaskID != "" {
			related = append(related, task.ParentTaskID)
		}
		if len(action.ExpectedTaskVersions) != len(uniqueStrings(related)) {
			return errors.New("create_task requires one expected version for every related task")
		}
		for _, id := range related {
			if strings.TrimSpace(action.ExpectedTaskVersions[id]) == "" {
				return fmt.Errorf("create_task requires the expected version for related task %s", id)
			}
		}
		if err := rejectActionFields(action, "existing_task", "route", "priority", "dependency"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported action kind %q", action.Kind)
	}
	return nil
}

func rejectActionFields(action Action, groups ...string) error {
	for _, group := range groups {
		switch group {
		case "existing_task":
			if action.TaskID != "" || action.ExpectedUpdatedAt != "" {
				return errors.New("action contains fields for an existing task mutation")
			}
		case "route":
			if action.Assignee != "" || action.Runtime != "" {
				return errors.New("action contains route fields that do not apply to its kind")
			}
		case "priority":
			if action.Priority != nil {
				return errors.New("action contains a priority field that does not apply to its kind")
			}
		case "dependency":
			if action.PrerequisiteID != "" || action.ExpectedPrerequisiteUpdatedAt != "" ||
				action.DependentID != "" || action.ExpectedDependentUpdatedAt != "" {
				return errors.New("action contains dependency fields that do not apply to its kind")
			}
		case "task":
			if action.Task != nil || len(action.ExpectedTaskVersions) != 0 {
				return errors.New("action contains task creation fields that do not apply to its kind")
			}
		}
	}
	return nil
}

func uniqueStrings(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = true
		}
	}
	return result
}

func duplicateString(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func proposalSchema(maxActions int) map[string]any {
	stringProperty := func() map[string]any { return map[string]any{"type": "string", "minLength": 1} }
	actionObject := func(required []string, properties map[string]any) map[string]any {
		properties["reason"] = stringProperty()
		required = append(required, "reason")
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"required": required, "properties": properties,
		}
	}
	existingTask := func(kind ActionKind, extraRequired []string, extra map[string]any) map[string]any {
		properties := map[string]any{
			"kind":              map[string]any{"const": string(kind)},
			"taskId":            stringProperty(),
			"expectedUpdatedAt": stringProperty(),
		}
		for key, value := range extra {
			properties[key] = value
		}
		required := append([]string{"kind", "taskId", "expectedUpdatedAt"}, extraRequired...)
		return actionObject(required, properties)
	}
	runtime := map[string]any{"type": "string", "enum": []string{"claude", "codex", "cline", "gemini"}}
	dependencyAction := func(kind ActionKind) map[string]any {
		return actionObject([]string{
			"kind", "prerequisiteId", "expectedPrerequisiteUpdatedAt",
			"dependentId", "expectedDependentUpdatedAt",
		}, map[string]any{
			"kind":                          map[string]any{"const": string(kind)},
			"prerequisiteId":                stringProperty(),
			"expectedPrerequisiteUpdatedAt": stringProperty(),
			"dependentId":                   stringProperty(),
			"expectedDependentUpdatedAt":    stringProperty(),
		})
	}
	taskSchema := map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{
			"key", "title", "body", "assignee", "runtime", "workflowRole",
			"priority", "prerequisites", "dependents", "parentTaskId",
		},
		"properties": map[string]any{
			"key": stringProperty(), "title": stringProperty(), "body": stringProperty(),
			"assignee": stringProperty(), "runtime": runtime,
			"workflowRole": map[string]any{"type": "string", "enum": []string{"worker", "reviewer"}},
			"priority":     map[string]any{"type": "integer"},
			"prerequisites": map[string]any{
				"type": "array", "uniqueItems": true, "items": stringProperty(),
			},
			"dependents": map[string]any{
				"type": "array", "uniqueItems": true, "items": stringProperty(),
			},
			"parentTaskId": map[string]any{
				"type": []string{"string", "null"}, "minLength": 1,
			},
		},
	}
	actionSchemas := []any{
		existingTask(ActionSetRoute, []string{"assignee", "runtime"}, map[string]any{
			"assignee": stringProperty(), "runtime": runtime,
		}),
		existingTask(ActionUpdatePriority, []string{"priority"}, map[string]any{
			"priority": map[string]any{"type": "integer"},
		}),
		existingTask(ActionUnblockTask, nil, map[string]any{}),
		existingTask(ActionMoveToTriage, nil, map[string]any{}),
		dependencyAction(ActionAddDependency),
		dependencyAction(ActionRemoveDependency),
		actionObject([]string{"kind", "task", "expectedTaskVersions"}, map[string]any{
			"kind": map[string]any{"const": string(ActionCreateTask)},
			"task": taskSchema,
			"expectedTaskVersions": map[string]any{
				"type": "array", "uniqueItems": true,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"taskId", "updatedAt"},
					"properties": map[string]any{
						"taskId": stringProperty(), "updatedAt": stringProperty(),
					},
				},
			},
		}),
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"incidentId", "expectedGraphRevision", "summary", "rationale", "actions",
		},
		"properties": map[string]any{
			"incidentId":            stringProperty(),
			"expectedGraphRevision": map[string]any{"type": "integer", "minimum": 0},
			"summary":               stringProperty(),
			"rationale":             stringProperty(),
			"actions": map[string]any{
				"type": "array", "maxItems": maxActions,
				"items": map[string]any{"anyOf": actionSchemas},
			},
		},
	}
}
