package orchestration

import (
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

// ResolveProfileRoutes merges routes observed on existing tasks with the board
// configuration. Configured routes win so every UI and dispatcher uses the
// same runtime and description for a profile name.
func ResolveProfileRoutes(tasks []model.Task, configured []ProfileRoute) []ProfileRoute {
	profiles := []ProfileRoute{}
	index := map[string]int{}
	for _, task := range tasks {
		if task.Assignee == nil || strings.TrimSpace(*task.Assignee) == "" || task.Runtime == model.RuntimeManual || !model.ValidRuntime(task.Runtime) {
			continue
		}
		if _, exists := index[*task.Assignee]; exists {
			continue
		}
		index[*task.Assignee] = len(profiles)
		profiles = append(profiles, ProfileRoute{Name: *task.Assignee, Runtime: task.Runtime})
	}
	for _, route := range configured {
		route.Name = strings.TrimSpace(route.Name)
		if route.Name == "" || route.Runtime == model.RuntimeManual || !model.ValidRuntime(route.Runtime) {
			continue
		}
		if old, exists := index[route.Name]; exists {
			profiles[old] = route
		} else {
			index[route.Name] = len(profiles)
			profiles = append(profiles, route)
		}
	}
	return profiles
}

// SelectProfileRoutes applies the configured names to a resolved roster and
// supplies a runnable fallback when a new board has no profiles yet.
func SelectProfileRoutes(profiles []ProfileRoute, defaultName, orchestratorName *string, plannerRuntime model.Runtime) (ProfileRoute, ProfileRoute) {
	fallback := ProfileRoute{}
	for _, profile := range profiles {
		if defaultName != nil && profile.Name == *defaultName {
			fallback = profile
			break
		}
	}
	if fallback.Name == "" && len(profiles) > 0 {
		fallback = profiles[0]
	}
	if plannerRuntime == model.RuntimeManual || !model.ValidRuntime(plannerRuntime) {
		plannerRuntime = model.RuntimeCodex
	}
	if fallback.Name == "" {
		fallback = ProfileRoute{Name: string(plannerRuntime) + "-worker", Runtime: plannerRuntime}
	}
	orchestrator := fallback
	for _, profile := range profiles {
		if orchestratorName != nil && profile.Name == *orchestratorName {
			orchestrator = profile
			break
		}
	}
	return fallback, orchestrator
}
