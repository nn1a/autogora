package terminalui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/supervisor"
)

type globalAgentsTab int

const (
	globalAgentsRegistry globalAgentsTab = iota
	globalAgentsDefaults
	globalAgentsSupervisor
	globalAgentsPresets
)

var globalAgentsTabLabels = []string{"Agents", "Defaults", "Supervisor", "Presets"}

type globalAgentField int

const (
	globalAgentSelection globalAgentField = iota
	globalAgentID
	globalAgentRuntime
	globalAgentCommand
	globalAgentModel
	globalAgentProvider
	globalAgentEnabled
	globalAgentMaxConcurrent
	globalAgentRoles
	globalAgentFallbacks

	globalDefaultWorkers
	globalDefaultPlanners
	globalDefaultCoordinators
	globalDefaultJudges

	globalSupervisorAutoStart
	globalSupervisorMaxWorkers
	globalSupervisorAllowWrites

	globalPresetSelection
	globalPresetMode
)

var (
	globalAgentRegistryFields = []globalAgentField{
		globalAgentSelection, globalAgentID, globalAgentRuntime, globalAgentCommand,
		globalAgentModel, globalAgentProvider, globalAgentEnabled,
		globalAgentMaxConcurrent, globalAgentRoles, globalAgentFallbacks,
	}
	globalAgentDefaultFields = []globalAgentField{
		globalDefaultWorkers, globalDefaultPlanners,
		globalDefaultCoordinators, globalDefaultJudges,
	}
	globalAgentSupervisorFields = []globalAgentField{
		globalSupervisorAutoStart, globalSupervisorMaxWorkers,
		globalSupervisorAllowWrites,
	}
	globalAgentPresetFields = []globalAgentField{
		globalPresetSelection, globalPresetMode,
	}
)

type settingsAgent struct {
	agent         agentconfig.Agent
	maxConcurrent string
	roles         string
	fallbacks     string
	originalID    string
}

type globalAgentsForm struct {
	path        string
	exists      bool
	revision    agentconfig.Revision
	draft       agentconfig.Config
	agents      []settingsAgent
	removedIDs  map[string]bool
	presets     []agentconfig.Preset
	detections  []agentconfig.Detection
	status      supervisor.Status
	activeRuns  int
	tab         globalAgentsTab
	focus       globalAgentField
	agentIndex  int
	presetIndex int
	replace     bool
	inputs      map[globalAgentField]textinput.Model
	scroll      int
	err         error
	saving      bool
	detecting   bool
	lifecycle   string
	notice      string
	help        bool
}

func cloneGlobalAgentConfig(config agentconfig.Config) agentconfig.Config {
	config.Defaults.WorkerAgents = append([]string{}, config.Defaults.WorkerAgents...)
	config.Defaults.PlannerAgents = append([]string{}, config.Defaults.PlannerAgents...)
	config.Defaults.CoordinatorAgents = append([]string{}, config.Defaults.CoordinatorAgents...)
	config.Defaults.JudgeAgents = append([]string{}, config.Defaults.JudgeAgents...)
	config.Agents = append([]agentconfig.Agent{}, config.Agents...)
	for index := range config.Agents {
		config.Agents[index].Roles = append([]agentconfig.Role{}, config.Agents[index].Roles...)
		config.Agents[index].Fallbacks = append([]string{}, config.Agents[index].Fallbacks...)
	}
	return config
}

func newGlobalAgentsForm(value GlobalAgentsContext) *globalAgentsForm {
	form := &globalAgentsForm{
		path: value.Path, exists: value.Exists,
		revision: value.Revision,
		presets:  append([]agentconfig.Preset{}, value.Presets...),
		status:   value.Supervisor, activeRuns: value.ActiveRuns,
		inputs: map[globalAgentField]textinput.Model{},
	}
	if len(form.presets) == 0 {
		form.presets = agentconfig.BuiltinPresets()
	}
	form.replaceConfig(value.Config)
	form.focus = form.fields()[0]
	form.syncFocus()
	return form
}

func (f *globalAgentsForm) replaceConfig(config agentconfig.Config) {
	f.draft = cloneGlobalAgentConfig(config)
	f.removedIDs = map[string]bool{}
	f.agents = make([]settingsAgent, 0, len(config.Agents))
	for _, agent := range config.Agents {
		f.agents = append(f.agents, settingsAgent{
			agent: agent, maxConcurrent: strconv.Itoa(agent.MaxConcurrent),
			roles:     strings.Join(agentRolesToStrings(agent.Roles), ", "),
			fallbacks: strings.Join(agent.Fallbacks, ", "), originalID: agent.ID,
		})
	}
	f.agentIndex = max(0, min(len(f.agents)-1, f.agentIndex))
	f.inputs[globalDefaultWorkers] = newBoardSettingsInput(
		strings.Join(config.Defaults.WorkerAgents, ", "), "ordered agent IDs", 2000,
	)
	f.inputs[globalDefaultPlanners] = newBoardSettingsInput(
		strings.Join(config.Defaults.PlannerAgents, ", "), "ordered agent IDs", 2000,
	)
	f.inputs[globalDefaultCoordinators] = newBoardSettingsInput(
		strings.Join(config.Defaults.CoordinatorAgents, ", "), "ordered agent IDs", 2000,
	)
	f.inputs[globalDefaultJudges] = newBoardSettingsInput(
		strings.Join(config.Defaults.JudgeAgents, ", "), "ordered agent IDs", 2000,
	)
	f.inputs[globalSupervisorMaxWorkers] = newBoardSettingsInput(
		strconv.Itoa(config.Supervisor.MaxWorkers), "at least 1", 8,
	)
	f.loadAgentInputs()
}

func agentRolesToStrings(roles []agentconfig.Role) []string {
	result := make([]string, len(roles))
	for index, role := range roles {
		result[index] = string(role)
	}
	return result
}

func settingsAgentRoles(value string) []agentconfig.Role {
	items := splitSettingsList(value)
	result := make([]agentconfig.Role, len(items))
	for index, item := range items {
		result[index] = agentconfig.Role(strings.ToLower(item))
	}
	return result
}

func (f *globalAgentsForm) fields() []globalAgentField {
	switch f.tab {
	case globalAgentsDefaults:
		return globalAgentDefaultFields
	case globalAgentsSupervisor:
		return globalAgentSupervisorFields
	case globalAgentsPresets:
		return globalAgentPresetFields
	default:
		if len(f.agents) == 0 {
			return globalAgentRegistryFields[:1]
		}
		return globalAgentRegistryFields
	}
}

func (f *globalAgentsForm) textField(field globalAgentField) bool {
	_, exists := f.inputs[field]
	return exists
}

func (f *globalAgentsForm) syncFocus() tea.Cmd {
	commands := []tea.Cmd{}
	for field, input := range f.inputs {
		if field == f.focus {
			commands = append(commands, input.Focus())
		} else {
			input.Blur()
		}
		f.inputs[field] = input
	}
	return tea.Batch(commands...)
}

func (f *globalAgentsForm) moveFocus(delta int) tea.Cmd {
	fields := f.fields()
	current := 0
	for index, field := range fields {
		if field == f.focus {
			current = index
			break
		}
	}
	f.focus = fields[cycle(current, len(fields), delta)]
	return f.syncFocus()
}

func (f *globalAgentsForm) moveTab(delta int) tea.Cmd {
	f.tab = globalAgentsTab(cycle(int(f.tab), len(globalAgentsTabLabels), delta))
	f.scroll = 0
	f.focus = f.fields()[0]
	f.err, f.notice = nil, ""
	return f.syncFocus()
}

func (f *globalAgentsForm) loadAgentInputs() {
	agentFields := []globalAgentField{
		globalAgentID, globalAgentCommand, globalAgentModel, globalAgentProvider,
		globalAgentMaxConcurrent, globalAgentRoles, globalAgentFallbacks,
	}
	if len(f.agents) == 0 {
		for _, field := range agentFields {
			delete(f.inputs, field)
		}
		f.agentIndex = 0
		return
	}
	f.agentIndex = max(0, min(len(f.agents)-1, f.agentIndex))
	agent := f.agents[f.agentIndex]
	values := map[globalAgentField]string{
		globalAgentID: agent.agent.ID, globalAgentCommand: agent.agent.Command,
		globalAgentModel: agent.agent.Model, globalAgentProvider: agent.agent.Provider,
		globalAgentMaxConcurrent: agent.maxConcurrent,
		globalAgentRoles:         agent.roles, globalAgentFallbacks: agent.fallbacks,
	}
	placeholders := map[globalAgentField]string{
		globalAgentID:            "lowercase agent ID",
		globalAgentCommand:       "executable name or absolute path",
		globalAgentModel:         "empty = coding-agent CLI default",
		globalAgentProvider:      "empty = coding-agent CLI default",
		globalAgentMaxConcurrent: "at least 1",
		globalAgentRoles:         "worker, planner, coordinator, judge",
		globalAgentFallbacks:     "ordered fallback agent IDs",
	}
	limits := map[globalAgentField]int{
		globalAgentID: 64, globalAgentCommand: 1000, globalAgentModel: 200,
		globalAgentProvider: 200, globalAgentMaxConcurrent: 8,
		globalAgentRoles: 200, globalAgentFallbacks: 2000,
	}
	for field, value := range values {
		f.inputs[field] = newBoardSettingsInput(value, placeholders[field], limits[field])
	}
}

func (f *globalAgentsForm) setInput(field globalAgentField, value string) {
	input, exists := f.inputs[field]
	if !exists {
		input = newBoardSettingsInput("", "", 2000)
	}
	input.SetValue(value)
	input.CursorEnd()
	f.inputs[field] = input
}

func removeSettingsListValue(values []string, removed string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

func (f *globalAgentsForm) removeAgentReferences(removed string) {
	for _, field := range []globalAgentField{
		globalDefaultWorkers, globalDefaultPlanners,
		globalDefaultCoordinators, globalDefaultJudges,
	} {
		values := removeSettingsListValue(splitSettingsList(f.inputs[field].Value()), removed)
		f.setInput(field, strings.Join(values, ", "))
	}
	for index := range f.agents {
		values := removeSettingsListValue(splitSettingsList(f.agents[index].fallbacks), removed)
		f.agents[index].fallbacks = strings.Join(values, ", ")
	}
}

func (f *globalAgentsForm) writeAgentInput(field globalAgentField, value string) {
	if len(f.agents) == 0 {
		return
	}
	agent := &f.agents[f.agentIndex]
	switch field {
	case globalAgentID:
		agent.agent.ID = value
	case globalAgentCommand:
		agent.agent.Command = value
	case globalAgentModel:
		agent.agent.Model = value
	case globalAgentProvider:
		agent.agent.Provider = value
	case globalAgentMaxConcurrent:
		agent.maxConcurrent = value
	case globalAgentRoles:
		agent.roles = value
	case globalAgentFallbacks:
		agent.fallbacks = value
	}
}

func (f *globalAgentsForm) addAgent() tea.Cmd {
	used := map[string]bool{}
	for _, agent := range f.agents {
		used[strings.TrimSpace(agent.agent.ID)] = true
	}
	id := "agent"
	for suffix := 2; used[id]; suffix++ {
		id = fmt.Sprintf("agent-%d", suffix)
	}
	f.agents = append(f.agents, settingsAgent{
		agent: agentconfig.Agent{
			ID: id, Runtime: model.RuntimeCodex, Command: string(model.RuntimeCodex),
			Enabled: true, MaxConcurrent: 1,
			Roles: []agentconfig.Role{
				agentconfig.RoleWorker, agentconfig.RolePlanner,
				agentconfig.RoleCoordinator, agentconfig.RoleJudge,
			},
		},
		maxConcurrent: "1",
		roles:         "worker, planner, coordinator, judge",
	})
	f.agentIndex = len(f.agents) - 1
	f.loadAgentInputs()
	f.focus = globalAgentID
	f.err, f.notice = nil, ""
	return f.syncFocus()
}

func (f *globalAgentsForm) removeAgent() tea.Cmd {
	if len(f.agents) == 0 {
		return nil
	}
	originalID := strings.TrimSpace(f.agents[f.agentIndex].originalID)
	currentID := strings.TrimSpace(f.agents[f.agentIndex].agent.ID)
	removedIDs := []string{}
	if originalID != "" {
		removedIDs = append(removedIDs, originalID)
	}
	currentUsedByAnotherAgent := false
	for index, agent := range f.agents {
		if index == f.agentIndex {
			continue
		}
		if strings.TrimSpace(agent.originalID) == currentID ||
			strings.TrimSpace(agent.agent.ID) == currentID {
			currentUsedByAnotherAgent = true
			break
		}
	}
	if currentID != "" && currentID != originalID && !currentUsedByAnotherAgent {
		removedIDs = append(removedIDs, currentID)
	}
	for _, removed := range removedIDs {
		if removed == "" {
			continue
		}
		f.removedIDs[removed] = true
		f.removeAgentReferences(removed)
	}
	f.agents = append(f.agents[:f.agentIndex], f.agents[f.agentIndex+1:]...)
	if f.agentIndex >= len(f.agents) {
		f.agentIndex = max(0, len(f.agents)-1)
	}
	f.loadAgentInputs()
	f.focus = globalAgentSelection
	f.err, f.notice = nil, ""
	return f.syncFocus()
}

func (f *globalAgentsForm) cycleSelection(field globalAgentField, delta int) tea.Cmd {
	switch field {
	case globalAgentSelection:
		if len(f.agents) > 0 {
			f.agentIndex = cycle(f.agentIndex, len(f.agents), delta)
			f.loadAgentInputs()
		}
	case globalAgentRuntime:
		if len(f.agents) > 0 {
			runtime := f.agents[f.agentIndex].agent.Runtime
			index := 0
			for candidate, value := range model.WorkerRuntimes {
				if value == runtime {
					index = candidate
					break
				}
			}
			selected := model.WorkerRuntimes[cycle(index, len(model.WorkerRuntimes), delta)]
			agent := &f.agents[f.agentIndex]
			oldRuntime := agent.agent.Runtime
			agent.agent.Runtime = selected
			if command := strings.TrimSpace(agent.agent.Command); command == "" || command == string(oldRuntime) {
				agent.agent.Command = string(selected)
				f.setInput(globalAgentCommand, agent.agent.Command)
			}
		}
	case globalAgentEnabled:
		if len(f.agents) > 0 {
			f.agents[f.agentIndex].agent.Enabled = !f.agents[f.agentIndex].agent.Enabled
		}
	case globalSupervisorAutoStart:
		f.draft.Supervisor.AutoStart = !f.draft.Supervisor.AutoStart
	case globalSupervisorAllowWrites:
		f.draft.Supervisor.AllowWrites = !f.draft.Supervisor.AllowWrites
	case globalPresetSelection:
		if len(f.presets) > 0 {
			f.presetIndex = cycle(f.presetIndex, len(f.presets), delta)
		}
	case globalPresetMode:
		f.replace = !f.replace
	}
	return f.syncFocus()
}

func (f *globalAgentsForm) selectValue(field globalAgentField) string {
	switch field {
	case globalAgentSelection:
		if len(f.agents) == 0 {
			return "No configured agents"
		}
		return fmt.Sprintf("%d/%d · %s", f.agentIndex+1, len(f.agents), f.agents[f.agentIndex].agent.ID)
	case globalAgentRuntime:
		if len(f.agents) > 0 {
			return string(f.agents[f.agentIndex].agent.Runtime)
		}
	case globalAgentEnabled:
		if len(f.agents) > 0 {
			return settingsBool(f.agents[f.agentIndex].agent.Enabled)
		}
	case globalSupervisorAutoStart:
		return settingsBool(f.draft.Supervisor.AutoStart)
	case globalSupervisorAllowWrites:
		return settingsBool(f.draft.Supervisor.AllowWrites)
	case globalPresetSelection:
		if len(f.presets) == 0 {
			return "No built-in presets"
		}
		return fmt.Sprintf("%d/%d · %s", f.presetIndex+1, len(f.presets), f.presets[f.presetIndex].ID)
	case globalPresetMode:
		if f.replace {
			return "replace matching preset agents and preferred lists"
		}
		return "merge missing agents and fill empty preferred lists"
	}
	return ""
}

func (f *globalAgentsForm) buildConfig() (agentconfig.Config, error) {
	config := cloneGlobalAgentConfig(f.draft)
	maxWorkers, err := settingsInteger(
		f.inputs[globalSupervisorMaxWorkers], "Supervisor max workers", 1, 0,
	)
	if err != nil {
		return agentconfig.Config{}, err
	}
	config.Supervisor.MaxWorkers = maxWorkers
	defaults := agentconfig.Defaults{
		WorkerAgents:      splitSettingsList(f.inputs[globalDefaultWorkers].Value()),
		PlannerAgents:     splitSettingsList(f.inputs[globalDefaultPlanners].Value()),
		CoordinatorAgents: splitSettingsList(f.inputs[globalDefaultCoordinators].Value()),
		JudgeAgents:       splitSettingsList(f.inputs[globalDefaultJudges].Value()),
	}
	config.Agents = make([]agentconfig.Agent, 0, len(f.agents))
	renamed := make(map[string]string, len(f.agents))
	finalIDs := make(map[string]string, len(f.agents))
	for index, value := range f.agents {
		maxConcurrent, parseErr := strconv.Atoi(strings.TrimSpace(value.maxConcurrent))
		if parseErr != nil || maxConcurrent < 1 {
			return agentconfig.Config{}, fmt.Errorf(
				"Agent %d max concurrent must be at least 1", index+1,
			)
		}
		agent := value.agent
		agent.ID = strings.TrimSpace(agent.ID)
		if agent.ID == "" {
			return agentconfig.Config{}, fmt.Errorf("Agent %d ID cannot be empty", index+1)
		}
		if previous, exists := finalIDs[agent.ID]; exists {
			return agentconfig.Config{}, fmt.Errorf(
				"Agent IDs must be unique: %q is used by %s and agent %d",
				agent.ID, previous, index+1,
			)
		}
		finalIDs[agent.ID] = fmt.Sprintf("agent %d", index+1)
		if value.originalID != "" {
			renamed[value.originalID] = agent.ID
		}
		agent.Command = strings.TrimSpace(agent.Command)
		agent.Model = strings.TrimSpace(agent.Model)
		agent.Provider = strings.TrimSpace(agent.Provider)
		agent.MaxConcurrent = maxConcurrent
		agent.Roles = settingsAgentRoles(value.roles)
		agent.Fallbacks = splitSettingsList(value.fallbacks)
		config.Agents = append(config.Agents, agent)
	}
	remap := func(values []string) []string {
		result := make([]string, 0, len(values))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			if f.removedIDs[value] {
				continue
			}
			if final, exists := renamed[value]; exists {
				value = final
			}
			if value != "" && !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
		return result
	}
	config.Defaults = agentconfig.Defaults{
		WorkerAgents:      remap(defaults.WorkerAgents),
		PlannerAgents:     remap(defaults.PlannerAgents),
		CoordinatorAgents: remap(defaults.CoordinatorAgents),
		JudgeAgents:       remap(defaults.JudgeAgents),
	}
	for index := range config.Agents {
		config.Agents[index].Fallbacks = remap(config.Agents[index].Fallbacks)
	}
	config = agentconfig.Normalize(config)
	if err := agentconfig.Validate(config); err != nil {
		return agentconfig.Config{}, err
	}
	return config, nil
}

func (f *globalAgentsForm) validate() error {
	_, err := f.buildConfig()
	return err
}

func (f *globalAgentsForm) selectedPreset() (agentconfig.Preset, error) {
	if len(f.presets) == 0 {
		return agentconfig.Preset{}, errors.New("No built-in agent presets are available")
	}
	f.presetIndex = max(0, min(len(f.presets)-1, f.presetIndex))
	return f.presets[f.presetIndex], nil
}

func (f *globalAgentsForm) applyPreset(detections []agentconfig.Detection) error {
	current, err := f.buildConfig()
	if err != nil {
		return err
	}
	preset, err := f.selectedPreset()
	if err != nil {
		return err
	}
	config, err := agentconfig.ApplyPreset(current, preset.ID, agentconfig.PresetApplyOptions{
		Detections: detections, ReplaceExisting: f.replace,
	})
	if err != nil {
		return err
	}
	tab, presetIndex, replace := f.tab, f.presetIndex, f.replace
	f.replaceConfig(config)
	f.tab, f.presetIndex, f.replace = tab, presetIndex, replace
	f.detections = append([]agentconfig.Detection{}, detections...)
	f.focus = globalPresetSelection
	f.notice = fmt.Sprintf("Applied %s to the draft; press Ctrl+S to save", preset.ID)
	f.err = nil
	return nil
}

func (f *globalAgentsForm) Update(message tea.KeyMsg) (tea.Cmd, formAction) {
	f.err, f.notice = nil, ""
	switch message.String() {
	case "esc":
		return nil, formCancel
	case "ctrl+s":
		if err := f.validate(); err != nil {
			f.err = err
			return nil, formContinue
		}
		return nil, formSubmit
	case "tab", "down":
		return f.moveFocus(1), formContinue
	case "shift+tab", "up":
		return f.moveFocus(-1), formContinue
	case "ctrl+right":
		return f.moveTab(1), formContinue
	case "ctrl+left":
		return f.moveTab(-1), formContinue
	case "pgdown":
		f.scroll += 5
		return nil, formContinue
	case "pgup":
		f.scroll = max(0, f.scroll-5)
		return nil, formContinue
	case "ctrl+n":
		if f.tab == globalAgentsRegistry {
			return f.addAgent(), formContinue
		}
	case "ctrl+x":
		if f.tab == globalAgentsRegistry {
			return f.removeAgent(), formContinue
		}
	}
	if f.textField(f.focus) {
		if message.String() == "enter" {
			return f.moveFocus(1), formContinue
		}
		input := f.inputs[f.focus]
		var command tea.Cmd
		input, command = input.Update(message)
		f.inputs[f.focus] = input
		f.writeAgentInput(f.focus, input.Value())
		return command, formContinue
	}
	switch message.String() {
	case "left":
		return f.cycleSelection(f.focus, -1), formContinue
	case "right", " ":
		return f.cycleSelection(f.focus, 1), formContinue
	case "enter":
		return f.moveFocus(1), formContinue
	}
	return nil, formContinue
}

func (f *globalAgentsForm) UpdateMessage(message tea.Msg) tea.Cmd {
	if !f.textField(f.focus) {
		return nil
	}
	input := f.inputs[f.focus]
	var command tea.Cmd
	input, command = input.Update(message)
	f.inputs[f.focus] = input
	return command
}
