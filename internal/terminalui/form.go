package terminalui

import (
	"errors"
	"strconv"
	"strings"
	"time"

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
	fieldScheduledAt
	fieldPriority
	fieldProfile
	fieldAssignee
	fieldRuntime
	fieldSkills
	fieldTenant
	fieldWorkspaceKind
	fieldWorkspace
	fieldBranch
	fieldMaxRuntime
	fieldMaxRetries
	fieldGoalMode
	fieldGoalMaxTurns
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
	mode              string
	taskID            string
	expectedUpdatedAt string
	activeRun         bool
	board             string
	profiles          []orchestration.ProfileRoute
	profileIndex      int
	statusIndex       int
	runtimeIndex      int
	workspaceIndex    int
	goalMode          bool
	focus             formField
	inputs            map[formField]textinput.Model
	body              textarea.Model
	err               error
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
	runnableProfiles := make([]orchestration.ProfileRoute, 0, len(profiles))
	for _, profile := range profiles {
		if orchestration.RunnableProfileRoute(profile) {
			runnableProfiles = append(runnableProfiles, profile)
		}
	}
	form := &taskForm{
		mode: "create", board: board, profiles: runnableProfiles,
		inputs: map[formField]textinput.Model{
			fieldTitle: newTextInput("", "Task title", 300), fieldPriority: newTextInput("0", "0", 12),
			fieldScheduledAt: newTextInput("", "RFC3339, for example 2026-07-23T18:00:00+09:00", 80),
			fieldAssignee:    newTextInput("", "profile or worker name", 200), fieldSkills: newTextInput("", "comma-separated", 1000),
			fieldTenant: newTextInput("", "optional", 200), fieldWorkspace: newTextInput("", "absolute path or empty", 2000),
			fieldBranch: newTextInput("", "optional base branch", 500), fieldMaxRuntime: newTextInput("", "seconds or empty", 12),
			fieldMaxRetries: newTextInput("2", "2", 6), fieldGoalMaxTurns: newTextInput("20", "20", 6),
		},
	}
	form.body = textarea.New()
	form.body.Placeholder = "Describe scope, acceptance criteria, and verification"
	form.body.CharLimit = 50_000
	form.body.SetHeight(6)
	form.statusIndex = optionIndex(formStatuses, string(status))
	if status == model.TaskStatusScheduled {
		form.setInputValue(fieldScheduledAt, time.Now().Add(time.Hour).Format(time.RFC3339))
	}
	if len(runnableProfiles) > 0 && (status == model.TaskStatusTodo || status == model.TaskStatusReady || status == model.TaskStatusScheduled) {
		form.profileIndex = 1
		form.setInputValue(fieldAssignee, runnableProfiles[0].Name)
		form.runtimeIndex = optionIndex(formRuntimes, string(runnableProfiles[0].Runtime))
	}
	form.focus = fieldTitle
	form.syncFocus()
	return form
}

func editTaskForm(board string, profiles []orchestration.ProfileRoute, task model.Task) *taskForm {
	form := newTaskForm(board, profiles, task.Status)
	form.mode, form.taskID, form.expectedUpdatedAt, form.activeRun = "edit", task.ID, task.UpdatedAt, task.CurrentRunID != nil
	form.setInputValue(fieldTitle, task.Title)
	form.setInputValue(fieldPriority, strconv.Itoa(task.Priority))
	form.setInputValue(fieldAssignee, pointer(task.Assignee, ""))
	form.setInputValue(fieldSkills, strings.Join(task.Skills, ", "))
	form.setInputValue(fieldTenant, pointer(task.Tenant, ""))
	form.setInputValue(fieldWorkspace, pointer(task.Workspace, ""))
	form.setInputValue(fieldBranch, pointer(task.Branch, ""))
	form.setInputValue(fieldScheduledAt, pointer(task.ScheduledAt, ""))
	if task.MaxRuntimeSeconds != nil {
		form.setInputValue(fieldMaxRuntime, strconv.Itoa(*task.MaxRuntimeSeconds))
	}
	form.setInputValue(fieldMaxRetries, strconv.Itoa(task.MaxRetries))
	form.setInputValue(fieldGoalMaxTurns, strconv.Itoa(task.GoalMaxTurns))
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
	if form.activeRun {
		form.focus = fieldPriority
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
		return []formField{fieldTitle, fieldBody, fieldStatus, fieldScheduledAt, fieldPriority, fieldProfile, fieldAssignee, fieldRuntime, fieldSkills, fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldBranch, fieldMaxRuntime, fieldMaxRetries, fieldGoalMode, fieldGoalMaxTurns}
	}
	return []formField{fieldTitle, fieldBody, fieldScheduledAt, fieldPriority, fieldProfile, fieldAssignee, fieldRuntime, fieldSkills, fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldBranch, fieldMaxRuntime, fieldMaxRetries, fieldGoalMode, fieldGoalMaxTurns}
}

func (f *taskForm) stepFields() [][]formField {
	task := []formField{fieldTitle, fieldBody, fieldPriority}
	if f.mode == "create" {
		task = []formField{fieldTitle, fieldBody, fieldStatus, fieldScheduledAt, fieldPriority}
	}
	return [][]formField{
		task,
		{fieldProfile, fieldAssignee, fieldRuntime, fieldSkills},
		{fieldTenant, fieldWorkspaceKind, fieldWorkspace, fieldBranch, fieldMaxRuntime, fieldMaxRetries, fieldGoalMode, fieldGoalMaxTurns},
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
	case fieldPriority:
		return false
	default:
		return true
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
	if f.focus == fieldBody && !f.locked(fieldBody) {
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
	next := f.step()
	for range len(steps) {
		next = (next + delta + len(steps)) % len(steps)
		for _, field := range steps[next] {
			if !f.locked(field) {
				f.focus = field
				return f.syncFocus()
			}
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
		status := model.TaskStatus(formStatuses[f.statusIndex])
		if status == model.TaskStatusScheduled && strings.TrimSpace(f.inputs[fieldScheduledAt].Value()) == "" {
			f.setInputValue(fieldScheduledAt, time.Now().Add(time.Hour).Format(time.RFC3339))
		}
		if f.profileIndex == 0 && len(f.profiles) > 0 && (status == model.TaskStatusTodo || status == model.TaskStatusReady || status == model.TaskStatusScheduled) {
			f.profileIndex = 1
			f.setInputValue(fieldAssignee, f.profiles[0].Name)
			f.runtimeIndex = optionIndex(formRuntimes, string(f.profiles[0].Runtime))
		}
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
	if f.focus == fieldBody && !f.locked(fieldBody) {
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
	if _, err := strconv.Atoi(strings.TrimSpace(f.inputs[fieldPriority].Value())); err != nil {
		return errors.New("priority must be an integer")
	}
	if f.activeRun {
		return nil
	}
	if strings.TrimSpace(f.inputs[fieldTitle].Value()) == "" {
		return errors.New("title cannot be empty")
	}
	if value := strings.TrimSpace(f.inputs[fieldMaxRuntime].Value()); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 {
			return errors.New("max runtime must be a positive number of seconds")
		}
	}
	for field, label := range map[formField]string{fieldMaxRetries: "max retries", fieldGoalMaxTurns: "goal max turns"} {
		value, err := strconv.Atoi(strings.TrimSpace(f.inputs[field].Value()))
		if err != nil || value < 1 {
			return errors.New(label + " must be a positive integer")
		}
	}
	if f.runtimeIndex < 0 || f.runtimeIndex >= len(formRuntimes) {
		return errors.New("select a valid runtime")
	}
	if f.mode == "create" && formStatuses[f.statusIndex] == string(model.TaskStatusReady) &&
		(strings.TrimSpace(f.inputs[fieldAssignee].Value()) == "" || formRuntimes[f.runtimeIndex] == string(model.RuntimeManual)) {
		return errors.New("Ready requires an assignee and an agent runtime")
	}
	if formStatuses[f.statusIndex] == string(model.TaskStatusScheduled) {
		value := strings.TrimSpace(f.inputs[fieldScheduledAt].Value())
		at, err := time.Parse(time.RFC3339, value)
		if err != nil || !at.After(time.Now()) {
			return errors.New("Scheduled requires a future RFC3339 time")
		}
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
	maxRetries, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldMaxRetries].Value()))
	goalMaxTurns, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldGoalMaxTurns].Value()))
	var maxRuntime *int
	if value := strings.TrimSpace(f.inputs[fieldMaxRuntime].Value()); value != "" {
		seconds, _ := strconv.Atoi(value)
		maxRuntime = &seconds
	}
	return store.CreateTaskInput{
		Title: strings.TrimSpace(f.inputs[fieldTitle].Value()), Body: f.body.Value(), Board: f.board,
		Status: model.TaskStatus(formStatuses[f.statusIndex]), Assignee: optionalValue(f.inputs[fieldAssignee].Value()),
		Runtime: model.Runtime(formRuntimes[f.runtimeIndex]), Priority: priority, Tenant: optionalValue(f.inputs[fieldTenant].Value()),
		WorkspaceKind: model.WorkspaceKind(formWorkspaceKinds[f.workspaceIndex]), Workspace: optionalValue(f.inputs[fieldWorkspace].Value()),
		Branch: optionalValue(f.inputs[fieldBranch].Value()), ScheduledAt: optionalValue(f.inputs[fieldScheduledAt].Value()), MaxRuntimeSeconds: maxRuntime,
		MaxRetries: maxRetries, Skills: splitSkills(f.inputs[fieldSkills].Value()), GoalMode: f.goalMode, GoalMaxTurns: goalMaxTurns,
	}
}

func (f *taskForm) updateInput() store.UpdateTaskInput {
	priority, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldPriority].Value()))
	expectedUpdatedAt := f.expectedUpdatedAt
	if f.activeRun {
		return store.UpdateTaskInput{
			ExpectedUpdatedAt: &expectedUpdatedAt,
			Priority:          &priority,
		}
	}
	maxRetries, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldMaxRetries].Value()))
	goalMaxTurns, _ := strconv.Atoi(strings.TrimSpace(f.inputs[fieldGoalMaxTurns].Value()))
	var maxRuntime *int
	if value := strings.TrimSpace(f.inputs[fieldMaxRuntime].Value()); value != "" {
		seconds, _ := strconv.Atoi(value)
		maxRuntime = &seconds
	}
	title, body := strings.TrimSpace(f.inputs[fieldTitle].Value()), f.body.Value()
	runtime, kind, skills, goal := model.Runtime(formRuntimes[f.runtimeIndex]), model.WorkspaceKind(formWorkspaceKinds[f.workspaceIndex]), splitSkills(f.inputs[fieldSkills].Value()), f.goalMode
	return store.UpdateTaskInput{
		ExpectedUpdatedAt: &expectedUpdatedAt,
		Title:             &title, Body: &body, Priority: &priority, Runtime: &runtime, WorkspaceKind: &kind, Skills: &skills, GoalMode: &goal,
		MaxRuntimeSeconds: store.OptionalInt{Set: true, Value: maxRuntime}, MaxRetries: &maxRetries, GoalMaxTurns: &goalMaxTurns,
		Assignee:    store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldAssignee].Value())},
		Tenant:      store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldTenant].Value())},
		Workspace:   store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldWorkspace].Value())},
		Branch:      store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldBranch].Value())},
		ScheduledAt: store.OptionalString{Set: true, Value: optionalValue(f.inputs[fieldScheduledAt].Value())},
	}
}
