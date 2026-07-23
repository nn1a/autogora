package terminalui

import (
	"context"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
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
	BoardSettingsContext(context.Context) (taskservice.BoardContext, error)
	SpecifyTask(context.Context, string, *orchestration.SpecificationPlan, string) (model.TaskDetail, error)
	DecomposeTask(context.Context, string, *orchestration.DecompositionPlan) (orchestration.DecompositionResult, error)
	DispatchTask(context.Context, string) error
	TerminateRun(context.Context, string, string) (runcontrol.Termination, error)
	DeleteTask(context.Context, string) error
	ScheduleTask(context.Context, string, *string, string) (model.TaskDetail, error)
	LinkTasks(context.Context, string, string) (model.TaskDetail, error)
	UnlinkTasks(context.Context, string, string) (model.TaskDetail, error)
	SetSubtaskParent(context.Context, string, string, *int) (model.TaskDetail, error)
	RemoveSubtask(context.Context, string, string) (model.TaskDetail, error)
	AttachFile(context.Context, string, string, string) (model.Attachment, error)
	AttachURL(context.Context, string, string, string) (model.Attachment, error)
	RemoveAttachment(context.Context, string, string) error
	UpdateBoardOrchestration(context.Context, boards.OrchestrationSettings, boards.OrchestrationUpdate) (taskservice.BoardContext, error)
}
