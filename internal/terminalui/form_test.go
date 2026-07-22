package terminalui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

func TestTaskFormAppliesBoardProfileToAgentFields(t *testing.T) {
	profiles := []orchestration.ProfileRoute{{Name: "reviewer", Runtime: model.RuntimeGemini, Description: "Reviews changes"}}
	form := newTaskForm("product", profiles, model.TaskStatusTodo)
	form.focus = fieldProfile
	form.syncFocus()
	form.Update(tea.KeyMsg{Type: tea.KeyRight})
	if form.profileIndex != 1 || form.inputs[fieldAssignee].Value() != "reviewer" || formRuntimes[form.runtimeIndex] != "gemini" {
		t.Fatalf("profile was not applied: profile=%d assignee=%q runtime=%q", form.profileIndex, form.inputs[fieldAssignee].Value(), formRuntimes[form.runtimeIndex])
	}
}

func TestTaskFormBuildsCompleteCreateInput(t *testing.T) {
	form := newTaskForm("product", nil, model.TaskStatusReady)
	form.setInputValue(fieldTitle, "Implement TUI forms")
	form.body.SetValue("Match the Web task fields.")
	form.setInputValue(fieldPriority, "8")
	form.setInputValue(fieldAssignee, "implementer")
	form.runtimeIndex = optionIndex(formRuntimes, "codex")
	form.setInputValue(fieldSkills, "go, tui, go")
	form.setInputValue(fieldTenant, "product")
	form.workspaceIndex = optionIndex(formWorkspaceKinds, "worktree")
	form.setInputValue(fieldWorkspace, "/workspace/repo")
	form.goalMode = true

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
	if input.WorkspaceKind != model.WorkspaceWorktree || len(input.Skills) != 2 || !input.GoalMode {
		t.Fatalf("execution fields missing: %#v", input)
	}
}

func TestRunningTaskFormLocksOwnershipButAllowsDescription(t *testing.T) {
	runID := "run"
	task := testTask("task", "Running work", model.TaskStatusRunning)
	task.CurrentRunID = &runID
	form := editTaskForm("default", nil, task)
	for _, field := range []formField{fieldProfile, fieldAssignee, fieldRuntime, fieldWorkspaceKind, fieldWorkspace} {
		if !form.locked(field) {
			t.Fatalf("field %v should be locked", field)
		}
	}
	if form.locked(fieldTitle) || form.locked(fieldBody) || form.locked(fieldPriority) {
		t.Fatal("non-ownership fields should remain editable")
	}
}

func TestReadyFormRequiresRunnableAgent(t *testing.T) {
	form := newTaskForm("default", nil, model.TaskStatusReady)
	form.setInputValue(fieldTitle, "Runnable task")
	if err := form.validate(); err == nil {
		t.Fatal("Ready task without an agent should be rejected")
	}
	form.setInputValue(fieldAssignee, "worker")
	form.runtimeIndex = optionIndex(formRuntimes, "claude")
	if err := form.validate(); err != nil {
		t.Fatalf("valid agent route was rejected: %v", err)
	}
}
