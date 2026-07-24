package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

// CoordinationApplyAuthorization binds proposal application to the durable
// approval state that authorized it. Validation decides whether every action
// is safe for automatic application; the store only accepts that path from a
// validated proposal that is still owned by the live coordinator claim.
type CoordinationApplyAuthorization string

const (
	CoordinationApplyValidatedAuto CoordinationApplyAuthorization = "validated_auto"
	CoordinationApplyApproved      CoordinationApplyAuthorization = "approved"
)

type ApplyCoordinationProposalInput struct {
	Authorization         CoordinationApplyAuthorization
	ExpectedGraphRevision *int64
	ClaimToken            string
	Current               time.Time
}

type ApplyCoordinationProposalResult struct {
	Proposal       model.CoordinationProposal `json:"proposal"`
	Incident       model.CoordinationIncident `json:"incident"`
	GraphRevision  int64                      `json:"graphRevision"`
	CreatedTaskIDs map[string]string          `json:"createdTaskIds"`
}

type coordinationActionKind string

const (
	coordinationActionSetRoute         coordinationActionKind = "set_route"
	coordinationActionUpdatePriority   coordinationActionKind = "update_priority"
	coordinationActionUnblockTask      coordinationActionKind = "unblock_task"
	coordinationActionMoveToTriage     coordinationActionKind = "move_to_triage"
	coordinationActionAddDependency    coordinationActionKind = "add_dependency"
	coordinationActionRemoveDependency coordinationActionKind = "remove_dependency"
	coordinationActionCreateTask       coordinationActionKind = "create_task"
)

type coordinationTaskDraft struct {
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

type coordinationAction struct {
	Kind                          coordinationActionKind `json:"kind"`
	TaskID                        string                 `json:"taskId,omitempty"`
	ExpectedUpdatedAt             string                 `json:"expectedUpdatedAt,omitempty"`
	Assignee                      string                 `json:"assignee,omitempty"`
	Runtime                       model.Runtime          `json:"runtime,omitempty"`
	Priority                      *int                   `json:"priority,omitempty"`
	PrerequisiteID                string                 `json:"prerequisiteId,omitempty"`
	ExpectedPrerequisiteUpdatedAt string                 `json:"expectedPrerequisiteUpdatedAt,omitempty"`
	DependentID                   string                 `json:"dependentId,omitempty"`
	ExpectedDependentUpdatedAt    string                 `json:"expectedDependentUpdatedAt,omitempty"`
	Task                          *coordinationTaskDraft `json:"task,omitempty"`
	ExpectedTaskVersions          map[string]string      `json:"expectedTaskVersions,omitempty"`
	Reason                        string                 `json:"reason"`
}

type coordinationApplyPlan struct {
	actions          []coordinationAction
	tasks            map[string]model.Task
	createTaskIDs    map[string]string
	createExisting   map[string]bool
	dependencies     map[string]bool
	dependencyGraph  map[string]map[string]bool
	hierarchyParents map[string]string
	hierarchyGraph   map[string]map[string]bool
}

func expectedCoordinationApplyStates(
	authorization CoordinationApplyAuthorization,
) (model.CoordinationProposalStatus, model.CoordinationIncidentStatus, error) {
	switch authorization {
	case CoordinationApplyValidatedAuto:
		return model.CoordinationProposalValidated, model.CoordinationIncidentCoordinating, nil
	case CoordinationApplyApproved:
		return model.CoordinationProposalApproved, model.CoordinationIncidentAwaitingApproval, nil
	default:
		return "", "", fmt.Errorf("invalid coordination apply authorization: %s", authorization)
	}
}

func decodeCoordinationActions(raw json.RawMessage) ([]coordinationAction, error) {
	if len(raw) > 512*1024 {
		return nil, errors.New("coordination proposal actions exceed the 512 KiB application limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var actions []coordinationAction
	if err := decoder.Decode(&actions); err != nil {
		return nil, fmt.Errorf("decode coordination proposal actions: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return nil, fmt.Errorf("decode coordination proposal actions: %w", err)
	}
	if actions == nil {
		actions = []coordinationAction{}
	}
	if len(actions) > 20 {
		return nil, fmt.Errorf("coordination proposal has %d actions; maximum is 20", len(actions))
	}
	createdKeys := map[string]bool{}
	for index := range actions {
		if err := normalizeCoordinationAction(&actions[index], createdKeys); err != nil {
			return nil, fmt.Errorf("coordination action %d: %w", index+1, err)
		}
		if actions[index].Kind == coordinationActionCreateTask {
			createdKeys[actions[index].Task.Key] = true
		}
	}
	return actions, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected JSON after the actions array")
	}
	return err
}

func normalizeCoordinationAction(action *coordinationAction, createdKeys map[string]bool) error {
	action.TaskID = strings.TrimSpace(action.TaskID)
	action.ExpectedUpdatedAt = strings.TrimSpace(action.ExpectedUpdatedAt)
	action.Assignee = strings.TrimSpace(action.Assignee)
	action.PrerequisiteID = strings.TrimSpace(action.PrerequisiteID)
	action.ExpectedPrerequisiteUpdatedAt = strings.TrimSpace(action.ExpectedPrerequisiteUpdatedAt)
	action.DependentID = strings.TrimSpace(action.DependentID)
	action.ExpectedDependentUpdatedAt = strings.TrimSpace(action.ExpectedDependentUpdatedAt)
	action.Reason = strings.TrimSpace(action.Reason)
	if action.Reason == "" {
		return errors.New("reason cannot be empty")
	}
	requireTask := func() error {
		if action.TaskID == "" || action.ExpectedUpdatedAt == "" {
			return errors.New("taskId and expectedUpdatedAt are required")
		}
		return nil
	}
	reject := func(existing, route, priority, dependency, task bool) error {
		if existing && (action.TaskID != "" || action.ExpectedUpdatedAt != "") {
			return errors.New("action contains fields for an existing task mutation")
		}
		if route && (action.Assignee != "" || action.Runtime != "") {
			return errors.New("action contains route fields that do not apply to its kind")
		}
		if priority && action.Priority != nil {
			return errors.New("action contains a priority field that does not apply to its kind")
		}
		if dependency && (action.PrerequisiteID != "" || action.ExpectedPrerequisiteUpdatedAt != "" ||
			action.DependentID != "" || action.ExpectedDependentUpdatedAt != "") {
			return errors.New("action contains dependency fields that do not apply to its kind")
		}
		if task && (action.Task != nil || len(action.ExpectedTaskVersions) != 0) {
			return errors.New("action contains task creation fields that do not apply to its kind")
		}
		return nil
	}
	switch action.Kind {
	case coordinationActionSetRoute:
		if err := requireTask(); err != nil {
			return err
		}
		if action.Assignee == "" || action.Runtime == model.RuntimeManual || !model.ValidRuntime(action.Runtime) {
			return errors.New("set_route requires an assignee and coding-agent runtime")
		}
		return reject(false, false, true, true, true)
	case coordinationActionUpdatePriority:
		if err := requireTask(); err != nil {
			return err
		}
		if action.Priority == nil {
			return errors.New("update_priority requires priority")
		}
		return reject(false, true, false, true, true)
	case coordinationActionUnblockTask, coordinationActionMoveToTriage:
		if err := requireTask(); err != nil {
			return err
		}
		return reject(false, true, true, true, true)
	case coordinationActionAddDependency, coordinationActionRemoveDependency:
		if action.PrerequisiteID == "" || action.DependentID == "" ||
			action.PrerequisiteID == action.DependentID {
			return errors.New("dependency action requires two distinct task IDs")
		}
		if action.ExpectedPrerequisiteUpdatedAt == "" || action.ExpectedDependentUpdatedAt == "" {
			return errors.New("dependency action requires expected versions for both tasks")
		}
		return reject(true, true, true, false, true)
	case coordinationActionCreateTask:
		if action.Task == nil {
			return errors.New("create_task requires task")
		}
		if err := reject(true, true, true, true, false); err != nil {
			return err
		}
		return normalizeCoordinationTaskDraft(action, createdKeys)
	default:
		return fmt.Errorf("unsupported action kind %q", action.Kind)
	}
}

func normalizeCoordinationTaskDraft(action *coordinationAction, createdKeys map[string]bool) error {
	task := action.Task
	task.Key = strings.TrimSpace(task.Key)
	task.Title = strings.TrimSpace(task.Title)
	task.Assignee = strings.TrimSpace(task.Assignee)
	task.ParentTaskID = strings.TrimSpace(task.ParentTaskID)
	if task.Key == "" || len(task.Key) > 128 || createdKeys[task.Key] {
		return errors.New("create_task requires a unique key of at most 128 characters")
	}
	if task.Title == "" || strings.TrimSpace(task.Body) == "" || task.Assignee == "" {
		return errors.New("create_task requires title, body, and assignee")
	}
	if task.Runtime == model.RuntimeManual || !model.ValidRuntime(task.Runtime) {
		return errors.New("create_task requires a coding-agent runtime")
	}
	if task.WorkflowRole != model.WorkflowRoleWorker && task.WorkflowRole != model.WorkflowRoleReviewer {
		return errors.New("create_task workflowRole must be worker or reviewer")
	}
	prerequisites, err := normalizedUniqueIDs(task.Prerequisites)
	if err != nil {
		return fmt.Errorf("create_task prerequisites: %w", err)
	}
	dependents, err := normalizedUniqueIDs(task.Dependents)
	if err != nil {
		return fmt.Errorf("create_task dependents: %w", err)
	}
	task.Prerequisites, task.Dependents = prerequisites, dependents
	prerequisiteSet := stringSet(prerequisites)
	for _, dependentID := range dependents {
		if prerequisiteSet[dependentID] {
			return fmt.Errorf("create_task cannot use %s as both prerequisite and dependent", dependentID)
		}
	}
	related := append(append([]string{}, prerequisites...), dependents...)
	if task.ParentTaskID != "" {
		related = append(related, task.ParentTaskID)
	}
	expected := make(map[string]string, len(action.ExpectedTaskVersions))
	for id, version := range action.ExpectedTaskVersions {
		id, version = strings.TrimSpace(id), strings.TrimSpace(version)
		if id == "" || version == "" || expected[id] != "" {
			return errors.New("create_task expectedTaskVersions must contain unique non-empty IDs and versions")
		}
		expected[id] = version
	}
	relatedSet := stringSet(related)
	if len(expected) != len(relatedSet) {
		return errors.New("create_task requires one expected version for every related task")
	}
	for id := range relatedSet {
		if expected[id] == "" {
			return fmt.Errorf("create_task requires the expected version for related task %s", id)
		}
	}
	action.ExpectedTaskVersions = expected
	return nil
}

func normalizedUniqueIDs(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return nil, errors.New("relationship IDs must be non-empty and unique")
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, nil
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func coordinationRecoveryIdempotencyKey(incidentID, key string) string {
	return "coordination:" + incidentID + ":" + key
}

func prevalidateCoordinationApply(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
	actions []coordinationAction,
) (coordinationApplyPlan, error) {
	plan := coordinationApplyPlan{
		actions: actions, tasks: map[string]model.Task{}, createTaskIDs: map[string]string{},
		createExisting: map[string]bool{}, dependencies: map[string]bool{},
		dependencyGraph: map[string]map[string]bool{}, hierarchyParents: map[string]string{},
		hierarchyGraph: map[string]map[string]bool{},
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT l.parent_id, l.child_id
		FROM task_links l JOIN tasks p ON p.id = l.parent_id
		WHERE p.board = ?
	`, incident.Board)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var prerequisiteID, dependentID string
		if err := rows.Scan(&prerequisiteID, &dependentID); err != nil {
			rows.Close()
			return plan, err
		}
		plan.dependencies[coordinationDependencyKey(prerequisiteID, dependentID)] = true
		coordinationAddEdge(plan.dependencyGraph, prerequisiteID, dependentID)
	}
	if err := rows.Close(); err != nil {
		return plan, err
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT h.parent_id, h.child_id
		FROM task_hierarchy h JOIN tasks p ON p.id = h.parent_id
		WHERE p.board = ?
	`, incident.Board)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var parentID, childID string
		if err := rows.Scan(&parentID, &childID); err != nil {
			rows.Close()
			return plan, err
		}
		plan.hierarchyParents[childID] = parentID
		coordinationAddEdge(plan.hierarchyGraph, parentID, childID)
	}
	if err := rows.Close(); err != nil {
		return plan, err
	}

	taskAtVersion := func(id, expected string) (model.Task, error) {
		task, ok := plan.tasks[id]
		if !ok {
			task, err = requireTask(ctx, tx, id)
			if err != nil {
				return model.Task{}, err
			}
			plan.tasks[id] = task
		}
		if task.Board != incident.Board {
			return model.Task{}, fmt.Errorf("task %s belongs to board %s, not %s", id, task.Board, incident.Board)
		}
		if task.UpdatedAt != expected {
			return model.Task{}, fmt.Errorf("task update conflict: %s changed at %s; refresh before applying proposal", id, task.UpdatedAt)
		}
		return task, nil
	}
	requireMutable := func(task model.Task) error {
		if task.CurrentRunID != nil {
			return fmt.Errorf("Coordinator cannot change task %s while it has an active run", task.ID)
		}
		if task.Status == model.TaskStatusRunning || task.Status == model.TaskStatusDone ||
			task.Status == model.TaskStatusArchived {
			return fmt.Errorf("Coordinator cannot change task %s in status %s", task.ID, task.Status)
		}
		if task.WorkflowRole == model.WorkflowRoleControl {
			return fmt.Errorf("Coordinator cannot change control task %s", task.ID)
		}
		return nil
	}

	for index, action := range actions {
		actionError := func(err error) (coordinationApplyPlan, error) {
			return plan, fmt.Errorf("coordination action %d (%s): %w", index+1, action.Kind, err)
		}
		switch action.Kind {
		case coordinationActionSetRoute, coordinationActionUpdatePriority,
			coordinationActionUnblockTask, coordinationActionMoveToTriage:
			task, err := taskAtVersion(action.TaskID, action.ExpectedUpdatedAt)
			if err != nil {
				return actionError(err)
			}
			if err := requireMutable(task); err != nil {
				return actionError(err)
			}
			if action.Kind == coordinationActionUnblockTask &&
				task.Status != model.TaskStatusBlocked &&
				!(task.Status == model.TaskStatusTriage && task.BlockReason != nil &&
					task.BlockRecurrences >= blockRecurrenceLimit) {
				return actionError(errors.New("unblock_task requires a blocked task or block-loop triage task"))
			}
		case coordinationActionAddDependency, coordinationActionRemoveDependency:
			prerequisite, err := taskAtVersion(action.PrerequisiteID, action.ExpectedPrerequisiteUpdatedAt)
			if err != nil {
				return actionError(err)
			}
			dependent, err := taskAtVersion(action.DependentID, action.ExpectedDependentUpdatedAt)
			if err != nil {
				return actionError(err)
			}
			if prerequisite.WorkflowRole == model.WorkflowRoleControl ||
				dependent.WorkflowRole == model.WorkflowRoleControl {
				return actionError(errors.New("Coordinator cannot change control task dependencies"))
			}
			if dependent.CurrentRunID != nil || dependent.Status == model.TaskStatusRunning ||
				dependent.Status == model.TaskStatusDone || dependent.Status == model.TaskStatusArchived {
				return actionError(fmt.Errorf(
					"Coordinator cannot change prerequisites for task %s in status %s",
					dependent.ID, dependent.Status,
				))
			}
			key := coordinationDependencyKey(action.PrerequisiteID, action.DependentID)
			if action.Kind == coordinationActionAddDependency {
				if prerequisite.Status == model.TaskStatusArchived {
					return actionError(errors.New("cannot add an archived prerequisite"))
				}
				if plan.dependencies[key] {
					return actionError(errors.New("dependency already exists"))
				}
				if coordinationReachable(plan.dependencyGraph, action.DependentID, action.PrerequisiteID) {
					return actionError(errors.New("dependency would create a cycle"))
				}
				plan.dependencies[key] = true
				coordinationAddEdge(plan.dependencyGraph, action.PrerequisiteID, action.DependentID)
			} else {
				if !plan.dependencies[key] {
					return actionError(errors.New("dependency no longer exists"))
				}
				delete(plan.dependencies, key)
				coordinationRemoveEdge(plan.dependencyGraph, action.PrerequisiteID, action.DependentID)
			}
		case coordinationActionCreateTask:
			taskID, existing, err := resolveCoordinationRecoveryTask(
				ctx, tx, incident.Board, incident.ID, action.Task,
			)
			if err != nil {
				return actionError(err)
			}
			if existing {
				existingTask, err := requireTask(ctx, tx, taskID)
				if err != nil {
					return actionError(err)
				}
				if err := validateIdempotentRecoveryTask(existingTask, incident.Board, action.Task); err != nil {
					return actionError(err)
				}
				if err := requireMutable(existingTask); err != nil {
					return actionError(err)
				}
				plan.tasks[taskID] = existingTask
			}
			plan.createTaskIDs[action.Task.Key] = taskID
			plan.createExisting[action.Task.Key] = existing
			for _, id := range action.Task.Prerequisites {
				task, err := taskAtVersion(id, action.ExpectedTaskVersions[id])
				if err != nil {
					return actionError(err)
				}
				if task.WorkflowRole == model.WorkflowRoleControl {
					return actionError(fmt.Errorf("control task %s cannot be a recovery prerequisite", id))
				}
				key := coordinationDependencyKey(id, taskID)
				if !plan.dependencies[key] {
					if coordinationReachable(plan.dependencyGraph, taskID, id) {
						return actionError(errors.New("created task prerequisites would create a cycle"))
					}
					plan.dependencies[key] = true
					coordinationAddEdge(plan.dependencyGraph, id, taskID)
				}
			}
			for _, id := range action.Task.Dependents {
				task, err := taskAtVersion(id, action.ExpectedTaskVersions[id])
				if err != nil {
					return actionError(err)
				}
				if task.WorkflowRole == model.WorkflowRoleControl {
					return actionError(fmt.Errorf("control task %s cannot depend on recovery work", id))
				}
				if task.CurrentRunID != nil || task.Status == model.TaskStatusRunning ||
					task.Status == model.TaskStatusDone || task.Status == model.TaskStatusArchived {
					return actionError(fmt.Errorf(
						"recovery work cannot become a prerequisite of task %s in status %s", id, task.Status,
					))
				}
				key := coordinationDependencyKey(taskID, id)
				if !plan.dependencies[key] {
					if coordinationReachable(plan.dependencyGraph, id, taskID) {
						return actionError(errors.New("created task dependents would create a cycle"))
					}
					plan.dependencies[key] = true
					coordinationAddEdge(plan.dependencyGraph, taskID, id)
				}
			}
			if parentID := action.Task.ParentTaskID; parentID != "" {
				parent, err := taskAtVersion(parentID, action.ExpectedTaskVersions[parentID])
				if err != nil {
					return actionError(err)
				}
				if parent.WorkflowRole == model.WorkflowRoleControl {
					return actionError(fmt.Errorf("control task %s cannot parent recovery work", parentID))
				}
				if parent.Status == model.TaskStatusArchived {
					return actionError(fmt.Errorf("archived task %s cannot parent recovery work", parentID))
				}
				if existingParent, ok := plan.hierarchyParents[taskID]; ok && existingParent != parentID {
					return actionError(fmt.Errorf(
						"idempotent recovery task %s already has parent %s", taskID, existingParent,
					))
				}
				if !coordinationHasEdge(plan.hierarchyGraph, parentID, taskID) {
					if coordinationReachable(plan.hierarchyGraph, taskID, parentID) {
						return actionError(errors.New("created task parent would create a hierarchy cycle"))
					}
					plan.hierarchyParents[taskID] = parentID
					coordinationAddEdge(plan.hierarchyGraph, parentID, taskID)
				}
			} else if existingParent, ok := plan.hierarchyParents[taskID]; existing && ok {
				return actionError(fmt.Errorf(
					"idempotent recovery task %s already has parent %s not declared by the proposal",
					taskID, existingParent,
				))
			}
		}
	}
	return plan, nil
}

func resolveCoordinationRecoveryTask(
	ctx context.Context,
	tx *sql.Tx,
	board, incidentID string,
	draft *coordinationTaskDraft,
) (string, bool, error) {
	key := coordinationRecoveryIdempotencyKey(incidentID, draft.Key)
	var taskID string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM tasks
		WHERE board = ? AND idempotency_key = ? AND status <> 'archived'
	`, board, key).Scan(&taskID)
	if err == nil {
		return taskID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	// A synthetic ID lets cycle checks include all proposed edges before the
	// transaction writes the real generated task ID.
	return "proposal:" + incidentID + ":" + draft.Key, false, nil
}

func validateIdempotentRecoveryTask(task model.Task, board string, draft *coordinationTaskDraft) error {
	if task.Board != board || task.Title != draft.Title || task.Body != draft.Body ||
		task.Assignee == nil || *task.Assignee != draft.Assignee || task.Runtime != draft.Runtime ||
		task.WorkflowRole != draft.WorkflowRole || task.Priority != draft.Priority {
		return fmt.Errorf(
			"idempotent recovery task %s no longer matches create_task key %s", task.ID, draft.Key,
		)
	}
	return nil
}

func coordinationDependencyKey(prerequisiteID, dependentID string) string {
	return prerequisiteID + "\x00" + dependentID
}

func coordinationAddEdge(graph map[string]map[string]bool, from, to string) {
	if graph[from] == nil {
		graph[from] = map[string]bool{}
	}
	graph[from][to] = true
}

func coordinationRemoveEdge(graph map[string]map[string]bool, from, to string) {
	delete(graph[from], to)
}

func coordinationHasEdge(graph map[string]map[string]bool, from, to string) bool {
	return graph[from] != nil && graph[from][to]
}

func coordinationReachable(graph map[string]map[string]bool, start, target string) bool {
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
		for next := range graph[current] {
			queue = append(queue, next)
		}
	}
	return false
}

func (s *Store) ApplyCoordinationProposal(
	ctx context.Context,
	proposalID string,
	input ApplyCoordinationProposalInput,
) (ApplyCoordinationProposalResult, error) {
	proposalID = strings.TrimSpace(proposalID)
	if proposalID == "" {
		return ApplyCoordinationProposalResult{}, errors.New("coordination proposal application requires a proposal ID")
	}
	if input.ExpectedGraphRevision == nil {
		return ApplyCoordinationProposalResult{}, errors.New(
			"coordination proposal application requires an expected graph revision",
		)
	}
	expectedProposalStatus, expectedIncidentStatus, err := expectedCoordinationApplyStates(input.Authorization)
	if err != nil {
		return ApplyCoordinationProposalResult{}, err
	}
	current, currentTimestamp, err := normalizeCoordinationClaimTime(input.Current)
	if err != nil {
		return ApplyCoordinationProposalResult{}, err
	}
	var result ApplyCoordinationProposalResult
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		proposal, incident, err := proposalWithIncident(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if proposal.Status != expectedProposalStatus {
			return &CoordinationStateConflictError{
				Kind: "proposal", ID: proposal.ID,
				Expected: string(expectedProposalStatus), Actual: string(proposal.Status),
			}
		}
		if incident.Status != expectedIncidentStatus {
			return &CoordinationStateConflictError{
				Kind: "incident", ID: incident.ID,
				Expected: string(expectedIncidentStatus), Actual: string(incident.Status),
			}
		}
		expectedRevision := *input.ExpectedGraphRevision
		if proposal.ExpectedGraphRevision != expectedRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: expectedRevision, Actual: proposal.ExpectedGraphRevision,
			}
		}
		if incident.GraphRevision != expectedRevision {
			return &GraphRevisionConflictError{
				Board: incident.Board, Expected: expectedRevision, Actual: incident.GraphRevision,
			}
		}
		state, err := requireBoardGraphRevision(ctx, tx, incident.Board, expectedRevision)
		if err != nil {
			return err
		}
		if !emptyJSONArray(proposal.ValidationErrors) {
			return errors.New("cannot apply a coordination proposal with validation errors")
		}
		if input.Authorization == CoordinationApplyValidatedAuto {
			if incident.ClaimToken == "" || incident.ClaimExpiresAt == nil {
				return fmt.Errorf("coordinating incident %s has no claim lease", incident.ID)
			}
			if strings.TrimSpace(input.ClaimToken) != incident.ClaimToken {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
			}
			expired, err := coordinationIncidentClaimExpired(incident, current)
			if err != nil {
				return err
			}
			if expired {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimExpired, incident.ID)
			}
		} else if incident.ClaimToken != "" || incident.ClaimExpiresAt != nil {
			return fmt.Errorf("approved incident %s unexpectedly retains a claim lease", incident.ID)
		}

		actions, err := decodeCoordinationActions(proposal.Actions)
		if err != nil {
			return err
		}
		plan, err := prevalidateCoordinationApply(ctx, tx, incident, actions)
		if err != nil {
			return err
		}
		createdTaskIDs, graphChanged, err := s.executeCoordinationApply(ctx, tx, incident, plan)
		if err != nil {
			return err
		}
		if err := requireCoordinationIncidentPostcondition(ctx, tx, incident); err != nil {
			return err
		}
		if graphChanged {
			state, err = bumpBoardGraphRevision(ctx, tx, incident.Board)
			if err != nil {
				return err
			}
		}

		appliedAt := now()
		proposalUpdate, err := tx.ExecContext(ctx, `
			UPDATE coordination_proposals
			SET status = 'applied', updated_at = ?, applied_at = ?
			WHERE id = ? AND status = ? AND expected_graph_revision = ? AND applied_at IS NULL
		`, appliedAt, appliedAt, proposal.ID, expectedProposalStatus, expectedRevision)
		if err != nil {
			return err
		}
		changed, err := proposalUpdate.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return &CoordinationStateConflictError{
				Kind: "proposal", ID: proposal.ID,
				Expected: string(expectedProposalStatus), Actual: "changed while applying",
			}
		}

		incidentStatement := `
			UPDATE coordination_incidents
			SET status = 'resolved', claim_token = NULL, claim_expires_at = NULL, updated_at = ?
			WHERE id = ? AND status = ? AND graph_revision = ?`
		incidentArguments := []any{appliedAt, incident.ID, expectedIncidentStatus, expectedRevision}
		if input.Authorization == CoordinationApplyValidatedAuto {
			incidentStatement += " AND claim_token = ? AND claim_expires_at = ? AND claim_expires_at > ?"
			incidentArguments = append(
				incidentArguments, incident.ClaimToken, *incident.ClaimExpiresAt, currentTimestamp,
			)
		} else {
			incidentStatement += " AND claim_token IS NULL AND claim_expires_at IS NULL"
		}
		incidentUpdate, err := tx.ExecContext(ctx, incidentStatement, incidentArguments...)
		if err != nil {
			return err
		}
		changed, err = incidentUpdate.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			if input.Authorization == CoordinationApplyValidatedAuto {
				return fmt.Errorf("%w: %s", ErrCoordinationClaimNotOwner, incident.ID)
			}
			return &CoordinationStateConflictError{
				Kind: "incident", ID: incident.ID,
				Expected: string(expectedIncidentStatus), Actual: "changed while applying",
			}
		}

		proposal.Status, proposal.UpdatedAt, proposal.AppliedAt =
			model.CoordinationProposalApplied, appliedAt, &appliedAt
		incident.Status, incident.ClaimToken, incident.ClaimExpiresAt, incident.UpdatedAt =
			model.CoordinationIncidentResolved, "", nil, appliedAt
		result = ApplyCoordinationProposalResult{
			Proposal: proposal, Incident: incident, GraphRevision: state.Revision,
			CreatedTaskIDs: createdTaskIDs,
		}
		return nil
	})
	return result, err
}

type coordinationIntegrationIncidentDetails struct {
	BlockKind model.BlockKind `json:"blockKind"`
	Reason    string          `json:"reason"`
}

func coordinationIntegrationReasonMatches(actual, expected string) bool {
	actual, expected = strings.TrimSpace(actual), strings.TrimSpace(expected)
	if actual == expected {
		return actual != ""
	}
	const truncationSuffix = "…"
	return strings.HasSuffix(expected, truncationSuffix) &&
		strings.HasPrefix(actual, strings.TrimSuffix(expected, truncationSuffix))
}

// requireCoordinationIncidentPostcondition prevents an applied proposal from
// resolving an integration incident while the exact block that opened it is
// still present. This store-level check protects approved/manual callers even
// when they bypass the Coordinator's snapshot validator.
func requireCoordinationIncidentPostcondition(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
) error {
	switch incident.Trigger {
	case model.CoordinationTriggerRunInvariant:
		return requireCoordinationRunInvariantPostcondition(
			ctx,
			tx,
			incident,
		)
	case model.CoordinationTriggerIntegrationConflict:
		// Continue with the integration-specific postcondition below.
	default:
		return nil
	}
	if incident.TaskID == nil || strings.TrimSpace(*incident.TaskID) == "" {
		return errors.New("integration coordination incident has no focus task")
	}
	task, err := requireTask(ctx, tx, strings.TrimSpace(*incident.TaskID))
	if err != nil {
		return err
	}
	if task.Status == model.TaskStatusDone ||
		task.Status == model.TaskStatusArchived {
		return nil
	}
	if task.Status == model.TaskStatusRunning {
		return fmt.Errorf(
			"integration coordination incident %s remains active: task %s is still running",
			incident.ID,
			task.ID,
		)
	}
	if task.Status != model.TaskStatusBlocked &&
		task.Status != model.TaskStatusTriage {
		return nil
	}
	var expected coordinationIntegrationIncidentDetails
	if err := json.Unmarshal(incident.Details, &expected); err != nil {
		return fmt.Errorf(
			"decode integration coordination incident %s postcondition: %w",
			incident.ID,
			err,
		)
	}
	kindMatches := expected.BlockKind == "" ||
		(task.BlockKind != nil && *task.BlockKind == expected.BlockKind)
	reasonMatches := strings.TrimSpace(expected.Reason) == "" ||
		(task.BlockReason != nil &&
			coordinationIntegrationReasonMatches(*task.BlockReason, expected.Reason))
	if kindMatches && reasonMatches {
		return fmt.Errorf(
			"integration coordination incident %s remains active: task %s retains its integration block",
			incident.ID,
			task.ID,
		)
	}
	return nil
}

type coordinationRunInvariantIncidentDetails struct {
	CurrentRunID    string                        `json:"currentRunId"`
	DiagnosticCode  string                        `json:"diagnosticCode"`
	FenceGeneration int                           `json:"fenceGeneration"`
	CheckpointID    string                        `json:"checkpointId"`
	CheckpointState model.RecoveryCheckpointState `json:"checkpointState"`
	RunStatus       model.RunStatus               `json:"runStatus"`
	Reason          string                        `json:"reason"`
}

// requireCoordinationRunInvariantPostcondition prevents a Coordinator
// proposal—including an explicitly approved one—from declaring an unsafe
// process-ownership fence resolved. Coordinator actions cannot prove that an
// unverifiable process tree or host Git writer stopped; only deterministic
// recovery may remove the fence after ownership actually quiesces.
func requireCoordinationRunInvariantPostcondition(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
) error {
	if incident.TaskID == nil || strings.TrimSpace(*incident.TaskID) == "" {
		return errors.New("run-invariant coordination incident has no focus task")
	}
	task, err := requireTask(ctx, tx, strings.TrimSpace(*incident.TaskID))
	if err != nil {
		return err
	}
	var expected coordinationRunInvariantIncidentDetails
	if len(incident.Details) > 0 {
		if err := json.Unmarshal(incident.Details, &expected); err != nil {
			return fmt.Errorf(
				"decode run-invariant coordination incident %s postcondition: %w",
				incident.ID,
				err,
			)
		}
	}
	switch strings.TrimSpace(expected.Reason) {
	case "operator_recovery_required":
		return requireCoordinationOperatorRecoveryPostcondition(
			ctx,
			tx,
			incident,
			expected,
		)
	case "recovery_checkpoint_adoption_exhausted":
		return requireCoordinationCheckpointPostcondition(
			ctx,
			tx,
			incident,
			task,
			expected,
		)
	case "current_run_on_non_running_task",
		"running_task_without_current_run",
		"referenced_run_missing",
		"referenced_run_belongs_to_another_task",
		"referenced_run_terminal",
		"running_owner_missing_from_active_runs":
		return requireCoordinationRunOwnershipPostcondition(
			ctx,
			tx,
			incident,
			task,
			expected,
		)
	default:
		return fmt.Errorf(
			"run-invariant coordination incident %s has unsupported safety reason %q",
			incident.ID,
			expected.Reason,
		)
	}
}

func requireCoordinationOperatorRecoveryPostcondition(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
	expected coordinationRunInvariantIncidentDetails,
) error {
	runID := strings.TrimSpace(expected.CurrentRunID)
	if runID == "" || expected.FenceGeneration < 1 {
		return fmt.Errorf(
			"run-invariant coordination incident %s has incomplete operator fence identity",
			incident.ID,
		)
	}
	var requiresOperator bool
	var diagnosticCode string
	var fenceGeneration int
	err := tx.QueryRowContext(
		ctx,
		`SELECT requires_operator, COALESCE(diagnostic_code, ''), fence_generation
		 FROM run_reclaim_requests
		 WHERE run_id = ?`,
		runID,
	).Scan(&requiresOperator, &diagnosticCode, &fenceGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !requiresOperator ||
		fenceGeneration != expected.FenceGeneration ||
		(strings.TrimSpace(expected.DiagnosticCode) != "" &&
			strings.TrimSpace(expected.DiagnosticCode) != diagnosticCode) {
		return nil
	}
	return fmt.Errorf(
		"run-invariant coordination incident %s remains active: run %s retains operator recovery fence generation %d (%s)",
		incident.ID,
		runID,
		fenceGeneration,
		diagnosticCode,
	)
}

func requireCoordinationCheckpointPostcondition(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
	task model.Task,
	expected coordinationRunInvariantIncidentDetails,
) error {
	checkpointID := strings.TrimSpace(expected.CheckpointID)
	if checkpointID == "" {
		return fmt.Errorf(
			"run-invariant coordination incident %s has no checkpoint identity",
			incident.ID,
		)
	}
	var state model.RecoveryCheckpointState
	err := tx.QueryRowContext(
		ctx,
		`SELECT state
		 FROM recovery_checkpoints
		 WHERE id = ? AND task_id = ?`,
		checkpointID,
		task.ID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != model.RecoveryCheckpointPending {
		return nil
	}
	return fmt.Errorf(
		"run-invariant coordination incident %s remains active: task %s retains pending recovery checkpoint %s",
		incident.ID,
		task.ID,
		checkpointID,
	)
}

func requireCoordinationRunOwnershipPostcondition(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
	task model.Task,
	expected coordinationRunInvariantIncidentDetails,
) error {
	runID := strings.TrimSpace(expected.CurrentRunID)
	currentRunID := ""
	if task.CurrentRunID != nil {
		currentRunID = strings.TrimSpace(*task.CurrentRunID)
	}
	persists := false
	switch strings.TrimSpace(expected.Reason) {
	case "current_run_on_non_running_task":
		persists = task.Status != model.TaskStatusRunning &&
			currentRunID != "" &&
			(runID == "" || currentRunID == runID)
	case "running_task_without_current_run":
		persists = task.Status == model.TaskStatusRunning && currentRunID == ""
	default:
		if runID == "" {
			return fmt.Errorf(
				"run-invariant coordination incident %s has no referenced run identity",
				incident.ID,
			)
		}
		if task.Status != model.TaskStatusRunning || currentRunID != runID {
			return nil
		}
		var ownerTaskID string
		var runStatus model.RunStatus
		err := tx.QueryRowContext(
			ctx,
			`SELECT task_id, status FROM task_runs WHERE id = ?`,
			runID,
		).Scan(&ownerTaskID, &runStatus)
		switch strings.TrimSpace(expected.Reason) {
		case "referenced_run_missing":
			persists = errors.Is(err, sql.ErrNoRows)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		case "referenced_run_belongs_to_another_task":
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			persists = ownerTaskID != task.ID
		case "referenced_run_terminal":
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			persists = ownerTaskID == task.ID &&
				runStatus != model.RunStatusRunning &&
				(expected.RunStatus == "" || runStatus == expected.RunStatus)
		case "running_owner_missing_from_active_runs":
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			// A task/run pair satisfying the exact active-run join is no
			// longer missing. Any remaining mismatch keeps the incident open.
			persists = ownerTaskID != task.ID ||
				runStatus != model.RunStatusRunning
		}
	}
	if !persists {
		return nil
	}
	return fmt.Errorf(
		"run-invariant coordination incident %s remains active: task %s retains %s",
		incident.ID,
		task.ID,
		expected.Reason,
	)
}

func (s *Store) executeCoordinationApply(
	ctx context.Context,
	tx *sql.Tx,
	incident model.CoordinationIncident,
	plan coordinationApplyPlan,
) (map[string]string, bool, error) {
	createdTaskIDs := map[string]string{}
	graphChanged := false
	for index, action := range plan.actions {
		fail := func(err error) (map[string]string, bool, error) {
			return nil, false, fmt.Errorf("apply coordination action %d (%s): %w", index+1, action.Kind, err)
		}
		switch action.Kind {
		case coordinationActionSetRoute:
			assignee := action.Assignee
			timestamp := now()
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks SET assignee = ?, runtime = ?, updated_at = ? WHERE id = ?
			`, assignee, action.Runtime, timestamp, action.TaskID); err != nil {
				return fail(err)
			}
			if err := appendEvent(ctx, tx, action.TaskID, "coordination_route_updated", map[string]any{
				"incidentId": incident.ID, "proposalReason": action.Reason,
				"assignee": assignee, "runtime": action.Runtime,
			}, nil); err != nil {
				return fail(err)
			}
			if err := recomputeReady(ctx, tx, action.TaskID); err != nil {
				return fail(err)
			}
		case coordinationActionUpdatePriority:
			if _, err := tx.ExecContext(ctx,
				"UPDATE tasks SET priority = ?, updated_at = ? WHERE id = ?",
				*action.Priority, now(), action.TaskID,
			); err != nil {
				return fail(err)
			}
			if err := appendEvent(ctx, tx, action.TaskID, "coordination_priority_updated", map[string]any{
				"incidentId": incident.ID, "proposalReason": action.Reason, "priority": *action.Priority,
			}, nil); err != nil {
				return fail(err)
			}
		case coordinationActionUnblockTask:
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'todo', scheduled_at = NULL, failure_count = 0,
					block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ?
				WHERE id = ?
			`, now(), action.TaskID); err != nil {
				return fail(err)
			}
			if err := appendEvent(ctx, tx, action.TaskID, "coordination_unblocked", map[string]any{
				"incidentId": incident.ID, "proposalReason": action.Reason,
			}, nil); err != nil {
				return fail(err)
			}
			if err := recomputeReady(ctx, tx, action.TaskID); err != nil {
				return fail(err)
			}
		case coordinationActionMoveToTriage:
			previous, err := requireTask(ctx, tx, action.TaskID)
			if err != nil {
				return fail(err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET status = 'triage', scheduled_at = NULL, failure_count = 0,
					block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ?
				WHERE id = ?
			`, now(), action.TaskID); err != nil {
				return fail(err)
			}
			if err := appendEvent(ctx, tx, action.TaskID, "coordination_moved_to_triage", map[string]any{
				"incidentId": incident.ID, "proposalReason": action.Reason,
				"previousScheduledAt": previous.ScheduledAt, "previousFailureCount": previous.FailureCount,
				"previousBlockKind": previous.BlockKind, "previousBlockReason": previous.BlockReason,
				"previousBlockRecurrences": previous.BlockRecurrences,
			}, nil); err != nil {
				return fail(err)
			}
		case coordinationActionAddDependency:
			changed, err := linkTasks(ctx, tx, action.PrerequisiteID, action.DependentID)
			if err != nil {
				return fail(err)
			}
			graphChanged = graphChanged || changed
		case coordinationActionRemoveDependency:
			deleteResult, err := tx.ExecContext(ctx,
				"DELETE FROM task_links WHERE parent_id = ? AND child_id = ?",
				action.PrerequisiteID, action.DependentID,
			)
			if err != nil {
				return fail(err)
			}
			changed, err := deleteResult.RowsAffected()
			if err != nil {
				return fail(err)
			}
			if changed != 1 {
				return fail(errors.New("dependency disappeared while applying"))
			}
			graphChanged = true
			if err := appendEvent(ctx, tx, action.DependentID, "unlinked", map[string]any{
				"parentId": action.PrerequisiteID, "incidentId": incident.ID,
				"proposalReason": action.Reason,
			}, nil); err != nil {
				return fail(err)
			}
			if err := recomputeReady(ctx, tx, action.DependentID); err != nil {
				return fail(err)
			}
		case coordinationActionCreateTask:
			draft := action.Task
			taskID := plan.createTaskIDs[draft.Key]
			if !plan.createExisting[draft.Key] {
				idempotencyKey := coordinationRecoveryIdempotencyKey(incident.ID, draft.Key)
				assignee := draft.Assignee
				var err error
				taskID, err = s.createTask(ctx, tx, CreateTaskInput{
					Title: draft.Title, Body: draft.Body, Board: incident.Board,
					IdempotencyKey: &idempotencyKey, Assignee: &assignee, Runtime: draft.Runtime,
					Priority: draft.Priority, WorkflowRole: draft.WorkflowRole,
				})
				if err != nil {
					return fail(err)
				}
				graphChanged = true
			}
			createdTaskIDs[draft.Key] = taskID
			for _, prerequisiteID := range draft.Prerequisites {
				changed, err := linkTasks(ctx, tx, prerequisiteID, taskID)
				if err != nil {
					return fail(err)
				}
				graphChanged = graphChanged || changed
			}
			for _, dependentID := range draft.Dependents {
				changed, err := linkTasks(ctx, tx, taskID, dependentID)
				if err != nil {
					return fail(err)
				}
				graphChanged = graphChanged || changed
			}
			if draft.ParentTaskID != "" {
				changed, err := setSubtask(ctx, tx, draft.ParentTaskID, taskID, nil)
				if err != nil {
					return fail(err)
				}
				graphChanged = graphChanged || changed
			}
		}
	}
	return createdTaskIDs, graphChanged, nil
}
