package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/maintenance"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/notifications"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
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
	secretSet  bool
}

type notificationListInput struct {
	Board  string `json:"board,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

type specifyInput struct {
	Board            string        `json:"board,omitempty"`
	TaskID           string        `json:"task_id"`
	Title            *string       `json:"title,omitempty"`
	Body             *string       `json:"body,omitempty"`
	Author           string        `json:"author,omitempty"`
	PlannerRuntime   model.Runtime `json:"planner_runtime,omitempty"`
	PlannerModel     string        `json:"planner_model,omitempty"`
	PlannerProvider  string        `json:"planner_provider,omitempty"`
	PlannerTimeoutMS int           `json:"planner_timeout_ms,omitempty"`
}

type profileRoute struct {
	Name          string        `json:"name"`
	Runtime       model.Runtime `json:"runtime"`
	Model         string        `json:"model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Description   string        `json:"description,omitempty"`
	Disabled      bool          `json:"disabled,omitempty"`
	MaxConcurrent int           `json:"maxConcurrent,omitempty"`
	Priority      int           `json:"priority,omitempty"`
	Fallbacks     []string      `json:"fallbacks,omitempty"`
}

type decomposeInput struct {
	Board               string                           `json:"board,omitempty"`
	TaskID              string                           `json:"task_id"`
	Profiles            []orchestration.ProfileRoute     `json:"profiles,omitempty"`
	DefaultProfile      *orchestration.ProfileRoute      `json:"default_profile,omitempty"`
	OrchestratorProfile *orchestration.ProfileRoute      `json:"orchestrator_profile,omitempty"`
	AutoPromoteChildren *bool                            `json:"auto_promote_children,omitempty"`
	Plan                *orchestration.DecompositionPlan `json:"plan,omitempty"`
	PlannerRuntime      model.Runtime                    `json:"planner_runtime,omitempty"`
	PlannerModel        string                           `json:"planner_model,omitempty"`
	PlannerProvider     string                           `json:"planner_provider,omitempty"`
	PlannerTimeoutMS    int                              `json:"planner_timeout_ms,omitempty"`
}

type profileDescribeInput struct {
	Board            string        `json:"board,omitempty"`
	Name             string        `json:"name"`
	Runtime          model.Runtime `json:"runtime"`
	PlannerRuntime   model.Runtime `json:"planner_runtime,omitempty"`
	PlannerModel     string        `json:"planner_model,omitempty"`
	PlannerProvider  string        `json:"planner_provider,omitempty"`
	PlannerTimeoutMS int           `json:"planner_timeout_ms,omitempty"`
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
	Board                                           string               `json:"board,omitempty"`
	TaskID                                          string               `json:"task_id"`
	Title                                           *string              `json:"title,omitempty"`
	Body                                            *string              `json:"body,omitempty"`
	Tenant                                          *string              `json:"tenant,omitempty"`
	Assignee                                        *string              `json:"assignee,omitempty"`
	Runtime                                         *model.Runtime       `json:"runtime,omitempty"`
	Priority                                        *int                 `json:"priority,omitempty"`
	Workspace                                       *string              `json:"workspace,omitempty"`
	WorkspaceKind                                   *model.WorkspaceKind `json:"workspace_kind,omitempty"`
	Branch                                          *string              `json:"branch,omitempty"`
	ScheduledAt                                     *string              `json:"scheduled_at,omitempty"`
	MaxRuntimeSeconds                               *int                 `json:"max_runtime_seconds,omitempty"`
	Skills                                          *[]string            `json:"skills,omitempty"`
	GoalMode                                        *bool                `json:"goal_mode,omitempty"`
	GoalMaxTurns                                    *int                 `json:"goal_max_turns,omitempty"`
	WorkflowTemplateID                              *string              `json:"workflow_template_id,omitempty"`
	CurrentStepKey                                  *string              `json:"current_step_key,omitempty"`
	Status                                          *model.TaskStatus    `json:"status,omitempty"`
	tenantSet, assigneeSet, workspaceSet, branchSet bool
	scheduledAtSet, maxRuntimeSecondsSet            bool
	workflowTemplateIDSet, currentStepKeySet        bool
}

func (input *notificationInput) UnmarshalJSON(raw []byte) error {
	type plain notificationInput
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*input = notificationInput(value)
	input.secretSet = presence(raw, "secret")["secret"]
	return nil
}

func (input *updateInput) UnmarshalJSON(raw []byte) error {
	type plain updateInput
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*input = updateInput(value)
	found := presence(raw, "tenant", "assignee", "workspace", "branch", "scheduled_at", "max_runtime_seconds", "workflow_template_id", "current_step_key")
	input.tenantSet, input.assigneeSet = found["tenant"], found["assignee"]
	input.workspaceSet, input.branchSet = found["workspace"], found["branch"]
	input.scheduledAtSet, input.maxRuntimeSecondsSet = found["scheduled_at"], found["max_runtime_seconds"]
	input.workflowTemplateIDSet, input.currentStepKeySet = found["workflow_template_id"], found["current_step_key"]
	return nil
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

type notificationDeliveryInput struct {
	Board     string `json:"board,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
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

func optionalStringSet(value *string, set bool) store.OptionalString {
	return store.OptionalString{Set: set, Value: value}
}
func optionalIntSet(value *int, set bool) store.OptionalInt {
	return store.OptionalInt{Set: set, Value: value}
}

func validPlannerRuntime(runtime model.Runtime) bool {
	return runtime == model.RuntimeClaude || runtime == model.RuntimeCodex || runtime == model.RuntimeCline || runtime == model.RuntimeGemini
}

func resolvePlannerSettings(metadata boards.Metadata, runtime model.Runtime, modelName, provider string) (model.Runtime, string, string) {
	explicitRuntime := strings.TrimSpace(string(runtime)) != ""
	if !explicitRuntime {
		runtime = metadata.Orchestration.PlannerRuntime
	}
	modelName, provider = strings.TrimSpace(modelName), strings.TrimSpace(provider)
	if !explicitRuntime {
		if modelName == "" {
			modelName = metadata.Orchestration.PlannerModel
		}
		if provider == "" {
			provider = metadata.Orchestration.PlannerProvider
		}
	}
	return runtime, modelName, provider
}

func supportsAgentRole(agent agentconfig.Agent, role agentconfig.Role) bool {
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

func defaultPlannerAgent(config agentconfig.Config) (agentconfig.Agent, bool) {
	for _, id := range config.Defaults.PlannerAgents {
		agent, found := config.Find(id)
		if found && agent.Enabled && supportsAgentRole(agent, agentconfig.RolePlanner) {
			return agent, true
		}
	}
	return agentconfig.Agent{}, false
}

func (s *Service) registeredPlannerSettings(metadata boards.Metadata, runtime model.Runtime, modelName, provider string) (model.Runtime, string, string, string, error) {
	resolvedRuntime, resolvedModel, resolvedProvider := resolvePlannerSettings(metadata, runtime, modelName, provider)
	// An explicit runtime is an ad-hoc request and must not inherit a registered
	// command or model. A board model/provider pin likewise remains authoritative.
	// Only an unpinned board delegates its planner choice to the global order.
	if strings.TrimSpace(string(runtime)) != "" {
		return resolvedRuntime, "", resolvedModel, resolvedProvider, nil
	}
	if strings.TrimSpace(metadata.Orchestration.PlannerModel) != "" || strings.TrimSpace(metadata.Orchestration.PlannerProvider) != "" {
		return resolvedRuntime, "", resolvedModel, resolvedProvider, nil
	}
	config, err := agentconfig.Load(agentconfig.Options{Getenv: s.getenv})
	if err != nil {
		return "", "", "", "", fmt.Errorf("load global agent configuration: %w", err)
	}
	agent, found := defaultPlannerAgent(config)
	if !found {
		return resolvedRuntime, "", resolvedModel, resolvedProvider, nil
	}
	resolvedRuntime, resolvedModel, resolvedProvider = agent.Runtime, agent.Model, agent.Provider
	if value := strings.TrimSpace(modelName); value != "" {
		resolvedModel = value
	}
	if value := strings.TrimSpace(provider); value != "" {
		resolvedProvider = value
	}
	return resolvedRuntime, agent.Command, resolvedModel, resolvedProvider, nil
}

func boardProfileRoutes(profiles []boards.Profile) []orchestration.ProfileRoute {
	routes := make([]orchestration.ProfileRoute, 0, len(profiles))
	for _, profile := range profiles {
		routes = append(routes, orchestration.ProfileRoute{
			Name: profile.Name, Runtime: profile.Runtime, Model: profile.Model, Provider: profile.Provider,
			Description: profile.Description, Disabled: profile.Disabled, MaxConcurrent: profile.MaxConcurrent,
			Priority: profile.Priority, Fallbacks: append([]string{}, profile.Fallbacks...),
		})
	}
	return routes
}

func decompositionProfiles(ctx context.Context, opened *store.Store, metadata boards.Metadata, requested []orchestration.ProfileRoute) ([]orchestration.ProfileRoute, error) {
	tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
	if err != nil {
		return nil, err
	}
	configured := append(boardProfileRoutes(metadata.Orchestration.Profiles), requested...)
	return orchestration.ResolveProfileRoutes(tasks, configured), nil
}

func (s *Service) planner(runtime model.Runtime, command, modelName, provider string, timeoutMS int) (orchestration.Planner, error) {
	if runtime == "" {
		runtime = model.RuntimeCodex
	}
	if !validPlannerRuntime(runtime) {
		return nil, fmt.Errorf("invalid planner runtime: %s", runtime)
	}
	if timeoutMS == 0 {
		timeoutMS = 120_000
	}
	if timeoutMS < 1_000 || timeoutMS > 600_000 {
		return nil, errors.New("planner_timeout_ms must be between 1000 and 600000")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: runtime, Command: strings.TrimSpace(command), Model: strings.TrimSpace(modelName),
		Provider: strings.TrimSpace(provider), CWD: cwd, Timeout: time.Duration(timeoutMS) * time.Millisecond, Getenv: s.getenv})
}

func (s *Service) registerMutations(server *mcp.Server) {
	addTool(server, "autogora_notify_deliver", "Deliver pending Kanban notifications", "Claim and deliver pending terminal events through registered adapters.", false, false, false, true, func(ctx context.Context, input notificationDeliveryInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if input.Limit == 0 {
			input.Limit = 25
		}
		if input.TimeoutMS == 0 {
			input.TimeoutMS = 10_000
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return notifications.Deliver(ctx, opened, notifications.Options{Limit: input.Limit, Timeout: time.Duration(input.TimeoutMS) * time.Millisecond})
		})
	})
	addTool(server, "autogora_gc", "Garbage collect Kanban data", "Delete expired events, worker logs, and verified terminal scratch workspaces.", false, true, true, false, func(ctx context.Context, input garbageCollectionInput) (any, error) {
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
	addTool(server, "autogora_run_terminate", "Terminate an Autogora worker run", "Persist termination intent, signal a live worker, and reclaim a missing process.", false, true, false, false, func(ctx context.Context, input terminateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if (input.TaskID == "") == (input.RunID == "") {
			return nil, errors.New("provide exactly one of task_id or run_id")
		}
		if input.Reason == "" {
			input.Reason = "Run terminated through Autogora MCP"
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			if input.RunID != "" {
				return runcontrol.TerminateRun(ctx, opened, input.RunID, input.Reason)
			}
			return runcontrol.TerminateTaskRun(ctx, opened, input.TaskID, input.Reason)
		})
	})
	addTool(server, "autogora_bulk", "Bulk mutate Kanban tasks", "Apply one mutation with per-task success and error results.", false, true, false, false, func(ctx context.Context, input bulkInput) (any, error) {
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
	addTool(server, "autogora_notify_subscribe", "Subscribe to Kanban task notifications", "Subscribe a platform destination to future task events.", false, false, true, false, func(ctx context.Context, input notificationInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.SubscribeTask(ctx, store.SubscriptionInput{TaskID: input.TaskID, Platform: input.Platform, ChatID: input.ChatID,
				ThreadID: input.ThreadID, UserID: input.UserID, EventKinds: input.EventKinds, Secret: optionalStringSet(input.Secret, input.secretSet)})
		})
	})
	addTool(server, "autogora_notify_list", "List Kanban notification subscriptions", "List subscriptions without exposing stored secrets.", true, false, true, false, func(ctx context.Context, input notificationListInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ListNotificationSubscriptions(ctx, input.TaskID)
		})
	})
	addTool(server, "autogora_notify_unsubscribe", "Unsubscribe from Kanban task notifications", "Remove a task notification destination and pending deliveries.", false, true, true, false, func(ctx context.Context, input notificationInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		removed, err := usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (bool, error) {
			return opened.UnsubscribeTask(ctx, input.TaskID, input.Platform, input.ChatID, input.ThreadID)
		})
		return map[string]any{"taskId": input.TaskID, "unsubscribed": removed}, err
	})
	addTool(server, "autogora_specify", "Specify a Kanban triage task", "Rewrite a rough triage card into an executable specification with explicit input or a constrained CLI planner.", false, false, false, true, func(ctx context.Context, input specifyInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		if (input.Title == nil) != (input.Body == nil) {
			return nil, errors.New("title and body must be provided together")
		}
		var explicit *orchestration.SpecificationPlan
		var planner orchestration.Planner
		if input.Title != nil {
			explicit = &orchestration.SpecificationPlan{Title: *input.Title, Body: *input.Body}
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) {
			if explicit == nil {
				metadata, err := s.manager.Read(board)
				if err != nil {
					return nil, err
				}
				runtime, command, modelName, provider, err := s.registeredPlannerSettings(metadata, input.PlannerRuntime, input.PlannerModel, input.PlannerProvider)
				if err != nil {
					return nil, err
				}
				planner, err = s.planner(runtime, command, modelName, provider, input.PlannerTimeoutMS)
				if err != nil {
					return nil, err
				}
			}
			return orchestration.SpecifyTriageTask(ctx, opened, input.TaskID, planner, explicit, input.Author)
		})
	})
	addTool(server, "autogora_decompose", "Decompose a Kanban triage task", "Use an explicit or constrained CLI planner-generated plan to atomically create and route an acyclic child task graph.", false, false, false, true, func(ctx context.Context, input decomposeInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, board string) (any, error) {
			metadata, err := s.manager.Read(board)
			if err != nil {
				return nil, err
			}
			plannerRuntime, plannerModel, plannerProvider := resolvePlannerSettings(metadata, input.PlannerRuntime, input.PlannerModel, input.PlannerProvider)
			var planner orchestration.Planner
			if input.Plan == nil {
				var plannerCommand string
				plannerRuntime, plannerCommand, plannerModel, plannerProvider, err = s.registeredPlannerSettings(metadata, input.PlannerRuntime, input.PlannerModel, input.PlannerProvider)
				if err != nil {
					return nil, err
				}
				planner, err = s.planner(plannerRuntime, plannerCommand, plannerModel, plannerProvider, input.PlannerTimeoutMS)
				if err != nil {
					return nil, err
				}
			}
			profiles, err := decompositionProfiles(ctx, opened, metadata, input.Profiles)
			if err != nil {
				return nil, err
			}
			defaultProfile, orchestratorProfile := orchestration.SelectProfileRoutes(
				profiles, metadata.Orchestration.DefaultProfile, metadata.Orchestration.OrchestratorProfile, plannerRuntime,
			)
			if input.DefaultProfile != nil {
				if !orchestration.RunnableProfileRoute(*input.DefaultProfile) {
					return nil, errors.New("default_profile requires an enabled worker profile")
				}
				defaultProfile = *input.DefaultProfile
				if input.OrchestratorProfile == nil {
					orchestratorProfile = defaultProfile
				}
			}
			if input.OrchestratorProfile != nil {
				if !orchestration.RunnableProfileRoute(*input.OrchestratorProfile) {
					return nil, errors.New("orchestrator_profile requires an enabled worker profile")
				}
				orchestratorProfile = *input.OrchestratorProfile
			}
			autoPromote := input.AutoPromoteChildren
			if autoPromote == nil {
				value := metadata.Orchestration.AutoPromoteChildren
				autoPromote = &value
			}
			return orchestration.DecomposeTriageTask(ctx, opened, input.TaskID, orchestration.DecomposeOptions{
				Profiles: profiles, DefaultProfile: defaultProfile, OrchestratorProfile: &orchestratorProfile,
				AutoPromoteChildren: autoPromote, Planner: planner, Plan: input.Plan,
			})
		})
	})
	addTool(server, "autogora_profile_describe_auto", "Describe a Kanban routing profile", "Generate and persist a concise board routing description from prior task evidence.", false, false, false, true, func(ctx context.Context, input profileDescribeInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" || !validPlannerRuntime(input.Runtime) {
			return nil, errors.New("name and a worker runtime are required")
		}
		board, err := s.selectedBoard(input.Board)
		if err != nil {
			return nil, err
		}
		metadata, err := s.manager.Read(board)
		if err != nil {
			return nil, err
		}
		plannerRuntime := input.PlannerRuntime
		if plannerRuntime == "" {
			plannerRuntime = metadata.Orchestration.PlannerRuntime
		}
		plannerModel, plannerProvider := input.PlannerModel, input.PlannerProvider
		if plannerModel == "" && plannerRuntime == metadata.Orchestration.PlannerRuntime {
			plannerModel, plannerProvider = metadata.Orchestration.PlannerModel, metadata.Orchestration.PlannerProvider
		}
		planner, err := s.planner(plannerRuntime, "", plannerModel, plannerProvider, input.PlannerTimeoutMS)
		if err != nil {
			return nil, err
		}
		opened, err := s.manager.OpenStore(ctx, board)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		existing := orchestration.ProfileRoute{Name: input.Name, Runtime: input.Runtime}
		for _, profile := range metadata.Orchestration.Profiles {
			if profile.Name == input.Name {
				existing = orchestration.ProfileRoute{Name: profile.Name, Runtime: profile.Runtime, Model: profile.Model,
					Provider: profile.Provider, Description: profile.Description, Disabled: profile.Disabled,
					MaxConcurrent: profile.MaxConcurrent, Priority: profile.Priority, Fallbacks: append([]string{}, profile.Fallbacks...)}
				break
			}
		}
		tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{Assignee: input.Name, IncludeArchived: true, Limit: 50})
		if err != nil {
			return nil, err
		}
		evidence := make([]orchestration.ProfileEvidence, 0, len(tasks))
		for _, task := range tasks {
			evidence = append(evidence, orchestration.ProfileEvidence{Title: task.Title, Body: task.Body, Skills: task.Skills})
		}
		described, err := orchestration.DescribeProfileRoute(ctx, existing, evidence, planner)
		if err != nil {
			return nil, err
		}
		profiles := make([]boards.Profile, 0, len(metadata.Orchestration.Profiles)+1)
		for _, profile := range metadata.Orchestration.Profiles {
			if profile.Name != input.Name {
				profiles = append(profiles, profile)
			}
		}
		profiles = append(profiles, boards.Profile{Name: described.Name, Runtime: described.Runtime, Model: described.Model,
			Provider: described.Provider, Description: described.Description, Disabled: described.Disabled,
			MaxConcurrent: described.MaxConcurrent, Priority: described.Priority, Fallbacks: append([]string{}, described.Fallbacks...)})
		if _, err := s.manager.Update(board, boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
			return nil, err
		}
		return described, nil
	})
	addTool(server, "autogora_swarm", "Create a Kanban swarm", "Create a completed blackboard, parallel workers, verifier, and synthesizer.", false, false, false, false, func(ctx context.Context, input swarmInput) (any, error) {
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
	addTool(server, "autogora_update", "Update Kanban task", "Update task metadata or perform an administrative status transition.", false, true, true, false, func(ctx context.Context, input updateInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.UpdateTask(ctx, input.TaskID, store.UpdateTaskInput{Title: input.Title, Body: input.Body,
				Tenant: optionalStringSet(input.Tenant, input.tenantSet), Assignee: optionalStringSet(input.Assignee, input.assigneeSet), Runtime: input.Runtime, Priority: input.Priority,
				Workspace: optionalStringSet(input.Workspace, input.workspaceSet), WorkspaceKind: input.WorkspaceKind, Branch: optionalStringSet(input.Branch, input.branchSet),
				ScheduledAt: optionalStringSet(input.ScheduledAt, input.scheduledAtSet), MaxRuntimeSeconds: optionalIntSet(input.MaxRuntimeSeconds, input.maxRuntimeSecondsSet), Skills: input.Skills,
				GoalMode: input.GoalMode, GoalMaxTurns: input.GoalMaxTurns, WorkflowTemplateID: optionalStringSet(input.WorkflowTemplateID, input.workflowTemplateIDSet),
				CurrentStepKey: optionalStringSet(input.CurrentStepKey, input.currentStepKeySet), Status: input.Status})
		})
	})
	addTool(server, "autogora_comment", "Comment on Kanban task", "Append a durable handoff or progress note.", false, false, false, false, func(ctx context.Context, input commentInput) (any, error) {
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
	addTool(server, "autogora_link", "Link Kanban dependency", "Create a prerequisite-to-dependent execution edge.", false, false, true, false, s.linkHandler(true))
	addTool(server, "autogora_unlink", "Unlink Kanban dependency", "Remove an execution edge and recompute readiness.", false, true, true, false, s.linkHandler(false))
	addTool(server, "autogora_subtask_set", "Set Autogora subtask parent", "Place a task under one hierarchy parent without changing dependencies.", false, false, true, false, s.subtaskHandler(true))
	addTool(server, "autogora_subtask_remove", "Remove Autogora subtask parent", "Remove a hierarchy edge without changing dependencies.", false, true, true, false, s.subtaskHandler(false))
	addTool(server, "autogora_promote", "Promote Kanban task", "Move a parked task into the executable pipeline.", false, true, false, false, s.adminTaskHandler("promote"))
	addTool(server, "autogora_archive", "Archive Kanban task", "Archive a task after any active run has ended.", false, true, true, false, s.adminTaskHandler("archive"))
	addTool(server, "autogora_delete", "Delete Kanban task", "Permanently delete a task and related durable records.", false, true, false, false, s.adminTaskHandler("delete"))
	addTool(server, "autogora_unblock", "Unblock Kanban task", "Release a blocked task back to todo or ready.", false, true, false, false, s.adminTaskHandler("unblock"))
	addTool(server, "autogora_schedule", "Schedule Kanban task", "Park a task until an optional ISO-8601 time or manual promotion.", false, true, true, false, func(ctx context.Context, input scheduleInput) (any, error) {
		if err := s.requireAdmin(); err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return opened.ScheduleTask(ctx, input.TaskID, input.ScheduledAt, input.Reason)
		})
	})
	addTool(server, "autogora_claim", "Claim Kanban task", "Atomically claim one ready task and create a run lease.", false, false, false, false, func(ctx context.Context, input claimInput) (any, error) {
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
			workspaces := workspace.New(s.manager)
			prepared, err := workspaces.Prepare(ctx, opened, claim)
			if err != nil {
				message := "Workspace preparation failed: " + err.Error()
				_, _ = opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, message, store.FailRunOptions{})
				return nil, err
			}
			if _, err := workspaces.IntegratePrerequisiteChangeSets(ctx, opened, prepared); err != nil {
				var integrationErr *workspace.PrerequisiteIntegrationError
				if errors.As(err, &integrationErr) {
					_, blockErr := opened.BlockRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.BlockInput{
						Reason: integrationErr.Reason, Kind: integrationErr.BlockKind,
					})
					return nil, errors.Join(err, blockErr)
				}
				countFailure := false
				_, failErr := opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "Prerequisite integration failed: "+err.Error(), store.FailRunOptions{
					Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
				})
				return nil, errors.Join(err, failErr)
			}
			return prepared, nil
		})
	})
	addTool(server, "autogora_attach", "Attach file to Kanban task", "Copy a local file into durable board-scoped storage.", false, false, false, false, s.attachmentHandler("file"))
	addTool(server, "autogora_attach_url", "Attach URL to Kanban task", "Add an HTTP(S) reference to a task.", false, false, false, true, s.attachmentHandler("url"))
	addTool(server, "autogora_attachments", "List Kanban attachments", "List durable files and URL references for a task.", true, false, true, false, s.attachmentHandler("list"))
	addTool(server, "autogora_attachment_remove", "Remove Kanban attachment", "Remove attachment metadata and its stored file.", false, true, false, false, func(ctx context.Context, input attachmentRemoveInput) (any, error) {
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
	addTool(server, "autogora_heartbeat", "Heartbeat Kanban run", "Refresh the active run lease and record an optional note.", false, false, false, false, func(ctx context.Context, input heartbeatInput) (any, error) {
		scope, err := s.scopedRun(input.RunID, input.ClaimToken)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) { return opened.Heartbeat(ctx, scope, input.Note) })
	})
	addTool(server, "autogora_complete", "Complete Kanban run", "Complete the active run with a summary and structured evidence.", false, true, false, false, func(ctx context.Context, input completeInput) (any, error) {
		scope, err := s.scopedRun(input.RunID, input.ClaimToken)
		if err != nil {
			return nil, err
		}
		return usingStore(ctx, s, input.Board, func(opened *store.Store, _ string) (any, error) {
			return workspace.New(s.manager).CompleteRun(ctx, opened, scope, store.CompletionInput{
				Summary: input.Summary, Result: input.Result, Metadata: input.Metadata, Artifacts: input.Artifacts,
			})
		})
	})
	addTool(server, "autogora_block", "Block Kanban run", "Stop an active run for input, capability, dependency, or transient reasons.", false, true, false, false, func(ctx context.Context, input blockInput) (any, error) {
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
