package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/mcpserver"
	"github.com/nn1a/autogora/internal/model"
	setupcfg "github.com/nn1a/autogora/internal/setup"
	"github.com/nn1a/autogora/internal/store"
)

const Help = `autogora <command> [options]

Commands:
  serve                 Run the stdio MCP server
  init                  Initialize the SQLite database
  paths                 Show resolved project data paths
  boards <action>       List, create, switch, show, rename, or remove boards
  create <title>        Create a task from the shell
  github import         Import GitHub issues into Triage through gh CLI
  list                  List tasks
  show <task-id>        Show task details and bounded worker context
  graph <task-id>       Show hierarchy, dependencies, and execution phases
  context <task-id>     Print the bounded worker context
  runs <task-id>        Show attempt history
  log <task-id>         Read the latest worker log tail
  terminate <task-id>   Signal and reclaim a task's active worker run
  stats                 Show board counts
  diagnostics           Inspect board health and active workers
  coordination <action> Inspect Coordinator incidents and proposals
  publication <action>  Inspect and control publication handoffs
  tail <task-id>        Read or follow one task's events
  watch                 Read or follow the board event stream
  bulk <id>...          Apply a mutation with per-task results
  gc                    Remove expired events, logs, and terminal scratch workspaces
  notify-subscribe <id> Subscribe a destination to task events
  notify-list           List notification subscriptions
  notify-unsubscribe <id> Remove a notification destination
  notify-deliver        Deliver pending notifications
  specify <id>          Expand a triage idea into an executable specification
  decompose <id>        Expand a triage idea into an atomic task graph
  swarm <goal>          Create a blackboard/worker/verifier/synthesizer graph
  edit <task-id>        Edit task metadata
  assign <id> <worker>  Assign or unassign a task
  reassign <id>...      Bulk assign or unassign tasks
  link <parent> <child> Add a dependency
  unlink <parent> <child> Remove a dependency
  subtask-add <p> <c>   Set a task's hierarchy parent
  subtask-rm <p> <c>    Remove a task from its hierarchy parent
  claim <task-id>       Atomically claim and prepare a ready task
  heartbeat <task-id>   Refresh the active run lease
  comment <id> <text>   Append a durable comment
  attach <id> <path>    Copy a durable attachment
  attach-url <id> <url> Attach an HTTP(S) reference
  attachments <id>      List task attachments
  attach-rm <id> <aid>  Remove an attachment
  complete <id>...      Complete one or more tasks
  block <id> <reason>   Block a task with an optional typed reason
  unblock <id>...       Return blocked tasks to the work queue
  promote <id>...       Promote parked tasks into the work queue
  schedule <id>         Park a task until a start time
  archive <id>...       Archive tasks
  delete <id>...        Permanently delete tasks
  dispatch              Run the worker dispatcher
  dashboard             Run the authenticated local web dashboard
  tui                   Open the interactive terminal board
  agents <action>       Configure and detect coding agents
  skills <action>       Install, inspect, or uninstall bundled Agent Skills
  mcp <action>          Register, inspect, or unregister the MCP server
  setup                 Install bundled Skills and register MCP together

Common options:
  --db <path>           Override the project-specific SQLite path
  --board <slug>        Override the current board for this command
`

type App struct {
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	Cwd            string
	Getenv         func(string) string
	Version        string
	CommandRunner  setupcfg.CommandRunner
	DispatchRunner DispatchRunner
}

func New(stdout, stderr io.Writer) *App {
	return &App{Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, Version: "dev"}
}

func (a *App) env(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(a.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func (a *App) workingDirectory() (string, error) {
	if a.Cwd != "" {
		return filepath.Abs(a.Cwd)
	}
	return os.Getwd()
}

func (a *App) defaultDBPath() (string, error) {
	return a.databasePath("")
}

func (a *App) managerFor(value string) (*boards.Manager, error) {
	dbPath, err := a.databasePath(value)
	if err != nil {
		return nil, err
	}
	if a.env("AUTOGORA_TASK_ID") != "" {
		if pinned := a.env("AUTOGORA_DB"); pinned != "" {
			resolvedPinned, _ := filepath.Abs(pinned)
			if dbPath != resolvedPinned {
				return nil, fmt.Errorf("this worker is scoped to database %s", resolvedPinned)
			}
		}
	}
	return boards.NewManager(dbPath)
}

func (a *App) board(opts options) string {
	requested := opts.value("board")
	pinned := a.env("AUTOGORA_BOARD")
	if pinned != "" {
		return pinned
	}
	return requested
}

func (a *App) validateBoardScope(opts options) error {
	pinned := a.env("AUTOGORA_BOARD")
	requested := strings.ToLower(strings.TrimSpace(opts.value("board")))
	if pinned != "" && requested != "" && strings.ToLower(pinned) != requested {
		return fmt.Errorf("this worker is scoped to board %s", pinned)
	}
	return nil
}

func (a *App) openStore(ctx context.Context, opts options) (*store.Store, *boards.Manager, string, error) {
	manager, err := a.managerFor(opts.value("db"))
	if err != nil {
		return nil, nil, "", err
	}
	board, err := manager.Resolve(a.board(opts))
	if err != nil {
		return nil, nil, "", err
	}
	opened, err := manager.OpenStore(ctx, board)
	return opened, manager, board, err
}

func (a *App) scopedTaskID(requested, command string) (string, error) {
	pinned := a.env("AUTOGORA_TASK_ID")
	if pinned != "" && requested != "" && pinned != requested {
		return "", fmt.Errorf("%s is scoped to task %s", command, pinned)
	}
	if pinned != "" {
		return pinned, nil
	}
	if requested == "" {
		return "", fmt.Errorf("%s requires a task id", command)
	}
	return requested, nil
}

func (a *App) scopedRun() (*store.RunScope, error) {
	runID := a.env("AUTOGORA_RUN_ID")
	token := a.env("AUTOGORA_CLAIM_TOKEN")
	if runID == "" && token == "" {
		return nil, nil
	}
	if runID == "" || token == "" {
		return nil, errors.New("scoped worker commands require AUTOGORA_RUN_ID and AUTOGORA_CLAIM_TOKEN")
	}
	return &store.RunScope{RunID: runID, ClaimToken: token}, nil
}

func writeJSON(writer io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "%s\n", encoded)
	return err
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) > 1 && args[0] == "help" {
		if help := setupCommandHelp(args[1]); help != "" {
			_, err := io.WriteString(a.Stdout, help)
			return err
		}
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		if help := setupCommandHelp(args[0]); help != "" {
			_, err := io.WriteString(a.Stdout, help)
			return err
		}
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(a.Stdout, Help)
		return err
	}
	command := args[0]
	opts, err := parseOptions(args[1:])
	if err != nil {
		return err
	}
	if err := a.validateBoardScope(opts); err != nil {
		return err
	}
	if a.env("AUTOGORA_TASK_ID") != "" {
		allowed := map[string]bool{"serve": true, "show": true, "graph": true, "context": true, "runs": true, "log": true, "heartbeat": true, "comment": true, "complete": true, "block": true}
		if !allowed[command] {
			return errors.New("dispatcher-scoped workers may only use Autogora CLI context and lifecycle commands")
		}
	}

	switch command {
	case "serve":
		manager, err := a.managerFor(opts.value("db"))
		if err != nil {
			return err
		}
		return mcpserver.RunStdio(ctx, manager, a.Version)
	case "boards":
		return a.runBoards(ctx, opts)
	case "init":
		return a.runInit(ctx, opts)
	case "paths":
		return a.runPaths(opts)
	case "create":
		return a.runCreate(ctx, opts)
	case "github":
		return a.runGitHub(ctx, opts)
	case "list":
		return a.runList(ctx, opts)
	case "show", "graph", "context", "runs", "log":
		return a.runReadTask(ctx, command, opts)
	case "stats", "diagnostics", "diag", "assignees":
		return a.runStats(ctx, command, opts)
	case "coordination":
		return a.runCoordination(ctx, opts)
	case "publication":
		return a.runPublication(ctx, opts)
	case "tail", "watch":
		return a.runEvents(ctx, command, opts)
	case "bulk":
		return a.runBulk(ctx, opts)
	case "specify", "decompose":
		return a.runOrchestration(ctx, command, opts)
	case "swarm":
		return a.runSwarm(ctx, opts)
	case "dispatch":
		return a.runDispatch(ctx, opts)
	case "dashboard":
		return a.runDashboard(ctx, opts)
	case "tui":
		return a.runTUI(ctx, opts)
	case "agents":
		return a.runAgents(ctx, opts)
	case "skills":
		return a.runSkills(opts)
	case "mcp":
		return a.runMCP(ctx, opts)
	case "setup":
		return a.runSetup(ctx, opts)
	case "claim":
		return a.runClaim(ctx, opts)
	case "terminate":
		return a.runTerminate(ctx, opts)
	case "gc":
		return a.runGarbageCollection(ctx, opts)
	case "edit", "assign", "reassign", "link", "unlink", "subtask-add", "subtask-rm":
		return a.runTaskMutation(ctx, command, opts)
	case "heartbeat", "comment", "attach", "attach-url", "attachments", "attach-rm":
		return a.runWorkerMutation(ctx, command, opts)
	case "complete", "block", "unblock", "promote", "archive", "delete", "schedule":
		return a.runLifecycle(ctx, command, opts)
	case "notify-subscribe", "notify-unsubscribe", "notify-list", "notify-deliver":
		return a.runNotifications(ctx, command, opts)
	default:
		return fmt.Errorf("unknown or not-yet-ported command: %s", command)
	}
}

func (a *App) runInit(ctx context.Context, opts options) error {
	manager, err := a.initManager(opts)
	if err != nil {
		return err
	}
	if _, err := manager.Create(ctx, "default", boards.Update{}); err != nil {
		return err
	}
	store, err := manager.OpenStore(ctx, "default")
	if err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}
	metadata, err := manager.Read("default")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Stdout, metadata.DBPath)
	return err
}

func (a *App) runBoards(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("boards requires list, create, switch, show, rename, or rm")
	}
	action := opts.positionals[0]
	values := opts.positionals[1:]
	manager, err := a.managerFor(opts.value("db"))
	if err != nil {
		return err
	}
	switch action {
	case "list", "ls":
		result, err := manager.List(ctx, opts.flags["all"])
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, result)
	case "create":
		if len(values) == 0 {
			return errors.New("boards create requires a slug")
		}
		update := boards.Update{Name: stringPointer(opts.value("name")), Description: stringPointer(opts.value("description")), Icon: stringPointer(opts.value("icon")), Color: stringPointer(opts.value("color"))}
		if opts.present("default-workdir") {
			update.DefaultWorkdir = store.OptionalString{Set: true, Value: stringPointer(opts.value("default-workdir"))}
		}
		metadata, err := manager.Create(ctx, values[0], update)
		if err != nil {
			return err
		}
		if opts.flags["switch"] {
			metadata, err = manager.Switch(metadata.Slug)
			if err != nil {
				return err
			}
		}
		return writeJSON(a.Stdout, metadata)
	case "switch", "use":
		if len(values) == 0 {
			return errors.New("boards switch requires a slug")
		}
		metadata, err := manager.Switch(values[0])
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, metadata)
	case "show", "current":
		slug := manager.Current()
		if len(values) > 0 {
			slug = values[0]
		}
		slug, err = manager.Resolve(slug)
		if err != nil {
			return err
		}
		metadata, err := manager.Read(slug)
		if err != nil {
			return err
		}
		opened, err := manager.OpenStore(ctx, slug)
		if err != nil {
			return err
		}
		metadata.Counts, err = opened.CountTasksByStatus(ctx, slug)
		closeErr := opened.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return writeJSON(a.Stdout, metadata)
	case "rename":
		if len(values) < 2 {
			return errors.New("boards rename requires a slug and display name")
		}
		name := strings.Join(values[1:], " ")
		metadata, err := manager.Update(values[0], boards.Update{Name: &name})
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, metadata)
	case "rm", "remove":
		if len(values) == 0 {
			return errors.New("boards rm requires a slug")
		}
		result, err := manager.Remove(values[0], opts.flags["delete"])
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, result)
	default:
		return fmt.Errorf("unknown boards action: %s", action)
	}
}

func (a *App) runCreate(ctx context.Context, opts options) error {
	title := strings.TrimSpace(strings.Join(opts.positionals, " "))
	if title == "" {
		return errors.New("create requires a title")
	}
	status, err := requireStatus(opts.value("status"))
	if err != nil {
		return err
	}
	if opts.flags["triage"] {
		if status != nil && *status != model.TaskStatusTriage {
			return errors.New("--triage cannot be combined with a different --status")
		}
		value := model.TaskStatusTriage
		status = &value
	}
	runtime, err := requireRuntime(opts.value("runtime"), model.RuntimeManual)
	if err != nil {
		return err
	}
	workspaceKind, err := requireWorkspaceKind(opts.value("workspace-kind"))
	if err != nil {
		return err
	}
	priority, err := numberOption(opts.value("priority"), 0)
	if err != nil {
		return err
	}
	maxRetries, err := numberOption(opts.value("max-retries"), 2)
	if err != nil {
		return err
	}
	goalMaxTurns, err := numberOption(opts.value("goal-max-turns"), 20)
	if err != nil {
		return err
	}
	var maxRuntime *int
	if value := opts.value("max-runtime"); value != "" {
		seconds, err := durationSeconds(value)
		if err != nil {
			return err
		}
		maxRuntime = &seconds
	} else if value := opts.value("max-runtime-seconds"); value != "" {
		seconds, err := durationSeconds(value)
		if err != nil {
			return err
		}
		maxRuntime = &seconds
	}
	opened, _, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	input := store.CreateTaskInput{
		Title: title, Body: opts.value("body"), Board: board, Tenant: stringPointer(opts.value("tenant")),
		IdempotencyKey: stringPointer(opts.value("idempotency-key")), Assignee: stringPointer(opts.value("assignee")),
		Runtime: runtime, Priority: priority, Workspace: stringPointer(opts.value("workspace")), WorkspaceKind: workspaceKind,
		Branch: stringPointer(opts.value("branch")), MaxRuntimeSeconds: maxRuntime, Skills: opts.many("skill"),
		GoalMode: opts.flags["goal"], GoalMaxTurns: goalMaxTurns,
		WorkflowTemplateID: stringPointer(opts.value("workflow-template-id")), CurrentStepKey: stringPointer(opts.value("current-step-key")),
		Parents: opts.many("parent"), MaxRetries: maxRetries,
	}
	if status != nil {
		input.Status = *status
	}
	if opts.present("scheduled-at") {
		input.ScheduledAt = stringPointer(opts.value("scheduled-at"))
	}
	detail, err := opened.CreateTask(ctx, input)
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, detail)
}

func (a *App) runList(ctx context.Context, opts options) error {
	status, err := requireStatus(opts.value("status"))
	if err != nil {
		return err
	}
	runtime, err := requireRuntime(opts.value("runtime"), "")
	if opts.value("runtime") == "" {
		runtime, err = "", nil
	}
	if err != nil {
		return err
	}
	sorts := map[string]bool{"": true, "created": true, "created-desc": true, "priority": true, "priority-desc": true, "status": true, "assignee": true, "title": true, "updated": true}
	if !sorts[opts.value("sort")] {
		return fmt.Errorf("invalid sort: %s", opts.value("sort"))
	}
	assignee := opts.value("assignee")
	if opts.flags["mine"] {
		if assignee != "" {
			return errors.New("--mine and --assignee cannot be combined")
		}
		assignee = a.env("AUTOGORA_PROFILE", "AUTOGORA_WORKER_ID")
		if assignee == "" {
			return errors.New("--mine requires AUTOGORA_PROFILE or AUTOGORA_WORKER_ID")
		}
	}
	limit, err := numberOption(opts.value("limit"), 100)
	if err != nil {
		return err
	}
	opened, _, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	filter := store.ListTaskFilter{Board: board, Tenant: opts.value("tenant"), Assignee: assignee, Runtime: runtime,
		WorkflowTemplateID: opts.value("workflow-template-id"), CurrentStepKey: opts.value("current-step-key"),
		IncludeArchived: opts.flags["archived"], Search: opts.value("search"), Sort: opts.value("sort"), Limit: limit}
	if status != nil {
		filter.Status = *status
	}
	tasks, err := opened.ListTasks(ctx, filter)
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, tasks)
}

func (a *App) runReadTask(ctx context.Context, command string, opts options) error {
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
	case "graph":
		value, err := a.relationshipGraph(ctx, opened, taskID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	case "context":
		value, err := opened.BuildWorkerContext(ctx, taskID)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.Stdout, value)
		return err
	case "runs":
		detail, err := opened.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, detail.Runs)
	case "log":
		tailBytes, err := numberOption(opts.value("tail-bytes"), 64*1024)
		if err != nil {
			return err
		}
		value, err := opened.ReadRunLog(ctx, taskID, tailBytes, opts.value("run"))
		if err != nil {
			return err
		}
		if strings.HasSuffix(value.Text, "\n") {
			_, err = io.WriteString(a.Stdout, value.Text)
		} else {
			_, err = fmt.Fprintln(a.Stdout, value.Text)
		}
		return err
	default:
		detail, err := a.taskDetail(ctx, opened, taskID)
		if err != nil {
			return err
		}
		graph, err := a.relationshipGraph(ctx, opened, taskID)
		if err != nil {
			return err
		}
		workerContext, err := opened.BuildWorkerContext(ctx, taskID)
		if err != nil {
			return err
		}
		value := struct {
			model.TaskDetail
			RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
			WorkerContext     string                  `json:"workerContext"`
		}{TaskDetail: detail, RelationshipGraph: graph, WorkerContext: workerContext}
		return writeJSON(a.Stdout, value)
	}
}

func (a *App) relationshipGraph(ctx context.Context, opened *store.Store, taskID string) (model.RelationshipGraph, error) {
	if a.env("AUTOGORA_TASK_ID") != "" {
		return opened.WorkerRelationshipGraph(ctx, taskID)
	}
	return opened.RelationshipGraph(ctx, taskID)
}

func (a *App) taskDetail(ctx context.Context, opened *store.Store, taskID string) (model.TaskDetail, error) {
	if a.env("AUTOGORA_TASK_ID") != "" {
		return opened.WorkerTaskDetail(ctx, taskID)
	}
	return opened.GetTask(ctx, taskID)
}

func (a *App) runStats(ctx context.Context, command string, opts options) error {
	opened, _, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	stats, err := opened.Stats(ctx, board)
	if err != nil {
		return err
	}
	if command == "stats" {
		return writeJSON(a.Stdout, stats)
	}
	if command == "assignees" {
		return writeJSON(a.Stdout, stats.ByAssignee)
	}
	diagnostics, err := opened.Diagnose(ctx, board)
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, diagnostics)
}

func (a *App) runEvents(ctx context.Context, command string, opts options) error {
	taskID := ""
	if command == "tail" {
		if len(opts.positionals) == 0 {
			return errors.New("tail requires a task id")
		}
		taskID = opts.positionals[0]
	}
	cursor, err := parseSince(opts.value("since"))
	if err != nil {
		return err
	}
	limit, err := numberOption(opts.value("limit"), 500)
	if err != nil {
		return err
	}
	interval, err := numberOption(opts.value("interval-ms"), 1000)
	if err != nil {
		return err
	}
	interval = max(100, interval)
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	for {
		events, err := opened.ListEvents(ctx, store.EventFilter{TaskID: taskID, SinceID: cursor, Kinds: parseKinds(opts.value("kinds")), Limit: limit})
		if err != nil {
			return err
		}
		for _, event := range events {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(a.Stdout, "%s\n", encoded); err != nil {
				return err
			}
			*cursor = max(*cursor, event.ID)
		}
		if !opts.flags["follow"] {
			return nil
		}
		timer := time.NewTimer(time.Duration(interval) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (a *App) runBulk(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("bulk requires at least one task id")
	}
	if !opts.present("status") && !opts.present("assignee") && !opts.present("priority") && !opts.flags["archive"] && !opts.flags["delete"] {
		return errors.New("bulk requires --status, --assignee, --priority, --archive, or --delete")
	}
	status, err := requireStatus(opts.value("status"))
	if err != nil {
		return err
	}
	priority := (*int)(nil)
	if opts.present("priority") {
		value, err := numberOption(opts.value("priority"), 0)
		if err != nil {
			return err
		}
		priority = &value
	}
	assignee := store.OptionalString{}
	if opts.present("assignee") {
		assignee.Set = true
		if value := opts.value("assignee"); value != "none" {
			assignee.Value = &value
		}
	}
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	result := opened.BulkMutate(ctx, opts.positionals, store.BulkMutation{Status: status, Assignee: assignee, Priority: priority, Archive: opts.flags["archive"], Delete: opts.flags["delete"]})
	return writeJSON(a.Stdout, result)
}
