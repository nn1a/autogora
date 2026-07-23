package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	setupcfg "github.com/nn1a/autogora/internal/setup"
)

const skillsHelp = `autogora skills <install|status|uninstall> --client <name> [options]

Options:
  --client <name>       codex, claude, gemini, or all; repeatable
  --scope <scope>       project (default) or user
  --project-dir <path>  Project used for project-scoped installation
  --force               Replace or remove modified managed files
  --dry-run             Report changes without writing files
`

const mcpHelp = `autogora mcp <register|status|unregister> --client <name> [options]

Options:
  --client <name>       codex, claude, gemini, or all; repeatable
  --scope <scope>       Client scope; omitted uses a safe client default
  --db <path>           Database passed to the registered MCP server
  --binary <path>       Autogora executable to register (default: current executable)
  --project-dir <path>  Directory in which the client CLI runs
  --replace             Replace a conflicting Autogora registration
  --dry-run             Inspect and report without changing client configuration

Default scopes: Codex user, Claude local, Gemini project.
The configured MCP-disabled Cline runtime uses the scoped CLI bridge instead.
`

const combinedSetupHelp = `autogora setup --client <name> [options]

Installs the bundled worker/coordinator Skills and registers Autogora MCP.
Both destinations are preflighted before changes begin.

Options:
  --client <name>       codex, claude, gemini, or all; repeatable
  --skill-scope <scope> project (default) or user
  --mcp-scope <scope>   Client MCP scope; omitted uses a safe client default
  --scope <scope>       Shorthand applied to both scopes when supported
  --db <path>           Database passed to the registered MCP server
  --binary <path>       Autogora executable to register (default: current executable)
  --project-dir <path>  Project used for installation and client commands
  --force               Replace modified managed Skill files
  --replace             Replace a conflicting Autogora MCP registration
  --dry-run             Report all planned changes without applying them
`

func setupCommandHelp(command string) string {
	switch command {
	case "init":
		return initHelp
	case "paths":
		return pathsHelp
	case "github":
		return githubHelp
	case "agents":
		return agentsHelp
	case "dashboard":
		return dashboardHelp
	case "skills":
		return skillsHelp
	case "mcp":
		return mcpHelp
	case "setup":
		return combinedSetupHelp
	default:
		return ""
	}
}

func (a *App) runSkills(opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("skills requires install, status, or uninstall")
	}
	action := strings.ToLower(strings.TrimSpace(opts.positionals[0]))
	if action != "install" && action != "status" && action != "uninstall" {
		return fmt.Errorf("unknown skills action %q", action)
	}
	clients := opts.many("client")
	if len(clients) == 0 {
		return errors.New("skills requires --client codex, claude, gemini, or all")
	}
	scope := setupcfg.SkillScope(strings.ToLower(strings.TrimSpace(opts.value("scope"))))
	if scope == "" {
		scope = setupcfg.SkillScopeProject
	}

	projectStart := opts.value("project-dir")
	if projectStart == "" {
		var err error
		projectStart, err = a.workingDirectory()
		if err != nil {
			return err
		}
	}
	projectRoot, err := setupcfg.DiscoverProjectRoot(projectStart)
	if err != nil {
		return err
	}
	home := strings.TrimSpace(a.env("HOME", "USERPROFILE"))
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	options := setupcfg.SkillOptions{
		Clients: clients, Scope: scope, Home: home, ProjectRoot: projectRoot,
		Version: a.Version, Force: opts.flags["force"], DryRun: opts.flags["dry-run"],
	}
	var results []setupcfg.SkillResult
	switch action {
	case "install":
		results, err = setupcfg.InstallSkills(options)
	case "status":
		results, err = setupcfg.SkillStatus(options)
	case "uninstall":
		results, err = setupcfg.UninstallSkills(options)
	}
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, results)
}

func (a *App) runMCP(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("mcp requires register, status, or unregister")
	}
	action := strings.ToLower(strings.TrimSpace(opts.positionals[0]))
	if action != "register" && action != "status" && action != "unregister" {
		return fmt.Errorf("unknown mcp action %q", action)
	}
	options, err := a.mcpOptions(opts)
	if err != nil {
		return err
	}
	var results []setupcfg.MCPResult
	switch action {
	case "register":
		results, err = setupcfg.RegisterMCP(ctx, options)
	case "status":
		results, err = setupcfg.MCPStatus(ctx, options)
	case "unregister":
		results, err = setupcfg.UnregisterMCP(ctx, options)
	}
	if err != nil {
		return err
	}
	return writeJSON(a.Stdout, results)
}

func (a *App) runSetup(ctx context.Context, opts options) error {
	if len(opts.positionals) > 0 {
		return errors.New("setup does not accept an action; use --client to select integrations")
	}
	clients := opts.many("client")
	if len(clients) == 0 {
		return errors.New("setup requires --client codex, claude, gemini, or all")
	}
	projectRoot, home, err := a.setupRoots(opts)
	if err != nil {
		return err
	}
	skillScope := strings.ToLower(strings.TrimSpace(opts.value("skill-scope")))
	mcpScope := strings.ToLower(strings.TrimSpace(opts.value("mcp-scope")))
	if shared := strings.ToLower(strings.TrimSpace(opts.value("scope"))); shared != "" {
		if skillScope == "" {
			skillScope = shared
		}
		if mcpScope == "" {
			mcpScope = shared
		}
	}
	if skillScope == "" {
		skillScope = string(setupcfg.SkillScopeProject)
	}
	skillOptions := setupcfg.SkillOptions{
		Clients: clients, Scope: setupcfg.SkillScope(skillScope), Home: home, ProjectRoot: projectRoot,
		Version: a.Version, Force: opts.flags["force"], DryRun: true,
	}
	mcpOptions, err := a.mcpOptionsWithRoots(opts, clients, mcpScope, projectRoot)
	if err != nil {
		return err
	}
	mcpOptions.DryRun = true

	// Validate both destinations before changing either client configuration.
	skillResults, err := setupcfg.InstallSkills(skillOptions)
	if err != nil {
		return err
	}
	mcpResults, err := setupcfg.RegisterMCP(ctx, mcpOptions)
	if err != nil {
		return err
	}
	if !opts.flags["dry-run"] {
		skillOptions.DryRun = false
		skillResults, err = setupcfg.InstallSkills(skillOptions)
		if err != nil {
			return err
		}
		mcpOptions.DryRun = false
		mcpResults, err = setupcfg.RegisterMCP(ctx, mcpOptions)
		if err != nil {
			return err
		}
	}
	return writeJSON(a.Stdout, struct {
		Skills []setupcfg.SkillResult `json:"skills"`
		MCP    []setupcfg.MCPResult   `json:"mcp"`
	}{Skills: skillResults, MCP: mcpResults})
}

func (a *App) mcpOptions(opts options) (setupcfg.MCPOptions, error) {
	clients := opts.many("client")
	if len(clients) == 0 {
		return setupcfg.MCPOptions{}, errors.New("mcp requires --client codex, claude, gemini, or all")
	}
	projectRoot, _, err := a.setupRoots(opts)
	if err != nil {
		return setupcfg.MCPOptions{}, err
	}
	return a.mcpOptionsWithRoots(opts, clients, opts.value("scope"), projectRoot)
}

func (a *App) mcpOptionsWithRoots(opts options, clients []string, scope, projectRoot string) (setupcfg.MCPOptions, error) {
	dbPath, err := a.defaultDBPath()
	if err != nil {
		return setupcfg.MCPOptions{}, err
	}
	if value := strings.TrimSpace(opts.value("db")); value != "" {
		dbPath, err = filepath.Abs(value)
		if err != nil {
			return setupcfg.MCPOptions{}, err
		}
	}
	binaryPath := strings.TrimSpace(opts.value("binary"))
	if binaryPath == "" {
		binaryPath, err = os.Executable()
		if err != nil {
			return setupcfg.MCPOptions{}, err
		}
	}
	binaryPath, err = filepath.Abs(binaryPath)
	if err != nil {
		return setupcfg.MCPOptions{}, err
	}
	return setupcfg.MCPOptions{
		Clients: clients, Scope: scope, BinaryPath: binaryPath, DBPath: dbPath,
		ProjectRoot: projectRoot, Replace: opts.flags["replace"], DryRun: opts.flags["dry-run"], Runner: a.CommandRunner,
	}, nil
}

func (a *App) setupRoots(opts options) (string, string, error) {
	projectStart := opts.value("project-dir")
	if projectStart == "" {
		var err error
		projectStart, err = a.workingDirectory()
		if err != nil {
			return "", "", err
		}
	}
	projectRoot, err := setupcfg.DiscoverProjectRoot(projectStart)
	if err != nil {
		return "", "", err
	}
	home := strings.TrimSpace(a.env("HOME", "USERPROFILE"))
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
	}
	return projectRoot, home, nil
}
