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

type Action struct {
	Kind              ActionKind    `json:"kind"`
	TaskID            string        `json:"taskId,omitempty"`
	ExpectedUpdatedAt string        `json:"expectedUpdatedAt,omitempty"`
	Assignee          string        `json:"assignee,omitempty"`
	Runtime           model.Runtime `json:"runtime,omitempty"`
	Priority          *int          `json:"priority,omitempty"`
	PrerequisiteID    string        `json:"prerequisiteId,omitempty"`
	DependentID       string        `json:"dependentId,omitempty"`
	Task              *TaskDraft    `json:"task,omitempty"`
	Reason            string        `json:"reason"`
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
	Summary   string   `json:"summary"`
	Rationale string   `json:"rationale"`
	Actions   []Action `json:"actions"`
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
	UpdatedAt        string             `json:"updatedAt"`
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
	Health        string        `json:"health"`
	MaxConcurrent int           `json:"maxConcurrent"`
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
	if err := ValidateProposal(proposal, maxActions); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

func ValidateProposal(proposal Proposal, maxActions int) error {
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
	case ActionUpdatePriority:
		if err := requireExistingTask(); err != nil {
			return err
		}
		if action.Priority == nil {
			return errors.New("update_priority requires priority")
		}
	case ActionUnblockTask, ActionMoveToTriage:
		if err := requireExistingTask(); err != nil {
			return err
		}
	case ActionAddDependency, ActionRemoveDependency:
		if strings.TrimSpace(action.PrerequisiteID) == "" || strings.TrimSpace(action.DependentID) == "" ||
			action.PrerequisiteID == action.DependentID {
			return errors.New("dependency action requires two distinct task IDs")
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
	default:
		return fmt.Errorf("unsupported action kind %q", action.Kind)
	}
	return nil
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
	actionKinds := []string{
		string(ActionSetRoute), string(ActionUpdatePriority), string(ActionUnblockTask),
		string(ActionMoveToTriage), string(ActionAddDependency), string(ActionRemoveDependency),
		string(ActionCreateTask),
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"summary", "rationale", "actions"},
		"properties": map[string]any{
			"summary":   map[string]any{"type": "string", "minLength": 1},
			"rationale": map[string]any{"type": "string", "minLength": 1},
			"actions": map[string]any{
				"type": "array", "maxItems": maxActions,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"kind", "reason"},
					"properties": map[string]any{
						"kind":              map[string]any{"type": "string", "enum": actionKinds},
						"taskId":            map[string]any{"type": "string"},
						"expectedUpdatedAt": map[string]any{"type": "string"},
						"assignee":          map[string]any{"type": "string"},
						"runtime":           map[string]any{"type": "string", "enum": []string{"claude", "codex", "cline", "gemini"}},
						"priority":          map[string]any{"type": "integer"},
						"prerequisiteId":    map[string]any{"type": "string"},
						"dependentId":       map[string]any{"type": "string"},
						"reason":            map[string]any{"type": "string", "minLength": 1},
						"task": map[string]any{
							"type": "object", "additionalProperties": false,
							"required": []string{"key", "title", "body", "assignee", "runtime", "workflowRole", "priority", "prerequisites", "dependents"},
							"properties": map[string]any{
								"key": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"},
								"body": map[string]any{"type": "string"}, "assignee": map[string]any{"type": "string"},
								"runtime":       map[string]any{"type": "string", "enum": []string{"claude", "codex", "cline", "gemini"}},
								"workflowRole":  map[string]any{"type": "string", "enum": []string{"worker", "reviewer"}},
								"priority":      map[string]any{"type": "integer"},
								"prerequisites": map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}},
								"dependents":    map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}},
								"parentTaskId":  map[string]any{"type": "string"},
							},
						},
					},
				},
			},
		},
	}
}
