package coordinator

import (
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type ValidationIssue struct {
	ActionIndex int    `json:"actionIndex"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

type ValidatedAction struct {
	Action Action     `json:"action"`
	Risk   ActionRisk `json:"risk"`
}

type ValidationResult struct {
	Valid   bool              `json:"valid"`
	Actions []ValidatedAction `json:"actions"`
	Issues  []ValidationIssue `json:"issues"`
}

func ValidateAgainstSnapshot(proposal Proposal, snapshot IncidentSnapshot, maxActions int) ValidationResult {
	result := ValidationResult{Actions: []ValidatedAction{}, Issues: []ValidationIssue{}}
	if err := ValidateProposal(proposal, maxActions); err != nil {
		result.Issues = append(result.Issues, ValidationIssue{
			ActionIndex: -1, Code: "invalid_proposal", Message: err.Error(),
		})
		return result
	}
	nodes := make(map[string]NodeSnapshot, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if id := strings.TrimSpace(node.ID); id != "" {
			nodes[id] = node
		}
	}
	agents := make(map[string]AgentSnapshot, len(snapshot.AvailableAgents))
	for _, agent := range snapshot.AvailableAgents {
		if id := strings.TrimSpace(agent.ID); id != "" {
			agents[id] = agent
		}
	}
	edges := make(map[string]bool, len(snapshot.Dependencies))
	adjacency := make(map[string][]string, len(nodes))
	for _, edge := range snapshot.Dependencies {
		key := dependencyKey(edge.PrerequisiteID, edge.DependentID)
		edges[key] = true
		adjacency[edge.PrerequisiteID] = append(adjacency[edge.PrerequisiteID], edge.DependentID)
	}
	addIssue := func(index int, code, message string) {
		result.Issues = append(result.Issues, ValidationIssue{ActionIndex: index, Code: code, Message: message})
	}
	for index, action := range proposal.Actions {
		result.Actions = append(result.Actions, ValidatedAction{Action: action, Risk: action.Risk()})
		switch action.Kind {
		case ActionSetRoute, ActionUpdatePriority, ActionUnblockTask, ActionMoveToTriage:
			node, found := nodes[action.TaskID]
			if !found {
				addIssue(index, "unknown_task", "task is not present in the bounded incident snapshot")
				continue
			}
			if node.UpdatedAt != action.ExpectedUpdatedAt {
				addIssue(index, "stale_task", fmt.Sprintf("task changed at %s", node.UpdatedAt))
			}
			if node.Status == model.TaskStatusRunning || node.Status == model.TaskStatusDone || node.Status == model.TaskStatusArchived {
				addIssue(index, "immutable_task_state", "running and terminal tasks cannot be changed by Coordinator")
			}
			if node.WorkflowRole == model.WorkflowRoleControl {
				addIssue(index, "control_task", "control tasks cannot be changed by Coordinator")
			}
			if action.Kind == ActionSetRoute {
				validateRoute(index, action.Assignee, action.Runtime, agents, addIssue)
			}
			if action.Kind == ActionUnblockTask && node.Status != model.TaskStatusBlocked &&
				!(node.Status == model.TaskStatusTriage && node.BlockReason != nil) {
				addIssue(index, "not_blocked", "unblock_task requires a blocked task or a triage task with a recorded block")
			}
		case ActionAddDependency, ActionRemoveDependency:
			prerequisite, prerequisiteFound := nodes[action.PrerequisiteID]
			dependent, dependentFound := nodes[action.DependentID]
			if !prerequisiteFound || !dependentFound {
				addIssue(index, "unknown_dependency_task", "both dependency tasks must be present in the bounded incident snapshot")
				continue
			}
			key := dependencyKey(action.PrerequisiteID, action.DependentID)
			if action.Kind == ActionAddDependency {
				if edges[key] {
					addIssue(index, "duplicate_dependency", "dependency already exists")
				}
				if dependent.Status == model.TaskStatusRunning || dependent.Status == model.TaskStatusArchived {
					addIssue(index, "immutable_dependent", "cannot add a prerequisite to a running or archived task")
				}
				if reachable(adjacency, action.DependentID, action.PrerequisiteID) {
					addIssue(index, "dependency_cycle", "dependency would create a cycle")
				}
			} else if !edges[key] {
				addIssue(index, "missing_dependency", "dependency no longer exists")
			}
			if prerequisite.WorkflowRole == model.WorkflowRoleControl || dependent.WorkflowRole == model.WorkflowRoleControl {
				addIssue(index, "control_task", "control task dependencies cannot be changed by Coordinator")
			}
		case ActionCreateTask:
			task := action.Task
			if task == nil {
				continue
			}
			validateRoute(index, task.Assignee, task.Runtime, agents, addIssue)
			for _, id := range append(append([]string{}, task.Prerequisites...), task.Dependents...) {
				if _, found := nodes[id]; !found {
					addIssue(index, "unknown_relationship_task", fmt.Sprintf("related task %s is not present in the bounded incident snapshot", id))
				}
			}
			if task.ParentTaskID != "" {
				if parent, found := nodes[task.ParentTaskID]; !found {
					addIssue(index, "unknown_parent_task", "parentTaskId is not present in the bounded incident snapshot")
				} else if parent.WorkflowRole == model.WorkflowRoleControl {
					addIssue(index, "control_parent", "Coordinator cannot attach recovery work below a control task")
				}
			}
		}
	}
	result.Valid = len(result.Issues) == 0
	return result
}

func validateRoute(index int, assignee string, runtime model.Runtime, agents map[string]AgentSnapshot, addIssue func(int, string, string)) {
	agent, found := agents[assignee]
	if !found {
		addIssue(index, "unavailable_agent", "assignee is not in the available Coordinator snapshot")
		return
	}
	if agent.Runtime != runtime {
		addIssue(index, "runtime_mismatch", fmt.Sprintf("agent %s uses runtime %s", assignee, agent.Runtime))
	}
	if health := strings.TrimSpace(agent.Health); health != "" && health != string(model.AgentHealthReady) {
		addIssue(index, "unhealthy_agent", fmt.Sprintf("agent %s health is %s", assignee, health))
	}
}

func dependencyKey(prerequisiteID, dependentID string) string {
	return prerequisiteID + "\x00" + dependentID
}

func reachable(adjacency map[string][]string, start, target string) bool {
	queue := []string{start}
	seen := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == target {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		queue = append(queue, adjacency[current]...)
	}
	return false
}
