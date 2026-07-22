package terminalui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
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
	tasks []model.Task
	err   error
	at    time.Time
}

type detailLoadedMsg struct {
	id     string
	detail model.TaskDetail
	err    error
}

type refreshMsg time.Time

type Model struct {
	ctx      context.Context
	backend  Backend
	board    string
	width    int
	height   int
	column   int
	cursors  map[model.TaskStatus]int
	tasks    map[model.TaskStatus][]model.Task
	detail   *model.TaskDetail
	loading  bool
	err      error
	updated  time.Time
	help     bool
	detailOn bool
}

func NewModel(ctx context.Context, backend Backend, board string) *Model {
	return &Model{
		ctx: ctx, backend: backend, board: board,
		width: 100, height: 30, cursors: map[model.TaskStatus]int{},
		tasks: map[model.TaskStatus][]model.Task{}, loading: true,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.loadTasks(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(at time.Time) tea.Msg { return refreshMsg(at) })
}

func (m *Model) loadTasks() tea.Cmd {
	return func() tea.Msg {
		tasks, err := m.backend.ListTasks(m.ctx, store.ListTaskFilter{
			Board: m.board, IncludeArchived: false, Sort: "priority-desc", Limit: 500,
		})
		return tasksLoadedMsg{tasks: tasks, err: err, at: time.Now()}
	}
}

func (m *Model) loadDetail(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		detail, err := m.backend.GetTask(m.ctx, id)
		return detailLoadedMsg{id: id, detail: detail, err: err}
	}
}

func (m *Model) selectedTask() *model.Task {
	status := boardStatuses[m.column]
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
	m.column = max(0, min(len(boardStatuses)-1, m.column+delta))
	m.detail = nil
	return m.loadDetail(m.selectedID())
}

func (m *Model) moveCard(delta int) tea.Cmd {
	status := boardStatuses[m.column]
	items := m.tasks[status]
	if len(items) == 0 {
		return nil
	}
	m.cursors[status] = max(0, min(len(items)-1, m.cursors[status]+delta))
	m.detail = nil
	return m.loadDetail(m.selectedID())
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
	case tea.KeyMsg:
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
			status := boardStatuses[m.column]
			m.cursors[status] = 0
			return m, m.loadDetail(m.selectedID())
		case "G", "end":
			status := boardStatuses[m.column]
			m.cursors[status] = max(0, len(m.tasks[status])-1)
			return m, m.loadDetail(m.selectedID())
		case "r":
			m.loading = true
			return m, m.loadTasks()
		case "?":
			m.help = !m.help
		case "enter":
			m.detailOn = !m.detailOn
		case "esc":
			m.help, m.detailOn = false, false
		}
	case tasksLoadedMsg:
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
		if selected != "" {
			for status, items := range grouped {
				for index := range items {
					if items[index].ID == selected {
						m.column, m.cursors[status] = statusIndex(status), index
					}
				}
			}
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
		m.detail, m.err = &message.detail, nil
	case refreshMsg:
		return m, tea.Batch(m.loadTasks(), tick())
	}
	return m, nil
}

func statusIndex(status model.TaskStatus) int {
	for index, candidate := range boardStatuses {
		if candidate == status {
			return index
		}
	}
	return 0
}
