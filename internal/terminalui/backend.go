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
}
