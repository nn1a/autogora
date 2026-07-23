package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/supervisor"
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
	config, err := agentconfig.Load(agentconfig.Options{Getenv: a.Getenv})
	if err != nil {
		return err
	}
	allowWrites := config.Supervisor.AllowWrites
	if opts.present("allow-writes") {
		allowWrites = opts.flags["allow-writes"]
	}
	controller := supervisor.New(supervisor.Options{
		DBPath: dbPath, CLIPath: cliPath, WorkingDirectory: cwd, Getenv: a.Getenv,
	})
	if config.Supervisor.AutoStart {
		controller.Start(ctx, config)
	}
	service := taskservice.New(opened, manager, board).WithTaskDispatcher(func(dispatchContext context.Context, taskID string) error {
		autoDecompose := false
		return dispatcher.Run(dispatchContext, dispatcher.Options{
			DBPath: dbPath, CLIPath: cliPath, Board: board, TaskID: taskID, Once: true,
			AutoDecompose: &autoDecompose, AgentConfig: &config,
			AllowWrites: allowWrites, WorkingDirectory: cwd, Getenv: a.Getenv,
		})
	})
	runErr := terminalui.Run(ctx, service, terminalui.Options{
		Board:  board,
		Input:  a.Stdin,
		Output: a.Stdout,
	})
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(runErr, controller.Stop(stop))
}
