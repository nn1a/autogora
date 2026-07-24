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

const claimCandidatePageSize = 50

var ErrRunTerminationPending = errors.New("run termination is pending")

type ClaimOptions struct {
	TaskID                   string
	Board                    string
	ExpectedUpdatedAt        *string
	Runtime                  model.Runtime
	WorkerID                 string
	ExcludeManual            bool
	ClaimTTLSeconds          int
	MaxInProgress            int
	MaxInProgressPerAssignee int
	MaxInProgressByAssignee  map[string]int
	ExcludedAssignees        []string
}

type RunScope struct {
	RunID      string
	ClaimToken string
}

type CompletionInput struct {
	Summary           string
	Result            string
	Metadata          map[string]any
	Artifacts         []string
	ExpectedUpdatedAt *string
}

type BlockInput struct {
	Reason            string
	Kind              model.BlockKind
	ExpectedUpdatedAt *string
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

// DeferredReclaim is the durable termination intent for a managed run. The
// dispatcher keeps the run active until the worker actually exits, then uses
// this outcome instead of relying on whichever task event happened last.
type DeferredReclaim struct {
	RunID                              string          `json:"runId"`
	ExpiresAt                          string          `json:"expiresAt"`
	Reason                             string          `json:"reason"`
	Outcome                            model.RunStatus `json:"outcome"`
	CountFailure                       bool            `json:"countFailure"`
	RequiresOperator                   bool            `json:"requiresOperator"`
	DiagnosticCode                     *string         `json:"diagnosticCode,omitempty"`
	FenceToken                         string          `json:"-"`
	FenceGeneration                    int             `json:"fenceGeneration"`
	HostAcknowledgedAt                 *string         `json:"hostAcknowledgedAt,omitempty"`
	RecoveryOwnerToken                 *string         `json:"-"`
	RecoveryOwnerExpiresAt             *string         `json:"-"`
	OperatorQuiescedAt                 *string         `json:"operatorQuiescedAt,omitempty"`
	OperatorQuiescedBy                 *string         `json:"operatorQuiescedBy,omitempty"`
	OperatorQuiescenceReason           *string         `json:"operatorQuiescenceReason,omitempty"`
	OperatorQuiescedGeneration         *int            `json:"operatorQuiescedGeneration,omitempty"`
	OperatorConfirmedWorkerStopped     bool            `json:"operatorConfirmedWorkerStopped"`
	OperatorConfirmedHostWritesStopped bool            `json:"operatorConfirmedHostWritesStopped"`
	OperatorObservedHeartbeatAt        *string         `json:"-"`
	OperatorObservedClaimExpiresAt     *string         `json:"-"`
	OperatorObservedPID                *int            `json:"-"`
	OperatorObservedProcessIdentity    *string         `json:"-"`
}

type ActiveRun struct {
	Task        model.Task            `json:"task"`
	Run         model.Run             `json:"run"`
	AgentConfig *model.RunAgentConfig `json:"agentConfig,omitempty"`
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

func requireActiveRunState(
	ctx context.Context,
	q querier,
	scope RunScope,
	allowRecoveryFence bool,
) (model.Task, model.Run, error) {
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
	if !allowRecoveryFence {
		if err := ensureNoRunRecoveryFence(ctx, q, run.ID); err != nil {
			return model.Task{}, model.Run{}, err
		}
	}
	return task, run, nil
}

func requireActiveRun(
	ctx context.Context,
	q querier,
	scope RunScope,
) (model.Task, model.Run, error) {
	return requireActiveRunState(ctx, q, scope, false)
}

func requireActiveRunForRecovery(
	ctx context.Context,
	q querier,
	scope RunScope,
) (model.Task, model.Run, error) {
	return requireActiveRunState(ctx, q, scope, true)
}

var guardedRunError = regexp.MustCompile(`(?i)(?:429|rate.?limit|quota|unauthorized|authentication|invalid api key)`)

func respawnGuardReason(ctx context.Context, q querier, taskID string) (string, error) {
	oneHourAgo := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	var runID, endedAt string
	var status string
	var runError sql.NullString
	err := q.QueryRowContext(ctx, "SELECT id, status, error, ended_at FROM task_runs WHERE task_id = ? AND ended_at >= ? ORDER BY ended_at DESC, id DESC LIMIT 1", taskID, oneHourAgo).Scan(&runID, &status, &runError, &endedAt)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if status == string(model.RunStatusCompleted) {
		// A recent success prevents accidental duplicate spawning, but an
		// explicit lifecycle edit after that success is a deliberate rerun.
		// Comments and attachments are intentionally not sufficient.
		var completedEventID sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT MAX(id) FROM task_events
			WHERE task_id = ? AND run_id = ? AND kind = 'completed'`, taskID, runID).Scan(&completedEventID); err != nil {
			return "", err
		}
		clauses := `task_id = ? AND kind IN ('updated', 'reopened_for_dependency', 'specified', 'unblocked', 'promote_requested', 'scheduled', 'schedule_due')`
		values := []any{taskID}
		if completedEventID.Valid {
			clauses += " AND id > ?"
			values = append(values, completedEventID.Int64)
		} else {
			clauses += " AND created_at >= ?"
			values = append(values, endedAt)
		}
		var explicitlyRequeued int
		err := q.QueryRowContext(ctx, "SELECT 1 FROM task_events WHERE "+clauses+" ORDER BY id DESC LIMIT 1", values...).Scan(&explicitlyRequeued)
		if err == nil {
			return "", nil
		}
		if err != sql.ErrNoRows {
			return "", err
		}
		return "recent_success", nil
	}
	if runError.Valid && guardedRunError.MatchString(runError.String) {
		return "blocker_auth", nil
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
		if err := ensureBoardNotRemoving(ctx, tx, board, boardRemovalScopeLocal); err != nil {
			return err
		}
		if input.ExpectedUpdatedAt != nil {
			if strings.TrimSpace(input.TaskID) == "" {
				return errors.New("expectedUpdatedAt requires a targeted task claim")
			}
			target, err := requireTask(ctx, tx, input.TaskID)
			if err != nil {
				return err
			}
			if strings.TrimSpace(*input.ExpectedUpdatedAt) != target.UpdatedAt {
				return fmt.Errorf("task update conflict: %s changed at %s; refresh before dispatching", target.ID, target.UpdatedAt)
			}
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
		excluded := make([]string, 0, len(input.ExcludedAssignees))
		seenExcluded := map[string]bool{}
		for _, assignee := range input.ExcludedAssignees {
			assignee = strings.TrimSpace(assignee)
			if assignee != "" && !seenExcluded[assignee] {
				seenExcluded[assignee] = true
				excluded = append(excluded, assignee)
			}
		}
		if len(excluded) > 0 {
			placeholders := make([]string, len(excluded))
			for index, assignee := range excluded {
				placeholders[index] = "?"
				values = append(values, assignee)
			}
			clauses = append(clauses, "(assignee IS NULL OR assignee NOT IN ("+strings.Join(placeholders, ",")+"))")
		}
		var selected *model.Task
		var cursor *model.Task
		for selected == nil {
			pageClauses := append([]string(nil), clauses...)
			pageValues := append([]any(nil), values...)
			if cursor != nil {
				pageClauses = append(pageClauses, `(priority < ? OR (priority = ? AND (created_at > ? OR (created_at = ? AND id > ?))))`)
				pageValues = append(pageValues, cursor.Priority, cursor.Priority, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
			}
			pageValues = append(pageValues, claimCandidatePageSize)
			rows, err := tx.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE "+strings.Join(pageClauses, " AND ")+" ORDER BY priority DESC, created_at ASC, id ASC LIMIT ?", pageValues...)
			if err != nil {
				return err
			}
			candidates := make([]model.Task, 0, claimCandidatePageSize)
			for rows.Next() {
				candidate, err := scanTask(rows)
				if err != nil {
					rows.Close()
					return err
				}
				candidates = append(candidates, candidate)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if len(candidates) == 0 {
				break
			}
			last := candidates[len(candidates)-1]
			cursor = &last
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
				assigneeLimit := input.MaxInProgressPerAssignee
				if candidate.Assignee != nil {
					if configured := input.MaxInProgressByAssignee[*candidate.Assignee]; configured > 0 && (assigneeLimit <= 0 || configured < assigneeLimit) {
						assigneeLimit = configured
					}
				}
				if assigneeLimit > 0 && candidate.Assignee != nil {
					var running int
					if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE board = ? AND status = 'running' AND assignee = ?", board, *candidate.Assignee).Scan(&running); err != nil {
						return err
					}
					if running >= max(1, assigneeLimit) {
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
			if len(candidates) < claimCandidatePageSize {
				break
			}
		}
		if selected == nil {
			return nil
		}
		runID, taskID = newID("r"), selected.ID
		generatedToken, err := claimToken()
		if err != nil {
			return err
		}
		token = generatedToken
		timestamp := now()
		ttl := input.ClaimTTLSeconds
		if ttl <= 0 {
			ttl = 15 * 60
		}
		expires := futureISO(max(1, ttl))
		result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'running', current_run_id = ?, updated_at = ?
			WHERE id = ? AND status = 'ready' AND current_run_id IS NULL AND updated_at = ?`,
			runID, timestamp, selected.ID, selected.UpdatedAt)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			taskID = ""
			if input.ExpectedUpdatedAt != nil {
				return fmt.Errorf("task update conflict: %s changed before dispatch claim; refresh before dispatching", selected.ID)
			}
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
		expiresAt := time.Now()
		oldExpiry, expiryErr := time.Parse(time.RFC3339Nano, run.ClaimExpiresAt)
		oldHeartbeat, heartbeatErr := time.Parse(time.RFC3339Nano, run.HeartbeatAt)
		ttl := 15 * time.Minute
		if expiryErr == nil && heartbeatErr == nil && oldExpiry.Sub(oldHeartbeat) > time.Second {
			ttl = oldExpiry.Sub(oldHeartbeat)
		}
		timestamp := now()
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
	if err := ensureNoRunRecoveryFence(ctx, tx, run.ID); err != nil {
		return err
	}
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

func ensureNoRunRecoveryFence(
	ctx context.Context,
	q querier,
	runID string,
) error {
	var reclaimCount int
	if err := q.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM run_reclaim_requests WHERE run_id = ?",
		runID,
	).Scan(&reclaimCount); err != nil {
		return err
	}
	if reclaimCount > 0 {
		return fmt.Errorf("%w: %s", ErrRunTerminationPending, runID)
	}
	return nil
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
	return s.RecordSpawnWithIdentity(ctx, scope, pid, logPath, "")
}

// RecordSpawnWithIdentity persists the OS process-start identity alongside the
// PID. Recovery must compare both before sending a signal because PIDs can be
// reused after the original worker exits.
func (s *Store) RecordSpawnWithIdentity(ctx context.Context, scope RunScope, pid int, logPath, processIdentity string) (model.Run, error) {
	resolved, err := filepath.Abs(logPath)
	if err != nil {
		return model.Run{}, err
	}
	processIdentity = strings.TrimSpace(processIdentity)
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		task, run, err := requireActiveRun(ctx, tx, scope)
		if err != nil {
			return err
		}
		if err := ensureNoTerminalRequest(ctx, tx, run.ID); err != nil {
			return err
		}
		if err := ensureNoRunRecoveryFence(ctx, tx, run.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET pid = ?, log_path = ? WHERE id = ?", pid, resolved, run.ID); err != nil {
			return err
		}
		if processIdentity == "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM run_process_identities WHERE run_id = ?", run.ID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `INSERT INTO run_process_identities(run_id, process_identity, recorded_at)
			VALUES (?, ?, ?) ON CONFLICT(run_id) DO UPDATE SET
				process_identity = excluded.process_identity,
				recorded_at = excluded.recorded_at`, run.ID, processIdentity, now()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, task.ID, "spawned", map[string]any{
			"pid": pid, "logPath": resolved, "processIdentity": normalizedPointer(&processIdentity),
		}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, scope.RunID)
}

// GetRunProcessIdentity returns the identity recorded by the latest spawn for
// a run. The coordination row is not subject to task-event garbage collection.
func (s *Store) GetRunProcessIdentity(ctx context.Context, runID string) (*string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("run ID is required")
	}
	var processIdentity string
	err := s.db.QueryRowContext(ctx,
		"SELECT process_identity FROM run_process_identities WHERE run_id = ?", runID,
	).Scan(&processIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return normalizedPointer(&processIdentity), nil
}

func (s *Store) CompleteRun(ctx context.Context, scope RunScope, completion CompletionInput) (model.TaskDetail, error) {
	return s.requestRunCompletion(ctx, scope, completion, false)
}

// RequestRunCompletion records a two-phase completion request without
// finalizing it. Callers use this when they must durably capture workspace
// changes before the task can become Done.
func (s *Store) RequestRunCompletion(ctx context.Context, scope RunScope, completion CompletionInput) (model.TaskDetail, error) {
	return s.requestRunCompletion(ctx, scope, completion, true)
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
	if err := requireExpectedTaskVersion(preflight, completion.ExpectedUpdatedAt); err != nil {
		return model.TaskDetail{}, err
	}
	if preflight.CurrentRunID != nil {
		return model.TaskDetail{}, errors.New("cannot complete a running task administratively; let the worker complete or terminate the active run first")
	}
	if err := requireNoActiveRecoveryCheckpointForCompletion(
		ctx,
		s.db,
		preflight.ID,
	); err != nil {
		return model.TaskDetail{}, err
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
		if err := requireExpectedTaskVersion(task, completion.ExpectedUpdatedAt); err != nil {
			return err
		}
		if task.Status == model.TaskStatusArchived {
			return errors.New("cannot complete an archived task")
		}
		if task.Status == model.TaskStatusDone {
			return nil
		}
		if err := requireNoActiveRecoveryCheckpointForCompletion(
			ctx,
			tx,
			task.ID,
		); err != nil {
			return err
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
		scheduled_at = NULL, block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ? WHERE id = ?`, resultValue, timestamp, taskID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, taskID, "completed", map[string]any{"summary": truncate(summary, 400), "resultLength": len(result)}, runID); err != nil {
			return err
		}
		return satisfyOutgoingDependencies(ctx, tx, taskID, timestamp, runID)
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
		if _, err := q.ExecContext(ctx, "UPDATE tasks SET status = 'todo', current_run_id = NULL, scheduled_at = NULL, block_kind = ?, block_reason = ?, updated_at = ? WHERE id = ?", kind, reason, timestamp, task.ID); err != nil {
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
	if _, err := q.ExecContext(ctx, `UPDATE tasks SET status = ?, current_run_id = NULL, scheduled_at = NULL, block_kind = ?, block_reason = ?,
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
	return s.requestRunBlock(ctx, scope, input)
}

func (s *Store) BlockTask(ctx context.Context, taskID string, input BlockInput) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, input.ExpectedUpdatedAt); err != nil {
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
	requestResult, err := q.ExecContext(ctx, "DELETE FROM run_terminal_requests WHERE run_id = ? AND finalized_at IS NULL", run.ID)
	if err != nil {
		return err
	}
	discardedRequest, err := requestResult.RowsAffected()
	if err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, "UPDATE task_runs SET status = ?, ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?", outcome, timestamp, timestamp, runError, run.ID); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `UPDATE tasks SET status = ?, current_run_id = NULL, failure_count = ?, scheduled_at = ?,
		block_reason = CASE WHEN ? THEN ? ELSE block_reason END, updated_at = ? WHERE id = ?`, next, failures, scheduledAt, exhausted, runError, timestamp, task.ID); err != nil {
		return err
	}
	payload := map[string]any{"error": runError, "failures": failures, "effectiveLimit": limit, "outcome": outcome, "countFailure": countFailure, "scheduledAt": scheduledAt, "discardedTerminalRequest": discardedRequest > 0}
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

func (s *Store) recoverAbandonedRun(
	ctx context.Context,
	runID string,
	observation RunRecoveryObservation,
	outcome model.RunStatus,
	runError string,
	countFailure bool,
) (model.TaskDetail, error) {
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
		if err := requireSupervisorRunRecoveryFence(
			ctx,
			tx,
			run,
			&observation,
		); err != nil {
			return err
		}
		return finishUnsuccessful(ctx, tx, task, run, runError, FailRunOptions{Outcome: outcome, CountFailure: &countFailure})
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	return s.GetTask(ctx, taskID)
}

// RecoverObservedAbandonedRun terminalizes only when the run still matches the
// exact lease/process state, durable fence, host/operator quiescence, and
// unexpired recovery-owner epoch observed by the Supervisor. There is no
// tokenless administrative terminalization path.
func (s *Store) RecoverObservedAbandonedRun(
	ctx context.Context,
	observation RunRecoveryObservation,
	outcome model.RunStatus,
	runError string,
	countFailure bool,
) (model.TaskDetail, error) {
	return s.recoverAbandonedRun(
		ctx,
		observation.RunID,
		observation,
		outcome,
		runError,
		countFailure,
	)
}

func (s *Store) deferRunRecovery(
	ctx context.Context,
	runID string,
	observation *RunRecoveryObservation,
	seconds int,
	reason string,
	outcome model.RunStatus,
	countFailure bool,
	requiresOperator bool,
	diagnosticCode string,
) (model.Run, error) {
	if seconds <= 0 {
		seconds = 120
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Run termination requested"
	}
	if outcome == "" {
		outcome = model.RunStatusReclaimed
	}
	switch outcome {
	case model.RunStatusReclaimed, model.RunStatusTimedOut, model.RunStatusCrashed:
	default:
		return model.Run{}, fmt.Errorf("unsupported deferred run outcome: %s", outcome)
	}
	diagnosticCode = strings.TrimSpace(diagnosticCode)
	if requiresOperator && diagnosticCode == "" {
		diagnosticCode = "unverifiable_process_ownership"
	}
	fenceToken, err := claimToken()
	if err != nil {
		return model.Run{}, fmt.Errorf("create run recovery fence token: %w", err)
	}
	err = s.withWrite(ctx, func(tx *sql.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if run.Status != model.RunStatusRunning {
			return fmt.Errorf("active run not found: %s", runID)
		}
		if err := requireRunRecoveryObservation(ctx, tx, run, observation); err != nil {
			return err
		}
		var currentReason, currentOutcome, currentFenceToken string
		var currentCountFailure, currentRequiresOperator bool
		var currentFenceGeneration int
		currentErr := tx.QueryRowContext(
			ctx,
			`SELECT reason, outcome, count_failure, requires_operator,
				fence_token, fence_generation
			 FROM run_reclaim_requests WHERE run_id = ?`,
			run.ID,
		).Scan(
			&currentReason,
			&currentOutcome,
			&currentCountFailure,
			&currentRequiresOperator,
			&currentFenceToken,
			&currentFenceGeneration,
		)
		exists := currentErr == nil
		if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
			return currentErr
		}
		// Only the audited quiescence-confirmation transaction can lower an
		// operator fence. Ordinary Supervisor refreshes preserve it.
		if exists && currentRequiresOperator && !requiresOperator {
			return nil
		}
		if exists && currentRequiresOperator && requiresOperator &&
			currentReason == reason &&
			currentOutcome == string(outcome) &&
			currentCountFailure == countFailure {
			return nil
		}
		operatorTransition := requiresOperator &&
			(!exists || !currentRequiresOperator)
		generation := 1
		if exists {
			fenceToken = currentFenceToken
			generation = currentFenceGeneration
			if operatorTransition {
				generation++
			}
		}
		expires := futureISO(max(1, seconds))
		requestedAt := now()
		var diagnosticValue any
		if requiresOperator {
			diagnosticValue = diagnosticCode
		}
		if _, err := tx.ExecContext(ctx, "UPDATE task_runs SET claim_expires_at = ? WHERE id = ?", expires, runID); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_reclaim_requests(
					run_id, expires_at, reason, outcome, count_failure, requested_at,
					requires_operator, diagnostic_code, fence_token, fence_generation
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				run.ID, expires, reason, outcome, countFailure, requestedAt,
				requiresOperator, diagnosticValue, fenceToken, generation); err != nil {
				return err
			}
		} else if operatorTransition {
			result, err := tx.ExecContext(ctx, `UPDATE run_reclaim_requests SET
					expires_at = ?, reason = ?, outcome = ?, count_failure = ?,
					requested_at = ?, requires_operator = 1,
					diagnostic_code = ?, fence_generation = ?,
					recovery_owner_token = NULL,
					recovery_owner_expires_at = NULL,
					operator_quiesced_at = NULL,
					operator_quiesced_by = NULL,
					operator_quiescence_reason = NULL,
					operator_quiesced_generation = NULL,
					operator_confirmed_worker_stopped = 0,
					operator_confirmed_host_writes_stopped = 0,
					operator_observed_heartbeat_at = NULL,
					operator_observed_claim_expires_at = NULL,
					operator_observed_pid = NULL,
					operator_observed_process_identity = NULL
				 WHERE run_id = ? AND fence_token = ?
				   AND fence_generation = ? AND requires_operator = 0`,
				expires, reason, outcome, countFailure, requestedAt,
				diagnosticValue, generation, run.ID, fenceToken,
				currentFenceGeneration)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return fmt.Errorf(
					"%w: run %s fence changed before operator transition",
					ErrRunRecoveryObservationChanged,
					run.ID,
				)
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE run_reclaim_requests SET
					expires_at = ?, reason = ?, outcome = ?, count_failure = ?,
					requested_at = ?
				 WHERE run_id = ? AND fence_token = ?
				   AND fence_generation = ?`,
				expires, reason, outcome, countFailure, requestedAt,
				run.ID, fenceToken, currentFenceGeneration)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows != 1 {
				return fmt.Errorf(
					"%w: run %s recovery fence changed",
					ErrRunRecoveryObservationChanged,
					run.ID,
				)
			}
		}
		if requiresOperator && !operatorTransition {
			return nil
		}
		event := "reclaim_deferred"
		if requiresOperator {
			event = "reclaim_requires_operator"
		}
		return appendEvent(ctx, tx, run.TaskID, event, map[string]any{
			"pid": run.PID, "expires": expires, "reason": reason,
			"outcome": outcome, "countFailure": countFailure,
			"diagnosticCode": diagnosticValue, "fenceGeneration": generation,
		}, &run.ID)
	})
	if err != nil {
		return model.Run{}, err
	}
	return getRun(ctx, s.db, runID)
}

func (s *Store) DeferReclaim(ctx context.Context, runID string, seconds int, reason string) (model.Run, error) {
	return s.deferRunRecovery(ctx, runID, nil, seconds, reason, model.RunStatusReclaimed, false, false, "")
}

func (s *Store) DeferTimedOutRun(ctx context.Context, runID string, seconds int, reason string) (model.Run, error) {
	return s.deferRunRecovery(ctx, runID, nil, seconds, reason, model.RunStatusTimedOut, true, false, "")
}

// DeferObservedReclaim persists the automatic Supervisor's termination intent
// only if no owner update won after its liveness observation.
func (s *Store) DeferObservedReclaim(
	ctx context.Context,
	observation RunRecoveryObservation,
	seconds int,
	reason string,
) (model.Run, error) {
	return s.deferRunRecovery(
		ctx,
		observation.RunID,
		&observation,
		seconds,
		reason,
		model.RunStatusReclaimed,
		false,
		false,
		"",
	)
}

// DeferObservedTimedOutRun is the maximum-runtime counterpart to
// DeferObservedReclaim.
func (s *Store) DeferObservedTimedOutRun(
	ctx context.Context,
	observation RunRecoveryObservation,
	seconds int,
	reason string,
) (model.Run, error) {
	return s.deferRunRecovery(
		ctx,
		observation.RunID,
		&observation,
		seconds,
		reason,
		model.RunStatusTimedOut,
		true,
		false,
		"",
	)
}

// FenceObservedRunRecovery wins durable recovery ownership before the
// Supervisor signals a process or reads and checkpoints a mutable worktree.
// Lease renewal and new process registration reject this fence.
func (s *Store) FenceObservedRunRecovery(
	ctx context.Context,
	observation RunRecoveryObservation,
	seconds int,
	reason string,
	outcome model.RunStatus,
	countFailure bool,
) (model.Run, error) {
	return s.deferRunRecovery(
		ctx,
		observation.RunID,
		&observation,
		seconds,
		reason,
		outcome,
		countFailure,
		false,
		"",
	)
}

// RequireObservedRunRecoveryIntervention durably exposes an unsafe process
// ownership state without repeatedly emitting the same event on every sweep.
func (s *Store) RequireObservedRunRecoveryIntervention(
	ctx context.Context,
	observation RunRecoveryObservation,
	seconds int,
	reason string,
	outcome model.RunStatus,
	countFailure bool,
) (model.Run, error) {
	return s.deferRunRecovery(
		ctx,
		observation.RunID,
		&observation,
		seconds,
		reason,
		outcome,
		countFailure,
		true,
		"unverifiable_process_ownership",
	)
}

// RequireObservedRunRecoveryInterventionWithDiagnostic records an explicit,
// stable operator incident code. It is used when the Supervisor has a stronger
// reason than ambiguous process ownership, such as unconfirmed subprocess
// teardown. Repeated identical incidents remain one-shot.
func (s *Store) RequireObservedRunRecoveryInterventionWithDiagnostic(
	ctx context.Context,
	observation RunRecoveryObservation,
	seconds int,
	reason string,
	outcome model.RunStatus,
	countFailure bool,
	diagnosticCode string,
) (model.Run, error) {
	return s.deferRunRecovery(
		ctx,
		observation.RunID,
		&observation,
		seconds,
		reason,
		outcome,
		countFailure,
		true,
		diagnosticCode,
	)
}

// GetDeferredReclaim reads coordination state directly by run ID. It is not
// subject to task-event retention.
func (s *Store) GetDeferredReclaim(ctx context.Context, runID string) (*DeferredReclaim, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, errors.New("run ID is required")
	}
	var value DeferredReclaim
	var countFailure, requiresOperator bool
	var diagnosticCode, hostAcknowledgedAt, hostAcknowledgedFenceToken sql.NullString
	var recoveryOwnerToken, recoveryOwnerExpiresAt sql.NullString
	var operatorQuiescedAt, operatorQuiescedBy sql.NullString
	var operatorQuiescenceReason sql.NullString
	var operatorQuiescedGeneration sql.NullInt64
	var operatorObservedHeartbeatAt, operatorObservedClaimExpiresAt sql.NullString
	var operatorObservedPID sql.NullInt64
	var operatorObservedProcessIdentity sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT run_id, expires_at, reason, outcome,
			count_failure, requires_operator, diagnostic_code, fence_token,
			fence_generation, host_acknowledged_at,
			host_acknowledged_fence_token, recovery_owner_token,
			recovery_owner_expires_at, operator_quiesced_at,
			operator_quiesced_by, operator_quiescence_reason,
			operator_quiesced_generation,
			operator_confirmed_worker_stopped,
			operator_confirmed_host_writes_stopped,
			operator_observed_heartbeat_at,
			operator_observed_claim_expires_at,
			operator_observed_pid,
			operator_observed_process_identity
		FROM run_reclaim_requests WHERE run_id = ?`, runID).Scan(
		&value.RunID,
		&value.ExpiresAt,
		&value.Reason,
		&value.Outcome,
		&countFailure,
		&requiresOperator,
		&diagnosticCode,
		&value.FenceToken,
		&value.FenceGeneration,
		&hostAcknowledgedAt,
		&hostAcknowledgedFenceToken,
		&recoveryOwnerToken,
		&recoveryOwnerExpiresAt,
		&operatorQuiescedAt,
		&operatorQuiescedBy,
		&operatorQuiescenceReason,
		&operatorQuiescedGeneration,
		&value.OperatorConfirmedWorkerStopped,
		&value.OperatorConfirmedHostWritesStopped,
		&operatorObservedHeartbeatAt,
		&operatorObservedClaimExpiresAt,
		&operatorObservedPID,
		&operatorObservedProcessIdentity,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339Nano, value.ExpiresAt); err != nil {
		return nil, fmt.Errorf("invalid deferred reclaim expiry for run %s: %w", runID, err)
	}
	if value.Outcome == "" {
		value.Outcome = model.RunStatusReclaimed
	}
	if strings.TrimSpace(value.Reason) == "" {
		value.Reason = "Run termination requested"
	}
	value.CountFailure = countFailure
	value.RequiresOperator = requiresOperator
	value.DiagnosticCode = stringPointer(diagnosticCode)
	if !validObservedHostAcknowledgment(
		value.FenceToken,
		hostAcknowledgedAt,
		hostAcknowledgedFenceToken,
	) {
		return nil, fmt.Errorf(
			"invalid host acknowledgment fence token for run %s",
			runID,
		)
	}
	value.HostAcknowledgedAt = stringPointer(hostAcknowledgedAt)
	value.RecoveryOwnerToken = stringPointer(recoveryOwnerToken)
	value.RecoveryOwnerExpiresAt = stringPointer(recoveryOwnerExpiresAt)
	value.OperatorQuiescedAt = stringPointer(operatorQuiescedAt)
	value.OperatorQuiescedBy = stringPointer(operatorQuiescedBy)
	value.OperatorQuiescenceReason = stringPointer(operatorQuiescenceReason)
	if operatorQuiescedGeneration.Valid {
		generation := int(operatorQuiescedGeneration.Int64)
		value.OperatorQuiescedGeneration = &generation
	}
	value.OperatorObservedHeartbeatAt = stringPointer(operatorObservedHeartbeatAt)
	value.OperatorObservedClaimExpiresAt = stringPointer(operatorObservedClaimExpiresAt)
	if operatorObservedPID.Valid {
		pid := int(operatorObservedPID.Int64)
		value.OperatorObservedPID = &pid
	}
	value.OperatorObservedProcessIdentity = stringPointer(
		operatorObservedProcessIdentity,
	)
	return &value, nil
}

func (s *Store) UnblockTask(ctx context.Context, taskID string) (model.TaskDetail, error) {
	return s.UnblockTaskWithVersion(ctx, taskID, nil)
}

// UnblockTaskWithVersion applies optimistic concurrency when expectedUpdatedAt
// is set. Non-interactive callers can continue to use UnblockTask.
func (s *Store) UnblockTaskWithVersion(ctx context.Context, taskID string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	err := s.withWrite(ctx, func(tx *sql.Tx) error {
		task, err := requireTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := requireExpectedTaskVersion(task, expectedUpdatedAt); err != nil {
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
		config, err := s.GetRunAgentConfig(ctx, value.runID)
		if err != nil {
			return nil, err
		}
		result = append(result, ActiveRun{Task: task, Run: run, AgentConfig: config})
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
