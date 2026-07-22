package terminalui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type fakeBackend struct {
	tasks   []model.Task
	details map[string]model.TaskDetail
}

func (f *fakeBackend) ListTasks(context.Context, store.ListTaskFilter) ([]model.Task, error) {
	return append([]model.Task{}, f.tasks...), nil
}

func (f *fakeBackend) GetTask(_ context.Context, id string) (model.TaskDetail, error) {
	return f.details[id], nil
}

func (f *fakeBackend) RelationshipGraph(_ context.Context, id string) (model.RelationshipGraph, error) {
	return model.RelationshipGraph{FocusTaskID: id, TotalConnectedNodes: 1}, nil
}

func testTask(id, title string, status model.TaskStatus) model.Task {
	return model.Task{ID: id, Title: title, Status: status, Runtime: model.RuntimeManual}
}

func TestModelLoadsAndNavigatesBoard(t *testing.T) {
	tasks := []model.Task{
		testTask("one", "First idea", model.TaskStatusTriage),
		testTask("two", "Second idea", model.TaskStatusTriage),
		testTask("three", "Ready work", model.TaskStatusReady),
	}
	backend := &fakeBackend{tasks: tasks, details: map[string]model.TaskDetail{}}
	for _, task := range tasks {
		backend.details[task.ID] = model.TaskDetail{Task: task}
	}
	m := NewModel(context.Background(), backend, "default")
	updated, command := m.Update(tasksLoadedMsg{tasks: tasks, at: time.Now()})
	m = updated.(*Model)
	if command == nil || m.selectedID() != "one" {
		t.Fatalf("expected first task and detail command, got %q", m.selectedID())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.selectedID() != "two" {
		t.Fatalf("expected second task, got %q", m.selectedID())
	}
	for range 3 {
		m.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.selectedID() != "three" {
		t.Fatalf("expected ready task, got %q", m.selectedID())
	}
}

func TestViewIncludesBoardAndResponsiveDetail(t *testing.T) {
	task := testTask("task_1", "Implement terminal board", model.TaskStatusReady)
	m := NewModel(context.Background(), &fakeBackend{}, "product")
	m.width, m.height = 120, 32
	m.tasks[model.TaskStatusReady] = []model.Task{task}
	m.column = statusIndex(m.statuses(), model.TaskStatusReady)
	m.detail = &model.TaskDetail{Task: task}
	view := m.View()
	for _, value := range []string{"AUTOGORA", "product", "READY", "Implement terminal board", "Overview"} {
		if !strings.Contains(view, value) {
			t.Fatalf("view does not contain %q", value)
		}
	}
}

func TestReloadKeepsSelectedTask(t *testing.T) {
	one := testTask("one", "One", model.TaskStatusTodo)
	two := testTask("two", "Two", model.TaskStatusTodo)
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.tasks[model.TaskStatusTodo] = []model.Task{one, two}
	m.column = statusIndex(m.statuses(), model.TaskStatusTodo)
	m.cursors[model.TaskStatusTodo] = 1
	m.Update(tasksLoadedMsg{tasks: []model.Task{two, one}, at: time.Now()})
	if m.selectedID() != "two" {
		t.Fatalf("reload changed selection to %q", m.selectedID())
	}
}

func TestSearchInputAndArchiveToggle(t *testing.T) {
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("terminal")})
	if m.searchDraft != "terminal" || m.inputMode != "search" {
		t.Fatalf("search input was not captured: %#v", m)
	}
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.search != "terminal" || command == nil {
		t.Fatalf("search was not applied: %q", m.search)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !m.showArchived || len(m.statuses()) != len(boardStatuses)+1 {
		t.Fatal("archived column was not enabled")
	}
}

func TestDetailTabsRenderRelationshipsAndActivity(t *testing.T) {
	task := testTask("task", "Focused task", model.TaskStatusBlocked)
	parent := testTask("parent", "Required work", model.TaskStatusDone)
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.width, m.height = 120, 36
	m.tasks[task.Status] = []model.Task{task}
	m.column = statusIndex(m.statuses(), task.Status)
	m.detail = &model.TaskDetail{Task: task, Prerequisites: []model.Task{parent}, Comments: []model.Comment{{Author: "human", Body: "Please verify"}}}
	m.graph = &model.RelationshipGraph{TotalConnectedNodes: 2, TotalPhases: 2}
	m.detailTab = 1
	if view := m.View(); !strings.Contains(view, "Required work") || !strings.Contains(view, "Connected 2") {
		t.Fatalf("relationship detail missing:\n%s", view)
	}
	m.detailTab = 2
	if view := m.View(); !strings.Contains(view, "Please verify") || !strings.Contains(view, "Activity") {
		t.Fatalf("activity detail missing:\n%s", view)
	}
}
