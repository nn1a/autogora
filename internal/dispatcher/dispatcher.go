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
	"github.com/nn1a/autogora/internal/agentcapacity"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/agenthealth"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/notifications"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

type GoalJudge func(context.Context, model.TaskDetail, int, string) (orchestration.GoalJudgment, error)
type AgentConfigLoader func() (agentconfig.Config, error)

type Options struct {
	DBPath                   string
	CLIPath                  string
	Board                    string
	TaskID                   string
	ExpectedUpdatedAt        *string
	Once                     bool
	Interval                 time.Duration
	MaxWorkers               int
	MaxInProgress            int
	MaxInProgressPerAssignee int
	ClaimTTLSeconds          int
	StaleTimeout             time.Duration
	HeartbeatMaxStale        time.Duration
	CrashGrace               *time.Duration
	TerminationGrace         time.Duration
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
	FinalizerProfile         *orchestration.ProfileRoute
	PlannerRuntime           model.Runtime
	PlannerModel             string
	PlannerProvider          string
	PlannerTimeout           time.Duration
	PlanningShutdownGrace    time.Duration
	DecompositionPlanner     orchestration.Planner
	CoordinatorPlanner       orchestration.Planner
	PublicationExecutor      PublicationExecutor
	PublicationTimeout       time.Duration
	PublicationClaimTTL      time.Duration
	AllowWrites              bool
	Autopilot                bool
	ClineApprovalDir         string
	WorkingDirectory         string
	Getenv                   func(string) string
	Now                      func() time.Time
	AgentConfig              *agentconfig.Config
	AgentConfigLoader        AgentConfigLoader
	OnLog                    func(string)
	testHooks                *dispatcherTestHooks
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
	if o.TerminationGrace <= 0 {
		o.TerminationGrace = 15 * time.Second
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
	} else if o.PlannerTimeout > 10*time.Minute {
		o.PlannerTimeout = 10 * time.Minute
	}
	if o.PlanningShutdownGrace <= 0 {
		o.PlanningShutdownGrace = 2 * time.Second
	}
	if o.PublicationTimeout <= 0 {
		o.PublicationTimeout = 2 * time.Minute
	}
	maxPublicationTimeout := store.MaxPublicationClaimTTL - publicationClaimGrace
	if o.PublicationTimeout > maxPublicationTimeout {
		o.PublicationTimeout = maxPublicationTimeout
	}
	minimumPublicationClaimTTL := o.PublicationTimeout + publicationClaimGrace
	if minimumPublicationClaimTTL < store.MinPublicationClaimTTL {
		minimumPublicationClaimTTL = store.MinPublicationClaimTTL
	}
	if o.PublicationClaimTTL < minimumPublicationClaimTTL {
		o.PublicationClaimTTL = minimumPublicationClaimTTL
	}
	if o.PublicationClaimTTL > store.MaxPublicationClaimTTL {
		o.PublicationClaimTTL = store.MaxPublicationClaimTTL
	}
	if o.PublicationExecutor == nil {
		o.PublicationExecutor = publisher.Execute
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
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

func (o Options) currentTime() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
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

func preservedWorkspaceReason(ctx context.Context, workspaces *workspace.Manager, bound *model.RunWorkspace, reason, unsafeReason string) (string, bool) {
	if bound == nil {
		return reason, false
	}
	inspection := workspace.ChangeInspection{}
	var inspectErr error
	if bound.Kind == model.WorkspaceDir {
		// A shared directory has no per-run baseline. Once a writable agent
		// starts, a different agent must not overwrite an uncertain result.
		inspectErr = errors.New("shared directory work cannot be attributed safely to one run")
	} else {
		inspection, inspectErr = workspaces.InspectChanges(ctx, *bound)
	}
	if !inspection.Changed && inspectErr == nil && strings.TrimSpace(unsafeReason) == "" {
		return reason, false
	}
	reason += "; partial changes remain at " + bound.Path
	if inspection.HeadCommit != "" {
		reason += "; current HEAD " + inspection.HeadCommit
	}
	if inspection.Changed {
		reason += "; the workspace differs from its recorded starting state"
	}
	if inspectErr != nil {
		reason += "; Autogora could not safely verify the workspace state: " + inspectErr.Error()
	}
	if value := strings.TrimSpace(unsafeReason); value != "" {
		reason += "; Autogora could not safely verify that writes had stopped: " + value
	}
	reason += "; inspect and integrate or discard this work before unblocking the task"
	return reason, true
}

func recoverRunWithWorkspaceProtection(ctx context.Context, opened *store.Store, workspaces *workspace.Manager, runID, taskID string,
	outcome model.RunStatus, reason string, countFailure bool, unsafeReason string, options Options,
) error {
	allowWrites, err := opened.GetManagedRunWritePolicy(ctx, runID)
	if err != nil {
		return err
	}
	if allowWrites != nil && !*allowWrites {
		return recoverRunDurably(ctx, opened, runID, outcome, reason, countFailure)
	}
	bound, err := opened.GetRunWorkspace(ctx, runID)
	if err != nil {
		return err
	}
	preservedReason, preserve := preservedWorkspaceReason(ctx, workspaces, bound, reason, unsafeReason)
	if !preserve {
		return recoverRunDurably(ctx, opened, runID, outcome, reason, countFailure)
	}
	if err = recoverRunBlockedDurably(ctx, opened, runID, store.RecoverBlockedRunInput{
		Outcome: outcome, Reason: preservedReason, Kind: model.BlockKindNeedsInput,
	}); err != nil {
		return err
	}
	options.log("preserved abandoned-run work for %s at %s", taskID, bound.Path)
	return nil
}

func recoverAbandonedRuns(ctx context.Context, opened *store.Store, board string, options Options) error {
	active, err := opened.ListActiveRuns(ctx, board)
	if err != nil {
		return err
	}
	workspaces := workspace.New(nil)
	now := time.Now()
	for _, item := range active {
		deferred, err := opened.GetDeferredReclaim(ctx, item.Run.ID)
		if err != nil {
			return err
		}
		elapsed := now.Sub(parseTimestamp(item.Run.ClaimedAt))
		heartbeatAge := now.Sub(parseTimestamp(item.Run.HeartbeatAt))
		expired := !now.Before(parseTimestamp(item.Run.ClaimExpiresAt))
		stale := elapsed >= options.StaleTimeout && heartbeatAge >= options.HeartbeatMaxStale
		timedOut := item.Task.MaxRuntimeSeconds != nil && elapsed >= time.Duration(*item.Task.MaxRuntimeSeconds)*time.Second
		var processIdentity *string
		processState := processidentity.State{}
		if item.Run.PID != nil {
			processIdentity, err = opened.GetRunProcessIdentity(ctx, item.Run.ID)
			if err != nil {
				return err
			}
			processState = processidentity.Inspect(*item.Run.PID, processIdentity)
		}
		identityMatches := processState.Alive && processState.Verified && processState.Matches
		identityMismatch := processState.Alive && processState.Verified && !processState.Matches
		// Dispatcher workers use a dedicated process group on Unix. The leader
		// can exit before descendants, so conservatively keep ownership while
		// that group still exists. This check never sends a signal; a reused
		// group ID can delay recovery but cannot terminate an unrelated process.
		descendantsAlive := !processState.Alive && runcontrol.ProcessTreeAlive(item.Run.PID)
		// An occupied PID with no verifiable identity may still be the worker,
		// so it keeps ownership. It is never safe to signal or force-kill it.
		workerAlive := (processState.Alive && !identityMismatch) || descendantsAlive
		crashed := item.Run.PID != nil && elapsed >= *options.CrashGrace && !workerAlive
		deferredExpired := false
		if deferred != nil {
			deferredExpired = !now.Before(parseTimestamp(deferred.ExpiresAt))
		}
		deferTermination := func(reason string, timedOut bool) error {
			signaled := runcontrol.SignalRunProcess(item.Run.PID, processIdentity)
			if processState.Alive && !signaled {
				reason += "; recorded PID identity could not be verified; no signal was sent"
			}
			seconds := max(1, int(options.TerminationGrace.Seconds()))
			if timedOut {
				_, err = opened.DeferTimedOutRun(ctx, item.Run.ID, seconds, reason)
			} else {
				_, err = opened.DeferReclaim(ctx, item.Run.ID, seconds, reason)
			}
			return err
		}
		forceTermination := func() error {
			forced := runcontrol.ForceKillRunProcess(item.Run.PID, processIdentity)
			reason := deferred.Reason
			if forced {
				reason += "; force termination sent after grace period"
			} else {
				reason += "; force termination could not be delivered"
			}
			seconds := max(1, int(options.TerminationGrace.Seconds()))
			if deferred.Outcome == model.RunStatusTimedOut {
				_, err = opened.DeferTimedOutRun(ctx, item.Run.ID, seconds, reason)
			} else {
				_, err = opened.DeferReclaim(ctx, item.Run.ID, seconds, reason)
			}
			return err
		}
		recoverDeferred := func() error {
			return recoverRunWithWorkspaceProtection(ctx, opened, workspaces, item.Run.ID, item.Task.ID,
				deferred.Outcome, deferred.Reason, deferred.CountFailure, "", options)
		}
		switch {
		case timedOut:
			if workerAlive {
				if deferred == nil || deferred.Outcome != model.RunStatusTimedOut {
					reason := fmt.Sprintf("Maximum runtime exceeded after %d seconds", int(elapsed.Seconds()))
					if err := deferTermination(reason, true); err != nil {
						return err
					}
					options.log("requested timeout termination for PID %d on %s", *item.Run.PID, item.Task.ID)
				} else if deferredExpired && identityMatches {
					if err := forceTermination(); err != nil {
						return err
					}
					options.log("escalated timeout termination for PID %d on %s", *item.Run.PID, item.Task.ID)
				} else if deferredExpired {
					options.log("refused timeout escalation for unverified PID %d on %s", *item.Run.PID, item.Task.ID)
				}
				continue
			}
			if err := recoverRunWithWorkspaceProtection(ctx, opened, workspaces, item.Run.ID, item.Task.ID, model.RunStatusTimedOut,
				fmt.Sprintf("Maximum runtime exceeded after %d seconds", int(elapsed.Seconds())), true, "", options); err != nil {
				return err
			}
			options.log("timed out %s", item.Task.ID)
		case crashed && deferred != nil:
			if err := recoverDeferred(); err != nil {
				return err
			}
			options.log("recovered %s after requested termination", item.Task.ID)
		case crashed:
			reason := fmt.Sprintf("Worker PID %d is no longer alive", *item.Run.PID)
			if identityMismatch {
				reason = fmt.Sprintf("Recorded worker PID %d now belongs to a different process; Autogora did not signal it", *item.Run.PID)
			}
			if err := recoverRunWithWorkspaceProtection(ctx, opened, workspaces, item.Run.ID, item.Task.ID, model.RunStatusCrashed,
				reason, true, "", options); err != nil {
				return err
			}
			options.log("reclaimed crashed worker %s", item.Task.ID)
		case expired || stale:
			if workerAlive {
				if deferred == nil {
					reason := "Claim TTL expired"
					if stale {
						reason = "Heartbeat became stale"
					}
					if err := deferTermination(reason, false); err != nil {
						return err
					}
					options.log("deferred reclaim while terminating PID %d for %s", *item.Run.PID, item.Task.ID)
				} else if deferredExpired && identityMatches {
					if err := forceTermination(); err != nil {
						return err
					}
					options.log("escalated stale-worker termination for PID %d on %s", *item.Run.PID, item.Task.ID)
				} else if deferredExpired {
					options.log("refused stale-worker escalation for unverified PID %d on %s", *item.Run.PID, item.Task.ID)
				}
				continue
			}
			if deferred != nil {
				if err := recoverDeferred(); err != nil {
					return err
				}
				options.log("recovered %s after requested termination", item.Task.ID)
			} else {
				reason := "Claim TTL expired"
				if stale {
					reason = "Heartbeat became stale"
				}
				if err := recoverRunWithWorkspaceProtection(ctx, opened, workspaces, item.Run.ID, item.Task.ID,
					model.RunStatusReclaimed, reason, false, "", options); err != nil {
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

func cloneAgentConfig(config agentconfig.Config) agentconfig.Config {
	cloned := config
	cloned.Defaults.WorkerAgents = append([]string(nil), config.Defaults.WorkerAgents...)
	cloned.Defaults.PlannerAgents = append([]string(nil), config.Defaults.PlannerAgents...)
	cloned.Defaults.CoordinatorAgents = append([]string(nil), config.Defaults.CoordinatorAgents...)
	cloned.Defaults.JudgeAgents = append([]string(nil), config.Defaults.JudgeAgents...)
	cloned.Agents = append([]agentconfig.Agent(nil), config.Agents...)
	for index := range cloned.Agents {
		cloned.Agents[index].Roles = append(
			[]agentconfig.Role(nil),
			config.Agents[index].Roles...,
		)
		cloned.Agents[index].Fallbacks = append(
			[]string(nil),
			config.Agents[index].Fallbacks...,
		)
	}
	return cloned
}

func configuredProfiles(manager *boards.Manager, board string, options Options) (configuredProfileSet, error) {
	metadata, err := manager.Read(board)
	if err != nil {
		return configuredProfileSet{}, err
	}
	config := agentconfig.Default()
	if options.AgentConfigLoader != nil {
		config, err = options.AgentConfigLoader()
		if err != nil {
			return configuredProfileSet{}, fmt.Errorf("load live agent configuration: %w", err)
		}
		config = cloneAgentConfig(config)
		config = agentconfig.Normalize(config)
		if err := agentconfig.Validate(config); err != nil {
			return configuredProfileSet{}, fmt.Errorf("validate live agent configuration: %w", err)
		}
	} else if options.AgentConfig != nil {
		config = agentconfig.Normalize(cloneAgentConfig(*options.AgentConfig))
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

func claimProfilePolicy(
	ctx context.Context,
	manager *boards.Manager,
	opened *store.Store,
	board string,
	options Options,
) (excluded []string, limits map[string]int, returnErr error) {
	configured, err := configuredProfiles(manager, board, options)
	if err != nil {
		return nil, nil, err
	}
	excluded = make([]string, 0)
	limits = map[string]int{}
	healthRouter := agenthealth.New(manager, opened)
	if opened.Board() != "default" {
		coordinationStore, err := manager.OpenCoordinationStore(ctx)
		if err != nil {
			return nil, nil, markGlobalCoordinationError("open shared agent-health store", err)
		}
		defer func() {
			if closeErr := coordinationStore.Close(); closeErr != nil {
				returnErr = errors.Join(
					returnErr,
					markGlobalCoordinationError("close shared agent-health store", closeErr),
				)
			}
		}()
		healthRouter = agenthealth.NewWithGlobal(manager, opened, coordinationStore)
	}
	for _, profile := range configured.Profiles {
		if _, available, availabilityErr := selectAvailableProfile(
			ctx, healthRouter, opened, profile.Name, configured.Profiles, configured.Config,
		); availabilityErr != nil {
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

func selectAvailableProfile(
	ctx context.Context,
	healthRouter agenthealth.Router,
	opened *store.Store,
	desired string,
	profiles []orchestration.ProfileRoute,
	config agentconfig.Config,
) (orchestration.ProfileRoute, bool, error) {
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
		globalRegistered := registeredAgentHasRole(config, candidate.Name, agentconfig.RoleWorker)
		health, err := healthRouter.Get(ctx, candidate.Name, globalRegistered)
		if err != nil {
			if globalRegistered {
				return orchestration.ProfileRoute{}, false, markGlobalCoordinationError(
					"read shared agent health for "+candidate.Name, err,
				)
			}
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
	Source           string
	FallbackFrom     *string
	Command          string
	GlobalRegistered bool
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
	selected, available, err := selectAvailableProfile(
		ctx, agenthealth.New(manager, opened), opened, name, profiles, configured.Config,
	)
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
	registered, globalRegistered := configured.Config.Find(selected.Name)
	globalRegistered = globalRegistered && registered.Enabled && hasAgentRole(registered, agentconfig.RoleWorker)
	resolved := resolvedRunProfile{
		ProfileRoute: selected, Source: source, Command: configured.Commands[selected.Name],
		GlobalRegistered: globalRegistered,
	}
	if selected.Name != name {
		resolved.FallbackFrom = &name
	}
	return resolved, nil
}

func registeredAgentHasRole(config agentconfig.Config, name string, role agentconfig.Role) bool {
	agent, found := config.Find(strings.TrimSpace(name))
	return found && hasAgentRole(agent, role)
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

type autoDecomposeDiagnostics struct {
	skippedGitHubImports map[string]struct{}
	triageCursors        map[string]store.TaskListCursor
	nextPlanningBoard    string
}

func isGitHubImportedTask(task model.Task) bool {
	return task.IdempotencyKey != nil && strings.HasPrefix(strings.TrimSpace(*task.IdempotencyKey), "github-issue:")
}

func isRepeatedBlockTriage(task model.Task) bool {
	return task.Status == model.TaskStatusTriage && task.BlockReason != nil && task.BlockRecurrences >= 2
}

func (d *autoDecomposeDiagnostics) reportGitHubImportSkip(options Options, board string, task model.Task) {
	if d == nil {
		return
	}
	if d.skippedGitHubImports == nil {
		d.skippedGitHubImports = make(map[string]struct{})
	}
	key := board + "\x00" + task.ID
	if _, reported := d.skippedGitHubImports[key]; reported {
		return
	}
	if len(d.skippedGitHubImports) >= autoDecomposeDiagnosticEntries {
		for candidate := range d.skippedGitHubImports {
			delete(d.skippedGitHubImports, candidate)
			break
		}
	}
	d.skippedGitHubImports[key] = struct{}{}
	options.log("auto-decompose skipped imported GitHub task %s; use Specify, Decompose, or Promote after review", task.ID)
}

func decomposeBoardTriage(ctx context.Context, manager *boards.Manager, boardSlugs []string, options Options, diagnostics *autoDecomposeDiagnostics) {
	remaining := options.AutoDecomposePerTick
	if remaining <= 0 {
		remaining = 500
	}
	orderedBoards := diagnostics.orderedPlanningBoards(boardSlugs)
	for _, board := range orderedBoards {
		if remaining <= 0 || ctx.Err() != nil {
			return
		}
		metadata, err := manager.Read(board)
		if err != nil {
			options.log("auto-decompose metadata failed %s: %v", board, err)
			continue
		}
		if !autoDecomposeEnabled(metadata, options) {
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
		plannerRuntime, _, _, _ := plannerConfiguration(metadata, configured, options)
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			options.log("auto-decompose store failed %s: %v", board, err)
			continue
		}
		planner := options.DecompositionPlanner
		discovered, discoverErr := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
		if discoverErr != nil {
			options.log("auto-decompose list failed %s: %v", board, discoverErr)
			opened.Close()
			continue
		}
		decompositionProfiles := mergeDecompositionProfiles(configured, options.DecompositionProfiles)
		cursor := diagnostics.triageCursor(board)
		scanned := 0
		plannerSetupFailed := false
		for remaining > 0 && boardRemaining > 0 && scanned < autoDecomposeCandidateScanLimit {
			pageLimit := min(autoDecomposeCandidatePageSize, autoDecomposeCandidateScanLimit-scanned)
			triage, listErr := opened.ListTasks(ctx, store.ListTaskFilter{
				Status: model.TaskStatusTriage, Sort: "priority-desc", Limit: pageLimit, After: cursor,
			})
			if listErr != nil {
				options.log("auto-decompose list failed %s: %v", board, listErr)
				break
			}
			if len(triage) == 0 {
				cursor = nil
				diagnostics.setTriageCursor(board, nil)
				break
			}
			reachedEnd := len(triage) < pageLimit
			for _, task := range triage {
				scanned++
				nextCursor := store.TaskListCursor{Priority: task.Priority, CreatedAt: task.CreatedAt, ID: task.ID}
				cursor = &nextCursor
				diagnostics.setTriageCursor(board, cursor)
				// Imported tasks remain in Triage until a user explicitly
				// reviews them. Imported and cooling-down candidates count
				// only toward the bounded scan, never the planning quota.
				if isGitHubImportedTask(task) {
					diagnostics.reportGitHubImportSkip(options, board, task)
					continue
				}
				// A repeated block is an exceptional recovery incident, not a
				// new rough idea. Keep it for Coordinator/user review instead
				// of asking Planner to overwrite its existing specification.
				if isRepeatedBlockTriage(task) {
					continue
				}
				claimTTL := autoDecomposeClaimTTL(options)
				decision, claimErr := opened.ClaimAutoDecompose(
					ctx,
					task.ID,
					store.AutoDecomposeMaxAttempts,
					claimTTL,
					options.currentTime(),
				)
				if claimErr != nil {
					options.log("auto-decompose claim failed %s: %v", task.ID, claimErr)
					continue
				}
				if decision.Claim == nil {
					continue
				}
				planningClaim := *decision.Claim
				if planner == nil {
					planner, err = createRolePlanner(
						manager,
						opened,
						metadata,
						configured,
						options,
						agentconfig.RolePlanner,
						options.WorkingDirectory,
					)
					if err != nil {
						setupErr := fmt.Errorf("configure Planner: %w", err)
						shouldStop := failAutoDecomposeClaim(
							ctx, opened, planningClaim, setupErr, options,
						)
						diagnostics.advancePlanningBoard(orderedBoards, board)
						remaining--
						boardRemaining--
						plannerSetupFailed = true
						if shouldStop {
							opened.Close()
							return
						}
						break
					}
				}
				// The board roster is intentionally bounded. Include the current
				// candidate so a task beyond that snapshot keeps its explicit route.
				taskRoster := make([]model.Task, len(discovered)+1)
				copy(taskRoster, discovered)
				taskRoster[len(discovered)] = task
				profiles := orchestration.ResolveProfileRoutes(taskRoster, decompositionProfiles)
				defaultName, finalizerName := metadata.Orchestration.DefaultProfile, metadata.Orchestration.FinalizerProfile
				if defaultName == nil {
					if globalDefault, found := firstConfiguredAgent(configured.Config, configured.DefaultWorkers, agentconfig.RoleWorker); found {
						value := globalDefault.ID
						defaultName = &value
					}
				}
				fallback, finalizer := orchestration.SelectProfileRoutes(profiles, defaultName, finalizerName, plannerRuntime)
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
				if options.FinalizerProfile != nil {
					finalizer = *options.FinalizerProfile
				} else if metadata.Orchestration.FinalizerProfile == nil && fallback.Name != finalizer.Name {
					finalizer = fallback
				}
				value := metadata.Orchestration.AutoPromoteChildren
				leaseGuard := startAutoDecomposeLeaseGuard(
					ctx, opened, planningClaim, claimTTL, options,
				)
				result, err := orchestration.DecomposeTriageTask(leaseGuard.ctx, opened, task.ID, orchestration.DecomposeOptions{
					Profiles: profiles, DefaultProfile: fallback, FinalizerProfile: &finalizer,
					AutoPromoteChildren: &value, AutoDecomposeClaim: &planningClaim,
					Planner: leaseGuard.planner(planner),
				})
				if err != nil {
					leaseGuard.stopHeartbeat()
					shouldStop := failAutoDecomposeClaim(
						ctx, opened, planningClaim, err, options,
					)
					leaseGuard.Stop()
					if shouldStop {
						opened.Close()
						return
					}
				} else {
					leaseGuard.Stop()
					action := "specified"
					if result.Fanout {
						action = "decomposed"
					}
					options.log("auto-%s %s: %s", action, task.ID, result.Reason)
				}
				diagnostics.advancePlanningBoard(orderedBoards, board)
				remaining--
				boardRemaining--
				if remaining <= 0 || boardRemaining <= 0 {
					break
				}
			}
			if remaining <= 0 || boardRemaining <= 0 {
				break
			}
			if plannerSetupFailed {
				break
			}
			if reachedEnd {
				cursor = nil
				diagnostics.setTriageCursor(board, nil)
				break
			}
		}
		opened.Close()
	}
}

func durableContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

const statePersistenceAttempts = 3

func transientStoreError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	message := strings.ToLower(err.Error())
	return containsAny(message, "database is locked", "database table is locked", "sqlite_busy", "sqlite_locked")
}

func retryStoreOperation[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	var zero T
	delay := 25 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < statePersistenceAttempts; attempt++ {
		value, err := operation()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !transientStoreError(err) || attempt == statePersistenceAttempts-1 {
			return zero, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
		delay *= 2
	}
	return zero, lastErr
}

func retryStoreAction(ctx context.Context, operation func() error) error {
	_, err := retryStoreOperation(ctx, func() (struct{}, error) {
		return struct{}{}, operation()
	})
	return err
}

func failRunDurably(ctx context.Context, opened *store.Store, scope store.RunScope, runError string, options store.FailRunOptions) error {
	_, err := retryStoreOperation(ctx, func() (model.TaskDetail, error) {
		return opened.FailRun(ctx, scope, runError, options)
	})
	if err == nil {
		return nil
	}
	// A write can commit successfully and still lose its response. Reconcile
	// that ambiguous case before reporting a persistence failure.
	inspection, inspectErr := retryStoreOperation(ctx, func() (store.RunInspection, error) {
		return opened.GetRun(ctx, scope.RunID)
	})
	expected := options.Outcome
	if expected == "" {
		expected = model.RunStatusFailed
	}
	if inspectErr == nil && inspection.Run.Status == expected && inspection.Task.CurrentRunID == nil &&
		inspection.Run.Error != nil && *inspection.Run.Error == runError {
		return nil
	}
	return errors.Join(err, inspectErr)
}

func recoverRunDurably(ctx context.Context, opened *store.Store, runID string, outcome model.RunStatus, reason string, countFailure bool) error {
	_, err := retryStoreOperation(ctx, func() (model.TaskDetail, error) {
		return opened.RecoverAbandonedRun(ctx, runID, outcome, reason, countFailure)
	})
	return err
}

func recoverRunBlockedDurably(ctx context.Context, opened *store.Store, runID string, input store.RecoverBlockedRunInput) error {
	_, err := retryStoreOperation(ctx, func() (model.TaskDetail, error) {
		return opened.RecoverRunBlocked(ctx, runID, input)
	})
	return err
}

func finalizeManagedTerminal(ctx context.Context, opened *store.Store, workspaces *workspace.Manager, prepared *model.ClaimedTask, scope store.RunScope, exitCode int) (model.TaskDetail, error) {
	request, err := retryStoreOperation(ctx, func() (*model.TerminalRequest, error) {
		return opened.GetRunTerminalRequest(ctx, scope.RunID)
	})
	if err != nil {
		return model.TaskDetail{}, err
	}
	if request == nil {
		return model.TaskDetail{}, fmt.Errorf("run has no terminal request: %s", scope.RunID)
	}
	if request.Kind == "complete" && prepared.Workspace != nil && prepared.Workspace.Kind == model.WorkspaceWorktree {
		existing, err := retryStoreOperation(ctx, func() (*model.ChangeSet, error) {
			return opened.GetRunChangeSet(ctx, scope.RunID)
		})
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
			if _, err := retryStoreOperation(ctx, func() (model.ChangeSet, error) {
				return opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
					RunID: scope.RunID, RepositoryPath: snapshot.RepositoryPath, WorktreePath: snapshot.WorktreePath,
					BaseCommit: snapshot.BaseCommit, HeadCommit: snapshot.HeadCommit, DurableRef: snapshot.DurableRef,
					State: snapshot.State, ChangedFiles: snapshot.ChangedFiles,
				})
			}); err != nil {
				return model.TaskDetail{}, err
			}
		}
	}
	return retryStoreOperation(ctx, func() (model.TaskDetail, error) {
		return opened.FinalizeRunTerminal(ctx, scope.RunID, exitCode)
	})
}

func runClaim(ctx context.Context, manager *boards.Manager, opened *store.Store, claim *model.ClaimedTask, options Options, processes *ProcessSet, clineApprovalDir string) (runErr error) {
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	var agentLease *agentcapacity.Lease
	defer func() {
		if agentLease == nil {
			return
		}
		durable, cancel := durableContext()
		defer cancel()
		terminal, err := retryStoreOperation(durable, func() (bool, error) {
			return opened.IsRunTerminal(durable, scope.RunID)
		})
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("verify terminal run before releasing agent capacity for %s: %w", claim.Task.Task.ID, err))
			return
		}
		if !terminal {
			return
		}
		inspection, err := retryStoreOperation(durable, func() (store.RunInspection, error) {
			return opened.GetRun(durable, scope.RunID)
		})
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("inspect terminal process before releasing agent capacity for %s: %w", claim.Task.Task.ID, err))
			return
		}
		processIdentity, err := opened.GetRunProcessIdentity(durable, scope.RunID)
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("read terminal process identity before releasing agent capacity for %s: %w", claim.Task.Task.ID, err))
			return
		}
		if runcontrol.ProcessMayStillBeRunning(inspection.Run.PID, processIdentity) {
			return
		}
		if err := agentLease.Release(durable); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("release agent capacity for %s: %w", claim.Task.Task.ID, err))
		}
	}()
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, options.AllowWrites); err != nil {
		durable, cancel := durableContext()
		defer cancel()
		if persistErr := failRunDurably(durable, opened, scope, "Unable to register dispatcher ownership: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit}); persistErr != nil {
			return fmt.Errorf("persist dispatcher ownership failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
	}
	profile, err := resolveRunProfile(ctx, manager, opened, claim.Task.Task, options)
	if err != nil {
		durable, cancel := durableContext()
		defer cancel()
		countFailure := false
		if persistErr := failRunDurably(durable, opened, scope, "Agent profile resolution failed: "+err.Error(), store.FailRunOptions{
			Outcome: model.RunStatusReclaimed, CountFailure: &countFailure, CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
		}); persistErr != nil {
			return fmt.Errorf("persist profile resolution failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
	}
	if profile.GlobalRegistered {
		var acquired bool
		agentLease, acquired, err = agentcapacity.New(manager).AcquireWorker(
			ctx, profile.Name, profile.MaxConcurrent, claim.Task.Task.Board, claim.Run.ID,
		)
		if err != nil || !acquired {
			durable, cancel := durableContext()
			defer cancel()
			countFailure := false
			reason := fmt.Sprintf("Global agent capacity is full for profile %s", profile.Name)
			if err != nil {
				reason = "Global agent capacity check failed: " + err.Error()
			}
			if persistErr := failRunDurably(durable, opened, scope, reason, store.FailRunOptions{
				Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
				CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
			}); persistErr != nil {
				return fmt.Errorf("persist global agent capacity outcome for %s: %w", claim.Task.Task.ID, errors.Join(err, persistErr))
			}
			options.log("requeued %s because global profile %s is at capacity", claim.Task.Task.ID, profile.Name)
			return nil
		}
	}
	configured, err := opened.RecordRunAgentConfig(ctx, scope, store.RecordRunAgentConfigInput{
		Profile: profile.Name, Runtime: profile.Runtime, Model: profile.Model, Provider: profile.Provider, Source: profile.Source,
		FallbackFrom: profile.FallbackFrom, AllowRuntimeOverride: profile.Runtime != claim.Run.Runtime,
	})
	if err != nil {
		durable, cancel := durableContext()
		defer cancel()
		if persistErr := failRunDurably(durable, opened, scope, "Unable to pin agent configuration: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit}); persistErr != nil {
			return fmt.Errorf("persist agent configuration failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
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
		persistErr := failRunDurably(durable, opened, scope, "Workspace preparation failed: "+err.Error(), failure)
		options.log("workspace failure %s: %v", claim.Task.Task.ID, err)
		if persistErr != nil {
			return fmt.Errorf("persist workspace preparation failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
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
				recoveryErr := recoverRunBlockedDurably(durable, opened, scope.RunID, store.RecoverBlockedRunInput{
					Outcome: model.RunStatusBlocked, Reason: integrationErr.Reason, Kind: integrationErr.BlockKind,
				})
				return fmt.Errorf("finalize prerequisite integration block for %s: %w", claim.Task.Task.ID, errors.Join(blockErr, recoveryErr))
			}
			options.log("blocked prerequisite integration for %s: %v", claim.Task.Task.ID, err)
			return nil
		}
		countFailure := false
		persistErr := failRunDurably(durable, opened, scope, "Prerequisite integration failed: "+err.Error(), store.FailRunOptions{
			Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
			CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
		})
		options.log("prerequisite integration failure %s: %v", claim.Task.Task.ID, err)
		if persistErr != nil {
			return fmt.Errorf("persist prerequisite integration failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
	}
	blockPreparedResolution := func(reason string, exitCode int) (bool, error) {
		if prepared.IntegrationResolution == nil {
			return false, nil
		}
		if prepared.Workspace != nil {
			reason += "; unresolved finalizer workspace preserved at " + prepared.Workspace.Path
		}
		durable, cancel := durableContext()
		defer cancel()
		err := recoverRunBlockedDurably(durable, opened, scope.RunID, store.RecoverBlockedRunInput{
			Outcome: model.RunStatusBlocked, Reason: reason,
			Kind: model.BlockKindNeedsInput, ExitCode: &exitCode,
		})
		return true, err
	}
	logsRoot, rootsErr := manager.LogsRoot(prepared.Task.Task.Board)
	workspaceRoot, workspaceErr := manager.WorkspaceRoot(prepared.Task.Task.Board)
	attachmentsRoot, attachmentsErr := manager.AttachmentsRoot(prepared.Task.Task.Board)
	if rootsErr != nil || workspaceErr != nil || attachmentsErr != nil {
		err := errors.Join(rootsErr, workspaceErr, attachmentsErr)
		if blocked, blockErr := blockPreparedResolution("Board path resolution failed before finalizer launch: "+err.Error(), 1); blocked {
			if blockErr != nil {
				return fmt.Errorf("preserve finalizer resolution after board path failure for %s: %w", claim.Task.Task.ID, blockErr)
			}
			return nil
		}
		durable, cancel := durableContext()
		defer cancel()
		if persistErr := failRunDurably(durable, opened, scope, "Board path resolution failed: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit}); persistErr != nil {
			return fmt.Errorf("persist board path failure for %s: %w", claim.Task.Task.ID, persistErr)
		}
		return nil
	}
	if err := os.MkdirAll(logsRoot, 0o755); err != nil {
		if blocked, blockErr := blockPreparedResolution("Log directory creation failed before finalizer launch: "+err.Error(), 1); blocked {
			if blockErr != nil {
				return fmt.Errorf("preserve finalizer resolution after log directory failure for %s: %w", prepared.Task.Task.ID, blockErr)
			}
			return nil
		}
		durable, cancel := durableContext()
		defer cancel()
		persistErr := failRunDurably(durable, opened, scope, "Log directory creation failed: "+err.Error(), store.FailRunOptions{FailureLimit: options.FailureLimit})
		options.log("log directory failure %s: %v", prepared.Task.Task.ID, err)
		if persistErr != nil {
			return fmt.Errorf("persist log directory failure for %s: %w", prepared.Task.Task.ID, persistErr)
		}
		return nil
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
	blockManagedRun := func(durable context.Context, reason string, kind model.BlockKind, exitCode int) error {
		if _, requestErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: reason, Kind: kind}); requestErr != nil {
			recoveryErr := recoverRunBlockedDurably(durable, opened, scope.RunID, store.RecoverBlockedRunInput{
				Outcome: model.RunStatusBlocked, Reason: reason, Kind: kind, ExitCode: &exitCode,
			})
			return errors.Join(requestErr, recoveryErr)
		}
		if _, finalizeErr := finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, exitCode); finalizeErr != nil {
			recoveryErr := recoverRunBlockedDurably(durable, opened, scope.RunID, store.RecoverBlockedRunInput{
				Outcome: model.RunStatusBlocked, Reason: reason, Kind: kind, ExitCode: &exitCode,
			})
			return errors.Join(finalizeErr, recoveryErr)
		}
		return nil
	}
	var goalPlanner orchestration.Planner
	if goalMode && options.GoalJudge == nil {
		metadata, metadataErr := manager.Read(prepared.Task.Task.Board)
		profileSet, profileErr := configuredProfiles(manager, prepared.Task.Task.Board, options)
		if metadataErr != nil || profileErr != nil {
			reason := "Goal judge configuration failed: " + errors.Join(metadataErr, profileErr).Error()
			if blocked, blockErr := blockPreparedResolution(reason, 1); blocked {
				if blockErr != nil {
					return fmt.Errorf("preserve finalizer resolution after goal judge configuration failure for %s: %w", taskID, blockErr)
				}
				return nil
			}
			durable, cancel := durableContext()
			defer cancel()
			if persistErr := blockManagedRun(durable, reason, model.BlockKindTransient, 0); persistErr != nil {
				return fmt.Errorf("persist goal judge configuration block for %s: %w", taskID, persistErr)
			}
			return nil
		}
		plannerCWD := options.WorkingDirectory
		if prepared.Workspace != nil {
			plannerCWD = prepared.Workspace.Path
		}
		goalPlanner, err = createRolePlanner(manager, opened, metadata, profileSet, options, agentconfig.RoleJudge, plannerCWD)
		if err != nil {
			reason := "Goal judge setup failed: " + err.Error()
			if blocked, blockErr := blockPreparedResolution(reason, 1); blocked {
				if blockErr != nil {
					return fmt.Errorf("preserve finalizer resolution after goal judge setup failure for %s: %w", taskID, blockErr)
				}
				return nil
			}
			durable, cancel := durableContext()
			defer cancel()
			if persistErr := blockManagedRun(durable, reason, model.BlockKindTransient, 0); persistErr != nil {
				return fmt.Errorf("persist goal judge setup block for %s: %w", taskID, persistErr)
			}
			return nil
		}
	}
	cleanupIfDone := func(current model.TaskDetail) {
		options.log("finish %s: %s", current.Task.ID, current.Task.Status)
		if current.Task.Status == model.TaskStatusDone && prepared.Workspace != nil {
			if _, err := workspaces.Cleanup(current.Task.Board, *prepared.Workspace); err != nil {
				options.log("workspace cleanup failed %s: %v", current.Task.ID, err)
			}
		}
	}
	blockPreservedRun := func(durable context.Context, reason string, exitCode int) error {
		return recoverRunBlockedDurably(durable, opened, scope.RunID, store.RecoverBlockedRunInput{
			Outcome: model.RunStatusBlocked, Reason: reason, Kind: model.BlockKindNeedsInput, ExitCode: &exitCode,
		})
	}
	preserveIfChanged := func(durable context.Context, reason string, exitCode int) (bool, error) {
		if !options.AllowWrites || prepared.Workspace == nil {
			return false, nil
		}
		preservedReason, preserve := preservedWorkspaceReason(durable, workspaces, prepared.Workspace, reason, "")
		if !preserve {
			return false, nil
		}
		if err := blockPreservedRun(durable, preservedReason, exitCode); err != nil {
			options.log("preserved-work block fallback failed %s: %v", taskID, err)
			return true, err
		}
		options.log("preserved partial work for %s", taskID)
		return true, nil
	}
	persistExecutionFailure := func(durable context.Context, preserveReason, runError string, exitCode int, failure store.FailRunOptions) error {
		preserved, err := preserveIfChanged(durable, preserveReason, exitCode)
		if err != nil || preserved {
			return err
		}
		return failRunDurably(durable, opened, scope, runError, failure)
	}
	health := agenthealth.New(manager, opened)
	resolutionStarted := false
	var resolutionStartGate TurnStartGate
	if resolution := prepared.IntegrationResolution; resolution != nil {
		resolutionStartGate = func(startCtx context.Context) (TurnStartCompensation, error) {
			if resolutionStarted {
				return nil, nil
			}
			if err := validateIntegrationResolutionHandoff(*prepared, runnerOptions, resolution.WorkspacePath); err != nil {
				return nil, fmt.Errorf("revalidate integration resolution handoff before launch: %w", err)
			}
			if err := workspace.ValidateIntegrationResolutionStart(startCtx, *resolution); err != nil {
				return nil, fmt.Errorf("revalidate live integration conflict before launch: %w", err)
			}
			input := store.StartIntegrationResolutionInput{
				ConflictFingerprint: resolution.ConflictFingerprint,
				ExpectedAttempt:     resolution.Attempt,
				ExpectedMaxAttempts: resolution.MaxAttempts,
			}
			started, err := opened.StartIntegrationResolutionAttempt(startCtx, scope, input)
			if err != nil {
				return nil, err
			}
			if !started.StartedNow {
				return nil, errors.New("integration resolution attempt already crossed its process-start boundary")
			}
			resolutionStarted = true
			resolution.Attempt, resolution.MaxAttempts = started.Attempt, started.MaxAttempts
			return func(compensationCtx context.Context) error {
				if err := opened.CompensateIntegrationResolutionStart(compensationCtx, scope, input); err != nil {
					return err
				}
				resolutionStarted = false
				return nil
			}, nil
		}
	}

	for {
		var command RunnerCommand
		if continuation != "" && (sessionID != "" || prepared.Task.Task.Runtime == model.RuntimeCline) {
			command, err = BuildGoalContinuationCommand(*prepared, runnerOptions, sessionID, continuation)
		} else {
			command, err = BuildRunnerCommand(*prepared, runnerOptions, sessionID)
		}
		if err != nil {
			if blocked, blockErr := blockPreparedResolution("Runner command construction failed before finalizer launch: "+err.Error(), 1); blocked {
				if blockErr != nil {
					return fmt.Errorf("preserve finalizer resolution after runner construction failure for %s: %w", taskID, blockErr)
				}
				return nil
			}
			durable, cancel := durableContext()
			preserved, persistErr := preserveIfChanged(durable, "Runner command construction failed after work began: "+err.Error(), 1)
			if persistErr == nil && !preserved {
				persistErr = failRunDurably(durable, opened, scope, err.Error(), store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			}
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist runner construction failure for %s: %w", taskID, persistErr)
			}
			return nil
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
		healthObservation, observationErr := health.Begin(ctx, configured.Profile, profile.GlobalRegistered)
		if observationErr != nil {
			if blocked, blockErr := blockPreparedResolution("Agent health observation failed before finalizer launch: "+observationErr.Error(), 1); blocked {
				if blockErr != nil {
					return fmt.Errorf("preserve finalizer resolution after agent health failure for %s: %w", taskID, blockErr)
				}
				return nil
			}
			durable, cancel := durableContext()
			countFailure := false
			persistErr := failRunDurably(durable, opened, scope, "Unable to reserve agent availability observation: "+observationErr.Error(), store.FailRunOptions{
				Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
				CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
			})
			cancel()
			if persistErr != nil {
				return fmt.Errorf(
					"persist agent health observation failure for %s: %w",
					taskID,
					errors.Join(observationErr, persistErr),
				)
			}
			return nil
		}
		options.log("launch %s via %s/%s profile=%s goal_turn=%d log=%s", taskID, prepared.Task.Task.Runtime, modelName, configured.Profile, turn, logPath)
		var execution TurnExecution
		if resolutionStartGate != nil {
			execution = ExecuteTurn(ctx, command, opened, scope, processes, logPath, runtimeLimit, resolutionStartGate)
		} else {
			execution = ExecuteTurn(ctx, command, opened, scope, processes, logPath, runtimeLimit)
		}
		durable, cancel := durableContext()
		currentDetail, getErr := retryStoreOperation(durable, func() (model.TaskDetail, error) {
			return opened.GetTask(durable, taskID)
		})
		if getErr != nil {
			cancel()
			options.log("post-run read failed %s: %v", taskID, getErr)
			return fmt.Errorf("read task %s after worker exit: %w", taskID, getErr)
		}
		current := currentDetail.Task
		if current.Status != model.TaskStatusRunning || current.CurrentRunID == nil || *current.CurrentRunID != prepared.Run.ID {
			cancel()
			cleanupIfDone(currentDetail)
			return nil
		}
		deferred, deferredErr := retryStoreOperation(durable, func() (*store.DeferredReclaim, error) {
			return opened.GetDeferredReclaim(durable, prepared.Run.ID)
		})
		if deferredErr != nil {
			cancel()
			return fmt.Errorf("read deferred reclaim for %s: %w", taskID, deferredErr)
		}
		runInspection, inspectErr := retryStoreOperation(durable, func() (store.RunInspection, error) {
			return opened.GetRun(durable, prepared.Run.ID)
		})
		if inspectErr != nil {
			cancel()
			return fmt.Errorf("inspect process ownership for %s: %w", taskID, inspectErr)
		}
		processIdentity, identityErr := opened.GetRunProcessIdentity(durable, prepared.Run.ID)
		if identityErr != nil {
			cancel()
			return fmt.Errorf("read process identity for %s: %w", taskID, identityErr)
		}
		if runcontrol.ProcessMayStillBeRunning(runInspection.Run.PID, processIdentity) {
			cancel()
			options.log("kept %s active because worker descendants still own the process group", taskID)
			return nil
		}
		if deferred != nil {
			preserved, persistErr := preserveIfChanged(durable, deferred.Reason, execution.Code)
			if persistErr == nil && !preserved {
				persistErr = recoverRunDurably(durable, opened, prepared.Run.ID, deferred.Outcome, deferred.Reason, deferred.CountFailure)
			}
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist deferred reclaim for %s: %w", taskID, persistErr)
			}
			options.log("reclaimed %s after deferred termination", taskID)
			return nil
		}
		terminalRequest, requestErr := retryStoreOperation(durable, func() (*model.TerminalRequest, error) {
			return opened.GetRunTerminalRequest(durable, prepared.Run.ID)
		})
		if requestErr != nil {
			cancel()
			options.log("terminal request read failed %s: %v", taskID, requestErr)
			return fmt.Errorf("read terminal request for %s after worker exit: %w", taskID, requestErr)
		}
		executionSucceeded := !execution.TimedOut && execution.SpawnError == nil && execution.Code == 0 && !execution.Canceled
		if executionSucceeded {
			lastRunID := prepared.Run.ID
			record, healthErr := health.RecordWorkerObservation(durable, healthObservation, store.SetAgentHealthInput{
				AgentID: configured.Profile, Status: model.AgentHealthReady, LastRunID: &lastRunID,
			}, profile.GlobalRegistered)
			if record.AuditError != nil {
				options.log("local agent health audit failed %s: %v", configured.Profile, record.AuditError)
			}
			if healthErr != nil {
				options.log("agent health update failed %s: %v", configured.Profile, healthErr)
			}
		}
		terminalCanFinalize := terminalRequest != nil && terminalRequest.FinalizedAt == nil &&
			(executionSucceeded || terminalRequest.Kind == "block")
		if terminalCanFinalize {
			if goalMode && terminalRequest.Kind == "complete" {
				if err := retryStoreAction(durable, func() error {
					return opened.DiscardRunTerminalRequest(durable, scope, "goal completion requires independent judgment")
				}); err != nil {
					cancel()
					return fmt.Errorf("discard premature goal completion for %s: %w", taskID, err)
				}
			} else {
				finalized, finalizeErr := finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, execution.Code)
				if finalizeErr != nil {
					reason := "Terminal finalization failed; the workspace was preserved for review: " + finalizeErr.Error()
					if prepared.Workspace != nil {
						reason += "; workspace: " + prepared.Workspace.Path
					}
					recoveryErr := blockPreservedRun(durable, reason, execution.Code)
					cancel()
					options.log("terminal finalization failed %s: %v", taskID, finalizeErr)
					return fmt.Errorf("finalize terminal request for %s: %w", taskID, errors.Join(finalizeErr, recoveryErr))
				}
				cancel()
				cleanupIfDone(finalized)
				return nil
			}
		}
		detail := fmt.Sprintf("Runner exited without a terminal Autogora call (%s)", execution.ExitDescription())
		if execution.SpawnError != nil {
			detail = execution.SpawnError.Error()
		}
		availability, agentUnavailable := classifyAgentAvailability(execution)
		switch {
		case execution.TimedOut || (runtimeLimit != nil && *runtimeLimit <= 0):
			persistErr := persistExecutionFailure(durable, "Runner timed out after work began: "+detail, detail, execution.Code,
				store.FailRunOptions{Outcome: model.RunStatusTimedOut, FailureLimit: options.FailureLimit})
			cancel()
			options.log("requeue/fail %s: %s", taskID, detail)
			if persistErr != nil {
				return fmt.Errorf("persist timeout for %s: %w", taskID, persistErr)
			}
			return nil
		case execution.Canceled:
			persistErr := persistExecutionFailure(durable, "Runner was canceled after work began: "+detail, detail, execution.Code,
				store.FailRunOptions{FailureLimit: options.FailureLimit})
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist cancellation for %s: %w", taskID, persistErr)
			}
			return nil
		case agentUnavailable:
			lastRunID, lastError := prepared.Run.ID, detail
			cooldownUntil := agentCooldown(availability.Status, *options.RateLimitCooldown, *options.AgentRetryCooldown)
			record, healthErr := health.RecordWorkerObservation(durable, healthObservation, store.SetAgentHealthInput{
				AgentID: configured.Profile, Status: availability.Status, CooldownUntil: cooldownUntil,
				LastError: &lastError, LastRunID: &lastRunID,
			}, profile.GlobalRegistered)
			if record.AuditError != nil {
				options.log("local agent health audit failed %s: %v", configured.Profile, record.AuditError)
			}
			if healthErr != nil {
				countFailure := false
				persistErr := failRunDurably(durable, opened, scope, "Unable to record agent availability: "+healthErr.Error(), store.FailRunOptions{
					Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
					CooldownSeconds: max(1, int(options.Interval.Seconds())), FailureLimit: options.FailureLimit,
				})
				cancel()
				if persistErr != nil {
					return fmt.Errorf("persist agent health failure for %s: %w", taskID, errors.Join(healthErr, persistErr))
				}
				return nil
			}
			preserved, preserveErr := preserveIfChanged(durable, fmt.Sprintf("Agent %s became %s after work began", configured.Profile, availability.Status), execution.Code)
			if preserveErr != nil {
				cancel()
				return fmt.Errorf("preserve unavailable-agent work for %s: %w", taskID, preserveErr)
			}
			if preserved {
				cancel()
				return nil
			}
			next, fallbackErr := resolveRunProfile(durable, manager, opened, prepared.Task.Task, options)
			fallbackAvailable := fallbackErr == nil && next.Name != configured.Profile
			countFailure := false
			cooldownSeconds := 0
			if availability.Status == model.AgentHealthRateLimited && !fallbackAvailable {
				cooldownSeconds = int(options.RateLimitCooldown.Seconds())
			}
			persistErr := failRunDurably(durable, opened, scope, detail, store.FailRunOptions{Outcome: availability.Outcome,
				CountFailure: &countFailure, CooldownSeconds: cooldownSeconds, FailureLimit: options.FailureLimit})
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist agent availability outcome for %s: %w", taskID, persistErr)
			}
			if fallbackAvailable {
				options.log("requeued %s for fallback %s after %s became %s", taskID, next.Name, configured.Profile, availability.Status)
			} else {
				options.log("paused %s because %s is %s and no fallback is available", taskID, configured.Profile, availability.Status)
			}
			return nil
		case execution.SpawnError != nil:
			persistErr := persistExecutionFailure(durable, "Runner could not restart after work began: "+detail, detail, execution.Code,
				store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist spawn failure for %s: %w", taskID, persistErr)
			}
			return nil
		case execution.Code != 0:
			persistErr := persistExecutionFailure(durable, "Runner exited unsuccessfully after work began: "+detail, detail, execution.Code,
				store.FailRunOptions{FailureLimit: options.FailureLimit})
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist unsuccessful exit for %s: %w", taskID, persistErr)
			}
			return nil
		case !goalMode:
			persistErr := persistExecutionFailure(durable, "Runner exited without reporting a terminal outcome after work began", detail, execution.Code,
				store.FailRunOptions{Outcome: model.RunStatusProtocolViolation, FailureLimit: options.FailureLimit})
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist protocol violation for %s: %w", taskID, persistErr)
			}
			return nil
		}
		if _, err := opened.PauseGoalRun(durable, scope, turn); err != nil {
			cancel()
			return fmt.Errorf("persist goal turn %d pause for %s: %w", turn, taskID, err)
		}
		if sessionID == "" {
			sessionID = execution.SessionID
		}
		if sessionID == "" && prepared.Task.Task.Runtime != model.RuntimeCline {
			reason := "Goal-mode runner did not report a resumable session id"
			preserved, persistErr := preserveIfChanged(durable, reason, execution.Code)
			if persistErr == nil && !preserved {
				persistErr = failRunDurably(durable, opened, scope, reason, store.FailRunOptions{Outcome: model.RunStatusProtocolViolation, FailureLimit: options.FailureLimit})
			}
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist missing goal session for %s: %w", taskID, persistErr)
			}
			return nil
		}
		var judgment orchestration.GoalJudgment
		if options.GoalJudge != nil {
			judgment, err = options.GoalJudge(durable, currentDetail, turn, execution.Output)
		} else {
			judgment, err = orchestration.JudgeGoalProgress(durable, currentDetail, turn, execution.Output, goalPlanner)
		}
		if err != nil {
			reason := "Goal judge failed: " + err.Error()
			persistErr := blockManagedRun(durable, reason, model.BlockKindTransient, 0)
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist goal judge failure for %s: %w", taskID, persistErr)
			}
			return nil
		}
		if _, err := opened.RecordGoalJudgment(durable, scope, store.GoalJudgment{Turn: turn, Complete: judgment.Complete, Reason: judgment.Reason, NextPrompt: judgment.NextPrompt}); err != nil {
			cancel()
			return fmt.Errorf("persist goal judgment for %s: %w", taskID, err)
		}
		if judgment.Complete {
			if _, requestErr := opened.CompleteRun(durable, scope, store.CompletionInput{Summary: fmt.Sprintf("Goal accepted after %d turn(s): %s", turn, judgment.Reason), Metadata: map[string]any{"goalMode": true, "turns": turn, "judgeReason": judgment.Reason}}); requestErr != nil {
				cancel()
				return fmt.Errorf("persist goal completion request for %s: %w", taskID, requestErr)
			}
			finalized, finalizeErr := finalizeManagedTerminal(durable, opened, workspaces, prepared, scope, 0)
			if finalizeErr != nil {
				reason := "Goal completion finalization failed; the workspace was preserved for review: " + finalizeErr.Error()
				recoveryErr := blockPreservedRun(durable, reason, 0)
				cancel()
				return fmt.Errorf("finalize goal completion for %s: %w", taskID, errors.Join(finalizeErr, recoveryErr))
			}
			cancel()
			cleanupIfDone(finalized)
			return nil
		}
		if turn >= prepared.Task.Task.GoalMaxTurns {
			reason := fmt.Sprintf("Goal turn budget exhausted after %d turns: %s", turn, judgment.Reason)
			persistErr := blockManagedRun(durable, reason, model.BlockKindNeedsInput, 0)
			cancel()
			if persistErr != nil {
				return fmt.Errorf("persist goal turn budget block for %s: %w", taskID, persistErr)
			}
			return nil
		}
		cancel()
		turn++
		continuation = judgment.NextPrompt
		if continuation == "" {
			continuation = "Continue toward the task acceptance criteria. Address this gap: " + judgment.Reason
		}
	}
}

func selectedBoards(ctx context.Context, manager *boards.Manager, options Options) ([]string, error) {
	if strings.TrimSpace(options.Board) != "" {
		board, err := manager.Resolve(options.Board)
		if err != nil {
			return nil, err
		}
		return []string{board}, nil
	}
	discovered, err := options.discoverBoards(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(discovered))
	for _, board := range discovered {
		if board == "default" || manager.Exists(board) {
			result = append(result, board)
		}
	}
	return result, nil
}

func maintainGlobalCoordination(ctx context.Context, manager *boards.Manager, options Options) (err error) {
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		return markGlobalCoordinationError("open global coordination store for maintenance", err)
	}
	defer func() {
		if closeErr := coordination.Close(); closeErr != nil {
			err = errors.Join(err, markGlobalCoordinationError("close global coordination store after maintenance", closeErr))
		}
	}()
	if _, clearErr := coordination.ClearExpiredAgentCooldowns(ctx, options.currentTime()); clearErr != nil {
		return markGlobalCoordinationError("clear global agent cooldowns", clearErr)
	}
	return nil
}

func maintainBoard(ctx context.Context, manager *boards.Manager, board string, options Options) (err error) {
	opened, err := options.openBoardStore(ctx, manager, board)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, opened.Close())
	}()
	current := options.currentTime()
	if opened.Board() != "default" {
		if _, err = opened.ClearExpiredAgentCooldowns(ctx, current); err != nil {
			return err
		}
	}
	if _, err = opened.PromoteDueTasks(ctx, board, current); err != nil {
		return err
	}
	if err = recoverAbandonedRuns(ctx, opened, board, options); err != nil {
		return err
	}
	return nil
}

func maintainBoards(ctx context.Context, manager *boards.Manager, boardSlugs []string, options Options) error {
	if err := maintainGlobalCoordination(ctx, manager, options); err != nil {
		return err
	}
	for _, board := range boardSlugs {
		if err := options.maintainOneBoard(ctx, manager, board); err != nil {
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
	type workerResult struct {
		board      string
		taskID     string
		generation uint64
		err        error
	}
	var workerResultMu sync.Mutex
	var workerResults []workerResult
	recordWorkerResult := func(result workerResult) {
		workerResultMu.Lock()
		workerResults = append(workerResults, result)
		workerResultMu.Unlock()
		select {
		case workerFinished <- struct{}{}:
		default:
		}
	}
	takeWorkerResults := func() []workerResult {
		workerResultMu.Lock()
		defer workerResultMu.Unlock()
		result := workerResults
		workerResults = nil
		return result
	}
	resilientWatch := resilientAllBoardWatch(options)
	boardCircuits := newBoardFailureCircuit(options.Interval, options.Now)
	reportBoardFailure := func(board, stage string, failure error) error {
		if failure == nil {
			return nil
		}
		wrapped := fmt.Errorf("%s for board %s: %w", stage, board, failure)
		if isGlobalCoordinationError(failure) || !resilientWatch {
			return wrapped
		}
		state := boardCircuits.failure(board)
		options.log(
			"paused board %s for %s after %s failure %d; retry at %s: %v",
			board, state.Delay, stage, state.Failures,
			state.RetryAt.Format(time.RFC3339Nano), failure,
		)
		return nil
	}
	reportBoardSuccess := func(board string, generation uint64) {
		if boardCircuits.success(board, generation) {
			options.log("resumed board %s after a successful dispatcher probe", board)
		}
	}
	handleWorkerResults := func() error {
		results := takeWorkerResults()
		if len(results) == 0 {
			return nil
		}
		boardErrors := make(map[string]error)
		boardOrder := make([]string, 0)
		successes := make(map[string][]uint64)
		var processErrors error
		for _, result := range results {
			if result.err == nil {
				successes[result.board] = append(successes[result.board], result.generation)
				continue
			}
			wrapped := fmt.Errorf("worker %s on board %s: %w", result.taskID, result.board, result.err)
			if isGlobalCoordinationError(result.err) || !resilientWatch {
				processErrors = errors.Join(processErrors, wrapped)
				continue
			}
			if _, exists := boardErrors[result.board]; !exists {
				boardOrder = append(boardOrder, result.board)
			}
			boardErrors[result.board] = errors.Join(boardErrors[result.board], wrapped)
		}
		for board, generations := range successes {
			if boardErrors[board] != nil {
				continue
			}
			for _, generation := range generations {
				reportBoardSuccess(board, generation)
			}
		}
		for _, board := range boardOrder {
			if err := reportBoardFailure(board, "worker execution", boardErrors[board]); err != nil {
				processErrors = errors.Join(processErrors, err)
			}
		}
		return processErrors
	}
	generatedClineApprovalDir := ""
	planning := startPlanningQueue(ctx, manager, options)
	coordination := startCoordinationQueue(ctx, manager, options)
	publication := startPublicationQueue(ctx, manager, options)
	oncePlanningWaited := false
	onceCoordinationWaited := false
	nextClaimBoard := ""
	defer func() {
		cancelDispatcher()
		processes.StopAll()
		workers.Wait()
		if runErr == nil {
			runErr = handleWorkerResults()
		}
		if !planning.Wait(options.PlanningShutdownGrace) {
			options.log("planner did not stop within %s; dispatcher shutdown will continue", options.PlanningShutdownGrace)
		}
		if !coordination.Wait(options.PlanningShutdownGrace) {
			options.log("Coordinator did not stop within %s; dispatcher shutdown will continue", options.PlanningShutdownGrace)
		}
		if !publication.Wait(options.PlanningShutdownGrace) {
			options.log("Publisher did not stop within %s; dispatcher shutdown will continue", options.PlanningShutdownGrace)
		}
		leader.Close()
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(runErr, ctxErr) {
			// Cancellation is the normal watch-mode shutdown signal. A storage
			// call may observe it just before the loop's explicit ctx check.
			runErr = nil
		}
		if runErr == nil && leader != nil {
			runErr = leader.Err()
		}
		if generatedClineApprovalDir != "" {
			_ = os.RemoveAll(generatedClineApprovalDir)
		}
	}()

	for {
		if err := handleWorkerResults(); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		boardSlugs, err := selectedBoards(ctx, manager, options)
		if err != nil {
			return err
		}
		boardCircuits.retain(boardSlugs)
		passBoards := boardSlugs
		if resilientWatch {
			if err := maintainGlobalCoordination(ctx, manager, options); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			passBoards = nil
			for _, board := range boardCircuits.eligible(boardSlugs) {
				if err := options.maintainOneBoard(ctx, manager, board); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					if processErr := reportBoardFailure(board, "maintenance", err); processErr != nil {
						return processErr
					}
					continue
				}
				passBoards = append(passBoards, board)
			}
		} else if err := maintainBoards(ctx, manager, boardSlugs, options); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		deliverBoardNotifications(ctx, manager, passBoards, options)
		var planningDone <-chan struct{}
		if !options.Once || !oncePlanningWaited {
			planningDone = planning.Enqueue(passBoards)
		}
		if !options.Once {
			coordination.Enqueue(passBoards)
			publication.Enqueue(passBoards)
		}
		launched := false
		foundInPass := true
		for ctx.Err() == nil && int(active.Load()) < options.MaxWorkers && foundInPass {
			foundInPass = false
			claimOrder := rotatedBoardSlugs(passBoards, nextClaimBoard)
			for _, board := range claimOrder {
				if ctx.Err() != nil || int(active.Load()) >= options.MaxWorkers {
					break
				}
				if resilientWatch && !boardCircuits.beginProbe(board) {
					continue
				}
				generation := boardCircuits.generation(board)
				runOptions := options
				if options.Autopilot {
					metadata, metadataErr := options.readBoardMetadata(manager, board)
					if metadataErr != nil {
						if processErr := reportBoardFailure(board, "read automation settings", metadataErr); processErr != nil {
							return processErr
						}
						continue
					}
					autopilot := metadata.Orchestration.Autopilot
					if !autopilot.Enabled || !autopilot.AutoExecute {
						reportBoardSuccess(board, generation)
						continue
					}
					runOptions.AllowWrites = options.AllowWrites && autopilot.WorkspaceWrites
				}
				opened, err := options.openBoardStore(ctx, manager, board)
				if err != nil {
					if processErr := reportBoardFailure(board, "open task store", err); processErr != nil {
						return processErr
					}
					continue
				}
				excluded, profileLimits, err := runOptions.boardClaimProfilePolicy(ctx, manager, opened, board)
				if err != nil {
					err = errors.Join(err, opened.Close())
					if processErr := reportBoardFailure(board, "resolve claim profiles", err); processErr != nil {
						return processErr
					}
					continue
				}
				claim, err := runOptions.claimBoardTask(ctx, opened, store.ClaimOptions{TaskID: options.TaskID, Board: board, WorkerID: fmt.Sprintf("dispatcher-%d", os.Getpid()), ExcludeManual: true,
					ExpectedUpdatedAt: options.ExpectedUpdatedAt,
					ClaimTTLSeconds:   options.ClaimTTLSeconds, MaxInProgress: options.MaxInProgress, MaxInProgressPerAssignee: options.MaxInProgressPerAssignee,
					MaxInProgressByAssignee: profileLimits, ExcludedAssignees: excluded})
				if err != nil {
					err = errors.Join(err, opened.Close())
					if processErr := reportBoardFailure(board, "claim task", err); processErr != nil {
						return processErr
					}
					continue
				}
				if claim == nil {
					if closeErr := opened.Close(); closeErr != nil {
						if processErr := reportBoardFailure(board, "close task store after claim probe", closeErr); processErr != nil {
							return processErr
						}
						continue
					}
					reportBoardSuccess(board, generation)
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
				nextClaimBoard = boardAfter(claimOrder, board)
				active.Add(1)
				workers.Add(1)
				go func(opened *store.Store, claim *model.ClaimedTask, approvalDir string, runOptions Options, generation uint64) {
					defer workers.Done()
					workerErr := runOptions.executeClaim(ctx, manager, opened, claim, processes, approvalDir)
					closeErr := opened.Close()
					active.Add(-1)
					recordWorkerResult(workerResult{
						board: claim.Task.Task.Board, taskID: claim.Task.Task.ID,
						generation: generation, err: errors.Join(workerErr, closeErr),
					})
				}(opened, claim, approvalDir, runOptions, generation)
				if options.Once {
					break
				}
			}
			if !foundInPass && len(claimOrder) > 0 {
				nextClaimBoard = boardAfter(claimOrder, claimOrder[0])
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
					if err := handleWorkerResults(); err != nil {
						processes.StopAll()
						workers.Wait()
						return err
					}
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
			if err := handleWorkerResults(); err != nil {
				return err
			}
			if !launched && !oncePlanningWaited && planningDone != nil {
				select {
				case <-ctx.Done():
					return nil
				case <-planningDone:
					oncePlanningWaited = true
					continue
				}
			}
			if !launched && !onceCoordinationWaited {
				coordinationDone := coordination.Enqueue(passBoards)
				if coordinationDone != nil {
					select {
					case <-ctx.Done():
						return nil
					case <-coordinationDone:
						onceCoordinationWaited = true
						continue
					}
				}
				onceCoordinationWaited = true
			}
			publicationDone := publication.Enqueue(passBoards)
			if publicationDone != nil {
				select {
				case <-ctx.Done():
					return nil
				case <-publicationDone:
				}
			}
			deliverBoardNotifications(ctx, manager, passBoards, options)
			return nil
		}
		timer := time.NewTimer(options.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			processes.StopAll()
			workers.Wait()
			return nil
		case <-workerFinished:
			timer.Stop()
			if err := handleWorkerResults(); err != nil {
				processes.StopAll()
				workers.Wait()
				return err
			}
		case <-timer.C:
		}
	}
}
