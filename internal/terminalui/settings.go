package terminalui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

type boardSettingsTab int

const (
	settingsBoard boardSettingsTab = iota
	settingsProfiles
	settingsAutopilot
)

var boardSettingsTabLabels = []string{"Board", "Profiles", "Autopilot"}

type boardSettingField int

const (
	settingAutoDecompose boardSettingField = iota
	settingAutoDecomposePerTick
	settingAutoPromoteChildren
	settingPlannerRuntime
	settingPlannerModel
	settingPlannerProvider
	settingDefaultProfile
	settingFinalizerProfile

	settingProfileSelection
	settingProfileName
	settingProfileRuntime
	settingProfileModel
	settingProfileProvider
	settingProfileDescription
	settingProfileDisabled
	settingProfileMaxConcurrent
	settingProfilePriority
	settingProfileFallbacks

	settingAutopilotEnabled
	settingAutoPlan
	settingAutoExecute
	settingWorkspaceWrites
	settingCoordinationMode
	settingCoordinatorProfile
	settingCoordinationIdleSeconds
	settingCoordinatorCallsPerHour
	settingCoordinatorActionsPerIncident
	settingPublicationMode
	settingPublicationTarget
	settingPublicationRemote
	settingPublicationApproval
)

var (
	boardSettingFields = []boardSettingField{
		settingAutoDecompose, settingAutoDecomposePerTick, settingAutoPromoteChildren,
		settingPlannerRuntime, settingPlannerModel, settingPlannerProvider,
		settingDefaultProfile, settingFinalizerProfile,
	}
	profileSettingFields = []boardSettingField{
		settingProfileSelection, settingProfileName, settingProfileRuntime,
		settingProfileModel, settingProfileProvider, settingProfileDescription,
		settingProfileDisabled, settingProfileMaxConcurrent, settingProfilePriority,
		settingProfileFallbacks,
	}
	autopilotSettingFields = []boardSettingField{
		settingAutopilotEnabled, settingAutoPlan, settingAutoExecute, settingWorkspaceWrites,
		settingCoordinationMode, settingCoordinatorProfile, settingCoordinationIdleSeconds,
		settingCoordinatorCallsPerHour, settingCoordinatorActionsPerIncident,
		settingPublicationMode, settingPublicationTarget, settingPublicationRemote,
		settingPublicationApproval,
	}
	settingsPlannerRuntimes = []model.Runtime{
		model.RuntimeCodex, model.RuntimeClaude, model.RuntimeCline, model.RuntimeGemini,
	}
	settingsCoordinationModes = []boards.CoordinationMode{
		boards.CoordinationModeObserve, boards.CoordinationModeAssist, boards.CoordinationModeAuto,
	}
	settingsPublicationModes = []boards.PublicationMode{
		boards.PublicationModeManual, boards.PublicationModeLocalFF, boards.PublicationModePullRequest,
	}
)

type settingsProfile struct {
	boards.Profile
	maxConcurrent string
	priority      string
	fallbacks     string
	referenceName string
}

type boardSettingsForm struct {
	expected          boards.OrchestrationSettings
	draft             boards.OrchestrationSettings
	inheritedProfiles []orchestration.ProfileRoute
	profiles          []settingsProfile
	tab               boardSettingsTab
	focus             boardSettingField
	profileIndex      int
	inputs            map[boardSettingField]textinput.Model
	scroll            int
	err               error
	saving            bool
	activeRuns        int
}

func cloneOrchestrationSettings(value boards.OrchestrationSettings) boards.OrchestrationSettings {
	copyString := func(value *string) *string {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	value.DefaultProfile = copyString(value.DefaultProfile)
	value.FinalizerProfile = copyString(value.FinalizerProfile)
	value.Autopilot.Coordination.Profile = copyString(value.Autopilot.Coordination.Profile)
	profiles := make([]boards.Profile, len(value.Profiles))
	for index, profile := range value.Profiles {
		profile.Fallbacks = append([]string{}, profile.Fallbacks...)
		profiles[index] = profile
	}
	value.Profiles = profiles
	return value
}

func newBoardSettingsInput(value, placeholder string, limit int) textinput.Model {
	input := newTextInput(value, placeholder, limit)
	input.Prompt = ""
	return input
}

func newBoardSettingsForm(context taskservice.BoardContext) *boardSettingsForm {
	expected := cloneOrchestrationSettings(context.Metadata.Orchestration)
	form := &boardSettingsForm{
		expected: expected,
		draft:    cloneOrchestrationSettings(expected),
		inheritedProfiles: append(
			[]orchestration.ProfileRoute{},
			context.InheritedProfiles...,
		),
		inputs:     map[boardSettingField]textinput.Model{},
		activeRuns: context.ActiveRuns,
	}
	for _, profile := range expected.Profiles {
		form.profiles = append(form.profiles, settingsProfile{
			Profile:       profile,
			maxConcurrent: strconv.Itoa(profile.MaxConcurrent),
			priority:      strconv.Itoa(profile.Priority),
			fallbacks:     strings.Join(profile.Fallbacks, ", "),
			referenceName: strings.TrimSpace(profile.Name),
		})
	}
	form.draft.Profiles = nil
	form.inputs[settingAutoDecomposePerTick] = newBoardSettingsInput(
		strconv.Itoa(expected.AutoDecomposePerTick), "1-100", 3,
	)
	form.inputs[settingPlannerModel] = newBoardSettingsInput(expected.PlannerModel, "CLI default (unpinned)", 200)
	form.inputs[settingPlannerProvider] = newBoardSettingsInput(expected.PlannerProvider, "CLI default", 200)
	coordination := expected.Autopilot.Coordination
	form.inputs[settingCoordinatorProfile] = newBoardSettingsInput(
		pointer(coordination.Profile, ""), "global or board agent ID", 200,
	)
	form.inputs[settingCoordinationIdleSeconds] = newBoardSettingsInput(
		strconv.Itoa(coordination.IdleSeconds), fmt.Sprintf("at least %d", boards.MinCoordinationIdleSeconds), 8,
	)
	form.inputs[settingCoordinatorCallsPerHour] = newBoardSettingsInput(
		strconv.Itoa(coordination.MaxCallsPerHour), fmt.Sprintf("1-%d", boards.MaxCoordinationCallsPerHour), 4,
	)
	form.inputs[settingCoordinatorActionsPerIncident] = newBoardSettingsInput(
		strconv.Itoa(coordination.MaxActionsPerIncident), fmt.Sprintf("1-%d", boards.MaxCoordinationActionsPerIncident), 4,
	)
	form.inputs[settingPublicationTarget] = newBoardSettingsInput(
		expected.Autopilot.Publication.TargetBranch, "main", 300,
	)
	form.inputs[settingPublicationRemote] = newBoardSettingsInput(
		expected.Autopilot.Publication.Remote, "origin", 200,
	)
	form.focus = boardSettingFields[0]
	form.loadProfileInputs()
	form.syncFocus()
	return form
}

func (f *boardSettingsForm) fields() []boardSettingField {
	switch f.tab {
	case settingsProfiles:
		if len(f.profiles) == 0 {
			return profileSettingFields[:1]
		}
		return profileSettingFields
	case settingsAutopilot:
		return autopilotSettingFields
	default:
		return boardSettingFields
	}
}

func (f *boardSettingsForm) textField(field boardSettingField) bool {
	_, exists := f.inputs[field]
	return exists
}

func (f *boardSettingsForm) syncFocus() tea.Cmd {
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

func (f *boardSettingsForm) moveFocus(delta int) tea.Cmd {
	fields := f.fields()
	current := 0
	for index, field := range fields {
		if field == f.focus {
			current = index
			break
		}
	}
	current = cycle(current, len(fields), delta)
	f.focus = fields[current]
	return f.syncFocus()
}

func (f *boardSettingsForm) moveTab(delta int) tea.Cmd {
	f.tab = boardSettingsTab(cycle(int(f.tab), len(boardSettingsTabLabels), delta))
	f.scroll = 0
	f.focus = f.fields()[0]
	return f.syncFocus()
}

func (f *boardSettingsForm) setInput(field boardSettingField, value string) {
	input, exists := f.inputs[field]
	if !exists {
		input = newBoardSettingsInput("", "", 1000)
	}
	input.SetValue(value)
	input.CursorEnd()
	f.inputs[field] = input
}

func (f *boardSettingsForm) loadProfileInputs() {
	if len(f.profiles) == 0 {
		for _, field := range []boardSettingField{
			settingProfileName, settingProfileModel, settingProfileProvider,
			settingProfileDescription, settingProfileMaxConcurrent,
			settingProfilePriority, settingProfileFallbacks,
		} {
			delete(f.inputs, field)
		}
		f.profileIndex = 0
		return
	}
	f.profileIndex = max(0, min(len(f.profiles)-1, f.profileIndex))
	profile := f.profiles[f.profileIndex]
	values := map[boardSettingField]string{
		settingProfileName:          profile.Name,
		settingProfileModel:         profile.Model,
		settingProfileProvider:      profile.Provider,
		settingProfileDescription:   profile.Description,
		settingProfileMaxConcurrent: profile.maxConcurrent,
		settingProfilePriority:      profile.priority,
		settingProfileFallbacks:     profile.fallbacks,
	}
	placeholders := map[boardSettingField]string{
		settingProfileName:          "unique board profile name",
		settingProfileModel:         "CLI default (unpinned)",
		settingProfileProvider:      "CLI default",
		settingProfileDescription:   "when this route should be used",
		settingProfileMaxConcurrent: "0 = no board override",
		settingProfilePriority:      "0",
		settingProfileFallbacks:     "comma-separated profile names",
	}
	limits := map[boardSettingField]int{
		settingProfileName: 200, settingProfileModel: 200, settingProfileProvider: 200,
		settingProfileDescription: 1000, settingProfileMaxConcurrent: 8,
		settingProfilePriority: 8, settingProfileFallbacks: 2000,
	}
	for field, value := range values {
		input := newBoardSettingsInput(value, placeholders[field], limits[field])
		f.inputs[field] = input
	}
}

func (f *boardSettingsForm) writeProfileInput(field boardSettingField, value string) {
	if len(f.profiles) == 0 {
		return
	}
	profile := &f.profiles[f.profileIndex]
	switch field {
	case settingProfileName:
		profile.Name = value
		if renamed := strings.TrimSpace(value); renamed != "" {
			f.renameProfileReference(profile.referenceName, renamed)
			profile.referenceName = renamed
		}
	case settingProfileModel:
		profile.Model = value
	case settingProfileProvider:
		profile.Provider = value
	case settingProfileDescription:
		profile.Description = value
	case settingProfileMaxConcurrent:
		profile.maxConcurrent = value
	case settingProfilePriority:
		profile.priority = value
	case settingProfileFallbacks:
		profile.fallbacks = value
	}
}

func (f *boardSettingsForm) renameProfileReference(old, renamed string) {
	old, renamed = strings.TrimSpace(old), strings.TrimSpace(renamed)
	if old == "" || renamed == "" || old == renamed {
		return
	}
	for _, target := range []**string{&f.draft.DefaultProfile, &f.draft.FinalizerProfile, &f.draft.Autopilot.Coordination.Profile} {
		if *target != nil && strings.TrimSpace(**target) == old {
			value := renamed
			*target = &value
		}
	}
	for index := range f.profiles {
		fallbacks := splitSettingsList(f.profiles[index].fallbacks)
		changed := false
		for fallbackIndex, fallback := range fallbacks {
			if fallback == old {
				fallbacks[fallbackIndex], changed = renamed, true
			}
		}
		if changed {
			f.profiles[index].fallbacks = strings.Join(fallbacks, ", ")
			if index == f.profileIndex {
				f.setInput(settingProfileFallbacks, f.profiles[index].fallbacks)
			}
		}
	}
}

func (f *boardSettingsForm) addProfile() tea.Cmd {
	used := map[string]bool{}
	for _, profile := range f.profiles {
		used[strings.TrimSpace(profile.Name)] = true
	}
	name := "worker"
	for suffix := 2; used[name]; suffix++ {
		name = fmt.Sprintf("worker-%d", suffix)
	}
	runtime := f.draft.PlannerRuntime
	if runtime == model.RuntimeManual || !model.ValidRuntime(runtime) {
		runtime = model.RuntimeCodex
	}
	f.profiles = append(f.profiles, settingsProfile{
		Profile:       boards.Profile{Name: name, Runtime: runtime},
		maxConcurrent: "0", priority: "0",
		referenceName: name,
	})
	f.profileIndex = len(f.profiles) - 1
	f.loadProfileInputs()
	f.focus = settingProfileName
	f.err = nil
	return f.syncFocus()
}

func (f *boardSettingsForm) removeProfile() tea.Cmd {
	if len(f.profiles) == 0 {
		return nil
	}
	removedName := strings.TrimSpace(f.profiles[f.profileIndex].Name)
	f.profiles = append(f.profiles[:f.profileIndex], f.profiles[f.profileIndex+1:]...)
	if removedName != "" && !f.inheritedProfileNamed(removedName) {
		f.removeProfileReferences(removedName)
	}
	if f.profileIndex >= len(f.profiles) {
		f.profileIndex = max(0, len(f.profiles)-1)
	}
	f.loadProfileInputs()
	f.focus = settingProfileSelection
	f.err = nil
	return f.syncFocus()
}

func (f *boardSettingsForm) inheritedProfileNamed(name string) bool {
	for _, profile := range f.inheritedProfiles {
		if strings.TrimSpace(profile.Name) == name {
			return true
		}
	}
	return false
}

func (f *boardSettingsForm) removeProfileReferences(name string) {
	for _, target := range []**string{
		&f.draft.DefaultProfile,
		&f.draft.FinalizerProfile,
		&f.draft.Autopilot.Coordination.Profile,
	} {
		if *target != nil && strings.TrimSpace(**target) == name {
			*target = nil
		}
	}
	for index := range f.profiles {
		fallbacks := splitSettingsList(f.profiles[index].fallbacks)
		filtered := fallbacks[:0]
		for _, fallback := range fallbacks {
			if fallback != name {
				filtered = append(filtered, fallback)
			}
		}
		f.profiles[index].fallbacks = strings.Join(filtered, ", ")
	}
}

func splitSettingsList(value string) []string {
	items := []string{}
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			items = append(items, item)
		}
	}
	return items
}

func settingsBool(value bool) string {
	if value {
		return "[x] enabled"
	}
	return "[ ] disabled"
}

func (f *boardSettingsForm) profileChoices(current *string) []string {
	values := []string{""}
	seen := map[string]bool{"": true}
	for _, profile := range append(append([]orchestration.ProfileRoute{}, f.inheritedProfiles...), f.effectiveLocalProfiles()...) {
		name := strings.TrimSpace(profile.Name)
		if name != "" && !seen[name] {
			seen[name] = true
			values = append(values, name)
		}
	}
	if current != nil {
		name := strings.TrimSpace(*current)
		if name != "" && !seen[name] {
			values = append(values, name)
		}
	}
	return values
}

func (f *boardSettingsForm) effectiveLocalProfiles() []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(f.profiles))
	for _, profile := range f.profiles {
		result = append(result, orchestration.ProfileRoute{
			Name: profile.Name, Runtime: profile.Runtime, Model: profile.Model,
			Provider: profile.Provider, Description: profile.Description,
			Disabled: profile.Disabled, MaxConcurrent: profile.MaxConcurrent,
			Priority: profile.Priority, Fallbacks: splitSettingsList(profile.fallbacks),
		})
	}
	return result
}

func cycleSettingsPointer(current **string, values []string, delta int) {
	value := pointer(*current, "")
	index := optionIndex(values, value)
	index = cycle(index, len(values), delta)
	if values[index] == "" {
		*current = nil
		return
	}
	selected := values[index]
	*current = &selected
}

func (f *boardSettingsForm) cycleSelection(field boardSettingField, delta int) tea.Cmd {
	switch field {
	case settingAutoDecompose:
		f.draft.AutoDecompose = !f.draft.AutoDecompose
	case settingAutoPromoteChildren:
		f.draft.AutoPromoteChildren = !f.draft.AutoPromoteChildren
	case settingPlannerRuntime:
		index := 0
		for candidate, runtime := range settingsPlannerRuntimes {
			if runtime == f.draft.PlannerRuntime {
				index = candidate
			}
		}
		f.draft.PlannerRuntime = settingsPlannerRuntimes[cycle(index, len(settingsPlannerRuntimes), delta)]
	case settingDefaultProfile:
		cycleSettingsPointer(&f.draft.DefaultProfile, f.profileChoices(f.draft.DefaultProfile), delta)
	case settingFinalizerProfile:
		cycleSettingsPointer(&f.draft.FinalizerProfile, f.profileChoices(f.draft.FinalizerProfile), delta)
	case settingProfileSelection:
		if len(f.profiles) > 0 {
			f.profileIndex = cycle(f.profileIndex, len(f.profiles), delta)
			f.loadProfileInputs()
		}
	case settingProfileRuntime:
		if len(f.profiles) > 0 {
			index := 0
			for candidate, runtime := range settingsPlannerRuntimes {
				if runtime == f.profiles[f.profileIndex].Runtime {
					index = candidate
				}
			}
			f.profiles[f.profileIndex].Runtime = settingsPlannerRuntimes[cycle(index, len(settingsPlannerRuntimes), delta)]
		}
	case settingProfileDisabled:
		if len(f.profiles) > 0 {
			f.profiles[f.profileIndex].Disabled = !f.profiles[f.profileIndex].Disabled
		}
	case settingAutopilotEnabled:
		f.draft.Autopilot.Enabled = !f.draft.Autopilot.Enabled
	case settingAutoPlan:
		f.draft.Autopilot.AutoPlan = !f.draft.Autopilot.AutoPlan
	case settingAutoExecute:
		f.draft.Autopilot.AutoExecute = !f.draft.Autopilot.AutoExecute
	case settingWorkspaceWrites:
		f.draft.Autopilot.WorkspaceWrites = !f.draft.Autopilot.WorkspaceWrites
	case settingCoordinationMode:
		index := 0
		for candidate, mode := range settingsCoordinationModes {
			if mode == f.draft.Autopilot.Coordination.Mode {
				index = candidate
			}
		}
		f.draft.Autopilot.Coordination.Mode = settingsCoordinationModes[cycle(index, len(settingsCoordinationModes), delta)]
	case settingPublicationMode:
		index := 0
		for candidate, mode := range settingsPublicationModes {
			if mode == f.draft.Autopilot.Publication.Mode {
				index = candidate
			}
		}
		f.draft.Autopilot.Publication.Mode = settingsPublicationModes[cycle(index, len(settingsPublicationModes), delta)]
	case settingPublicationApproval:
		f.draft.Autopilot.Publication.RequireApproval = !f.draft.Autopilot.Publication.RequireApproval
	}
	return f.syncFocus()
}

func (f *boardSettingsForm) selectValue(field boardSettingField) string {
	switch field {
	case settingAutoDecompose:
		return settingsBool(f.draft.AutoDecompose)
	case settingAutoPromoteChildren:
		return settingsBool(f.draft.AutoPromoteChildren)
	case settingPlannerRuntime:
		return string(f.draft.PlannerRuntime)
	case settingDefaultProfile:
		return pointer(f.draft.DefaultProfile, "Automatic fallback")
	case settingFinalizerProfile:
		return pointer(f.draft.FinalizerProfile, "Use default profile")
	case settingProfileSelection:
		if len(f.profiles) == 0 {
			return "No board-only profiles"
		}
		return fmt.Sprintf("%d/%d · %s", f.profileIndex+1, len(f.profiles), f.profiles[f.profileIndex].Name)
	case settingProfileRuntime:
		if len(f.profiles) > 0 {
			return string(f.profiles[f.profileIndex].Runtime)
		}
	case settingProfileDisabled:
		if len(f.profiles) > 0 {
			if f.profiles[f.profileIndex].Disabled {
				return "[x] disabled"
			}
			return "[ ] active"
		}
	case settingAutopilotEnabled:
		return settingsBool(f.draft.Autopilot.Enabled)
	case settingAutoPlan:
		return settingsBool(f.draft.Autopilot.AutoPlan)
	case settingAutoExecute:
		return settingsBool(f.draft.Autopilot.AutoExecute)
	case settingWorkspaceWrites:
		return settingsBool(f.draft.Autopilot.WorkspaceWrites)
	case settingCoordinationMode:
		return string(f.draft.Autopilot.Coordination.Mode)
	case settingPublicationMode:
		return string(f.draft.Autopilot.Publication.Mode)
	case settingPublicationApproval:
		return settingsBool(f.draft.Autopilot.Publication.RequireApproval)
	}
	return ""
}

func settingsInteger(input textinput.Model, label string, minimum, maximum int) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(input.Value()))
	if err != nil || value < minimum || (maximum > 0 && value > maximum) {
		if maximum > 0 {
			return 0, fmt.Errorf("%s must be between %d and %d", label, minimum, maximum)
		}
		return 0, fmt.Errorf("%s must be at least %d", label, minimum)
	}
	return value, nil
}

func optionalSettingsString(value string) store.OptionalString {
	value = strings.TrimSpace(value)
	if value == "" {
		return store.OptionalString{Set: true}
	}
	return store.OptionalString{Set: true, Value: &value}
}

func (f *boardSettingsForm) buildUpdate() (boards.OrchestrationUpdate, error) {
	perTick, err := settingsInteger(f.inputs[settingAutoDecomposePerTick], "Auto-decompose per tick", 1, 100)
	if err != nil {
		return boards.OrchestrationUpdate{}, err
	}
	idleSeconds, err := settingsInteger(
		f.inputs[settingCoordinationIdleSeconds], "Coordinator idle seconds",
		boards.MinCoordinationIdleSeconds, 0,
	)
	if err != nil {
		return boards.OrchestrationUpdate{}, err
	}
	callsPerHour, err := settingsInteger(
		f.inputs[settingCoordinatorCallsPerHour], "Coordinator calls per hour",
		1, boards.MaxCoordinationCallsPerHour,
	)
	if err != nil {
		return boards.OrchestrationUpdate{}, err
	}
	actionsPerIncident, err := settingsInteger(
		f.inputs[settingCoordinatorActionsPerIncident], "Coordinator actions per incident",
		1, boards.MaxCoordinationActionsPerIncident,
	)
	if err != nil {
		return boards.OrchestrationUpdate{}, err
	}
	target := strings.TrimSpace(f.inputs[settingPublicationTarget].Value())
	if target == "" {
		return boards.OrchestrationUpdate{}, errors.New("Publication target branch cannot be empty")
	}
	remote := strings.TrimSpace(f.inputs[settingPublicationRemote].Value())
	if remote == "" {
		return boards.OrchestrationUpdate{}, errors.New("Publication remote cannot be empty")
	}
	profiles := make([]boards.Profile, 0, len(f.profiles))
	for index, value := range f.profiles {
		name := strings.TrimSpace(value.Name)
		maxConcurrent, parseErr := strconv.Atoi(strings.TrimSpace(value.maxConcurrent))
		if parseErr != nil || maxConcurrent < 0 {
			return boards.OrchestrationUpdate{}, fmt.Errorf("Profile %d max concurrency must be zero or greater", index+1)
		}
		priority, parseErr := strconv.Atoi(strings.TrimSpace(value.priority))
		if parseErr != nil {
			return boards.OrchestrationUpdate{}, fmt.Errorf("Profile %d priority must be an integer", index+1)
		}
		profiles = append(profiles, boards.Profile{
			Name: name, Runtime: value.Runtime, Model: strings.TrimSpace(value.Model),
			Provider: strings.TrimSpace(value.Provider), Description: strings.TrimSpace(value.Description),
			Disabled: value.Disabled, MaxConcurrent: maxConcurrent, Priority: priority,
			Fallbacks: splitSettingsList(value.fallbacks),
		})
	}
	plannerModel := f.inputs[settingPlannerModel].Value()
	plannerProvider := f.inputs[settingPlannerProvider].Value()
	enabled, autoPlan := f.draft.Autopilot.Enabled, f.draft.Autopilot.AutoPlan
	autoExecute, workspaceWrites := f.draft.Autopilot.AutoExecute, f.draft.Autopilot.WorkspaceWrites
	coordinationMode := f.draft.Autopilot.Coordination.Mode
	publicationMode := f.draft.Autopilot.Publication.Mode
	publicationApproval := f.draft.Autopilot.Publication.RequireApproval
	update := boards.OrchestrationUpdate{
		AutoDecompose: &f.draft.AutoDecompose, AutoDecomposePerTick: &perTick,
		AutoPromoteChildren: &f.draft.AutoPromoteChildren,
		PlannerRuntime:      &f.draft.PlannerRuntime,
		PlannerModel:        &plannerModel,
		PlannerProvider:     &plannerProvider,
		DefaultProfile:      optionalSettingsString(pointer(f.draft.DefaultProfile, "")),
		FinalizerProfile:    optionalSettingsString(pointer(f.draft.FinalizerProfile, "")),
		Profiles:            &profiles,
		Autopilot: &boards.AutopilotUpdate{
			Enabled: &enabled, AutoPlan: &autoPlan, AutoExecute: &autoExecute,
			WorkspaceWrites: &workspaceWrites,
			Coordination: &boards.CoordinationUpdate{
				Mode: &coordinationMode,
				Profile: optionalSettingsString(
					f.inputs[settingCoordinatorProfile].Value(),
				),
				IdleSeconds: &idleSeconds, MaxCallsPerHour: &callsPerHour,
				MaxActionsPerIncident: &actionsPerIncident,
			},
			Publication: &boards.PublicationUpdate{
				Mode: &publicationMode, TargetBranch: &target, Remote: &remote,
				RequireApproval: &publicationApproval,
			},
		},
	}
	if err := boards.ValidateOrchestrationUpdate(&update); err != nil {
		return boards.OrchestrationUpdate{}, err
	}
	return update, nil
}

func (f *boardSettingsForm) validate() error {
	_, err := f.buildUpdate()
	return err
}

func (f *boardSettingsForm) Update(message tea.KeyMsg) (tea.Cmd, formAction) {
	f.err = nil
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
		if f.tab == settingsProfiles {
			return f.addProfile(), formContinue
		}
	case "ctrl+x":
		if f.tab == settingsProfiles {
			return f.removeProfile(), formContinue
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
		f.writeProfileInput(f.focus, input.Value())
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

func (f *boardSettingsForm) UpdateMessage(message tea.Msg) tea.Cmd {
	if !f.textField(f.focus) {
		return nil
	}
	input := f.inputs[f.focus]
	var command tea.Cmd
	input, command = input.Update(message)
	f.inputs[f.focus] = input
	return command
}
