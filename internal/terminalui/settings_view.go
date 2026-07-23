package terminalui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func boardSettingLabel(field boardSettingField) string {
	labels := map[boardSettingField]string{
		settingAutoDecompose:                 "Auto-decompose Triage tasks",
		settingAutoDecomposePerTick:          "Auto-decompose per tick",
		settingAutoPromoteChildren:           "Auto-promote ready children",
		settingPlannerRuntime:                "Planner runtime",
		settingPlannerModel:                  "Planner model",
		settingPlannerProvider:               "Planner provider",
		settingDefaultProfile:                "Default worker profile",
		settingFinalizerProfile:              "Finalizer profile",
		settingProfileSelection:              "Board profile",
		settingProfileName:                   "Profile name",
		settingProfileRuntime:                "Runtime",
		settingProfileModel:                  "Model",
		settingProfileProvider:               "Provider",
		settingProfileDescription:            "Description",
		settingProfileDisabled:               "Profile disabled",
		settingProfileMaxConcurrent:          "Max concurrent",
		settingProfilePriority:               "Priority",
		settingProfileFallbacks:              "Fallback profiles",
		settingAutopilotEnabled:              "Autopilot",
		settingAutoPlan:                      "Automatic planning",
		settingAutoExecute:                   "Automatic execution",
		settingWorkspaceWrites:               "Workspace writes",
		settingCoordinationMode:              "Coordinator mode",
		settingCoordinatorProfile:            "Coordinator profile",
		settingCoordinationIdleSeconds:       "Idle threshold (seconds)",
		settingCoordinatorCallsPerHour:       "Coordinator calls per hour",
		settingCoordinatorActionsPerIncident: "Actions per incident",
		settingPublicationMode:               "Publication mode",
		settingPublicationTarget:             "Target branch",
		settingPublicationRemote:             "Git remote",
		settingPublicationApproval:           "Publication approval",
	}
	return labels[field]
}

func boardSettingHint(field boardSettingField) string {
	switch field {
	case settingAutoDecompose, settingAutoPromoteChildren, settingProfileDisabled,
		settingAutopilotEnabled, settingAutoPlan, settingAutoExecute,
		settingWorkspaceWrites, settingPublicationApproval:
		return "Space toggle"
	case settingPlannerRuntime, settingDefaultProfile, settingFinalizerProfile,
		settingProfileSelection, settingProfileRuntime, settingCoordinationMode,
		settingPublicationMode:
		return "←/→ select"
	default:
		return "type value"
	}
}

func (f *boardSettingsForm) renderField(field boardSettingField, width int) []string {
	focused := field == f.focus
	marker, color := "  ", colorMuted
	if focused {
		marker, color = "› ", colorFocus
	}
	label := marker + boardSettingLabel(field)
	hint := ""
	if focused {
		hint = " · " + boardSettingHint(field)
	}
	label = truncate(label+hint, width)
	labelLine := lipgloss.NewStyle().Bold(focused).Foreground(color).Render(label)
	value := ""
	if input, exists := f.inputs[field]; exists {
		input.Width = max(8, width-2)
		f.inputs[field] = input
		value = input.View()
	} else {
		value = "‹  " + f.selectValue(field) + "  ›"
		value = lipgloss.NewStyle().Foreground(colorText).Render(truncate(value, width-2))
	}
	return []string{labelLine, "  " + value, ""}
}

func (f *boardSettingsForm) bodyLines(width int) ([]string, int) {
	lines := []string{}
	focusLine := 0
	for _, field := range f.fields() {
		if field == f.focus {
			focusLine = len(lines)
		}
		lines = append(lines, f.renderField(field, width)...)
	}
	if f.tab == settingsProfiles && len(f.profiles) == 0 {
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("No board-only profiles. Press Ctrl+N to add one.", width),
			),
		)
	}
	return lines, focusLine
}

func settingsTabs(active boardSettingsTab) string {
	tabs := append([]string{}, boardSettingsTabLabels...)
	for index := range tabs {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if boardSettingsTab(index) == active {
			style = style.Foreground(colorFocus).Bold(true).Underline(true)
		}
		tabs[index] = style.Render(tabs[index])
	}
	return strings.Join(tabs, "   ")
}

func (m *Model) renderCompactBoardSettings(width, height int) string {
	form := m.settings
	panelWidth := max(8, width-2)
	innerWidth := max(4, panelWidth-4)
	field := form.renderField(form.focus, innerWidth)
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(
			truncate("Board settings · "+boardSettingsTabLabels[form.tab], innerWidth),
		),
	}
	if height >= 6 {
		lines = append(lines, field[:2]...)
	}
	if form.err != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(
			truncate(form.err.Error(), innerWidth),
		))
	} else if form.saving {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Saving…"))
	}
	if height >= 9 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			truncate("↑/↓ field · Ctrl+←/→ tab · Ctrl+S save · Esc cancel", innerWidth),
		))
	}
	innerHeight := max(1, height-2)
	if len(lines) > innerHeight {
		lines = lines[:innerHeight]
	}
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(0, 1).Render(
		strings.Join(lines, "\n"),
	)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func (m *Model) renderBoardSettings(width, height int) string {
	form := m.settings
	if form == nil {
		return ""
	}
	if height < 14 || width < 34 {
		return m.renderCompactBoardSettings(width, height)
	}
	panelWidth := min(96, width-2)
	innerWidth := max(12, panelWidth-6)
	fixedHeight := 13
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
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render("Board orchestration settings"),
		settingsTabs(form.tab) + lipgloss.NewStyle().Foreground(colorMuted).Render(position),
	}
	switch form.tab {
	case settingsProfiles:
		header = append(header,
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Only board.json profiles are edited here. Inherited global and task routes remain read-only.", innerWidth),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Ctrl+N adds · Ctrl+X removes from this draft", innerWidth),
			),
		)
	case settingsAutopilot:
		header = append(header,
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Supervisor, Dispatcher, and Publisher are deterministic host services; Coordinator uses the selected agent.", innerWidth),
			),
		)
	default:
		header = append(header,
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Planner specifies Triage work; worker and finalizer routes apply to subsequent claims.", innerWidth),
			),
		)
	}
	if form.activeRuns > 0 {
		header = append(header,
			lipgloss.NewStyle().Foreground(statusColor("scheduled")).Render(
				truncate(fmt.Sprintf("%d active run(s) keep their pinned route and policy snapshot.", form.activeRuns), innerWidth),
			),
			lipgloss.NewStyle().Foreground(colorMuted).Render(
				truncate("Saved values apply to later claims; active work is not interrupted.", innerWidth),
			),
		)
	}
	footer := []string{}
	if form.err != nil {
		footer = append(footer, lipgloss.NewStyle().Foreground(statusColor("blocked")).Render(
			truncate(form.err.Error(), innerWidth),
		))
	}
	if form.saving {
		footer = append(footer, lipgloss.NewStyle().Foreground(colorMuted).Render("Saving with version check…"))
	}
	firstHelp := "↑/↓ field · ←/→ selection · Space toggle · Tab/Shift+Tab field"
	secondHelp := "Ctrl+←/→ section · PgUp/PgDn scroll · Ctrl+S save · Esc cancel"
	if innerWidth < 64 {
		firstHelp = "↑/↓ field · ←/→ select · PgUp/PgDn scroll"
		secondHelp = "Ctrl+←/→ tab · Ctrl+S save · Esc cancel"
	}
	footer = append(footer,
		lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(firstHelp, innerWidth)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(secondHelp, innerWidth)),
	)
	content := strings.Join(header, "\n") + "\n\n" + body + "\n" + strings.Join(footer, "\n")
	panel := baseBorder.Copy().BorderForeground(colorFocus).Width(panelWidth-2).Padding(1, 2).Render(content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}
