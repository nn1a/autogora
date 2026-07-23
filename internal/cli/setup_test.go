package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	setupcfg "github.com/nn1a/autogora/internal/setup"
)

type cliSetupRunner struct {
	calls [][]string
}

func (runner *cliSetupRunner) LookPath(file string) (string, error) {
	return "/clients/" + file, nil
}

func (runner *cliSetupRunner) Run(_ context.Context, _ string, file string, args ...string) (setupcfg.CommandOutput, error) {
	runner.calls = append(runner.calls, append([]string{file}, args...))
	if file == "/clients/gemini" {
		return setupcfg.CommandOutput{Stdout: "No MCP servers configured.\n"}, nil
	}
	return setupcfg.CommandOutput{Stderr: "No MCP server named 'autogora' found.\n"}, os.ErrNotExist
}

func TestSkillsCommandInstallsAndRemovesBundledSkills(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = filepath.Join(project, "nested")
	app.Version = "test-version"
	if err := os.MkdirAll(app.Cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	app.Getenv = func(name string) string {
		if name == "HOME" {
			return t.TempDir()
		}
		return ""
	}

	installed := runApp(t, app, "skills", "install", "--client", "codex", "--scope", "project")
	if !strings.Contains(installed, `"state": "installed"`) || !strings.Contains(installed, filepath.Join(project, ".agents", "skills")) {
		t.Fatalf("install output = %s", installed)
	}
	status := runApp(t, app, "skills", "status", "--client", "codex", "--scope", "project")
	if strings.Count(status, `"state": "installed"`) != 2 {
		t.Fatalf("status output = %s", status)
	}
	removed := runApp(t, app, "skills", "uninstall", "--client", "codex", "--scope", "project")
	if strings.Count(removed, `"state": "missing"`) != 2 {
		t.Fatalf("uninstall output = %s", removed)
	}
}

func TestSkillsCommandRejectsClineBridgeRuntime(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = t.TempDir()
	app.Getenv = func(string) string { return "" }
	err := app.Run(context.Background(), []string{"skills", "install", "--client", "cline"})
	if err == nil || !strings.Contains(err.Error(), "scoped CLI bridge") {
		t.Fatalf("cline error = %v", err)
	}
}

func TestMCPAndCombinedSetupCommands(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &cliSetupRunner{}
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = project
	app.Version = "test-version"
	app.CommandRunner = runner
	app.Getenv = func(name string) string {
		if name == "HOME" {
			return filepath.Join(project, "home")
		}
		return ""
	}

	mcp := runApp(t, app, "mcp", "register", "--client", "codex", "--binary", "/opt/autogora", "--db", filepath.Join(project, "autogora.db"), "--dry-run")
	if !strings.Contains(mcp, `"client": "codex"`) || !strings.Contains(mcp, `"message": "would register"`) {
		t.Fatalf("mcp dry-run output = %s", mcp)
	}
	combined := runApp(t, app, "setup", "--client", "codex", "--binary", "/opt/autogora", "--db", filepath.Join(project, "autogora.db"), "--dry-run")
	if !strings.Contains(combined, `"skills"`) || !strings.Contains(combined, `"mcp"`) || strings.Count(combined, `"message": "would install"`) != 2 {
		t.Fatalf("combined setup output = %s", combined)
	}
	if _, err := os.Stat(filepath.Join(project, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("combined dry-run wrote skills: %v", err)
	}
}

func TestSetupCommandHelp(t *testing.T) {
	for _, command := range []string{"init", "paths", "github", "dashboard", "skills", "mcp", "setup"} {
		app := New(&bytes.Buffer{}, &bytes.Buffer{})
		output := runApp(t, app, command, "--help")
		if !strings.HasPrefix(output, "autogora "+command) {
			t.Fatalf("%s help output = %q", command, output)
		}

		app = New(&bytes.Buffer{}, &bytes.Buffer{})
		output = runApp(t, app, "help", command)
		if !strings.HasPrefix(output, "autogora "+command) {
			t.Fatalf("help %s output = %q", command, output)
		}
	}
}
