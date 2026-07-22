package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

const blockRecurrenceLimit = 2

type ClaimOptions struct {
	TaskID                   string
	Board                    string
	Runtime                  model.Runtime
	WorkerID                 string
	ExcludeManual            bool
	ClaimTTLSeconds          int
	MaxInProgress            int
	MaxInProgressPerAssignee int
}

type RunScope struct {
	RunID      string
	ClaimToken string
}

type CompletionInput struct {
	Summary   string
	Result    string
	Metadata  map[string]any
	Artifacts []string
}

type BlockInput struct {
	Reason string
	Kind   model.BlockKind
}

type FailRunOptions struct {
	Outcome         model.RunStatus
	CountFailure    *bool
	CooldownSeconds int
	FailureLimit    int
}

type GoalJudgment struct {
	Turn       int
	Complete   bool
	Reason     string
	NextPrompt string
}

type ActiveRun struct {
	Task model.Task `json:"task"`
	Run  model.Run  `json:"run"`
}

type RunInspection = ActiveRun

type RunLog struct {
	RunID     string `json:"runId"`
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func futureISO(seconds int) string {
	return time.Now().Add(time.Duration(seconds) * time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
}

func claimToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func getRun(ctx context.Context, q querier, runID string) (model.Run, error) {
	run, err := scanRun(q.QueryRowContext(ctx, "SELECT "+runColumns+" FROM task_runs WHERE id = ?", runID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("run not found: %s", runID)
	}
	return run, err
}

func requireActiveRun(ctx context.Context, q querier, scope RunScope) (model.Task, model.Run, error) {
	run, err := getRun(ctx, q, scope.RunID)
	if err != nil {
		return model.Task{}, model.Run{}, err
	}
	var token string
	if err := q.QueryRowContext(ctx, "SELECT claim_token FROM task_runs WHERE id = ?", scope.RunID).Scan(&token); err != nil {
		return model.Task{}, model.Run{}, err
	}
	if token != scope.ClaimToken {
		return model.Task{}, model.Run{}, errors.New("invalid claim token")
	}
	if run.Status != model.RunStatusRunning {
		return model.Task{}, model.Run{}, fmt.Errorf("run is already terminal: %s", run.Status)
	}
	task, err := requireTask(ctx, q, run.TaskID)
	if err != nil {
		return model.Task{}, model.Run{}, err
	}
	if task.CurrentRunID == nil || *task.CurrentRunID != run.ID || task.Status != model.TaskStatusRunning {
		return model.Task{}, model.Run{}, errors.New("run no longer owns this task")
	}
	return task, run, nil
}

var guardedRunError = regexp.MustCompile(`(?i)(?:429|rate.?limit|quota|unauthorized|authentication|invalid api key)`)

func respawnGuardReason(ctx context.Context, q querier, taskID string) (string, error) {
	oneHourAgo := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	var status string
	var runError sql.NullString
	err := q.QueryRowContext(ctx, "SELECT status, error FROM task_runs WHERE task_id = ? AND ended_at >= ? ORDER BY ended_at DESC LIMIT 1", taskID, oneHourAgo).Scan(&status, &runError)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if status == string(model.RunStatusCompleted) {
		return "recent_success", nil
	}
	if runError.Valid && guardedRunError.MatchString(runError.String) {
		return "blocker_auth", nil
	}
	var found int
	err = q.QueryRowContext(ctx, "SELECT 1 FROM task_comments WHERE task_id = ? AND body LIKE '%github.com/%/pull/%' ORDER BY id DESC LIMIT 1", taskID).Scan(&found)
	if err == nil {
		return "active_pr", nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	return "", nil
}

func appendRespawnGuard(ctx context.Context, q querier, taskID, reason string) error {
	var payload sql.NullString
	err := q.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE task_id = ? AND kind = 'respawn_guarded'
		AND created_at >= ? ORDER BY id DESC LIMIT 1`, taskID, time.Now().Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&payload)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if payload.Valid {
		var value map[string]any
		if json.Unmarshal([]byte(payload.String), &value) == nil && value["reason"] == reason {
			return nil
		}
	}
	return appendEvent(ctx, q, taskID, "respawn_guarded", map[string]any{"reason": reason}, nil)
}

func (s *Store) ClaimTask(ctx context.Context, input ClaimOptions) (*model.ClaimedTask, error) {
	var taskID, runID, token string
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		board := input.Board
		if board == "" {
			board = s.board
		}
		if input.MaxInProgress > 0 {
			var running int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE board = ? AND status = 'running'", board).Scan(&running); err != nil {
				return err
			}
			if running >= max(1, input.MaxInProgress) {
				return nil
			}
		}
		clauses := []string{"board = ?", "status = 'ready'", "current_run_id IS NULL", "(scheduled_at IS NULL OR scheduled_at <= ?)"}
		values := []any{board, now()}
		if input.TaskID != "" {
			clauses = append(clauses, "id = ?")
			values = append(values, input.TaskID)
		}
		if input.Runtime != "" {
			clauses = append(clauses, "runtime = ?")
			values = append(values, input.Runtime)
		}
		if input.ExcludeManual {
			clauses = append(clauses, "runtime <> 'manual'")
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE "+strings.Join(clauses, " AND ")+" ORDER BY priority DESC, created_at ASC LIMIT 50", values...)
		if err != nil {
			return err
		}
		candidates := []model.Task{}
		for rows.Next() {
			candidate, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		var selected *model.Task
		for index := range candidates {
			candidate := &candidates[index]
			open, err := hasOpenParents(ctx, tx, candidate.ID)
			if err != nil {
				return err
			}
			if open {
				if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?", now(), candidate.ID); err != nil {
					return err
				}
				continue
			}
			if input.MaxInProgressPerAssignee > 0 && candidate.Assignee != nil {
				var running int
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE board = ? AND status = 'running' AND assignee = ?", board, *candidate.Assignee).Scan(&running); err != nil {
					return err
				}
				if running >= max(1, input.MaxInProgressPerAssignee) {
					continue
				}
			}
			guard, err := respawnGuardReason(ctx, tx, candidate.ID)
			if err != nil {
				return err
			}
			if guard != "" {
				if err := appendRespawnGuard(ctx, tx, candidate.ID, guard); err != nil {
					return err
				}
				continue
			}
			selected = candidate
			break
		}
		if selected == nil {
			return nil
		}
		runID, taskID = newID("r"), selected.ID
		token, err = claimToken()
		if err != nil {
			return err
		}
		timestamp := now()
		ttl := input.ClaimTTLSeconds
		if ttl <= 0 {
			ttl = 15 * 60
		}
		expires := futureISO(max(1, ttl))
		result, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'running', current_run_id = ?, updated_at = ? WHERE id = ? AND status = 'ready' AND current_run_id IS NULL", runID, timestamp, selected.ID)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			taskID = ""
			return nil
		}
		workerID := input.WorkerID
		if workerID == "" {
			workerID = string(selected.Runtime) + "-worker"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_runs(id, task_id, worker_id, runtime, status, claim_token,
			claimed_at, claim_expires_at, heartbeat_at) VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?)`,
			runID, selected.ID, workerID, selected.Runtime, token, timestamp, expires, timestamp); err != nil {
			return err
		}
		return appendEvent(ctx, tx, selected.ID, "claimed", map[string]any{"workerId": workerID, "expires": expires}, &runID)
	})
	if err != nil {
		return nil, err
	}
	if taskID == "" {
		return nil, nil
	}
	detail, err := s.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	run, err := getRun(ctx, s.db, runID)
	if err != nil {
		return nil, err
	}
	return &model.ClaimedTask{Task: detail, Run: run, ClaimToken: token}, nil
}

func (s *Store) Heartbeat(ctx context.Context, scope RunScope, note string) (model.Run, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		expiresAt, heartbeatAt := time.Now(), time.Now()
		oldExpiry, expiryErr := time.Parse(time.RFC3339Nano, run.ClaimExpiresAt)
		oldHeartbeat, heartbeatErr := time.Parse(time.RFC3339Nano, run.HeartbeatAt)
		ttl := 15 * time.Minute
		if expiryErr == nil && heartbeatErr == nil && oldExpiry.Sub(oldHeartbeat) > time.Second {
			ttl = oldExpiry.Sub(oldHeartbeat)
		}
		timestamp := heartbeatAt.UTC().Format("2006-01-02T15:04:05.000Z")
		expires := expiresAt.Add(ttl).UTC().Format("2006-01-02T15:04:05.000Z")
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET heartbeat_at = ?, claim_expires_at = ? WHERE id = ?", timestamp, expires, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET updated_at = ? WHERE id = ?", timestamp, task.ID); err != nil {
			return err
		}
		var payload any
		if strings.TrimSpace(note) != "" {
			payload = map[string]any{"note": note}
		}
		return appendEvent(ctx, tx, task.ID, "heartbeat", payload, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}

func (s *Store) HeartbeatTask(ctx context.Context, taskID, note string) (model.Run, error) {
	task, err := requireTask(ctx, s.db, taskID)
	if err != nil {
		return model.Run{}, err
	}
	if task.CurrentRunID == nil || task.Status != model.TaskStatusRunning {
		return model.Run{}, fmt.Errorf("task has no active run: %s", taskID)
	}
	var token string
	if err := s.db.QueryRowContext(ctx, "SELECT claim_token FROM task_runs WHERE id = ?", *task.CurrentRunID).Scan(&token); err != nil {
		return model.Run{}, err
	}
	return s.Heartbeat(ctx, RunScope{RunID: *task.CurrentRunID, ClaimToken: token}, note)
}

func extendRunLease(ctx context.Context, tx *sql.Tx, task model.Task, run model.Run, clearPID bool) error {
	expiry, expiryErr := time.Parse(time.RFC3339Nano, run.ClaimExpiresAt)
	heartbeat, heartbeatErr := time.Parse(time.RFC3339Nano, run.HeartbeatAt)
	ttl := 15 * time.Minute
	if expiryErr == nil && heartbeatErr == nil {
		ttl = max(time.Second, expiry.Sub(heartbeat))
	}
	timestamp := now()
	expires := time.Now().Add(ttl).UTC().Format("2006-01-02T15:04:05.000Z")
	statement := "UPDATE task_runs SET heartbeat_at = ?, claim_expires_at = ? WHERE id = ?"
	if clearPID {
		statement = "UPDATE task_runs SET pid = NULL, heartbeat_at = ?, claim_expires_at = ? WHERE id = ?"
	}
	if _, err := tx.ExecContext(ctx, statement, timestamp, expires, run.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE tasks SET updated_at = ? WHERE id = ?", timestamp, task.ID)
	return err
}

func boundedGoalText(value string) string {
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}

func (s *Store) RecordGoalJudgment(ctx context.Context, scope RunScope, input GoalJudgment) (model.Run, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := extendRunLease(ctx, tx, task, run, false); err != nil {
			return err
		}
		var nextPrompt any
		if value := boundedGoalText(input.NextPrompt); value != "" {
			nextPrompt = value
		}
		return appendEvent(ctx, tx, task.ID, "goal_judged", map[string]any{
			"turn": input.Turn, "complete": input.Complete,
			"reason": boundedGoalText(input.Reason), "nextPrompt": nextPrompt,
		}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}

func (s *Store) PauseGoalRun(ctx context.Context, scope RunScope, turn int) (model.Run, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := extendRunLease(ctx, tx, task, run, true); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "goal_turn_finished", map[string]any{"turn": turn}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}

func (s *Store) RecordSpawn(ctx context.Context, scope RunScope, pid int, logPath string) (model.Run, error) {
	resolved, err := filepath.Abs(logPath)
	if err != nil {
		return model.Run{}, err
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET pid = ?, log_path = ? WHERE id = ?", pid, resolved, run.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "spawned", map[string]any{"pid": pid, "logPath": resolved}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}

func (s *Store) BindRunWorkspace(ctx context.Context, scope RunScope, workspace string, kind model.WorkspaceKind) (model.TaskDetail, error) {
	resolved, err := filepath.Abs(workspace)
	if err != nil {
		return model.TaskDetail{}, err
	}
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		taskID = task.ID
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET workspace = ?, workspace_kind = ?, updated_at = ? WHERE id = ?", resolved, kind, now(), task.ID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "workspace_prepared", map[string]any{"path": resolved, "kind": kind}, &run.ID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) CompleteRun(ctx context.Context, scope RunScope, completion CompletionInput) (model.TaskDetail, error) {
	summary := strings.TrimSpace(completion.Summary)
	result := strings.TrimSpace(completion.Result)
	if summary == "" {
		summary = result
	}
	if summary == "" {
		return model.TaskDetail{}, errors.New("completion requires a summary or result")
	}
	preflight, _, err := requireActiveRun(ctx, s.db, scope)
	if err != nil {
		return model.TaskDetail{}, err
	}
	captured, err := s.captureArtifacts(ctx, preflight, completion.Artifacts)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if len(captured) > 0 {
		if completion.Metadata == nil {
			completion.Metadata = map[string]any{}
		}
		artifacts := make([]map[string]any, 0, len(captured))
		for _, attachment := range captured {
			artifacts = append(artifacts, map[string]any{"id": attachment.ID, "name": attachment.Name, "path": attachment.Path})
		}
		completion.Metadata["artifacts"] = artifacts
	}
	var resultValue any
	if result != "" {
		resultValue = result
	}
	var metadata any
	if completion.Metadata != nil {
		encoded, err := json.Marshal(completion.Metadata)
		if err != nil {
			return model.TaskDetail{}, err
		}
		metadata = string(encoded)
	}
	taskID := ""
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		open, err := hasOpenParents(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		if open {
			return errors.New("task prerequisites changed while the run was active; terminate or requeue the run before completing")
		}
		taskID = task.ID
		timestamp := now()
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET status = 'completed', ended_at = ?, heartbeat_at = ?, summary = ?, metadata_json = ? WHERE id = ?", timestamp, timestamp, summary, metadata, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'done', current_run_id = NULL, result = ?, failure_count = 0,
			block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ? WHERE id = ?`, resultValue, timestamp, task.ID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, task.ID, "completed", map[string]any{"summary": truncate(summary, 400), "resultLength": len(result)}, &run.ID); err != nil {
			return err
		}
		return satisfyOutgoingDependencies(ctx, tx, task.ID, timestamp)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func syntheticRun(ctx context.Context, q querier, task model.Task, status model.RunStatus, summary string, metadata map[string]any, runError string) (string, error) {
	runID, timestamp := newID("r"), now()
	var metadataValue any
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return "", err
		}
		metadataValue = string(encoded)
	}
	var summaryValue, errorValue any
	if summary != "" {
		summaryValue = summary
	}
	if runError != "" {
		errorValue = runError
	}
	_, err := q.ExecContext(ctx, `INSERT INTO task_runs(id, task_id, worker_id, runtime, status, claim_token, claimed_at,
		claim_expires_at, heartbeat_at, ended_at, summary, metadata_json, error) VALUES (?, ?, 'human', ?, ?, 'synthetic', ?, ?, ?, ?, ?, ?, ?)`,
		runID, task.ID, task.Runtime, status, timestamp, timestamp, timestamp, timestamp, summaryValue, metadataValue, errorValue)
	return runID, err
}

func (s *Store) CompleteTask(ctx context.Context, taskID string, completion CompletionInput) (model.TaskDetail, error) {
	preflight, err := requireTask(ctx, s.db, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if preflight.CurrentRunID != nil {
		return model.TaskDetail{}, errors.New("cannot complete a running task administratively; let the worker complete or terminate the active run first")
	}
	if preflight.Status != model.TaskStatusDone {
		captured, err := s.captureArtifacts(ctx, preflight, completion.Artifacts)
		if err != nil {
			return model.TaskDetail{}, err
		}
		if len(captured) > 0 {
			if completion.Metadata == nil {
				completion.Metadata = map[string]any{}
			}
			artifacts := make([]map[string]any, 0, len(captured))
			for _, attachment := range captured {
				artifacts = append(artifacts, map[string]any{"id": attachment.ID, "name": attachment.Name, "path": attachment.Path})
			}
			completion.Metadata["artifacts"] = artifacts
		}
	}
	summary, result := strings.TrimSpace(completion.Summary), strings.TrimSpace(completion.Result)
	if summary == "" {
		summary = result
	}
	var resultValue any
	if result != "" {
		resultValue = result
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Status == model.TaskStatusArchived {
			return errors.New("cannot complete an archived task")
		}
		if task.Status == model.TaskStatusDone {
			return nil
		}
		var runID *string
		if summary != "" || completion.Metadata != nil {
			id, err := syntheticRun(ctx, tx, task, model.RunStatusCompleted, summary, completion.Metadata, "")
			if err != nil {
				return err
			}
			runID = &id
		}
		timestamp := now()
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'done', current_run_id = NULL, result = ?, failure_count = 0,
		block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ? WHERE id = ?`, resultValue, timestamp, taskID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "completed", map[string]any{"summary": truncate(summary, 400), "resultLength": len(result)}, runID); err != nil {
			return err
		}
		return satisfyOutgoingDependencies(ctx, tx, taskID, timestamp)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func blockTaskRecord(ctx context.Context, q querier, task model.Task, input BlockInput, runID *string) error {
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		return errors.New("block reason cannot be empty")
	}
	if input.Kind != "" && input.Kind != model.BlockKindDependency && input.Kind != model.BlockKindNeedsInput && input.Kind != model.BlockKindCapability && input.Kind != model.BlockKindTransient {
		return fmt.Errorf("invalid block kind: %s", input.Kind)
	}
	timestamp := now()
	var kind any
	if input.Kind != "" {
		kind = input.Kind
	}
	if input.Kind == model.BlockKindDependency {
		if _, err := q.ExecContext(ctx, "UPDATE tasks SET status = 'todo', current_run_id = NULL, block_kind = ?, block_reason = ?, updated_at = ? WHERE id = ?", kind, reason, timestamp, task.ID); err != nil {
			return err
		}
		return appendEvent(ctx, q, task.ID, "dependency_wait", map[string]any{"reason": reason, "kind": input.Kind}, runID)
	}
	same := task.BlockKind != nil && *task.BlockKind == input.Kind && task.BlockReason != nil && *task.BlockReason == reason
	recurrences := 1
	if same {
		recurrences = task.BlockRecurrences + 1
	}
	loop := recurrences >= blockRecurrenceLimit && task.BlockRecurrences > 0
	status, event := model.TaskStatusBlocked, "blocked"
	if loop {
		status, event = model.TaskStatusTriage, "block_loop_detected"
	}
	if _, err := q.ExecContext(ctx, `UPDATE tasks SET status = ?, current_run_id = NULL, block_kind = ?, block_reason = ?,
		block_recurrences = ?, updated_at = ? WHERE id = ?`, status, kind, reason, recurrences, timestamp, task.ID); err != nil {
		return err
	}
	payload := map[string]any{"reason": reason, "kind": kind, "recurrences": recurrences}
	if loop {
		payload["limit"] = blockRecurrenceLimit
	}
	return appendEvent(ctx, q, task.ID, event, payload, runID)
}

func (s *Store) BlockRun(ctx context.Context, scope RunScope, input BlockInput) (model.TaskDetail, error) {
	taskID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		taskID = task.ID
		timestamp := now()
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET status = 'blocked', ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?", timestamp, timestamp, strings.TrimSpace(input.Reason), run.ID); err != nil {
			return err
		}
		return blockTaskRecord(ctx, tx, task, input, &run.ID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) BlockTask(ctx context.Context, taskID string, input BlockInput) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Status == model.TaskStatusDone || task.Status == model.TaskStatusArchived {
			return fmt.Errorf("cannot block a %s task", task.Status)
		}
		if task.Status == model.TaskStatusBlocked {
			return errors.New("task is already blocked; unblock it before blocking again")
		}
		if task.CurrentRunID != nil {
			return errors.New("cannot block a running task administratively; terminate the active run first")
		}
		runID, err := syntheticRun(ctx, tx, task, model.RunStatusBlocked, "", nil, strings.TrimSpace(input.Reason))
		if err != nil {
			return err
		}
		return blockTaskRecord(ctx, tx, task, input, &runID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func finishUnsuccessful(ctx context.Context, q querier, task model.Task, run model.Run, runError string, options FailRunOptions) error {
	outcome := options.Outcome
	if outcome == "" {
		outcome = model.RunStatusFailed
	}
	countFailure := true
	if options.CountFailure != nil {
		countFailure = *options.CountFailure
	}
	failures := task.FailureCount
	if countFailure {
		failures++
	}
	limit := options.FailureLimit
	if limit <= 0 {
		limit = task.MaxRetries
	}
	limit = max(1, limit)
	exhausted := countFailure && failures >= limit
	var scheduledAt any
	next := model.TaskStatusReady
	if exhausted {
		next = model.TaskStatusBlocked
	} else if options.CooldownSeconds > 0 {
		next = model.TaskStatusScheduled
		scheduledAt = futureISO(options.CooldownSeconds)
	} else {
		open, err := hasOpenParents(ctx, q, task.ID)
		if err != nil {
			return err
		}
		if open || task.Assignee == nil || task.Runtime == model.RuntimeManual {
			next = model.TaskStatusTodo
		}
	}
	timestamp := now()
	if _, err := q.ExecContext(ctx, "UPDATE task_runs SET status = ?, ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?", outcome, timestamp, timestamp, runError, run.ID); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `UPDATE tasks SET status = ?, current_run_id = NULL, failure_count = ?, scheduled_at = ?,
		block_reason = CASE WHEN ? THEN ? ELSE block_reason END, updated_at = ? WHERE id = ?`, next, failures, scheduledAt, exhausted, runError, timestamp, task.ID); err != nil {
		return err
	}
	payload := map[string]any{"error": runError, "failures": failures, "effectiveLimit": limit, "outcome": outcome, "countFailure": countFailure, "scheduledAt": scheduledAt}
	event := string(outcome)
	if outcome == model.RunStatusFailed {
		if exhausted {
			event = "gave_up"
		} else {
			event = "requeued"
		}
	}
	if err := appendEvent(ctx, q, task.ID, event, payload, &run.ID); err != nil {
		return err
	}
	if exhausted && outcome != model.RunStatusFailed {
		return appendEvent(ctx, q, task.ID, "gave_up", payload, &run.ID)
	}
	return nil
}

func (s *Store) FailRun(ctx context.Context, scope RunScope, runError string, options FailRunOptions) (model.TaskDetail, error) {
	taskID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		taskID = task.ID
		return finishUnsuccessful(ctx, tx, task, run, runError, options)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) RecoverAbandonedRun(ctx context.Context, runID string, outcome model.RunStatus, runError string, countFailure bool) (model.TaskDetail, error) {
	taskID := ""
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		task, err := requireTask(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		taskID = task.ID
		if run.Status != model.RunStatusRunning || task.CurrentRunID == nil || *task.CurrentRunID != run.ID || task.Status != model.TaskStatusRunning {
			return nil
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, FailRunOptions{Outcome: outcome, CountFailure: &countFailure})
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) DeferReclaim(ctx context.Context, runID string, seconds int, reason string) (model.Run, error) {
	if seconds <= 0 {
		seconds = 120
	}
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if run.Status != model.RunStatusRunning {
			return fmt.Errorf("active run not found: %s", runID)
		}
		expires := futureISO(max(1, seconds))
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET claim_expires_at = ? WHERE id = ?", expires, runID); err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.TaskID, "reclaim_deferred", map[string]any{"pid": run.PID, "expires": expires, "reason": normalizedPointer(&reason)}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, runID)
}

func (s *Store) UnblockTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Status != model.TaskStatusBlocked {
			return fmt.Errorf("task is not blocked: %s", task.Status)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'todo', failure_count = 0, updated_at = ? WHERE id = ?", now(), taskID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "unblocked", nil, nil); err != nil {
			return err
		}
		return recomputeReady(ctx, tx, taskID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

func (s *Store) ListActiveRuns(ctx context.Context, board string) ([]ActiveRun, error) {
	if board == "" {
		board = s.board
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.task_id FROM task_runs r JOIN tasks t ON t.id = r.task_id
		WHERE t.board = ? AND t.status = 'running' AND t.current_run_id = r.id AND r.status = 'running' ORDER BY r.claimed_at`, board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct{ runID, taskID string }
	pairs := []pair{}
	for rows.Next() {
		var value pair
		if err := rows.Scan(&value.runID, &value.taskID); err != nil {
			return nil, err
		}
		pairs = append(pairs, value)
	}
	result := make([]ActiveRun, 0, len(pairs))
	for _, value := range pairs {
		task, err := requireTask(ctx, s.db, value.taskID)
		if err != nil {
			return nil, err
		}
		run, err := getRun(ctx, s.db, value.runID)
		if err != nil {
			return nil, err
		}
		result = append(result, ActiveRun{Task: task, Run: run})
	}
	return result, rows.Err()
}

func (s *Store) GetRun(ctx context.Context, runID string) (RunInspection, error) {
	run, err := getRun(ctx, s.db, runID)
	if err != nil {
		return RunInspection{}, err
	}
	task, err := requireTask(ctx, s.db, run.TaskID)
	if err != nil {
		return RunInspection{}, err
	}
	if task.Board != s.board {
		return RunInspection{}, fmt.Errorf("run not found: %s", runID)
	}
	return RunInspection{Task: task, Run: run}, nil
}

func (s *Store) ReadRunLog(ctx context.Context, taskID string, tailBytes int, runID string) (RunLog, error) {
	if _, err := requireTask(ctx, s.db, taskID); err != nil {
		return RunLog{}, err
	}
	var id string
	var path sql.NullString
	var err error
	if runID != "" {
		err = s.db.QueryRowContext(ctx, "SELECT id, log_path FROM task_runs WHERE id = ? AND task_id = ?", runID, taskID).Scan(&id, &path)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT id, log_path FROM task_runs WHERE task_id = ? AND log_path IS NOT NULL ORDER BY claimed_at DESC LIMIT 1", taskID).Scan(&id, &path)
	}
	if err != nil || !path.Valid {
		return RunLog{}, fmt.Errorf("no worker log found for task: %s", taskID)
	}
	content, err := os.ReadFile(path.String)
	if err != nil {
		return RunLog{}, fmt.Errorf("worker log file is missing: %s", path.String)
	}
	if tailBytes <= 0 {
		tailBytes = 64 * 1024
	}
	tailBytes = max(1, min(tailBytes, 1024*1024))
	truncated := len(content) > tailBytes
	if truncated {
		content = content[len(content)-tailBytes:]
	}
	return RunLog{RunID: id, Path: path.String, Text: string(content), Truncated: truncated}, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	omitted := len(value) - limit
	return value[:max(0, limit-24)] + fmt.Sprintf("\n… (%d chars omitted)", omitted)
}
