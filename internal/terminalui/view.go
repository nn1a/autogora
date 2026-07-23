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
		model.TaskStatusArchived: "239",
	}
	return colors[status]
}

func statusLabel(status model.TaskStatus) string {
	labels := map[model.TaskStatus]string{
		model.TaskStatusTriage: "TRIAGE", model.TaskStatusTodo: "TODO", model.TaskStatusScheduled: "SCHEDULED",
		model.TaskStatusReady: "READY", model.TaskStatusRunning: "RUNNING", model.TaskStatusBlocked: "BLOCKED",
		model.TaskStatusReview: "REVIEW", model.TaskStatusDone: "DONE",
		model.TaskStatusArchived: "ARCHIVED",
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

func titleCase(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

func confirmationLabel(action string) string {
	labels := map[string]string{
		"promote": "Promote task?", "unblock": "Unblock task?", "archive": "Archive task?",
		"delete": "Permanently delete task?", "specify": "Specify with the board planner?",
		"decompose": "Decompose with the board planner?", "start": "Run this task with the dispatcher?",
		"terminate": "Terminate active run?",
	}
	if label := labels[action]; label != "" {
		return label
	}
	for prefix, label := range map[string]string{
		"move:": "Move task?", "link-": "Add execution relationship?", "unlink-": "Remove execution relationship?",
		"set-parent:": "Set hierarchy parent?", "add-subtask:": "Add hierarchy subtask?",
		"remove-parent:": "Remove hierarchy parent?", "remove-subtask:": "Remove hierarchy subtask?",
		"attachment-remove:": "Remove attachment?",
	} {
		if strings.HasPrefix(action, prefix) {
			return label
		}
	}
	return titleCase(action) + " task?"
}

func (m *Model) visibleColumns(boardWidth int) (int, int, int) {
	statuses := m.statuses()
	columnWidth := 24
	count := max(1, boardWidth/(columnWidth+1))
	count = min(count, len(statuses))
	start := m.column - count/2
	start = max(0, min(len(statuses)-count, start))
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

func (m *Model) renderEmptyBoard(width, height int) string {
	panelWidth := min(78, width-4)
	lineWidth := max(4, panelWidth-6)
	importCommand := fmt.Sprintf("autogora github import --repo OWNER/REPO --board %s", m.board)
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("Start your first workflow"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("Complete these once, then bring work into Triage."),
		"",
		lipgloss.NewStyle().Bold(true).Render("1  Configure agents"),
		"   " + truncate("Press A → Presets → Ctrl+P → Ctrl+S", lineWidth-3),
		lipgloss.NewStyle().Foreground(colorMuted).Render("   Detect installed CLIs, choose role defaults, and review Supervisor policy."),
		"",
		lipgloss.NewStyle().Bold(true).Render("2  Choose a workspace"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("   Press n → Execution → Workspace, or use the board default."),
		"",
		lipgloss.NewStyle().Bold(true).Render("3  Import or create work"),
		"   " + truncate(importCommand, lineWidth-3),
		lipgloss.NewStyle().Foreground(colorMuted).Render("   Run import in another terminal, then press r. Press n to create."),
	}
	if width < 70 || height < 20 {
		lines = []string{
			lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("Start your first workflow"), "",
			"1  Agents",
			truncate("A → Presets → Ctrl+P → Ctrl+S", lineWidth),
			"2  Workspace",
			truncate("n → Execution → Workspace", lineWidth),
			"3  Import or create",
			truncate("autogora github import", lineWidth),
			truncate("--repo OWNER/REPO --board "+m.board, lineWidth),
			truncate("or press n to create · r refresh", lineWidth),
		}
	}
	if height < 13 {
		lines = []string{
			lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("Empty board"),
			truncate("1 Agents: A → Presets → Ctrl+P", lineWidth),
			truncate("2 Workspace: n → Execution", lineWidth),
			truncate("3 Import: autogora github import", lineWidth),
			truncate("n create · r refresh · ? help", lineWidth),
		}
	}
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func detailTaskLine(task model.Task, width int) string {
	return lipgloss.NewStyle().Foreground(statusColor(task.Status)).Render("• "+truncate(task.Title, max(1, width-4))) +
		lipgloss.NewStyle().Foreground(colorMuted).Render("  "+task.ID)
}

func (m *Model) detailBody(width int) string {
	if m.detail == nil {
		return "Select a task"
	}
	task := m.detail.Task
	lines := []string{}
	switch m.detailTab {
	case 1:
		lines = append(lines, lipgloss.NewStyle().Bold(true).Render("Hierarchy"))
		if m.detail.ParentTask != nil {
			lines = append(lines, "Parent", detailTaskLine(*m.detail.ParentTask, width))
		}
		if len(m.detail.Subtasks) > 0 {
			lines = append(lines, "Subtasks")
			for _, related := range m.detail.Subtasks {
				lines = append(lines, detailTaskLine(related, width))
			}
		}
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Render("Dependencies"))
		if len(m.detail.Prerequisites) == 0 && len(m.detail.Dependents) == 0 {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No dependency links"))
		}
		if len(m.detail.Prerequisites) > 0 {
			lines = append(lines, "Blocked by")
			for _, related := range m.detail.Prerequisites {
				lines = append(lines, detailTaskLine(related, width))
			}
		}
		if len(m.detail.Dependents) > 0 {
			lines = append(lines, "Unlocks")
			for _, related := range m.detail.Dependents {
				lines = append(lines, detailTaskLine(related, width))
			}
		}
		if m.graph != nil {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("Connected %d · phases %d", m.graph.TotalConnectedNodes, m.graph.TotalPhases)))
		}
	case 2:
		lines = append(lines, lipgloss.NewStyle().Bold(true).Render("Activity"))
		if len(m.detail.Runs)+len(m.detail.Comments)+len(m.detail.Events)+len(m.detail.Attachments) == 0 {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No activity yet"))
		}
		for index := len(m.detail.Comments) - 1; index >= 0; index-- {
			comment := m.detail.Comments[index]
			lines = append(lines, "", lipgloss.NewStyle().Foreground(colorFocus).Render(comment.Author), lipgloss.NewStyle().Width(max(1, width-4)).Render(comment.Body))
		}
		for index := len(m.detail.Runs) - 1; index >= 0; index-- {
			run := m.detail.Runs[index]
			lines = append(lines, "", fmt.Sprintf("Run %s · %s", run.Status, run.WorkerID), lipgloss.NewStyle().Foreground(colorMuted).Render(run.ID))
			for _, config := range m.detail.RunAgentConfigs {
				if config.RunID != run.ID {
					continue
				}
				modelName := config.Model
				if modelName == "" {
					modelName = "CLI default (unpinned)"
				}
				lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%s · %s · %s", config.Profile, config.Runtime, modelName)))
				break
			}
		}
		for index := len(m.detail.Events) - 1; index >= 0 && index >= len(m.detail.Events)-10; index-- {
			event := m.detail.Events[index]
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(event.CreatedAt+"  ")+event.Kind)
		}
		if len(m.detail.Attachments) > 0 {
			lines = append(lines, "", lipgloss.NewStyle().Bold(true).Render("Attachments"))
			for _, attachment := range m.detail.Attachments {
				lines = append(lines, "• "+attachment.Name+lipgloss.NewStyle().Foreground(colorMuted).Render("  "+attachment.Kind))
			}
		}
	default:
		status := lipgloss.NewStyle().Bold(true).Foreground(statusColor(task.Status)).Render(statusLabel(task.Status))
		lines = append(lines,
			status+lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("  P%d · %s · %s", task.Priority, pointer(task.Assignee, "unassigned"), task.Runtime)),
			lipgloss.NewStyle().Foreground(colorMuted).Render(task.ID),
		)
		if strings.TrimSpace(task.Body) != "" {
			lines = append(lines, "", lipgloss.NewStyle().Width(max(1, width-4)).Foreground(colorText).Render(task.Body))
		}
		if task.BlockReason != nil {
			lines = append(lines, "", lipgloss.NewStyle().Foreground(statusColor(model.TaskStatusBlocked)).Render("Blocked: "+*task.BlockReason))
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("Created %s\nUpdated %s", task.CreatedAt, task.UpdatedAt)))
	}
	return strings.Join(lines, "\n")
}

func clipped(value string, start, height int) string {
	lines := strings.Split(value, "\n")
	start = max(0, min(start, max(0, len(lines)-1)))
	end := min(len(lines), start+max(1, height))
	return strings.Join(lines[start:end], "\n")
}

func (m *Model) renderDetail(width, height int) string {
	if m.detail == nil {
		return baseBorder.Copy().Width(width-2).Height(height).Padding(0, 1).Foreground(colorMuted).Render("Select a task")
	}
	task := m.detail.Task
	tabs := []string{"Overview", "Relations", "Activity"}
	for index := range tabs {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if index == m.detailTab {
			style = style.Foreground(colorFocus).Bold(true).Underline(true)
		}
		tabs[index] = style.Render(tabs[index])
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(truncate(task.Title, width-4))
	bodyHeight := max(1, height-4)
	body := clipped(m.detailBody(width), m.detailScroll, bodyHeight)
	return baseBorder.Copy().BorderForeground(statusColor(task.Status)).Width(width-2).Height(height).Padding(0, 1).Render(title + "\n" + strings.Join(tabs, "  ") + "\n\n" + body)
}

func (m *Model) View() string {
	if m.width < 10 || m.height < 6 {
		return "Terminal is too small\n"
	}
	boardName := m.board
	if m.boardContext != nil && m.boardContext.Metadata.Name != "" {
		boardName = m.boardContext.Metadata.Name
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("AUTOGORA") +
		lipgloss.NewStyle().Foreground(colorText).Render("  "+boardName)
	if m.boardContext != nil && m.width >= 100 {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("  %d profiles · planner %s", len(m.boardContext.Profiles), m.boardContext.Metadata.Orchestration.PlannerRuntime))
	} else if m.boardContext != nil && m.width >= 70 {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("  %d profiles", len(m.boardContext.Profiles)))
	}
	if m.inputMode == "search" {
		header += lipgloss.NewStyle().Foreground(colorFocus).Render("  /" + m.searchDraft + "█")
	} else if m.search != "" {
		header += lipgloss.NewStyle().Foreground(colorFocus).Render("  /" + m.search)
	}
	if m.showArchived {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  + archived")
	}
	filters := []string{}
	if m.tenantFilter != "" {
		filters = append(filters, "tenant="+m.tenantFilter)
	}
	if m.assigneeFilter != "" {
		filters = append(filters, "assignee="+m.assigneeFilter)
	}
	if m.runtimeFilter != "" {
		filters = append(filters, "runtime="+string(m.runtimeFilter))
	}
	if len(filters) > 0 {
		label := fmt.Sprintf("%d filters", len(filters))
		if m.width >= 100 {
			label = strings.Join(filters, " · ")
		}
		header += lipgloss.NewStyle().Foreground(colorFocus).Render("  [" + label + "]")
	}
	if m.loading {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  refreshing…")
	} else if !m.updated.IsZero() && m.width >= 80 {
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  updated " + m.updated.Format(time.Kitchen))
	}
	visibleErr := m.err
	if visibleErr == nil {
		visibleErr = m.backgroundError()
	}
	if visibleErr != nil {
		header += lipgloss.NewStyle().Foreground(statusColor(model.TaskStatusBlocked)).Render("  " + truncate(visibleErr.Error(), max(10, m.width-30)))
	} else if m.notice != "" {
		header += lipgloss.NewStyle().Foreground(statusColor(model.TaskStatusDone)).Render("  " + m.notice)
	}
	if m.busy {
		busyLabel := "applying…"
		if m.busyAction == "start" {
			busyLabel = "dispatcher running · navigation available"
		}
		header += lipgloss.NewStyle().Foreground(colorMuted).Render("  " + busyLabel)
	}

	footerText := "space actions  n new  e edit  A agents  O board settings  m move  f filters  ? help"
	if m.width < 100 {
		footerText = "space actions · n new · A agents · O board · ? help"
	}
	if m.width < 60 {
		footerText = "A agents · O board · ? help"
	}
	if m.width < 30 {
		footerText = "A agents · ? help"
	}
	footer := lipgloss.NewStyle().Foreground(colorMuted).Render(footerText)
	contentHeight := max(3, m.height-4)
	detailWidth := 0
	boardWidth := m.width
	wideDetail := m.width >= 105
	if wideDetail {
		detailWidth = min(44, m.width/3)
		boardWidth -= detailWidth + 1
	}
	start, count, columnWidth := m.visibleColumns(boardWidth)
	statuses := m.statuses()
	columns := make([]string, 0, count)
	for index := start; index < start+count; index++ {
		columns = append(columns, m.renderColumn(statuses[index], columnWidth, contentHeight, index == m.column))
	}
	board := lipgloss.JoinHorizontal(lipgloss.Top, columns...)
	content := board
	if len(m.allTasks) == 0 && !m.loading && m.tasksErr == nil && m.search == "" && m.tenantFilter == "" && m.assigneeFilter == "" && m.runtimeFilter == "" {
		content = m.renderEmptyBoard(m.width, contentHeight)
	} else if wideDetail {
		content = lipgloss.JoinHorizontal(lipgloss.Top, board, " ", m.renderDetail(detailWidth, contentHeight))
	} else if m.detailOn {
		content = m.renderDetail(m.width, contentHeight)
	}
	if m.help {
		help := baseBorder.Copy().BorderForeground(colorFocus).Width(min(62, m.width-4)).Padding(1, 2).Render(
			"Navigate\n  h/l · ←/→     columns\n  j/k · ↑/↓     cards\n  g / G         first / last\n  /             search\n  f             tenant/assignee/runtime filters\n  a             archived column\n  tab · 1/2/3   detail tabs\n  pgup / pgdown scroll detail\n\nChange\n  space         task action menu\n  n             full create form\n  e / s         edit form / agent section\n  A             global agents and Supervisor\n  O             board orchestration settings\n  m             move to status\n  C             add comment\n  p / u         promote / unblock\n  b / c / x     block / complete / archive\n\n  r refresh  ? help  q quit",
		)
		content = lipgloss.Place(m.width, contentHeight, lipgloss.Center, lipgloss.Center, help)
	}
	if m.inputMode != "" && m.inputMode != "search" {
		prompt := baseBorder.Copy().BorderForeground(colorFocus).Width(min(70, m.width-4)).Padding(1, 2).Render(
			lipgloss.NewStyle().Bold(true).Render(m.promptLabel) + "\n\n" + m.promptDraft + "█\n\n" + lipgloss.NewStyle().Foreground(colorMuted).Render("enter apply · esc cancel"),
		)
		content = lipgloss.Place(m.width, contentHeight, lipgloss.Center, lipgloss.Center, prompt)
	}
	if m.confirm != nil {
		confirmation := baseBorder.Copy().BorderForeground(statusColor(model.TaskStatusBlocked)).Width(min(68, m.width-4)).Padding(1, 2).Render(
			lipgloss.NewStyle().Bold(true).Render(confirmationLabel(m.confirm.action)) + "\n\n" + truncate(m.confirm.title, min(58, m.width-10)) + "\n" +
				lipgloss.NewStyle().Foreground(colorMuted).Render(m.confirm.id) + "\n\n" + lipgloss.NewStyle().Foreground(colorMuted).Render("y/enter confirm · n/esc cancel"),
		)
		content = lipgloss.Place(m.width, contentHeight, lipgloss.Center, lipgloss.Center, confirmation)
	}
	if m.form != nil {
		content = m.renderTaskForm(m.width, contentHeight)
	}
	if m.menu != nil {
		content = m.renderActionMenu(m.width, contentHeight)
	}
	if m.settings != nil {
		content = m.renderBoardSettings(m.width, contentHeight)
	}
	if m.globalLoading {
		content = m.renderGlobalAgentsLoading(m.width, contentHeight)
	}
	if m.globalAgents != nil {
		content = m.renderGlobalAgents(m.width, contentHeight)
	}
	return lipgloss.NewStyle().Width(m.width).Render(header) + "\n" + content + "\n" + footer
}
