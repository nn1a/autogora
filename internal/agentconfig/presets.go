package agentconfig

import (
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

// Preset describes a built-in agent registry. Presets intentionally omit
// model and provider pins so each coding-agent CLI can use its current default.
type Preset struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Defaults    Defaults `json:"defaults"`
	Agents      []Agent  `json:"agents"`
}

// PresetApplyOptions controls how a preset is merged into an existing
// configuration. The default merge only adds missing agents and fills empty
// role defaults. ReplaceExisting must be selected explicitly to replace agents
// with matching IDs and role defaults.
type PresetApplyOptions struct {
	Detections      []Detection
	ReplaceExisting bool
}

var builtinPresets = []Preset{
	singleRuntimePreset(
		"codex",
		"One Codex agent for planning, implementation, exceptional recovery, and judging.",
		"codex",
		model.RuntimeCodex,
	),
	singleRuntimePreset(
		"claude",
		"One Claude agent for planning, implementation, exceptional recovery, and judging.",
		"claude",
		model.RuntimeClaude,
	),
	dualRuntimePreset(
		"codex-claude",
		"Codex leads planning and implementation; Claude provides fallback capacity and first-pass recovery and judging.",
		"codex",
		model.RuntimeCodex,
		"claude",
		model.RuntimeClaude,
	),
	dualRuntimePreset(
		"claude-codex",
		"Claude leads planning and implementation; Codex provides fallback capacity and first-pass recovery and judging.",
		"claude",
		model.RuntimeClaude,
		"codex",
		model.RuntimeCodex,
	),
}

func singleRuntimePreset(id, description, agentID string, runtime model.Runtime) Preset {
	return Preset{
		ID:          id,
		Description: description,
		Defaults: Defaults{
			WorkerAgents:      []string{agentID},
			PlannerAgents:     []string{agentID},
			CoordinatorAgents: []string{agentID},
			JudgeAgents:       []string{agentID},
		},
		Agents: []Agent{presetAgent(agentID, runtime, nil)},
	}
}

func dualRuntimePreset(
	id, description, primaryID string,
	primaryRuntime model.Runtime,
	secondaryID string,
	secondaryRuntime model.Runtime,
) Preset {
	return Preset{
		ID:          id,
		Description: description,
		Defaults: Defaults{
			WorkerAgents:      []string{primaryID, secondaryID},
			PlannerAgents:     []string{primaryID, secondaryID},
			CoordinatorAgents: []string{secondaryID, primaryID},
			JudgeAgents:       []string{secondaryID, primaryID},
		},
		Agents: []Agent{
			presetAgent(primaryID, primaryRuntime, []string{secondaryID}),
			presetAgent(secondaryID, secondaryRuntime, nil),
		},
	}
}

func presetAgent(id string, runtime model.Runtime, fallbacks []string) Agent {
	return Agent{
		ID:            id,
		Runtime:       runtime,
		Command:       string(runtime),
		Enabled:       true,
		MaxConcurrent: 1,
		Roles:         []Role{RoleWorker, RolePlanner, RoleCoordinator, RoleJudge},
		Fallbacks:     append([]string{}, fallbacks...),
	}
}

// BuiltinPresets returns independent copies so callers may prepare previews
// without changing the process-wide catalog.
func BuiltinPresets() []Preset {
	result := make([]Preset, len(builtinPresets))
	for index, preset := range builtinPresets {
		result[index] = clonePreset(preset)
	}
	return result
}

// FindPreset resolves a built-in preset by its case-insensitive ID.
func FindPreset(id string) (Preset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, preset := range builtinPresets {
		if preset.ID == id {
			return clonePreset(preset), true
		}
	}
	return Preset{}, false
}

// BuildPreset creates a standalone normalized configuration. A nil detections
// slice means availability was not checked and leaves all preset agents
// enabled. A non-nil slice is authoritative: missing executables disable their
// agents, while installed executables replace the default command name.
func BuildPreset(id string, detections []Detection) (Config, error) {
	preset, found := FindPreset(id)
	if !found {
		return Config{}, fmt.Errorf("unknown agent preset %q", strings.TrimSpace(id))
	}
	config := Default()
	config.Defaults = cloneDefaults(preset.Defaults)
	config.Agents = cloneAgents(preset.Agents)
	applyPresetDetections(config.Agents, detections)
	config = Normalize(config)
	if err := Validate(config); err != nil {
		return Config{}, fmt.Errorf("validate agent preset %q: %w", preset.ID, err)
	}
	return config, nil
}

// ApplyPreset returns a new configuration without writing it to disk. By
// default, existing agents and non-empty role defaults win. Explicit
// ReplaceExisting replaces matching preset agents and all role defaults while
// preserving supervisor settings and unrelated agents.
func ApplyPreset(current Config, id string, options PresetApplyOptions) (Config, error) {
	current = Normalize(cloneConfig(current))
	if err := Validate(current); err != nil {
		return Config{}, fmt.Errorf("validate existing agent configuration: %w", err)
	}
	generated, err := BuildPreset(id, options.Detections)
	if err != nil {
		return Config{}, err
	}

	result := cloneConfig(current)
	indexByID := make(map[string]int, len(result.Agents))
	for index, agent := range result.Agents {
		indexByID[agent.ID] = index
	}
	for _, agent := range generated.Agents {
		index, exists := indexByID[agent.ID]
		if exists && !options.ReplaceExisting {
			continue
		}
		if exists {
			result.Agents[index] = cloneAgent(agent)
			continue
		}
		indexByID[agent.ID] = len(result.Agents)
		result.Agents = append(result.Agents, cloneAgent(agent))
	}

	if options.ReplaceExisting {
		result.Defaults = cloneDefaults(generated.Defaults)
	} else {
		if len(result.Defaults.WorkerAgents) == 0 {
			result.Defaults.WorkerAgents = append([]string{}, generated.Defaults.WorkerAgents...)
		}
		if len(result.Defaults.PlannerAgents) == 0 {
			result.Defaults.PlannerAgents = append([]string{}, generated.Defaults.PlannerAgents...)
		}
		if len(result.Defaults.CoordinatorAgents) == 0 {
			result.Defaults.CoordinatorAgents = append([]string{}, generated.Defaults.CoordinatorAgents...)
		}
		if len(result.Defaults.JudgeAgents) == 0 {
			result.Defaults.JudgeAgents = append([]string{}, generated.Defaults.JudgeAgents...)
		}
	}

	result = Normalize(result)
	if err := Validate(result); err != nil {
		return Config{}, fmt.Errorf("apply agent preset %q: %w", strings.TrimSpace(id), err)
	}
	return result, nil
}

func applyPresetDetections(agents []Agent, detections []Detection) {
	if detections == nil {
		return
	}
	byRuntime := make(map[model.Runtime]Detection, len(detections))
	for _, detection := range detections {
		byRuntime[detection.Runtime] = detection
	}
	for index := range agents {
		agent := &agents[index]
		detection, found := byRuntime[agent.Runtime]
		available := found && detection.State != "missing" && strings.TrimSpace(detection.Executable) != ""
		agent.Enabled = available
		if available {
			agent.Command = strings.TrimSpace(detection.Executable)
		}
	}
}

func clonePreset(preset Preset) Preset {
	preset.Defaults = cloneDefaults(preset.Defaults)
	preset.Agents = cloneAgents(preset.Agents)
	return preset
}

func cloneConfig(config Config) Config {
	config.Defaults = cloneDefaults(config.Defaults)
	config.Agents = cloneAgents(config.Agents)
	return config
}

func cloneDefaults(defaults Defaults) Defaults {
	defaults.WorkerAgents = cloneStrings(defaults.WorkerAgents)
	defaults.PlannerAgents = cloneStrings(defaults.PlannerAgents)
	defaults.CoordinatorAgents = cloneStrings(defaults.CoordinatorAgents)
	defaults.JudgeAgents = cloneStrings(defaults.JudgeAgents)
	return defaults
}

func cloneAgents(agents []Agent) []Agent {
	if agents == nil {
		return nil
	}
	result := make([]Agent, len(agents))
	for index, agent := range agents {
		result[index] = cloneAgent(agent)
	}
	return result
}

func cloneAgent(agent Agent) Agent {
	if agent.Roles != nil {
		agent.Roles = append([]Role{}, agent.Roles...)
	}
	agent.Fallbacks = cloneStrings(agent.Fallbacks)
	return agent
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}
