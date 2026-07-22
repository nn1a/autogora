package terminalui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

const refreshInterval = 2 * time.Second

var boardStatuses = []model.TaskStatus{
	model.TaskStatusTriage,
	model.TaskStatusTodo,
	model.TaskStatusScheduled,
	model.TaskStatusReady,
	model.TaskStatusRunning,
	model.TaskStatusBlocked,
	model.TaskStatusReview,
	model.TaskStatusDone,
}

type tasksLoadedMsg struct {
	tasks        []model.Task
	err          error
	at           time.Time
	search       string
	showArchived bool
}

type detailLoadedMsg struct {
	id     string
	detail model.TaskDetail
	graph  model.RelationshipGraph
	err    error
}

type refreshMsg time.Time

type boardContextMsg struct {
	context taskservice.BoardContext
	err     error
}

type mutationMsg struct {
	action string
	id     string
	detail *model.TaskDetail
	err    error
}

type confirmState struct {
	action string
	id     string
	title  string
}

type Model struct {
	ctx              context.Context
	backend          Backend
	board            string
	boardContext     *taskservice.BoardContext
	width            int
	height           int
	column           int
	cursors          map[model.TaskStatus]int
	tasks            map[model.TaskStatus][]model.Task
	allTasks         []model.Task
	detail           *model.TaskDetail
	graph            *model.RelationshipGraph
	loading          bool
	err              error
	updated          time.Time
	help             bool
	detailOn         bool
	detailTab        int
	detailScroll     int
	inputMode        string
	search           string
	searchDraft      string
	showArchived     bool
	busy             bool
	notice           string
	promptLabel      string
	promptDraft      string
	promptTaskID     string
	confirm          *confirmState
	desiredSelection string
	form             *taskForm
	menu             *actionMenu
}

func NewModel(ctx context.Context, backend Backend, board string) *Model {
	return &Model{
		ctx: ctx, backend: backend, board: board,
		width: 100, height: 30, cursors: map[model.TaskStatus]int{},
		tasks: map[model.TaskStatus][]model.Task{}, loading: true,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.loadTasks(), m.loadBoardContext(), tick())
}

func (m *Model) loadBoardContext() tea.Cmd {
	return func() tea.Msg {
		value, err := m.backend.BoardContext(m.ctx)
		return boardContextMsg{context: value, err: err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(at time.Time) tea.Msg { return refreshMsg(at) })
}

func (m *Model) loadTasks() tea.Cmd {
	search, showArchived := m.search, m.showArchived
	return func() tea.Msg {
		tasks, err := m.backend.ListTasks(m.ctx, store.ListTaskFilter{
			Board: m.board, IncludeArchived: showArchived, Sort: "priority-desc", Limit: 500,
		})
		return tasksLoadedMsg{tasks: tasks, err: err, at: time.Now(), search: search, showArchived: showArchived}
	}
}

func (m *Model) loadDetail(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		detail, err := m.backend.GetTask(m.ctx, id)
		if err != nil {
			return detailLoadedMsg{id: id, err: err}
		}
		graph, err := m.backend.RelationshipGraph(m.ctx, id)
		return detailLoadedMsg{id: id, detail: detail, graph: graph, err: err}
	}
}

func (m *Model) statuses() []model.TaskStatus {
	if !m.showArchived {
		return boardStatuses
	}
	return append(append([]model.TaskStatus{}, boardStatuses...), model.TaskStatusArchived)
}

func (m *Model) selectedTask() *model.Task {
	statuses := m.statuses()
	m.column = max(0, min(len(statuses)-1, m.column))
	status := statuses[m.column]
	items := m.tasks[status]
	if len(items) == 0 {
		return nil
	}
	index := m.cursors[status]
	if index >= len(items) {
		index = len(items) - 1
		m.cursors[status] = index
	}
	return &items[index]
}

func (m *Model) selectedID() string {
	if task := m.selectedTask(); task != nil {
		return task.ID
	}
	return ""
}

func (m *Model) moveColumn(delta int) tea.Cmd {
	m.column = max(0, min(len(m.statuses())-1, m.column+delta))
	m.detail, m.graph, m.detailScroll = nil, nil, 0
	return m.loadDetail(m.selectedID())
}

func (m *Model) moveCard(delta int) tea.Cmd {
	status := m.statuses()[m.column]
	items := m.tasks[status]
	if len(items) == 0 {
		return nil
	}
	m.cursors[status] = max(0, min(len(items)-1, m.cursors[status]+delta))
	m.detail, m.graph, m.detailScroll = nil, nil, 0
	return m.loadDetail(m.selectedID())
}

func (m *Model) beginPrompt(mode, label, value, taskID string) {
	m.inputMode, m.promptLabel, m.promptDraft, m.promptTaskID = mode, label, value, taskID
	m.err, m.notice = nil, ""
}

func (m *Model) beginConfirm(action string, task model.Task) {
	m.confirm = &confirmState{action: action, id: task.ID, title: task.Title}
	m.err, m.notice = nil, ""
}

func (m *Model) openCreateForm() tea.Cmd {
	if m.boardContext == nil {
		m.err = errors.New("board settings are still loading")
		return nil
	}
	status := m.statuses()[m.column]
	m.form = newTaskForm(m.board, m.boardContext.Profiles, status)
	m.err, m.notice = nil, ""
	return m.form.syncFocus()
}

func (m *Model) openEditForm(focus formField) tea.Cmd {
	if m.boardContext == nil {
		m.err = errors.New("board settings are still loading")
		return nil
	}
	task := m.selectedTask()
	if task == nil {
		return nil
	}
	return m.openEditTaskForm(*task, focus)
}

func (m *Model) openEditTaskForm(task model.Task, focus formField) tea.Cmd {
	if m.boardContext == nil {
		m.err = errors.New("board settings are still loading")
		return nil
	}
	m.form = editTaskForm(m.board, m.boardContext.Profiles, task)
	if !m.form.locked(focus) {
		m.form.focus = focus
	}
	m.err, m.notice = nil, ""
	return m.form.syncFocus()
}

func (m *Model) submitForm(form *taskForm) tea.Cmd {
	m.busy = true
	return func() tea.Msg {
		var detail model.TaskDetail
		var err error
		action := form.mode
		if form.mode == "create" {
			detail, err = m.backend.CreateTask(m.ctx, form.createInput())
		} else {
			detail, err = m.backend.UpdateTask(m.ctx, form.taskID, form.updateInput())
		}
		return mutationMsg{action: action, id: form.taskID, detail: &detail, err: err}
	}
}

func (m *Model) mutate(action, id, value string) tea.Cmd {
	m.busy = true
	return func() tea.Msg {
		var detail model.TaskDetail
		var err error
		switch action {
		case "create":
			detail, err = m.backend.CreateTask(m.ctx, store.CreateTaskInput{
				Title: value, Board: m.board, Runtime: model.RuntimeManual, Status: model.TaskStatusTriage,
			})
		case "title":
			detail, err = m.backend.UpdateTask(m.ctx, id, store.UpdateTaskInput{Title: &value})
		case "assign":
			assignee := store.OptionalString{Set: true}
			if strings.TrimSpace(value) != "" && value != "none" {
				trimmed := strings.TrimSpace(value)
				assignee.Value = &trimmed
			}
			detail, err = m.backend.UpdateTask(m.ctx, id, store.UpdateTaskInput{Assignee: assignee})
		case "comment":
			_, err = m.backend.AddComment(m.ctx, id, "human", value)
			if err == nil {
				detail, err = m.backend.GetTask(m.ctx, id)
			}
		case "promote":
			detail, err = m.backend.PromoteTask(m.ctx, id)
		case "complete":
			detail, err = m.backend.CompleteTask(m.ctx, id, store.CompletionInput{Summary: value})
		case "block":
			detail, err = m.backend.BlockTask(m.ctx, id, store.BlockInput{Reason: value, Kind: model.BlockKindNeedsInput})
		case "unblock":
			detail, err = m.backend.UnblockTask(m.ctx, id)
		case "archive":
			detail, err = m.backend.ArchiveTask(m.ctx, id)
		case "specify":
			detail, err = m.backend.SpecifyTask(m.ctx, id, nil, "tui")
		case "decompose":
			result, decomposeErr := m.backend.DecomposeTask(m.ctx, id, nil)
			detail, err = result.Task, decomposeErr
		case "start":
			claim, claimErr := m.backend.ClaimTaskForUser(m.ctx, id, 900, "")
			err = claimErr
			if claim != nil {
				detail = claim.Task
			}
		case "terminate":
			termination, terminationErr := m.backend.TerminateRun(m.ctx, id, "Terminated from TUI")
			detail, err = termination.Task, terminationErr
		case "delete":
			err = m.backend.DeleteTask(m.ctx, id)
		case "schedule":
			detail, err = m.backend.ScheduleTask(m.ctx, id, optionalValue(value), "Scheduled from TUI")
		case "attach-file":
			_, err = m.backend.AttachFile(m.ctx, id, value, "")
			if err == nil {
				detail, err = m.backend.GetTask(m.ctx, id)
			}
		case "attach-url":
			_, err = m.backend.AttachURL(m.ctx, id, value, "")
			if err == nil {
				detail, err = m.backend.GetTask(m.ctx, id)
			}
		default:
			relationID := func(prefix string) string { return strings.TrimPrefix(action, prefix) }
			switch {
			case strings.HasPrefix(action, "link-prerequisite:"):
				detail, err = m.backend.LinkTasks(m.ctx, relationID("link-prerequisite:"), id)
			case strings.HasPrefix(action, "link-dependent:"):
				_, err = m.backend.LinkTasks(m.ctx, id, relationID("link-dependent:"))
				if err == nil {
					detail, err = m.backend.GetTask(m.ctx, id)
				}
			case strings.HasPrefix(action, "set-parent:"):
				detail, err = m.backend.SetSubtaskParent(m.ctx, relationID("set-parent:"), id, nil)
			case strings.HasPrefix(action, "add-subtask:"):
				_, err = m.backend.SetSubtaskParent(m.ctx, id, relationID("add-subtask:"), nil)
				if err == nil {
					detail, err = m.backend.GetTask(m.ctx, id)
				}
			case strings.HasPrefix(action, "unlink-prerequisite:"):
				detail, err = m.backend.UnlinkTasks(m.ctx, relationID("unlink-prerequisite:"), id)
			case strings.HasPrefix(action, "unlink-dependent:"):
				_, err = m.backend.UnlinkTasks(m.ctx, id, relationID("unlink-dependent:"))
				if err == nil {
					detail, err = m.backend.GetTask(m.ctx, id)
				}
			case strings.HasPrefix(action, "remove-parent:"):
				detail, err = m.backend.RemoveSubtask(m.ctx, relationID("remove-parent:"), id)
			case strings.HasPrefix(action, "remove-subtask:"):
				_, err = m.backend.RemoveSubtask(m.ctx, id, relationID("remove-subtask:"))
				if err == nil {
					detail, err = m.backend.GetTask(m.ctx, id)
				}
			case strings.HasPrefix(action, "attachment-remove:"):
				err = m.backend.RemoveAttachment(m.ctx, id, relationID("attachment-remove:"))
				if err == nil {
					detail, err = m.backend.GetTask(m.ctx, id)
				}
			default:
				if strings.HasPrefix(action, "move:") {
					status := model.TaskStatus(strings.TrimPrefix(action, "move:"))
					if !model.ValidTaskStatus(status) || status == model.TaskStatusRunning {
						err = fmt.Errorf("invalid move target: %s", status)
					} else {
						detail, err = m.backend.UpdateTask(m.ctx, id, store.UpdateTaskInput{Status: &status})
					}
					break
				}
				err = fmt.Errorf("unknown TUI action: %s", action)
			}
		}
		return mutationMsg{action: action, id: id, detail: &detail, err: err}
	}
}

func actionLabel(action string) string {
	labels := map[string]string{
		"create": "Task created", "edit": "Task updated", "title": "Title updated", "assign": "Assignee updated",
		"comment": "Comment added", "promote": "Task promoted", "complete": "Task completed",
		"block": "Task blocked", "unblock": "Task unblocked", "archive": "Task archived", "specify": "Task specified",
		"decompose": "Task decomposed", "start": "Task claimed", "terminate": "Run termination requested", "delete": "Task deleted",
		"schedule": "Task scheduled", "attach-file": "File attached", "attach-url": "URL attached",
	}
	if strings.HasPrefix(action, "move:") {
		return "Task moved to " + strings.TrimPrefix(action, "move:")
	}
	for prefix, label := range map[string]string{
		"link-prerequisite:": "Prerequisite linked", "link-dependent:": "Dependent linked",
		"set-parent:": "Parent task set", "add-subtask:": "Subtask added",
		"unlink-prerequisite:": "Prerequisite unlinked", "unlink-dependent:": "Dependent unlinked",
		"remove-parent:": "Parent task removed", "remove-subtask:": "Subtask removed",
		"attachment-remove:": "Attachment removed",
	} {
		if strings.HasPrefix(action, prefix) {
			return label
		}
	}
	return labels[action]
}

func (m *Model) handlePrompt(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.inputMode, m.promptDraft, m.promptTaskID = "", "", ""
	case "enter":
		value := strings.TrimSpace(m.promptDraft)
		if value == "" && m.inputMode != "assign" && m.inputMode != "schedule" {
			m.err = fmt.Errorf("%s cannot be empty", strings.ToLower(m.promptLabel))
			return m, nil
		}
		action, id := m.inputMode, m.promptTaskID
		m.inputMode, m.promptDraft, m.promptTaskID = "", "", ""
		return m, m.mutate(action, id, value)
	case "backspace":
		runes := []rune(m.promptDraft)
		if len(runes) > 0 {
			m.promptDraft = string(runes[:len(runes)-1])
		}
	case "ctrl+u":
		m.promptDraft = ""
	default:
		if message.Type == tea.KeyRunes {
			m.promptDraft += string(message.Runes)
		}
	}
	return m, nil
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.form != nil {
		if key, ok := message.(tea.KeyMsg); ok {
			if key.String() == "ctrl+c" {
				return m, tea.Quit
			}
			command, action := m.form.Update(key)
			switch action {
			case formCancel:
				m.form = nil
				return m, nil
			case formSubmit:
				form := m.form
				m.form = nil
				return m, m.submitForm(form)
			default:
				return m, command
			}
		}
		switch message.(type) {
		case tea.WindowSizeMsg, tasksLoadedMsg, detailLoadedMsg, boardContextMsg, refreshMsg, mutationMsg:
		default:
			return m, m.form.UpdateMessage(message)
		}
	}
	if m.menu != nil {
		if key, ok := message.(tea.KeyMsg); ok {
			return m.updateMenu(key)
		}
	}
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
	case tea.KeyMsg:
		if m.confirm != nil {
			switch message.String() {
			case "y", "enter":
				confirmation := *m.confirm
				m.confirm = nil
				return m, m.mutate(confirmation.action, confirmation.id, "")
			case "n", "esc":
				m.confirm = nil
			case "ctrl+c":
				return m, tea.Quit
			}
			return m, nil
		}
		if m.inputMode != "" && m.inputMode != "search" {
			return m.handlePrompt(message)
		}
		if m.busy {
			if message.String() == "ctrl+c" || message.String() == "q" {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.inputMode == "search" {
			switch message.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.inputMode, m.searchDraft = "", m.search
			case "enter":
				m.inputMode, m.search = "", m.searchDraft
				m.loading = true
				return m, m.loadTasks()
			case "backspace":
				runes := []rune(m.searchDraft)
				if len(runes) > 0 {
					m.searchDraft = string(runes[:len(runes)-1])
				}
			case "ctrl+u":
				m.searchDraft = ""
			default:
				if message.Type == tea.KeyRunes {
					m.searchDraft += string(message.Runes)
				}
			}
			return m, nil
		}
		switch message.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "left", "h":
			return m, m.moveColumn(-1)
		case "right", "l":
			return m, m.moveColumn(1)
		case "up", "k":
			return m, m.moveCard(-1)
		case "down", "j":
			return m, m.moveCard(1)
		case "g", "home":
			status := m.statuses()[m.column]
			m.cursors[status] = 0
			return m, m.loadDetail(m.selectedID())
		case "G", "end":
			status := m.statuses()[m.column]
			m.cursors[status] = max(0, len(m.tasks[status])-1)
			return m, m.loadDetail(m.selectedID())
		case "r":
			m.loading = true
			return m, m.loadTasks()
		case "/":
			m.inputMode, m.searchDraft = "search", m.search
		case "a":
			m.showArchived = !m.showArchived
			if !m.showArchived && m.column >= len(boardStatuses) {
				m.column = len(boardStatuses) - 1
			}
			m.loading = true
			return m, m.loadTasks()
		case "tab":
			m.detailTab, m.detailScroll = (m.detailTab+1)%3, 0
		case "1", "2", "3":
			m.detailTab, m.detailScroll = int(message.Runes[0]-'1'), 0
		case "pgdown", "ctrl+d":
			m.detailScroll += max(1, m.height/3)
		case "pgup", "ctrl+u":
			m.detailScroll = max(0, m.detailScroll-max(1, m.height/3))
		case "?":
			m.help = !m.help
		case " ":
			if task := m.selectedTask(); task != nil {
				var detail *model.TaskDetail
				if m.detail != nil && m.detail.Task.ID == task.ID {
					detail = m.detail
				}
				m.menu = taskActionMenu(*task, detail)
			}
		case "m":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil {
				m.menu = statusMenu(*task)
			}
		case "n":
			return m, m.openCreateForm()
		case "e":
			return m, m.openEditForm(fieldTitle)
		case "s":
			return m, m.openEditForm(fieldProfile)
		case "C":
			if task := m.selectedTask(); task != nil {
				m.beginPrompt("comment", "Comment", "", task.ID)
			}
		case "b":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil && task.Status != model.TaskStatusDone && task.Status != model.TaskStatusArchived && task.Status != model.TaskStatusBlocked {
				m.beginPrompt("block", "Block reason", "", task.ID)
			}
		case "p":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil && (task.Status == model.TaskStatusTriage || task.Status == model.TaskStatusTodo || task.Status == model.TaskStatusScheduled || task.Status == model.TaskStatusReview) {
				m.beginConfirm("promote", *task)
			}
		case "u":
			if task := m.selectedTask(); task != nil && task.Status == model.TaskStatusBlocked {
				m.beginConfirm("unblock", *task)
			}
		case "c":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil && task.Status != model.TaskStatusDone && task.Status != model.TaskStatusArchived {
				m.beginPrompt("complete", "Completion summary", "", task.ID)
			}
		case "x":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil && task.Status != model.TaskStatusArchived {
				m.beginConfirm("archive", *task)
			}
		case "enter":
			m.detailOn = !m.detailOn
		case "esc":
			m.help, m.detailOn = false, false
		}
	case tea.MouseMsg:
		event := tea.MouseEvent(message)
		switch event.Button {
		case tea.MouseButtonWheelUp:
			return m, m.moveCard(-1)
		case tea.MouseButtonWheelDown:
			return m, m.moveCard(1)
		case tea.MouseButtonWheelLeft:
			return m, m.moveColumn(-1)
		case tea.MouseButtonWheelRight:
			return m, m.moveColumn(1)
		}
	case tasksLoadedMsg:
		if message.search != m.search || message.showArchived != m.showArchived {
			break
		}
		m.loading = false
		if message.err != nil {
			m.err = message.err
			break
		}
		selected := m.selectedID()
		m.err, m.updated = nil, message.at
		grouped := map[model.TaskStatus][]model.Task{}
		m.allTasks = append([]model.Task{}, message.tasks...)
		for _, task := range message.tasks {
			if !taskMatchesSearch(task, m.search) {
				continue
			}
			grouped[task.Status] = append(grouped[task.Status], task)
		}
		m.tasks = grouped
		if m.desiredSelection != "" {
			selected = m.desiredSelection
			m.desiredSelection = ""
		}
		if selected != "" {
			for status, items := range grouped {
				for index := range items {
					if items[index].ID == selected {
						m.column, m.cursors[status] = statusIndex(m.statuses(), status), index
					}
				}
			}
		}
		if m.detail != nil && m.detail.Task.ID != m.selectedID() {
			m.detail, m.graph, m.detailScroll = nil, nil, 0
		}
		return m, m.loadDetail(m.selectedID())
	case detailLoadedMsg:
		if message.id != m.selectedID() {
			break
		}
		if message.err != nil {
			m.err = message.err
			break
		}
		m.detail, m.graph, m.err = &message.detail, &message.graph, nil
	case boardContextMsg:
		if message.err != nil {
			m.err = message.err
			break
		}
		m.boardContext = &message.context
	case mutationMsg:
		m.busy = false
		if message.err != nil {
			m.err, m.notice = message.err, ""
			break
		}
		m.err, m.notice = nil, actionLabel(message.action)
		if message.detail != nil && message.detail.Task.ID != "" {
			m.desiredSelection = message.detail.Task.ID
		}
		m.loading = true
		return m, m.loadTasks()
	case refreshMsg:
		return m, tea.Batch(m.loadTasks(), m.loadBoardContext(), tick())
	}
	return m, nil
}

func taskMatchesSearch(task model.Task, search string) bool {
	search = strings.ToLower(strings.TrimSpace(search))
	if search == "" {
		return true
	}
	encoded, _ := json.Marshal(task)
	return strings.Contains(strings.ToLower(string(encoded)), search)
}

func statusIndex(statuses []model.TaskStatus, status model.TaskStatus) int {
	for index, candidate := range statuses {
		if candidate == status {
			return index
		}
	}
	return 0
}
