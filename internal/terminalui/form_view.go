package terminalui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/nn1a/autogora/internal/model"
)

func formFieldLabel(field formField) string {
	labels := map[formField]string{
		fieldTitle: "Title", fieldBody: "Description", fieldStatus: "Status", fieldPriority: "Priority",
		fieldProfile: "Board profile", fieldAssignee: "Assignee", fieldRuntime: "Runtime", fieldSkills: "Skills",
		fieldTenant: "Tenant", fieldWorkspaceKind: "Workspace kind", fieldWorkspace: "Workspace path", fieldGoalMode: "Goal mode", fieldScheduledAt: "Run after (RFC3339)",
		fieldBranch: "Branch", fieldMaxRuntime: "Max runtime (seconds)", fieldMaxRetries: "Max retries", fieldGoalMaxTurns: "Goal max turns",
	}
	return labels[field]
}

type formRouteSummary struct {
	profile     string
	assignee    string
	runtime     string
	model       string
	provider    string
	description string
}

func (f *taskForm) effectiveRoute() formRouteSummary {
	summary := formRouteSummary{
		profile:  "Custom",
		assignee: strings.TrimSpace(f.inputs[fieldAssignee].Value()),
		runtime:  formRuntimes[f.runtimeIndex],
	}
	if summary.assignee == "" {
		summary.assignee = "Unassigned"
	}
	if profileIndex := f.profileIndexForAssignee(); profileIndex > 0 {
		profile := f.profiles[profileIndex-1]
		summary.profile = profile.Name
		summary.runtime = string(profile.Runtime)
		summary.model = strings.TrimSpace(profile.Model)
		summary.provider = strings.TrimSpace(profile.Provider)
		summary.description = strings.TrimSpace(profile.Description)
	}
	if summary.runtime == string(model.RuntimeManual) {
		summary.model = "Manual task"
		summary.provider = "Not applicable"
		return summary
	}
	if summary.model == "" {
		summary.model = "CLI default (unpinned)"
	}
	if summary.provider == "" {
		summary.provider = "CLI default"
	}
	return summary
}

func (f *taskForm) selectionPosition(field formField) (int, int) {
	switch field {
	case fieldStatus:
		return f.statusIndex + 1, len(formStatuses)
	case fieldProfile:
		return f.profileIndex + 1, len(f.profiles) + 1
	case fieldRuntime:
		return f.runtimeIndex + 1, len(formRuntimes)
	case fieldWorkspaceKind:
		return f.workspaceIndex + 1, len(formWorkspaceKinds)
	default:
		return 0, 0
	}
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
		model := strings.TrimSpace(profile.Model)
		if model == "" {
			model = "CLI default (unpinned)"
		}
		return fmt.Sprintf("%s · %s · %s", profile.Name, profile.Runtime, model)
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
		} else if position, count := f.selectionPosition(field); count > 0 {
			hint += fmt.Sprintf(" · %d/%d", position, count)
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

func (f *taskForm) renderRouteSummary(width int) string {
	route := f.effectiveRoute()
	title := lipgloss.NewStyle().Bold(true).Foreground(colorFocus).Render("Effective route")
	details := fmt.Sprintf(
		"Profile: %s · Assignee: %s\nRuntime: %s · Model: %s\nProvider: %s",
		route.profile,
		route.assignee,
		route.runtime,
		route.model,
		route.provider,
	)
	lines := []string{
		title,
		lipgloss.NewStyle().Width(width).Foreground(colorText).Render(details),
		lipgloss.NewStyle().Width(width).Foreground(colorMuted).Render(
			"Profiles own Assignee and Runtime. Choosing Custom or changing Runtime while profiled clears the profile Assignee, so the saved route is custom. Typing an exact listed name reselects that profile and its runtime/model. Manual uses no agent and shows Manual task.",
		),
	}
	if len(f.profiles) == 0 {
		availability := "Custom + Manual routes remain available."
		if f.mode == "create" {
			availability = "Custom + Manual task creation remains available; Ready requires an agent route."
		}
		lines = append(lines, lipgloss.NewStyle().Width(width).Foreground(statusColor("scheduled")).Render(
			"No runnable profiles. Configure one in Agents / Board settings. "+availability,
		))
	} else if route.description != "" && f.focus == fieldProfile {
		lines = append(lines, lipgloss.NewStyle().Width(width).Foreground(colorMuted).Render("About: "+route.description))
	}
	return strings.Join(lines, "\n")
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
	if f.activeRun {
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorMuted).Render("Running task · only Priority is editable."),
			lipgloss.NewStyle().Foreground(colorMuted).Render("Terminate the active run before changing the title, description, tenant, or execution settings."),
			"",
		)
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
	if f.step() == 1 {
		lines = append(lines, f.renderRouteSummary(innerWidth), "")
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
