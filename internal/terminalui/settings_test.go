package terminalui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/taskservice"
)

func settingsTestContext() taskservice.BoardContext {
	defaultProfile, finalizerProfile := "local", "inherited"
	return taskservice.BoardContext{
		Metadata: boards.Metadata{Orchestration: boards.OrchestrationSettings{
			AutoDecompose: true, AutoDecomposePerTick: 3, AutoPromoteChildren: true,
			PlannerRuntime: model.RuntimeCodex, PlannerModel: "planner-model",
			DefaultProfile: &defaultProfile, FinalizerProfile: &finalizerProfile,
			Profiles: []boards.Profile{{
				Name: "local", Runtime: model.RuntimeCline, Model: "cline-model",
				Description: "board-only route", MaxConcurrent: 2,
			}},
			Autopilot: boards.AutopilotSettings{
				Enabled: true, AutoPlan: true, AutoExecute: true,
				Coordination: boards.CoordinationSettings{
					Mode: boards.CoordinationModeAssist, IdleSeconds: 300,
					MaxCallsPerHour: 4, MaxActionsPerIncident: 3,
				},
				Publication: boards.PublicationSettings{
					Mode: boards.PublicationModeManual, TargetBranch: "main",
					Remote: "origin", RequireApproval: true,
				},
			},
		}},
		Profiles: []orchestration.ProfileRoute{
			{Name: "inherited", Runtime: model.RuntimeClaude},
			{Name: "local", Runtime: model.RuntimeCline, Model: "cline-model"},
		},
		InheritedProfiles: []orchestration.ProfileRoute{
			{Name: "inherited", Runtime: model.RuntimeClaude},
		},
		ActiveRuns: 2,
	}
}

func TestBoardSettingsEditsOnlyBoardProfiles(t *testing.T) {
	form := newBoardSettingsForm(settingsTestContext())
	if len(form.profiles) != 1 || form.profiles[0].Name != "local" {
		t.Fatalf("effective profiles leaked into editable profiles: %#v", form.profiles)
	}
	choices := form.profileChoices(form.draft.DefaultProfile)
	if strings.Join(choices, ",") != ",inherited,local" {
		t.Fatalf("default profile choices = %v", choices)
	}
	update, err := form.buildUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if update.Profiles == nil || len(*update.Profiles) != 1 || (*update.Profiles)[0].Name != "local" {
		t.Fatalf("settings save would persist merged effective profiles: %#v", update.Profiles)
	}
}

func TestBoardSettingsBuildsCompleteAutopilotPolicy(t *testing.T) {
	form := newBoardSettingsForm(settingsTestContext())
	form.draft.AutoDecompose = false
	form.draft.AutoPromoteChildren = false
	form.draft.PlannerRuntime = model.RuntimeGemini
	form.setInput(settingPlannerModel, "gemini-pro")
	form.setInput(settingPlannerProvider, "google")
	form.draft.Autopilot.Enabled = true
	form.draft.Autopilot.AutoPlan = false
	form.draft.Autopilot.AutoExecute = true
	form.draft.Autopilot.WorkspaceWrites = true
	form.draft.Autopilot.Coordination.Mode = boards.CoordinationModeAuto
	form.setInput(settingCoordinatorProfile, "coordinator")
	form.setInput(settingCoordinationIdleSeconds, "90")
	form.setInput(settingCoordinatorCallsPerHour, "8")
	form.setInput(settingCoordinatorActionsPerIncident, "5")
	form.draft.Autopilot.Publication.Mode = boards.PublicationModePullRequest
	form.setInput(settingPublicationTarget, "develop")
	form.setInput(settingPublicationRemote, "upstream")
	form.draft.Autopilot.Publication.RequireApproval = false

	update, err := form.buildUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if *update.AutoDecompose || *update.AutoPromoteChildren ||
		*update.PlannerRuntime != model.RuntimeGemini ||
		*update.PlannerModel != "gemini-pro" || *update.PlannerProvider != "google" {
		t.Fatalf("board policy update missing fields: %#v", update)
	}
	autopilot := update.Autopilot
	if autopilot == nil || *autopilot.AutoPlan || !*autopilot.AutoExecute ||
		!*autopilot.WorkspaceWrites ||
		*autopilot.Coordination.Mode != boards.CoordinationModeAuto ||
		autopilot.Coordination.Profile.Value == nil ||
		*autopilot.Coordination.Profile.Value != "coordinator" ||
		*autopilot.Coordination.MaxCallsPerHour != 8 ||
		*autopilot.Publication.Mode != boards.PublicationModePullRequest ||
		*autopilot.Publication.TargetBranch != "develop" ||
		*autopilot.Publication.Remote != "upstream" ||
		*autopilot.Publication.RequireApproval {
		t.Fatalf("autopilot update missing fields: %#v", autopilot)
	}
}

func TestBoardSettingsValidatesProfilesAndBudgets(t *testing.T) {
	form := newBoardSettingsForm(settingsTestContext())
	form.profiles = append(form.profiles, settingsProfile{
		Profile:       boards.Profile{Name: "local", Runtime: model.RuntimeClaude},
		maxConcurrent: "1", priority: "0",
	})
	if err := form.validate(); err == nil || !strings.Contains(err.Error(), "duplicate profile") {
		t.Fatalf("duplicate profile validation = %v", err)
	}
	form.profiles = form.profiles[:1]
	form.profiles[0].fallbacks = "local"
	if err := form.validate(); err == nil || !strings.Contains(err.Error(), "fall back to itself") {
		t.Fatalf("self fallback validation = %v", err)
	}
	form.profiles[0].fallbacks = ""
	form.setInput(settingCoordinatorCallsPerHour, "101")
	if err := form.validate(); err == nil || !strings.Contains(err.Error(), "between 1 and 100") {
		t.Fatalf("coordinator budget validation = %v", err)
	}
}

func TestBoardProfileRenameKeepsReferencesCoherent(t *testing.T) {
	context := settingsTestContext()
	coordinator := "local"
	context.Metadata.Orchestration.Autopilot.Coordination.Profile = &coordinator
	context.Metadata.Orchestration.Profiles = append(context.Metadata.Orchestration.Profiles,
		boards.Profile{Name: "review", Runtime: model.RuntimeClaude, Fallbacks: []string{"local"}},
	)
	form := newBoardSettingsForm(context)
	form.writeProfileInput(settingProfileName, "")
	form.writeProfileInput(settingProfileName, "primary")
	if pointer(form.draft.DefaultProfile, "") != "primary" ||
		pointer(form.draft.Autopilot.Coordination.Profile, "") != "primary" ||
		form.profiles[1].fallbacks != "primary" {
		t.Fatalf("rename left stale references: default=%v coordinator=%v profiles=%#v",
			form.draft.DefaultProfile, form.draft.Autopilot.Coordination.Profile, form.profiles)
	}
}

func TestRemovingBoardOnlyProfileClearsOnlyOrphanedReferences(t *testing.T) {
	context := settingsTestContext()
	coordinator := "local"
	context.Metadata.Orchestration.Autopilot.Coordination.Profile = &coordinator
	form := newBoardSettingsForm(context)
	form.removeProfile()
	if form.draft.DefaultProfile != nil || form.draft.Autopilot.Coordination.Profile != nil {
		t.Fatalf("removed board-only profile left references: default=%v coordinator=%v",
			form.draft.DefaultProfile, form.draft.Autopilot.Coordination.Profile)
	}

	context = settingsTestContext()
	context.InheritedProfiles = append(context.InheritedProfiles,
		orchestration.ProfileRoute{Name: "local", Runtime: model.RuntimeCodex},
	)
	form = newBoardSettingsForm(context)
	form.removeProfile()
	if pointer(form.draft.DefaultProfile, "") != "local" {
		t.Fatalf("removing an override cleared the inherited profile reference: %v", form.draft.DefaultProfile)
	}
}

func TestBoardSettingsOverlayScrollsWithinSmallTerminal(t *testing.T) {
	model := NewModel(nil, &fakeBackend{}, "default")
	model.settings = newBoardSettingsForm(settingsTestContext())
	model.settings.tab = settingsAutopilot
	model.settings.focus = settingPublicationApproval
	model.settings.syncFocus()
	view := model.renderBoardSettings(54, 18)
	if lipgloss.Width(view) > 54 || lipgloss.Height(view) > 18 {
		t.Fatalf("settings overlay exceeds terminal: %dx%d\n%s", lipgloss.Width(view), lipgloss.Height(view), view)
	}
	if !strings.Contains(view, "Ctrl+S save") || model.settings.scroll == 0 {
		t.Fatalf("small settings overlay lacks controls or focus scrolling:\n%s", view)
	}
}

func TestBoardSettingsCompactOverlayNeverExceedsAvailableArea(t *testing.T) {
	tuiModel := NewModel(nil, &fakeBackend{}, "default")
	tuiModel.settings = newBoardSettingsForm(settingsTestContext())
	for _, size := range []struct{ width, height int }{{10, 3}, {24, 8}, {33, 13}} {
		view := tuiModel.renderBoardSettings(size.width, size.height)
		if lipgloss.Width(view) > size.width || lipgloss.Height(view) > size.height {
			t.Fatalf("compact overlay %dx%d rendered %dx%d:\n%s",
				size.width, size.height, lipgloss.Width(view), lipgloss.Height(view), view)
		}
	}
}

func TestModelSavesBoardSettingsAndReloadsContext(t *testing.T) {
	context := settingsTestContext()
	backend := &fakeBackend{boardContext: context}
	model := NewModel(nil, backend, "default")
	model.Update(boardContextMsg{context: context})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'O'}})
	if model.settings == nil || !strings.Contains(model.View(), "Board orchestration settings") {
		t.Fatal("board settings did not open")
	}
	model.settings.draft.Autopilot.AutoExecute = false
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if command == nil || !model.settings.saving {
		t.Fatal("board settings did not start saving")
	}
	model.Update(command())
	if model.settings != nil || model.boardContext == nil || len(backend.boardSettingsUpdates) != 1 {
		t.Fatalf("settings save did not reload context: form=%v updates=%d", model.settings != nil, len(backend.boardSettingsUpdates))
	}
	if backend.boardSettingsUpdates[0].Profiles == nil ||
		len(*backend.boardSettingsUpdates[0].Profiles) != 1 {
		t.Fatalf("model persisted effective profiles: %#v", backend.boardSettingsUpdates[0].Profiles)
	}
	if !strings.Contains(model.notice, "2 active run(s) keep pinned settings") {
		t.Fatalf("active-run safety notice = %q", model.notice)
	}
}

func TestModelKeepsSettingsOpenOnCASConflict(t *testing.T) {
	context := settingsTestContext()
	backend := &fakeBackend{
		boardContext: context, boardSettingsErr: boards.ErrBoardSettingsConflict,
	}
	model := NewModel(nil, backend, "default")
	model.Update(boardContextMsg{context: context})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'O'}})
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model.Update(command())
	if model.settings == nil || model.settings.err == nil ||
		!strings.Contains(model.settings.err.Error(), "changed elsewhere") {
		t.Fatalf("CAS conflict was not shown in settings: %#v", model.settings)
	}
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if model.settings != nil {
		t.Fatal("settings cancel did not discard the stale draft")
	}
}

func TestSavedBoardContextRejectsOlderPollResult(t *testing.T) {
	initial := settingsTestContext()
	tuiModel := NewModel(nil, &fakeBackend{}, "default")
	tuiModel.Update(boardContextMsg{context: initial})
	tuiModel.settings = newBoardSettingsForm(initial)
	saved := settingsTestContext()
	saved.Metadata.Orchestration.PlannerRuntime = model.RuntimeGemini
	tuiModel.Update(boardSettingsSavedMsg{context: saved})
	stale := settingsTestContext()
	stale.Metadata.Orchestration.PlannerRuntime = model.RuntimeClaude
	tuiModel.Update(boardContextMsg{context: stale, generation: 0})
	if tuiModel.boardContext == nil ||
		tuiModel.boardContext.Metadata.Orchestration.PlannerRuntime != model.RuntimeGemini {
		t.Fatalf("older poll replaced saved settings: %#v", tuiModel.boardContext)
	}
}

func TestBoardSettingsCancelDoesNotCallBackend(t *testing.T) {
	context := settingsTestContext()
	backend := &fakeBackend{boardContext: context}
	model := NewModel(nil, backend, "default")
	model.Update(boardContextMsg{context: context})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'O'}})
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(backend.boardSettingsUpdates) != 0 || model.settings != nil {
		t.Fatalf("cancel changed settings: updates=%d form=%v", len(backend.boardSettingsUpdates), model.settings != nil)
	}
}

func TestBoardSettingsSaveErrorRemainsVisible(t *testing.T) {
	context := settingsTestContext()
	saveErr := errors.New("metadata lock unavailable")
	backend := &fakeBackend{boardContext: context, boardSettingsErr: saveErr}
	model := NewModel(nil, backend, "default")
	model.Update(boardContextMsg{context: context})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'O'}})
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	model.Update(command())
	if model.settings == nil || !errors.Is(model.settings.err, saveErr) ||
		!strings.Contains(model.View(), saveErr.Error()) {
		t.Fatalf("save error is not visible: %#v", model.settings)
	}
}
