package terminalui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nn1a/autogora/internal/model"
)

type menuItem struct {
	label  string
	action string
	danger bool
}

type actionMenu struct {
	title     string
	task      model.Task
	items     []menuItem
	allItems  []menuItem
	index     int
	query     string
	searching bool
}

func newActionMenu(title string, task model.Task, items []menuItem) *actionMenu {
	return &actionMenu{title: title, task: task, items: append([]menuItem{}, items...), allItems: append([]menuItem{}, items...)}
}

func taskActionMenu(task model.Task, detail *model.TaskDetail) *actionMenu {
	items := []menuItem{{label: "Edit task", action: "edit"}, {label: "Add comment", action: "comment"}, {label: "Relationships…", action: "relationships"}}
	if task.CurrentRunID == nil {
		items = append(items, menuItem{label: "Move to status…", action: "move"})
	}
	if task.Status == model.TaskStatusTriage {
		items = append(items,
			menuItem{label: "Specify with board planner", action: "specify"},
			menuItem{label: "Decompose with board planner", action: "decompose"},
		)
	}
	if task.Status == model.TaskStatusBlocked {
		items = append(items, menuItem{label: "Unblock", action: "unblock"})
	}
	if task.Status == model.TaskStatusReady {
		items = append(items, menuItem{label: "Start manually (claim)", action: "start"})
	}
	if task.CurrentRunID != nil {
		items = append(items, menuItem{label: "Terminate active run", action: "terminate:" + *task.CurrentRunID, danger: true})
	} else if task.Status != model.TaskStatusDone && task.Status != model.TaskStatusArchived {
		if task.Status == model.TaskStatusTodo || task.Status == model.TaskStatusScheduled || task.Status == model.TaskStatusBlocked || task.Status == model.TaskStatusTriage || task.Status == model.TaskStatusReview {
			items = append(items, menuItem{label: "Promote", action: "promote"})
		}
		if task.Status != model.TaskStatusBlocked {
			items = append(items, menuItem{label: "Block…", action: "block"})
		}
		items = append(items, menuItem{label: "Complete…", action: "complete"}, menuItem{label: "Schedule…", action: "schedule"})
	}
	items = append(items, menuItem{label: "Attach file…", action: "attach-file"}, menuItem{label: "Attach URL…", action: "attach-url"})
	if detail != nil && detail.Task.ID == task.ID {
		for _, attachment := range detail.Attachments {
			items = append(items, menuItem{label: "Remove attachment · " + attachment.Name, action: "attachment-remove:" + attachment.ID, danger: true})
		}
	}
	if task.CurrentRunID == nil && task.Status != model.TaskStatusArchived {
		items = append(items, menuItem{label: "Archive", action: "archive", danger: true})
	}
	if task.CurrentRunID == nil {
		items = append(items, menuItem{label: "Delete permanently", action: "delete", danger: true})
	}
	return newActionMenu("Task actions", task, items)
}

func (m *Model) relationshipMenu(task model.Task) *actionMenu {
	items := []menuItem{
		{label: "Add prerequisite…", action: "relation-pick:link-prerequisite"},
		{label: "Add dependent…", action: "relation-pick:link-dependent"},
		{label: "Set parent task…", action: "relation-pick:set-parent"},
		{label: "Add subtask…", action: "relation-pick:add-subtask"},
	}
	if m.detail != nil && m.detail.Task.ID == task.ID {
		if m.detail.ParentTask != nil {
			items = append(items, menuItem{label: "Remove parent · " + m.detail.ParentTask.Title, action: "remove-parent:" + m.detail.ParentTask.ID, danger: true})
		}
		for _, related := range m.detail.Subtasks {
			items = append(items, menuItem{label: "Remove subtask · " + related.Title, action: "remove-subtask:" + related.ID, danger: true})
		}
		for _, related := range m.detail.Prerequisites {
			items = append(items, menuItem{label: "Unlink prerequisite · " + related.Title, action: "unlink-prerequisite:" + related.ID, danger: true})
		}
		for _, related := range m.detail.Dependents {
			items = append(items, menuItem{label: "Unlink dependent · " + related.Title, action: "unlink-dependent:" + related.ID, danger: true})
		}
	}
	return newActionMenu("Relationships", task, items)
}

func (m *Model) relationshipPicker(task model.Task, kind string) *actionMenu {
	items := []menuItem{}
	for _, candidate := range m.allTasks {
		if candidate.ID == task.ID || candidate.Status == model.TaskStatusArchived {
			continue
		}
		items = append(items, menuItem{label: fmt.Sprintf("%-9s %s", candidate.Status, candidate.Title), action: kind + ":" + candidate.ID})
	}
	return newActionMenu("Select related task", task, items)
}

func statusMenu(task model.Task) *actionMenu {
	items := []menuItem{}
	for _, status := range model.TaskStatuses {
		if status == task.Status || (status == model.TaskStatusRunning && task.Status != model.TaskStatusReady) {
			continue
		}
		items = append(items, menuItem{label: statusLabel(status), action: "move:" + string(status), danger: status == model.TaskStatusArchived})
	}
	return newActionMenu("Move task", task, items)
}

func (m *actionMenu) applyQuery() {
	query := strings.ToLower(strings.TrimSpace(m.query))
	m.items = m.items[:0]
	for _, item := range m.allItems {
		if query == "" || strings.Contains(strings.ToLower(item.label), query) {
			m.items = append(m.items, item)
		}
	}
	m.index = max(0, min(len(m.items)-1, m.index))
}

func (m *Model) runMenuAction(menu *actionMenu, action string) tea.Cmd {
	task := menu.task
	switch action {
	case "edit":
		return m.openEditTaskForm(task, fieldTitle)
	case "comment":
		m.beginPrompt("comment", "Comment", "", task.ID)
	case "block":
		m.beginPrompt("block", "Block reason", "", task.ID)
	case "complete":
		m.beginPrompt("complete", "Completion summary", "", task.ID)
	case "schedule":
		m.beginPrompt("schedule", "Schedule at (RFC3339; empty parks)", "", task.ID)
	case "attach-file":
		m.beginPrompt("attach-file", "Attachment file path", "", task.ID)
	case "attach-url":
		m.beginPrompt("attach-url", "Attachment URL", "", task.ID)
	case "move":
		m.menu = statusMenu(task)
		return nil
	case "relationships":
		m.menu = m.relationshipMenu(task)
		return nil
	default:
		if strings.HasPrefix(action, "terminate:") {
			m.confirm = &confirmState{action: "terminate", id: strings.TrimPrefix(action, "terminate:"), title: task.Title}
			return nil
		}
		if strings.HasPrefix(action, "relation-pick:") {
			m.menu = m.relationshipPicker(task, strings.TrimPrefix(action, "relation-pick:"))
			return nil
		}
		if strings.HasPrefix(action, "move:") {
			target := model.TaskStatus(strings.TrimPrefix(action, "move:"))
			switch target {
			case model.TaskStatusRunning:
				action = "start"
			case model.TaskStatusBlocked:
				m.beginPrompt("block", "Block reason", "", task.ID)
				return nil
			case model.TaskStatusDone:
				m.beginPrompt("complete", "Completion summary", "", task.ID)
				return nil
			case model.TaskStatusArchived:
				action = "archive"
			}
		}
		m.beginConfirm(action, task)
	}
	return nil
}

func (m *Model) updateMenu(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.menu.searching {
		switch message.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.menu.searching, m.menu.query = false, ""
			m.menu.applyQuery()
		case "enter":
			m.menu.searching = false
		case "backspace":
			runes := []rune(m.menu.query)
			if len(runes) > 0 {
				m.menu.query = string(runes[:len(runes)-1])
			}
			m.menu.applyQuery()
		case "ctrl+u":
			m.menu.query = ""
			m.menu.applyQuery()
		default:
			if message.Type == tea.KeyRunes {
				m.menu.query += string(message.Runes)
				m.menu.applyQuery()
			}
		}
		return m, nil
	}
	switch message.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.menu = nil
	case "up", "k":
		m.menu.index = max(0, m.menu.index-1)
	case "down", "j":
		m.menu.index = min(len(m.menu.items)-1, m.menu.index+1)
	case "home", "g":
		m.menu.index = 0
	case "end", "G":
		m.menu.index = max(0, len(m.menu.items)-1)
	case "/":
		m.menu.searching = true
	case "enter", " ":
		if len(m.menu.items) == 0 {
			return m, nil
		}
		menu, action := m.menu, m.menu.items[m.menu.index].action
		m.menu = nil
		return m, m.runMenuAction(menu, action)
	}
	return m, nil
}

func (m *Model) renderActionMenu(width, height int) string {
	menu := m.menu
	if menu == nil {
		return ""
	}
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(menu.title), lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(menu.task.Title, 58)), lipgloss.NewStyle().Foreground(colorMuted).Render(menu.task.ID), ""}
	if menu.searching || menu.query != "" {
		cursor := ""
		if menu.searching {
			cursor = "█"
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(colorFocus).Render("/"+menu.query+cursor), "")
	}
	start := max(0, menu.index-max(4, height/3))
	end := min(len(menu.items), start+max(5, height-9))
	for index := start; index < end; index++ {
		item := menu.items[index]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(colorText)
		if item.danger {
			style = style.Foreground(statusColor(model.TaskStatusBlocked))
		}
		if index == menu.index {
			marker, style = "› ", style.Bold(true).Background(lipgloss.Color("236"))
		}
		lines = append(lines, style.Width(58).Render(marker+item.label))
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render("↑/↓ select · / filter · enter open · esc close"))
	panelWidth := min(66, width-4)
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}
