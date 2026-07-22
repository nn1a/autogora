package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nn1a/kanban/internal/dispatcher"
	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/orchestration"
	"github.com/nn1a/kanban/internal/store"
)

func (a *App) dispatcherDBPath(value string) (string, error) {
	if value == "" {
		return a.defaultDBPath()
	}
	return filepath.Abs(value)
}

func (a *App) dispatchCandidates(ctx context.Context, opts options, limit int) (any, error) {
	manager, err := a.managerFor(opts.value("db"))
	if err != nil {
		return nil, err
	}
	boardsToRead := []string{}
	if requested := a.board(opts); requested != "" {
		resolved, err := manager.Resolve(requested)
		if err != nil {
			return nil, err
		}
		boardsToRead = append(boardsToRead, resolved)
	} else {
		metadata, err := manager.List(ctx, false)
		if err != nil {
			return nil, err
		}
		for _, board := range metadata {
			if !board.Archived {
				boardsToRead = append(boardsToRead, board.Slug)
			}
		}
	}
	candidates := []model.Task{}
	for _, board := range boardsToRead {
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			return nil, err
		}
		tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{Status: model.TaskStatusReady, Sort: "priority-desc", Limit: 500})
		if err != nil {
			opened.Close()
			return nil, err
		}
		for _, task := range tasks {
			if task.Assignee == nil || task.Runtime == model.RuntimeManual {
				continue
			}
			if task.ScheduledAt != nil {
				if scheduled, err := time.Parse(time.RFC3339Nano, *task.ScheduledAt); err == nil && scheduled.After(time.Now()) {
					continue
				}
			}
			detail, err := opened.GetTask(ctx, task.ID)
			if err != nil {
				continue
			}
			blocked := false
			for _, parent := range detail.Parents {
				if parent.Status != model.TaskStatusDone {
					blocked = true
					break
				}
			}
			if !blocked {
				candidates = append(candidates, task)
			}
			if len(candidates) >= limit {
				break
			}
		}
		opened.Close()
		if len(candidates) >= limit {
			break
		}
	}
	return map[string]any{"dryRun": true, "candidates": candidates}, nil
}

func (a *App) runDispatch(ctx context.Context, command string, opts options) error {
	if command == "daemon" && !opts.flags["force"] {
		return errors.New("daemon is deprecated and requires --force; prefer dispatch --watch")
	}
	maxWorkers, err := numberOption(firstNonEmpty(opts.value("max"), opts.value("max-workers")), 2)
	if err != nil || maxWorkers < 1 {
		return errors.New("--max-workers must be at least 1")
	}
	if opts.flags["dry-run"] {
		value, err := a.dispatchCandidates(ctx, opts, maxWorkers)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	}
	dbPath, err := a.dispatcherDBPath(opts.value("db"))
	if err != nil {
		return err
	}
	cliPath, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := a.workingDirectory()
	if err != nil {
		return err
	}
	intervalMS, err := numberOption(opts.value("interval-ms"), 2_000)
	if err != nil {
		return err
	}
	claimTTL, err := numberOption(opts.value("claim-ttl-seconds"), 900)
	if err != nil {
		return err
	}
	staleSeconds, err := numberOption(opts.value("stale-timeout-seconds"), 4*60*60)
	if err != nil {
		return err
	}
	heartbeatSeconds, err := numberOption(opts.value("heartbeat-max-stale-seconds"), 60*60)
	if err != nil {
		return err
	}
	crashSeconds, err := numberOption(opts.value("crash-grace-seconds"), 30)
	if err != nil || crashSeconds < 0 {
		return errors.New("--crash-grace-seconds cannot be negative")
	}
	rateLimitSeconds, err := numberOption(opts.value("rate-limit-cooldown-seconds"), 60)
	if err != nil || rateLimitSeconds < 0 {
		return errors.New("--rate-limit-cooldown-seconds cannot be negative")
	}
	maxInProgress, err := optionalNumber(opts, "max-in-progress")
	if err != nil {
		return err
	}
	maxPerAssignee, err := optionalNumber(opts, "max-per-assignee")
	if err != nil {
		return err
	}
	failureLimit, err := optionalNumber(opts, "failure-limit")
	if err != nil {
		return err
	}
	autoPerTick, err := numberOption(opts.value("auto-decompose-per-tick"), 3)
	if err != nil {
		return err
	}
	plannerRuntime, err := requirePlannerRuntime(opts.value("planner-runtime"))
	if err != nil {
		return err
	}
	plannerTimeoutMS, err := numberOption(opts.value("planner-timeout-ms"), 120_000)
	if err != nil {
		return err
	}
	profiles := make([]orchestration.ProfileRoute, 0, len(opts.many("profile")))
	for _, raw := range opts.many("profile") {
		profile, err := parseRoutingProfile(raw, plannerRuntime)
		if err != nil {
			return err
		}
		profiles = append(profiles, profile)
	}
	var defaultProfile, orchestratorProfile *orchestration.ProfileRoute
	if opts.present("default-profile") {
		value, err := parseRoutingProfile(opts.value("default-profile"), plannerRuntime)
		if err != nil {
			return err
		}
		defaultProfile = &value
	}
	if opts.present("orchestrator-profile") {
		value, err := parseRoutingProfile(opts.value("orchestrator-profile"), plannerRuntime)
		if err != nil {
			return err
		}
		orchestratorProfile = &value
	}
	var autoDecompose *bool
	if opts.present("auto-decompose") {
		value := opts.flags["auto-decompose"]
		autoDecompose = &value
	}
	crashGrace, rateCooldown := time.Duration(crashSeconds)*time.Second, time.Duration(rateLimitSeconds)*time.Second
	return dispatcher.Run(ctx, dispatcher.Options{
		DBPath: dbPath, CLIPath: cliPath, Board: a.board(opts), Once: command != "daemon" && opts.flags["once"],
		Interval: time.Duration(intervalMS) * time.Millisecond, MaxWorkers: maxWorkers,
		MaxInProgress: maxInProgress, MaxInProgressPerAssignee: maxPerAssignee,
		ClaimTTLSeconds: claimTTL, StaleTimeout: time.Duration(staleSeconds) * time.Second,
		HeartbeatMaxStale: time.Duration(heartbeatSeconds) * time.Second, CrashGrace: &crashGrace,
		RateLimitCooldown: &rateCooldown, FailureLimit: failureLimit,
		AutoDecompose: autoDecompose, AutoDecomposePerTick: autoPerTick,
		DecompositionProfiles: profiles, DefaultProfile: defaultProfile, OrchestratorProfile: orchestratorProfile,
		PlannerRuntime: plannerRuntime, PlannerTimeout: time.Duration(plannerTimeoutMS) * time.Millisecond,
		AllowWrites: opts.flags["allow-writes"], WorkingDirectory: cwd, Getenv: a.Getenv,
		OnLog: func(message string) { _, _ = fmt.Fprintf(a.Stderr, "[taskcircuit] %s\n", message) },
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func optionalNumber(opts options, name string) (int, error) {
	if !opts.present(name) {
		return 0, nil
	}
	value, err := numberOption(opts.value(name), 0)
	if err != nil {
		return 0, err
	}
	if value < 1 {
		return 0, fmt.Errorf("--%s must be at least 1", name)
	}
	return value, nil
}
