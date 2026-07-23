package terminalui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

func TestTaskFormAppliesBoardProfileToAgentFields(t *testing.T) {
	profiles := []orchestration.ProfileRoute{{Name: "reviewer", Runtime: model.RuntimeGemini, Description: "Reviews changes"}}
	form := newTaskForm("product", profiles, model.TaskStatusTodo, nil)
	if form.profileIndex != 1 || form.inputs[fieldAssignee].Value() != "reviewer" || formRuntimes[form.runtimeIndex] != "gemini" {
		t.Fatalf("default profile was not applied: profile=%d assignee=%q runtime=%q", form.profileIndex, form.inputs[fieldAssignee].Value(), formRuntimes[form.runtimeIndex])
	}
	form.focus = fieldProfile
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if form.profileIndex != 0 {
		t.Fatal("profile selector did not retain its custom route option")
	}
	if assignee := form.inputs[fieldAssignee].Value(); assignee != "" {
		t.Fatalf("selecting Custom retained profile assignee %q", assignee)
	}
}

func TestTaskFormHonorsBoardDefaultProfile(t *testing.T) {
	defaultProfile := "reviewer"
	profiles := []orchestration.ProfileRoute{
		{Name: "implementer", Runtime: model.RuntimeCodex},
		{Name: "reviewer", Runtime: model.RuntimeGemini, Model: "gemini-review"},
	}
	form := newTaskForm("product", profiles, model.TaskStatusTodo, &defaultProfile)
	if form.profileIndex != 2 ||
		form.inputs[fieldAssignee].Value() != "reviewer" ||
		formRuntimes[form.runtimeIndex] != "gemini" {
		t.Fatalf(
			"board default was not applied: profile=%d assignee=%q runtime=%q",
			form.profileIndex,
			form.inputs[fieldAssignee].Value(),
			formRuntimes[form.runtimeIndex],
		)
	}

	triage := newTaskForm("product", profiles, model.TaskStatusTriage, &defaultProfile)
	triage.focus = fieldStatus
	triage.statusIndex = optionIndex(formStatuses, "triage")
	triage.cycleSelection(1)
	if triage.profileIndex != 2 || triage.inputs[fieldAssignee].Value() != "reviewer" {
		t.Fatalf("status transition ignored board default: %#v", triage.effectiveRoute())
	}
}

func TestTaskFormFallsBackWhenBoardDefaultIsMissingOrDisabled(t *testing.T) {
	profiles := []orchestration.ProfileRoute{
		{Name: "disabled-default", Runtime: model.RuntimeGemini, Disabled: true},
		{Name: "implementer", Runtime: model.RuntimeCodex},
		{Name: "reviewer", Runtime: model.RuntimeClaude},
	}
	for _, defaultName := range []string{"disabled-default", "missing"} {
		t.Run(defaultName, func(t *testing.T) {
			form := newTaskForm("product", profiles, model.TaskStatusReady, &defaultName)
			if form.profileIndex != 1 ||
				form.inputs[fieldAssignee].Value() != "implementer" ||
				formRuntimes[form.runtimeIndex] != "codex" {
				t.Fatalf(
					"invalid default did not fall back safely: profile=%d assignee=%q runtime=%q profiles=%#v",
					form.profileIndex,
					form.inputs[fieldAssignee].Value(),
					formRuntimes[form.runtimeIndex],
					form.profiles,
				)
			}
		})
	}
}

func TestScheduledFormRequiresAndPersistsFutureTime(t *testing.T) {
	form := newTaskForm("default", []orchestration.ProfileRoute{{Name: "worker", Runtime: model.RuntimeCodex}}, model.TaskStatusScheduled, nil)
	form.setInputValue(fieldTitle, "Run later")
	if err := form.validate(); err != nil {
		t.Fatalf("default future schedule was rejected: %v", err)
	}
	input := form.createInput()
	if input.ScheduledAt == nil {
		t.Fatal("scheduled create input omitted scheduledAt")
	}
	parsed, err := time.Parse(time.RFC3339, *input.ScheduledAt)
	if err != nil || !parsed.After(time.Now()) {
		t.Fatalf("invalid persisted schedule %q: %v", *input.ScheduledAt, err)
	}
	form.setInputValue(fieldScheduledAt, "")
	if err := form.validate(); err == nil || !strings.Contains(err.Error(), "future RFC3339") {
		t.Fatalf("missing schedule error = %v", err)
	}
}

func TestTaskFormBuildsCompleteCreateInput(t *testing.T) {
	form := newTaskForm("product", nil, model.TaskStatusReady, nil)
	form.setInputValue(fieldTitle, "Implement TUI forms")
	form.body.SetValue("Match the Web task fields.")
	form.setInputValue(fieldPriority, "8")
	form.setInputValue(fieldAssignee, "implementer")
	form.runtimeIndex = optionIndex(formRuntimes, "codex")
	form.setInputValue(fieldSkills, "go, tui, go")
	form.setInputValue(fieldTenant, "product")
	form.workspaceIndex = optionIndex(formWorkspaceKinds, "worktree")
	form.setInputValue(fieldWorkspace, "/workspace/repo")
	form.setInputValue(fieldBranch, "feature/tui")
	form.setInputValue(fieldMaxRuntime, "900")
	form.setInputValue(fieldMaxRetries, "4")
	form.goalMode = true
	form.setInputValue(fieldGoalMaxTurns, "12")

	if err := form.validate(); err != nil {
		t.Fatal(err)
	}
	input := form.createInput()
	if input.Title != "Implement TUI forms" || input.Body == "" || input.Status != model.TaskStatusReady || input.Priority != 8 || input.Runtime != model.RuntimeCodex {
		t.Fatalf("basic fields missing: %#v", input)
	}
	if input.Assignee == nil || *input.Assignee != "implementer" || input.Tenant == nil || *input.Tenant != "product" || input.Workspace == nil || *input.Workspace != "/workspace/repo" {
		t.Fatalf("optional fields missing: %#v", input)
	}
	if input.WorkspaceKind != model.WorkspaceWorktree || len(input.Skills) != 2 || !input.GoalMode || input.Branch == nil || *input.Branch != "feature/tui" {
		t.Fatalf("execution fields missing: %#v", input)
	}
	if input.MaxRuntimeSeconds == nil || *input.MaxRuntimeSeconds != 900 || input.MaxRetries != 4 || input.GoalMaxTurns != 12 {
		t.Fatalf("execution limits missing: %#v", input)
	}
}

func TestRunningTaskFormOnlyAllowsPriority(t *testing.T) {
	runID := "run"
	task := testTask("task", "Running work", model.TaskStatusRunning)
	task.CurrentRunID = &runID
	task.UpdatedAt = "2026-07-23T12:00:00.000Z"
	task.Priority = 3
	tenant := "product"
	task.Tenant = &tenant
	form := editTaskForm("default", nil, task, nil)
	for _, field := range form.fields() {
		wantLocked := field != fieldPriority
		if form.locked(field) != wantLocked {
			t.Fatalf("field %v locked = %v, want %v", field, form.locked(field), wantLocked)
		}
	}
	if form.focus != fieldPriority {
		t.Fatalf("running form focus = %v, want priority", form.focus)
	}
	form.moveStep(1)
	if form.focus != fieldPriority {
		t.Fatalf("section navigation did not skip fully locked sections: focus=%v", form.focus)
	}

	form.setInputValue(fieldPriority, "8")
	form.setInputValue(fieldTenant, "operations")
	form.setInputValue(fieldMaxRetries, "locked-invalid-value")
	if err := form.validate(); err != nil {
		t.Fatalf("locked execution fields affected validation: %v", err)
	}
	expectedUpdatedAt, priority := task.UpdatedAt, 8
	want := store.UpdateTaskInput{
		ExpectedUpdatedAt: &expectedUpdatedAt,
		Priority:          &priority,
	}
	if input := form.updateInput(); !reflect.DeepEqual(input, want) {
		t.Fatalf("running update input = %#v, want %#v", input, want)
	}

	view := (&Model{form: form}).renderTaskForm(120, 34)
	for _, text := range []string{
		"only Priority is editable",
		"Terminate the active run",
		"locked while Running",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("running form omitted guidance %q:\n%s", text, view)
		}
	}
}

func TestReadyFormRequiresRunnableAgentAfterProfileDetaches(t *testing.T) {
	form := newTaskForm(
		"default",
		[]orchestration.ProfileRoute{{Name: "worker", Runtime: model.RuntimeCodex}},
		model.TaskStatusReady,
		nil,
	)
	form.setInputValue(fieldTitle, "Runnable task")
	form.focus = fieldProfile
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyLeft})
	form.focus = fieldRuntime
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyRight})
	if form.profileIndex != 0 ||
		form.inputs[fieldAssignee].Value() != "" ||
		formRuntimes[form.runtimeIndex] != "claude" {
		t.Fatalf(
			"custom runtime retained a profile route: profile=%d assignee=%q runtime=%q",
			form.profileIndex,
			form.inputs[fieldAssignee].Value(),
			formRuntimes[form.runtimeIndex],
		)
	}
	if err := form.validate(); err == nil {
		t.Fatal("Ready task with a detached profile assignee should be rejected")
	}
	form.setInputValue(fieldAssignee, "custom-worker")
	if err := form.validate(); err != nil {
		t.Fatalf("Ready task with a custom agent route was rejected: %v", err)
	}
}

func TestTaskFormExplainsSelectionControls(t *testing.T) {
	form := newTaskForm("default", []orchestration.ProfileRoute{{Name: "reviewer", Runtime: model.RuntimeGemini}}, model.TaskStatusTriage, nil)
	form.focus = fieldProfile
	form.syncFocus()
	view := (&Model{form: form}).renderTaskForm(120, 32)
	for _, text := range []string{"Board profile", "↑/↓ select", "1/2", "↑/↓ select value", "Space toggle"} {
		if !strings.Contains(view, text) {
			t.Fatalf("form omitted selection help %q", text)
		}
	}
	form.Update(tea.KeyMsg{Type: tea.KeyRight})
	if view := form.renderField(fieldProfile, 40); !strings.Contains(view, "2/2") {
		t.Fatalf("profile selector omitted updated position:\n%s", view)
	}

	form.focus = fieldGoalMode
	form.syncFocus()
	if view := form.renderField(fieldGoalMode, 40); !strings.Contains(view, "Space toggle") {
		t.Fatal("goal mode omitted its toggle hint")
	}
}

func TestTaskFormRouteSummaryShowsModelProviderAndManualSemantics(t *testing.T) {
	profiles := []orchestration.ProfileRoute{{
		Name: "reviewer", Runtime: model.RuntimeCodex, Model: "gpt-review",
		Provider: "openai", Description: "Checks the final change",
	}}
	form := newTaskForm("default", profiles, model.TaskStatusTodo, nil)
	form.focus = fieldAssignee
	view := strings.Join(strings.Fields(form.renderRouteSummary(80)), " ")
	for _, text := range []string{
		"Effective route",
		"Profile: reviewer",
		"Assignee: reviewer",
		"Runtime: codex",
		"Model: gpt-review",
		"Provider: openai",
		"Profiles own Assignee and Runtime",
		"changing Runtime while profiled clears the profile Assignee",
		"Typing an exact listed name reselects that profile",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("profile route summary omitted %q:\n%s", text, view)
		}
	}

	unpinned := newTaskForm("default", []orchestration.ProfileRoute{{
		Name: "worker", Runtime: model.RuntimeGemini,
	}}, model.TaskStatusTodo, nil).renderRouteSummary(80)
	if !strings.Contains(unpinned, "Model: CLI default (unpinned)") ||
		!strings.Contains(unpinned, "Provider: CLI default") {
		t.Fatalf("unpinned profile route was summarized incorrectly:\n%s", unpinned)
	}

	form.focus = fieldRuntime
	form.runtimeIndex = optionIndex(formRuntimes, "codex")
	form.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if form.profileIndex != 0 ||
		form.inputs[fieldAssignee].Value() != "" ||
		formRuntimes[form.runtimeIndex] != "manual" {
		t.Fatalf(
			"manual runtime did not detach the profile: profile=%d assignee=%q runtime=%q",
			form.profileIndex,
			form.inputs[fieldAssignee].Value(),
			formRuntimes[form.runtimeIndex],
		)
	}
	manual := form.renderRouteSummary(80)
	for _, text := range []string{"Profile: Custom", "Runtime: manual", "Model: Manual task", "Provider: Not applicable"} {
		if !strings.Contains(manual, text) {
			t.Fatalf("manual route summary omitted %q:\n%s", text, manual)
		}
	}

	form.runtimeIndex = optionIndex(formRuntimes, "claude")
	custom := form.renderRouteSummary(80)
	if !strings.Contains(custom, "Model: CLI default (unpinned)") {
		t.Fatalf("custom agent route did not explain its unpinned model:\n%s", custom)
	}
}

func TestTaskFormRouteSummaryExplainsMissingProfilesWithoutBlockingManualTask(t *testing.T) {
	form := newTaskForm("default", nil, model.TaskStatusTriage, nil)
	form.setInputValue(fieldTitle, "Manual follow-up")
	form.focus = fieldProfile
	view := (&Model{form: form}).renderTaskForm(58, 40)
	normalized := strings.Join(strings.Fields(strings.ReplaceAll(view, "│", " ")), " ")
	for _, text := range []string{
		"No runnable profiles",
		"Agents / Board settings",
		"Manual task creation remains available",
		"Model: Manual task",
		"Ctrl+S save",
	} {
		if !strings.Contains(normalized, text) {
			t.Fatalf("narrow no-profile form omitted %q:\n%s", text, view)
		}
	}
	if err := form.validate(); err != nil {
		t.Fatalf("no-profile callout blocked a Triage manual task: %v", err)
	}
}

func TestTypingKnownProfileNameReassociatesAuthoritativeRoute(t *testing.T) {
	profiles := []orchestration.ProfileRoute{{
		Name: "reviewer", Runtime: model.RuntimeCodex, Model: "gpt-review", Provider: "openai",
	}}
	form := newTaskForm("default", profiles, model.TaskStatusTriage, nil)
	form.focus = fieldAssignee
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("reviewer")})

	if form.profileIndex != 1 ||
		form.inputs[fieldAssignee].Value() != "reviewer" ||
		formRuntimes[form.runtimeIndex] != "codex" {
		t.Fatalf(
			"known assignee did not restore its profile: profile=%d assignee=%q runtime=%q",
			form.profileIndex,
			form.inputs[fieldAssignee].Value(),
			formRuntimes[form.runtimeIndex],
		)
	}
	summary := form.renderRouteSummary(80)
	for _, text := range []string{"Profile: reviewer", "Runtime: codex", "Model: gpt-review", "Provider: openai"} {
		if !strings.Contains(summary, text) {
			t.Fatalf("reassociated route summary omitted %q:\n%s", text, summary)
		}
	}
}

func TestTaskFormTreatsTaskDerivedRouteNameAsCanonical(t *testing.T) {
	// BoardContext does not distinguish task-derived routes from configured
	// profiles, so every displayed runnable name has the same form invariant.
	form := newTaskForm(
		"default",
		[]orchestration.ProfileRoute{{Name: "historical-worker", Runtime: model.RuntimeClaude}},
		model.TaskStatusTriage,
		nil,
	)
	form.setInputValue(fieldAssignee, "historical-worker")
	form.syncProfileFromAssignee()
	if form.profileIndex != 1 || formRuntimes[form.runtimeIndex] != "claude" {
		t.Fatalf(
			"task-derived route was not treated canonically: profile=%d runtime=%q",
			form.profileIndex,
			formRuntimes[form.runtimeIndex],
		)
	}

	form.focus = fieldRuntime
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyRight})
	if form.profileIndex != 0 || form.inputs[fieldAssignee].Value() != "" {
		t.Fatalf(
			"changing a task-derived route runtime did not detach its name: profile=%d assignee=%q",
			form.profileIndex,
			form.inputs[fieldAssignee].Value(),
		)
	}
}

func TestEditTaskWithProfileNameUsesAuthoritativeRoute(t *testing.T) {
	assignee := "reviewer"
	task := testTask("task", "Custom route", model.TaskStatusReady)
	task.Assignee = &assignee
	task.Runtime = model.RuntimeClaude
	form := editTaskForm(
		"default",
		[]orchestration.ProfileRoute{{Name: "reviewer", Runtime: model.RuntimeCodex, Model: "gpt-review"}},
		task,
		nil,
	)
	if form.profileIndex != 1 || formRuntimes[form.runtimeIndex] != "codex" {
		t.Fatalf(
			"runtime-mismatched edit did not adopt configured route: profile=%d runtime=%q",
			form.profileIndex,
			formRuntimes[form.runtimeIndex],
		)
	}
	summary := form.renderRouteSummary(80)
	for _, text := range []string{"Profile: reviewer", "Runtime: codex", "Model: gpt-review"} {
		if !strings.Contains(summary, text) {
			t.Fatalf("authoritative edit route summary omitted %q:\n%s", text, summary)
		}
	}
	input := form.updateInput()
	if input.Runtime == nil || *input.Runtime != model.RuntimeCodex {
		t.Fatalf("authoritative edit runtime was not persisted: %#v", input.Runtime)
	}
}
