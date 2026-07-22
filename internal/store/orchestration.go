package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type TaskGraphNode struct {
	Key      string        `json:"key"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	Assignee string        `json:"assignee"`
	Runtime  model.Runtime `json:"runtime"`
	Priority *int          `json:"priority,omitempty"`
	Skills   []string      `json:"skills,omitempty"`
}

type TaskGraphDependency struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
}

type TaskGraphInput struct {
	RootTaskID           string
	RootTitle            string
	RootBody             string
	OrchestratorAssignee string
	OrchestratorRuntime  model.Runtime
	AutoPromoteChildren  *bool
	Nodes                []TaskGraphNode
	Dependencies         []TaskGraphDependency
}

type TaskGraphResult struct {
	Root              model.TaskDetail        `json:"root"`
	ChildIDs          []string                `json:"childIds"`
	TasksByKey        map[string]string       `json:"tasksByKey"`
	LeafIDs           []string                `json:"leafIds"`
	RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
}

type SwarmRoute struct {
	Assignee string        `json:"assignee"`
	Runtime  model.Runtime `json:"runtime"`
}

type SwarmInput struct {
	Goal          string
	Workers       []SwarmRoute
	Verifier      SwarmRoute
	Synthesizer   SwarmRoute
	Tenant        *string
	Workspace     *string
	WorkspaceKind model.WorkspaceKind
	Blackboard    map[string]any
}

type SwarmResult struct {
	Root          model.TaskDetail `json:"root"`
	WorkerIDs     []string         `json:"workerIds"`
	VerifierID    string           `json:"verifierId"`
	SynthesizerID string           `json:"synthesizerId"`
}

func (s *Store) ApplyTaskGraph(ctx context.Context, input TaskGraphInput) (TaskGraphResult, error) {
	if len(input.Nodes) == 0 {
		return TaskGraphResult{}, errors.New("a task graph requires at least one child")
	}
	if len(input.Nodes) > 100 {
		return TaskGraphResult{}, errors.New("a task graph cannot exceed 100 children")
	}
	keys := make(map[string]bool, len(input.Nodes))
	for index := range input.Nodes {
		node := &input.Nodes[index]
		node.Key = strings.TrimSpace(node.Key)
		if node.Key == "" || keys[node.Key] {
			return TaskGraphResult{}, fmt.Errorf("task graph keys must be non-empty and unique: %s", node.Key)
		}
		keys[node.Key] = true
		if !model.ValidRuntime(node.Runtime) {
			return TaskGraphResult{}, fmt.Errorf("invalid task graph runtime: %s", node.Runtime)
		}
		node.Assignee = strings.TrimSpace(node.Assignee)
		if node.Assignee == "" {
			return TaskGraphResult{}, fmt.Errorf("task graph node %s has no assignee", node.Key)
		}
	}
	if !model.ValidRuntime(input.OrchestratorRuntime) {
		return TaskGraphResult{}, fmt.Errorf("invalid orchestrator runtime: %s", input.OrchestratorRuntime)
	}
	input.OrchestratorAssignee = strings.TrimSpace(input.OrchestratorAssignee)
	if input.OrchestratorAssignee == "" {
		return TaskGraphResult{}, errors.New("orchestrator assignee cannot be empty")
	}
	for _, dependency := range input.Dependencies {
		if !keys[dependency.Parent] || !keys[dependency.Child] {
			return TaskGraphResult{}, fmt.Errorf("unknown task graph dependency: %s -> %s", dependency.Parent, dependency.Child)
		}
		if dependency.Parent == dependency.Child {
			return TaskGraphResult{}, errors.New("a task graph node cannot depend on itself")
		}
	}

	tasksByKey := make(map[string]string, len(input.Nodes))
	childIDs := make([]string, 0, len(input.Nodes))
	leafIDs := []string{}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		root, err := requireTask(ctx, tx, input.RootTaskID)
		if err != nil {
			return err
		}
		if root.Status != model.TaskStatusTriage {
			return fmt.Errorf("task is not in triage: %s", input.RootTaskID)
		}
		for _, node := range input.Nodes {
			workspace := (*string)(nil)
			if root.WorkspaceKind == model.WorkspaceDir {
				workspace = root.Workspace
			}
			priority := root.Priority
			if node.Priority != nil {
				priority = *node.Priority
			}
			status := model.TaskStatus("")
			if input.AutoPromoteChildren != nil && !*input.AutoPromoteChildren {
				status = model.TaskStatusTodo
			}
			id, err := s.createTask(ctx, tx, CreateTaskInput{
				Title: node.Title, Body: node.Body, Board: root.Board, Tenant: root.Tenant,
				Assignee: &node.Assignee, Runtime: node.Runtime, Priority: priority,
				Workspace: workspace, WorkspaceKind: root.WorkspaceKind,
				MaxRuntimeSeconds: root.MaxRuntimeSeconds, Skills: node.Skills,
				MaxRetries: root.MaxRetries, Status: status,
			})
			if err != nil {
				return err
			}
			tasksByKey[node.Key] = id
			childIDs = append(childIDs, id)
		}
		for position, node := range input.Nodes {
			value := position
			if err := setSubtask(ctx, tx, root.ID, tasksByKey[node.Key], &value); err != nil {
				return err
			}
		}
		for _, dependency := range input.Dependencies {
			if err := linkTasks(ctx, tx, tasksByKey[dependency.Parent], tasksByKey[dependency.Child]); err != nil {
				return err
			}
		}
		nonLeaves := map[string]bool{}
		for _, dependency := range input.Dependencies {
			nonLeaves[dependency.Parent] = true
		}
		for _, node := range input.Nodes {
			if !nonLeaves[node.Key] {
				leafIDs = append(leafIDs, tasksByKey[node.Key])
			}
		}
		title, body := strings.TrimSpace(input.RootTitle), strings.TrimSpace(input.RootBody)
		if title == "" {
			title = root.Title
		}
		if body == "" {
			body = root.Body
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET title = ?, body = ?, assignee = ?, runtime = ?, status = 'todo',
			block_kind = NULL, block_reason = NULL, updated_at = ? WHERE id = ?`,
			title, body, input.OrchestratorAssignee, input.OrchestratorRuntime, now(), root.ID); err != nil {
			return err
		}
		for _, leafID := range leafIDs {
			if err := linkTasks(ctx, tx, leafID, root.ID); err != nil {
				return err
			}
		}
		autoPromote := input.AutoPromoteChildren == nil || *input.AutoPromoteChildren
		subtasks := make([]map[string]any, 0, len(input.Nodes))
		for position, node := range input.Nodes {
			subtasks = append(subtasks, map[string]any{"key": node.Key, "taskId": tasksByKey[node.Key], "position": position})
		}
		return appendEvent(ctx, tx, root.ID, "decomposed", map[string]any{
			"childIds": childIDs, "leafIds": leafIDs, "subtasks": subtasks,
			"dependencies": input.Dependencies, "autoPromoteChildren": autoPromote,
		}, nil)
	})
	if err != nil {
		return TaskGraphResult{}, err
	}
	root, err := s.GetTask(ctx, input.RootTaskID)
	if err != nil {
		return TaskGraphResult{}, err
	}
	graph, err := s.RelationshipGraph(ctx, input.RootTaskID)
	if err != nil {
		return TaskGraphResult{}, err
	}
	return TaskGraphResult{Root: root, ChildIDs: childIDs, TasksByKey: tasksByKey, LeafIDs: leafIDs, RelationshipGraph: graph}, nil
}

func validateSwarmRoute(route SwarmRoute) error {
	if strings.TrimSpace(route.Assignee) == "" {
		return errors.New("swarm assignees cannot be empty")
	}
	if !model.ValidRuntime(route.Runtime) || route.Runtime == model.RuntimeManual {
		return fmt.Errorf("invalid swarm runtime: %s", route.Runtime)
	}
	return nil
}

func (s *Store) CreateSwarm(ctx context.Context, input SwarmInput) (SwarmResult, error) {
	goal := strings.TrimSpace(input.Goal)
	if goal == "" {
		return SwarmResult{}, errors.New("swarm goal cannot be empty")
	}
	if len(input.Workers) == 0 {
		return SwarmResult{}, errors.New("a swarm requires at least one worker")
	}
	if len(input.Workers) > 50 {
		return SwarmResult{}, errors.New("a swarm cannot exceed 50 workers")
	}
	for _, route := range append(append([]SwarmRoute{}, input.Workers...), input.Verifier, input.Synthesizer) {
		if err := validateSwarmRoute(route); err != nil {
			return SwarmResult{}, err
		}
	}
	workspace := input.Workspace
	if input.WorkspaceKind == model.WorkspaceWorktree {
		workspace = nil
	}
	result := SwarmResult{WorkerIDs: []string{}}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		rootID, err := s.createTask(ctx, tx, CreateTaskInput{
			Title: "Swarm blackboard: " + goal, Body: goal, Tenant: input.Tenant,
			Status: model.TaskStatusTodo, Runtime: model.RuntimeManual,
			Workspace: workspace, WorkspaceKind: input.WorkspaceKind,
		})
		if err != nil {
			return err
		}
		root, err := requireTask(ctx, tx, rootID)
		if err != nil {
			return err
		}
		metadata := map[string]any{"goal": goal}
		blackboard := map[string]any{"type": "autogora_swarm_blackboard", "goal": goal}
		for key, value := range input.Blackboard {
			metadata[key], blackboard[key] = value, value
		}
		runID, err := syntheticRun(ctx, tx, root, model.RunStatusCompleted, "Swarm blackboard initialized", metadata, "")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'done', result = ?, updated_at = ? WHERE id = ?", "Swarm blackboard initialized", now(), rootID); err != nil {
			return err
		}
		encodedBlackboard, err := json.Marshal(blackboard)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO task_comments(task_id, author, body, created_at) VALUES (?, 'swarm', ?, ?)", rootID, string(encodedBlackboard), now()); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, rootID, "completed", map[string]any{"summary": "Swarm blackboard initialized"}, &runID); err != nil {
			return err
		}
		result.Root.Task.ID = rootID
		for index, worker := range input.Workers {
			assignee := strings.TrimSpace(worker.Assignee)
			id, err := s.createTask(ctx, tx, CreateTaskInput{
				Title:  fmt.Sprintf("Swarm worker %d (%s): %s", index+1, assignee, goal),
				Body:   "Work independently on this swarm goal. Read the blackboard parent and leave a structured handoff.\n\n" + goal,
				Tenant: input.Tenant, Assignee: &assignee, Runtime: worker.Runtime,
				Workspace: workspace, WorkspaceKind: input.WorkspaceKind, Parents: []string{rootID},
			})
			if err != nil {
				return err
			}
			result.WorkerIDs = append(result.WorkerIDs, id)
		}
		verifierAssignee := strings.TrimSpace(input.Verifier.Assignee)
		result.VerifierID, err = s.createTask(ctx, tx, CreateTaskInput{
			Title:  "Verify swarm results: " + goal,
			Body:   "Review every worker handoff against the shared goal. Identify gaps and provide a clear verification decision.",
			Tenant: input.Tenant, Assignee: &verifierAssignee, Runtime: input.Verifier.Runtime,
			Workspace: workspace, WorkspaceKind: input.WorkspaceKind, Parents: result.WorkerIDs,
		})
		if err != nil {
			return err
		}
		synthesizerAssignee := strings.TrimSpace(input.Synthesizer.Assignee)
		result.SynthesizerID, err = s.createTask(ctx, tx, CreateTaskInput{
			Title:  "Synthesize swarm result: " + goal,
			Body:   "Produce the final deliverable using the verified swarm handoffs and verification decision.",
			Tenant: input.Tenant, Assignee: &synthesizerAssignee, Runtime: input.Synthesizer.Runtime,
			Workspace: workspace, WorkspaceKind: input.WorkspaceKind, Parents: []string{result.VerifierID},
		})
		if err != nil {
			return err
		}
		all := append(append([]string{}, result.WorkerIDs...), result.VerifierID, result.SynthesizerID)
		for position, taskID := range all {
			value := position
			if err := setSubtask(ctx, tx, rootID, taskID, &value); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, rootID, "swarm_created", map[string]any{
			"workerIds": result.WorkerIDs, "verifierId": result.VerifierID, "synthesizerId": result.SynthesizerID,
		}, nil)
	})
	if err != nil {
		return SwarmResult{}, err
	}
	result.Root, err = s.GetTask(ctx, result.Root.Task.ID)
	return result, err
}
