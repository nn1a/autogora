package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/notifications"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type GoalJudge func(context.Context, model.TaskDetail, int, string) (orchestration.GoalJudgment, error)

type Options struct {
	DBPath                   string
	CLIPath                  string
	Board                    string
	TaskID                   string
	Once                     bool
	Interval                 time.Duration
	MaxWorkers               int
	MaxInProgress            int
	MaxInProgressPerAssignee int
	ClaimTTLSeconds          int
	StaleTimeout             time.Duration
	HeartbeatMaxStale        time.Duration
	CrashGrace               *time.Duration
	RateLimitCooldown        *time.Duration
	AgentRetryCooldown       *time.Duration
	FailureLimit             int
	NotificationLimit        int
	NotificationTimeout      time.Duration
	GoalJudge                GoalJudge
	AutoDecompose            *bool
	AutoDecomposePerTick     int
	DecompositionProfiles    []orchestration.ProfileRoute
	DefaultProfile           *orchestration.ProfileRoute
	OrchestratorProfile      *orchestration.ProfileRoute
	PlannerRuntime           model.Runtime
	PlannerModel             string
	PlannerProvider          string
	PlannerTimeout           time.Duration
	DecompositionPlanner     orchestration.Planner
	AllowWrites              bool
	ClineApprovalDir         string
	WorkingDirectory         string
	Getenv                   func(string) string
	AgentConfig              *agentconfig.Config
	OnLog                    func(string)
}

func (o *Options) normalize() {
	if o.Interval < 250*time.Millisecond {
		o.Interval = 2 * time.Second
	}
	if o.MaxWorkers < 1 {
		o.MaxWorkers = 2
	}
	if o.ClaimTTLSeconds < 1 {
		o.ClaimTTLSeconds = 900
	}
	if o.StaleTimeout < time.Minute {
		o.StaleTimeout = 4 * time.Hour
	}
	if o.HeartbeatMaxStale < time.Minute {
		o.HeartbeatMaxStale = time.Hour
	}
	if o.CrashGrace == nil {
		value := 30 * time.Second
		o.CrashGrace = &value
	}
	if o.RateLimitCooldown == nil {
		value := time.Minute
		o.RateLimitCooldown = &value
	}
	if o.AgentRetryCooldown == nil {
		value := 5 * time.Minute
		o.AgentRetryCooldown = &value
	}
	if o.NotificationLimit < 1 {
		o.NotificationLimit = 25
	}
	if o.NotificationTimeout <= 0 {
		o.NotificationTimeout = 5 * time.Second
	}
	if o.PlannerTimeout <= 0 {
		o.PlannerTimeout = 120 * time.Second
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
}

func validWorkerRuntime(runtime model.Runtime) bool {
	return runtime == model.RuntimeClaude || runtime == model.RuntimeCodex || runtime == model.RuntimeCline || runtime == model.RuntimeGemini
}

func (o Options) log(format string, values ...any) {
	if o.OnLog != nil {
		o.OnLog(fmt.Sprintf(format, values...))
	}
}

func parseTimestamp(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

type agentAvailabilityFailure struct {
	Status  model.AgentHealthStatus
	Outcome model.RunStatus
}

func containsAny(value string, patterns ...string) bool {
	for _, pattern := range patterns {
		if strings.Contains(value, pattern) {
			return true
		}
	}
	return false
}

func classifyAgentAvailability(execution TurnExecution) (agentAvailabilityFailure, bool) {
	if execution.SpawnError != nil {
		message := strings.ToLower(execution.SpawnError.Error())
		if errors.Is(execution.SpawnError, exec.ErrNotFound) || errors.Is(execution.SpawnError, os.ErrNotExist) ||
			containsAny(message, "executable file not found", "no such file or directory", "cannot find the file") {
			return agentAvailabilityFailure{Status: model.AgentHealthMissing, Outcome: model.RunStatusSpawnFailed}, true
		}
	}
	message := strings.ToLower(execution.Output)
	if execution.SpawnError != nil {
		message += "\n" + strings.ToLower(execution.SpawnError.Error())
	}
	if execution.Code == 75 || containsAny(message,
		"rate limit", "rate_limit", "too many requests", "resource_exhausted", "resource exhausted",
		"quota exceeded", "usage limit", "usage_limit", "credit balance", "http 429", "status 429", `"status":429`) {
		return agentAvailabilityFailure{Status: model.AgentHealthRateLimited, Outcome: model.RunStatusRateLimited}, true
	}
	if containsAny(message,
		"authentication required", "authentication failed", "not logged in", "please log in", "please login",
		"unauthorized", "invalid api key", "invalid_api_key", "api key not found", "missing api key",
		"http 401", "status 401", `"status":401`) {
		return agentAvailabilityFailure{Status: model.AgentHealthAuthRequired, Outcome: model.RunStatusFailed}, true
	}
	return agentAvailabilityFailure{}, false
}

func agentCooldown(status model.AgentHealthStatus, rateLimit, retry time.Duration) *string {
	duration := retry
	if status == model.AgentHealthRateLimited {
		duration = rateLimit
	}
	if duration < 0 {
		return nil
	}
	value := time.Now().Add(duration).UTC().Format(time.RFC3339Nano)
	return &value
}

func recoverAbandonedRuns(ctx context.Context, opened *store.Store, board string, options Options) error {
	active, err := opened.ListActiveRuns(ctx, board)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, item := range active {
		elapsed := now.Sub(parseTimestamp(item.Run.ClaimedAt))
		heartbeatAge := now.Sub(parseTimestamp(item.Run.HeartbeatAt))
		expired := !now.Before(parseTimestamp(item.Run.ClaimExpiresAt))
		stale := elapsed >= options.StaleTimeout && heartbeatAge >= options.HeartbeatMaxStale
		timedOut := item.Task.MaxRuntimeSeconds != nil && elapsed >= time.Duration(*item.Task.MaxRuntimeSeconds)*time.Second
		alive := item.Run.PID != nil && processAlive(*item.Run.PID)
		crashed := item.Run.PID != nil && elapsed >= *options.CrashGrace && !alive
		switch {
		case timedOut:
			_ = runcontrol.SignalRunProcess(item.Run.PID)
			if _, err := opened.RecoverAbandonedRun(ctx, item.Run.ID, model.RunStatusTimedOut, fmt.Sprintf("Maximum runtime exceeded after %d seconds", int(elapsed.Seconds())), true); err != nil {
				return err
			}
			options.log("timed out %s", item.Task.ID)
		case crashed:
			if _, err := opened.RecoverAbandonedRun(ctx, item.Run.ID, model.RunStatusCrashed, fmt.Sprintf("Worker PID %d is no longer alive", *item.Run.PID), true); err != nil {
				return err
			}
			options.log("reclaimed crashed worker %s", item.Task.ID)
		case expired || stale:
			if alive && runcontrol.SignalRunProcess(item.Run.PID) {
				if _, err := opened.DeferReclaim(ctx, item.Run.ID, 120, "Dispatcher terminating stale worker"); err != nil {
					return err
				}
				options.log("deferred reclaim while terminating PID %d for %s", *item.Run.PID, item.Task.ID)
			} else {
				reason := "Claim TTL expired"
				if stale {
					reason = "Heartbeat became stale"
				}
				if _, err := opened.RecoverAbandonedRun(ctx, item.Run.ID, model.RunStatusReclaimed, reason, false); err != nil {
					return err
				}
				options.log("reclaimed %s: %s", item.Task.ID, reason)
			}
		}
	}
	return nil
}

func deliverBoardNotifications(ctx context.Context, manager *boards.Manager, boardSlugs []string, options Options) {
	var wait sync.WaitGroup
	for _, board := range boardSlugs {
		board := board
		wait.Add(1)
		go func() {
			defer wait.Done()
			opened, err := manager.OpenStore(ctx, board)
			if err != nil {
				options.log("notification sweep failed for %s: %v", board, err)
				return
			}
			defer opened.Close()
			results, err := notifications.Deliver(ctx, opened, notifications.Options{Limit: options.NotificationLimit, Timeout: options.NotificationTimeout})
			if err != nil {
				options.log("notification sweep failed for %s: %v", board, err)
				return
			}
			for _, delivery := range results {
				if delivery.Delivered {
					options.log("notified %s: %s", delivery.TaskID, delivery.EventKind)
				} else {
					options.log("notification failed %s: %s", delivery.TaskID, delivery.Error)
				}
			}
		}()
	}
	wait.Wait()
}

func boardProfiles(configured []boards.Profile) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(configured))
	for _, profile := range configured {
		result = append(result, orchestration.ProfileRoute{Name: profile.Name, Runtime: profile.Runtime, Model: profile.Model, Provider: profile.Provider,
			Description: profile.Description, Disabled: profile.Disabled, MaxConcurrent: profile.MaxConcurrent, Priority: profile.Priority,
			Fallbacks: append([]string{}, profile.Fallbacks...)})
	}
	return result
}

type configuredProfileSet struct {
	Profiles       []orchestration.ProfileRoute
	Commands       map[string]string
	Sources        map[string]string
	DefaultWorkers []string
	Config         agentconfig.Config
}

func hasAgentRole(agent agentconfig.Agent, role agentconfig.Role) bool {
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

func concurrencyCap(global, board int) int {
	switch {
	case global > 0 && board > 0:
		return min(global, board)
	case global > 0:
		return global
	default:
		return board
	}
}

func configuredProfiles(manager *boards.Manager, board string, options Options) (configuredProfileSet, error) {
	metadata, err := manager.Read(board)
	if err != nil {
		return configuredProfileSet{}, err
	}
	config := agentconfig.Default()
	if options.AgentConfig != nil {
		config = agentconfig.Normalize(*options.AgentConfig)
		if err := agentconfig.Validate(config); err != nil {
			return configuredProfileSet{}, fmt.Errorf("validate agent configuration: %w", err)
		}
	} else {
		config, err = agentconfig.Load(agentconfig.Options{Getenv: options.Getenv})
		if err != nil {
			return configuredProfileSet{}, err
		}
	}
	set := configuredProfileSet{
		Profiles:       make([]orchestration.ProfileRoute, 0, len(config.Agents)+len(metadata.Orchestration.Profiles)),
		Commands:       map[string]string{},
		Sources:        map[string]string{},
		DefaultWorkers: append([]string{}, config.Defaults.WorkerAgents...),
		Config:         config,
	}
	indexes := map[string]int{}
	for _, agent := range config.Agents {
		if !hasAgentRole(agent, agentconfig.RoleWorker) {
			continue
		}
		indexes[agent.ID] = len(set.Profiles)
		set.Profiles = append(set.Profiles, orchestration.ProfileRoute{
			Name: agent.ID, Runtime: agent.Runtime, Model: agent.Model, Provider: agent.Provider,
			Disabled: !agent.Enabled, MaxConcurrent: agent.MaxConcurrent, Fallbacks: append([]string{}, agent.Fallbacks...),
		})
		set.Commands[agent.ID] = agent.Command
		set.Sources[agent.ID] = "global_profile"
	}
	for _, profile := range boardProfiles(metadata.Orchestration.Profiles) {
		index, registered := indexes[profile.Name]
		if !registered {
			indexes[profile.Name] = len(set.Profiles)
			set.Profiles = append(set.Profiles, profile)
			set.Sources[profile.Name] = "board_profile"
			continue
		}
		global := set.Profiles[index]
		// Runtime, disabled state, command, and the global concurrency cap are
		// registry policy. A board may specialize routing metadata and lower
		// concurrency without turning an unavailable executable back on.
		if profile.Model != "" {
			global.Model = profile.Model
		}
		if profile.Provider != "" {
			global.Provider = profile.Provider
		}
		if profile.Description != "" {
			global.Description = profile.Description
		}
		global.Disabled = global.Disabled || profile.Disabled
		global.MaxConcurrent = concurrencyCap(global.MaxConcurrent, profile.MaxConcurrent)
		global.Priority = profile.Priority
		if len(profile.Fallbacks) > 0 {
			global.Fallbacks = append([]string{}, profile.Fallbacks...)
		}
		set.Profiles[index] = global
		set.Sources[profile.Name] = "board_profile"
	}
	set.Profiles = orchestration.ResolveProfileRoutes(nil, set.Profiles)
	return set, nil
}

func claimProfilePolicy(ctx context.Context, manager *boards.Manager, opened *store.Store, board string, options Options) ([]string, map[string]int, error) {
	configured, err := configuredProfiles(manager, board, options)
	if err != nil {
		return nil, nil, err
	}
	excluded := make([]string, 0)
	limits := map[string]int{}
	for _, profile := range configured.Profiles {
		if _, available, availabilityErr := selectAvailableProfile(ctx, opened, profile.Name, configured.Profiles); availabilityErr != nil {
			return nil, nil, availabilityErr
		} else if !available {
			excluded = append(excluded, profile.Name)
			continue
		}
		if profile.MaxConcurrent > 0 {
			limits[profile.Name] = profile.MaxConcurrent
		}
	}
	return excluded, limits, nil
}

func selectAvailableProfile(ctx context.Context, opened *store.Store, desired string, profiles []orchestration.ProfileRoute) (orchestration.ProfileRoute, bool, error) {
	byName := make(map[string]orchestration.ProfileRoute, len(profiles))
	for _, profile := range profiles {
		if name := strings.TrimSpace(profile.Name); name != "" {
			byName[name] = profile
		}
	}
	queue, seen := []string{desired}, map[string]bool{}
	for len(queue) > 0 {
		candidateName := queue[0]
		queue = queue[1:]
		if seen[candidateName] {
			continue
		}
		seen[candidateName] = true
		candidate, exists := byName[candidateName]
		if !exists {
			continue
		}
		queue = append(queue, candidate.Fallbacks...)
		if !orchestration.RunnableProfileRoute(candidate) {
			continue
		}
		health, err := opened.GetAgentHealth(ctx, candidate.Name)
		if err != nil {
			return orchestration.ProfileRoute{}, false, err
		}
		if store.IsAgentUnavailable(health, time.Now()) {
			continue
		}
		if candidate.MaxConcurrent > 0 {
			active, err := opened.CountActiveAgentRuns(ctx, candidate.Name)
			if err != nil {
				return orchestration.ProfileRoute{}, false, err
			}
			if active >= candidate.MaxConcurrent {
				continue
			}
		}
		return candidate, true, nil
	}
	return orchestration.ProfileRoute{}, false, nil
}

type resolvedRunProfile struct {
	orchestration.ProfileRoute
	Source       string
	FallbackFrom *string
	Command      string
}

func resolveRunProfile(ctx context.Context, manager *boards.Manager, opened *store.Store, task model.Task, options Options) (resolvedRunProfile, error) {
	name := string(task.Runtime) + "-worker"
	if task.Assignee != nil && strings.TrimSpace(*task.Assignee) != "" {
		name = strings.TrimSpace(*task.Assignee)
	}
	taskRoute := orchestration.ProfileRoute{Name: name, Runtime: task.Runtime}
	configured, err := configuredProfiles(manager, task.Board, options)
	if err != nil {
		return resolvedRunProfile{}, err
	}
	configuredDesired := false
	for _, profile := range configured.Profiles {
		if strings.TrimSpace(profile.Name) != "" {
			configuredDesired = configuredDesired || profile.Name == name
		}
	}
	profiles := configured.Profiles
	if !configuredDesired {
		profiles = append(append([]orchestration.ProfileRoute{}, profiles...), taskRoute)
	}
	selected, available, err := selectAvailableProfile(ctx, opened, name, profiles)
	if err != nil {
		return resolvedRunProfile{}, err
	}
	if !available {
		return resolvedRunProfile{}, fmt.Errorf("no available agent for profile %s", name)
	}
	source := configured.Sources[selected.Name]
	if source == "" {
		source = "board_profile"
	}
	if selected.Name != name {
		source = "fallback"
	} else if !configuredDesired {
		source = "task_route"
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	prefix := "AUTOGORA_" + strings.ToUpper(string(selected.Runtime))
	if strings.TrimSpace(selected.Model) == "" {
		selected.Model = strings.TrimSpace(getenv(prefix + "_MODEL"))
	}
	if strings.TrimSpace(selected.Provider) == "" {
		selected.Provider = strings.TrimSpace(getenv(prefix + "_PROVIDER"))
	}
	if selected.Model == "" && source == "task_route" {
		source = "cli_default"
	}
	resolved := resolvedRunProfile{ProfileRoute: selected, Source: source, Command: configured.Commands[selected.Name]}
	if selected.Name != name {
		resolved.FallbackFrom = &name
	}
	return resolved, nil
}

func mergeDecompositionProfiles(configured configuredProfileSet, overrides []orchestration.ProfileRoute) []orchestration.ProfileRoute {
	profiles := append([]orchestration.ProfileRoute{}, configured.Profiles...)
	indexes := make(map[string]int, len(profiles))
	for index, profile := range profiles {
		indexes[profile.Name] = index
	}
	for _, override := range overrides {
		override.Name = strings.TrimSpace(override.Name)
		if override.Name == "" {
			continue
		}
		_, exists := indexes[override.Name]
		if !exists {
			indexes[override.Name] = len(profiles)
			profiles = append(profiles, override)
			continue
		}
		// CLI decomposition routes may add ephemeral workers, but an existing
		// global or board profile remains authoritative. Otherwise --profile,
		// whose syntax cannot express the full execution policy, could erase a
		// pinned model, disabled state, or concurrency cap.
	}
	return orchestration.ResolveProfileRoutes(nil, profiles)
}

func firstConfiguredAgent(config agentconfig.Config, ids []string, role agentconfig.Role) (agentconfig.Agent, bool) {
	for _, id := range ids {
		agent, found := config.Find(id)
		if found && agent.Enabled && hasAgentRole(agent, role) {
			return agent, true
		}
	}
	return agentconfig.Agent{}, false
}

func plannerConfiguration(metadata boards.Metadata, configured configuredProfileSet, options Options) (model.Runtime, string, string, string) {
	if options.PlannerRuntime != "" {
		return options.PlannerRuntime, strings.TrimSpace(options.PlannerModel), strings.TrimSpace(options.PlannerProvider), ""
	}
	runtime := metadata.Orchestration.PlannerRuntime
	modelName := strings.TrimSpace(metadata.Orchestration.PlannerModel)
	provider := strings.TrimSpace(metadata.Orchestration.PlannerProvider)
	command := ""
	// A board with an explicitly pinned planner keeps that choice. New boards
	// have an unpinned Codex default, so the global planner order supplies the
	// actual runtime and model when one has been configured.
	if modelName == "" && provider == "" {
		if agent, found := firstConfiguredAgent(configured.Config, configured.Config.Defaults.PlannerAgents, agentconfig.RolePlanner); found {
			runtime, modelName, provider, command = agent.Runtime, agent.Model, agent.Provider, agent.Command
		}
	}
	if value := strings.TrimSpace(options.PlannerModel); value != "" {
		modelName = value
	}
	if value := strings.TrimSpace(options.PlannerProvider); value != "" {
		provider = value
	}
	return runtime, modelName, provider, command
}

func judgeConfiguration(metadata boards.Metadata, configured configuredProfileSet, options Options) (model.Runtime, string, string, string) {
	if agent, found := firstConfiguredAgent(configured.Config, configured.Config.Defaults.JudgeAgents, agentconfig.RoleJudge); found {
		return agent.Runtime, agent.Model, agent.Provider, agent.Command
	}
	return plannerConfiguration(metadata, configured, options)
}

func decomposeBoardTriage(ctx context.Context, manager *boards.Manager, boardSlugs []string, options Options) {
	remaining := options.AutoDecomposePerTick
	if remaining <= 0 {
		remaining = 500
	}
	for _, board := range boardSlugs {
		if remaining <= 0 || ctx.Err() != nil {
			return
		}
		metadata, err := manager.Read(board)
		if err != nil {
			options.log("auto-decompose metadata failed %s: %v", board, err)
			continue
		}
		enabled := metadata.Orchestration.AutoDecompose
		if options.AutoDecompose != nil {
			enabled = *options.AutoDecompose
		}
		if !enabled {
			continue
		}
		boardRemaining := min(remaining, metadata.Orchestration.AutoDecomposePerTick)
		if options.AutoDecomposePerTick > 0 {
			boardRemaining = min(remaining, options.AutoDecomposePerTick)
		}
		configured, err := configuredProfiles(manager, board, options)
		if err != nil {
			options.log("auto-decompose profiles failed %s: %v", board, err)
			continue
		}
		plannerRuntime, plannerModel, plannerProvider, plannerCommand := plannerConfiguration(metadata, configured, options)
		planner := options.DecompositionPlanner
		if planner == nil {
			planner, err = orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: plannerRuntime, Model: plannerModel,
				Provider: plannerProvider, Command: plannerCommand, CWD: options.WorkingDirectory, Timeout: options.PlannerTimeout, Getenv: options.Getenv})
			if err != nil {
				options.log("auto-decompose planner failed %s: %v", board, err)
				continue
			}
		}
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			options.log("auto-decompose store failed %s: %v", board, err)
			continue
		}
		triage, listErr := opened.ListTasks(ctx, store.ListTaskFilter{Status: model.TaskStatusTriage, Limit: boardRemaining})
		discovered, discoverErr := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
		if listErr != nil || discoverErr != nil {
			options.log("auto-decompose list failed %s: %v", board, errors.Join(listErr, discoverErr))
			opened.Close()
			continue
		}
		decompositionProfiles := mergeDecompositionProfiles(configured, options.DecompositionProfiles)
		profiles := orchestration.ResolveProfileRoutes(discovered, decompositionProfiles)
		for _, task := range triage {
			defaultName, orchestratorName := metadata.Orchestration.DefaultProfile, metadata.Orchestration.OrchestratorProfile
			if defaultName == nil {
				if globalDefault, found := firstConfiguredAgent(configured.Config, configured.DefaultWorkers, agentconfig.RoleWorker); found {
					value := globalDefault.ID
					defaultName = &value
				}
			}
			fallback, orchestrator := orchestration.SelectProfileRoutes(profiles, defaultName, orchestratorName, plannerRuntime)
			if options.DefaultProfile != nil {
				fallback = *options.DefaultProfile
			}
			if options.DefaultProfile == nil && metadata.Orchestration.DefaultProfile == nil && task.Assignee != nil && task.Runtime != model.RuntimeManual {
				for _, candidate := range profiles {
					if candidate.Name == *task.Assignee && orchestration.RunnableProfileRoute(candidate) {
						fallback = candidate
						break
					}
				}
			}
			if options.OrchestratorProfile != nil {
				orchestrator = *options.OrchestratorProfile
			} else if metadata.Orchestration.OrchestratorProfile == nil && fallback.Name != orchestrator.Name {
				orchestrator = fallback
			}
			value := metadata.Orchestration.AutoPromoteChildren
			result, err := orchestration.DecomposeTriageTask(ctx, opened, task.ID, orchestration.DecomposeOptions{
				Profiles: profiles, DefaultProfile: fallback, OrchestratorProfile: &orchestrator, AutoPromoteChildren: &value, Planner: planner,
			})
			if err != nil {
				options.log("auto-decompose failed %s: %v", task.ID, err)
			} else {
				action := "specified"
				if result.Fanout {
					action = "decomposed"
				}
				options.log("auto-%s %s: %s", action, task.ID, result.Reason)
			}
			remaining--
			boardRemaining--
			if remaining <= 0 || boardRemaining <= 0 {
				break
			}
		}
		opened.Close()
	}
}

func durableContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func hasDeferredReclaim(detail model.TaskDetail, runID string) bool {
	if len(detail.Events) == 0 {
		return false
	}
	event := detail.Events[len(detail.Events)-1]
	return event.Kind == "reclaim_deferred" && event.RunID != nil && *event.RunID == runID
}

func finalizeManagedTerminal(ctx context.Context, opened *store.Store, workspaces *workspace.Manager, prepared *model.ClaimedTask, scope store.RunScope, exitCode int) (model.TaskDetail, error) {
	request, err := opened.GetRunTerminalRequest(ctx, scope.RunID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if request == nil {
		return model.TaskDetail{}, fmt.Errorf("run has no terminal request: %s", scope.RunID)
	}
	if request.Kind == "complete" && prepared.Workspace != nil && prepared.Workspace.Kind == model.WorkspaceWorktree {
		existing, err := opened.GetRunChangeSet(ctx, scope.RunID)
		if err != nil {
			return model.TaskDetail{}, err
		}
		if existing == nil {
			snapshot, err := workspaces.CaptureChangeSet(ctx, *prepared.Workspace, prepared.Task.Task.ID, prepared.Task.Task.Title)
			if err != nil {
				return model.TaskDetail{}, err
			}
			if err := workspaces.VerifyPrerequisiteChangeSets(ctx, opened, prepared.Task.Task.ID, *prepared.Workspace, snapshot.HeadCommit); err != nil {
				return model.TaskDetail{}, err
			}
			if _, err := opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
				RunID: scope.RunID, RepositoryPath: snapshot.RepositoryPath, WorktreePath: snapshot.WorktreePath,
				BaseCommit: snapshot.BaseCommit, HeadCommit: snapshot.HeadCommit, DurableRef: snapshot.DurableRef,
				State: snapshot.State, ChangedFiles: snapshot.ChangedFiles,
			}); err != nil {
				return model.TaskDetail{}, err
			}
		}
	}
	return opened.FinalizeRunTerminal(ctx, scope.RunID, exitCode)
}

func runClaim(ctx context.Context, manager *boards.Manager, opened *store.Store, claim *model.ClaimedTask, options Options, processes *ProcessSet, clineApprovalDir string) {
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		durable, cancel := durableContext()
		defer cancel()
		_, _ = opened.FailRun(durable, scope, "Unable to register dispatcher ownership: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit})
		return
	}
	profile, err := resolveRunProfile(ctx, manager, opened, claim.Task.Task, options)
	if err != nil {
		durable, cancel := durableContext()
		defer cancel()
		countFailure := false
		_, _ = opened.FailRun(durable, scope, "Agent profile resolution failed: "+err.Error(), store.FailRunOptions{
			Outcome: model.RunStatusReclaimed, CountFailure: &countFailure, CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
		})
		return
	}
	configured, err := opened.RecordRunAgentConfig(ctx, scope, store.RecordRunAgentConfigInput{
		Profile: profile.Name, Runtime: profile.Runtime, Model: profile.Model, Provider: profile.Provider, Source: profile.Source,
		FallbackFrom: profile.FallbackFrom, AllowRuntimeOverride: profile.Runtime != claim.Run.Runtime,
	})
	if err != nil {
		durable, cancel := durableContext()
		defer cancel()
		_, _ = opened.FailRun(durable, scope, "Unable to pin agent configuration: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit})
		return
	}
	claim.Run.Runtime = configured.Runtime
	claim.Task.Task.Runtime = configured.Runtime
	workspaces := workspace.New(manager)
	workspaces.SetAllowWrites(options.AllowWrites)
	if options.WorkingDirectory != "" {
		workspaces.SetWorkingDirectory(options.WorkingDirectory)
	}
	prepared, err := workspaces.Prepare(ctx, opened, claim)
	if err != nil {
		durable, cancel := durableContext()
		defer cancel()
		failure := store.FailRunOptions{FailureLimit: options.FailureLimit}
		if errors.Is(err, store.ErrResourceBusy) {
			countFailure := false
			failure = store.FailRunOptions{Outcome: model.RunStatusReclaimed, CountFailure: &countFailure, CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit}
		}
		_, _ = opened.FailRun(durable, scope, "Workspace preparation failed: "+err.Error(), failure)
		options.log("workspace failure %s: %v", claim.Task.Task.ID, err)
		return
	}
	if _, err := workspaces.IntegratePrerequisiteChangeSets(ctx, opened, prepared); err != nil {
		durable, cancel := durableContext()
		defer cancel()
		var integrationErr *workspace.PrerequisiteIntegrationError
		if errors.As(err, &integrationErr) {
			_, blockErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: integrationErr.Reason, Kind: integrationErr.BlockKind})
			if blockErr == nil {
				_, blockErr = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			if blockErr != nil {
				_ = opened.DiscardRunTerminalRequest(durable, scope, "prerequisite integration block finalization failed")
				countFailure := false
				_, failErr := opened.FailRun(durable, scope, integrationErr.Reason, store.FailRunOptions{
					CountFailure: &countFailure, FailureLimit: options.FailureLimit,
				})
				if failErr == nil {
					_, failErr = opened.BlockTask(durable, claim.Task.Task.ID, store.BlockInput{Reason: integrationErr.Reason, Kind: integrationErr.BlockKind})
				}
				options.log("prerequisite integration block fallback failed %s: %v", claim.Task.Task.ID, errors.Join(blockErr, failErr))
			}
			options.log("blocked prerequisite integration for %s: %v", claim.Task.Task.ID, err)
			return
		}
		countFailure := false
		_, _ = opened.FailRun(durable, scope, "Prerequisite integration failed: "+err.Error(), store.FailRunOptions{
			Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
			CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
		})
		options.log("prerequisite integration failure %s: %v", claim.Task.Task.ID, err)
		return
	}
	logsRoot, rootsErr := manager.LogsRoot(prepared.Task.Task.Board)
	workspaceRoot, workspaceErr := manager.WorkspaceRoot(prepared.Task.Task.Board)
	attachmentsRoot, attachmentsErr := manager.AttachmentsRoot(prepared.Task.Task.Board)
	if rootsErr != nil || workspaceErr != nil || attachmentsErr != nil {
		err := errors.Join(rootsErr, workspaceErr, attachmentsErr)
		durable, cancel := durableContext()
		defer cancel()
		_, _ = opened.FailRun(durable, scope, "Board path resolution failed: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit})
		return
	}
	if err := os.MkdirAll(logsRoot, 0o755); err != nil {
		durable, cancel := durableContext()
		defer cancel()
		_, _ = opened.FailRun(durable, scope, "Log directory creation failed: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit})
		options.log("log directory failure %s: %v", prepared.Task.Task.ID, err)
		return
	}
	logPath := filepath.Join(logsRoot, prepared.Task.Task.ID+"-"+prepared.Run.ID+".log")
	runnerOptions := RunnerOptions{DBPath: options.DBPath, CLIPath: options.CLIPath, Profile: configured.Profile,
		Command: profile.Command, Model: configured.Model, Provider: configured.Provider, AllowWrites: options.AllowWrites,
		WorkspaceRoot: workspaceRoot, AttachmentsRoot: attachmentsRoot, LogsRoot: logsRoot,
		ClineApprovalDir: clineApprovalDir, Getenv: options.Getenv}
	taskID := prepared.Task.Task.ID
	goalMode := prepared.Task.Task.GoalMode
	sessionID := ""
	if goalMode && prepared.Task.Task.Runtime == model.RuntimeClaude {
		sessionID = uuid.NewString()
	}
	turn, continuation := 1, ""
	runStarted := parseTimestamp(prepared.Run.ClaimedAt)
	var goalPlanner orchestration.Planner
	if goalMode && options.GoalJudge == nil {
		metadata, metadataErr := manager.Read(prepared.Task.Task.Board)
		profileSet, profileErr := configuredProfiles(manager, prepared.Task.Task.Board, options)
		if metadataErr != nil || profileErr != nil {
			durable, cancel := durableContext()
			defer cancel()
			if _, requestErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: "Goal judge configuration failed: " + errors.Join(metadataErr, profileErr).Error(), Kind: model.BlockKindTransient}); requestErr == nil {
				_, _ = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			return
		}
		plannerCWD := options.WorkingDirectory
		if prepared.Workspace != nil {
			plannerCWD = prepared.Workspace.Path
		}
		judgeRuntime, judgeModel, judgeProvider, judgeCommand := judgeConfiguration(metadata, profileSet, options)
		goalPlanner, err = orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: judgeRuntime,
			Command: judgeCommand, Model: judgeModel, Provider: judgeProvider, CWD: plannerCWD, Timeout: options.PlannerTimeout, Getenv: options.Getenv})
		if err != nil {
			durable, cancel := durableContext()
			defer cancel()
			if _, requestErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: "Goal judge setup failed: " + err.Error(), Kind: model.BlockKindTransient}); requestErr == nil {
				_, _ = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			return
		}
	}
	cleanupIfDone := func() {
		durable, cancel := durableContext()
		defer cancel()
		current, err := opened.GetTask(durable, taskID)
		if err != nil {
			return
		}
		options.log("finish %s: %s", current.Task.ID, current.Task.Status)
		if current.Task.Status == model.TaskStatusDone && prepared.Workspace != nil {
			if _, err := workspaces.Cleanup(current.Task.Board, *prepared.Workspace); err != nil {
				options.log("workspace cleanup failed %s: %v", current.Task.ID, err)
			}
		}
	}
	blockPreservedRun := func(durable context.Context, reason string, exitCode int) error {
		request, requestErr := opened.GetRunTerminalRequest(durable, scope.RunID)
		if requestErr != nil {
			return requestErr
		}
		if request != nil && request.FinalizedAt == nil {
			if err := opened.DiscardRunTerminalRequest(durable, scope, "preserving workspace after failed execution"); err != nil {
				return err
			}
		}
		_, blockErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: reason, Kind: model.BlockKindNeedsInput})
		if blockErr == nil {
			_, blockErr = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, exitCode)
		}
		if blockErr == nil {
			return nil
		}
		_ = opened.DiscardRunTerminalRequest(durable, scope, "preserved-work block finalization failed")
		countFailure := false
		_, failErr := opened.FailRun(durable, scope, reason, store.FailRunOptions{
			CountFailure: &countFailure, FailureLimit: options.FailureLimit,
		})
		if failErr == nil {
			_, failErr = opened.BlockTask(durable, taskID, store.BlockInput{Reason: reason, Kind: model.BlockKindNeedsInput})
		}
		return errors.Join(blockErr, failErr)
	}
	preserveIfChanged := func(durable context.Context, reason string, exitCode int) bool {
		if !options.AllowWrites || prepared.Workspace == nil {
			return false
		}
		partial, partialErr := false, error(nil)
		switch prepared.Workspace.Kind {
		case model.WorkspaceWorktree, model.WorkspaceScratch:
			partial, partialErr = workspaces.HasChanges(durable, *prepared.Workspace)
		case model.WorkspaceDir:
			// A shared directory has no per-run baseline. Once a writable agent
			// starts, a different agent must not overwrite an uncertain result.
			partial = true
		}
		if !partial && partialErr == nil {
			return false
		}
		reason += "; partial changes remain at " + prepared.Workspace.Path
		if partialErr != nil {
			reason += "; Autogora could not verify the workspace state: " + partialErr.Error()
		}
		if err := blockPreservedRun(durable, reason, exitCode); err != nil {
			options.log("preserved-work block fallback failed %s: %v", taskID, err)
		}
		options.log("preserved partial work for %s", taskID)
		return true
	}

	for {
		var command RunnerCommand
		if continuation != "" && (sessionID != "" || prepared.Task.Task.Runtime == model.RuntimeCline) {
			command, err = BuildGoalContinuationCommand(*prepared, runnerOptions, sessionID, continuation)
		} else {
			command, err = BuildRunnerCommand(*prepared, runnerOptions, sessionID)
		}
		if err != nil {
			durable, cancel := durableContext()
			if !preserveIfChanged(durable, "Runner command construction failed after work began: "+err.Error(), 1) {
				_, _ = opened.FailRun(durable, scope, err.Error(), store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			}
			cancel()
			return
		}
		var runtimeLimit *time.Duration
		if prepared.Task.Task.MaxRuntimeSeconds != nil {
			value := time.Duration(*prepared.Task.Task.MaxRuntimeSeconds)*time.Second - time.Since(runStarted)
			runtimeLimit = &value
		}
		modelName := configured.Model
		if modelName == "" {
			modelName = "CLI default (unpinned)"
		}
		options.log("launch %s via %s/%s profile=%s goal_turn=%d log=%s", taskID, prepared.Task.Task.Runtime, modelName, configured.Profile, turn, logPath)
		execution := ExecuteTurn(ctx, command, opened, scope, processes, logPath, runtimeLimit)
		durable, cancel := durableContext()
		currentDetail, getErr := opened.GetTask(durable, taskID)
		if getErr != nil {
			cancel()
			options.log("post-run read failed %s: %v", taskID, getErr)
			return
		}
		current := currentDetail.Task
		if current.Status != model.TaskStatusRunning || current.CurrentRunID == nil || *current.CurrentRunID != prepared.Run.ID {
			cancel()
			cleanupIfDone()
			return
		}
		if hasDeferredReclaim(currentDetail, prepared.Run.ID) {
			_, _ = opened.RecoverAbandonedRun(durable, prepared.Run.ID, model.RunStatusReclaimed, "Claim reclaimed after worker termination", false)
			cancel()
			options.log("reclaimed %s after deferred termination", taskID)
			return
		}
		terminalRequest, requestErr := opened.GetRunTerminalRequest(durable, prepared.Run.ID)
		if requestErr != nil {
			cancel()
			options.log("terminal request read failed %s: %v", taskID, requestErr)
			return
		}
		executionSucceeded := !execution.TimedOut && execution.SpawnError == nil && execution.Code == 0 && !execution.Canceled
		if executionSucceeded {
			lastRunID := prepared.Run.ID
			if _, healthErr := opened.SetAgentHealth(durable, store.SetAgentHealthInput{
				AgentID: configured.Profile, Status: model.AgentHealthReady, LastRunID: &lastRunID,
			}); healthErr != nil {
				options.log("agent health update failed %s: %v", configured.Profile, healthErr)
			}
		}
		if terminalRequest != nil && terminalRequest.FinalizedAt == nil && executionSucceeded {
			if goalMode && terminalRequest.Kind == "complete" {
				if err := opened.DiscardRunTerminalRequest(durable, scope, "goal completion requires independent judgment"); err != nil {
					cancel()
					return
				}
			} else {
				if _, err := finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, execution.Code); err != nil {
					reason := "Terminal finalization failed; the workspace was preserved for review: " + err.Error()
					if prepared.Workspace != nil {
						reason += "; workspace: " + prepared.Workspace.Path
					}
					_ = blockPreservedRun(durable, reason, execution.Code)
					cancel()
					options.log("terminal finalization failed %s: %v", taskID, err)
					return
				}
				cancel()
				cleanupIfDone()
				return
			}
		}
		detail := fmt.Sprintf("Runner exited without a terminal Autogora call (%s)", execution.ExitDescription())
		if execution.SpawnError != nil {
			detail = execution.SpawnError.Error()
		}
		availability, agentUnavailable := classifyAgentAvailability(execution)
		switch {
		case execution.TimedOut || (runtimeLimit != nil && *runtimeLimit <= 0):
			if preserveIfChanged(durable, "Runner timed out after work began: "+detail, execution.Code) {
				cancel()
				return
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusTimedOut, FailureLimit: options.FailureLimit})
			cancel()
			options.log("requeue/fail %s: %s", taskID, detail)
			return
		case execution.Canceled:
			if preserveIfChanged(durable, "Runner was canceled after work began: "+detail, execution.Code) {
				cancel()
				return
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{FailureLimit: options.FailureLimit})
			cancel()
			return
		case agentUnavailable:
			lastRunID, lastError := prepared.Run.ID, detail
			cooldownUntil := agentCooldown(availability.Status, *options.RateLimitCooldown, *options.AgentRetryCooldown)
			if _, healthErr := opened.SetAgentHealth(durable, store.SetAgentHealthInput{
				AgentID: configured.Profile, Status: availability.Status, CooldownUntil: cooldownUntil,
				LastError: &lastError, LastRunID: &lastRunID,
			}); healthErr != nil {
				countFailure := false
				_, _ = opened.FailRun(durable, scope, "Unable to record agent availability: "+healthErr.Error(), store.FailRunOptions{
					Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
					CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
				})
				cancel()
				return
			}
			if preserveIfChanged(durable, fmt.Sprintf("Agent %s became %s after work began", configured.Profile, availability.Status), execution.Code) {
				cancel()
				return
			}
			next, fallbackErr := resolveRunProfile(durable, manager, opened, prepared.Task.Task, options)
			fallbackAvailable := fallbackErr == nil && next.Name != configured.Profile
			countFailure := false
			cooldownSeconds := 0
			if availability.Status == model.AgentHealthRateLimited && !fallbackAvailable {
				cooldownSeconds = int(options.RateLimitCooldown.Seconds())
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: availability.Outcome,
				CountFailure: &countFailure, CooldownSeconds: cooldownSeconds, FailureLimit: options.FailureLimit})
			cancel()
			if fallbackAvailable {
				options.log("requeued %s for fallback %s after %s became %s", taskID, next.Name, configured.Profile, availability.Status)
			} else {
				options.log("paused %s because %s is %s and no fallback is available", taskID, configured.Profile, availability.Status)
			}
			return
		case execution.SpawnError != nil:
			if preserveIfChanged(durable, "Runner could not restart after work began: "+detail, execution.Code) {
				cancel()
				return
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			cancel()
			return
		case execution.Code != 0:
			if preserveIfChanged(durable, "Runner exited unsuccessfully after work began: "+detail, execution.Code) {
				cancel()
				return
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{FailureLimit: options.FailureLimit})
			cancel()
			return
		case !goalMode:
			if preserveIfChanged(durable, "Runner exited without reporting a terminal outcome after work began", execution.Code) {
				cancel()
				return
			}
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusProtocolViolation, FailureLimit: options.FailureLimit})
			cancel()
			return
		}
		if _, err := opened.PauseGoalRun(durable, scope, turn); err != nil {
			cancel()
			return
		}
		if sessionID == "" {
			sessionID = execution.SessionID
		}
		if sessionID == "" && prepared.Task.Task.Runtime != model.RuntimeCline {
			reason := "Goal-mode runner did not report a resumable session id"
			if !preserveIfChanged(durable, reason, execution.Code) {
				_, _ = opened.FailRun(durable, scope, reason, store.FailRunOptions{Outcome: model.RunStatusProtocolViolation, FailureLimit: options.FailureLimit})
			}
			cancel()
			return
		}
		var judgment orchestration.GoalJudgment
		if options.GoalJudge != nil {
			judgment, err = options.GoalJudge(durable, currentDetail, turn, execution.Output)
		} else {
			judgment, err = orchestration.JudgeGoalProgress(durable, currentDetail, turn, execution.Output, goalPlanner)
		}
		if err != nil {
			if _, requestErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: "Goal judge failed: " + err.Error(), Kind: model.BlockKindTransient}); requestErr == nil {
				_, _ = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			cancel()
			return
		}
		_, _ = opened.RecordGoalJudgment(durable, scope, store.GoalJudgment{Turn: turn, Complete: judgment.Complete, Reason: judgment.Reason, NextPrompt: judgment.NextPrompt})
		if judgment.Complete {
			if _, requestErr := opened.CompleteRun(durable, scope, store.CompletionInput{Summary: fmt.Sprintf("Goal accepted after %d turn(s): %s", turn, judgment.Reason), Metadata: map[string]any{"goalMode": true, "turns": turn, "judgeReason": judgment.Reason}}); requestErr == nil {
				_, _ = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			cancel()
			cleanupIfDone()
			return
		}
		if turn >= prepared.Task.Task.GoalMaxTurns {
			if _, requestErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: fmt.Sprintf("Goal turn budget exhausted after %d turns: %s", turn, judgment.Reason), Kind: model.BlockKindNeedsInput}); requestErr == nil {
				_, _ = finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			}
			cancel()
			return
		}
		cancel()
		turn++
		continuation = judgment.NextPrompt
		if continuation == "" {
			continuation = "Continue toward the task acceptance criteria. Address this gap: " + judgment.Reason
		}
	}
}

func selectedBoards(ctx context.Context, manager *boards.Manager, requested string) ([]string, error) {
	if requested != "" {
		board, err := manager.Resolve(requested)
		if err != nil {
			return nil, err
		}
		return []string{board}, nil
	}
	metadata, err := manager.List(ctx, false)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(metadata))
	for _, board := range metadata {
		if !board.Archived {
			result = append(result, board.Slug)
		}
	}
	return result, nil
}

func maintainBoards(ctx context.Context, manager *boards.Manager, boardSlugs []string, options Options) error {
	for _, board := range boardSlugs {
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			return err
		}
		if _, err := opened.ClearExpiredAgentCooldowns(ctx, time.Now()); err != nil {
			opened.Close()
			return err
		}
		if _, err := opened.PromoteDueTasks(ctx, board, time.Now()); err != nil {
			opened.Close()
			return err
		}
		if err := recoverAbandonedRuns(ctx, opened, board, options); err != nil {
			opened.Close()
			return err
		}
		if err := opened.Close(); err != nil {
			return err
		}
	}
	return nil
}

func Run(ctx context.Context, options Options) (runErr error) {
	options.normalize()
	if options.DBPath == "" || options.CLIPath == "" {
		return errors.New("dispatcher requires DBPath and CLIPath")
	}
	manager, err := boards.NewManager(options.DBPath)
	if err != nil {
		return err
	}
	ctx, cancelDispatcher := context.WithCancel(ctx)
	var leader *supervisorLease
	if !options.Once {
		leader, err = startSupervisorLease(ctx, cancelDispatcher, manager)
		if err != nil {
			cancelDispatcher()
			return err
		}
	}
	processes := NewProcessSet()
	var active atomic.Int32
	var workers sync.WaitGroup
	workerFinished := make(chan struct{}, 1)
	generatedClineApprovalDir := ""
	defer func() {
		cancelDispatcher()
		processes.StopAll()
		workers.Wait()
		leader.Close()
		if runErr == nil && leader != nil {
			runErr = leader.Err()
		}
		if generatedClineApprovalDir != "" {
			_ = os.RemoveAll(generatedClineApprovalDir)
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		boardSlugs, err := selectedBoards(ctx, manager, options.Board)
		if err != nil {
			return err
		}
		deliverBoardNotifications(ctx, manager, boardSlugs, options)
		decomposeBoardTriage(ctx, manager, boardSlugs, options)
		if err := maintainBoards(ctx, manager, boardSlugs, options); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		launched := false
		foundInPass := true
		for ctx.Err() == nil && int(active.Load()) < options.MaxWorkers && foundInPass {
			foundInPass = false
			for _, board := range boardSlugs {
				if ctx.Err() != nil || int(active.Load()) >= options.MaxWorkers {
					break
				}
				opened, err := manager.OpenStore(ctx, board)
				if err != nil {
					return err
				}
				excluded, profileLimits, err := claimProfilePolicy(ctx, manager, opened, board, options)
				if err != nil {
					opened.Close()
					return err
				}
				claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: options.TaskID, Board: board, WorkerID: fmt.Sprintf("dispatcher-%d", os.Getpid()), ExcludeManual: true,
					ClaimTTLSeconds: options.ClaimTTLSeconds, MaxInProgress: options.MaxInProgress, MaxInProgressPerAssignee: options.MaxInProgressPerAssignee,
					MaxInProgressByAssignee: profileLimits, ExcludedAssignees: excluded})
				if err != nil {
					opened.Close()
					return err
				}
				if claim == nil {
					opened.Close()
					continue
				}
				approvalDir := options.ClineApprovalDir
				if claim.Task.Task.Runtime == model.RuntimeCline && approvalDir == "" {
					if generatedClineApprovalDir == "" {
						generatedClineApprovalDir, err = os.MkdirTemp("", "autogora-cline-approvals-")
						if err != nil {
							opened.Close()
							return err
						}
					}
					approvalDir = generatedClineApprovalDir
				}
				foundInPass, launched = true, true
				active.Add(1)
				workers.Add(1)
				go func(opened *store.Store, claim *model.ClaimedTask, approvalDir string) {
					defer workers.Done()
					defer func() {
						select {
						case workerFinished <- struct{}{}:
						default:
						}
					}()
					defer active.Add(-1)
					defer opened.Close()
					runClaim(ctx, manager, opened, claim, options, processes, approvalDir)
				}(opened, claim, approvalDir)
				if options.Once {
					break
				}
			}
			if options.Once && launched {
				break
			}
		}
		if options.Once {
			for active.Load() > 0 {
				timer := time.NewTimer(options.Interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					processes.StopAll()
					workers.Wait()
					return nil
				case <-workerFinished:
					timer.Stop()
				case <-timer.C:
					if err := maintainBoards(ctx, manager, boardSlugs, options); err != nil {
						if ctx.Err() != nil {
							return nil
						}
						return err
					}
				}
			}
			workers.Wait()
			deliverBoardNotifications(ctx, manager, boardSlugs, options)
			return nil
		}
		timer := time.NewTimer(options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			processes.StopAll()
			workers.Wait()
			return nil
		case <-timer.C:
		}
	}
}
