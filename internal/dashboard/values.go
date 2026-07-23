package dashboard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func intValue(value any, fallback int) int {
	switch parsed := value.(type) {
	case float64:
		return int(parsed)
	case int:
		return parsed
	default:
		return fallback
	}
}

func boolValue(value any, fallback bool) bool {
	result, ok := value.(bool)
	if !ok {
		return fallback
	}
	return result
}

func stringArray(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string{}, strings...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func stringMap(value any) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("expectedUpdatedAt must be an object keyed by task id")
	}
	result := make(map[string]string, len(items))
	for key, value := range items {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expectedUpdatedAt for %s must be a string", key)
		}
		result[key] = text
	}
	return result, nil
}

func runtimeValue(value any) model.Runtime {
	runtime := model.Runtime(stringValue(value))
	if runtime != "" && model.ValidRuntime(runtime) {
		return runtime
	}
	return ""
}

func statusValue(value any) model.TaskStatus {
	status := model.TaskStatus(stringValue(value))
	if status != "" && model.ValidTaskStatus(status) {
		return status
	}
	return ""
}

func optionalString(body map[string]any, key string) store.OptionalString {
	value, exists := body[key]
	if !exists {
		return store.OptionalString{}
	}
	result := store.OptionalString{Set: true}
	if text, ok := value.(string); ok {
		result.Value = &text
	}
	return result
}

func optionalInt(body map[string]any, key string) store.OptionalInt {
	value, exists := body[key]
	if !exists {
		return store.OptionalInt{}
	}
	result := store.OptionalInt{Set: true}
	if value != nil {
		parsed := intValue(value, 0)
		result.Value = &parsed
	}
	return result
}

func stringPointerFrom(body map[string]any, key string) *string {
	value, exists := body[key]
	if !exists {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func intPointerFrom(body map[string]any, key string) *int {
	value, exists := body[key]
	if !exists || value == nil {
		return nil
	}
	parsed := intValue(value, 0)
	return &parsed
}

func boolPointerFrom(body map[string]any, key string) *bool {
	value, exists := body[key]
	if !exists {
		return nil
	}
	parsed, ok := value.(bool)
	if !ok {
		return nil
	}
	return &parsed
}

func taskUpdate(body map[string]any) store.UpdateTaskInput {
	input := store.UpdateTaskInput{ExpectedUpdatedAt: stringPointerFrom(body, "expectedUpdatedAt"), Title: stringPointerFrom(body, "title"), Body: stringPointerFrom(body, "body"),
		Assignee: optionalString(body, "assignee"), Tenant: optionalString(body, "tenant"), Workspace: optionalString(body, "workspace"),
		Branch: optionalString(body, "branch"), ScheduledAt: optionalString(body, "scheduledAt"), MaxRuntimeSeconds: optionalInt(body, "maxRuntimeSeconds"),
		MaxRetries: intPointerFrom(body, "maxRetries"), GoalMode: boolPointerFrom(body, "goalMode"), GoalMaxTurns: intPointerFrom(body, "goalMaxTurns")}
	if runtime := runtimeValue(body["runtime"]); runtime != "" {
		input.Runtime = &runtime
	}
	if priority := intPointerFrom(body, "priority"); priority != nil {
		input.Priority = priority
	}
	if value, exists := body["workspaceKind"]; exists {
		kind := model.WorkspaceKind(stringValue(value))
		input.WorkspaceKind = &kind
	}
	if value, exists := body["skills"]; exists {
		skills := stringArray(value)
		input.Skills = &skills
	}
	if status := statusValue(body["status"]); status != "" {
		input.Status = &status
	}
	return input
}

func orchestrationUpdate(value any) (*boards.OrchestrationUpdate, error) {
	body, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	update := &boards.OrchestrationUpdate{AutoDecompose: boolPointerFrom(body, "autoDecompose"), AutoDecomposePerTick: intPointerFrom(body, "autoDecomposePerTick"), AutoPromoteChildren: boolPointerFrom(body, "autoPromoteChildren"),
		PlannerModel: stringPointerFrom(body, "plannerModel"), PlannerProvider: stringPointerFrom(body, "plannerProvider")}
	if raw, exists := body["plannerRuntime"]; exists {
		runtime := runtimeValue(raw)
		if runtime == "" || runtime == model.RuntimeManual {
			return nil, errors.New("invalid planner runtime")
		}
		update.PlannerRuntime = &runtime
	}
	update.DefaultProfile = optionalString(body, "defaultProfile")
	update.FinalizerProfile = optionalString(body, "finalizerProfile")
	autopilot, err := autopilotUpdate(body["autopilot"])
	if err != nil {
		return nil, err
	}
	update.Autopilot = autopilot
	if raw, exists := body["profiles"]; exists {
		items, ok := raw.([]any)
		if !ok {
			return nil, errors.New("profiles must be an array")
		}
		profiles := make([]boards.Profile, 0, len(items))
		for _, item := range items {
			record, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("profile route must be an object")
			}
			name, runtime := strings.TrimSpace(stringValue(record["name"])), runtimeValue(record["runtime"])
			if name == "" || runtime == "" || runtime == model.RuntimeManual {
				return nil, errors.New("profile route requires name and a worker runtime")
			}
			profiles = append(profiles, boards.Profile{Name: name, Runtime: runtime, Model: stringValue(record["model"]), Provider: stringValue(record["provider"]),
				Description: stringValue(record["description"]), Disabled: boolValue(record["disabled"], false),
				MaxConcurrent: intValue(record["maxConcurrent"], 0), Priority: intValue(record["priority"], 0), Fallbacks: stringArray(record["fallbacks"])})
		}
		update.Profiles = &profiles
	}
	return update, nil
}

func autopilotUpdate(value any) (*boards.AutopilotUpdate, error) {
	body, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	update := &boards.AutopilotUpdate{
		Enabled: boolPointerFrom(body, "enabled"), AutoPlan: boolPointerFrom(body, "autoPlan"),
		AutoExecute: boolPointerFrom(body, "autoExecute"), WorkspaceWrites: boolPointerFrom(body, "workspaceWrites"),
		ReviewGate: boolPointerFrom(body, "reviewGate"),
	}
	if raw, exists := body["coordination"]; exists {
		coordination, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("autopilot coordination must be an object")
		}
		mode := boards.CoordinationMode(strings.TrimSpace(stringValue(coordination["mode"])))
		if _, provided := coordination["mode"]; provided {
			if mode != boards.CoordinationModeObserve && mode != boards.CoordinationModeAssist && mode != boards.CoordinationModeAuto {
				return nil, errors.New("coordination mode must be observe, assist, or auto")
			}
		}
		coordinationUpdate := &boards.CoordinationUpdate{
			Profile:               optionalString(coordination, "profile"),
			IdleSeconds:           intPointerFrom(coordination, "idleSeconds"),
			MaxCallsPerHour:       intPointerFrom(coordination, "maxCallsPerHour"),
			MaxActionsPerIncident: intPointerFrom(coordination, "maxActionsPerIncident"),
		}
		if _, provided := coordination["mode"]; provided {
			coordinationUpdate.Mode = &mode
		}
		if coordinationUpdate.IdleSeconds != nil && *coordinationUpdate.IdleSeconds < boards.MinCoordinationIdleSeconds {
			return nil, fmt.Errorf("coordination idleSeconds must be at least %d", boards.MinCoordinationIdleSeconds)
		}
		if coordinationUpdate.MaxCallsPerHour != nil && (*coordinationUpdate.MaxCallsPerHour < 1 ||
			*coordinationUpdate.MaxCallsPerHour > boards.MaxCoordinationCallsPerHour) {
			return nil, fmt.Errorf("coordination maxCallsPerHour must be between 1 and %d", boards.MaxCoordinationCallsPerHour)
		}
		if coordinationUpdate.MaxActionsPerIncident != nil && (*coordinationUpdate.MaxActionsPerIncident < 1 ||
			*coordinationUpdate.MaxActionsPerIncident > boards.MaxCoordinationActionsPerIncident) {
			return nil, fmt.Errorf("coordination maxActionsPerIncident must be between 1 and %d", boards.MaxCoordinationActionsPerIncident)
		}
		update.Coordination = coordinationUpdate
	}
	if raw, exists := body["publication"]; exists {
		publication, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("autopilot publication must be an object")
		}
		mode := boards.PublicationMode(strings.TrimSpace(stringValue(publication["mode"])))
		if _, provided := publication["mode"]; provided {
			if mode != boards.PublicationModeManual && mode != boards.PublicationModeLocalFF && mode != boards.PublicationModePullRequest {
				return nil, errors.New("publication mode must be manual, local_ff, or pull_request")
			}
		}
		publicationUpdate := &boards.PublicationUpdate{
			TargetBranch: stringPointerFrom(publication, "targetBranch"), Remote: stringPointerFrom(publication, "remote"),
			RequireApproval: boolPointerFrom(publication, "requireApproval"),
		}
		if _, provided := publication["mode"]; provided {
			publicationUpdate.Mode = &mode
		}
		update.Publication = publicationUpdate
	}
	return update, nil
}

func boardUpdate(body map[string]any) (boards.Update, error) {
	update := boards.Update{Name: stringPointerFrom(body, "name"), Description: stringPointerFrom(body, "description"), Icon: stringPointerFrom(body, "icon"), Color: stringPointerFrom(body, "color"), DefaultWorkdir: optionalString(body, "defaultWorkdir")}
	orchestration, err := orchestrationUpdate(body["orchestration"])
	if err != nil {
		return boards.Update{}, err
	}
	update.Orchestration = orchestration
	return update, nil
}

func requireSort(value string) (string, error) {
	if value == "" {
		return "priority-desc", nil
	}
	valid := map[string]bool{"created": true, "created-desc": true, "priority": true, "priority-desc": true, "status": true, "assignee": true, "title": true, "updated": true}
	if !valid[value] {
		return "", fmt.Errorf("invalid task sort: %s", value)
	}
	return value, nil
}
