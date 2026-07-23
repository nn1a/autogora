package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

type DispatchRunner func(context.Context, dispatcher.Options) error

const dispatchHelp = `autogora dispatch [--once|--watch] [options]

Modes:
  --once                Run one scheduling cycle, then exit
  --watch               Keep scheduling until interrupted (default)
  --dry-run             List eligible tasks without claiming them
  --autopilot=<bool>    Follow each board's automation policy and enable Coordinator recovery

Options:
  --board <slug>        Limit scheduling to one board
  --max-workers <n>     Maximum concurrent workers (default: 2)
  --allow-writes=<bool> Permit coding agents to modify trusted workspaces
  --interval-ms <ms>    Scheduling interval (default: 2000)
  --planner-runtime <r> Override the board planner runtime
  --planner-model <id>  Override the board planner model
  --planner-provider <p> Override the board planner provider
  --planner-timeout-ms <ms> Planner timeout from 1000 to 600000
  --db <path>           Override the project-specific SQLite path

Autopilot does not enable automation by itself. It applies each board's saved
Auto Plan, Auto Execute, Coordinator, and workspace-write policy. --allow-writes
remains the process-wide upper bound for workspace changes.
`

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

func (a *App) runDispatch(ctx context.Context, opts options) error {
	if opts.flags["once"] && opts.flags["watch"] {
		return errors.New("--once and --watch cannot be used together")
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
	autoPerTick, err := numberOption(opts.value("auto-decompose-per-tick"), 0)
	if err != nil {
		return err
	}
	plannerRuntime := model.Runtime("")
	profileFallback := model.RuntimeCodex
	if opts.present("planner-runtime") {
		plannerRuntime, err = requirePlannerRuntime(opts.value("planner-runtime"))
		if err != nil {
			return err
		}
		profileFallback = plannerRuntime
	}
	plannerTimeoutMS, err := numberOption(opts.value("planner-timeout-ms"), 120_000)
	if err != nil || plannerTimeoutMS < 1_000 || plannerTimeoutMS > 600_000 {
		return errors.New("--planner-timeout-ms must be between 1000 and 600000")
	}
	var profiles []orchestration.ProfileRoute
	for _, raw := range opts.many("profile") {
		profile, err := parseRoutingProfile(raw, profileFallback)
		if err != nil {
			return err
		}
		profiles = append(profiles, profile)
	}
	var defaultProfile, finalizerProfile *orchestration.ProfileRoute
	if opts.present("default-profile") {
		value, err := parseRoutingProfile(opts.value("default-profile"), profileFallback)
		if err != nil {
			return err
		}
		defaultProfile = &value
	}
	if opts.present("finalizer-profile") {
		value, err := parseRoutingProfile(opts.value("finalizer-profile"), profileFallback)
		if err != nil {
			return err
		}
		finalizerProfile = &value
	}
	var autoDecompose *bool
	if opts.present("auto-decompose") {
		value := opts.flags["auto-decompose"]
		autoDecompose = &value
	}
	crashGrace, rateCooldown := time.Duration(crashSeconds)*time.Second, time.Duration(rateLimitSeconds)*time.Second
	run := a.DispatchRunner
	if run == nil {
		run = dispatcher.Run
	}
	return run(ctx, dispatcher.Options{
		DBPath: dbPath, CLIPath: cliPath, Board: a.board(opts), Once: opts.flags["once"],
		Interval: time.Duration(intervalMS) * time.Millisecond, MaxWorkers: maxWorkers,
		MaxInProgress: maxInProgress, MaxInProgressPerAssignee: maxPerAssignee,
		ClaimTTLSeconds: claimTTL, StaleTimeout: time.Duration(staleSeconds) * time.Second,
		HeartbeatMaxStale: time.Duration(heartbeatSeconds) * time.Second, CrashGrace: &crashGrace,
		RateLimitCooldown: &rateCooldown, FailureLimit: failureLimit,
		AutoDecompose: autoDecompose, AutoDecomposePerTick: autoPerTick,
		DecompositionProfiles: profiles, DefaultProfile: defaultProfile, FinalizerProfile: finalizerProfile,
		PlannerRuntime: plannerRuntime, PlannerTimeout: time.Duration(plannerTimeoutMS) * time.Millisecond,
		PlannerModel: opts.value("planner-model"), PlannerProvider: opts.value("planner-provider"),
		AllowWrites: opts.flags["allow-writes"], Autopilot: opts.flags["autopilot"],
		WorkingDirectory: cwd, Getenv: a.Getenv,
		OnLog: func(message string) { _, _ = fmt.Fprintf(a.Stderr, "[autogora] %s\n", message) },
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
