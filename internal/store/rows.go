package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nn1a/autogora/internal/model"
)

type scanner interface {
	Scan(dest ...any) error
}

const taskColumns = `
id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
priority, workspace, workspace_kind, branch, current_run_id, result, scheduled_at,
max_runtime_seconds, skills_json, goal_mode, goal_max_turns, workflow_template_id,
current_step_key, block_kind, block_reason, block_recurrences, failure_count,
max_retries, created_at, updated_at, workflow_role`

const taskColumnsT = `
t.id, t.board, t.tenant, t.idempotency_key, t.title, t.body, t.assignee, t.runtime, t.status,
t.priority, t.workspace, t.workspace_kind, t.branch, t.current_run_id, t.result, t.scheduled_at,
t.max_runtime_seconds, t.skills_json, t.goal_mode, t.goal_max_turns, t.workflow_template_id,
t.current_step_key, t.block_kind, t.block_reason, t.block_recurrences, t.failure_count,
t.max_retries, t.created_at, t.updated_at, t.workflow_role`

const runColumns = `
id, task_id, worker_id, runtime, status, claimed_at, claim_expires_at, heartbeat_at,
ended_at, pid, log_path, exit_code, summary, metadata_json, error`

func scanTask(row scanner) (model.Task, error) {
	var task model.Task
	var tenant, idempotencyKey, assignee, workspace, branch, currentRunID sql.NullString
	var result, scheduledAt, workflowTemplateID, currentStepKey sql.NullString
	var blockKind, blockReason sql.NullString
	var maxRuntimeSeconds sql.NullInt64
	var runtime, status, workspaceKind, workflowRole string
	var skillsJSON string
	var goalMode int
	if err := row.Scan(
		&task.ID, &task.Board, &tenant, &idempotencyKey, &task.Title, &task.Body,
		&assignee, &runtime, &status, &task.Priority, &workspace, &workspaceKind,
		&branch, &currentRunID, &result, &scheduledAt, &maxRuntimeSeconds, &skillsJSON,
		&goalMode, &task.GoalMaxTurns, &workflowTemplateID, &currentStepKey, &blockKind,
		&blockReason, &task.BlockRecurrences, &task.FailureCount, &task.MaxRetries,
		&task.CreatedAt, &task.UpdatedAt, &workflowRole,
	); err != nil {
		return model.Task{}, err
	}
	task.Tenant = stringPointer(tenant)
	task.IdempotencyKey = stringPointer(idempotencyKey)
	task.Assignee = stringPointer(assignee)
	task.Runtime = model.Runtime(runtime)
	task.Status = model.TaskStatus(status)
	task.WorkflowRole = model.WorkflowRole(workflowRole)
	task.Workspace = stringPointer(workspace)
	task.WorkspaceKind = model.WorkspaceKind(workspaceKind)
	task.Branch = stringPointer(branch)
	task.CurrentRunID = stringPointer(currentRunID)
	task.Result = stringPointer(result)
	task.ScheduledAt = stringPointer(scheduledAt)
	if maxRuntimeSeconds.Valid {
		value := int(maxRuntimeSeconds.Int64)
		task.MaxRuntimeSeconds = &value
	}
	if err := json.Unmarshal([]byte(skillsJSON), &task.Skills); err != nil {
		return model.Task{}, fmt.Errorf("decode task skills: %w", err)
	}
	if task.Skills == nil {
		task.Skills = []string{}
	}
	task.GoalMode = goalMode == 1
	task.WorkflowTemplateID = stringPointer(workflowTemplateID)
	task.CurrentStepKey = stringPointer(currentStepKey)
	if blockKind.Valid {
		value := model.BlockKind(blockKind.String)
		task.BlockKind = &value
	}
	task.BlockReason = stringPointer(blockReason)
	return task, nil
}

func scanRun(row scanner) (model.Run, error) {
	var run model.Run
	var runtime, status string
	var endedAt, logPath, summary, metadataJSON, runError sql.NullString
	var pid, exitCode sql.NullInt64
	if err := row.Scan(
		&run.ID, &run.TaskID, &run.WorkerID, &runtime, &status, &run.ClaimedAt,
		&run.ClaimExpiresAt, &run.HeartbeatAt, &endedAt, &pid, &logPath, &exitCode,
		&summary, &metadataJSON, &runError,
	); err != nil {
		return model.Run{}, err
	}
	run.Runtime = model.Runtime(runtime)
	run.Status = model.RunStatus(status)
	run.EndedAt = stringPointer(endedAt)
	if pid.Valid {
		value := int(pid.Int64)
		run.PID = &value
	}
	run.LogPath = stringPointer(logPath)
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	run.Summary = stringPointer(summary)
	run.Error = stringPointer(runError)
	if metadataJSON.Valid {
		if err := json.Unmarshal([]byte(metadataJSON.String), &run.Metadata); err != nil {
			return model.Run{}, fmt.Errorf("decode run metadata: %w", err)
		}
	}
	return run, nil
}

func scanRunAgentConfig(row scanner) (model.RunAgentConfig, error) {
	var value model.RunAgentConfig
	var runtime string
	var fallbackFrom sql.NullString
	if err := row.Scan(
		&value.RunID, &value.TaskID, &value.Profile, &runtime, &value.Model,
		&value.Provider, &value.Source, &fallbackFrom, &value.ConfiguredAt,
	); err != nil {
		return model.RunAgentConfig{}, err
	}
	value.Runtime = model.Runtime(runtime)
	value.FallbackFrom = stringPointer(fallbackFrom)
	return value, nil
}

func scanComment(row scanner) (model.Comment, error) {
	var comment model.Comment
	err := row.Scan(&comment.ID, &comment.TaskID, &comment.Author, &comment.Body, &comment.CreatedAt)
	return comment, err
}

func scanAttachment(row scanner) (model.Attachment, error) {
	var attachment model.Attachment
	var mediaType, sha256, path, rawURL sql.NullString
	var size sql.NullInt64
	if err := row.Scan(
		&attachment.ID, &attachment.TaskID, &attachment.Kind, &attachment.Name,
		&mediaType, &size, &sha256, &path, &rawURL, &attachment.CreatedAt,
	); err != nil {
		return model.Attachment{}, err
	}
	attachment.MediaType = stringPointer(mediaType)
	if size.Valid {
		value := size.Int64
		attachment.Size = &value
	}
	attachment.SHA256 = stringPointer(sha256)
	attachment.Path = stringPointer(path)
	attachment.URL = stringPointer(rawURL)
	return attachment, nil
}

func scanEvent(row scanner) (model.TaskEvent, error) {
	var event model.TaskEvent
	var runID, payload sql.NullString
	if err := row.Scan(&event.ID, &event.TaskID, &runID, &event.Kind, &payload, &event.CreatedAt); err != nil {
		return model.TaskEvent{}, err
	}
	event.RunID = stringPointer(runID)
	if payload.Valid {
		event.Payload = json.RawMessage(payload.String)
	}
	return event, nil
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
