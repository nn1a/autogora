package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
	PlannerTimeout           time.Duration
	DecompositionPlanner     orchestration.Planner
	AllowWrites              bool
	ClineApprovalDir         string
	WorkingDirectory         string
	Getenv                   func(string) string
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
		result = append(result, orchestration.ProfileRoute{Name: profile.Name, Runtime: profile.Runtime, Description: profile.Description})
	}
	return result
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
		plannerRuntime := options.PlannerRuntime
		if plannerRuntime == "" {
			plannerRuntime = metadata.Orchestration.PlannerRuntime
		}
		planner := options.DecompositionPlanner
		if planner == nil {
			planner, err = orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: plannerRuntime, CWD: options.WorkingDirectory, Timeout: options.PlannerTimeout, Getenv: options.Getenv})
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
		configuredProfiles := boardProfiles(metadata.Orchestration.Profiles)
		if options.DecompositionProfiles != nil {
			configuredProfiles = options.DecompositionProfiles
		}
		profiles := orchestration.ResolveProfileRoutes(discovered, configuredProfiles)
		for _, task := range triage {
			defaultName, orchestratorName := metadata.Orchestration.DefaultProfile, metadata.Orchestration.OrchestratorProfile
			fallback, orchestrator := orchestration.SelectProfileRoutes(profiles, defaultName, orchestratorName, plannerRuntime)
			if options.DefaultProfile != nil {
				fallback = *options.DefaultProfile
			}
			if options.DefaultProfile == nil && metadata.Orchestration.DefaultProfile == nil && task.Assignee != nil && task.Runtime != model.RuntimeManual {
				fallback = orchestration.ProfileRoute{Name: *task.Assignee, Runtime: task.Runtime}
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

func runClaim(ctx context.Context, manager *boards.Manager, opened *store.Store, claim *model.ClaimedTask, options Options, processes *ProcessSet, clineApprovalDir string) {
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
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
	runnerOptions := RunnerOptions{DBPath: options.DBPath, CLIPath: options.CLIPath, AllowWrites: options.AllowWrites,
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
		plannerCWD := options.WorkingDirectory
		if prepared.Workspace != nil {
			plannerCWD = prepared.Workspace.Path
		}
		goalPlanner, err = orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: prepared.Task.Task.Runtime, CWD: plannerCWD, Timeout: options.PlannerTimeout, Getenv: options.Getenv})
		if err != nil {
			durable, cancel := durableContext()
			defer cancel()
			_, _ = opened.BlockRun(durable, scope, store.BlockInput{Reason: "Goal judge setup failed: " + err.Error(), Kind: model.BlockKindTransient})
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

	for {
		var command RunnerCommand
		if continuation != "" && (sessionID != "" || prepared.Task.Task.Runtime == model.RuntimeCline) {
			command, err = BuildGoalContinuationCommand(*prepared, runnerOptions, sessionID, continuation)
		} else {
			command, err = BuildRunnerCommand(*prepared, runnerOptions, sessionID)
		}
		if err != nil {
			durable, cancel := durableContext()
			_, _ = opened.FailRun(durable, scope, err.Error(), store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			cancel()
			return
		}
		var runtimeLimit *time.Duration
		if prepared.Task.Task.MaxRuntimeSeconds != nil {
			value := time.Duration(*prepared.Task.Task.MaxRuntimeSeconds)*time.Second - time.Since(runStarted)
			runtimeLimit = &value
		}
		options.log("launch %s via %s goal_turn=%d log=%s", taskID, prepared.Task.Task.Runtime, turn, logPath)
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
		detail := fmt.Sprintf("Runner exited without a terminal Autogora call (%s)", execution.ExitDescription())
		if execution.SpawnError != nil {
			detail = execution.SpawnError.Error()
		}
		switch {
		case execution.TimedOut || (runtimeLimit != nil && *runtimeLimit <= 0):
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusTimedOut, FailureLimit: options.FailureLimit})
			cancel()
			options.log("requeue/fail %s: %s", taskID, detail)
			return
		case execution.SpawnError != nil:
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusSpawnFailed, FailureLimit: options.FailureLimit})
			cancel()
			return
		case execution.Code == 75:
			countFailure := false
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{Outcome: model.RunStatusRateLimited, CountFailure: &countFailure, CooldownSeconds: int(options.RateLimitCooldown.Seconds()), FailureLimit: options.FailureLimit})
			cancel()
			return
		case execution.Code != 0 || execution.Canceled:
			_, _ = opened.FailRun(durable, scope, detail, store.FailRunOptions{FailureLimit: options.FailureLimit})
			cancel()
			return
		case !goalMode:
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
			_, _ = opened.FailRun(durable, scope, "Goal-mode runner did not report a resumable session id", store.FailRunOptions{Outcome: model.RunStatusProtocolViolation, FailureLimit: options.FailureLimit})
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
			_, _ = opened.BlockRun(durable, scope, store.BlockInput{Reason: "Goal judge failed: " + err.Error(), Kind: model.BlockKindTransient})
			cancel()
			return
		}
		_, _ = opened.RecordGoalJudgment(durable, scope, store.GoalJudgment{Turn: turn, Complete: judgment.Complete, Reason: judgment.Reason, NextPrompt: judgment.NextPrompt})
		if judgment.Complete {
			_, _ = opened.CompleteRun(durable, scope, store.CompletionInput{Summary: fmt.Sprintf("Goal accepted after %d turn(s): %s", turn, judgment.Reason), Metadata: map[string]any{"goalMode": true, "turns": turn, "judgeReason": judgment.Reason}})
			cancel()
			cleanupIfDone()
			return
		}
		if turn >= prepared.Task.Task.GoalMaxTurns {
			_, _ = opened.BlockRun(durable, scope, store.BlockInput{Reason: fmt.Sprintf("Goal turn budget exhausted after %d turns: %s", turn, judgment.Reason), Kind: model.BlockKindNeedsInput})
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

func Run(ctx context.Context, options Options) error {
	options.normalize()
	if options.DBPath == "" || options.CLIPath == "" {
		return errors.New("dispatcher requires DBPath and CLIPath")
	}
	manager, err := boards.NewManager(options.DBPath)
	if err != nil {
		return err
	}
	ctx, cancelDispatcher := context.WithCancel(ctx)
	processes := NewProcessSet()
	var active atomic.Int32
	var workers sync.WaitGroup
	workerFinished := make(chan struct{}, 1)
	generatedClineApprovalDir := ""
	defer func() {
		cancelDispatcher()
		processes.StopAll()
		workers.Wait()
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
				claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: options.TaskID, Board: board, WorkerID: fmt.Sprintf("dispatcher-%d", os.Getpid()), ExcludeManual: true,
					ClaimTTLSeconds: options.ClaimTTLSeconds, MaxInProgress: options.MaxInProgress, MaxInProgressPerAssignee: options.MaxInProgressPerAssignee})
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
