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

func tuiAgentConfigLoader(
	configOptions agentconfig.Options,
	overrideAllowWrites bool,
	overrideAllowWritesValue bool,
) dispatcher.AgentConfigLoader {
	return func() (agentconfig.Config, error) {
		config, err := agentconfig.Load(configOptions)
		if err != nil {
			return agentconfig.Config{}, err
		}
		if overrideAllowWrites {
			config.Supervisor.AllowWrites = overrideAllowWritesValue
		}
		return config, nil
	}
}

func newTUITaskDispatcher(
	run DispatchRunner,
	configOptions agentconfig.Options,
	dbPath, cliPath, board, workingDirectory string,
	getenv func(string) string,
	overrideAllowWrites bool,
	overrideAllowWritesValue bool,
) func(context.Context, string) error {
	if run == nil {
		run = dispatcher.Run
	}
	loadConfig := tuiAgentConfigLoader(
		configOptions,
		overrideAllowWrites,
		overrideAllowWritesValue,
	)
	return func(dispatchContext context.Context, taskID string) error {
		currentConfig, err := loadConfig()
		if err != nil {
			return err
		}
		autoDecompose := false
		return run(dispatchContext, dispatcher.Options{
			DBPath: dbPath, CLIPath: cliPath, Board: board, TaskID: taskID, Once: true,
			AutoDecompose: &autoDecompose, AgentConfig: &currentConfig,
			AgentConfigLoader: loadConfig,
			AllowWrites:       currentConfig.Supervisor.AllowWrites,
			WorkingDirectory:  workingDirectory, Getenv: getenv,
		})
	}
}

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
	configOptions := agentconfig.Options{Getenv: a.Getenv}
	config, err := agentconfig.Load(configOptions)
	if err != nil {
		return err
	}
	overrideAllowWrites := opts.present("allow-writes")
	overrideAllowWritesValue := opts.flags["allow-writes"]
	loadConfig := tuiAgentConfigLoader(
		configOptions,
		overrideAllowWrites,
		overrideAllowWritesValue,
	)
	controller := supervisor.New(supervisor.Options{
		DBPath: dbPath, CLIPath: cliPath, WorkingDirectory: cwd, Getenv: a.Getenv,
		AgentConfigLoader: loadConfig,
	})
	globalAgents := &tuiGlobalAgentsBackend{
		options: configOptions, controller: controller, parent: ctx,
		detect: a.detectSupportedAgents,
		activeRuns: func(activeContext context.Context) (int, error) {
			runs, listErr := opened.ListActiveRuns(activeContext, board)
			return len(runs), listErr
		},
		overrideAllowWrites:      overrideAllowWrites,
		overrideAllowWritesValue: overrideAllowWritesValue,
	}
	if config.Supervisor.AutoStart {
		controller.Start(ctx, globalAgents.effectiveSupervisorConfig(config))
	}
	service := taskservice.New(opened, manager, board)
	service.WithTaskDispatcher(newTUITaskDispatcher(
		a.DispatchRunner, configOptions, dbPath, cliPath, board, cwd, a.Getenv,
		overrideAllowWrites, overrideAllowWritesValue,
	))
	runErr := terminalui.Run(ctx, service, terminalui.Options{
		Board: board, Input: a.Stdin, Output: a.Stdout, GlobalAgents: globalAgents,
	})
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(runErr, controller.Stop(stop))
}
