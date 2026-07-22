package terminalui

import (
	"context"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

// Backend keeps the terminal UI independent from its transport. The local
// implementation is a Store; a remote API can implement the same contract.
type Backend interface {
	ListTasks(context.Context, store.ListTaskFilter) ([]model.Task, error)
	GetTask(context.Context, string) (model.TaskDetail, error)
	RelationshipGraph(context.Context, string) (model.RelationshipGraph, error)
	CreateTask(context.Context, store.CreateTaskInput) (model.TaskDetail, error)
	UpdateTask(context.Context, string, store.UpdateTaskInput) (model.TaskDetail, error)
	PromoteTask(context.Context, string) (model.TaskDetail, error)
	CompleteTask(context.Context, string, store.CompletionInput) (model.TaskDetail, error)
	BlockTask(context.Context, string, store.BlockInput) (model.TaskDetail, error)
	UnblockTask(context.Context, string) (model.TaskDetail, error)
	ArchiveTask(context.Context, string) (model.TaskDetail, error)
	AddComment(context.Context, string, string, string) (model.Comment, error)
}
