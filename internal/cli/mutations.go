package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nn1a/kanban/internal/maintenance"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/runcontrol"
	"github.com/nn1a/kanban/internal/store"
	"github.com/nn1a/kanban/internal/workspace"
)

func optionalString(opts options, name string, noneIsNil bool) store.OptionalString {
	result := store.OptionalString{Set: opts.present(name)}
	if !result.Set {
		return result
	}
	value := opts.value(name)
	if noneIsNil && value == "none" {
		return result
	}
	result.Value = &value
	return result
}

func optionalInt(opts options, name string) (store.OptionalInt, error) {
	result := store.OptionalInt{Set: opts.present(name)}
	if !result.Set {
		return result, nil
	}
	value, err := numberOption(opts.value(name), 0)
	if err != nil {
		return store.OptionalInt{}, err
	}
	result.Value = &value
	return result, nil
}

func boolPointer(value bool) *bool { return &value }

func (a *App) runTaskMutation(ctx context.Context, command string, opts options) error {
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	switch command {
	case "edit":
		if len(opts.positionals) == 0 {
			return errors.New("edit requires a task id")
		}
		input := store.UpdateTaskInput{Assignee: optionalString(opts, "assignee", true), Tenant: optionalString(opts, "tenant", true), Workspace: optionalString(opts, "workspace", true), Branch: optionalString(opts, "branch", true)}
		if opts.present("title") {
			value := opts.value("title")
			input.Title = &value
		}
		if opts.present("body") {
			value := opts.value("body")
			input.Body = &value
		}
		if opts.present("runtime") {
			value, err := requireRuntime(opts.value("runtime"), model.RuntimeManual)
			if err != nil {
				return err
			}
			input.Runtime = &value
		}
		if opts.present("priority") {
			value, err := numberOption(opts.value("priority"), 0)
			if err != nil {
				return err
			}
			input.Priority = &value
		}
		if opts.present("workspace-kind") {
			value, err := requireWorkspaceKind(opts.value("workspace-kind"))
			if err != nil || value == "" {
				return fmt.Errorf("invalid workspace kind: %s", opts.value("workspace-kind"))
			}
			input.WorkspaceKind = &value
		}
		if opts.present("status") {
			input.Status, err = requireStatus(opts.value("status"))
			if err != nil {
				return err
			}
		}
		input.ScheduledAt = optionalString(opts, "scheduled-at", true)
		input.MaxRuntimeSeconds, err = optionalInt(opts, "max-runtime-seconds")
		if err != nil {
			return err
		}
		if opts.present("skill") {
			values := opts.many("skill")
			input.Skills = &values
		}
		if opts.flags["goal"] {
			input.GoalMode = boolPointer(true)
		}
		if opts.present("goal-max-turns") {
			value, err := numberOption(opts.value("goal-max-turns"), 20)
			if err != nil {
				return err
			}
			input.GoalMaxTurns = &value
		}
		detail, err := opened.UpdateTask(ctx, opts.positionals[0], input)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, detail)
	case "assign":
		if len(opts.positionals) < 2 {
			return errors.New("assign requires a task id and assignee (or 'none')")
		}
		assignee := store.OptionalString{Set: true}
		if opts.positionals[1] != "none" {
			assignee.Value = &opts.positionals[1]
		}
		detail, err := opened.UpdateTask(ctx, opts.positionals[0], store.UpdateTaskInput{Assignee: assignee})
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, detail)
	case "reassign":
		if len(opts.positionals) < 2 {
			return errors.New("reassign requires at least one task id and an assignee")
		}
		value := opts.positionals[len(opts.positionals)-1]
		assignee := store.OptionalString{Set: true}
		if value != "none" {
			assignee.Value = &value
		}
		result := opened.BulkMutate(ctx, opts.positionals[:len(opts.positionals)-1], store.BulkMutation{Assignee: assignee})
		return writeJSON(a.Stdout, result)
	case "link", "unlink":
		if len(opts.positionals) < 2 {
			return fmt.Errorf("%s requires parent and child task ids", command)
		}
		var detail model.TaskDetail
		if command == "link" {
			detail, err = opened.LinkTasks(ctx, opts.positionals[0], opts.positionals[1])
		} else {
			detail, err = opened.UnlinkTasks(ctx, opts.positionals[0], opts.positionals[1])
		}
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, detail)
	case "subtask-add", "subtask-rm":
		if len(opts.positionals) < 2 {
			return fmt.Errorf("%s requires parent task and subtask ids", command)
		}
		parentID, childID := opts.positionals[0], opts.positionals[1]
		var detail model.TaskDetail
		if command == "subtask-add" {
			var position *int
			if opts.present("position") {
				value, err := numberOption(opts.value("position"), 0)
				if err != nil {
					return err
				}
				position = &value
			}
			detail, err = opened.SetSubtaskParent(ctx, parentID, childID, position)
		} else {
			detail, err = opened.RemoveSubtask(ctx, parentID, childID)
		}
		if err != nil {
			return err
		}
		graph, err := opened.RelationshipGraph(ctx, childID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, struct {
			model.TaskDetail
			RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
		}{TaskDetail: detail, RelationshipGraph: graph})
	}
	return fmt.Errorf("unsupported mutation: %s", command)
}

func (a *App) runWorkerMutation(ctx context.Context, command string, opts options) error {
	requested := ""
	if len(opts.positionals) > 0 {
		requested = opts.positionals[0]
	}
	taskID, err := a.scopedTaskID(requested, command)
	if err != nil {
		return err
	}
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	switch command {
	case "heartbeat":
		scope, err := a.scopedRun()
		if err != nil {
			return err
		}
		var run model.Run
		if scope != nil {
			run, err = opened.Heartbeat(ctx, *scope, opts.value("note"))
		} else {
			run, err = opened.HeartbeatTask(ctx, taskID, opts.value("note"))
		}
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, run)
	case "comment":
		body := strings.TrimSpace(strings.Join(opts.positionals[1:], " "))
		if a.env("TASKCIRCUIT_TASK_ID", "KANBAN_TASK_ID") != "" && requested == "" {
			body = strings.TrimSpace(strings.Join(opts.positionals, " "))
		}
		if body == "" {
			return errors.New("comment requires text")
		}
		author := opts.value("author")
		if author == "" {
			author = "human"
		}
		comment, err := opened.AddComment(ctx, taskID, author, body)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, comment)
	case "attachments":
		values, err := opened.ListAttachments(ctx, taskID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, values)
	case "attach", "attach-url", "attach-rm":
		if len(opts.positionals) < 2 {
			return fmt.Errorf("%s requires a path, URL, or attachment id", command)
		}
		value := opts.positionals[1]
		if command == "attach" {
			attachment, err := opened.AttachFile(ctx, taskID, value, opts.value("name"))
			if err != nil {
				return err
			}
			return writeJSON(a.Stdout, attachment)
		}
		if command == "attach-url" {
			attachment, err := opened.AttachURL(ctx, taskID, value, opts.value("name"))
			if err != nil {
				return err
			}
			return writeJSON(a.Stdout, attachment)
		}
		if err := opened.RemoveAttachment(ctx, taskID, value); err != nil {
			return err
		}
		return writeJSON(a.Stdout, map[string]any{"id": value, "removed": true})
	}
	return fmt.Errorf("unsupported worker mutation: %s", command)
}

func requireBlockKind(value string) (model.BlockKind, error) {
	kind := model.BlockKind(value)
	if kind == "" || kind == model.BlockKindDependency || kind == model.BlockKindNeedsInput || kind == model.BlockKindCapability || kind == model.BlockKindTransient {
		return kind, nil
	}
	return "", fmt.Errorf("invalid block kind: %s", value)
}

func (a *App) runLifecycle(ctx context.Context, command string, opts options) error {
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	if command == "complete" {
		taskIDs := append([]string{}, opts.positionals...)
		if len(taskIDs) == 0 {
			if pinned := a.env("TASKCIRCUIT_TASK_ID", "KANBAN_TASK_ID"); pinned != "" {
				taskIDs = []string{pinned}
			}
		}
		if len(taskIDs) == 0 {
			return errors.New("complete requires at least one task id")
		}
		structured := opts.present("summary") || opts.present("result") || opts.present("metadata") || opts.present("artifact")
		if len(taskIDs) > 1 && structured {
			return errors.New("structured completion handoff is only allowed for one task at a time")
		}
		metadata, err := parseMetadata(opts.value("metadata"))
		if err != nil {
			return err
		}
		completion := store.CompletionInput{Summary: opts.value("summary"), Result: opts.value("result"), Metadata: metadata, Artifacts: opts.many("artifact")}
		scope, err := a.scopedRun()
		if err != nil {
			return err
		}
		results := []model.TaskDetail{}
		if scope != nil {
			value, err := opened.CompleteRun(ctx, *scope, completion)
			if err != nil {
				return err
			}
			results = append(results, value)
		} else {
			for _, requested := range taskIDs {
				taskID, err := a.scopedTaskID(requested, command)
				if err != nil {
					return err
				}
				value, err := opened.CompleteTask(ctx, taskID, completion)
				if err != nil {
					return err
				}
				results = append(results, value)
			}
		}
		return writeJSON(a.Stdout, results)
	}
	if command == "block" {
		if len(opts.positionals) == 0 && a.env("TASKCIRCUIT_TASK_ID", "KANBAN_TASK_ID") == "" {
			return errors.New("block requires a task id")
		}
		requested := ""
		if len(opts.positionals) > 0 {
			requested = opts.positionals[0]
		}
		taskID, err := a.scopedTaskID(requested, command)
		if err != nil {
			return err
		}
		reasonIndex := 1
		if requested == "" {
			reasonIndex = 0
		}
		if len(opts.positionals) <= reasonIndex || strings.TrimSpace(opts.positionals[reasonIndex]) == "" {
			return errors.New("block requires a reason")
		}
		reason := strings.TrimSpace(opts.positionals[reasonIndex])
		kind, err := requireBlockKind(opts.value("kind"))
		if err != nil {
			return err
		}
		scope, err := a.scopedRun()
		if err != nil {
			return err
		}
		results := []model.TaskDetail{}
		if scope != nil {
			value, err := opened.BlockRun(ctx, *scope, store.BlockInput{Reason: reason, Kind: kind})
			if err != nil {
				return err
			}
			results = append(results, value)
		} else {
			ids := []string{taskID}
			ids = append(ids, opts.positionals[reasonIndex+1:]...)
			ids = append(ids, opts.many("ids")...)
			for _, id := range ids {
				value, err := opened.BlockTask(ctx, id, store.BlockInput{Reason: reason, Kind: kind})
				if err != nil {
					return err
				}
				results = append(results, value)
			}
		}
		return writeJSON(a.Stdout, results)
	}
	if command == "schedule" {
		if len(opts.positionals) == 0 {
			return errors.New("schedule requires a task id")
		}
		var scheduledAt *string
		if opts.present("at") {
			value := opts.value("at")
			scheduledAt = &value
		}
		value, err := opened.ScheduleTask(ctx, opts.positionals[0], scheduledAt, opts.value("reason"))
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	}
	if len(opts.positionals) == 0 {
		return fmt.Errorf("%s requires at least one task id", command)
	}
	results := make([]any, 0, len(opts.positionals))
	for _, taskID := range opts.positionals {
		var value any
		switch command {
		case "unblock":
			value, err = opened.UnblockTask(ctx, taskID)
		case "promote":
			value, err = opened.PromoteTask(ctx, taskID)
		case "archive":
			value, err = opened.ArchiveTask(ctx, taskID)
		case "delete":
			err = opened.DeleteTask(ctx, taskID)
			value = map[string]any{"id": taskID, "deleted": true}
		}
		if err != nil {
			return err
		}
		results = append(results, value)
	}
	return writeJSON(a.Stdout, results)
}

func (a *App) runNotifications(ctx context.Context, command string, opts options) error {
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	if command == "notify-list" {
		taskID := ""
		if len(opts.positionals) > 0 {
			taskID = opts.positionals[0]
		}
		values, err := opened.ListNotificationSubscriptions(ctx, taskID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, values)
	}
	if len(opts.positionals) == 0 || opts.value("platform") == "" || opts.value("chat-id") == "" {
		return fmt.Errorf("%s requires a task id, --platform, and --chat-id", command)
	}
	taskID, platform, chatID := opts.positionals[0], opts.value("platform"), opts.value("chat-id")
	threadID := stringPointer(opts.value("thread-id"))
	if command == "notify-unsubscribe" {
		removed, err := opened.UnsubscribeTask(ctx, taskID, platform, chatID, threadID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, map[string]any{"taskId": taskID, "unsubscribed": removed})
	}
	secret := store.OptionalString{}
	if opts.flags["clear-secret"] {
		secret.Set = true
	} else if opts.present("secret") {
		secret.Set, secret.Value = true, stringPointer(opts.value("secret"))
	}
	value, err := opened.SubscribeTask(ctx, store.SubscriptionInput{TaskID: taskID, Platform: platform, ChatID: chatID,
		ThreadID: threadID, UserID: stringPointer(opts.value("user-id")), EventKinds: parseKinds(opts.value("kinds")), Secret: secret})
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, value)
}

type profileRoute struct {
	Assignee string
	Runtime  model.Runtime
}

func parseProfileRoute(value string) (profileRoute, error) {
	parts := strings.Split(value, ":")
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return profileRoute{}, fmt.Errorf("invalid profile route: %s", value)
	}
	runtimeValue := "codex"
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		runtimeValue = strings.TrimSpace(parts[1])
	}
	runtime, err := requireRuntime(runtimeValue, model.RuntimeCodex)
	if err != nil || runtime == model.RuntimeManual {
		return profileRoute{}, fmt.Errorf("invalid profile runtime in %s", value)
	}
	return profileRoute{Assignee: name, Runtime: runtime}, nil
}

func (a *App) runSwarm(ctx context.Context, opts options) error {
	goal := strings.TrimSpace(strings.Join(opts.positionals, " "))
	if goal == "" || opts.value("workers") == "" || opts.value("verifier") == "" || opts.value("synthesizer") == "" {
		return errors.New("swarm requires a goal, --workers, --verifier, and --synthesizer")
	}
	workers := []store.SwarmRoute{}
	for _, raw := range strings.Split(opts.value("workers"), ",") {
		route, err := parseProfileRoute(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		workers = append(workers, store.SwarmRoute{Assignee: route.Assignee, Runtime: route.Runtime})
	}
	verifier, err := parseProfileRoute(opts.value("verifier"))
	if err != nil {
		return err
	}
	synthesizer, err := parseProfileRoute(opts.value("synthesizer"))
	if err != nil {
		return err
	}
	workspaceKind, err := requireWorkspaceKind(opts.value("workspace-kind"))
	if err != nil {
		return err
	}
	blackboard, err := parseMetadata(opts.value("blackboard"))
	if err != nil {
		return err
	}
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	value, err := opened.CreateSwarm(ctx, store.SwarmInput{Goal: goal, Workers: workers,
		Verifier:    store.SwarmRoute{Assignee: verifier.Assignee, Runtime: verifier.Runtime},
		Synthesizer: store.SwarmRoute{Assignee: synthesizer.Assignee, Runtime: synthesizer.Runtime},
		Tenant:      stringPointer(opts.value("tenant")), Workspace: stringPointer(opts.value("workspace")), WorkspaceKind: workspaceKind, Blackboard: blackboard})
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, value)
}

func currentWorkerID() string { return fmt.Sprintf("cli-%d", os.Getpid()) }

func (a *App) runClaim(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("claim requires a task id")
	}
	ttl, err := numberOption(opts.value("ttl"), 900)
	if err != nil {
		return err
	}
	opened, manager, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	workerID := opts.value("worker")
	if workerID == "" {
		workerID = currentWorkerID()
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: opts.positionals[0], Board: board, WorkerID: workerID, ClaimTTLSeconds: ttl})
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("task is not claimable: %s", opts.positionals[0])
	}
	workspaces := workspace.New(manager)
	if a.Cwd != "" {
		workspaces.SetWorkingDirectory(a.Cwd)
	}
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		message := "Workspace preparation failed: " + err.Error()
		_, _ = opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, message, store.FailRunOptions{})
		return err
	}
	return writeJSON(a.Stdout, prepared)
}

func (a *App) runTerminate(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("terminate requires a task id")
	}
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	reason := opts.value("reason")
	if reason == "" {
		reason = "Run terminated from TaskCircuit CLI"
	}
	result, err := runcontrol.TerminateTaskRun(ctx, opened, opts.positionals[0], reason)
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, result)
}

func (a *App) runGarbageCollection(ctx context.Context, opts options) error {
	manager, err := a.managerFor(opts.value("db"))
	if err != nil {
		return err
	}
	board, err := manager.Resolve(a.board(opts))
	if err != nil {
		return err
	}
	events, err := numberOption(opts.value("event-retention-days"), 30)
	if err != nil {
		return err
	}
	logs, err := numberOption(opts.value("log-retention-days"), 30)
	if err != nil {
		return err
	}
	workspaces, err := numberOption(opts.value("workspace-retention-days"), 7)
	if err != nil {
		return err
	}
	result, err := maintenance.Collect(ctx, manager, board, maintenance.Options{EventRetentionDays: events, LogRetentionDays: logs, WorkspaceRetentionDays: workspaces})
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, result)
}
