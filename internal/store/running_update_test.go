package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestUpdateTaskLocksExecutionSettingsWhileRunning(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	assignee, tenant := "worker", "initial-tenant"
	workspace, branch := "/workspace/original", "feature/original"
	maxRuntime, maxRetries, goalTurns := 60, 2, 5
	workflow, step := "delivery", "implement"
	created, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "Original", Body: "Original body", Assignee: &assignee, Runtime: model.RuntimeCodex,
		Tenant: &tenant, Priority: 1, Workspace: &workspace, WorkspaceKind: model.WorkspaceDir,
		Branch: &branch, MaxRuntimeSeconds: &maxRuntime, MaxRetries: maxRetries,
		Skills: []string{"review"}, GoalMaxTurns: goalTurns,
		WorkflowTemplateID: &workflow, CurrentStepKey: &step,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: created.Task.ID, WorkerID: "worker-test"})
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Task.Task.Status != model.TaskStatusRunning {
		t.Fatalf("task was not claimed: %#v", claim)
	}

	version := claim.Task.Task.UpdatedAt
	newTitle, newBody := "Changed", "Changed body"
	newAssignee, newWorkspace, newBranch := "other-worker", "/workspace/other", "feature/other"
	newRuntime, newWorkspaceKind := model.RuntimeClaude, model.WorkspaceWorktree
	scheduledAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	newMaxRuntime, newMaxRetries := 120, 4
	newSkills := []string{"testing"}
	newGoalMode, newGoalTurns := true, 12
	newWorkflow, newStep := "replacement", "review"
	newStatus := model.TaskStatusReview
	newWorkflowRole := model.WorkflowRoleReviewer
	metadataPriority := 99

	tests := []struct {
		name  string
		input UpdateTaskInput
	}{
		{name: "title", input: UpdateTaskInput{Title: &newTitle}},
		{name: "body", input: UpdateTaskInput{Body: &newBody}},
		{name: "assignee", input: UpdateTaskInput{Assignee: OptionalString{Set: true, Value: &newAssignee}}},
		{name: "tenant", input: UpdateTaskInput{Tenant: OptionalString{Set: true, Value: &tenant}}},
		{name: "runtime", input: UpdateTaskInput{Runtime: &newRuntime}},
		{name: "workspace", input: UpdateTaskInput{Workspace: OptionalString{Set: true, Value: &newWorkspace}}},
		{name: "workspace kind", input: UpdateTaskInput{WorkspaceKind: &newWorkspaceKind}},
		{name: "branch", input: UpdateTaskInput{Branch: OptionalString{Set: true, Value: &newBranch}}},
		{name: "schedule", input: UpdateTaskInput{ScheduledAt: OptionalString{Set: true, Value: &scheduledAt}}},
		{name: "max runtime", input: UpdateTaskInput{MaxRuntimeSeconds: OptionalInt{Set: true, Value: &newMaxRuntime}}},
		{name: "max retries", input: UpdateTaskInput{MaxRetries: &newMaxRetries}},
		{name: "skills", input: UpdateTaskInput{Skills: &newSkills}},
		{name: "goal mode", input: UpdateTaskInput{GoalMode: &newGoalMode}},
		{name: "goal turns", input: UpdateTaskInput{GoalMaxTurns: &newGoalTurns}},
		{name: "workflow", input: UpdateTaskInput{WorkflowTemplateID: OptionalString{Set: true, Value: &newWorkflow}}},
		{name: "workflow step", input: UpdateTaskInput{CurrentStepKey: OptionalString{Set: true, Value: &newStep}}},
		{name: "status", input: UpdateTaskInput{Status: &newStatus}},
		{name: "workflow role", input: UpdateTaskInput{WorkflowRole: &newWorkflowRole}},
		{name: "mixed metadata and execution", input: UpdateTaskInput{Priority: &metadataPriority, Title: &newTitle}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.input.ExpectedUpdatedAt = &version
			if _, err := opened.UpdateTask(ctx, created.Task.ID, test.input); err == nil || !strings.Contains(err.Error(), "while running") {
				t.Fatalf("running update error = %v, want execution lock", err)
			}
			loaded, err := opened.GetTask(ctx, created.Task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Task.UpdatedAt != version || loaded.Task.Title != "Original" || loaded.Task.Priority != 1 {
				t.Fatalf("rejected update changed the running task: %#v", loaded.Task)
			}
		})
	}

	priority := 7
	updated, err := opened.UpdateTask(ctx, created.Task.ID, UpdateTaskInput{
		ExpectedUpdatedAt: &version,
		Priority:          &priority,
	})
	if err != nil {
		t.Fatalf("safe metadata update: %v", err)
	}
	if updated.Task.Status != model.TaskStatusRunning || updated.Task.CurrentRunID == nil ||
		*updated.Task.CurrentRunID != claim.Run.ID || updated.Task.Priority != priority ||
		updated.Task.Tenant == nil || *updated.Task.Tenant != tenant {
		t.Fatalf("safe metadata update disturbed the active run: %#v", updated.Task)
	}
}
