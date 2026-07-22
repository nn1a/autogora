package terminalui

import (
	"errors"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

type formField int

const (
	fieldTitle formField = iota
	fieldBody
	fieldStatus
	fieldPriority
	fieldProfile
	fieldAssignee
	fieldRuntime
	fieldSkills
	fieldTenant
	fieldWorkspaceKind
	fieldWorkspace
	fieldGoalMode
)

type formAction int

const (
	formContinue formAction = iota
	formCancel
	formSubmit
)

var (
	formStatuses       = []string{"triage", "todo", "scheduled", "ready", "blocked", "review", "done", "archived"}
	formRuntimes       = []string{"manual", "codex", "claude", "cline", "gemini"}
	formWorkspaceKinds = []string{"scratch", "dir", "worktree"}
)

type taskForm struct {
	mode           string
	taskID         string
	activeRun      bool
	board          string
	profiles       []orchestration.ProfileRoute
	profileIndex   int
	statusIndex    int
	runtimeIndex   int
	workspaceIndex int
	goalMode       bool
	focus          formField
	inputs         map[formField]textinput.Model
	body           textarea.Model
	err            error
}

func newTextInput(value, placeholder string, limit int) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = limit
	input.SetValue(value)
	input.CursorEnd()
	return input
}

func newTaskForm(board string, profiles []orchestration.ProfileRoute, status model.TaskStatus) *taskForm {
	if status == "" || status == model.TaskStatusRunning || status == model.TaskStatusArchived {
		status = model.TaskStatusTriage
	}
	form := &taskForm{
		mode: "create", board: board, profiles: append([]orchestration.ProfileRoute{}, profiles...),
		inputs: map[formField]textinput.Model{
			fieldTitle: newTextInput("", "Task title", 300), fieldPriority: newTextInput("0", "0", 12),
			fieldAssignee: newTextInput("", "profile or worker name", 200), fieldSkills: newTextInput("", "comma-separated", 1000),
			fieldTenant: newTextInput("", "optional", 200), fieldWorkspace: newTextInput("", "absolute path or empty", 2000),
		},
	}
	form.body = textarea.New()
	form.body.Placeholder = "Describe scope, acceptance criteria, and verification"
	form.body.CharLimit = 50_000
	form.body.SetHeight(6)
	form.statusIndex = optionIndex(formStatuses, string(status))
	form.focus = fieldTitle
	form.syncFocus()
	return form
}

func editTaskForm(board string, profiles []orchestration.ProfileRoute, task model.Task) *taskForm {
	form := newTaskForm(board, profiles, task.Status)
	form.mode, form.taskID, form.activeRun = "edit", task.ID, task.CurrentRunID != nil
	form.setInputValue(fieldTitle, task.Title)
	form.setInputValue(fieldPriority, strconv.Itoa(task.Priority))
	form.setInputValue(fieldAssignee, pointer(task.Assignee, ""))
	form.setInputValue(fieldSkills, strings.Join(task.Skills, ", "))
	form.setInputValue(fieldTenant, pointer(task.Tenant, ""))
	form.setInputValue(fieldWorkspace, pointer(task.Workspace, ""))
	form.body.SetValue(task.Body)
	form.runtimeIndex = optionIndex(formRuntimes, string(task.Runtime))
	form.workspaceIndex = optionIndex(formWorkspaceKinds, string(task.WorkspaceKind))
	form.goalMode = task.GoalMode
	for index, profile := range form.profiles {
		if task.Assignee != nil && profile.Name == *task.Assignee && profile.Runtime == task.Runtime {
			form.profileIndex = index + 1
			break
		}
	}
	form.syncFocus()
	return form
}

func (f *taskForm) setInputValue(field formField, value string) {
	input := f.inputs[field]
	input.SetValue(value)
	input.CursorEnd()
	f.inputs[field] = input
}

func optionIndex(options []string, value string) int {
	for index, option := range options {
		if option == value {
			return index
		}
	}
	return 0
}

func (f *taskForm) fields() []formField {
	if f.mode == "create" {
		return []formField{fieldTitle, fieldBody, fieldStatus, fieldPriority, fieldProfile, fieldAssignee, fieldRuntime, fieldSkills, fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldGoalMode}
	}
	return []formField{fieldTitle, fieldBody, fieldPriority, fieldProfile, fieldAssignee, fieldRuntime, fieldSkills, fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldGoalMode}
}

func (f *taskForm) stepFields() [][]formField {
	task := []formField{fieldTitle, fieldBody, fieldPriority}
	if f.mode == "create" {
		task = []formField{fieldTitle, fieldBody, fieldStatus, fieldPriority}
	}
	return [][]formField{
		task,
		{fieldProfile, fieldAssignee, fieldRuntime, fieldSkills},
		{fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldGoalMode},
	}
}

func (f *taskForm) step() int {
	for step, fields := range f.stepFields() {
		for _, field := range fields {
			if field == f.focus {
				return step
			}
		}
	}
	return 0
}

func (f *taskForm) locked(field formField) bool {
	if !f.activeRun {
		return false
	}
	switch field {
	case fieldProfile, fieldAssignee, fieldRuntime, fieldWorkspaceKind, fieldWorkspace:
		return true
	default:
		return false
	}
}

func (f *taskForm) textField(field formField) bool {
	_, exists := f.inputs[field]
	return exists
}

func (f *taskForm) syncFocus() tea.Cmd {
	commands := []tea.Cmd{}
	for field, input := range f.inputs {
		if field == f.focus && !f.locked(field) {
			commands = append(commands, input.Focus())
		} else {
			input.Blur()
		}
		f.inputs[field] = input
	}
	if f.focus == fieldBody {
		commands = append(commands, f.body.Focus())
	} else {
		f.body.Blur()
	}
	return tea.Batch(commands...)
}

func (f *taskForm) moveFocus(delta int) tea.Cmd {
	fields := f.fields()
	current := 0
	for index, field := range fields {
		if field == f.focus {
			current = index
			break
		}
	}
	for range len(fields) {
		current = (current + delta + len(fields)) % len(fields)
		if !f.locked(fields[current]) {
			f.focus = fields[current]
			break
		}
	}
	return f.syncFocus()
}

func (f *taskForm) moveStep(delta int) tea.Cmd {
	steps := f.stepFields()
	next := (f.step() + delta + len(steps)) % len(steps)
	for _, field := range steps[next] {
		if !f.locked(field) {
			f.focus = field
			break
		}
	}
	return f.syncFocus()
}

func cycle(index, length, delta int) int {
	if length == 0 {
		return 0
	}
	return (index + delta + length) % length
}

func (f *taskForm) cycleSelection(delta int) {
	switch f.focus {
	case fieldStatus:
		f.statusIndex = cycle(f.statusIndex, len(formStatuses), delta)
	case fieldProfile:
		f.profileIndex = cycle(f.profileIndex, len(f.profiles)+1, delta)
		if f.profileIndex > 0 {
			profile := f.profiles[f.profileIndex-1]
			assignee := f.inputs[fieldAssignee]
			assignee.SetValue(profile.Name)
			f.inputs[fieldAssignee] = assignee
			f.runtimeIndex = optionIndex(formRuntimes, string(profile.Runtime))
		}
	case fieldRuntime:
		f.runtimeIndex = cycle(f.runtimeIndex, len(formRuntimes), delta)
		f.profileIndex = 0
	case fieldWorkspaceKind:
		f.workspaceIndex = cycle(f.workspaceIndex, len(formWorkspaceKinds), delta)
	case fieldGoalMode:
		f.goalMode = !f.goalMode
	}
}

func (f *taskForm) Update(message tea.KeyMsg) (tea.Cmd, formAction) {
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
	case "tab":
		return f.moveFocus(1), formContinue
	case "shift+tab":
		return f.moveFocus(-1), formContinue
	case "ctrl+right":
		return f.moveStep(1), formContinue
	case "ctrl+left":
		return f.moveStep(-1), formContinue
	}

	if f.locked(f.focus) {
		return nil, formContinue
	}
	if f.focus == fieldBody {
		var command tea.Cmd
		f.body, command = f.body.Update(message)
		return command, formContinue
	}
	if f.textField(f.focus) {
		if message.String() == "enter" {
			return f.moveFocus(1), formContinue
		}
		input := f.inputs[f.focus]
		before := input.Value()
		var command tea.Cmd
		input, command = input.Update(message)
		f.inputs[f.focus] = input
		if (f.focus == fieldAssignee || f.focus == fieldRuntime) && input.Value() != before {
			f.profileIndex = 0
		}
		return command, formContinue
	}
	if message.String() == "left" || message.String() == "up" {
		f.cycleSelection(-1)
	} else if message.String() == "right" || message.String() == "down" || message.String() == " " {
		f.cycleSelection(1)
	} else if message.String() == "enter" {
		return f.moveFocus(1), formContinue
	}
	return nil, formContinue
}

func (f *taskForm) UpdateMessage(message tea.Msg) tea.Cmd {
	if f.focus == fieldBody {
		var command tea.Cmd
		f.body, command = f.body.Update(message)
		return command
	}
	if f.textField(f.focus) && !f.locked(f.focus) {
		input := f.inputs[f.focus]
		var command tea.Cmd
		input, command = input.Update(message)
		f.inputs[f.focus] = input
		return command
	}
	return nil
}

func (f *taskForm) validate() error {
	if strings.TrimSpace(f.inputs[fieldTitle].Value()) == "" {
		return errors.New("title cannot be empty")
	}
	if _, err := strconv.Atoi(strings.TrimSpace(f.inputs[fieldPriority].Value())); err != nil {
		return errors.New("priority must be an integer")
	}
	if f.runtimeIndex < 0 || f.runtimeIndex >= len(formRuntimes) {
		return errors.New("select a valid runtime")
	}
	if f.mode == "create" && formStatuses[f.statusIndex] == string(model.TaskStatusReady) &&
		(strings.TrimSpace(f.inputs[fieldAssignee].Value()) == "" || formRuntimes[f.runtimeIndex] == string(model.RuntimeManual)) {
		return errors.New("Ready requires an assignee and an agent runtime")
	}
	return nil
}

func splitSkills(value string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func optionalValue(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (f *taskForm) createInput() store.CreateTaskInput {
	priority, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldPriority].Value()))
	return store.CreateTaskInput{
		Title: strings.TrimSpace(f.inputs[fieldTitle].Value()), Body: f.body.Value(), Board: f.board,
		Status: model.TaskStatus(formStatuses[f.statusIndex]), Assignee: optionalValue(f.inputs[fieldAssignee].Value()),
		Runtime: model.Runtime(formRuntimes[f.runtimeIndex]), Priority: priority, Tenant: optionalValue(f.inputs[fieldTenant].Value()),
		WorkspaceKind: model.WorkspaceKind(formWorkspaceKinds[f.workspaceIndex]), Workspace: optionalValue(f.inputs[fieldWorkspace].Value()),
		Skills: splitSkills(f.inputs[fieldSkills].Value()), GoalMode: f.goalMode,
	}
}

func (f *taskForm) updateInput() store.UpdateTaskInput {
	priority, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldPriority].Value()))
	title, body := strings.TrimSpace(f.inputs[fieldTitle].Value()), f.body.Value()
	runtime, kind, skills, goal := model.Runtime(formRuntimes[f.runtimeIndex]), model.WorkspaceKind(formWorkspaceKinds[f.workspaceIndex]), splitSkills(f.inputs[fieldSkills].Value()), f.goalMode
	return store.UpdateTaskInput{
		Title: &title, Body: &body, Priority: &priority, Runtime: &runtime, WorkspaceKind: &kind, Skills: &skills, GoalMode: &goal,
		Assignee:  store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldAssignee].Value())},
		Tenant:    store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldTenant].Value())},
		Workspace: store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldWorkspace].Value())},
	}
}
