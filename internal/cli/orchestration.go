package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
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
	plannerRuntime := model.Runtime("")
	var err error
	if opts.present("planner-runtime") {
		plannerRuntime, err = requirePlannerRuntime(opts.value("planner-runtime"))
		if err != nil {
			return err
		}
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
	if plannerRuntime == "" {
		plannerRuntime = metadata.Orchestration.PlannerRuntime
	}
	plannerModel, plannerProvider := opts.value("planner-model"), opts.value("planner-provider")
	if plannerModel == "" && !opts.present("planner-runtime") {
		plannerModel = metadata.Orchestration.PlannerModel
	}
	if plannerProvider == "" && !opts.present("planner-runtime") {
		plannerProvider = metadata.Orchestration.PlannerProvider
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
	planner, err := orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: plannerRuntime, Model: plannerModel,
		Provider: plannerProvider, CWD: cwd, Timeout: time.Duration(timeoutMS) * time.Millisecond, Getenv: a.Getenv})
	if err != nil {
		return err
	}
	discovered, err := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
	if err != nil {
		return err
	}
	configured := cliBoardProfileRoutes(metadata.Orchestration.Profiles)
	for _, raw := range opts.many("profile") {
		profile, err := mergeExplicitProfileRoute(raw, configured, plannerRuntime)
		if err != nil {
			return err
		}
		replaced := false
		for index := range configured {
			if configured[index].Name == profile.Name {
				configured[index] = profile
				replaced = true
				break
			}
		}
		if !replaced {
			configured = append(configured, profile)
		}
	}
	profiles := orchestration.ResolveProfileRoutes(discovered, configured)
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
			_, getErr := opened.GetTask(ctx, taskID)
			if getErr != nil {
				err = getErr
			} else {
				fallback, selectedFinalizer := orchestration.SelectProfileRoutes(
					profiles, metadata.Orchestration.DefaultProfile, metadata.Orchestration.FinalizerProfile, plannerRuntime,
				)
				if opts.present("default-profile") {
					fallback, err = mergeExplicitProfileRoute(opts.value("default-profile"), profiles, plannerRuntime)
					if err == nil && !orchestration.RunnableProfileRoute(fallback) {
						err = errors.New("--default-profile requires an enabled worker profile")
					}
					if err == nil && !opts.present("finalizer-profile") {
						selectedFinalizer = fallback
					}
				}
				if err == nil && opts.present("finalizer-profile") {
					selectedFinalizer, err = mergeExplicitProfileRoute(opts.value("finalizer-profile"), profiles, plannerRuntime)
					if err == nil && !orchestration.RunnableProfileRoute(selectedFinalizer) {
						err = errors.New("--finalizer-profile requires an enabled worker profile")
					}
				}
				if err == nil {
					entry.Value, err = orchestration.DecomposeTriageTask(ctx, opened, taskID, orchestration.DecomposeOptions{
						Profiles: profiles, DefaultProfile: fallback, FinalizerProfile: &selectedFinalizer,
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

func cliBoardProfileRoutes(profiles []boards.Profile) []orchestration.ProfileRoute {
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

// mergeExplicitProfileRoute applies the runtime and description supplied by a
// CLI route while retaining board constraints. In particular, naming a
// disabled board profile cannot silently turn it back on for one command.
func mergeExplicitProfileRoute(raw string, profiles []orchestration.ProfileRoute, fallback model.Runtime) (orchestration.ProfileRoute, error) {
	parsed, err := parseRoutingProfile(raw, fallback)
	if err != nil {
		return orchestration.ProfileRoute{}, err
	}
	parts := strings.Split(raw, ":")
	for _, profile := range profiles {
		if profile.Name != parsed.Name {
			continue
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			profile.Runtime = parsed.Runtime
		}
		if len(parts) > 2 {
			profile.Description = parsed.Description
		}
		return profile, nil
	}
	return parsed, nil
}
