package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/supervisor"
	"github.com/nn1a/autogora/internal/terminalui"
)

type tuiGlobalAgentsBackend struct {
	options                  agentconfig.Options
	controller               *supervisor.Controller
	parent                   context.Context
	detect                   func(context.Context, agentconfig.Config) ([]agentconfig.Detection, error)
	activeRuns               func(context.Context) (int, error)
	overrideAllowWrites      bool
	overrideAllowWritesValue bool
}

func (b *tuiGlobalAgentsBackend) effectiveSupervisorConfig(
	config agentconfig.Config,
) agentconfig.Config {
	config = agentconfig.Normalize(config)
	if b.overrideAllowWrites {
		config.Supervisor.AllowWrites = b.overrideAllowWritesValue
	}
	return config
}

func (b *tuiGlobalAgentsBackend) LoadGlobalAgents(
	ctx context.Context,
) (terminalui.GlobalAgentsContext, error) {
	snapshot, err := agentconfig.LoadSnapshot(b.options)
	if err != nil {
		return terminalui.GlobalAgentsContext{}, err
	}
	return b.contextFromSnapshot(ctx, snapshot)
}

func (b *tuiGlobalAgentsBackend) contextFromSnapshot(
	ctx context.Context,
	snapshot agentconfig.Snapshot,
) (terminalui.GlobalAgentsContext, error) {
	activeRuns := 0
	var err error
	if b.activeRuns != nil {
		activeRuns, err = b.activeRuns(ctx)
		if err != nil {
			return terminalui.GlobalAgentsContext{}, fmt.Errorf("list active runs: %w", err)
		}
	}
	return terminalui.GlobalAgentsContext{
		Path: snapshot.Path, Exists: snapshot.Exists,
		Revision: snapshot.Revision, Config: snapshot.Config,
		Presets:    agentconfig.BuiltinPresets(),
		Supervisor: b.controller.Status(), ActiveRuns: activeRuns,
	}, nil
}

func (b *tuiGlobalAgentsBackend) DetectGlobalAgents(
	ctx context.Context,
	config agentconfig.Config,
) ([]agentconfig.Detection, error) {
	if b.detect == nil {
		return agentconfig.DetectSupportedAgents(ctx, config, agentconfig.DetectOptions{})
	}
	return b.detect(ctx, config)
}

func (b *tuiGlobalAgentsBackend) saveAndApply(
	ctx context.Context,
	expected agentconfig.Revision,
	config agentconfig.Config,
	desired bool,
) (terminalui.GlobalAgentsContext, error) {
	config = agentconfig.Normalize(config)
	if err := agentconfig.Validate(config); err != nil {
		return terminalui.GlobalAgentsContext{}, fmt.Errorf("validate agent configuration: %w", err)
	}
	snapshot, err := agentconfig.CompareAndSwap(b.options, expected, config)
	if err != nil {
		return terminalui.GlobalAgentsContext{}, agentConfigSaveError(err)
	}
	reconcile, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	effective := b.effectiveSupervisorConfig(snapshot.Config)
	if err := b.controller.Reconcile(reconcile, b.parent, effective, desired); err != nil {
		value, contextErr := b.contextFromSnapshot(ctx, snapshot)
		return value, errors.Join(fmt.Errorf(
			"saved configuration, but could not apply Supervisor settings: %w", err,
		), contextErr)
	}
	return b.contextFromSnapshot(ctx, snapshot)
}

func (b *tuiGlobalAgentsBackend) SaveGlobalAgents(
	ctx context.Context,
	expected agentconfig.Revision,
	config agentconfig.Config,
) (terminalui.GlobalAgentsContext, error) {
	desired := b.controller.Status().Desired
	return b.saveAndApply(ctx, expected, config, desired)
}

func (b *tuiGlobalAgentsBackend) StartSupervisor(
	ctx context.Context,
	expected agentconfig.Revision,
	config agentconfig.Config,
) (terminalui.GlobalAgentsContext, error) {
	return b.saveAndApply(ctx, expected, config, true)
}

func (b *tuiGlobalAgentsBackend) StopSupervisor(
	ctx context.Context,
) (terminalui.GlobalAgentsContext, error) {
	stop, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.controller.Stop(stop); err != nil {
		return terminalui.GlobalAgentsContext{}, err
	}
	return b.LoadGlobalAgents(ctx)
}

func (b *tuiGlobalAgentsBackend) SupervisorStatus() supervisor.Status {
	return b.controller.Status()
}
