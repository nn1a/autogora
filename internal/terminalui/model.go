package terminalui

import (
	"context"
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
			Board: m.board, IncludeArchived: showArchived, Search: search, Sort: "priority-desc", Limit: 500,
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
			detail, err = m.backend.CompleteTask(m.ctx, id, store.CompletionInput{})
		case "block":
			detail, err = m.backend.BlockTask(m.ctx, id, store.BlockInput{Reason: value})
		case "unblock":
			detail, err = m.backend.UnblockTask(m.ctx, id)
		case "archive":
			detail, err = m.backend.ArchiveTask(m.ctx, id)
		default:
			err = fmt.Errorf("unknown TUI action: %s", action)
		}
		return mutationMsg{action: action, id: id, detail: &detail, err: err}
	}
}

func actionLabel(action string) string {
	labels := map[string]string{
		"create": "Task created", "title": "Title updated", "assign": "Assignee updated",
		"comment": "Comment added", "promote": "Task promoted", "complete": "Task completed",
		"block": "Task blocked", "unblock": "Task unblocked", "archive": "Task archived",
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
		if value == "" && m.inputMode != "assign" {
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
		case "n":
			m.beginPrompt("create", "New task", "", "")
		case "e":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil {
				m.beginPrompt("title", "Title", task.Title, task.ID)
			}
		case "s":
			if task := m.selectedTask(); task != nil && task.CurrentRunID == nil {
				m.beginPrompt("assign", "Assignee (empty to clear)", pointer(task.Assignee, ""), task.ID)
			}
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
				m.beginConfirm("complete", *task)
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
		for _, task := range message.tasks {
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

func statusIndex(statuses []model.TaskStatus, status model.TaskStatus) int {
	for index, candidate := range statuses {
		if candidate == status {
			return index
		}
	}
	return 0
}
