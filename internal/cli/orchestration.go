package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/orchestration"
	"github.com/nn1a/kanban/internal/store"
)

func requirePlannerRuntime(value string) (model.Runtime, error) {
	runtime, err := requireRuntime(value, model.RuntimeCodex)
	if err != nil {
		return "", err
	}
	if runtime == model.RuntimeManual {
		return "", fmt.Errorf("invalid planner runtime: %s", runtime)
	}
	return runtime, nil
}

func parseRoutingProfile(value string, fallback model.Runtime) (orchestration.ProfileRoute, error) {
	parts := strings.Split(value, ":")
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return orchestration.ProfileRoute{}, fmt.Errorf("invalid profile route: %s", value)
	}
	runtime := fallback
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		parsed, err := requirePlannerRuntime(strings.TrimSpace(parts[1]))
		if err != nil {
			return orchestration.ProfileRoute{}, fmt.Errorf("invalid profile runtime in %s", value)
		}
		runtime = parsed
	}
	description := ""
	if len(parts) > 2 {
		description = strings.TrimSpace(strings.Join(parts[2:], ":"))
	}
	return orchestration.ProfileRoute{Name: name, Runtime: runtime, Description: description}, nil
}

type orchestrationCLIResult struct {
	TaskID string `json:"taskId"`
	OK     bool   `json:"ok"`
	Value  any    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (a *App) runOrchestration(ctx context.Context, command string, opts options) error {
	requestedID := ""
	if len(opts.positionals) > 0 {
		requestedID = opts.positionals[0]
	}
	if requestedID == "" && !opts.flags["all"] {
		return fmt.Errorf("%s requires a task id or --all", command)
	}
	if requestedID != "" && opts.flags["all"] {
		return fmt.Errorf("%s accepts a task id or --all, not both", command)
	}
	if opts.present("title") != opts.present("body") {
		return errors.New("--title and --body must be provided together")
	}
	if opts.flags["all"] && opts.present("title") {
		return errors.New("an explicit specification cannot be reused with --all")
	}
	plannerRuntime, err := requirePlannerRuntime(opts.value("planner-runtime"))
	if err != nil {
		return err
	}
	timeoutMS, err := numberOption(opts.value("planner-timeout-ms"), 120_000)
	if err != nil || timeoutMS < 1_000 || timeoutMS > 600_000 {
		return errors.New("--planner-timeout-ms must be between 1000 and 600000")
	}
	opened, manager, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	metadata, err := manager.Read(board)
	if err != nil {
		return err
	}
	taskIDs := []string{requestedID}
	if opts.flags["all"] {
		tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{Status: model.TaskStatusTriage, Tenant: opts.value("tenant"), Limit: 500})
		if err != nil {
			return err
		}
		taskIDs = make([]string, 0, len(tasks))
		for _, task := range tasks {
			taskIDs = append(taskIDs, task.ID)
		}
	}
	cwd, err := a.workingDirectory()
	if err != nil {
		return err
	}
	planner, err := orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: plannerRuntime, CWD: cwd, Timeout: time.Duration(timeoutMS) * time.Millisecond, Getenv: a.Getenv})
	if err != nil {
		return err
	}
	profiles := []orchestration.ProfileRoute{}
	seen := map[string]bool{}
	discovered, err := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
	if err != nil {
		return err
	}
	for _, task := range discovered {
		if task.Assignee != nil && task.Runtime != model.RuntimeManual && !seen[*task.Assignee] {
			seen[*task.Assignee] = true
			profiles = append(profiles, orchestration.ProfileRoute{Name: *task.Assignee, Runtime: task.Runtime})
		}
	}
	for _, raw := range opts.many("profile") {
		profile, err := parseRoutingProfile(raw, plannerRuntime)
		if err != nil {
			return err
		}
		if seen[profile.Name] {
			for index := range profiles {
				if profiles[index].Name == profile.Name {
					profiles[index] = profile
				}
			}
		} else {
			seen[profile.Name] = true
			profiles = append(profiles, profile)
		}
	}
	var explicitPlan *orchestration.DecompositionPlan
	if opts.present("plan-json") {
		explicitPlan = &orchestration.DecompositionPlan{}
		if err := json.Unmarshal([]byte(opts.value("plan-json")), explicitPlan); err != nil {
			return fmt.Errorf("invalid --plan-json: %w", err)
		}
	}
	results := make([]orchestrationCLIResult, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		entry := orchestrationCLIResult{TaskID: taskID}
		if command == "specify" {
			var explicit *orchestration.SpecificationPlan
			if opts.present("title") {
				explicit = &orchestration.SpecificationPlan{Title: opts.value("title"), Body: opts.value("body")}
			}
			entry.Value, err = orchestration.SpecifyTriageTask(ctx, opened, taskID, planner, explicit, opts.value("author"))
		} else {
			root, getErr := opened.GetTask(ctx, taskID)
			if getErr != nil {
				err = getErr
			} else {
				fallback := orchestration.ProfileRoute{}
				if opts.present("default-profile") {
					fallback, err = parseRoutingProfile(opts.value("default-profile"), plannerRuntime)
				} else if root.Task.Assignee != nil && root.Task.Runtime != model.RuntimeManual {
					fallback = orchestration.ProfileRoute{Name: *root.Task.Assignee, Runtime: root.Task.Runtime}
				} else if len(profiles) > 0 {
					fallback = profiles[0]
				} else {
					fallback = orchestration.ProfileRoute{Name: string(plannerRuntime) + "-worker", Runtime: plannerRuntime}
				}
				var orchestratorProfile *orchestration.ProfileRoute
				if err == nil && opts.present("orchestrator-profile") {
					value, parseErr := parseRoutingProfile(opts.value("orchestrator-profile"), plannerRuntime)
					err, orchestratorProfile = parseErr, &value
				}
				if err == nil {
					if orchestratorProfile == nil {
						value := fallback
						orchestratorProfile = &value
					}
					entry.Value, err = orchestration.DecomposeTriageTask(ctx, opened, taskID, orchestration.DecomposeOptions{
						Profiles: profiles, DefaultProfile: fallback, OrchestratorProfile: orchestratorProfile,
						AutoPromoteChildren: &metadata.Orchestration.AutoPromoteChildren, Planner: planner, Plan: explicitPlan,
					})
				}
			}
		}
		if err != nil {
			entry.Error = err.Error()
		} else {
			entry.OK = true
		}
		results = append(results, entry)
		err = nil
	}
	return writeJSON(a.Stdout, results)
}
