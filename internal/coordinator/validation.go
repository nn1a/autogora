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
	if proposal.IncidentID != snapshot.IncidentID {
		result.Issues = append(result.Issues, ValidationIssue{
			ActionIndex: -1, Code: "incident_mismatch", Message: "proposal incident does not match the snapshot",
		})
	}
	if proposal.ExpectedGraphRevision != snapshot.GraphRevision {
		result.Issues = append(result.Issues, ValidationIssue{
			ActionIndex: -1, Code: "stale_graph", Message: fmt.Sprintf(
				"graph changed from revision %d to %d", proposal.ExpectedGraphRevision, snapshot.GraphRevision,
			),
		})
	}
	nodes := make(map[string]NodeSnapshot, len(snapshot.Nodes))
	originalNodes := make(map[string]NodeSnapshot, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if id := strings.TrimSpace(node.ID); id != "" {
			nodes[id] = node
			originalNodes[id] = node
		}
	}
	agents := make(map[string]AgentSnapshot, len(snapshot.AvailableAgents))
	for _, agent := range snapshot.AvailableAgents {
		if id := strings.TrimSpace(agent.ID); id != "" {
			agents[id] = agent
		}
	}
	edges := make(map[string]DependencySnapshot, len(snapshot.Dependencies))
	adjacency := make(map[string]map[string]bool, len(nodes))
	for _, edge := range snapshot.Dependencies {
		key := dependencyKey(edge.PrerequisiteID, edge.DependentID)
		edges[key] = edge
		addAdjacency(adjacency, edge.PrerequisiteID, edge.DependentID)
	}
	addIssue := func(index int, code, message string) {
		result.Issues = append(result.Issues, ValidationIssue{ActionIndex: index, Code: code, Message: message})
	}
	for index, action := range proposal.Actions {
		issuesBefore := len(result.Issues)
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
			if node.CurrentRunID != nil {
				addIssue(index, "active_run", "tasks with an active run cannot be changed by Coordinator")
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
			if prerequisite.UpdatedAt != action.ExpectedPrerequisiteUpdatedAt {
				addIssue(index, "stale_prerequisite", fmt.Sprintf("prerequisite changed at %s", prerequisite.UpdatedAt))
			}
			if dependent.UpdatedAt != action.ExpectedDependentUpdatedAt {
				addIssue(index, "stale_dependent", fmt.Sprintf("dependent changed at %s", dependent.UpdatedAt))
			}
			key := dependencyKey(action.PrerequisiteID, action.DependentID)
			if action.Kind == ActionAddDependency {
				if _, exists := edges[key]; exists {
					addIssue(index, "duplicate_dependency", "dependency already exists")
				}
				if dependent.Status == model.TaskStatusRunning || dependent.Status == model.TaskStatusDone ||
					dependent.Status == model.TaskStatusArchived || dependent.CurrentRunID != nil {
					addIssue(index, "immutable_dependent", "cannot add a prerequisite to a running or terminal task")
				}
				if prerequisite.Status == model.TaskStatusArchived {
					addIssue(index, "archived_prerequisite", "cannot add an archived prerequisite")
				}
				if reachable(adjacency, action.DependentID, action.PrerequisiteID) {
					addIssue(index, "dependency_cycle", "dependency would create a cycle")
				}
			} else {
				if _, exists := edges[key]; !exists {
					addIssue(index, "missing_dependency", "dependency no longer exists")
				}
				if dependent.Status == model.TaskStatusRunning || dependent.Status == model.TaskStatusDone ||
					dependent.Status == model.TaskStatusArchived || dependent.CurrentRunID != nil {
					addIssue(index, "immutable_dependent", "cannot remove a prerequisite from a running or terminal task")
				}
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
				node, found := nodes[id]
				if !found {
					addIssue(index, "unknown_relationship_task", fmt.Sprintf("related task %s is not present in the bounded incident snapshot", id))
				} else if node.UpdatedAt != action.ExpectedTaskVersions[id] {
					addIssue(index, "stale_relationship_task", fmt.Sprintf("related task %s changed at %s", id, node.UpdatedAt))
				}
			}
			for _, id := range task.Prerequisites {
				if node, found := nodes[id]; found && node.WorkflowRole == model.WorkflowRoleControl {
					addIssue(index, "control_task", "control tasks cannot be task dependencies")
				}
			}
			for _, id := range task.Dependents {
				if node, found := nodes[id]; found {
					if node.WorkflowRole == model.WorkflowRoleControl {
						addIssue(index, "control_task", "control tasks cannot be task dependencies")
					}
					if node.Status == model.TaskStatusRunning || node.Status == model.TaskStatusDone ||
						node.Status == model.TaskStatusArchived || node.CurrentRunID != nil {
						addIssue(index, "immutable_dependent", "created recovery work cannot become a prerequisite of a running or terminal task")
					}
				}
			}
			if task.ParentTaskID != "" {
				if parent, found := nodes[task.ParentTaskID]; !found {
					addIssue(index, "unknown_parent_task", "parentTaskId is not present in the bounded incident snapshot")
				} else if parent.UpdatedAt != action.ExpectedTaskVersions[task.ParentTaskID] {
					addIssue(index, "stale_parent_task", fmt.Sprintf("parent task changed at %s", parent.UpdatedAt))
				} else if parent.WorkflowRole == model.WorkflowRoleControl {
					addIssue(index, "control_task", "control tasks cannot own Coordinator-created recovery work")
				} else if parent.Status == model.TaskStatusArchived {
					addIssue(index, "archived_parent_task", "archived tasks cannot own Coordinator-created recovery work")
				}
			}
		}
		if len(result.Issues) != issuesBefore {
			continue
		}
		switch action.Kind {
		case ActionSetRoute:
			node := nodes[action.TaskID]
			assignee := action.Assignee
			node.Assignee, node.Runtime = &assignee, action.Runtime
			nodes[action.TaskID] = node
		case ActionUpdatePriority:
			node := nodes[action.TaskID]
			node.Priority = *action.Priority
			nodes[action.TaskID] = node
		case ActionUnblockTask:
			node := nodes[action.TaskID]
			node.Status, node.BlockKind, node.BlockReason, node.BlockRecurrences = model.TaskStatusTodo, nil, nil, 0
			nodes[action.TaskID] = node
		case ActionMoveToTriage:
			node := nodes[action.TaskID]
			node.Status, node.BlockKind, node.BlockReason = model.TaskStatusTriage, nil, nil
			node.BlockRecurrences, node.FailureCount = 0, 0
			nodes[action.TaskID] = node
		case ActionAddDependency:
			edge := DependencySnapshot{PrerequisiteID: action.PrerequisiteID, DependentID: action.DependentID}
			edges[dependencyKey(action.PrerequisiteID, action.DependentID)] = edge
			addAdjacency(adjacency, action.PrerequisiteID, action.DependentID)
		case ActionRemoveDependency:
			delete(edges, dependencyKey(action.PrerequisiteID, action.DependentID))
			removeAdjacency(adjacency, action.PrerequisiteID, action.DependentID)
		case ActionCreateTask:
			simulateCreatedTask(index, action, nodes, edges, adjacency, addIssue)
		}
	}
	// An empty action list is an explicit manual-escalation proposal. It does
	// not claim that the integration condition was repaired, so preserve it as
	// a valid approval handoff. Any proposal that does mutate state must still
	// prove through simulation that the triggering block is gone.
	if snapshot.Trigger == string(model.CoordinationTriggerIntegrationConflict) &&
		len(proposal.Actions) > 0 {
		focus, found := nodes[snapshot.FocusTaskID]
		if !found {
			addIssue(
				-1,
				"integration_condition_unknown",
				"integration incident focus task is not present in the bounded snapshot",
			)
		} else if coordinatorIntegrationConditionRemains(snapshot.Details, focus) {
			addIssue(
				-1,
				"integration_condition_remains",
				"proposal does not clear the integration block that opened the incident",
			)
		}
	}
	for _, action := range proposal.Actions {
		result.Actions = append(result.Actions, ValidatedAction{
			Action: action, Risk: classifyValidatedRisk(action, originalNodes, nodes, agents),
		})
	}
	result.Valid = len(result.Issues) == 0
	return result
}

func coordinatorIntegrationConditionRemains(
	details map[string]any,
	focus NodeSnapshot,
) bool {
	if focus.Status == model.TaskStatusRunning {
		return true
	}
	if focus.Status == model.TaskStatusDone ||
		focus.Status == model.TaskStatusArchived {
		return false
	}
	if focus.Status != model.TaskStatusBlocked &&
		focus.Status != model.TaskStatusTriage {
		return false
	}
	detailString := func(value any) string {
		text, _ := value.(string)
		return strings.TrimSpace(text)
	}
	expectedKind := detailString(details["blockKind"])
	expectedReason := detailString(details["reason"])
	if expectedKind != "" {
		if focus.BlockKind == nil ||
			strings.TrimSpace(string(*focus.BlockKind)) != expectedKind {
			return false
		}
	}
	if expectedReason != "" {
		if focus.BlockReason == nil ||
			!coordinatorIntegrationReasonMatches(*focus.BlockReason, expectedReason) {
			return false
		}
	}
	// Malformed legacy details are handled conservatively: a still-blocked
	// focus task must not be declared recovered merely because the expected
	// kind or reason was absent from the incident payload.
	return focus.BlockKind != nil || focus.BlockReason != nil ||
		(expectedKind == "" && expectedReason == "")
}

// ManualEscalationConditionResolved reports whether an empty integration
// proposal has been satisfied by a person since it was created. Other trigger
// types remain explicit decisions because their recovery condition cannot be
// inferred safely from one focus task.
func ManualEscalationConditionResolved(
	proposal Proposal,
	snapshot IncidentSnapshot,
) bool {
	if len(proposal.Actions) != 0 ||
		snapshot.Trigger != string(model.CoordinationTriggerIntegrationConflict) {
		return false
	}
	for _, node := range snapshot.Nodes {
		if node.ID == snapshot.FocusTaskID {
			return !coordinatorIntegrationConditionRemains(snapshot.Details, node)
		}
	}
	return false
}

func coordinatorIntegrationReasonMatches(actual, expected string) bool {
	actual, expected = strings.TrimSpace(actual), strings.TrimSpace(expected)
	if actual == expected {
		return actual != ""
	}
	const truncationSuffix = "…"
	return strings.HasSuffix(expected, truncationSuffix) &&
		strings.HasPrefix(actual, strings.TrimSuffix(expected, truncationSuffix))
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
	if !agent.Enabled {
		addIssue(index, "disabled_agent", fmt.Sprintf("agent %s is disabled", assignee))
	}
	if !containsString(agent.Roles, "worker") {
		addIssue(index, "wrong_agent_role", fmt.Sprintf("agent %s does not have the worker role", assignee))
	}
	if health := strings.TrimSpace(agent.Health); health != string(model.AgentHealthReady) {
		addIssue(index, "unhealthy_agent", fmt.Sprintf("agent %s health is %s", assignee, health))
	}
	// A healthy route remains valid while all of its slots are occupied.
	// Capacity is transient dispatch backpressure, not a reason to spend
	// another Coordinator analysis call or reject an otherwise safe recovery.
	// Invalid capacity metadata is still rejected.
	if agent.MaxConcurrent < 1 || agent.ActiveSlots < 0 {
		addIssue(index, "agent_capacity", fmt.Sprintf(
			"agent %s has %d of %d slots active", assignee, agent.ActiveSlots, agent.MaxConcurrent,
		))
	}
	if agent.CooldownUntil != nil && strings.TrimSpace(*agent.CooldownUntil) != "" {
		addIssue(index, "agent_cooldown", fmt.Sprintf("agent %s is cooling down until %s", assignee, *agent.CooldownUntil))
	}
}

func dependencyKey(prerequisiteID, dependentID string) string {
	return prerequisiteID + "\x00" + dependentID
}

func addAdjacency(adjacency map[string]map[string]bool, prerequisiteID, dependentID string) {
	if adjacency[prerequisiteID] == nil {
		adjacency[prerequisiteID] = map[string]bool{}
	}
	adjacency[prerequisiteID][dependentID] = true
}

func removeAdjacency(adjacency map[string]map[string]bool, prerequisiteID, dependentID string) {
	delete(adjacency[prerequisiteID], dependentID)
}

func reachable(adjacency map[string]map[string]bool, start, target string) bool {
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
		for next := range adjacency[current] {
			queue = append(queue, next)
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func simulateCreatedTask(
	index int,
	action Action,
	nodes map[string]NodeSnapshot,
	edges map[string]DependencySnapshot,
	adjacency map[string]map[string]bool,
	addIssue func(int, string, string),
) {
	task := action.Task
	if task == nil {
		return
	}
	syntheticID := "proposal:" + task.Key
	if _, exists := nodes[syntheticID]; exists {
		addIssue(index, "duplicate_created_task", "created task key is already present in the simulated proposal")
		return
	}
	simulatedAdjacency := cloneAdjacency(adjacency)
	for _, prerequisiteID := range task.Prerequisites {
		if reachable(simulatedAdjacency, syntheticID, prerequisiteID) {
			addIssue(index, "dependency_cycle", "created task prerequisites would create a cycle")
			return
		}
		addAdjacency(simulatedAdjacency, prerequisiteID, syntheticID)
	}
	for _, dependentID := range task.Dependents {
		if reachable(simulatedAdjacency, dependentID, syntheticID) {
			addIssue(index, "dependency_cycle", "created task dependents would create a cycle")
			return
		}
		addAdjacency(simulatedAdjacency, syntheticID, dependentID)
	}
	assignee := task.Assignee
	nodes[syntheticID] = NodeSnapshot{
		ID: syntheticID, Title: task.Title, Status: model.TaskStatusTodo,
		WorkflowRole: task.WorkflowRole, Assignee: &assignee, Runtime: task.Runtime, Priority: task.Priority,
	}
	for _, prerequisiteID := range task.Prerequisites {
		edge := DependencySnapshot{PrerequisiteID: prerequisiteID, DependentID: syntheticID}
		edges[dependencyKey(prerequisiteID, syntheticID)] = edge
		addAdjacency(adjacency, prerequisiteID, syntheticID)
	}
	for _, dependentID := range task.Dependents {
		edge := DependencySnapshot{PrerequisiteID: syntheticID, DependentID: dependentID}
		edges[dependencyKey(syntheticID, dependentID)] = edge
		addAdjacency(adjacency, syntheticID, dependentID)
	}
}

func cloneAdjacency(source map[string]map[string]bool) map[string]map[string]bool {
	cloned := make(map[string]map[string]bool, len(source))
	for sourceID, destinations := range source {
		cloned[sourceID] = make(map[string]bool, len(destinations))
		for destinationID := range destinations {
			cloned[sourceID][destinationID] = true
		}
	}
	return cloned
}

func classifyValidatedRisk(
	action Action,
	originalNodes map[string]NodeSnapshot,
	finalNodes map[string]NodeSnapshot,
	agents map[string]AgentSnapshot,
) ActionRisk {
	switch action.Kind {
	case ActionUpdatePriority:
		return ActionRiskConditional
	case ActionSetRoute:
		node := finalNodes[action.TaskID]
		if node.CurrentRunID == nil && !node.PreservedWork && !node.WorkspaceDirty {
			return ActionRiskConditional
		}
	case ActionUnblockTask:
		original, final := originalNodes[action.TaskID], finalNodes[action.TaskID]
		if original.CurrentRunID == nil && !original.PreservedWork && !original.WorkspaceDirty &&
			original.BlockKind != nil &&
			(*original.BlockKind == model.BlockKindCapability || *original.BlockKind == model.BlockKindTransient) &&
			final.Assignee != nil {
			agent, found := agents[*final.Assignee]
			if found && agent.Enabled && agent.Runtime == final.Runtime &&
				strings.TrimSpace(agent.Health) == string(model.AgentHealthReady) &&
				agent.MaxConcurrent > 0 && agent.ActiveSlots >= 0 &&
				agent.CooldownUntil == nil && containsString(agent.Roles, "worker") {
				return ActionRiskConditional
			}
		}
	case ActionCreateTask:
		if action.Task != nil && len(action.Task.Dependents) == 0 {
			return ActionRiskConditional
		}
	}
	return ActionRiskApproval
}
