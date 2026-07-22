package cli

import (
	"context"
	"os"

	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/taskservice"
	"github.com/nn1a/autogora/internal/terminalui"
)

func (a *App) runTUI(ctx context.Context, opts options) error {
	opened, manager, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()

	dbPath, err := a.dispatcherDBPath(opts.value("db"))
	if err != nil {
		return err
	}
	cliPath, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := a.workingDirectory()
	if err != nil {
		return err
	}
	service := taskservice.New(opened, manager, board).WithTaskDispatcher(func(dispatchContext context.Context, taskID string) error {
		autoDecompose := false
		return dispatcher.Run(dispatchContext, dispatcher.Options{
			DBPath: dbPath, CLIPath: cliPath, Board: board, TaskID: taskID, Once: true,
			AutoDecompose: &autoDecompose,
			AllowWrites:   opts.flags["allow-writes"], WorkingDirectory: cwd, Getenv: a.Getenv,
		})
	})
	return terminalui.Run(ctx, service, terminalui.Options{
		Board:  board,
		Input:  a.Stdin,
		Output: a.Stdout,
	})
}
