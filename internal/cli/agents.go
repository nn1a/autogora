package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	setupcfg "github.com/nn1a/autogora/internal/setup"
)

const agentsHelp = `autogora agents <action> [options]

Actions:
  path                  Show the global agent configuration path
  list                  Show the global agent registry and defaults
  detect [--save]       Find supported CLIs through PATH and run only --version
  presets               List built-in agent registry presets
  preset <id>           Preview a preset; use --apply to save it
  set <id>              Add or update an agent
  enable <id>           Make a configured agent eligible for work
  disable <id>          Keep an agent configured but exclude it from work
  remove <id>           Remove an agent and references to it
  defaults              Select the preferred agents for each role
  supervisor            Configure automatic orchestration

Set options:
  --runtime <runtime>   codex, claude, cline, or gemini (required)
  --command <path>      Executable name or path (default: runtime)
  --model <model>       Model pinned for this agent
  --provider <name>     Provider pinned for this agent
  --roles <roles>       Comma-separated worker, planner, coordinator, and/or judge
  --fallbacks <ids>     Comma-separated fallback agent IDs
  --max-concurrent <n>  Maximum concurrent runs for this agent

Defaults options:
  --worker <ids>        Comma-separated worker agent IDs
  --planner <ids>       Comma-separated planner agent IDs
  --coordinator <ids>   Comma-separated coordinator agent IDs
  --judge <ids>         Comma-separated judge agent IDs

Preset options:
  --apply               Save the previewed preset
  --replace             Replace matching preset agents and preferred orders

Supervisor options:
  --auto-start=<bool>   Start orchestration with supported user interfaces
  --max-workers <n>     Maximum workers started by the supervisor
  --allow-writes=<bool> Allow coding agents to modify their workspaces

The configuration contains routing metadata only. Do not store API keys or
other credentials in it. Detection never sends a prompt or makes a paid API call.
`

type agentConfigReport struct {
	Path       string                 `json:"path"`
	Exists     bool                   `json:"exists"`
	Revision   agentconfig.Revision   `json:"revision"`
	Supervisor agentconfig.Supervisor `json:"supervisor"`
	Defaults   agentconfig.Defaults   `json:"defaults"`
	Agents     []agentconfig.Agent    `json:"agents"`
}

type agentPathReport struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type agentDetection struct {
	ID         string        `json:"id"`
	Runtime    model.Runtime `json:"runtime"`
	Executable string        `json:"executable,omitempty"`
	Version    string        `json:"version,omitempty"`
	State      string        `json:"state"`
	Configured bool          `json:"configured"`
	Saved      bool          `json:"saved"`
	Message    string        `json:"message,omitempty"`
}

type agentDetectionReport struct {
	Path   string           `json:"path"`
	Saved  bool             `json:"saved"`
	Agents []agentDetection `json:"agents"`
}

type agentPresetCatalogReport struct {
	Presets []agentconfig.Preset `json:"presets"`
}

type agentPresetReport struct {
	Path       string                  `json:"path"`
	Exists     bool                    `json:"exists"`
	Preset     agentconfig.Preset      `json:"preset"`
	Applied    bool                    `json:"applied"`
	Replaced   bool                    `json:"replaced"`
	Detections []agentconfig.Detection `json:"detections"`
	Config     agentconfig.Config      `json:"config"`
}

func (a *App) runAgents(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("agents requires path, list, detect, presets, preset, set, enable, disable, remove, defaults, or supervisor")
	}
	action := strings.ToLower(strings.TrimSpace(opts.positionals[0]))
	values := opts.positionals[1:]
	switch action {
	case "path":
		if err := rejectAgentOptions(opts); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents path does not accept arguments")
		}
		path, exists, _, err := a.loadAgentConfig()
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, agentPathReport{Path: path, Exists: exists})
	case "list", "ls":
		if err := rejectAgentOptions(opts); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents list does not accept arguments")
		}
		return a.writeAgentConfigReport()
	case "detect":
		if err := rejectAgentOptions(opts, "save"); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents detect does not accept arguments")
		}
		return a.detectAgents(ctx, opts.flags["save"])
	case "presets":
		if err := rejectAgentOptions(opts); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents presets does not accept arguments")
		}
		return writeJSON(a.Stdout, agentPresetCatalogReport{Presets: agentconfig.BuiltinPresets()})
	case "preset":
		if err := rejectAgentOptions(opts, "apply", "replace"); err != nil {
			return err
		}
		if len(values) != 1 {
			return errors.New("agents preset requires exactly one preset id")
		}
		if opts.flags["replace"] && !opts.flags["apply"] {
			return errors.New("agents preset --replace requires --apply")
		}
		return a.previewAgentPreset(ctx, values[0], opts.flags["apply"], opts.flags["replace"])
	case "set":
		if err := rejectAgentOptions(opts, "runtime", "command", "model", "provider", "roles", "fallbacks", "max-concurrent"); err != nil {
			return err
		}
		return a.setAgent(opts, values)
	case "enable", "disable":
		if err := rejectAgentOptions(opts); err != nil {
			return err
		}
		return a.setAgentEnabled(action, values)
	case "remove", "rm":
		if err := rejectAgentOptions(opts); err != nil {
			return err
		}
		return a.removeAgent(values)
	case "defaults":
		if err := rejectAgentOptions(opts, "worker", "planner", "coordinator", "judge"); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents defaults does not accept arguments")
		}
		return a.setAgentDefaults(opts)
	case "supervisor":
		if err := rejectAgentOptions(opts, "auto-start", "max-workers", "allow-writes"); err != nil {
			return err
		}
		if len(values) != 0 {
			return errors.New("agents supervisor does not accept arguments")
		}
		return a.setAgentSupervisor(opts)
	default:
		return fmt.Errorf("unknown agents action %q", action)
	}
}

func (a *App) agentConfigOptions() agentconfig.Options {
	return agentconfig.Options{Getenv: a.Getenv}
}

func (a *App) loadAgentConfig() (string, bool, agentconfig.Config, error) {
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return "", false, agentconfig.Config{}, err
	}
	return snapshot.Path, snapshot.Exists, snapshot.Config, nil
}

func (a *App) loadAgentConfigSnapshot() (agentconfig.Snapshot, error) {
	return agentconfig.LoadSnapshot(a.agentConfigOptions())
}

func (a *App) writeAgentConfigReport() error {
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	return a.writeAgentConfigSnapshot(snapshot)
}

func (a *App) writeAgentConfigSnapshot(snapshot agentconfig.Snapshot) error {
	return writeJSON(a.Stdout, agentConfigReport{
		Path: snapshot.Path, Exists: snapshot.Exists, Revision: snapshot.Revision,
		Supervisor: snapshot.Config.Supervisor,
		Defaults:   snapshot.Config.Defaults, Agents: snapshot.Config.Agents,
	})
}

func agentConfigSaveError(err error) error {
	if errors.Is(err, agentconfig.ErrRevisionConflict) {
		return fmt.Errorf(
			"global agent configuration changed while this command was running; rerun the command: %w",
			err,
		)
	}
	return err
}

func (a *App) saveAgentConfig(
	expected agentconfig.Revision,
	config agentconfig.Config,
) error {
	snapshot, err := agentconfig.CompareAndSwap(
		a.agentConfigOptions(), expected, config,
	)
	if err != nil {
		return agentConfigSaveError(err)
	}
	return a.writeAgentConfigSnapshot(snapshot)
}

func (a *App) setAgent(opts options, values []string) error {
	if len(values) != 1 {
		return errors.New("agents set requires exactly one agent id")
	}
	if !opts.present("runtime") {
		return errors.New("agents set requires --runtime codex, claude, cline, or gemini")
	}
	runtime, err := workerRuntime(opts.value("runtime"))
	if err != nil {
		return err
	}
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	config := snapshot.Config
	id := strings.TrimSpace(values[0])
	agent, found := config.Find(id)
	previousRuntime := agent.Runtime
	if !found {
		agent = agentconfig.Agent{
			ID: id, Runtime: runtime, Command: string(runtime), Enabled: true,
			MaxConcurrent: 1, Roles: []agentconfig.Role{agentconfig.RoleWorker},
		}
	}
	agent.Runtime = runtime
	if opts.present("command") {
		agent.Command = strings.TrimSpace(opts.value("command"))
	} else if !found || agent.Command == string(previousRuntime) {
		agent.Command = string(runtime)
	}
	if opts.present("model") {
		agent.Model = strings.TrimSpace(opts.value("model"))
	}
	if opts.present("provider") {
		agent.Provider = strings.TrimSpace(opts.value("provider"))
	}
	if opts.present("roles") {
		agent.Roles = roleOptions(opts.many("roles"))
		if len(agent.Roles) == 0 {
			return errors.New("--roles requires worker, planner, coordinator, and/or judge")
		}
	}
	if opts.present("fallbacks") {
		agent.Fallbacks = commaOptions(opts.many("fallbacks"))
	}
	if opts.present("max-concurrent") {
		agent.MaxConcurrent, err = numberOption(opts.value("max-concurrent"), 1)
		if err != nil {
			return err
		}
		if agent.MaxConcurrent < 1 {
			return errors.New("--max-concurrent must be at least 1")
		}
	}
	if found {
		for index := range config.Agents {
			if config.Agents[index].ID == id {
				config.Agents[index] = agent
				break
			}
		}
	} else {
		config.Agents = append(config.Agents, agent)
	}
	return a.saveAgentConfig(snapshot.Revision, config)
}

func (a *App) setAgentEnabled(action string, values []string) error {
	if len(values) != 1 {
		return fmt.Errorf("agents %s requires exactly one agent id", action)
	}
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	config := snapshot.Config
	id := strings.TrimSpace(values[0])
	if _, found := config.Find(id); !found {
		return fmt.Errorf("agent %q is not configured", id)
	}
	for index := range config.Agents {
		if config.Agents[index].ID == id {
			config.Agents[index].Enabled = action == "enable"
			break
		}
	}
	return a.saveAgentConfig(snapshot.Revision, config)
}

func (a *App) removeAgent(values []string) error {
	if len(values) != 1 {
		return errors.New("agents remove requires exactly one agent id")
	}
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	config := snapshot.Config
	id := strings.TrimSpace(values[0])
	if _, found := config.Find(id); !found {
		return fmt.Errorf("agent %q is not configured", id)
	}
	agents := make([]agentconfig.Agent, 0, len(config.Agents)-1)
	for _, agent := range config.Agents {
		if agent.ID == id {
			continue
		}
		agent.Fallbacks = withoutString(agent.Fallbacks, id)
		agents = append(agents, agent)
	}
	config.Agents = agents
	config.Defaults.WorkerAgents = withoutString(config.Defaults.WorkerAgents, id)
	config.Defaults.PlannerAgents = withoutString(config.Defaults.PlannerAgents, id)
	config.Defaults.CoordinatorAgents = withoutString(config.Defaults.CoordinatorAgents, id)
	config.Defaults.JudgeAgents = withoutString(config.Defaults.JudgeAgents, id)
	return a.saveAgentConfig(snapshot.Revision, config)
}

func (a *App) setAgentDefaults(opts options) error {
	if !opts.present("worker") && !opts.present("planner") && !opts.present("coordinator") && !opts.present("judge") {
		return errors.New("agents defaults requires --worker, --planner, --coordinator, or --judge")
	}
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	config := snapshot.Config
	if opts.present("worker") {
		config.Defaults.WorkerAgents = commaOptions(opts.many("worker"))
	}
	if opts.present("planner") {
		config.Defaults.PlannerAgents = commaOptions(opts.many("planner"))
	}
	if opts.present("coordinator") {
		config.Defaults.CoordinatorAgents = commaOptions(opts.many("coordinator"))
	}
	if opts.present("judge") {
		config.Defaults.JudgeAgents = commaOptions(opts.many("judge"))
	}
	return a.saveAgentConfig(snapshot.Revision, config)
}

func (a *App) setAgentSupervisor(opts options) error {
	if !opts.present("auto-start") && !opts.present("max-workers") && !opts.present("allow-writes") {
		return errors.New("agents supervisor requires --auto-start, --max-workers, or --allow-writes")
	}
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	config := snapshot.Config
	if opts.present("auto-start") {
		config.Supervisor.AutoStart = opts.flags["auto-start"]
	}
	if opts.present("allow-writes") {
		config.Supervisor.AllowWrites = opts.flags["allow-writes"]
	}
	if opts.present("max-workers") {
		config.Supervisor.MaxWorkers, err = numberOption(opts.value("max-workers"), 1)
		if err != nil {
			return err
		}
		if config.Supervisor.MaxWorkers < 1 {
			return errors.New("--max-workers must be at least 1")
		}
	}
	return a.saveAgentConfig(snapshot.Revision, config)
}

func (a *App) detectAgents(ctx context.Context, save bool) error {
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	path, config := snapshot.Path, snapshot.Config
	commonDetections, err := a.detectSupportedAgents(ctx, config)
	if err != nil {
		return err
	}
	detections := make([]agentDetection, 0, len(commonDetections))
	for _, common := range commonDetections {
		detection := agentDetection{
			ID: common.ID, Runtime: common.Runtime, Executable: common.Executable,
			Version: common.Version, State: common.State, Configured: common.Configured,
			Message: common.Message,
		}
		if save && !detection.Configured && detection.State != "missing" {
			config.Agents = append(config.Agents, agentconfig.Agent{
				ID: detection.ID, Runtime: detection.Runtime, Command: detection.Executable, Enabled: true, MaxConcurrent: 1,
				Roles: []agentconfig.Role{agentconfig.RoleWorker, agentconfig.RolePlanner, agentconfig.RoleCoordinator, agentconfig.RoleJudge},
			})
			detection.Configured = true
			detection.Saved = true
		}
		detections = append(detections, detection)
	}
	if save {
		if _, err := agentconfig.CompareAndSwap(
			a.agentConfigOptions(), snapshot.Revision, config,
		); err != nil {
			return agentConfigSaveError(err)
		}
	}
	return writeJSON(a.Stdout, agentDetectionReport{Path: path, Saved: save, Agents: detections})
}

func (a *App) detectSupportedAgents(ctx context.Context, config agentconfig.Config) ([]agentconfig.Detection, error) {
	runner := a.CommandRunner
	if runner == nil {
		return agentconfig.DetectSupportedAgents(ctx, config, agentconfig.DetectOptions{})
	}
	boundedRunner, ok := runner.(setupcfg.BoundedCommandRunner)
	if !ok {
		return nil, errors.New("injected agent detection runner must enforce bounded command output")
	}
	directory, err := a.workingDirectory()
	if err != nil {
		return nil, err
	}
	return agentconfig.DetectSupportedAgents(ctx, config, agentconfig.DetectOptions{
		LookPath: runner.LookPath,
		RunVersion: func(ctx context.Context, executable string) (string, string, error) {
			output, runErr := boundedRunner.RunBounded(
				ctx, directory, executable,
				agentconfig.MaxDetectionOutputBytes,
				agentconfig.MaxDetectionOutputBytes,
				"--version",
			)
			return output.Stdout, output.Stderr, runErr
		},
	})
}

func (a *App) previewAgentPreset(ctx context.Context, id string, apply, replace bool) error {
	snapshot, err := a.loadAgentConfigSnapshot()
	if err != nil {
		return err
	}
	path, exists, config := snapshot.Path, snapshot.Exists, snapshot.Config
	preset, found := agentconfig.FindPreset(id)
	if !found {
		return fmt.Errorf("unknown agent preset %q", strings.TrimSpace(id))
	}
	detections, err := a.detectSupportedAgents(ctx, config)
	if err != nil {
		return err
	}
	preview, err := agentconfig.ApplyPreset(config, preset.ID, agentconfig.PresetApplyOptions{
		Detections: detections, ReplaceExisting: replace,
	})
	if err != nil {
		return err
	}
	if apply {
		if _, err := agentconfig.CompareAndSwap(
			a.agentConfigOptions(), snapshot.Revision, preview,
		); err != nil {
			return agentConfigSaveError(err)
		}
		exists = true
	}
	return writeJSON(a.Stdout, agentPresetReport{
		Path: path, Exists: exists, Preset: preset, Applied: apply, Replaced: replace,
		Detections: detections, Config: preview,
	})
}

func workerRuntime(value string) (model.Runtime, error) {
	runtime := model.Runtime(strings.ToLower(strings.TrimSpace(value)))
	for _, candidate := range model.WorkerRuntimes {
		if runtime == candidate {
			return runtime, nil
		}
	}
	return "", fmt.Errorf("unsupported worker runtime %q; use codex, claude, cline, or gemini", value)
}

func commaOptions(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			result = append(result, item)
		}
	}
	if result == nil {
		return []string{}
	}
	return result
}

func roleOptions(values []string) []agentconfig.Role {
	items := commaOptions(values)
	result := make([]agentconfig.Role, 0, len(items))
	for _, item := range items {
		result = append(result, agentconfig.Role(strings.ToLower(item)))
	}
	return result
}

func withoutString(values []string, removed string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

func rejectAgentOptions(opts options, allowedNames ...string) error {
	allowed := make(map[string]bool, len(allowedNames))
	for _, name := range allowedNames {
		allowed[name] = true
	}
	for name := range opts.values {
		if !allowed[name] {
			return fmt.Errorf("unknown agents option --%s", name)
		}
	}
	for name := range opts.flagSet {
		if !allowed[name] {
			return fmt.Errorf("unknown agents option --%s", name)
		}
	}
	return nil
}
