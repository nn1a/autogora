package terminalui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/nn1a/autogora/internal/model"
)

var (
	colorMuted = lipgloss.Color("241")
	colorText  = lipgloss.Color("252")
	colorFocus = lipgloss.Color("81")
	baseBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238"))
)

func statusColor(status model.TaskStatus) lipgloss.Color {
	colors := map[model.TaskStatus]lipgloss.Color{
		model.TaskStatusTriage: "213", model.TaskStatusTodo: "245", model.TaskStatusScheduled: "111",
		model.TaskStatusReady: "81", model.TaskStatusRunning: "220", model.TaskStatusBlocked: "203",
		model.TaskStatusReview: "141", model.TaskStatusDone: "78",
	}
	return colors[status]
}

func statusLabel(status model.TaskStatus) string {
	labels := map[model.TaskStatus]string{
		model.TaskStatusTriage: "TRIAGE", model.TaskStatusTodo: "TODO", model.TaskStatusScheduled: "SCHEDULED",
		model.TaskStatusReady: "READY", model.TaskStatusRunning: "RUNNING", model.TaskStatusBlocked: "BLOCKED",
		model.TaskStatusReview: "REVIEW", model.TaskStatusDone: "DONE",
	}
	return labels[status]
}

func truncate(value string, width int) string {
	value = strings.Join(strings.Fields(value), " ")
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func pointer(value *string, fallback string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return fallback
	}
	return *value
}

func (m *Model) visibleColumns(boardWidth int) (int, int, int) {
	columnWidth := 24
	count := max(1, boardWidth/(columnWidth+1))
	count = min(count, len(boardStatuses))
	start := m.column - count/2
	start = max(0, min(len(boardStatuses)-count, start))
	return start, count, max(18, boardWidth/count-1)
}

func (m *Model) renderColumn(status model.TaskStatus, width, height int, focused bool) string {
	items := m.tasks[status]
	header := lipgloss.NewStyle().Bold(true).Foreground(statusColor(status)).Render(
		fmt.Sprintf("%s  %d", statusLabel(status), len(items)),
	)
	lines := []string{header, ""}
	cardWidth := max(8, width-4)
	available := max(1, height-4)
	start := 0
	cursor := m.cursors[status]
	if cursor >= available {
		start = cursor - available + 1
	}
	for index := start; index < len(items) && len(lines) < height-1; index++ {
		task := items[index]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(colorText)
		if focused && index == cursor {
			marker = "› "
			style = style.Bold(true).Foreground(colorFocus)
		}
		meta := fmt.Sprintf("P%d · %s", task.Priority, pointer(task.Assignee, "unassigned"))
		lines = append(lines, style.Render(marker+truncate(task.Title, cardWidth-2)))
		if len(lines) < height-1 {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+truncate(meta, cardWidth-2)))
		}
	}
	if len(items) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("  No tasks"))
	}
	borderColor := lipgloss.Color("238")
	if focused {
		borderColor = statusColor(status)
	}
	return baseBorder.Copy().BorderForeground(borderColor).Width(width-2).Height(height).Padding(0, 1).Render(strings.Join(lines, "\n"))
}

func (m *Model) renderDetail(width, height int) string {
	if m.detail == nil {
		return baseBorder.Copy().Width(width-2).Height(height).Padding(0, 1).Foreground(colorMuted).Render("Select a task")
	}
	task := m.detail.Task
	status := lipgloss.NewStyle().Bold(true).Foreground(statusColor(task.Status)).Render(statusLabel(task.Status))
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(truncate(task.Title, width-4)),
		status + lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("  P%d · %s · %s", task.Priority, pointer(task.Assignee, "unassigned"), task.Runtime)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(task.ID),
	}
	if strings.TrimSpace(task.Body) != "" {
		lines = append(lines, "", lipgloss.NewStyle().Width(max(1, width-4)).Foreground(colorText).Render(task.Body))
	}
	if task.BlockReason != nil {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(statusColor(model.TaskStatusBlocked)).Render("Blocked: "+*task.BlockReason))
	}
	relations := fmt.Sprintf("Prerequisites %d · Dependents %d · Subtasks %d", len(m.detail.Prerequisites), len(m.detail.Dependents), len(m.detail.Subtasks))
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(relations))
	if len(m.detail.Comments) > 0 {
		last := m.detail.Comments[len(m.detail.Comments)-1]
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Render("Latest comment"), lipgloss.NewStyle().Width(max(1, width-4)).Render(last.Author+": "+last.Body))
	}
	return baseBorder.Copy().BorderForeground(statusColor(task.Status)).Width(width-2).Height(height).Padding(0, 1).Render(strings.Join(lines, "\n"))
}

func (m *Model) View() string {
	if m.width < 10 || m.height < 6 {
		return "Terminal is too small\n"
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("AUTOGORA") +
		lipgloss.NewStyle().Foreground(colorText).Render("  "+m.board)
	if m.loading {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  refreshing…")
	} else if !m.updated.IsZero() {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  updated " + m.updated.Format(time.Kitchen))
	}
	if m.err != nil {
		header += lipgloss.NewStyle().Foreground(statusColor(model.TaskStatusBlocked)).Render("  " + truncate(m.err.Error(), max(10, m.width-30)))
	}

	footer := lipgloss.NewStyle().Foreground(colorMuted).Render("←/→ columns  ↑/↓ cards  enter details  r refresh  ? help  q quit")
	contentHeight := max(3, m.height-4)
	detailWidth := 0
	boardWidth := m.width
	wideDetail := m.width >= 105
	if wideDetail {
		detailWidth = min(44, m.width/3)
		boardWidth -= detailWidth + 1
	}
	start, count, columnWidth := m.visibleColumns(boardWidth)
	columns := make([]string, 0, count)
	for index := start; index < start+count; index++ {
		columns = append(columns, m.renderColumn(boardStatuses[index], columnWidth, contentHeight, index == m.column))
	}
	board := lipgloss.JoinHorizontal(lipgloss.Top, columns...)
	content := board
	if wideDetail {
		content = lipgloss.JoinHorizontal(lipgloss.Top, board, " ", m.renderDetail(detailWidth, contentHeight))
	} else if m.detailOn {
		content = m.renderDetail(m.width, contentHeight)
	}
	if m.help {
		help := baseBorder.Copy().BorderForeground(colorFocus).Width(min(62, m.width-4)).Padding(1, 2).Render(
			"Keyboard\n\n  h/l or ←/→   move between columns\n  j/k or ↑/↓   move between cards\n  g / G         first / last card\n  enter         toggle details on narrow terminals\n  r             refresh now\n  ? or esc      close help\n  q             quit",
		)
		content = lipgloss.Place(m.width, contentHeight, lipgloss.Center, lipgloss.Center, help)
	}
	return lipgloss.NewStyle().Width(m.width).Render(header) + "\n" + content + "\n" + footer
}
