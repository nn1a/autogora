package terminalui

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/supervisor"
)

func globalAgentsTestConfig() agentconfig.Config {
	return agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor: agentconfig.Supervisor{
			AutoStart: true, MaxWorkers: 2, AllowWrites: true,
		},
		Defaults: agentconfig.Defaults{
			WorkerAgents: []string{"codex"}, PlannerAgents: []string{"codex"},
			CoordinatorAgents: []string{"claude"}, JudgeAgents: []string{"claude"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: "codex", Runtime: model.RuntimeCodex, Command: "/tools/codex",
				Model: "gpt-custom", Enabled: true, MaxConcurrent: 2,
				Roles: []agentconfig.Role{
					agentconfig.RoleWorker, agentconfig.RolePlanner,
					agentconfig.RoleCoordinator, agentconfig.RoleJudge,
				},
				Fallbacks: []string{"claude"},
			},
			{
				ID: "claude", Runtime: model.RuntimeClaude, Command: "/tools/claude",
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{
					agentconfig.RoleWorker, agentconfig.RolePlanner,
					agentconfig.RoleCoordinator, agentconfig.RoleJudge,
				},
			},
		},
	}
}

func globalAgentsTestContext() GlobalAgentsContext {
	return GlobalAgentsContext{
		Path: "/tmp/autogora-config.json", Exists: true,
		Revision: "sha256:test-revision-1",
		Config:   globalAgentsTestConfig(), Presets: agentconfig.BuiltinPresets(),
		Supervisor: supervisor.Status{
			Running: true, Desired: true, MaxWorkers: 2, AllowWrites: true,
		},
		ActiveRuns: 3,
	}
}

func TestGlobalAgentsFormBuildsCompleteValidatedConfiguration(t *testing.T) {
	form := newGlobalAgentsForm(globalAgentsTestContext())
	form.setInput(globalAgentID, "primary")
	form.writeAgentInput(globalAgentID, "primary")
	form.setInput(globalAgentModel, "")
	form.writeAgentInput(globalAgentModel, "")
	form.setInput(globalAgentProvider, "openai")
	form.writeAgentInput(globalAgentProvider, "openai")
	form.setInput(globalAgentMaxConcurrent, "4")
	form.writeAgentInput(globalAgentMaxConcurrent, "4")
	form.setInput(globalAgentRoles, "worker, planner, coordinator, judge")
	form.writeAgentInput(globalAgentRoles, form.inputs[globalAgentRoles].Value())
	form.setInput(globalSupervisorMaxWorkers, "5")

	config, err := form.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	primary, found := config.Find("primary")
	if !found || primary.Model != "" || primary.Provider != "openai" ||
		primary.MaxConcurrent != 4 || primary.Command != "/tools/codex" {
		t.Fatalf("edited agent was not built correctly: %#v", primary)
	}
	if !reflect.DeepEqual(config.Defaults.WorkerAgents, []string{"primary"}) ||
		!reflect.DeepEqual(config.Defaults.PlannerAgents, []string{"primary"}) ||
		config.Supervisor.MaxWorkers != 5 {
		t.Fatalf("renamed defaults or Supervisor fields were lost: %#v", config)
	}
	if !reflect.DeepEqual(primary.Fallbacks, []string{"claude"}) {
		t.Fatalf("fallback was lost: %#v", primary.Fallbacks)
	}

	form.agentIndex = 1
	form.loadAgentInputs()
	form.removeAgent()
	config, err = form.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	primary, _ = config.Find("primary")
	if len(primary.Fallbacks) != 0 ||
		len(config.Defaults.CoordinatorAgents) != 0 ||
		len(config.Defaults.JudgeAgents) != 0 {
		t.Fatalf("removed agent references remained: %#v", config)
	}
}

func TestGlobalAgentsRenameRemapsStableReferencesOnlyAtBuild(t *testing.T) {
	allRoles := []agentconfig.Role{
		agentconfig.RoleWorker, agentconfig.RolePlanner,
		agentconfig.RoleCoordinator, agentconfig.RoleJudge,
	}
	config := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 1},
		Defaults: agentconfig.Defaults{
			WorkerAgents: []string{"a", "ab"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: "a", Runtime: model.RuntimeCodex, Command: "codex",
				Enabled: true, MaxConcurrent: 1, Roles: allRoles,
			},
			{
				ID: "ab", Runtime: model.RuntimeClaude, Command: "claude",
				Enabled: true, MaxConcurrent: 1, Roles: allRoles,
				Fallbacks: []string{"a"},
			},
		},
	}
	form := newGlobalAgentsForm(GlobalAgentsContext{
		Revision: "sha256:rename", Config: config,
	})
	form.setInput(globalAgentID, "ab")
	form.writeAgentInput(globalAgentID, "ab")
	if got := form.inputs[globalDefaultWorkers].Value(); got != "a, ab" {
		t.Fatalf("intermediate duplicate rewrote references: %q", got)
	}
	form.setInput(globalAgentID, "ab2")
	form.writeAgentInput(globalAgentID, "ab2")
	if got := form.inputs[globalDefaultWorkers].Value(); got != "a, ab" {
		t.Fatalf("typing final ID rewrote references before build: %q", got)
	}
	built, err := form.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built.Defaults.WorkerAgents, []string{"ab2", "ab"}) {
		t.Fatalf("stable one-pass default remap = %#v", built.Defaults.WorkerAgents)
	}
	unchangedAB, found := built.Find("ab")
	if !found || !reflect.DeepEqual(unchangedAB.Fallbacks, []string{"ab2"}) {
		t.Fatalf("stable fallback remap = %#v", unchangedAB)
	}

	remove := newGlobalAgentsForm(GlobalAgentsContext{
		Revision: "sha256:remove", Config: config,
	})
	remove.setInput(globalAgentID, "")
	remove.writeAgentInput(globalAgentID, "")
	remove.removeAgent()
	built, err = remove.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	remaining, found := built.Find("ab")
	if !found || len(remaining.Fallbacks) != 0 ||
		!reflect.DeepEqual(built.Defaults.WorkerAgents, []string{"ab"}) {
		t.Fatalf("blank-ID removal did not clean stable references: %#v", built)
	}
}

func TestGlobalAgentsRemoveNewDraftCleansDraftReferences(t *testing.T) {
	form := newGlobalAgentsForm(globalAgentsTestContext())
	form.addAgent()
	newID := form.agents[form.agentIndex].agent.ID
	form.setInput(globalDefaultWorkers, "codex, "+newID)
	form.agents[0].fallbacks = "claude, " + newID
	form.removeAgent()
	built, err := form.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built.Defaults.WorkerAgents, []string{"codex"}) {
		t.Fatalf("removed new draft remained in defaults: %#v", built.Defaults)
	}
	codex, _ := built.Find("codex")
	if !reflect.DeepEqual(codex.Fallbacks, []string{"claude"}) {
		t.Fatalf("removed new draft remained in fallbacks: %#v", codex.Fallbacks)
	}
}

func TestGlobalAgentsPresetMergeAndExplicitReplaceUseDetections(t *testing.T) {
	current := agentconfig.Config{
		SchemaVersion: agentconfig.SchemaVersion,
		Supervisor:    agentconfig.Supervisor{MaxWorkers: 7, AllowWrites: true},
		Defaults: agentconfig.Defaults{
			WorkerAgents: []string{"custom"},
		},
		Agents: []agentconfig.Agent{
			{
				ID: "custom", Runtime: model.RuntimeGemini, Command: "/custom/gemini",
				Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{
					agentconfig.RoleWorker, agentconfig.RolePlanner,
					agentconfig.RoleCoordinator, agentconfig.RoleJudge,
				},
			},
			{
				ID: "codex", Runtime: model.RuntimeCodex, Command: "/custom/codex",
				Model: "pinned", Enabled: false, MaxConcurrent: 4,
				Roles: []agentconfig.Role{
					agentconfig.RoleWorker, agentconfig.RolePlanner,
					agentconfig.RoleCoordinator, agentconfig.RoleJudge,
				},
			},
		},
	}
	detections := []agentconfig.Detection{
		{ID: "codex", Runtime: model.RuntimeCodex, Executable: "/detected/codex", State: "installed"},
		{ID: "claude", Runtime: model.RuntimeClaude, Executable: "/detected/claude", State: "installed"},
	}
	context := GlobalAgentsContext{Config: current, Presets: agentconfig.BuiltinPresets()}
	form := newGlobalAgentsForm(context)
	for index, preset := range form.presets {
		if preset.ID == "codex-claude" {
			form.presetIndex = index
		}
	}
	if err := form.applyPreset(detections); err != nil {
		t.Fatal(err)
	}
	merged, err := form.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	codex, _ := merged.Find("codex")
	claude, found := merged.Find("claude")
	if codex.Command != "/custom/codex" || codex.Model != "pinned" || codex.Enabled ||
		!found || claude.Command != "/detected/claude" || !claude.Enabled {
		t.Fatalf("merge overwrote existing agent or missed detected agent: %#v", merged.Agents)
	}
	if !reflect.DeepEqual(merged.Defaults.WorkerAgents, []string{"custom"}) ||
		!reflect.DeepEqual(merged.Defaults.PlannerAgents, []string{"codex", "claude"}) {
		t.Fatalf("merge did not preserve/fill preferred lists: %#v", merged.Defaults)
	}

	replacement := newGlobalAgentsForm(context)
	for index, preset := range replacement.presets {
		if preset.ID == "codex-claude" {
			replacement.presetIndex = index
		}
	}
	replacement.replace = true
	if err := replacement.applyPreset(detections); err != nil {
		t.Fatal(err)
	}
	replaced, err := replacement.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	codex, _ = replaced.Find("codex")
	if codex.Command != "/detected/codex" || codex.Model != "" || !codex.Enabled ||
		replaced.Supervisor.MaxWorkers != 7 || !replaced.Supervisor.AllowWrites {
		t.Fatalf("replace did not update preset-owned fields safely: %#v", replaced)
	}
	if !reflect.DeepEqual(replaced.Defaults.WorkerAgents, []string{"codex", "claude"}) ||
		!reflect.DeepEqual(replaced.Defaults.CoordinatorAgents, []string{"claude", "codex"}) {
		t.Fatalf("replace did not update preferred role lists: %#v", replaced.Defaults)
	}

	missing := newGlobalAgentsForm(GlobalAgentsContext{
		Config: agentconfig.Default(), Presets: agentconfig.BuiltinPresets(),
	})
	for index, preset := range missing.presets {
		if preset.ID == "codex" {
			missing.presetIndex = index
		}
	}
	if err := missing.applyPreset([]agentconfig.Detection{{
		ID: "codex", Runtime: model.RuntimeCodex, State: "missing",
	}}); err != nil {
		t.Fatal(err)
	}
	missingConfig, err := missing.buildConfig()
	if err != nil {
		t.Fatal(err)
	}
	missingCodex, found := missingConfig.Find("codex")
	if !found || missingCodex.Enabled {
		t.Fatalf("preset enabled a CLI that detection marked missing: %#v", missingCodex)
	}
}

type fakeGlobalAgentsBackend struct {
	context      GlobalAgentsContext
	detections   []agentconfig.Detection
	loadCalls    int
	detectCalls  int
	saveCalls    int
	startCalls   int
	stopCalls    int
	lastConfig   agentconfig.Config
	lastExpected agentconfig.Revision
}

func (f *fakeGlobalAgentsBackend) LoadGlobalAgents(context.Context) (GlobalAgentsContext, error) {
	f.loadCalls++
	return f.context, nil
}

func (f *fakeGlobalAgentsBackend) DetectGlobalAgents(
	_ context.Context,
	config agentconfig.Config,
) ([]agentconfig.Detection, error) {
	f.detectCalls++
	f.lastConfig = config
	return append([]agentconfig.Detection{}, f.detections...), nil
}

func (f *fakeGlobalAgentsBackend) SaveGlobalAgents(
	_ context.Context,
	expected agentconfig.Revision,
	config agentconfig.Config,
) (GlobalAgentsContext, error) {
	f.saveCalls++
	f.lastExpected = expected
	if expected != f.context.Revision {
		return GlobalAgentsContext{}, &agentconfig.RevisionConflictError{
			Expected: expected, Actual: f.context.Revision,
		}
	}
	f.lastConfig = config
	f.context.Config, f.context.Exists = config, true
	f.context.Revision = agentconfig.Revision(string(f.context.Revision) + "-saved")
	return f.context, nil
}

func (f *fakeGlobalAgentsBackend) StartSupervisor(
	_ context.Context,
	expected agentconfig.Revision,
	config agentconfig.Config,
) (GlobalAgentsContext, error) {
	f.startCalls++
	f.lastExpected = expected
	if expected != f.context.Revision {
		return GlobalAgentsContext{}, &agentconfig.RevisionConflictError{
			Expected: expected, Actual: f.context.Revision,
		}
	}
	f.lastConfig = config
	f.context.Config, f.context.Exists = config, true
	f.context.Revision = agentconfig.Revision(string(f.context.Revision) + "-started")
	f.context.Supervisor = supervisor.Status{
		Running: true, Desired: true, MaxWorkers: config.Supervisor.MaxWorkers,
		AllowWrites: config.Supervisor.AllowWrites,
	}
	return f.context, nil
}

func (f *fakeGlobalAgentsBackend) StopSupervisor(context.Context) (GlobalAgentsContext, error) {
	f.stopCalls++
	f.context.Supervisor.Running, f.context.Supervisor.Desired = false, false
	return f.context, nil
}

func (f *fakeGlobalAgentsBackend) SupervisorStatus() supervisor.Status {
	return f.context.Supervisor
}

func TestGlobalAgentsOverlayDetectsPresetSavesAndControlsSupervisor(t *testing.T) {
	globalContext := globalAgentsTestContext()
	backend := &fakeGlobalAgentsBackend{
		context: globalContext,
		detections: []agentconfig.Detection{
			{ID: "codex", Runtime: model.RuntimeCodex, Executable: "/tools/codex", State: "installed"},
			{ID: "claude", Runtime: model.RuntimeClaude, Executable: "/tools/claude", State: "installed"},
		},
	}
	m := NewModelWithGlobalAgents(context.Background(), &fakeBackend{}, backend, "default")
	_, load := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if load == nil || !m.globalLoading {
		t.Fatal("A did not begin loading global settings")
	}
	m.Update(load())
	if m.globalAgents == nil || m.globalLoading || backend.loadCalls != 1 {
		t.Fatalf("global settings did not open: form=%#v loads=%d", m.globalAgents, backend.loadCalls)
	}
	view := m.View()
	for _, text := range []string{
		"Global agents and orchestration", "Agents", "Defaults", "Supervisor", "Presets",
		"empty Model or Provider", "Ctrl+N add", "3 active run(s)", "←/→ select",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("global settings omitted %q:\n%s", text, view)
		}
	}

	m.globalAgents.tab = globalAgentsPresets
	m.globalAgents.focus = globalPresetSelection
	for index, preset := range m.globalAgents.presets {
		if preset.ID == "codex-claude" {
			m.globalAgents.presetIndex = index
		}
	}
	for _, text := range []string{
		"only checks PATH and runs --version", "never sends a prompt", "Ctrl+P detect + apply",
	} {
		if view := m.View(); !strings.Contains(view, text) {
			t.Fatalf("preset tab omitted safety guidance %q:\n%s", text, view)
		}
	}
	_, detect := m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if detect == nil || !m.globalAgents.detecting {
		t.Fatal("Ctrl+P did not begin safe preset detection")
	}
	m.Update(detect())
	if backend.detectCalls != 1 || m.globalAgents.detecting ||
		!strings.Contains(m.globalAgents.notice, "press Ctrl+S") {
		t.Fatalf("preset detection was not applied to draft: %#v", m.globalAgents)
	}
	_, save := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if save == nil {
		t.Fatal("Ctrl+S did not save global settings")
	}
	m.Update(save())
	if backend.saveCalls != 1 || m.globalAgents != nil ||
		!strings.Contains(m.notice, "active run") {
		t.Fatalf("global save did not complete safely: saves=%d notice=%q", backend.saveCalls, m.notice)
	}

	_, load = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m.Update(load())
	m.globalAgents.tab = globalAgentsSupervisor
	m.globalAgents.focus = globalSupervisorAutoStart
	m.globalAgents.setInput(globalSupervisorMaxWorkers, "6")
	for _, text := range []string{
		"deterministic host services", "Current process:", "running true", "desired true",
		"preserves the current desired state", "AutoStart is the startup policy",
		"Ctrl+R save + start", "Ctrl+T stop",
	} {
		if view := m.View(); !strings.Contains(view, text) {
			t.Fatalf("Supervisor tab omitted guidance %q:\n%s", text, view)
		}
	}
	_, start := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	if start == nil {
		t.Fatal("Ctrl+R did not start Supervisor")
	}
	m.Update(start())
	if backend.startCalls != 1 || !m.globalAgents.status.Desired ||
		backend.lastConfig.Supervisor.MaxWorkers != 6 {
		t.Fatalf("Supervisor start ignored draft: calls=%d status=%#v config=%#v",
			backend.startCalls, m.globalAgents.status, backend.lastConfig.Supervisor)
	}
	_, stop := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if stop == nil {
		t.Fatal("Ctrl+T did not stop Supervisor")
	}
	m.Update(stop())
	if backend.stopCalls != 1 || m.globalAgents.status.Desired {
		t.Fatalf("Supervisor did not stop: calls=%d status=%#v", backend.stopCalls, m.globalAgents.status)
	}
}

func TestGlobalAgentsOverlayRejectsStaleRevisionAndRequiresReload(t *testing.T) {
	backend := &fakeGlobalAgentsBackend{context: globalAgentsTestContext()}
	m := NewModelWithGlobalAgents(
		context.Background(), &fakeBackend{}, backend, "default",
	)
	_, load := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m.Update(load())
	openedRevision := m.globalAgents.revision
	backend.context.Revision = "sha256:changed-elsewhere"
	backend.context.Config.Supervisor.MaxWorkers = 11
	_, save := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if save == nil {
		t.Fatal("stale form did not attempt CAS save")
	}
	m.Update(save())
	if m.globalAgents == nil || m.globalAgents.revision != openedRevision ||
		m.globalAgents.err == nil ||
		!strings.Contains(m.globalAgents.err.Error(), "Cancel and reopen") {
		t.Fatalf("stale form did not retain its snapshot with reload guidance: %#v", m.globalAgents)
	}
	if backend.context.Config.Supervisor.MaxWorkers != 11 {
		t.Fatal("stale TUI save overwrote another process")
	}
}

func TestGlobalAgentsOverlayCompactMatrixKeepsNavigationHintsVisible(t *testing.T) {
	actions := map[globalAgentsTab]string{
		globalAgentsRegistry:   "Ctrl+N/X",
		globalAgentsDefaults:   "Edit prefer",
		globalAgentsSupervisor: "Ctrl+R/T",
		globalAgentsPresets:    "Ctrl+P",
	}
	for _, width := range []int{20, 24, 29, 33, 34, 40, 49} {
		for _, height := range []int{6, 7, 9, 13} {
			for tab, action := range actions {
				t.Run(fmt.Sprintf("%dx%d/%d", width, height, tab), func(t *testing.T) {
					m := NewModel(context.Background(), &fakeBackend{}, "default")
					m.width, m.height = width, height
					m.globalAgents = newGlobalAgentsForm(globalAgentsTestContext())
					m.globalAgents.tab = tab
					m.globalAgents.focus = m.globalAgents.fields()[0]
					view := m.View()
					for _, text := range []string{"Ctrl+←/→", "←/→ select", action} {
						if !strings.Contains(view, text) {
							t.Fatalf("compact global settings omitted %q:\n%s", text, view)
						}
					}
					for lineNumber, line := range strings.Split(view, "\n") {
						if lineWidth := lipgloss.Width(line); lineWidth > m.width {
							t.Fatalf("compact view line %d is %d cells wide, terminal is %d:\n%s",
								lineNumber+1, lineWidth, m.width, view)
						}
					}
				})
			}
		}
	}
}

func TestGlobalAgentsCompactInputKeepsCursorViewportAndHelp(t *testing.T) {
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.width, m.height = 40, 9
	m.globalAgents = newGlobalAgentsForm(globalAgentsTestContext())
	m.globalAgents.focus = globalAgentCommand
	m.globalAgents.setInput(globalAgentCommand, "/a/very/long/executable/path/END")
	input := m.globalAgents.inputs[globalAgentCommand]
	input.Focus()
	m.globalAgents.inputs[globalAgentCommand] = input
	if view := m.View(); !strings.Contains(view, "END") {
		t.Fatalf("compact input hid the cursor-end viewport:\n%s", view)
	}
	m.height = 20
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !m.globalAgents.help {
		t.Fatal("? did not open global agents help")
	}
	if view := m.View(); !strings.Contains(view, "Global agents help") ||
		!strings.Contains(view, "Ctrl+P") {
		t.Fatalf("global agents help omitted overlay guidance:\n%s", view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.globalAgents == nil || m.globalAgents.help {
		t.Fatal("Esc did not close help while retaining the settings overlay")
	}
}
