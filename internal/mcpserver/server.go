package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

const instructions = "Use TaskCircuit as the canonical task state. Workers must read their task first and heartbeat during long work. Ordinary workers terminate exactly once with taskcircuit_complete or taskcircuit_block; goal-mode workers may leave a non-terminal progress handoff so the dispatcher can judge and resume the session. Orchestrators route work but do not implement it."

type Service struct {
	manager *boards.Manager
	getenv  func(string) string
}

func New(manager *boards.Manager, version string) (*mcp.Server, *Service) {
	service := &Service{manager: manager, getenv: os.Getenv}
	server := mcp.NewServer(&mcp.Implementation{Name: "taskcircuit", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	service.registerCore(server)
	service.registerMutations(server)
	return server, service
}

func RunStdio(ctx context.Context, manager *boards.Manager, version string) error {
	server, _ := New(manager, version)
	return server.Run(ctx, &mcp.StdioTransport{})
}

func addTool[Input any](server *mcp.Server, name, title, description string, readOnly, destructive, idempotent, openWorld bool, handler func(context.Context, Input) (any, error)) {
	mcp.AddTool[Input, any](server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		Title: title, ReadOnlyHint: readOnly, DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &openWorld,
	}}, func(ctx context.Context, _ *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, any, error) {
		value, err := handler(ctx, input)
		return nil, value, err
	})
}

func (s *Service) env(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(s.getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func (s *Service) requireAdmin() error {
	if s.env("TASKCIRCUIT_TASK_ID") != "" {
		return errors.New("dispatcher-scoped workers cannot plan, route, claim, or unblock board tasks")
	}
	return nil
}

func (s *Service) scopedTaskID(requested string) (string, error) {
	pinned := s.env("TASKCIRCUIT_TASK_ID")
	if pinned != "" && requested != "" && pinned != requested {
		return "", errors.New("this worker is scoped to a different task")
	}
	if pinned != "" {
		return pinned, nil
	}
	if requested == "" {
		return "", errors.New("task_id is required outside a dispatcher-scoped worker")
	}
	return requested, nil
}

func (s *Service) scopedRun(runID, claimToken string) (store.RunScope, error) {
	pinnedRun := s.env("TASKCIRCUIT_RUN_ID")
	pinnedToken := s.env("TASKCIRCUIT_CLAIM_TOKEN")
	if pinnedRun != "" && runID != "" && pinnedRun != runID {
		return store.RunScope{}, errors.New("this worker is scoped to a different run")
	}
	if pinnedToken != "" && claimToken != "" && pinnedToken != claimToken {
		return store.RunScope{}, errors.New("claim token mismatch")
	}
	if pinnedRun != "" {
		runID = pinnedRun
	}
	if pinnedToken != "" {
		claimToken = pinnedToken
	}
	if runID == "" || claimToken == "" {
		return store.RunScope{}, errors.New("run_id and claim_token are required outside a dispatcher-scoped worker")
	}
	return store.RunScope{RunID: runID, ClaimToken: claimToken}, nil
}

func (s *Service) selectedBoard(requested string) (string, error) {
	pinned := s.env("TASKCIRCUIT_BOARD")
	if pinned != "" && requested != "" && strings.ToLower(strings.TrimSpace(requested)) != strings.ToLower(pinned) {
		return "", errors.New("this worker is scoped to a different board")
	}
	if pinned != "" {
		requested = pinned
	}
	return s.manager.Resolve(requested)
}

func usingStore[T any](ctx context.Context, service *Service, requested string, handler func(*store.Store, string) (T, error)) (T, error) {
	var zero T
	board, err := service.selectedBoard(requested)
	if err != nil {
		return zero, err
	}
	opened, err := service.manager.OpenStore(ctx, board)
	if err != nil {
		return zero, err
	}
	defer opened.Close()
	return handler(opened, board)
}

type boardInput struct {
	Board string `json:"board,omitempty" jsonschema:"board slug; omit to use the current board"`
}

type taskInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type boardsListInput struct {
	IncludeArchived bool `json:"include_archived,omitempty"`
}

type boardCreateInput struct {
	Slug              string                   `json:"slug"`
	Name              string                   `json:"name,omitempty"`
	Description       string                   `json:"description,omitempty"`
	Icon              string                   `json:"icon,omitempty"`
	Color             string                   `json:"color,omitempty"`
	DefaultWorkdir    *string                  `json:"default_workdir,omitempty"`
	Orchestration     *boardOrchestrationInput `json:"orchestration,omitempty"`
	Switch            bool                     `json:"switch,omitempty"`
	defaultWorkdirSet bool
}

type boardUpdateInput struct {
	Slug              string                   `json:"slug"`
	Name              *string                  `json:"name,omitempty"`
	Description       *string                  `json:"description,omitempty"`
	Icon              *string                  `json:"icon,omitempty"`
	Color             *string                  `json:"color,omitempty"`
	DefaultWorkdir    *string                  `json:"default_workdir,omitempty"`
	Orchestration     *boardOrchestrationInput `json:"orchestration,omitempty"`
	defaultWorkdirSet bool
}

type boardOrchestrationInput struct {
	AutoDecompose          *bool           `json:"autoDecompose,omitempty"`
	AutoDecomposePerTick   *int            `json:"autoDecomposePerTick,omitempty"`
	AutoPromoteChildren    *bool           `json:"autoPromoteChildren,omitempty"`
	PlannerRuntime         *model.Runtime  `json:"plannerRuntime,omitempty"`
	DefaultProfile         *string         `json:"defaultProfile,omitempty"`
	OrchestratorProfile    *string         `json:"orchestratorProfile,omitempty"`
	Profiles               *[]profileRoute `json:"profiles,omitempty"`
	defaultProfileSet      bool
	orchestratorProfileSet bool
}

func presence(raw []byte, names ...string) map[string]bool {
	values := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &values)
	result := map[string]bool{}
	for _, name := range names {
		_, result[name] = values[name]
	}
	return result
}

func (input *boardCreateInput) UnmarshalJSON(raw []byte) error {
	type plain boardCreateInput
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*input = boardCreateInput(value)
	input.defaultWorkdirSet = presence(raw, "default_workdir")["default_workdir"]
	return nil
}

func (input *boardUpdateInput) UnmarshalJSON(raw []byte) error {
	type plain boardUpdateInput
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*input = boardUpdateInput(value)
	input.defaultWorkdirSet = presence(raw, "default_workdir")["default_workdir"]
	return nil
}

func (input *boardOrchestrationInput) UnmarshalJSON(raw []byte) error {
	type plain boardOrchestrationInput
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*input = boardOrchestrationInput(value)
	found := presence(raw, "defaultProfile", "orchestratorProfile")
	input.defaultProfileSet, input.orchestratorProfileSet = found["defaultProfile"], found["orchestratorProfile"]
	return nil
}

type boardSlugInput struct {
	Slug string `json:"slug"`
}

type boardRemoveInput struct {
	Slug       string `json:"slug"`
	HardDelete bool   `json:"hard_delete,omitempty"`
}

type createInput struct {
	Title              string              `json:"title"`
	Body               string              `json:"body,omitempty"`
	Board              string              `json:"board,omitempty"`
	Tenant             *string             `json:"tenant,omitempty"`
	IdempotencyKey     *string             `json:"idempotency_key,omitempty"`
	Assignee           *string             `json:"assignee,omitempty"`
	Runtime            model.Runtime       `json:"runtime,omitempty"`
	Priority           int                 `json:"priority,omitempty"`
	Workspace          *string             `json:"workspace,omitempty"`
	WorkspaceKind      model.WorkspaceKind `json:"workspace_kind,omitempty"`
	Branch             *string             `json:"branch,omitempty"`
	Status             model.TaskStatus    `json:"status,omitempty"`
	ScheduledAt        *string             `json:"scheduled_at,omitempty"`
	MaxRuntimeSeconds  *int                `json:"max_runtime_seconds,omitempty"`
	Skills             []string            `json:"skills,omitempty"`
	GoalMode           bool                `json:"goal_mode,omitempty"`
	GoalMaxTurns       int                 `json:"goal_max_turns,omitempty"`
	WorkflowTemplateID *string             `json:"workflow_template_id,omitempty"`
	CurrentStepKey     *string             `json:"current_step_key,omitempty"`
	MaxRetries         int                 `json:"max_retries,omitempty"`
	Parents            []string            `json:"parents,omitempty"`
}

type listInput struct {
	Board              string           `json:"board,omitempty"`
	Status             model.TaskStatus `json:"status,omitempty"`
	Tenant             string           `json:"tenant,omitempty"`
	Assignee           string           `json:"assignee,omitempty"`
	Runtime            model.Runtime    `json:"runtime,omitempty"`
	WorkflowTemplateID string           `json:"workflow_template_id,omitempty"`
	CurrentStepKey     string           `json:"current_step_key,omitempty"`
	IncludeArchived    bool             `json:"include_archived,omitempty"`
	Search             string           `json:"search,omitempty"`
	Sort               string           `json:"sort,omitempty"`
	Limit              int              `json:"limit,omitempty"`
}

type eventInput struct {
	Board   string   `json:"board,omitempty"`
	TaskID  string   `json:"task_id,omitempty"`
	SinceID *int64   `json:"since_id,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

type logInput struct {
	Board     string `json:"board,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	TailBytes int    `json:"tail_bytes,omitempty"`
}

func (s *Service) registerCore(server *mcp.Server) {
	addTool(server, "taskcircuit_boards_list", "List Kanban boards", "List isolated boards with metadata, paths, and per-status task counts.", true, false, true, false, func(ctx context.Context, input boardsListInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return s.manager.List(ctx, input.IncludeArchived)
	})
	addTool(server, "taskcircuit_boards_create", "Create Kanban board", "Create an isolated board with its own database, workspaces, attachments, and logs.", false, false, true, false, func(ctx context.Context, input boardCreateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		update := boards.Update{Name: pointerIfSet(input.Name), Description: pointerIfSet(input.Description), Icon: pointerIfSet(input.Icon), Color: pointerIfSet(input.Color)}
		if input.defaultWorkdirSet {
			update.DefaultWorkdir = store.OptionalString{Set: true, Value: input.DefaultWorkdir}
		}
		var err error
		update.Orchestration, err = orchestrationBoardUpdate(input.Orchestration)
		if err != nil {
			return nil, err
		}
		metadata, err := s.manager.Create(ctx, input.Slug, update)
		if err == nil && input.Switch {
			metadata, err = s.manager.Switch(metadata.Slug)
		}
		return metadata, err
	})
	addTool(server, "taskcircuit_boards_update", "Update Kanban board", "Update board presentation metadata and its default project directory.", false, false, true, false, func(_ context.Context, input boardUpdateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		update := boards.Update{Name: input.Name, Description: input.Description, Icon: input.Icon, Color: input.Color}
		if input.defaultWorkdirSet {
			update.DefaultWorkdir = store.OptionalString{Set: true, Value: input.DefaultWorkdir}
		}
		var err error
		update.Orchestration, err = orchestrationBoardUpdate(input.Orchestration)
		if err != nil {
			return nil, err
		}
		return s.manager.Update(input.Slug, update)
	})
	addTool(server, "taskcircuit_boards_switch", "Switch current Kanban board", "Persist the current board used when an explicit board is omitted.", false, false, true, false, func(_ context.Context, input boardSlugInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return s.manager.Switch(input.Slug)
	})
	addTool(server, "taskcircuit_boards_remove", "Remove Kanban board", "Archive a named board by default, or permanently delete it when hard_delete is true.", false, true, false, false, func(_ context.Context, input boardRemoveInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return s.manager.Remove(input.Slug, input.HardDelete)
	})
	addTool(server, "taskcircuit_create", "Create Kanban task", "Create a durable task assigned to Claude, Codex, Cline, Gemini, or a human.", false, false, false, false, func(ctx context.Context, input createInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if input.Runtime == "" {
			input.Runtime = model.RuntimeManual
		}
		if input.GoalMaxTurns == 0 {
			input.GoalMaxTurns = 20
		}
		if input.MaxRetries == 0 {
			input.MaxRetries = 2
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) {
			return opened.CreateTask(ctx, store.CreateTaskInput{Title: input.Title, Body: input.Body, Board: board, Tenant: input.Tenant,
				IdempotencyKey: input.IdempotencyKey, Assignee: input.Assignee, Runtime: input.Runtime, Priority: input.Priority,
				Workspace: input.Workspace, WorkspaceKind: input.WorkspaceKind, Branch: input.Branch, Status: input.Status,
				ScheduledAt: input.ScheduledAt, MaxRuntimeSeconds: input.MaxRuntimeSeconds, Skills: input.Skills,
				GoalMode: input.GoalMode, GoalMaxTurns: input.GoalMaxTurns, WorkflowTemplateID: input.WorkflowTemplateID,
				CurrentStepKey: input.CurrentStepKey, MaxRetries: input.MaxRetries, Parents: input.Parents})
		})
	})
	addTool(server, "taskcircuit_list", "List Kanban tasks", "List board tasks with optional status, assignee, runtime, and search filters.", true, false, true, false, func(ctx context.Context, input listInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if input.Limit == 0 {
			input.Limit = 100
		}
		if input.Sort == "" {
			input.Sort = "priority-desc"
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) {
			return opened.ListTasks(ctx, store.ListTaskFilter{Board: board, Status: input.Status, Tenant: input.Tenant,
				Assignee: input.Assignee, Runtime: input.Runtime, WorkflowTemplateID: input.WorkflowTemplateID,
				CurrentStepKey: input.CurrentStepKey, IncludeArchived: input.IncludeArchived, Search: input.Search, Sort: input.Sort, Limit: input.Limit})
		})
	})
	addTool(server, "taskcircuit_show", "Show Kanban task", "Read a task with dependencies, comments, run history, relationship graph, and bounded worker context.", true, false, true, false, func(ctx context.Context, input taskInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			detail, err := opened.GetTask(ctx, taskID)
			if err != nil {
				return nil, err
			}
			graph, err := opened.RelationshipGraph(ctx, taskID)
			if err != nil {
				return nil, err
			}
			workerContext, err := opened.BuildWorkerContext(ctx, taskID)
			if err != nil {
				return nil, err
			}
			return struct {
				model.TaskDetail
				RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
				WorkerContext     string                  `json:"workerContext"`
			}{TaskDetail: detail, RelationshipGraph: graph, WorkerContext: workerContext}, nil
		})
	})
	addTool(server, "taskcircuit_context", "Build Kanban worker context", "Return the bounded task body, execution order, handoffs, attachments, prior attempts, and comments.", true, false, true, false, func(ctx context.Context, input taskInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) { return opened.BuildWorkerContext(ctx, taskID) })
	})
	addTool(server, "taskcircuit_graph", "Show TaskCircuit relationship graph", "Show a bounded hierarchy and dependency DAG with execution phases.", true, false, true, false, func(ctx context.Context, input taskInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) { return opened.RelationshipGraph(ctx, taskID) })
	})
	addTool(server, "taskcircuit_stats", "Get Kanban statistics", "Count board tasks by status, assignee, runtime, and tenant.", true, false, true, false, func(ctx context.Context, input boardInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) { return opened.Stats(ctx, board) })
	})
	addTool(server, "taskcircuit_diagnostics", "Diagnose Kanban board", "Inspect task/run invariants, queue stalls, and active workers.", true, false, true, false, func(ctx context.Context, input boardInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) { return opened.Diagnose(ctx, board) })
	})
	addTool(server, "taskcircuit_events", "Read Kanban events", "Read the append-only board event stream by cursor, task, and kind.", true, false, true, false, func(ctx context.Context, input eventInput) (any, error) {
		if s.env("TASKCIRCUIT_TASK_ID") != "" {
			var err error
			input.TaskID, err = s.scopedTaskID(input.TaskID)
			if err != nil {
				return nil, err
			}
		}
		if input.Limit == 0 {
			input.Limit = 500
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ListEvents(ctx, store.EventFilter{TaskID: input.TaskID, SinceID: input.SinceID, Kinds: input.Kinds, Limit: input.Limit})
		})
	})
	addTool(server, "taskcircuit_runs", "List Kanban runs", "Read full attempt history for one task.", true, false, true, false, func(ctx context.Context, input taskInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			detail, err := opened.GetTask(ctx, taskID)
			return detail.Runs, err
		})
	})
	addTool(server, "taskcircuit_log", "Read Kanban worker log", "Read up to 1 MB from the tail of a task run log.", true, false, true, false, func(ctx context.Context, input logInput) (any, error) {
		taskID, err := s.scopedTaskID(input.TaskID)
		if err != nil {
			return nil, err
		}
		if input.TailBytes == 0 {
			input.TailBytes = 64 * 1024
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ReadRunLog(ctx, taskID, input.TailBytes, input.RunID)
		})
	})
}

func pointerIfSet(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func orchestrationBoardUpdate(input *boardOrchestrationInput) (*boards.OrchestrationUpdate, error) {
	if input == nil {
		return nil, nil
	}
	if input.AutoDecomposePerTick != nil && (*input.AutoDecomposePerTick < 1 || *input.AutoDecomposePerTick > 100) {
		return nil, errors.New("autoDecomposePerTick must be between 1 and 100")
	}
	if input.PlannerRuntime != nil && !validPlannerRuntime(*input.PlannerRuntime) {
		return nil, fmt.Errorf("invalid planner runtime: %s", *input.PlannerRuntime)
	}
	update := &boards.OrchestrationUpdate{
		AutoDecompose: input.AutoDecompose, AutoDecomposePerTick: input.AutoDecomposePerTick,
		AutoPromoteChildren: input.AutoPromoteChildren, PlannerRuntime: input.PlannerRuntime,
	}
	if input.defaultProfileSet {
		update.DefaultProfile = store.OptionalString{Set: true, Value: input.DefaultProfile}
	}
	if input.orchestratorProfileSet {
		update.OrchestratorProfile = store.OptionalString{Set: true, Value: input.OrchestratorProfile}
	}
	if input.Profiles != nil {
		if len(*input.Profiles) > 200 {
			return nil, errors.New("profiles cannot exceed 200 entries")
		}
		profiles := make([]boards.Profile, 0, len(*input.Profiles))
		for _, profile := range *input.Profiles {
			profile.Name = strings.TrimSpace(profile.Name)
			if profile.Name == "" || !validPlannerRuntime(profile.Runtime) {
				return nil, errors.New("profile requires a name and worker runtime")
			}
			profiles = append(profiles, boards.Profile{Name: profile.Name, Runtime: profile.Runtime, Description: profile.Description})
		}
		update.Profiles = &profiles
	}
	return update, nil
}

func (s *Service) String() string { return fmt.Sprintf("TaskCircuit MCP (%s)", s.manager.Current()) }
