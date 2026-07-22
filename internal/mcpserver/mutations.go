package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nn1a/kanban/internal/maintenance"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/runcontrol"
	"github.com/nn1a/kanban/internal/store"
	"github.com/nn1a/kanban/internal/workspace"
)

type bulkInput struct {
	Board    string           `json:"board,omitempty"`
	TaskIDs  []string         `json:"task_ids"`
	Status   model.TaskStatus `json:"status,omitempty"`
	Assignee *string          `json:"assignee,omitempty"`
	Priority *int             `json:"priority,omitempty"`
	Archive  bool             `json:"archive,omitempty"`
	Delete   bool             `json:"delete,omitempty"`
}

type notificationInput struct {
	Board      string   `json:"board,omitempty"`
	TaskID     string   `json:"task_id"`
	Platform   string   `json:"platform"`
	ChatID     string   `json:"chat_id"`
	ThreadID   *string  `json:"thread_id,omitempty"`
	UserID     *string  `json:"user_id,omitempty"`
	EventKinds []string `json:"event_kinds,omitempty"`
	Secret     *string  `json:"secret,omitempty"`
}

type notificationListInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type specifyInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author string `json:"author,omitempty"`
}

type profileRoute struct {
	Name        string        `json:"name"`
	Runtime     model.Runtime `json:"runtime"`
	Description string        `json:"description,omitempty"`
}

type swarmInput struct {
	Board         string              `json:"board,omitempty"`
	Goal          string              `json:"goal"`
	Workers       []profileRoute      `json:"workers"`
	Verifier      profileRoute        `json:"verifier"`
	Synthesizer   profileRoute        `json:"synthesizer"`
	Tenant        *string             `json:"tenant,omitempty"`
	Workspace     *string             `json:"workspace,omitempty"`
	WorkspaceKind model.WorkspaceKind `json:"workspace_kind,omitempty"`
	Blackboard    map[string]any      `json:"blackboard,omitempty"`
}

type updateInput struct {
	Board              string               `json:"board,omitempty"`
	TaskID             string               `json:"task_id"`
	Title              *string              `json:"title,omitempty"`
	Body               *string              `json:"body,omitempty"`
	Tenant             *string              `json:"tenant,omitempty"`
	Assignee           *string              `json:"assignee,omitempty"`
	Runtime            *model.Runtime       `json:"runtime,omitempty"`
	Priority           *int                 `json:"priority,omitempty"`
	Workspace          *string              `json:"workspace,omitempty"`
	WorkspaceKind      *model.WorkspaceKind `json:"workspace_kind,omitempty"`
	Branch             *string              `json:"branch,omitempty"`
	ScheduledAt        *string              `json:"scheduled_at,omitempty"`
	MaxRuntimeSeconds  *int                 `json:"max_runtime_seconds,omitempty"`
	Skills             *[]string            `json:"skills,omitempty"`
	GoalMode           *bool                `json:"goal_mode,omitempty"`
	GoalMaxTurns       *int                 `json:"goal_max_turns,omitempty"`
	WorkflowTemplateID *string              `json:"workflow_template_id,omitempty"`
	CurrentStepKey     *string              `json:"current_step_key,omitempty"`
	Status             *model.TaskStatus    `json:"status,omitempty"`
}

type commentInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	Author string `json:"author,omitempty"`
	Body   string `json:"body"`
}

type linkInput struct {
	Board    string `json:"board,omitempty"`
	ParentID string `json:"parent_id"`
	ChildID  string `json:"child_id"`
}

type subtaskInput struct {
	Board        string `json:"board,omitempty"`
	ParentTaskID string `json:"parent_task_id"`
	SubtaskID    string `json:"subtask_id"`
	Position     *int   `json:"position,omitempty"`
}

type scheduleInput struct {
	Board       string  `json:"board,omitempty"`
	TaskID      string  `json:"task_id"`
	ScheduledAt *string `json:"scheduled_at,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

type claimInput struct {
	Board      string        `json:"board,omitempty"`
	TaskID     string        `json:"task_id,omitempty"`
	Runtime    model.Runtime `json:"runtime,omitempty"`
	WorkerID   string        `json:"worker_id,omitempty"`
	TTLSeconds int           `json:"ttl_seconds,omitempty"`
}

type attachmentInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	Path   string `json:"path,omitempty"`
	URL    string `json:"url,omitempty"`
	Name   string `json:"name,omitempty"`
}

type attachmentRemoveInput struct {
	Board        string `json:"board,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	AttachmentID string `json:"attachment_id"`
}

type heartbeatInput struct {
	Board      string `json:"board,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	ClaimToken string `json:"claim_token,omitempty"`
	Note       string `json:"note,omitempty"`
}

type completeInput struct {
	Board      string         `json:"board,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	ClaimToken string         `json:"claim_token,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Result     string         `json:"result,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Artifacts  []string       `json:"artifacts,omitempty"`
}

type blockInput struct {
	Board      string          `json:"board,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	ClaimToken string          `json:"claim_token,omitempty"`
	Reason     string          `json:"reason"`
	Kind       model.BlockKind `json:"kind,omitempty"`
}

type terminateInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type garbageCollectionInput struct {
	Board                  string `json:"board,omitempty"`
	EventRetentionDays     *int   `json:"event_retention_days,omitempty"`
	LogRetentionDays       *int   `json:"log_retention_days,omitempty"`
	WorkspaceRetentionDays *int   `json:"workspace_retention_days,omitempty"`
}

func optionalString(value *string) store.OptionalString {
	if value == nil {
		return store.OptionalString{}
	}
	return store.OptionalString{Set: true, Value: value}
}

func optionalInt(value *int) store.OptionalInt {
	if value == nil {
		return store.OptionalInt{}
	}
	return store.OptionalInt{Set: true, Value: value}
}

func (s *Service) registerMutations(server *mcp.Server) {
	addTool(server, "kanban_gc", "Garbage collect Kanban data", "Delete expired events, worker logs, and verified terminal scratch workspaces.", false, true, true, false, func(ctx context.Context, input garbageCollectionInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		board, err := s.selectedBoard(input.Board)
		if err != nil {
			return nil, err
		}
		events, logs, workspaces := 30, 30, 7
		if input.EventRetentionDays != nil {
			events = *input.EventRetentionDays
		}
		if input.LogRetentionDays != nil {
			logs = *input.LogRetentionDays
		}
		if input.WorkspaceRetentionDays != nil {
			workspaces = *input.WorkspaceRetentionDays
		}
		return maintenance.Collect(ctx, s.manager, board, maintenance.Options{EventRetentionDays: events, LogRetentionDays: logs, WorkspaceRetentionDays: workspaces})
	})
	addTool(server, "kanban_run_terminate", "Terminate a TaskCircuit worker run", "Persist termination intent, signal a live worker, and reclaim a missing process.", false, true, false, false, func(ctx context.Context, input terminateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if (input.TaskID == "") == (input.RunID == "") {
			return nil, errors.New("provide exactly one of task_id or run_id")
		}
		if input.Reason == "" {
			input.Reason = "Run terminated through TaskCircuit MCP"
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			if input.RunID != "" {
				return runcontrol.TerminateRun(ctx, opened, input.RunID, input.Reason)
			}
			return runcontrol.TerminateTaskRun(ctx, opened, input.TaskID, input.Reason)
		})
	})
	addTool(server, "kanban_bulk", "Bulk mutate Kanban tasks", "Apply one mutation with per-task success and error results.", false, true, false, false, func(ctx context.Context, input bulkInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if len(input.TaskIDs) == 0 {
			return nil, errors.New("task_ids cannot be empty")
		}
		var status *model.TaskStatus
		if input.Status != "" {
			status = &input.Status
		}
		assignee := optionalString(input.Assignee)
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.BulkMutate(ctx, input.TaskIDs, store.BulkMutation{Status: status, Assignee: assignee, Priority: input.Priority, Archive: input.Archive, Delete: input.Delete}), nil
		})
	})
	addTool(server, "kanban_notify_subscribe", "Subscribe to Kanban task notifications", "Subscribe a platform destination to future task events.", false, false, true, false, func(ctx context.Context, input notificationInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.SubscribeTask(ctx, store.SubscriptionInput{TaskID: input.TaskID, Platform: input.Platform, ChatID: input.ChatID,
				ThreadID: input.ThreadID, UserID: input.UserID, EventKinds: input.EventKinds, Secret: optionalString(input.Secret)})
		})
	})
	addTool(server, "kanban_notify_list", "List Kanban notification subscriptions", "List subscriptions without exposing stored secrets.", true, false, true, false, func(ctx context.Context, input notificationListInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ListNotificationSubscriptions(ctx, input.TaskID)
		})
	})
	addTool(server, "kanban_notify_unsubscribe", "Unsubscribe from Kanban task notifications", "Remove a task notification destination and pending deliveries.", false, true, true, false, func(ctx context.Context, input notificationInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		removed, err := usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (bool, error) {
			return opened.UnsubscribeTask(ctx, input.TaskID, input.Platform, input.ChatID, input.ThreadID)
		})
		return map[string]any{"taskId": input.TaskID, "unsubscribed": removed}, err
	})
	addTool(server, "kanban_specify", "Specify a Kanban triage task", "Apply an explicit executable title and body to a triage task.", false, false, false, false, func(ctx context.Context, input specifyInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Body) == "" {
			return nil, errors.New("title and body must be provided together")
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.SpecifyTask(ctx, input.TaskID, input.Title, input.Body, input.Author)
		})
	})
	addTool(server, "kanban_swarm", "Create a Kanban swarm", "Create a completed blackboard, parallel workers, verifier, and synthesizer.", false, false, false, false, func(ctx context.Context, input swarmInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		workers := make([]store.SwarmRoute, 0, len(input.Workers))
		for _, route := range input.Workers {
			workers = append(workers, store.SwarmRoute{Assignee: route.Name, Runtime: route.Runtime})
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.CreateSwarm(ctx, store.SwarmInput{Goal: input.Goal, Workers: workers,
				Verifier:    store.SwarmRoute{Assignee: input.Verifier.Name, Runtime: input.Verifier.Runtime},
				Synthesizer: store.SwarmRoute{Assignee: input.Synthesizer.Name, Runtime: input.Synthesizer.Runtime},
				Tenant:      input.Tenant, Workspace: input.Workspace, WorkspaceKind: input.WorkspaceKind, Blackboard: input.Blackboard})
		})
	})
	addTool(server, "kanban_update", "Update Kanban task", "Update task metadata or perform an administrative status transition.", false, true, true, false, func(ctx context.Context, input updateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.UpdateTask(ctx, input.TaskID, store.UpdateTaskInput{Title: input.Title, Body: input.Body,
				Tenant: optionalString(input.Tenant), Assignee: optionalString(input.Assignee), Runtime: input.Runtime, Priority: input.Priority,
				Workspace: optionalString(input.Workspace), WorkspaceKind: input.WorkspaceKind, Branch: optionalString(input.Branch),
				ScheduledAt: optionalString(input.ScheduledAt), MaxRuntimeSeconds: optionalInt(input.MaxRuntimeSeconds), Skills: input.Skills,
				GoalMode: input.GoalMode, GoalMaxTurns: input.GoalMaxTurns, WorkflowTemplateID: optionalString(input.WorkflowTemplateID),
				CurrentStepKey: optionalString(input.CurrentStepKey), Status: input.Status})
		})
	})
	addTool(server, "kanban_comment", "Comment on Kanban task", "Append a durable handoff or progress note.", false, false, false, false, func(ctx context.Context, input commentInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		if input.Author == "" {
			input.Author = "agent"
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.AddComment(ctx, taskID, input.Author, input.Body)
		})
	})
	addTool(server, "kanban_link", "Link Kanban dependency", "Create a prerequisite-to-dependent execution edge.", false, false, true, false, s.linkHandler(true))
	addTool(server, "kanban_unlink", "Unlink Kanban dependency", "Remove an execution edge and recompute readiness.", false, true, true, false, s.linkHandler(false))
	addTool(server, "kanban_subtask_set", "Set TaskCircuit subtask parent", "Place a task under one hierarchy parent without changing dependencies.", false, false, true, false, s.subtaskHandler(true))
	addTool(server, "kanban_subtask_remove", "Remove TaskCircuit subtask parent", "Remove a hierarchy edge without changing dependencies.", false, true, true, false, s.subtaskHandler(false))
	addTool(server, "kanban_promote", "Promote Kanban task", "Move a parked task into the executable pipeline.", false, true, false, false, s.adminTaskHandler("promote"))
	addTool(server, "kanban_archive", "Archive Kanban task", "Archive a task after any active run has ended.", false, true, true, false, s.adminTaskHandler("archive"))
	addTool(server, "kanban_delete", "Delete Kanban task", "Permanently delete a task and related durable records.", false, true, false, false, s.adminTaskHandler("delete"))
	addTool(server, "kanban_unblock", "Unblock Kanban task", "Release a blocked task back to todo or ready.", false, true, false, false, s.adminTaskHandler("unblock"))
	addTool(server, "kanban_schedule", "Schedule Kanban task", "Park a task until an optional ISO-8601 time or manual promotion.", false, true, true, false, func(ctx context.Context, input scheduleInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ScheduleTask(ctx, input.TaskID, input.ScheduledAt, input.Reason)
		})
	})
	addTool(server, "kanban_claim", "Claim Kanban task", "Atomically claim one ready task and create a run lease.", false, false, false, false, func(ctx context.Context, input claimInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if input.TTLSeconds == 0 {
			input.TTLSeconds = 900
		}
		if input.WorkerID == "" {
			input.WorkerID = fmt.Sprintf("mcp-%d", os.Getpid())
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) {
			claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: input.TaskID, Board: board, Runtime: input.Runtime,
				WorkerID: input.WorkerID, ClaimTTLSeconds: input.TTLSeconds})
			if err != nil || claim == nil {
				return claim, err
			}
			prepared, err := workspace.New(s.manager).Prepare(ctx, opened, claim)
			if err != nil {
				message := "Workspace preparation failed: " + err.Error()
				_, _ = opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, message, store.FailRunOptions{})
				return nil, err
			}
			return prepared, nil
		})
	})
	addTool(server, "kanban_attach", "Attach file to Kanban task", "Copy a local file into durable board-scoped storage.", false, false, false, false, s.attachmentHandler("file"))
	addTool(server, "kanban_attach_url", "Attach URL to Kanban task", "Add an HTTP(S) reference to a task.", false, false, false, true, s.attachmentHandler("url"))
	addTool(server, "kanban_attachments", "List Kanban attachments", "List durable files and URL references for a task.", true, false, true, false, s.attachmentHandler("list"))
	addTool(server, "kanban_attachment_remove", "Remove Kanban attachment", "Remove attachment metadata and its stored file.", false, true, false, false, func(ctx context.Context, input attachmentRemoveInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			if err := opened.RemoveAttachment(ctx, taskID, input.AttachmentID); err != nil {
				return nil, err
			}
			return map[string]any{"id": input.AttachmentID, "removed": true}, nil
		})
	})
	addTool(server, "kanban_heartbeat", "Heartbeat Kanban run", "Refresh the active run lease and record an optional note.", false, false, false, false, func(ctx context.Context, input heartbeatInput) (any, error) {
		scope, err := s.scopedRun(input.RunID, input.ClaimToken)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) { return opened.Heartbeat(ctx, scope, input.Note) })
	})
	addTool(server, "kanban_complete", "Complete Kanban run", "Complete the active run with a summary and structured evidence.", false, true, false, false, func(ctx context.Context, input completeInput) (any, error) {
		scope, err := s.scopedRun(input.RunID, input.ClaimToken)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.CompleteRun(ctx, scope, store.CompletionInput{Summary: input.Summary, Result: input.Result, Metadata: input.Metadata, Artifacts: input.Artifacts})
		})
	})
	addTool(server, "kanban_block", "Block Kanban run", "Stop an active run for input, capability, dependency, or transient reasons.", false, true, false, false, func(ctx context.Context, input blockInput) (any, error) {
		scope, err := s.scopedRun(input.RunID, input.ClaimToken)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.BlockRun(ctx, scope, store.BlockInput{Reason: input.Reason, Kind: input.Kind})
		})
	})
}

func (s *Service) linkHandler(link bool) func(context.Context, linkInput) (any, error) {
	return func(ctx context.Context, input linkInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			if link {
				return opened.LinkTasks(ctx, input.ParentID, input.ChildID)
			}
			return opened.UnlinkTasks(ctx, input.ParentID, input.ChildID)
		})
	}
}

func (s *Service) subtaskHandler(set bool) func(context.Context, subtaskInput) (any, error) {
	return func(ctx context.Context, input subtaskInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			var detail model.TaskDetail
			var err error
			if set {
				detail, err = opened.SetSubtaskParent(ctx, input.ParentTaskID, input.SubtaskID, input.Position)
			} else {
				detail, err = opened.RemoveSubtask(ctx, input.ParentTaskID, input.SubtaskID)
			}
			if err != nil {
				return nil, err
			}
			graph, err := opened.RelationshipGraph(ctx, input.SubtaskID)
			return map[string]any{"detail": detail, "graph": graph}, err
		})
	}
}

func (s *Service) adminTaskHandler(action string) func(context.Context, taskInput) (any, error) {
	return func(ctx context.Context, input taskInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			switch action {
			case "promote":
				return opened.PromoteTask(ctx, input.TaskID)
			case "archive":
				return opened.ArchiveTask(ctx, input.TaskID)
			case "unblock":
				return opened.UnblockTask(ctx, input.TaskID)
			case "delete":
				if err := opened.DeleteTask(ctx, input.TaskID); err != nil {
					return nil, err
				}
				return map[string]any{"id": input.TaskID, "deleted": true}, nil
			default:
				return nil, fmt.Errorf("unknown task action: %s", action)
			}
		})
	}
}

func (s *Service) attachmentHandler(action string) func(context.Context, attachmentInput) (any, error) {
	return func(ctx context.Context, input attachmentInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			switch action {
			case "file":
				return opened.AttachFile(ctx, taskID, input.Path, input.Name)
			case "url":
				return opened.AttachURL(ctx, taskID, input.URL, input.Name)
			case "list":
				return opened.ListAttachments(ctx, taskID)
			default:
				return nil, fmt.Errorf("unknown attachment action: %s", action)
			}
		})
	}
}
