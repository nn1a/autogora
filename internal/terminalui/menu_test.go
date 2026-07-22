package terminalui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
)

func menuHas(menu *actionMenu, action string) bool {
	for _, item := range menu.items {
		if item.action == action {
			return true
		}
	}
	return false
}

func TestTriageActionMenuIncludesPlannerActions(t *testing.T) {
	task := testTask("triage", "Unclear request", model.TaskStatusTriage)
	menu := taskActionMenu(task, nil)
	for _, action := range []string{"edit", "specify", "decompose", "promote", "complete", "relationships", "delete"} {
		if !menuHas(menu, action) {
			t.Fatalf("triage menu omitted %q: %#v", action, menu.items)
		}
	}
}

func TestRunningActionMenuOffersTerminationWithoutInvalidMoves(t *testing.T) {
	runID := "run_active"
	task := testTask("running", "Active work", model.TaskStatusRunning)
	task.CurrentRunID = &runID
	menu := taskActionMenu(task, nil)
	if !menuHas(menu, "terminate:"+runID) {
		t.Fatal("running task does not offer termination")
	}
	for _, action := range []string{"move", "complete", "archive", "delete"} {
		if menuHas(menu, action) {
			t.Fatalf("running task unexpectedly offers %q", action)
		}
	}
}

func TestPlannerActionStaysPinnedToMenuTask(t *testing.T) {
	task := testTask("triage", "Plan this", model.TaskStatusTriage)
	backend := &fakeBackend{}
	m := NewModel(context.Background(), backend, "default")
	menu := taskActionMenu(task, nil)
	m.menu = menu
	for index, item := range menu.items {
		if item.action == "decompose" {
			m.menu.index = index
		}
	}
	m.updateMenu(tea.KeyMsg{Type: tea.KeyEnter})
	if m.confirm == nil || m.confirm.id != task.ID {
		t.Fatal("decompose should require a task-pinned confirmation")
	}
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m.Update(command())
	if len(backend.actions) != 1 || backend.actions[0] != "decompose:triage" {
		t.Fatalf("wrong planner action: %v", backend.actions)
	}
}

func TestRelationshipPickerUsesUnfilteredBoardTasks(t *testing.T) {
	focus := testTask("focus", "Focus", model.TaskStatusTodo)
	hidden := testTask("hidden", "Hidden by board search", model.TaskStatusReady)
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.allTasks = []model.Task{focus, hidden}
	menu := m.relationshipPicker(focus, "link-prerequisite")
	if len(menu.items) != 1 || menu.items[0].action != "link-prerequisite:hidden" {
		t.Fatalf("relationship candidate missing: %#v", menu.items)
	}
}

func TestActionMenuFiltersWithoutLosingItems(t *testing.T) {
	task := testTask("task", "Work", model.TaskStatusTriage)
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.menu = taskActionMenu(task, nil)
	original := len(m.menu.items)
	m.updateMenu(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.updateMenu(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("decompose")})
	if len(m.menu.items) != 1 || m.menu.items[0].action != "decompose" {
		t.Fatalf("menu filter mismatch: %#v", m.menu.items)
	}
	m.updateMenu(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.menu.items) != original {
		t.Fatalf("clearing filter lost menu items: got %d want %d", len(m.menu.items), original)
	}
}

func TestBoardFilterUsesSameTaskDataAsRelationshipPicker(t *testing.T) {
	reviewer := "reviewer"
	implementer := "implementer"
	review := testTask("review", "Review", model.TaskStatusTodo)
	review.Assignee, review.Runtime = &reviewer, model.RuntimeGemini
	implementation := testTask("implementation", "Implement", model.TaskStatusReady)
	implementation.Assignee, implementation.Runtime = &implementer, model.RuntimeCodex
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.allTasks = []model.Task{review, implementation}
	m.regroupTasks()

	menu := m.filterMenu()
	m.runMenuAction(menu, "filter-assignee")
	if len(m.menu.items) != 3 {
		t.Fatalf("unexpected assignee choices: %#v", m.menu.items)
	}
	m.runMenuAction(m.menu, "set-assignee:reviewer")
	if len(m.tasks[model.TaskStatusTodo]) != 1 || len(m.tasks[model.TaskStatusReady]) != 0 {
		t.Fatalf("filter did not narrow board: %#v", m.tasks)
	}
	if len(m.allTasks) != 2 {
		t.Fatal("filter changed the shared unfiltered task data")
	}
}
