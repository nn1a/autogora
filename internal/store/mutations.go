package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func validateSatisfyingRun(ctx context.Context, q querier, parentID string, satisfiedRunID *string) error {
	if satisfiedRunID != nil {
		var runTaskID, status string
		if err := q.QueryRowContext(ctx, "SELECT task_id, status FROM task_runs WHERE id = ?", *satisfiedRunID).Scan(&runTaskID, &status); err != nil {
			return err
		}
		if runTaskID != parentID || model.RunStatus(status) != model.RunStatusCompleted {
			return fmt.Errorf("satisfying run %s is not a completed run for prerequisite %s", *satisfiedRunID, parentID)
		}
	}
	return nil
}

func satisfyOutgoingDependencies(ctx context.Context, q querier, parentID, satisfiedAt string, satisfiedRunID *string) error {
	if err := validateSatisfyingRun(ctx, q, parentID, satisfiedRunID); err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, "SELECT child_id FROM task_links WHERE parent_id = ? AND satisfied_at IS NULL", parentID)
	if err != nil {
		return err
	}
	children := make([]string, 0)
	for rows.Next() {
		var childID string
		if err := rows.Scan(&childID); err != nil {
			rows.Close()
			return err
		}
		children = append(children, childID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE task_links SET satisfied_at = ?, satisfied_run_id = ?
		WHERE parent_id = ? AND satisfied_at IS NULL`, satisfiedAt, nullableString(satisfiedRunID), parentID); err != nil {
		return err
	}
	for _, childID := range children {
		if err := appendEvent(ctx, q, childID, "dependency_satisfied", map[string]any{
			"parentId": parentID, "satisfiedAt": satisfiedAt, "satisfiedRunId": satisfiedRunID,
		}, satisfiedRunID); err != nil {
			return err
		}
		if err := recomputeReady(ctx, q, childID); err != nil {
			return err
		}
	}
	return nil
}

func assertHierarchyAcyclic(ctx context.Context, q querier, parentID, childID string) error {
	if parentID == childID {
		return errors.New("a task cannot be its own subtask")
	}
	var found int
	err := q.QueryRowContext(ctx, `
		WITH RECURSIVE descendants(id) AS (
			SELECT child_id FROM task_hierarchy WHERE parent_id = ?
			UNION
			SELECT h.child_id FROM task_hierarchy h JOIN descendants d ON h.parent_id = d.id
		)
		SELECT 1 FROM descendants WHERE id = ? LIMIT 1
	`, childID, parentID).Scan(&found)
	if err == nil {
		return fmt.Errorf("task hierarchy cycle rejected: %s -> %s", parentID, childID)
	}
	if err != sql.ErrNoRows {
		return err
	}
	return nil
}

func setSubtask(ctx context.Context, q querier, parentID, childID string, position *int) (bool, error) {
	parent, err := requireTask(ctx, q, parentID)
	if err != nil {
		return false, err
	}
	child, err := requireTask(ctx, q, childID)
	if err != nil {
		return false, err
	}
	if parent.Board != child.Board {
		return false, errors.New("cross-board task hierarchy is not allowed")
	}
	if err := assertHierarchyAcyclic(ctx, q, parentID, childID); err != nil {
		return false, err
	}
	var existingParent sql.NullString
	var existingPosition sql.NullInt64
	err = q.QueryRowContext(ctx, "SELECT parent_id, position FROM task_hierarchy WHERE child_id = ?", childID).Scan(&existingParent, &existingPosition)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	resolved := 0
	if position != nil {
		resolved = *position
	} else if existingParent.Valid && existingParent.String == parentID {
		resolved = int(existingPosition.Int64)
	} else if err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(position) + 1, 0) FROM task_hierarchy WHERE parent_id = ?", parentID).Scan(&resolved); err != nil {
		return false, err
	}
	if resolved < 0 {
		return false, errors.New("subtask position must be a non-negative integer")
	}
	if existingParent.Valid && existingParent.String == parentID && int(existingPosition.Int64) == resolved {
		return false, nil
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO task_hierarchy(parent_id, child_id, position) VALUES (?, ?, ?)
		ON CONFLICT(child_id) DO UPDATE SET parent_id = excluded.parent_id, position = excluded.position
	`, parentID, childID, resolved); err != nil {
		return false, err
	}
	var previous any
	if existingParent.Valid {
		previous = existingParent.String
	}
	if err := appendEvent(ctx, q, childID, "subtask_linked", map[string]any{
		"parentTaskId": parentID, "position": resolved, "previousParentTaskId": previous,
	}, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) SetSubtaskParent(ctx context.Context, parentID, childID string, position *int) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		changed, err := setSubtask(ctx, tx, parentID, childID, position)
		if err != nil || !changed {
			return err
		}
		parent, err := requireTask(ctx, tx, parentID)
		if err != nil {
			return err
		}
		_, err = bumpBoardGraphRevision(ctx, tx, parent.Board)
		return err
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, childID)
}

func (s *Store) RemoveSubtask(ctx context.Context, parentID, childID string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		parent, err := requireTask(ctx, tx, parentID)
		if err != nil {
			return err
		}
		if _, err := requireTask(ctx, tx, childID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM task_hierarchy WHERE parent_id = ? AND child_id = ?", parentID, childID)
		if err != nil {
			return err
		}
		changes, _ := result.RowsAffected()
		if changes > 0 {
			if err := appendEvent(ctx, tx, childID, "subtask_unlinked", map[string]any{"parentTaskId": parentID}, nil); err != nil {
				return err
			}
			_, err = bumpBoardGraphRevision(ctx, tx, parent.Board)
			return err
		}
		return nil
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, childID)
}

func (s *Store) LinkTasks(ctx context.Context, parentID, childID string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		changed, err := linkTasks(ctx, tx, parentID, childID)
		if err != nil || !changed {
			return err
		}
		parent, err := requireTask(ctx, tx, parentID)
		if err != nil {
			return err
		}
		_, err = bumpBoardGraphRevision(ctx, tx, parent.Board)
		return err
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, childID)
}

func (s *Store) UnlinkTasks(ctx context.Context, parentID, childID string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		parent, err := requireTask(ctx, tx, parentID)
		if err != nil {
			return err
		}
		child, err := requireTask(ctx, tx, childID)
		if err != nil {
			return err
		}
		if child.Status == model.TaskStatusRunning {
			var existing int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_links WHERE parent_id = ? AND child_id = ?", parentID, childID).Scan(&existing); err != nil {
				return err
			}
			if existing == 0 {
				return nil
			}
			return errors.New("cannot remove a prerequisite while the dependent task is running; terminate or finish its active run first")
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM task_links WHERE parent_id = ? AND child_id = ?", parentID, childID)
		if err != nil {
			return err
		}
		changes, _ := result.RowsAffected()
		if changes > 0 {
			if err := appendEvent(ctx, tx, childID, "unlinked", map[string]any{"parentId": parentID}, nil); err != nil {
				return err
			}
			if _, err := bumpBoardGraphRevision(ctx, tx, parent.Board); err != nil {
				return err
			}
		}
		return recomputeReady(ctx, tx, childID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, childID)
}

func (s *Store) SpecifyTask(ctx context.Context, taskID, title, body, author string) (model.TaskDetail, error) {
	return s.SpecifyTaskWithVersion(ctx, taskID, title, body, author, nil)
}

// SpecifyTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use SpecifyTask.
func (s *Store) SpecifyTaskWithVersion(ctx context.Context, taskID, title, body, author string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" {
		return model.TaskDetail{}, errors.New("specified task title cannot be empty")
	}
	if body == "" {
		return model.TaskDetail{}, errors.New("specified task body cannot be empty")
	}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
			return err
		}
		if task.Status != model.TaskStatusTriage {
			return fmt.Errorf("task is not in triage: %s", taskID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET title = ?, body = ?, status = 'todo', block_kind = NULL,
			block_reason = NULL, updated_at = ? WHERE id = ?`, title, body, now(), taskID); err != nil {
			return err
		}
		if strings.TrimSpace(author) == "" {
			author = "specifier"
		}
		return appendEvent(ctx, tx, taskID, "specified", map[string]any{"author": strings.TrimSpace(author), "title": title}, nil)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) ArchiveTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	return s.ArchiveTaskWithVersion(ctx, taskID, nil)
}

// ArchiveTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use ArchiveTask.
func (s *Store) ArchiveTaskWithVersion(ctx context.Context, taskID string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
			return err
		}
		if task.Status == model.TaskStatusArchived {
			return nil
		}
		if task.CurrentRunID != nil {
			return errors.New("cannot archive a running task; terminate the active run first")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'archived', current_run_id = NULL, scheduled_at = NULL, updated_at = ? WHERE id = ?", now(), taskID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "archived", nil, nil); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM notification_subscriptions WHERE task_id = ?", taskID)
		return err
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	return s.DeleteTaskWithVersion(ctx, taskID, nil)
}

// DeleteTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use DeleteTask.
func (s *Store) DeleteTaskWithVersion(ctx context.Context, taskID string, expectedUpdatedAt *string) error {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
			return err
		}
		if task.CurrentRunID != nil {
			return errors.New("cannot delete a running task; terminate the active run first")
		}
		var topologyEdges int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM task_links WHERE parent_id = ? OR child_id = ?) +
				(SELECT COUNT(*) FROM task_hierarchy WHERE parent_id = ? OR child_id = ?)
		`, taskID, taskID, taskID, taskID).Scan(&topologyEdges); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT link.child_id, child.status
			FROM task_links link JOIN tasks child ON child.id = link.child_id
			WHERE link.parent_id = ?`, taskID)
		if err != nil {
			return err
		}
		dependents := []string{}
		for rows.Next() {
			var id string
			var status model.TaskStatus
			if err := rows.Scan(&id, &status); err != nil {
				rows.Close()
				return err
			}
			if status == model.TaskStatusRunning {
				rows.Close()
				return fmt.Errorf("cannot delete prerequisite %s while dependent task %s is running", taskID, id)
			}
			dependents = append(dependents, id)
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", taskID); err != nil {
			return err
		}
		if topologyEdges > 0 {
			if _, err := bumpBoardGraphRevision(ctx, tx, task.Board); err != nil {
				return err
			}
		}
		for _, dependent := range dependents {
			if err := recomputeReady(ctx, tx, dependent); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.attachmentsRoot, taskID))
}

func (s *Store) PromoteTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	return s.PromoteTaskWithVersion(ctx, taskID, nil)
}

// PromoteTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use PromoteTask.
func (s *Store) PromoteTaskWithVersion(ctx context.Context, taskID string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
			return err
		}
		if !slices.Contains([]model.TaskStatus{model.TaskStatusTodo, model.TaskStatusScheduled, model.TaskStatusBlocked, model.TaskStatusTriage, model.TaskStatusReview}, task.Status) {
			return fmt.Errorf("task cannot be promoted from %s", task.Status)
		}
		if task.CurrentRunID != nil {
			return errors.New("cannot promote a running task")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'todo', scheduled_at = NULL, failure_count = 0, updated_at = ? WHERE id = ?", now(), taskID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "promote_requested", nil, nil); err != nil {
			return err
		}
		return recomputeReady(ctx, tx, taskID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) ScheduleTask(ctx context.Context, taskID string, scheduledAt *string, reason string) (model.TaskDetail, error) {
	return s.ScheduleTaskWithVersion(ctx, taskID, scheduledAt, reason, nil)
}

// ScheduleTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use ScheduleTask.
func (s *Store) ScheduleTaskWithVersion(ctx context.Context, taskID string, scheduledAt *string, reason string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	normalized, err := normalizeISO(scheduledAt, "scheduledAt")
	if err != nil {
		return model.TaskDetail{}, err
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
			return err
		}
		if task.CurrentRunID != nil {
			return errors.New("cannot schedule a running task")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'scheduled', scheduled_at = ?, updated_at = ? WHERE id = ?", nullableString(normalized), now(), taskID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "scheduled", map[string]any{"scheduledAt": normalized, "reason": normalizedPointer(&reason)}, nil)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) PromoteDueTasks(ctx context.Context, board string, at time.Time) (int, error) {
	if board == "" {
		board = s.board
	}
	if at.IsZero() {
		at = time.Now()
	}
	timestamp := at.UTC().Format("2006-01-02T15:04:05.000Z")
	count := 0
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks WHERE board = ? AND status = 'scheduled' AND scheduled_at IS NOT NULL AND scheduled_at <= ?", board, timestamp)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?", now(), id); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, id, "schedule_due", map[string]any{"scheduledAt": timestamp}, nil); err != nil {
				return err
			}
			if err := recomputeReady(ctx, tx, id); err != nil {
				return err
			}
		}
		count = len(ids)
		return nil
	})
	return count, err
}

func (s *Store) GarbageCollectEvents(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays < 0 {
		retentionDays = 0
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	var count int64
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "DELETE FROM task_events WHERE created_at < ?", cutoff)
		if err == nil {
			count, err = result.RowsAffected()
		}
		return err
	})
	return count, err
}

func requestsRunningExecutionUpdate(input UpdateTaskInput) bool {
	return input.Title != nil ||
		input.Body != nil ||
		input.Assignee.Set ||
		input.Tenant.Set ||
		input.Runtime != nil ||
		input.Workspace.Set ||
		input.WorkspaceKind != nil ||
		input.Branch.Set ||
		input.ScheduledAt.Set ||
		input.MaxRuntimeSeconds.Set ||
		input.MaxRetries != nil ||
		input.Skills != nil ||
		input.GoalMode != nil ||
		input.GoalMaxTurns != nil ||
		input.WorkflowTemplateID.Set ||
		input.CurrentStepKey.Set ||
		input.Status != nil ||
		input.WorkflowRole != nil
}

func (s *Store) UpdateTask(ctx context.Context, taskID string, input UpdateTaskInput) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, input.ExpectedUpdatedAt); err != nil {
			return err
		}
		if task.Status == model.TaskStatusRunning && requestsRunningExecutionUpdate(input) {
			return errors.New("cannot change task execution settings while running; terminate the active run first (only priority may be changed)")
		}
		if input.Status != nil && *input.Status == model.TaskStatusRunning && task.Status != model.TaskStatusRunning {
			return errors.New("tasks enter running only through an atomic claim")
		}
		ownershipChanged := (input.Assignee.Set && !sameString(input.Assignee.Value, task.Assignee)) ||
			(input.Runtime != nil && *input.Runtime != task.Runtime) ||
			(input.Workspace.Set && !sameString(input.Workspace.Value, task.Workspace)) ||
			(input.WorkspaceKind != nil && *input.WorkspaceKind != task.WorkspaceKind) ||
			(input.Branch.Set && !sameString(input.Branch.Value, task.Branch))
		if task.CurrentRunID != nil && ownershipChanged {
			return errors.New("cannot change task ownership, workspace, or branch while a run is active; terminate the run first")
		}
		if task.CurrentRunID != nil && input.Status != nil && *input.Status != model.TaskStatusRunning {
			return errors.New("cannot change the status of a running task administratively; terminate the active run first")
		}
		updates, values := []string{}, []any{}
		add := func(column string, value any) {
			updates = append(updates, column+" = ?")
			values = append(values, value)
		}
		if input.Title != nil {
			value := strings.TrimSpace(*input.Title)
			if value == "" {
				return errors.New("task title cannot be empty")
			}
			add("title", value)
		}
		if input.Body != nil {
			add("body", *input.Body)
		}
		if input.Assignee.Set {
			add("assignee", nullableString(input.Assignee.Value))
		}
		if input.Tenant.Set {
			add("tenant", nullableString(normalizedPointer(input.Tenant.Value)))
		}
		if input.Runtime != nil {
			if !model.ValidRuntime(*input.Runtime) {
				return fmt.Errorf("invalid runtime: %s", *input.Runtime)
			}
			add("runtime", *input.Runtime)
		}
		if input.Priority != nil {
			add("priority", *input.Priority)
		}
		if input.Workspace.Set {
			add("workspace", nullableString(input.Workspace.Value))
			if input.WorkspaceKind == nil {
				add("workspace_kind", resolveWorkspaceKind(input.Workspace.Value, ""))
			}
		}
		if input.WorkspaceKind != nil {
			add("workspace_kind", *input.WorkspaceKind)
		}
		if input.Branch.Set {
			add("branch", nullableString(input.Branch.Value))
		}
		if input.ScheduledAt.Set {
			normalized, err := normalizeISO(input.ScheduledAt.Value, "scheduledAt")
			if err != nil {
				return err
			}
			add("scheduled_at", nullableString(normalized))
		}
		if input.MaxRuntimeSeconds.Set {
			if input.MaxRuntimeSeconds.Value != nil && *input.MaxRuntimeSeconds.Value < 1 {
				return errors.New("maxRuntimeSeconds must be a positive integer")
			}
			add("max_runtime_seconds", nullableInt(input.MaxRuntimeSeconds.Value))
		}
		if input.MaxRetries != nil {
			if *input.MaxRetries < 1 {
				return errors.New("maxRetries must be a positive integer")
			}
			add("max_retries", *input.MaxRetries)
		}
		if input.Skills != nil {
			encoded, _ := jsonMarshal(normalizeSkills(*input.Skills))
			add("skills_json", encoded)
		}
		if input.GoalMode != nil {
			if *input.GoalMode {
				add("goal_mode", 1)
			} else {
				add("goal_mode", 0)
			}
		}
		if input.GoalMaxTurns != nil {
			if *input.GoalMaxTurns < 1 {
				return errors.New("goalMaxTurns must be a positive integer")
			}
			add("goal_max_turns", *input.GoalMaxTurns)
		}
		if input.WorkflowTemplateID.Set {
			add("workflow_template_id", nullableString(input.WorkflowTemplateID.Value))
		}
		if input.CurrentStepKey.Set {
			add("current_step_key", nullableString(input.CurrentStepKey.Value))
		}
		if input.Status != nil {
			if !model.ValidTaskStatus(*input.Status) {
				return fmt.Errorf("invalid status: %s", *input.Status)
			}
			add("status", *input.Status)
		}
		if input.WorkflowRole != nil {
			if !model.ValidWorkflowRole(*input.WorkflowRole) {
				return fmt.Errorf("invalid workflow role: %s", *input.WorkflowRole)
			}
			add("workflow_role", *input.WorkflowRole)
		}
		if input.Status != nil && *input.Status == model.TaskStatusDone {
			add("failure_count", 0)
			add("block_kind", nil)
			add("block_reason", nil)
			add("block_recurrences", 0)
		}
		if len(updates) == 0 {
			return nil
		}
		updates = append(updates, "updated_at = ?")
		values = append(values, now(), taskID)
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(updates, ", ")+" WHERE id = ?", values...); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "updated", map[string]any{"fields": updates}, nil); err != nil {
			return err
		}
		if input.Status != nil && *input.Status == model.TaskStatusDone && task.Status != model.TaskStatusDone {
			completedAt := now()
			if err := appendEvent(ctx, tx, taskID, "completed", map[string]any{"summary": "Completed by administrative status update"}, nil); err != nil {
				return err
			}
			if err := satisfyOutgoingDependencies(ctx, tx, taskID, completedAt, nil); err != nil {
				return err
			}
		}
		if input.Status != nil && *input.Status == model.TaskStatusArchived {
			if _, err := tx.ExecContext(ctx, "DELETE FROM notification_subscriptions WHERE task_id = ?", taskID); err != nil {
				return err
			}
		}
		if input.Status == nil || slices.Contains([]model.TaskStatus{model.TaskStatusReady, model.TaskStatusTodo, model.TaskStatusScheduled}, *input.Status) {
			return recomputeReady(ctx, tx, taskID)
		}
		return nil
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func sameString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func jsonMarshal(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
