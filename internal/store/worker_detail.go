package store

import (
	"context"

	"github.com/nn1a/autogora/internal/model"
)

// WorkerTaskDetail keeps the focus task and its handoffs intact while reducing
// directly related tasks to routing metadata. This prevents scoped workers
// from learning neighboring bodies, workspaces, results, or block details
// through the embedded TaskDetail returned by show.
func (s *Store) WorkerTaskDetail(ctx context.Context, taskID string) (model.TaskDetail, error) {
	detail, err := s.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	detail.Parents = workerRelatedTasks(detail.Parents)
	detail.Children = workerRelatedTasks(detail.Children)
	detail.Prerequisites = workerRelatedTasks(detail.Prerequisites)
	detail.Dependents = workerRelatedTasks(detail.Dependents)
	detail.Subtasks = workerRelatedTasks(detail.Subtasks)
	if detail.ParentTask != nil {
		value := workerRelatedTask(*detail.ParentTask)
		detail.ParentTask = &value
	}
	return detail, nil
}

func workerRelatedTasks(tasks []model.Task) []model.Task {
	result := make([]model.Task, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, workerRelatedTask(task))
	}
	return result
}

func workerRelatedTask(task model.Task) model.Task {
	return model.Task{
		ID: task.ID, Board: task.Board, Tenant: task.Tenant, Title: task.Title,
		Assignee: task.Assignee, Runtime: task.Runtime, Status: task.Status,
		WorkflowRole: task.WorkflowRole, Priority: task.Priority,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
}
