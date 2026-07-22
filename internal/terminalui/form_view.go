package terminalui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func formFieldLabel(field formField) string {
	labels := map[formField]string{
		fieldTitle: "Title", fieldBody: "Description", fieldStatus: "Status", fieldPriority: "Priority",
		fieldProfile: "Board profile", fieldAssignee: "Assignee", fieldRuntime: "Runtime", fieldSkills: "Skills",
		fieldTenant: "Tenant", fieldWorkspaceKind: "Workspace kind", fieldWorkspace: "Workspace path", fieldGoalMode: "Goal mode",
	}
	return labels[field]
}

func (f *taskForm) selectValue(field formField) string {
	switch field {
	case fieldStatus:
		return formStatuses[f.statusIndex]
	case fieldProfile:
		if f.profileIndex == 0 {
			return "Custom"
		}
		profile := f.profiles[f.profileIndex-1]
		return fmt.Sprintf("%s · %s", profile.Name, profile.Runtime)
	case fieldRuntime:
		return formRuntimes[f.runtimeIndex]
	case fieldWorkspaceKind:
		return formWorkspaceKinds[f.workspaceIndex]
	case fieldGoalMode:
		if f.goalMode {
			return "[x] enabled"
		}
		return "[ ] disabled"
	}
	return ""
}

func (f *taskForm) renderField(field formField, width int) string {
	focused, locked := field == f.focus, f.locked(field)
	marker := "  "
	labelColor := colorMuted
	if focused {
		marker, labelColor = "› ", colorFocus
	}
	label := lipgloss.NewStyle().Bold(focused).Foreground(labelColor).Render(marker + formFieldLabel(field))
	if locked {
		label += lipgloss.NewStyle().Foreground(colorMuted).Render("  locked while Running")
	} else if focused && !f.textField(field) && field != fieldBody {
		hint := "  ↑/↓ select"
		if field == fieldGoalMode {
			hint = "  Space toggle"
		}
		label += lipgloss.NewStyle().Foreground(colorMuted).Render(hint)
	}
	var value string
	if field == fieldBody {
		f.body.SetWidth(max(20, width-2))
		value = f.body.View()
	} else if input, exists := f.inputs[field]; exists {
		input.Width = max(10, width-2)
		f.inputs[field] = input
		value = input.View()
	} else {
		value = f.selectValue(field)
		if !locked {
			value = "‹  " + value + "  ›"
		}
		value = lipgloss.NewStyle().Foreground(colorText).Render(value)
	}
	if locked {
		value = lipgloss.NewStyle().Foreground(colorMuted).Render(value)
	}
	return lipgloss.NewStyle().Width(width).Render(label + "\n" + value)
}

func (m *Model) renderTaskForm(width, height int) string {
	f := m.form
	if f == nil {
		return ""
	}
	panelWidth := min(92, width-4)
	innerWidth := max(24, panelWidth-6)
	f.body.SetHeight(max(3, min(6, height-10)))
	mode := "Create task"
	if f.mode == "edit" {
		mode = "Edit task"
	}
	steps := []string{"Task", "Agent", "Execution"}
	for index := range steps {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if index == f.step() {
			style = style.Foreground(colorFocus).Bold(true).Underline(true)
		}
		steps[index] = style.Render(fmt.Sprintf("%d %s", index+1, steps[index]))
	}
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(mode),
		strings.Join(steps, "   "), "",
	}
	fields := f.stepFields()[f.step()]
	if innerWidth >= 66 && f.step() != 0 {
		columnWidth := (innerWidth - 3) / 2
		for index := 0; index < len(fields); index += 2 {
			left := f.renderField(fields[index], columnWidth)
			right := ""
			if index+1 < len(fields) {
				right = f.renderField(fields[index+1], columnWidth)
			}
			lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right), "")
		}
	} else {
		for _, field := range fields {
			lines = append(lines, f.renderField(field, innerWidth), "")
		}
	}
	if f.focus == fieldProfile && f.profileIndex > 0 {
		description := strings.TrimSpace(f.profiles[f.profileIndex-1].Description)
		if description != "" {
			lines = append(lines, lipgloss.NewStyle().Width(innerWidth).Foreground(colorMuted).Render(description), "")
		}
	}
	if f.err != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(f.err.Error()), "")
	}
	helpStyle := lipgloss.NewStyle().Foreground(colorMuted)
	lines = append(lines,
		helpStyle.Render("↑/↓ select value · Space toggle · Tab/Shift+Tab field"),
		helpStyle.Render("Ctrl+←/→ section · Ctrl+S save · Esc cancel"),
	)
	content := strings.Join(lines, "\n")
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(content)
	if lipgloss.Height(panel) > height {
		panel = clipped(panel, max(0, lipgloss.Height(panel)-height), height)
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}
