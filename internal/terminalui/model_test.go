package terminalui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

type fakeBackend struct {
	tasks   []model.Task
	details map[string]model.TaskDetail
	actions []string
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

func (f *fakeBackend) BoardContext(context.Context) (taskservice.BoardContext, error) {
	return taskservice.BoardContext{}, nil
}

func (f *fakeBackend) SpecifyTask(_ context.Context, id string, _ *orchestration.SpecificationPlan, _ string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "specify:"+id)
	return model.TaskDetail{Task: testTask(id, "specified", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) DecomposeTask(_ context.Context, id string, _ *orchestration.DecompositionPlan) (orchestration.DecompositionResult, error) {
	f.actions = append(f.actions, "decompose:"+id)
	return orchestration.DecompositionResult{Task: model.TaskDetail{Task: testTask(id, "decomposed", model.TaskStatusTodo)}}, nil
}

func (f *fakeBackend) DispatchTask(_ context.Context, id string) error {
	f.actions = append(f.actions, "start:"+id)
	return nil
}

func (f *fakeBackend) TerminateRun(_ context.Context, id, _ string) (runcontrol.Termination, error) {
	f.actions = append(f.actions, "terminate:"+id)
	return runcontrol.Termination{Task: model.TaskDetail{Task: testTask("task", "terminated", model.TaskStatusTodo)}}, nil
}

func (f *fakeBackend) DeleteTask(_ context.Context, id string) error {
	f.actions = append(f.actions, "delete:"+id)
	return nil
}

func (f *fakeBackend) ScheduleTask(_ context.Context, id string, _ *string, _ string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "schedule:"+id)
	return model.TaskDetail{Task: testTask(id, "scheduled", model.TaskStatusScheduled)}, nil
}

func (f *fakeBackend) LinkTasks(_ context.Context, parent, child string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "link:"+parent+":"+child)
	return model.TaskDetail{Task: testTask(child, "linked", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) UnlinkTasks(_ context.Context, parent, child string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "unlink:"+parent+":"+child)
	return model.TaskDetail{Task: testTask(child, "unlinked", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) SetSubtaskParent(_ context.Context, parent, child string, _ *int) (model.TaskDetail, error) {
	f.actions = append(f.actions, "subtask:"+parent+":"+child)
	return model.TaskDetail{Task: testTask(child, "subtask", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) RemoveSubtask(_ context.Context, parent, child string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "subtask-remove:"+parent+":"+child)
	return model.TaskDetail{Task: testTask(child, "subtask removed", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) AttachFile(_ context.Context, id, path, _ string) (model.Attachment, error) {
	f.actions = append(f.actions, "attach-file:"+id+":"+path)
	return model.Attachment{TaskID: id, Path: &path}, nil
}

func (f *fakeBackend) AttachURL(_ context.Context, id, url, _ string) (model.Attachment, error) {
	f.actions = append(f.actions, "attach-url:"+id+":"+url)
	return model.Attachment{TaskID: id, URL: &url}, nil
}

func (f *fakeBackend) RemoveAttachment(_ context.Context, id, attachment string) error {
	f.actions = append(f.actions, "attach-remove:"+id+":"+attachment)
	return nil
}

func (f *fakeBackend) CreateTask(_ context.Context, input store.CreateTaskInput) (model.TaskDetail, error) {
	task := testTask("created", input.Title, input.Status)
	f.actions = append(f.actions, "create:"+input.Title)
	return model.TaskDetail{Task: task}, nil
}

func (f *fakeBackend) UpdateTask(_ context.Context, id string, input store.UpdateTaskInput) (model.TaskDetail, error) {
	f.actions = append(f.actions, "update:"+id)
	task := testTask(id, "updated", model.TaskStatusTodo)
	if input.Title != nil {
		task.Title = *input.Title
	}
	return model.TaskDetail{Task: task}, nil
}

func (f *fakeBackend) PromoteTask(_ context.Context, id string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "promote:"+id)
	return model.TaskDetail{Task: testTask(id, "promoted", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) CompleteTask(_ context.Context, id string, _ store.CompletionInput) (model.TaskDetail, error) {
	f.actions = append(f.actions, "complete:"+id)
	return model.TaskDetail{Task: testTask(id, "done", model.TaskStatusDone)}, nil
}

func (f *fakeBackend) BlockTask(_ context.Context, id string, input store.BlockInput) (model.TaskDetail, error) {
	f.actions = append(f.actions, "block:"+id+":"+input.Reason)
	return model.TaskDetail{Task: testTask(id, "blocked", model.TaskStatusBlocked)}, nil
}

func (f *fakeBackend) UnblockTask(_ context.Context, id string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "unblock:"+id)
	return model.TaskDetail{Task: testTask(id, "unblocked", model.TaskStatusTodo)}, nil
}

func (f *fakeBackend) ArchiveTask(_ context.Context, id string) (model.TaskDetail, error) {
	f.actions = append(f.actions, "archive:"+id)
	return model.TaskDetail{Task: testTask(id, "archived", model.TaskStatusArchived)}, nil
}

func (f *fakeBackend) AddComment(_ context.Context, id, author, body string) (model.Comment, error) {
	f.actions = append(f.actions, "comment:"+id+":"+body)
	return model.Comment{TaskID: id, Author: author, Body: body}, nil
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

func TestDestructiveActionPinsSelectionAndRequiresConfirmation(t *testing.T) {
	task := testTask("task", "Finish safely", model.TaskStatusReview)
	backend := &fakeBackend{details: map[string]model.TaskDetail{}}
	m := NewModel(context.Background(), backend, "default")
	m.tasks[task.Status] = []model.Task{task}
	m.column = statusIndex(m.statuses(), task.Status)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if m.confirm == nil || len(backend.actions) != 0 {
		t.Fatal("archive should wait for confirmation")
	}
	command := m.moveColumn(-1)
	_ = command
	_, command = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	message := command()
	m.Update(message)
	if len(backend.actions) != 1 || backend.actions[0] != "archive:task" {
		t.Fatalf("confirmation targeted the wrong task: %v", backend.actions)
	}
}

func TestTaskFormCreatesTriageTask(t *testing.T) {
	backend := &fakeBackend{}
	m := NewModel(context.Background(), backend, "default")
	m.Update(boardContextMsg{context: taskservice.BoardContext{}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Investigate issue")})
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	message := command()
	m.Update(message)
	if len(backend.actions) != 1 || backend.actions[0] != "create:Investigate issue" || m.desiredSelection != "created" {
		t.Fatalf("task creation did not complete: actions=%v selection=%q", backend.actions, m.desiredSelection)
	}
}

func TestMutationErrorSurvivesBackgroundLoadsUntilDismissedOrRetried(t *testing.T) {
	task := testTask("task", "Keep the failure visible", model.TaskStatusTodo)
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.tasks[task.Status] = []model.Task{task}
	m.allTasks = []model.Task{task}
	m.column = statusIndex(m.statuses(), task.Status)
	mutationErr := errors.New("worker authentication required")
	m.Update(mutationMsg{action: "promote", id: task.ID, err: mutationErr})

	m.Update(tasksLoadedMsg{tasks: []model.Task{task}, at: time.Now()})
	m.Update(detailLoadedMsg{id: task.ID, detail: model.TaskDetail{Task: task}})
	m.Update(boardContextMsg{context: taskservice.BoardContext{}})
	if !errors.Is(m.err, mutationErr) {
		t.Fatalf("background loads cleared mutation error: %v", m.err)
	}
	detailRefreshErr := errors.New("temporary detail failure")
	m.Update(detailLoadedMsg{id: task.ID, err: detailRefreshErr})
	if !errors.Is(m.err, mutationErr) || !errors.Is(m.detailErr, detailRefreshErr) {
		t.Fatalf("detail error displaced mutation failure: mutation=%v detail=%v", m.err, m.detailErr)
	}

	refreshErr := errors.New("temporary refresh failure")
	m.Update(tasksLoadedMsg{err: refreshErr})
	if !errors.Is(m.err, mutationErr) || !strings.Contains(m.View(), mutationErr.Error()) {
		t.Fatalf("refresh error displaced mutation failure: mutation=%v refresh=%v", m.err, m.tasksErr)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.err != nil {
		t.Fatalf("escape did not dismiss mutation error: %v", m.err)
	}
	if !strings.Contains(m.View(), refreshErr.Error()) {
		t.Fatal("dismissing the mutation error should reveal the current refresh error")
	}

	m.err = mutationErr
	command := m.mutate("promote", task.ID, "")
	if command == nil || m.err != nil || !m.busy {
		t.Fatalf("starting the next mutation did not clear the old error: command=%v err=%v busy=%v", command != nil, m.err, m.busy)
	}
}

func TestTargetedRunKeepsNavigationAndRefreshAvailable(t *testing.T) {
	ready := testTask("ready", "Long running task", model.TaskStatusReady)
	running := testTask("running", "Another active task", model.TaskStatusRunning)
	backend := &fakeBackend{tasks: []model.Task{ready, running}, details: map[string]model.TaskDetail{
		ready.ID:   {Task: ready},
		running.ID: {Task: running},
	}}
	m := NewModel(context.Background(), backend, "default")
	m.allTasks = append([]model.Task{}, backend.tasks...)
	m.regroupTasks()
	m.column = statusIndex(m.statuses(), model.TaskStatusReady)

	runCommand := m.mutate("start", ready.ID, "")
	if runCommand == nil || !m.busy || m.busyAction != "start" {
		t.Fatalf("targeted dispatcher did not enter running state: busy=%v action=%q", m.busy, m.busyAction)
	}
	_, detailCommand := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.selectedID() != running.ID || detailCommand == nil {
		t.Fatalf("navigation/detail load was blocked during dispatcher run: selected=%q", m.selectedID())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.detailTab != 1 {
		t.Fatalf("detail tabs were blocked during dispatcher run: tab=%d", m.detailTab)
	}
	_, refreshCommand := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if refreshCommand == nil {
		t.Fatal("manual refresh was blocked during dispatcher run")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.form != nil {
		t.Fatal("a second mutation form opened while dispatcher run was active")
	}
	if command := m.mutate("promote", running.ID, ""); command != nil {
		t.Fatal("a second mutation started while dispatcher run was active")
	}
	if !strings.Contains(m.View(), "dispatcher running") || !strings.Contains(m.View(), "navigation available") {
		t.Fatal("the running-state hint does not explain that navigation remains available")
	}

	m.Update(runCommand())
	if m.busy || m.busyAction != "" {
		t.Fatalf("dispatcher completion did not release mutation lock: busy=%v action=%q", m.busy, m.busyAction)
	}
}

func TestEmptyBoardExplainsFirstWorkflowAndGitHubImport(t *testing.T) {
	m := NewModel(context.Background(), &fakeBackend{}, "product")
	m.width, m.height = 120, 32
	m.Update(tasksLoadedMsg{tasks: nil, at: time.Now()})
	view := m.View()
	for _, value := range []string{
		"Start your first workflow",
		"Configure agents",
		"autogora agents detect --save",
		"Choose a workspace",
		"Execution",
		"Import or create work",
		"autogora github import --repo OWNER/REPO --board product",
		"Press n to create",
	} {
		if !strings.Contains(view, value) {
			t.Fatalf("empty-board guidance omitted %q:\n%s", value, view)
		}
	}
}

func TestSuccessNoticeExpiresWithoutBeingClearedByImmediateReload(t *testing.T) {
	m := NewModel(context.Background(), &fakeBackend{}, "default")
	m.Update(mutationMsg{action: "create", detail: &model.TaskDetail{Task: testTask("created", "New", model.TaskStatusTriage)}})
	if m.notice == "" || m.noticeUntil.IsZero() {
		t.Fatal("successful mutation did not create a bounded notice")
	}
	m.Update(tasksLoadedMsg{tasks: nil, at: m.noticeUntil.Add(-time.Second)})
	if m.notice == "" {
		t.Fatal("the mutation reload cleared the success notice immediately")
	}
	m.Update(refreshMsg(m.noticeUntil))
	if m.notice != "" || !m.noticeUntil.IsZero() {
		t.Fatalf("stale success notice remained visible: %q until=%v", m.notice, m.noticeUntil)
	}
}
