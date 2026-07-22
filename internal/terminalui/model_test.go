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
	m.column = statusIndex(model.TaskStatusReady)
	m.detail = &model.TaskDetail{Task: task}
	view := m.View()
	for _, value := range []string{"AUTOGORA", "product", "READY", "Implement terminal board", "Prerequisites"} {
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
	m.column = statusIndex(model.TaskStatusTodo)
	m.cursors[model.TaskStatusTodo] = 1
	m.Update(tasksLoadedMsg{tasks: []model.Task{two, one}, at: time.Now()})
	if m.selectedID() != "two" {
		t.Fatalf("reload changed selection to %q", m.selectedID())
	}
}
