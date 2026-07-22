package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nn1a/kanban/internal/model"
)

type DiagnosticIssue struct {
	Kind   string `json:"kind"`
	TaskID string `json:"taskId"`
	Detail string `json:"detail"`
}

type BoardDiagnostics struct {
	Board      string            `json:"board"`
	Healthy    bool              `json:"healthy"`
	Stats      Stats             `json:"stats"`
	Issues     []DiagnosticIssue `json:"issues"`
	ActiveRuns []ActiveRun       `json:"activeRuns"`
}

type BulkMutation struct {
	Status   *model.TaskStatus
	Assignee OptionalString
	Priority *int
	Archive  bool
	Delete   bool
}

type BulkSuccess struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

type BulkError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

type BulkResult struct {
	OK     []BulkSuccess `json:"ok"`
	Errors []BulkError   `json:"errors"`
}

func (s *Store) Diagnose(ctx context.Context, board string) (BoardDiagnostics, error) {
	if board == "" {
		board = s.board
	}
	issues := []DiagnosticIssue{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, status, current_run_id FROM tasks
		WHERE board = ? AND ((status = 'running' AND current_run_id IS NULL) OR
		(status <> 'running' AND current_run_id IS NOT NULL))`, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	for rows.Next() {
		var id string
		var status model.TaskStatus
		var currentRunID sql.NullString
		if err := rows.Scan(&id, &status, &currentRunID); err != nil {
			rows.Close()
			return BoardDiagnostics{}, err
		}
		value := "null"
		if currentRunID.Valid {
			value = currentRunID.String
		}
		issues = append(issues, DiagnosticIssue{Kind: "run_invariant", TaskID: id, Detail: fmt.Sprintf("status=%s, currentRunId=%s", status, value)})
	}
	if err := rows.Close(); err != nil {
		return BoardDiagnostics{}, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id, assignee, runtime FROM tasks
		WHERE board = ? AND status = 'ready' AND (assignee IS NULL OR runtime = 'manual')`, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	for rows.Next() {
		var id string
		var assignee sql.NullString
		var runtime model.Runtime
		if err := rows.Scan(&id, &assignee, &runtime); err != nil {
			rows.Close()
			return BoardDiagnostics{}, err
		}
		detail := "manual runtime cannot be dispatched"
		if !assignee.Valid {
			detail = "ready task has no assignee"
		}
		issues = append(issues, DiagnosticIssue{Kind: "stranded_in_ready", TaskID: id, Detail: detail})
	}
	if err := rows.Close(); err != nil {
		return BoardDiagnostics{}, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT t.id FROM tasks t
		WHERE t.board = ? AND t.status = 'todo' AND t.assignee IS NOT NULL AND t.runtime <> 'manual'
		AND NOT EXISTS (SELECT 1 FROM task_links l WHERE l.child_id = t.id AND l.satisfied_at IS NULL)`, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return BoardDiagnostics{}, err
		}
		issues = append(issues, DiagnosticIssue{Kind: "promotion_lag", TaskID: id, Detail: "todo task has no open dependency; promote it unless it is intentionally parked for manual review"})
	}
	if err := rows.Close(); err != nil {
		return BoardDiagnostics{}, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT t.id, t.status, GROUP_CONCAT(l.parent_id)
		FROM tasks t JOIN task_links l ON l.child_id = t.id
		WHERE t.board = ? AND t.status IN ('running', 'done') AND l.satisfied_at IS NULL
		GROUP BY t.id, t.status LIMIT 500`, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	for rows.Next() {
		var id, parentIDs string
		var status model.TaskStatus
		if err := rows.Scan(&id, &status, &parentIDs); err != nil {
			rows.Close()
			return BoardDiagnostics{}, err
		}
		kind := "done_with_open_dependency"
		if status == model.TaskStatusRunning {
			kind = "running_with_open_dependency"
		}
		issues = append(issues, DiagnosticIssue{Kind: kind, TaskID: id, Detail: "unsatisfied prerequisites=" + parentIDs})
	}
	if err := rows.Close(); err != nil {
		return BoardDiagnostics{}, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT c.id, p.id, p.status FROM task_links l
		JOIN tasks c ON c.id = l.child_id JOIN tasks p ON p.id = l.parent_id
		WHERE c.board = ? AND l.satisfied_at IS NULL AND c.status NOT IN ('done', 'archived')
		AND p.status IN ('archived', 'blocked', 'triage', 'review')
		ORDER BY c.id, p.id LIMIT 500`, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	for rows.Next() {
		var childID, parentID string
		var status model.TaskStatus
		if err := rows.Scan(&childID, &parentID, &status); err != nil {
			rows.Close()
			return BoardDiagnostics{}, err
		}
		kind := "stalled_prerequisite"
		if status == model.TaskStatusArchived {
			kind = "terminal_prerequisite"
		}
		issues = append(issues, DiagnosticIssue{Kind: kind, TaskID: childID,
			Detail: fmt.Sprintf("prerequisite=%s, status=%s; complete, unblock, or unlink it", parentID, status)})
	}
	if err := rows.Close(); err != nil {
		return BoardDiagnostics{}, err
	}
	stats, err := s.Stats(ctx, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	activeRuns, err := s.ListActiveRuns(ctx, board)
	if err != nil {
		return BoardDiagnostics{}, err
	}
	return BoardDiagnostics{Board: board, Healthy: len(issues) == 0, Stats: stats, Issues: issues, ActiveRuns: activeRuns}, nil
}

func (s *Store) BuildWorkerContext(ctx context.Context, taskID string) (string, error) {
	detail, err := s.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	graph, err := s.RelationshipGraph(ctx, taskID)
	if err != nil {
		return "", err
	}
	nodesByID := map[string]model.RelationshipNode{}
	for _, node := range graph.Nodes {
		nodesByID[node.Task.ID] = node
	}
	focus, ok := nodesByID[taskID]
	if !ok {
		return "", fmt.Errorf("focus task missing from relationship graph: %s", taskID)
	}
	root, ok := nodesByID[graph.RootTaskID]
	if !ok {
		return "", fmt.Errorf("root task missing from relationship graph: %s", graph.RootTaskID)
	}
	rootTask := detail.Task
	if graph.RootTaskID != taskID {
		rootDetail, err := s.GetTask(ctx, graph.RootTaskID)
		if err != nil {
			return "", err
		}
		rootTask = rootDetail.Task
	}
	prerequisites := map[string][]string{}
	for _, dependency := range graph.Dependencies {
		prerequisites[dependency.DependentID] = append(prerequisites[dependency.DependentID], dependency.PrerequisiteID)
	}
	priority := map[string]bool{taskID: true, graph.RootTaskID: true}
	for _, task := range append(append(append([]model.Task{}, detail.Parents...), detail.Children...), detail.Subtasks...) {
		priority[task.ID] = true
	}
	if detail.ParentTask != nil {
		priority[detail.ParentTask.ID] = true
	}
	for _, edge := range graph.Hierarchy {
		if edge.ParentTaskID == graph.RootTaskID {
			priority[edge.SubtaskID] = true
		}
	}
	contextNodes := make([]model.RelationshipNode, 0, min(50, len(graph.Nodes)))
	selected := map[string]bool{}
	appendNodes := func(wantPriority bool) {
		for _, node := range graph.Nodes {
			if len(contextNodes) >= 50 {
				return
			}
			if priority[node.Task.ID] != wantPriority || selected[node.Task.ID] {
				continue
			}
			selected[node.Task.ID] = true
			contextNodes = append(contextNodes, node)
		}
	}
	appendNodes(true)
	appendNodes(false)

	value := func(pointer *string, fallback string) string {
		if pointer == nil {
			return fallback
		}
		return *pointer
	}
	lines := []string{
		"# TaskCircuit task " + detail.Task.ID, "",
		"Title: " + detail.Task.Title,
		"Board: " + detail.Task.Board,
		"Tenant: " + value(detail.Task.Tenant, "(none)"),
		fmt.Sprintf("Assignee/runtime: %s / %s", value(detail.Task.Assignee, "(unassigned)"), detail.Task.Runtime),
		"Status: " + string(detail.Task.Status),
		fmt.Sprintf("Workspace: %s (%s)", value(detail.Task.Workspace, "(not prepared)"), detail.Task.WorkspaceKind),
		"", "## Task body", truncate(orFallback(detail.Task.Body, "(empty)"), 8*1024),
	}
	if graph.RootTaskID != taskID {
		lines = append(lines, "", "## Parent task goal",
			fmt.Sprintf("- %s [%s] %s", root.Task.ID, root.Task.Status, root.Task.Title),
			truncate(orFallback(rootTask.Body, "(empty)"), 4*1024))
	}
	phase := "invalid dependency cycle"
	if focus.Phase >= 0 {
		phase = fmt.Sprintf("%d of %d", focus.Phase+1, graph.TotalPhases)
	}
	lines = append(lines, "", "## Relationship and execution order",
		"Task hierarchy (parent/subtask) is separate from execution dependencies (prerequisite/dependent).",
		"TaskCircuit permits a claim only after every direct prerequisite handoff is satisfied.",
		"Only relationship metadata is shown for other nodes; their bodies, workspaces, attachments, and unfinished results are intentionally excluded.",
		fmt.Sprintf("Hierarchy root: %s — %s", graph.RootTaskID, root.Task.Title),
		"Current phase: "+phase)
	for _, node := range contextNodes {
		role := "related task"
		if node.Task.ID == graph.RootTaskID {
			role = "root"
		} else if node.ParentTaskID != nil {
			role = "subtask of " + *node.ParentTaskID
		}
		current := ""
		if node.Task.ID == taskID {
			current = " ← current"
		}
		phaseValue := "?"
		if node.Phase >= 0 {
			phaseValue = fmt.Sprintf("%d", node.Phase+1)
		}
		requires, unlocks := "none", "none"
		if len(prerequisites[node.Task.ID]) > 0 {
			requires = strings.Join(prerequisites[node.Task.ID], ", ")
		}
		if len(node.Unlocks) > 0 {
			unlocks = strings.Join(node.Unlocks, ", ")
		}
		lines = append(lines,
			fmt.Sprintf("- Phase %s [%s] %s (%s) %s%s", phaseValue, node.Task.Status, node.Task.ID, role, node.Task.Title, current),
			fmt.Sprintf("  Requires: %s; unlocks: %s", requires, unlocks))
	}
	omitted := len(graph.Nodes) - len(contextNodes) + graph.OmittedNodeCount
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("- … %d additional related node(s) omitted from this bounded context; use taskcircuit_graph from an orchestrator/admin session for the bounded topology view.", omitted))
	}
	if len(detail.Parents) > 0 {
		lines = append(lines, "", "## Prerequisite handoffs")
		for _, parent := range detail.Parents {
			parentDetail, err := s.GetTask(ctx, parent.ID)
			if err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("- %s [%s] %s", parent.ID, parent.Status, parent.Title))
			for index := len(parentDetail.Runs) - 1; index >= 0; index-- {
				run := parentDetail.Runs[index]
				if run.Status != model.RunStatusCompleted {
					continue
				}
				if run.Summary != nil {
					lines = append(lines, "  Summary: "+truncate(*run.Summary, 4*1024))
				}
				if run.Metadata != nil {
					encoded, _ := json.Marshal(run.Metadata)
					lines = append(lines, "  Metadata: "+truncate(string(encoded), 4*1024))
				}
				break
			}
		}
	}
	if len(detail.Children) > 0 {
		lines = append(lines, "", "## Completion unlocks")
		for _, dependent := range detail.Children {
			lines = append(lines, fmt.Sprintf("- %s [%s] %s", dependent.ID, dependent.Status, dependent.Title))
		}
	}
	if len(detail.Attachments) > 0 {
		lines = append(lines, "", "## Attachments")
		for _, attachment := range detail.Attachments {
			location := "(unavailable)"
			if attachment.Path != nil {
				location = *attachment.Path
			} else if attachment.URL != nil {
				location = *attachment.URL
			}
			lines = append(lines, fmt.Sprintf("- %s: %s", attachment.Name, location))
		}
	}
	terminalRuns := make([]model.Run, 0, len(detail.Runs))
	for _, run := range detail.Runs {
		if run.Status != model.RunStatusRunning {
			terminalRuns = append(terminalRuns, run)
		}
	}
	if len(terminalRuns) > 10 {
		terminalRuns = terminalRuns[len(terminalRuns)-10:]
	}
	if len(terminalRuns) > 0 {
		lines = append(lines, "", "## Prior attempts")
		for _, run := range terminalRuns {
			line := fmt.Sprintf("- %s: %s", run.ID, run.Status)
			if run.Summary != nil {
				line += " — " + truncate(*run.Summary, 4*1024)
			}
			lines = append(lines, line)
			if run.Error != nil {
				lines = append(lines, "  Error: "+truncate(*run.Error, 4*1024))
			}
		}
	}
	comments := detail.Comments
	if len(comments) > 30 {
		comments = comments[len(comments)-30:]
	}
	if len(comments) > 0 {
		lines = append(lines, "", "## Comments")
		for _, comment := range comments {
			lines = append(lines, fmt.Sprintf("- %s (%s): %s", comment.Author, comment.CreatedAt, truncate(comment.Body, 2*1024)))
		}
	}
	return truncate(strings.Join(lines, "\n"), 96*1024), nil
}

func orFallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (s *Store) BulkMutate(ctx context.Context, taskIDs []string, mutation BulkMutation) BulkResult {
	result := BulkResult{OK: []BulkSuccess{}, Errors: []BulkError{}}
	seen := map[string]bool{}
	for _, taskID := range taskIDs {
		if seen[taskID] {
			continue
		}
		seen[taskID] = true
		var value any
		var err error
		switch {
		case mutation.Delete:
			err = s.DeleteTask(ctx, taskID)
			value = map[string]any{"id": taskID, "deleted": true}
		case mutation.Archive:
			value, err = s.ArchiveTask(ctx, taskID)
		case mutation.Status != nil && *mutation.Status == model.TaskStatusDone:
			value, err = s.CompleteTask(ctx, taskID, CompletionInput{})
		default:
			value, err = s.UpdateTask(ctx, taskID, UpdateTaskInput{Status: mutation.Status, Assignee: mutation.Assignee, Priority: mutation.Priority})
		}
		if err != nil {
			result.Errors = append(result.Errors, BulkError{ID: taskID, Error: err.Error()})
			continue
		}
		result.OK = append(result.OK, BulkSuccess{ID: taskID, Value: value})
	}
	return result
}
