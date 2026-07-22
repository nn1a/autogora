package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillTargetsDeduplicateInteroperableAgentPath(t *testing.T) {
	root := t.TempDir()
	targets, err := SkillTargets(SkillOptions{Clients: []string{"codex", "gemini", "claude"}, Scope: SkillScopeProject, ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %#v", targets)
	}
	var shared *SkillTarget
	for index := range targets {
		if strings.HasSuffix(targets[index].Path, filepath.Join(".agents", "skills")) {
			shared = &targets[index]
		}
	}
	if shared == nil || strings.Join(shared.Clients, ",") != "codex,gemini" {
		t.Fatalf("interoperable target = %#v", shared)
	}
}

func TestSkillInstallStatusAndUninstall(t *testing.T) {
	root := t.TempDir()
	options := SkillOptions{Clients: []string{"all"}, Scope: SkillScopeProject, ProjectRoot: root, Version: "v1"}
	installed, err := InstallSkills(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 4 {
		t.Fatalf("install results = %#v", installed)
	}
	for _, result := range installed {
		if !result.Changed || result.State != "installed" {
			t.Fatalf("unexpected install result: %#v", result)
		}
		if _, err := os.Stat(filepath.Join(result.Path, "SKILL.md")); err != nil {
			t.Fatalf("installed SKILL.md: %v", err)
		}
	}

	statuses, err := SkillStatus(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range statuses {
		if result.State != "installed" || result.Changed {
			t.Fatalf("unexpected status result: %#v", result)
		}
	}

	removed, err := UninstallSkills(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range removed {
		if result.State != "missing" || !result.Changed {
			t.Fatalf("unexpected uninstall result: %#v", result)
		}
		if _, err := os.Stat(result.Path); !os.IsNotExist(err) {
			t.Fatalf("skill path still exists after uninstall: %s (%v)", result.Path, err)
		}
	}
}

func TestSkillLifecycleProtectsModifiedAndUnmanagedFiles(t *testing.T) {
	root := t.TempDir()
	options := SkillOptions{Clients: []string{"codex"}, Scope: SkillScopeProject, ProjectRoot: root, Version: "v1"}
	installed, err := InstallSkills(options)
	if err != nil {
		t.Fatal(err)
	}
	modified := installed[0]
	if err := os.WriteFile(filepath.Join(modified.Path, "SKILL.md"), []byte("user edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statuses, err := SkillStatus(options)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].State != "modified" {
		t.Fatalf("modified state = %#v", statuses[0])
	}
	if _, err := InstallSkills(options); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("modified install error = %v", err)
	}
	if _, err := UninstallSkills(options); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("modified uninstall error = %v", err)
	}

	unmanagedRoot := t.TempDir()
	unmanaged := filepath.Join(unmanagedRoot, ".claude", "skills", "autogora-worker")
	if err := os.MkdirAll(unmanaged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanaged, "SKILL.md"), []byte("owned by user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unmanagedOptions := SkillOptions{Clients: []string{"claude"}, Scope: SkillScopeProject, ProjectRoot: unmanagedRoot, Version: "v1"}
	if _, err := InstallSkills(unmanagedOptions); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("unmanaged install error = %v", err)
	}
	if _, err := UninstallSkills(unmanagedOptions); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("unmanaged uninstall error = %v", err)
	}
}

func TestSkillDryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	results, err := InstallSkills(SkillOptions{Clients: []string{"codex"}, Scope: SkillScopeUser, Home: root, Version: "v1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Changed || results[0].Message != "would install" {
		t.Fatalf("dry-run results = %#v", results)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote files: %v", err)
	}
}

func TestDiscoverProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "packages", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	discovered, err := DiscoverProjectRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if discovered != root {
		t.Fatalf("project root = %q, want %q", discovered, root)
	}
}

func TestSkillManifestCannotEscapeManagedDirectory(t *testing.T) {
	root := t.TempDir()
	skillPath := filepath.Join(root, ".agents", "skills", "autogora-worker")
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := skillManifest{Schema: 1, Skill: "autogora-worker", Version: "v1", Files: map[string]string{"../../outside": "bad"}}
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillPath, skillManifestName), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	options := SkillOptions{Clients: []string{"codex"}, Scope: SkillScopeProject, ProjectRoot: root, Version: "v1", Force: true}
	statuses, err := SkillStatus(options)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].State != "modified" || !strings.Contains(statuses[0].Message, "invalid managed skill path") {
		t.Fatalf("malicious manifest status = %#v", statuses[0])
	}
	if _, err := UninstallSkills(options); err == nil || !strings.Contains(err.Error(), "invalid managed skill path") {
		t.Fatalf("malicious manifest uninstall error = %v", err)
	}
}
