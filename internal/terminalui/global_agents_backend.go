package terminalui

import (
	"context"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/supervisor"
)

// GlobalAgentsContext is the local, process-aware snapshot used by the TUI.
// The agent registry is global, while ActiveRuns describes the currently open
// board so users understand when a saved route will take effect.
type GlobalAgentsContext struct {
	Path       string
	Exists     bool
	Revision   agentconfig.Revision
	Config     agentconfig.Config
	Presets    []agentconfig.Preset
	Supervisor supervisor.Status
	ActiveRuns int
}

// GlobalAgentsBackend keeps global configuration and Supervisor lifecycle
// operations out of the terminal renderer. Implementations are local process
// adapters; the TUI never calls its own HTTP API.
type GlobalAgentsBackend interface {
	LoadGlobalAgents(context.Context) (GlobalAgentsContext, error)
	DetectGlobalAgents(context.Context, agentconfig.Config) ([]agentconfig.Detection, error)
	SaveGlobalAgents(context.Context, agentconfig.Revision, agentconfig.Config) (GlobalAgentsContext, error)
	StartSupervisor(context.Context, agentconfig.Revision, agentconfig.Config) (GlobalAgentsContext, error)
	StopSupervisor(context.Context) (GlobalAgentsContext, error)
	SupervisorStatus() supervisor.Status
}
