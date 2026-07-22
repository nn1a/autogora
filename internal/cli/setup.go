package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	setupcfg "github.com/nn1a/autogora/internal/setup"
)

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
