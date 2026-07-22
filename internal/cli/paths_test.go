package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newProjectDataTestApp(t *testing.T) (*App, string, string) {
	t.Helper()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(project, "src", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(t.TempDir(), "autogora-home")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = nested
	app.Getenv = func(name string) string {
		if name == "AUTOGORA_DATA_HOME" {
			return dataHome
		}
		return ""
	}
	return app, project, dataHome
}

func TestPathsAndInitUseExternalProjectDefault(t *testing.T) {
	app, project, dataHome := newProjectDataTestApp(t)
	output := runApp(t, app, "paths")
	var report pathReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if report.Project.Root != project || report.Source != "project_default" {
		t.Fatalf("paths report = %#v", report)
	}
	if !strings.HasPrefix(report.Database, filepath.Join(dataHome, "projects")+string(filepath.Separator)) {
		t.Fatalf("database = %q", report.Database)
	}
	if _, err := os.Stat(report.DataRoot); !os.IsNotExist(err) {
		t.Fatalf("paths created data root: %v", err)
	}

	initialized := strings.TrimSpace(runApp(t, app, "init"))
	if initialized != report.Database {
		t.Fatalf("init = %q, want %q", initialized, report.Database)
	}
	if _, err := os.Stat(initialized); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, "data")); !os.IsNotExist(err) {
		t.Fatalf("init created repository data directory: %v", err)
	}
}

func TestInitPersistsIgnoredRepositoryLocalOverrideAndReset(t *testing.T) {
	app, project, dataHome := newProjectDataTestApp(t)
	localDB := strings.TrimSpace(runApp(t, app, "init", "--data-dir", ".autogora"))
	if localDB != filepath.Join(project, ".autogora", "autogora.db") {
		t.Fatalf("local database = %q", localDB)
	}
	ignore, err := os.ReadFile(filepath.Join(project, ".autogora", ".gitignore"))
	if err != nil || string(ignore) != "*\n" {
		t.Fatalf("local ignore = %q, %v", ignore, err)
	}
	output := runApp(t, app, "paths")
	if !strings.Contains(output, `"source": "project_override"`) || !strings.Contains(output, localDB) {
		t.Fatalf("persisted paths = %s", output)
	}

	defaultDB := strings.TrimSpace(runApp(t, app, "init", "--reset-data-dir"))
	if !strings.HasPrefix(defaultDB, filepath.Join(dataHome, "projects")+string(filepath.Separator)) {
		t.Fatalf("reset database = %q", defaultDB)
	}
	output = runApp(t, app, "paths")
	if !strings.Contains(output, `"source": "project_default"`) || !strings.Contains(output, defaultDB) {
		t.Fatalf("reset paths = %s", output)
	}
}

func TestDatabaseSelectionPrecedenceAndRelativeExplicitPath(t *testing.T) {
	app, _, _ := newProjectDataTestApp(t)
	environmentDB := filepath.Join(t.TempDir(), "environment.db")
	baseGetenv := app.Getenv
	app.Getenv = func(name string) string {
		if name == "AUTOGORA_DB" {
			return environmentDB
		}
		return baseGetenv(name)
	}
	if got, err := app.databasePath(""); err != nil || got != environmentDB {
		t.Fatalf("environment database = %q, %v", got, err)
	}
	explicit, err := app.databasePath(filepath.Join("state", "explicit.db"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(app.Cwd, "state", "explicit.db")
	if explicit != want {
		t.Fatalf("explicit database = %q, want %q", explicit, want)
	}
	if err := app.Run(t.Context(), []string{"init", "--data-dir", ".autogora"}); err == nil || !strings.Contains(err.Error(), "AUTOGORA_DB") {
		t.Fatalf("data-dir conflict error = %v", err)
	}
}
