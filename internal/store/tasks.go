package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/model"
)

type CreateTaskInput struct {
	Title              string
	Body               string
	Board              string
	Tenant             *string
	IdempotencyKey     *string
	Assignee           *string
	Runtime            model.Runtime
	Priority           int
	Workspace          *string
	WorkspaceKind      model.WorkspaceKind
	Branch             *string
	Status             model.TaskStatus
	ScheduledAt        *string
	MaxRuntimeSeconds  *int
	Skills             []string
	GoalMode           bool
	GoalMaxTurns       int
	WorkflowTemplateID *string
	CurrentStepKey     *string
	MaxRetries         int
	Parents            []string
}

type UpdateTaskInput struct {
	Title              *string
	Body               *string
	Assignee           OptionalString
	Tenant             OptionalString
	Runtime            *model.Runtime
	Priority           *int
	Workspace          OptionalString
	WorkspaceKind      *model.WorkspaceKind
	Branch             OptionalString
	ScheduledAt        OptionalString
	MaxRuntimeSeconds  OptionalInt
	Skills             *[]string
	GoalMode           *bool
	GoalMaxTurns       *int
	WorkflowTemplateID OptionalString
	CurrentStepKey     OptionalString
	Status             *model.TaskStatus
}

type OptionalString struct {
	Set   bool
	Value *string
}

type OptionalInt struct {
	Set   bool
	Value *int
}

type ListTaskFilter struct {
	Board              string
	Status             model.TaskStatus
	Tenant             string
	Assignee           string
	Runtime            model.Runtime
	WorkflowTemplateID string
	CurrentStepKey     string
	IncludeArchived    bool
	Search             string
	Sort               string
	Limit              int
}

type EventFilter struct {
	TaskID  string
	SinceID *int64
	Kinds   []string
	Limit   int
}

type Stats struct {
	Board      string                   `json:"board"`
	Total      int                      `json:"total"`
	ByStatus   map[model.TaskStatus]int `json:"byStatus"`
	ByAssignee map[string]int           `json:"byAssignee"`
	ByRuntime  map[model.Runtime]int    `json:"byRuntime"`
	ByTenant   map[string]int           `json:"byTenant"`
}

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func newID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

func normalizeISO(value *string, field string) (*string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*value))
	if err != nil {
		return nil, fmt.Errorf("%s must be a valid ISO-8601 date", field)
	}
	result := parsed.UTC().Format("2006-01-02T15:04:05.000Z")
	return &result, nil
}

func normalizeSkills(skills []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(skills))
	for _, skill := range skills {
		skill = strings.TrimSpace(skill)
		if skill != "" && !seen[skill] {
			seen[skill] = true
			result = append(result, skill)
		}
	}
	return result
}

func normalizedPointer(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func resolveWorkspaceKind(workspace *string, explicit model.WorkspaceKind) model.WorkspaceKind {
	if explicit != "" {
		return explicit
	}
	if workspace == nil || *workspace == "" || *workspace == "scratch" {
		return model.WorkspaceScratch
	}
	if *workspace == "worktree" || strings.HasPrefix(*workspace, "worktree:") {
		return model.WorkspaceWorktree
	}
	return model.WorkspaceDir
}

func (s *Store) withWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func requireTask(ctx context.Context, q querier, taskID string) (model.Task, error) {
	task, err := scanTask(q.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, fmt.Errorf("task not found: %s", taskID)
	}
	return task, err
}

func appendEvent(ctx context.Context, q querier, taskID, kind string, payload any, runID *string) error {
	var encoded any
	if payload != nil {
		value, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		encoded = string(value)
	}
	_, err := q.ExecContext(ctx,
		"INSERT INTO task_events(task_id, run_id, kind, payload_json, created_at) VALUES (?, ?, ?, ?, ?)",
		taskID, nullableString(runID), kind, encoded, now(),
	)
	return err
}

func hasOpenParents(ctx context.Context, q querier, taskID string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_links WHERE child_id = ? AND satisfied_at IS NULL", taskID).Scan(&count)
	return count > 0, err
}

type dependencySatisfaction struct {
	at    *string
	runID *string
}

func dependencySatisfactionForParent(ctx context.Context, q querier, parentID string) (dependencySatisfaction, error) {
	parent, err := requireTask(ctx, q, parentID)
	if err != nil {
		return dependencySatisfaction{}, err
	}
	if parent.Status != model.TaskStatusDone && parent.Status != model.TaskStatusArchived {
		return dependencySatisfaction{}, nil
	}
	var completed, runID sql.NullString
	err = q.QueryRowContext(ctx,
		"SELECT created_at, run_id FROM task_events WHERE task_id = ? AND kind = 'completed' ORDER BY id DESC LIMIT 1",
		parentID,
	).Scan(&completed, &runID)
	if err == nil {
		return dependencySatisfaction{at: stringPointer(completed), runID: stringPointer(runID)}, nil
	}
	if err != sql.ErrNoRows {
		return dependencySatisfaction{}, err
	}
	if parent.Status == model.TaskStatusDone {
		return dependencySatisfaction{at: &parent.UpdatedAt}, nil
	}
	return dependencySatisfaction{}, nil
}

func recomputeReady(ctx context.Context, q querier, taskID string) error {
	task, err := requireTask(ctx, q, taskID)
	if err != nil {
		return err
	}
	if slices.Contains([]model.TaskStatus{
		model.TaskStatusTriage, model.TaskStatusRunning, model.TaskStatusBlocked,
		model.TaskStatusReview, model.TaskStatusDone, model.TaskStatusArchived,
	}, task.Status) {
		return nil
	}
	if task.Status == model.TaskStatusScheduled && task.ScheduledAt == nil {
		return nil
	}
	if task.ScheduledAt != nil {
		scheduled, err := time.Parse(time.RFC3339Nano, *task.ScheduledAt)
		if err == nil && scheduled.After(time.Now()) {
			if task.Status != model.TaskStatusScheduled {
				if _, err := q.ExecContext(ctx, "UPDATE tasks SET status = 'scheduled', updated_at = ? WHERE id = ?", now(), taskID); err != nil {
					return err
				}
				return appendEvent(ctx, q, taskID, "scheduled", map[string]any{"scheduledAt": task.ScheduledAt}, nil)
			}
			return nil
		}
	}
	open, err := hasOpenParents(ctx, q, taskID)
	if err != nil {
		return err
	}
	status := model.TaskStatusReady
	if open || task.Assignee == nil || task.Runtime == model.RuntimeManual {
		status = model.TaskStatusTodo
	}
	if status == task.Status {
		return nil
	}
	if _, err := q.ExecContext(ctx, "UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?", status, now(), taskID); err != nil {
		return err
	}
	kind := "dependency_wait"
	if status == model.TaskStatusReady {
		kind = "promoted"
	}
	return appendEvent(ctx, q, taskID, kind, nil, nil)
}

func assertDependencyAcyclic(ctx context.Context, q querier, parentID, childID string) error {
	if parentID == childID {
		return errors.New("a task cannot depend on itself")
	}
	var found int
	err := q.QueryRowContext(ctx, `
		WITH RECURSIVE descendants(id) AS (
			SELECT child_id FROM task_links WHERE parent_id = ?
			UNION
			SELECT l.child_id FROM task_links l JOIN descendants d ON l.parent_id = d.id
		)
		SELECT 1 FROM descendants WHERE id = ? LIMIT 1
	`, childID, parentID).Scan(&found)
	if err == nil {
		return fmt.Errorf("dependency cycle rejected: %s -> %s", parentID, childID)
	}
	if err != sql.ErrNoRows {
		return err
	}
	return nil
}

func linkTasks(ctx context.Context, q querier, parentID, childID string) error {
	parent, err := requireTask(ctx, q, parentID)
	if err != nil {
		return err
	}
	child, err := requireTask(ctx, q, childID)
	if err != nil {
		return err
	}
	if parent.Board != child.Board {
		return errors.New("cross-board dependencies are not allowed")
	}
	if err := assertDependencyAcyclic(ctx, q, parentID, childID); err != nil {
		return err
	}
	satisfaction, err := dependencySatisfactionForParent(ctx, q, parentID)
	if err != nil {
		return err
	}
	if err := validateSatisfyingRun(ctx, q, parentID, satisfaction.runID); err != nil {
		return err
	}
	if satisfaction.at == nil && child.Status == model.TaskStatusRunning {
		return errors.New("cannot add an unfinished prerequisite to a running task; terminate or finish the active run first")
	}
	result, err := q.ExecContext(ctx,
		"INSERT OR IGNORE INTO task_links(parent_id, child_id, satisfied_at, satisfied_run_id) VALUES (?, ?, ?, ?)",
		parentID, childID, nullableString(satisfaction.at), nullableString(satisfaction.runID),
	)
	if err != nil {
		return err
	}
	changes, _ := result.RowsAffected()
	if changes == 0 {
		return nil
	}
	if err := appendEvent(ctx, q, childID, "linked", map[string]any{
		"parentId": parentID, "satisfiedAt": satisfaction.at, "satisfiedRunId": satisfaction.runID,
	}, nil); err != nil {
		return err
	}
	if satisfaction.at == nil && child.Status == model.TaskStatusDone {
		if _, err := q.ExecContext(ctx, `
			UPDATE tasks SET status = 'todo', block_kind = NULL, block_reason = NULL,
			block_recurrences = 0, updated_at = ? WHERE id = ?
		`, now(), childID); err != nil {
			return err
		}
		if err := appendEvent(ctx, q, childID, "reopened_for_dependency", map[string]any{"parentId": parentID}, nil); err != nil {
			return err
		}
	}
	return recomputeReady(ctx, q, childID)
}

func (s *Store) createTask(ctx context.Context, q querier, input CreateTaskInput) (string, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return "", errors.New("task title cannot be empty")
	}
	runtime := input.Runtime
	if runtime == "" {
		runtime = model.RuntimeManual
	}
	if !model.ValidRuntime(runtime) {
		return "", fmt.Errorf("invalid runtime: %s", runtime)
	}
	if input.Status != "" && !model.ValidTaskStatus(input.Status) {
		return "", fmt.Errorf("invalid status: %s", input.Status)
	}
	if input.Status == model.TaskStatusRunning {
		return "", errors.New("tasks enter running only through an atomic claim")
	}
	scheduledAt, err := normalizeISO(input.ScheduledAt, "scheduledAt")
	if err != nil {
		return "", err
	}
	if input.MaxRuntimeSeconds != nil && *input.MaxRuntimeSeconds < 1 {
		return "", errors.New("maxRuntimeSeconds must be a positive integer")
	}
	maxRetries := input.MaxRetries
	if maxRetries == 0 {
		maxRetries = 2
	}
	if maxRetries < 1 {
		return "", errors.New("maxRetries must be a positive integer")
	}
	goalMaxTurns := input.GoalMaxTurns
	if goalMaxTurns == 0 {
		goalMaxTurns = 20
	}
	if goalMaxTurns < 1 {
		return "", errors.New("goalMaxTurns must be a positive integer")
	}
	board := input.Board
	if board == "" {
		board = s.board
	}
	idempotencyKey := normalizedPointer(input.IdempotencyKey)
	if idempotencyKey != nil {
		var existing string
		err := q.QueryRowContext(ctx,
			"SELECT id FROM tasks WHERE board = ? AND idempotency_key = ? AND status <> 'archived'",
			board, *idempotencyKey,
		).Scan(&existing)
		if err == nil {
			return existing, nil
		}
		if err != sql.ErrNoRows {
			return "", err
		}
	}
	status := input.Status
	if status == "" {
		status = model.TaskStatusTodo
		if scheduledAt != nil {
			scheduled, _ := time.Parse(time.RFC3339Nano, *scheduledAt)
			if scheduled.After(time.Now()) {
				status = model.TaskStatusScheduled
			}
		} else if input.Assignee != nil && runtime != model.RuntimeManual && len(input.Parents) == 0 {
			status = model.TaskStatusReady
		}
	}
	skillsJSON, _ := json.Marshal(normalizeSkills(input.Skills))
	taskID := newID("t")
	timestamp := now()
	goalMode := 0
	if input.GoalMode {
		goalMode = 1
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO tasks(
			id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
			priority, workspace, workspace_kind, branch, current_run_id, result,
			scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
			workflow_template_id, current_step_key, block_kind, block_reason,
			block_recurrences, failure_count, max_retries, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, 0, ?, ?, ?)
	`, taskID, board, nullableString(normalizedPointer(input.Tenant)), nullableString(idempotencyKey),
		title, input.Body, nullableString(input.Assignee), runtime, status, input.Priority,
		nullableString(input.Workspace), resolveWorkspaceKind(input.Workspace, input.WorkspaceKind),
		nullableString(input.Branch), nullableString(scheduledAt), nullableInt(input.MaxRuntimeSeconds),
		string(skillsJSON), goalMode, goalMaxTurns, nullableString(input.WorkflowTemplateID),
		nullableString(input.CurrentStepKey), maxRetries, timestamp, timestamp,
	)
	if err != nil {
		return "", err
	}
	if err := appendEvent(ctx, q, taskID, "created", map[string]any{
		"runtime": runtime, "assignee": input.Assignee, "tenant": input.Tenant,
		"status": status, "parents": input.Parents,
	}, nil); err != nil {
		return "", err
	}
	for _, parentID := range input.Parents {
		if err := linkTasks(ctx, q, parentID, taskID); err != nil {
			return "", err
		}
	}
	if input.Status == model.TaskStatusReady {
		open, err := hasOpenParents(ctx, q, taskID)
		if err != nil {
			return "", err
		}
		if open {
			_, err = q.ExecContext(ctx, "UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?", now(), taskID)
		}
	}
	return taskID, err
}

func (s *Store) CreateTask(ctx context.Context, input CreateTaskInput) (model.TaskDetail, error) {
	detail, _, err := s.CreateTaskWithDisposition(ctx, input)
	return detail, err
}

// CreateTaskWithDisposition atomically reports whether this call inserted the
// task or resolved an existing active idempotency key.
func (s *Store) CreateTaskWithDisposition(ctx context.Context, input CreateTaskInput) (model.TaskDetail, bool, error) {
	var taskID string
	created := true
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if key := normalizedPointer(input.IdempotencyKey); key != nil {
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM tasks WHERE board = ? AND idempotency_key = ? AND status <> 'archived'",
				orFallback(input.Board, s.board), *key,
			).Scan(&taskID)
			if err == nil {
				created = false
				return nil
			}
			if err != sql.ErrNoRows {
				return err
			}
		}
		var err error
		taskID, err = s.createTask(ctx, tx, input)
		return err
	})
	if err != nil {
		return model.TaskDetail{}, false, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return detail, created, err
}

// FindTaskByIdempotencyKey returns the active task previously created for an
// external source. Archived tasks are intentionally ignored so an explicitly
// retired import can be brought back as a new task.
func (s *Store) FindTaskByIdempotencyKey(ctx context.Context, key string) (*model.TaskDetail, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("idempotency key cannot be empty")
	}
	var taskID string
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM tasks WHERE board = ? AND idempotency_key = ? AND status <> 'archived'",
		s.board, key,
	).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	detail, err := s.GetTask(ctx, taskID)
	return &detail, err
}

func (s *Store) ListTasks(ctx context.Context, filter ListTaskFilter) ([]model.Task, error) {
	clauses := []string{"board = ?"}
	board := filter.Board
	if board == "" {
		board = s.board
	}
	values := []any{board}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		values = append(values, filter.Status)
	} else if !filter.IncludeArchived {
		clauses = append(clauses, "status <> 'archived'")
	}
	filters := []struct{ column, value string }{
		{"tenant", filter.Tenant}, {"assignee", filter.Assignee},
		{"runtime", string(filter.Runtime)}, {"workflow_template_id", filter.WorkflowTemplateID},
		{"current_step_key", filter.CurrentStepKey},
	}
	for _, item := range filters {
		if item.value != "" {
			clauses = append(clauses, item.column+" = ?")
			values = append(values, item.value)
		}
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		clauses = append(clauses, "(title LIKE ? OR body LIKE ?)")
		values = append(values, "%"+search+"%", "%"+search+"%")
	}
	orders := map[string]string{
		"created": "created_at ASC", "created-desc": "created_at DESC",
		"priority": "priority ASC, created_at ASC", "priority-desc": "priority DESC, created_at ASC",
		"status":   "status ASC, priority DESC, created_at ASC",
		"assignee": "assignee ASC, priority DESC, created_at ASC",
		"title":    "title COLLATE NOCASE ASC", "updated": "updated_at DESC",
	}
	order := orders[filter.Sort]
	if order == "" {
		order = orders["priority-desc"]
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	limit = max(1, min(limit, 500))
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE "+strings.Join(clauses, " AND ")+" ORDER BY "+order+" LIMIT ?",
		values...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func queryTasks(ctx context.Context, q querier, statement string, args ...any) ([]model.Task, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	task, err := requireTask(ctx, s.db, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	parents, err := queryTasks(ctx, s.db, "SELECT "+taskColumnsT+" FROM tasks t JOIN task_links l ON l.parent_id = t.id WHERE l.child_id = ? ORDER BY t.created_at", taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	children, err := queryTasks(ctx, s.db, "SELECT "+taskColumnsT+" FROM tasks t JOIN task_links l ON l.child_id = t.id WHERE l.parent_id = ? ORDER BY t.created_at", taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	var parentTask *model.Task
	parent, err := scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskColumnsT+" FROM tasks t JOIN task_hierarchy h ON h.parent_id = t.id WHERE h.child_id = ?", taskID))
	if err == nil {
		parentTask = &parent
	} else if err != sql.ErrNoRows {
		return model.TaskDetail{}, err
	}
	subtasks, err := queryTasks(ctx, s.db, "SELECT "+taskColumnsT+" FROM tasks t JOIN task_hierarchy h ON h.child_id = t.id WHERE h.parent_id = ? ORDER BY h.position, t.created_at", taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	comments, err := s.listComments(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	attachments, err := s.listAttachments(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	runs, err := s.listRuns(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	runAgentConfigs, err := s.ListTaskRunAgentConfigs(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	workspaces, err := s.listRunWorkspaces(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	terminalRequests, err := s.listTerminalRequests(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	changeSets, err := s.listChangeSets(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	prerequisiteHandoffs, err := s.ListPrerequisiteHandoffs(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	events, err := s.listTaskEvents(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	return model.TaskDetail{
		Task: task, Parents: parents, Children: children, Prerequisites: parents,
		Dependents: children, ParentTask: parentTask, Subtasks: subtasks,
		Comments: comments, Attachments: attachments, Runs: runs, RunAgentConfigs: runAgentConfigs, RunWorkspaces: workspaces,
		TerminalRequests: terminalRequests, ChangeSets: changeSets,
		PrerequisiteHandoffs: prerequisiteHandoffs, Events: events,
	}, nil
}

func (s *Store) listComments(ctx context.Context, taskID string) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, task_id, author, body, created_at FROM task_comments WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Comment, 0)
	for rows.Next() {
		value, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) listAttachments(ctx context.Context, taskID string) ([]model.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, task_id, kind, name, media_type, size, sha256, path, url, created_at FROM task_attachments WHERE task_id = ? ORDER BY created_at, id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Attachment, 0)
	for rows.Next() {
		value, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) listRuns(ctx context.Context, taskID string) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+runColumns+" FROM task_runs WHERE task_id = ? ORDER BY claimed_at", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.Run, 0)
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) listTaskEvents(ctx context.Context, taskID string) ([]model.TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, task_id, run_id, kind, payload_json, created_at FROM task_events WHERE task_id = ? ORDER BY id DESC LIMIT 100", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.TaskEvent, 0)
	for rows.Next() {
		value, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(result)
	return result, nil
}

func (s *Store) AddComment(ctx context.Context, taskID, author, body string) (model.Comment, error) {
	author, body = strings.TrimSpace(author), strings.TrimSpace(body)
	if author == "" || body == "" {
		return model.Comment{}, errors.New("comment author and body cannot be empty")
	}
	var id int64
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		if _, err := requireTask(ctx, tx, taskID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO task_comments(task_id, author, body, created_at) VALUES (?, ?, ?, ?)", taskID, author, body, now())
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, taskID, "commented", map[string]any{"author": author}, nil)
	})
	if err != nil {
		return model.Comment{}, err
	}
	return scanComment(s.db.QueryRowContext(ctx, "SELECT id, task_id, author, body, created_at FROM task_comments WHERE id = ?", id))
}

func (s *Store) ListEvents(ctx context.Context, filter EventFilter) ([]model.TaskEvent, error) {
	clauses := []string{"t.board = ?"}
	values := []any{s.board}
	if filter.TaskID != "" {
		clauses = append(clauses, "e.task_id = ?")
		values = append(values, filter.TaskID)
	}
	if filter.SinceID != nil {
		clauses = append(clauses, "e.id > ?")
		values = append(values, *filter.SinceID)
	}
	if len(filter.Kinds) > 0 {
		placeholders := make([]string, len(filter.Kinds))
		for index, kind := range filter.Kinds {
			placeholders[index] = "?"
			values = append(values, kind)
		}
		clauses = append(clauses, "e.kind IN ("+strings.Join(placeholders, ", ")+")")
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 500
	}
	limit = max(1, min(limit, 2000))
	values = append(values, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.task_id, e.run_id, e.kind, e.payload_json, e.created_at
		FROM task_events e JOIN tasks t ON t.id = e.task_id WHERE `+strings.Join(clauses, " AND ")+" ORDER BY e.id ASC LIMIT ?", values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]model.TaskEvent, 0)
	for rows.Next() {
		value, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func EmptyStatusCounts() map[model.TaskStatus]int {
	counts := make(map[model.TaskStatus]int, len(model.TaskStatuses))
	for _, status := range model.TaskStatuses {
		counts[status] = 0
	}
	return counts
}

func (s *Store) CountTasksByStatus(ctx context.Context, board string) (map[model.TaskStatus]int, error) {
	if board == "" {
		board = s.board
	}
	counts := EmptyStatusCounts()
	rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM tasks WHERE board = ? GROUP BY status", board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status model.TaskStatus
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func (s *Store) Stats(ctx context.Context, board string) (Stats, error) {
	if board == "" {
		board = s.board
	}
	byStatus, err := s.CountTasksByStatus(ctx, board)
	if err != nil {
		return Stats{}, err
	}
	group := func(column string) (map[string]int, error) {
		rows, err := s.db.QueryContext(ctx, "SELECT COALESCE("+column+", '(unassigned)'), COUNT(*) FROM tasks WHERE board = ? GROUP BY "+column, board)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := map[string]int{}
		for rows.Next() {
			var key string
			var count int
			if err := rows.Scan(&key, &count); err != nil {
				return nil, err
			}
			result[key] = count
		}
		return result, rows.Err()
	}
	byAssignee, err := group("assignee")
	if err != nil {
		return Stats{}, err
	}
	runtimeRaw, err := group("runtime")
	if err != nil {
		return Stats{}, err
	}
	byTenant, err := group("tenant")
	if err != nil {
		return Stats{}, err
	}
	byRuntime := map[model.Runtime]int{}
	for _, runtime := range model.Runtimes {
		byRuntime[runtime] = runtimeRaw[string(runtime)]
	}
	total := 0
	for _, count := range byStatus {
		total += count
	}
	return Stats{Board: board, Total: total, ByStatus: byStatus, ByAssignee: byAssignee, ByRuntime: byRuntime, ByTenant: byTenant}, nil
}

func uniqueSorted(values []string) []string {
	result := slices.Compact(append([]string(nil), values...))
	sort.Strings(result)
	return slices.Compact(result)
}
