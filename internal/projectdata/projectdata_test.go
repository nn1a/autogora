package projectdata

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppDataRootUsesNativeConventions(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "users", "tester")
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{name: "linux xdg", goos: "linux", env: map[string]string{"XDG_DATA_HOME": "/xdg/data"}, want: "/xdg/data/autogora"},
		{name: "linux fallback", goos: "linux", want: filepath.Join(home, ".local", "share", "autogora")},
		{name: "macos", goos: "darwin", want: filepath.Join(home, "Library", "Application Support", "autogora")},
		{name: "windows local app data", goos: "windows", env: map[string]string{"LOCALAPPDATA": filepath.Join(home, "Local")}, want: filepath.Join(home, "Local", "autogora")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, err := appDataRoot(Options{GOOS: test.goos, HomeDirectory: home, Getenv: func(name string) string { return test.env[name] }})
			if err != nil {
				t.Fatal(err)
			}
			if root != test.want {
				t.Fatalf("root = %q, want %q", root, test.want)
			}
		})
	}
}

func TestResolveCreatesStableProjectSpecificDefault(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(project, "src", "feature")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(t.TempDir(), "app-data")
	options := Options{WorkingDirectory: nested, Getenv: func(name string) string {
		if name == "AUTOGORA_DATA_HOME" {
			return dataHome
		}
		return ""
	}}
	location, err := Resolve(options)
	if err != nil {
		t.Fatal(err)
	}
	if location.Project.Root != project || !strings.HasPrefix(location.DataRoot, filepath.Join(dataHome, "projects")+string(filepath.Separator)) {
		t.Fatalf("location = %#v", location)
	}
	if location.DBPath != filepath.Join(location.DataRoot, DatabaseName) || location.Source != "project_default" {
		t.Fatalf("location = %#v", location)
	}
	fromRoot, err := Resolve(Options{WorkingDirectory: project, Getenv: options.Getenv})
	if err != nil {
		t.Fatal(err)
	}
	if fromRoot.DataRoot != location.DataRoot {
		t.Fatalf("subdirectory selected %q, root selected %q", location.DataRoot, fromRoot.DataRoot)
	}
}

func TestConfigurePersistsProjectOverrideAndReset(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(t.TempDir(), "app-data")
	options := Options{WorkingDirectory: project, Getenv: func(name string) string {
		if name == "AUTOGORA_DATA_HOME" {
			return dataHome
		}
		return ""
	}}
	configured, err := Configure(options, ".autogora")
	if err != nil {
		t.Fatal(err)
	}
	if configured.DataRoot != filepath.Join(project, ".autogora") {
		t.Fatalf("configured = %#v", configured)
	}
	resolved, err := Resolve(options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.DataRoot != configured.DataRoot || resolved.Source != "project_override" {
		t.Fatalf("resolved = %#v", resolved)
	}
	reset, err := Reset(options)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = Resolve(options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.DataRoot != reset.DataRoot || resolved.Source != "project_default" {
		t.Fatalf("reset resolution = %#v", resolved)
	}
}

func TestConfigureRejectsProjectRootAndGitInternals(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := Options{WorkingDirectory: project, Getenv: func(name string) string {
		if name == "AUTOGORA_DATA_HOME" {
			return filepath.Join(t.TempDir(), "data")
		}
		return ""
	}}
	if _, err := Configure(options, project); err == nil || !strings.Contains(err.Error(), "project root") {
		t.Fatalf("project root error = %v", err)
	}
	if _, err := Configure(options, filepath.Join(project, ".git", "autogora")); err == nil || !strings.Contains(err.Error(), "Git's internal") {
		t.Fatalf("Git directory error = %v", err)
	}
}

func TestLinkedWorktreesShareProjectIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(directory string, args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", directory}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit(project, "init", "-q")
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("worktree test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(project, "add", "README.md")
	runGit(project, "-c", "user.name=Autogora Test", "-c", "user.email=test@autogora.invalid", "commit", "-qm", "initial")
	linked := filepath.Join(parent, "linked")
	runGit(project, "worktree", "add", "-q", "-b", "linked-test", linked)

	mainProject, err := DiscoverProject(project)
	if err != nil {
		t.Fatal(err)
	}
	linkedProject, err := DiscoverProject(linked)
	if err != nil {
		t.Fatal(err)
	}
	if mainProject.ID != linkedProject.ID || mainProject.CommonDir != linkedProject.CommonDir {
		t.Fatalf("main = %#v, linked = %#v", mainProject, linkedProject)
	}
}
