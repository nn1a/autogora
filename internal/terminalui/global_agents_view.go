package terminalui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func globalAgentFieldLabel(field globalAgentField) string {
	labels := map[globalAgentField]string{
		globalAgentSelection:        "Configured agent",
		globalAgentID:               "Agent ID",
		globalAgentRuntime:          "Runtime",
		globalAgentCommand:          "Command",
		globalAgentModel:            "Model",
		globalAgentProvider:         "Provider",
		globalAgentEnabled:          "Eligible for work",
		globalAgentMaxConcurrent:    "Max concurrent",
		globalAgentRoles:            "Roles",
		globalAgentFallbacks:        "Fallback agents",
		globalDefaultWorkers:        "Preferred workers",
		globalDefaultPlanners:       "Preferred planners",
		globalDefaultCoordinators:   "Preferred coordinators",
		globalDefaultJudges:         "Preferred judges",
		globalSupervisorAutoStart:   "AutoStart for future UIs",
		globalSupervisorMaxWorkers:  "Maximum workers",
		globalSupervisorAllowWrites: "Workspace writes",
		globalPresetSelection:       "Built-in preset",
		globalPresetMode:            "Apply mode",
	}
	return labels[field]
}

func globalAgentFieldHint(field globalAgentField) string {
	switch field {
	case globalAgentEnabled, globalSupervisorAutoStart, globalSupervisorAllowWrites:
		return "Space toggle"
	case globalAgentSelection, globalAgentRuntime, globalPresetSelection, globalPresetMode:
		return "←/→ select"
	default:
		return "type value"
	}
}

func (f *globalAgentsForm) renderField(field globalAgentField, width int) []string {
	focused := field == f.focus
	marker, color := "  ", colorMuted
	if focused {
		marker, color = "› ", colorFocus
	}
	label := marker + globalAgentFieldLabel(field)
	if focused {
		label += " · " + globalAgentFieldHint(field)
	}
	labelLine := lipgloss.NewStyle().Bold(focused).Foreground(color).Render(
		truncate(label, width),
	)
	value := ""
	if input, exists := f.inputs[field]; exists {
		input.Width = max(8, width-2)
		f.inputs[field] = input
		value = input.View()
	} else {
		value = lipgloss.NewStyle().Foreground(colorText).Render(
			truncate("‹  "+f.selectValue(field)+"  ›", width-2),
		)
	}
	return []string{labelLine, "  " + value, ""}
}

func (f *globalAgentsForm) bodyLines(width int) ([]string, int) {
	lines := []string{}
	focusLine := 0
	for _, field := range f.fields() {
		if field == f.focus {
			focusLine = len(lines)
		}
		lines = append(lines, f.renderField(field, width)...)
	}
	if f.tab == globalAgentsRegistry && len(f.agents) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate("No agents yet. Press Ctrl+N or apply a preset from the Presets tab.", width),
		))
	}
	return lines, focusLine
}

func globalAgentsTabs(active globalAgentsTab) string {
	tabs := append([]string{}, globalAgentsTabLabels...)
	for index := range tabs {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if globalAgentsTab(index) == active {
			style = style.Foreground(colorFocus).Bold(true).Underline(true)
		}
		tabs[index] = style.Render(tabs[index])
	}
	return strings.Join(tabs, "   ")
}

func supervisorStatusLines(f *globalAgentsForm, width int) []string {
	state := "stopped"
	color := colorMuted
	if f.status.Desired && f.status.Running {
		state, color = "running", statusColor("done")
	} else if f.status.Desired {
		state, color = "waiting to restart", statusColor("scheduled")
	}
	summary := fmt.Sprintf(
		"Current process: %s · running %t · desired %t · max %d · writes %t · restarts %d",
		state, f.status.Running, f.status.Desired, f.status.MaxWorkers,
		f.status.AllowWrites, f.status.RestartCount,
	)
	lines := []string{
		lipgloss.NewStyle().Foreground(color).Render(truncate(summary, width)),
	}
	if f.status.NextAttemptAt != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate("Next attempt: "+f.status.NextAttemptAt, width),
		))
	}
	if f.status.LastError != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(
			truncate("Last error: "+f.status.LastError, width),
		))
	}
	return lines
}

func presetDetectionLines(f *globalAgentsForm, width int) []string {
	if len(f.detections) == 0 {
		return nil
	}
	values := make([]string, 0, len(f.detections))
	for _, detection := range f.detections {
		label := string(detection.Runtime) + "=" + detection.State
		if detection.Executable != "" {
			label += " (" + detection.Executable + ")"
		}
		values = append(values, label)
	}
	return []string{lipgloss.NewStyle().Foreground(colorMuted).Render(
		truncate("Last detection: "+strings.Join(values, " · "), width),
	)}
}

func (f *globalAgentsForm) contextLines(width int) []string {
	switch f.tab {
	case globalAgentsDefaults:
		return []string{
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Preferred lists are tried in order; eligible role agents remain automatic fallbacks.", width),
			),
		}
	case globalAgentsSupervisor:
		lines := []string{
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Supervisor and Dispatcher are deterministic host services; these fields do not select a model.", width),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Ctrl+S preserves the current desired state; only Ctrl+R starts and Ctrl+T stops this process.", width),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("AutoStart is the startup policy for future supported UI sessions.", width),
			),
		}
		return append(lines, supervisorStatusLines(f, width)...)
	case globalAgentsPresets:
		lines := []string{
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Ctrl+P only checks PATH and runs --version; it never sends a prompt or calls a paid model API.", width),
			),
		}
		if len(f.presets) > 0 {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate(f.presets[f.presetIndex].Description, width),
			))
		}
		if f.replace {
			lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("scheduled")).Render(
				truncate("Replace overwrites matching preset agents and all preferred role lists; unrelated agents stay.", width),
			))
		}
		return append(lines, presetDetectionLines(f, width)...)
	default:
		return []string{
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("An empty Model or Provider leaves that choice to the coding-agent CLI; no model is pinned.", width),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Ctrl+N adds · Ctrl+X removes the selected draft agent", width),
			),
		}
	}
}

func (m *Model) renderCompactGlobalAgents(width, height int) string {
	form := m.globalAgents
	panelWidth := max(8, width-2)
	innerWidth := max(4, panelWidth-4)
	title := lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(
		truncate("Global · "+globalAgentsTabLabels[form.tab], innerWidth),
	)
	sectionHelp := lipgloss.NewStyle().Foreground(colorMuted).Render(
		truncate("Ctrl+←/→ section", innerWidth),
	)
	selectionHelp := lipgloss.NewStyle().Foreground(colorMuted).Render(
		truncate("←/→ select · ↑/↓ field", innerWidth),
	)
	actionHelp := map[globalAgentsTab]string{
		globalAgentsRegistry:   "Ctrl+N/X add/remove",
		globalAgentsDefaults:   "Edit preferred order",
		globalAgentsSupervisor: "Ctrl+R/T start/stop",
		globalAgentsPresets:    "Ctrl+P apply preset",
	}[form.tab]
	fieldPrefix := truncate(globalAgentFieldLabel(form.focus)+": ", max(1, innerWidth-1))
	fieldValue := ""
	if input, exists := form.inputs[form.focus]; exists {
		input.Width = max(1, innerWidth-lipgloss.Width(fieldPrefix))
		form.inputs[form.focus] = input
		fieldValue = input.View()
	} else {
		fieldValue = truncate(form.selectValue(form.focus), max(1, innerWidth-lipgloss.Width(fieldPrefix)))
	}
	field := lipgloss.NewStyle().Foreground(colorFocus).Render(fieldPrefix) + fieldValue
	if height <= 8 {
		lines := []string{
			lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(actionHelp, innerWidth)),
			sectionHelp,
			selectionHelp,
			field,
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Ctrl+S save · ? help · Esc cancel", innerWidth),
			),
		}
		if height >= 5 {
			lines = append([]string{title}, lines...)
		}
		if len(lines) > height {
			lines = lines[:height]
		}
		return lipgloss.Place(
			width, height, lipgloss.Center, lipgloss.Center,
			strings.Join(lines, "\n"),
		)
	}
	innerHeight := max(1, height-2)
	lines := []string{title}
	candidates := []string{
		lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(actionHelp, innerWidth)),
		sectionHelp,
		selectionHelp,
		field,
		lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate("Ctrl+S save · ? help · Esc cancel", innerWidth),
		),
	}
	for _, candidate := range candidates {
		if len(lines) >= innerHeight {
			break
		}
		lines = append(lines, candidate)
	}
	if len(lines) < innerHeight && form.err != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(
			truncate(form.err.Error(), innerWidth),
		))
	} else if len(lines) < innerHeight && (form.saving || form.detecting || form.lifecycle != "") {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate(globalAgentsBusyLabel(form), innerWidth),
		))
	} else if len(lines) < innerHeight && form.notice != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("done")).Render(
			truncate(form.notice, innerWidth),
		))
	}
	if len(lines) > innerHeight {
		lines = lines[:innerHeight]
	}
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(0, 1).Render(
		strings.Join(lines, "\n"),
	)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func (m *Model) renderGlobalAgentsHelp(width, height int) string {
	panelWidth := max(8, min(72, width-2))
	innerWidth := max(4, panelWidth-6)
	lines := []string{
		"Global agents help",
		"Ctrl+←/→  change section",
		"↑/↓       change field",
		"←/→       select a list value",
		"Ctrl+N/X  add/remove an agent",
		"Ctrl+P    detect CLIs and apply preset to draft",
		"Ctrl+R/T  save + start / stop Supervisor",
		"Ctrl+S    validate, save, and reconcile",
		"? / Esc   close this help",
	}
	visible := make([]string, 0, len(lines))
	for _, line := range lines {
		visible = append(visible, truncate(line, innerWidth))
	}
	innerHeight := max(1, height-2)
	if len(visible) > innerHeight {
		visible = visible[:innerHeight]
	}
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(0, 1).Render(
		strings.Join(visible, "\n"),
	)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func globalAgentsBusyLabel(form *globalAgentsForm) string {
	switch {
	case form.saving:
		return "Validating, saving, and applying Supervisor settings…"
	case form.detecting:
		return "Detecting installed coding-agent CLIs with --version…"
	case form.lifecycle == "start":
		return "Saving settings and starting Supervisor…"
	case form.lifecycle == "stop":
		return "Stopping Supervisor…"
	default:
		return ""
	}
}

func (m *Model) renderGlobalAgents(width, height int) string {
	form := m.globalAgents
	if form == nil {
		return ""
	}
	if form.help {
		return m.renderGlobalAgentsHelp(width, height)
	}
	if height < 14 || width < 50 {
		return m.renderCompactGlobalAgents(width, height)
	}
	panelWidth := min(100, width-2)
	innerWidth := max(12, panelWidth-6)
	contextLines := form.contextLines(innerWidth)
	fixedHeight := 12 + len(contextLines)
	if form.activeRuns > 0 {
		fixedHeight += 2
	}
	bodyHeight := max(1, height-fixedHeight)
	lines, focusLine := form.bodyLines(innerWidth)
	maxScroll := max(0, len(lines)-bodyHeight)
	if focusLine < form.scroll {
		form.scroll = focusLine
	}
	if focusLine+2 >= form.scroll+bodyHeight {
		form.scroll = focusLine + 3 - bodyHeight
	}
	form.scroll = max(0, min(maxScroll, form.scroll))
	end := min(len(lines), form.scroll+bodyHeight)
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines[form.scroll:end], "\n")
	}
	position := ""
	if len(lines) > bodyHeight {
		position = fmt.Sprintf("  · %d-%d/%d", form.scroll+1, end, len(lines))
	}
	header := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render("Global agents and orchestration"),
		globalAgentsTabs(form.tab) + lipgloss.NewStyle().Foreground(colorMuted).Render(position),
	}
	header = append(header, contextLines...)
	if form.activeRuns > 0 {
		header = append(header,
			lipgloss.NewStyle().Foreground(statusColor("scheduled")).Render(
				truncate(fmt.Sprintf("%d active run(s) on this board retain their pinned agent snapshot.", form.activeRuns), innerWidth),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Saved routes apply to later claims; current workers are not reassigned.", innerWidth),
			),
		)
	}
	footer := []string{}
	if form.err != nil {
		footer = append(footer, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(
			truncate(form.err.Error(), innerWidth),
		))
	} else if form.notice != "" {
		footer = append(footer, lipgloss.NewStyle().Foreground(statusColor("done")).Render(
			truncate(form.notice, innerWidth),
		))
	}
	if busy := globalAgentsBusyLabel(form); busy != "" {
		footer = append(footer, lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate(busy, innerWidth),
		))
	}
	firstHelp := "↑/↓ field · ←/→ selection · Space toggle · Tab/Shift+Tab field"
	secondHelp := "Ctrl+←/→ section · PgUp/PgDn scroll · Ctrl+S save · Esc cancel"
	switch form.tab {
	case globalAgentsRegistry:
		secondHelp = "Ctrl+N add · Ctrl+X remove · Ctrl+S save · Esc cancel"
	case globalAgentsSupervisor:
		secondHelp = "Ctrl+R save + start · Ctrl+T stop · Ctrl+S save/reconcile · Esc cancel"
	case globalAgentsPresets:
		secondHelp = "Ctrl+P detect + apply to draft · Ctrl+S save · Esc cancel"
	}
	if innerWidth < 64 {
		firstHelp = "↑/↓ field · ←/→ select · PgUp/PgDn scroll"
	}
	footer = append(footer,
		lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(firstHelp, innerWidth)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(secondHelp, innerWidth)),
	)
	content := strings.Join(header, "\n") + "\n\n" + body + "\n" + strings.Join(footer, "\n")
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func (m *Model) renderGlobalAgentsLoading(width, height int) string {
	panelWidth := min(64, max(8, width-2))
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render("Global agents") +
			"\n\n" + lipgloss.NewStyle().Foreground(colorMuted).Render("Loading registry and Supervisor status…"),
	)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}
