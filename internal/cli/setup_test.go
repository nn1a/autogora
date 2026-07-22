package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
