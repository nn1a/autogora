package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	portable "github.com/nn1a/autogora/skills"
)

const skillManifestName = ".autogora-install.json"

type SkillScope string

const (
	SkillScopeUser    SkillScope = "user"
	SkillScopeProject SkillScope = "project"
)

type SkillTarget struct {
	Clients []string `json:"clients"`
	Scope   string   `json:"scope"`
	Path    string   `json:"path"`
}

type SkillResult struct {
	Skill   string   `json:"skill"`
	Clients []string `json:"clients"`
	Scope   string   `json:"scope"`
	Path    string   `json:"path"`
	State   string   `json:"state"`
	Changed bool     `json:"changed"`
	Message string   `json:"message,omitempty"`
}

type SkillOptions struct {
	Clients     []string
	Scope       SkillScope
	Home        string
	ProjectRoot string
	Version     string
	Force       bool
	DryRun      bool
}

type skillManifest struct {
	Schema  int               `json:"schema"`
	Skill   string            `json:"skill"`
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

type embeddedSkill struct {
	Name  string
	Files map[string][]byte
	Hash  map[string]string
}

func DiscoverProjectRoot(start string) (string, error) {
	if strings.TrimSpace(start) == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Abs(start)
		}
		current = parent
	}
}

func SkillTargets(options SkillOptions) ([]SkillTarget, error) {
	if len(options.Clients) == 0 {
		return nil, errors.New("at least one --client is required")
	}
	if options.Scope == "" {
		options.Scope = SkillScopeProject
	}
	if options.Scope != SkillScopeUser && options.Scope != SkillScopeProject {
		return nil, fmt.Errorf("invalid skill scope %q: use user or project", options.Scope)
	}

	clients := normalizeClients(options.Clients)
	if slices.Contains(clients, "all") {
		clients = []string{"codex", "claude", "gemini"}
	}
	for _, client := range clients {
		if client == "cline" {
			return nil, errors.New("the configured Cline runtime uses Autogora's scoped CLI bridge; Skill installation is not supported")
		}
		if client != "codex" && client != "claude" && client != "gemini" {
			return nil, fmt.Errorf("unsupported skill client %q", client)
		}
	}

	root := options.ProjectRoot
	if options.Scope == SkillScopeUser {
		root = options.Home
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%s root is empty", options.Scope)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*SkillTarget{}
	for _, client := range clients {
		relative := filepath.Join(".agents", "skills")
		if client == "claude" {
			relative = filepath.Join(".claude", "skills")
		}
		path := filepath.Join(root, relative)
		target := byPath[path]
		if target == nil {
			target = &SkillTarget{Scope: string(options.Scope), Path: path}
			byPath[path] = target
		}
		target.Clients = append(target.Clients, client)
	}
	targets := make([]SkillTarget, 0, len(byPath))
	for _, target := range byPath {
		sort.Strings(target.Clients)
		targets = append(targets, *target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Path < targets[j].Path })
	return targets, nil
}

func SkillStatus(options SkillOptions) ([]SkillResult, error) {
	return visitSkills(options, "status")
}

func InstallSkills(options SkillOptions) ([]SkillResult, error) {
	return visitSkills(options, "install")
}

func UninstallSkills(options SkillOptions) ([]SkillResult, error) {
	return visitSkills(options, "uninstall")
}

func visitSkills(options SkillOptions, action string) ([]SkillResult, error) {
	targets, err := SkillTargets(options)
	if err != nil {
		return nil, err
	}
	assets, err := embeddedSkills()
	if err != nil {
		return nil, err
	}
	results := make([]SkillResult, 0, len(targets)*len(assets))
	for _, target := range targets {
		for _, asset := range assets {
			var result SkillResult
			switch action {
			case "status":
				result, err = inspectSkill(target, asset, options.Version)
			case "install":
				result, err = installSkill(target, asset, options)
			case "uninstall":
				result, err = uninstallSkill(target, asset, options)
			default:
				err = fmt.Errorf("unsupported skill action %q", action)
			}
			if err != nil {
				return results, err
			}
			results = append(results, result)
		}
	}
	return results, nil
}

func embeddedSkills() ([]embeddedSkill, error) {
	result := make([]embeddedSkill, 0, len(portable.Names))
	for _, name := range portable.Names {
		asset := embeddedSkill{Name: name, Files: map[string][]byte{}, Hash: map[string]string{}}
		err := fs.WalkDir(portable.Files, name, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			contents, err := portable.Files.ReadFile(path)
			if err != nil {
				return err
			}
			relative := strings.TrimPrefix(path, name+"/")
			asset.Files[relative] = contents
			asset.Hash[relative] = hash(contents)
			return nil
		})
		if err != nil {
			return nil, err
		}
		result = append(result, asset)
	}
	return result, nil
}

func inspectSkill(target SkillTarget, asset embeddedSkill, version string) (SkillResult, error) {
	path := filepath.Join(target.Path, asset.Name)
	result := SkillResult{Skill: asset.Name, Clients: target.Clients, Scope: target.Scope, Path: path, State: "missing"}
	manifest, err := readSkillManifest(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(path); statErr == nil {
			result.State = "unmanaged"
			result.Message = "destination exists without an Autogora installation manifest"
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return result, statErr
		}
		return result, nil
	}
	if err != nil {
		result.State = "modified"
		result.Message = err.Error()
		return result, nil
	}
	if manifest.Skill != asset.Name || manifest.Schema != 1 {
		result.State = "modified"
		result.Message = "installation manifest does not match the embedded skill"
		return result, nil
	}
	for relative, expected := range manifest.Files {
		managed, pathErr := managedSkillPath(path, relative)
		if pathErr != nil {
			result.State = "modified"
			result.Message = pathErr.Error()
			return result, nil
		}
		contents, readErr := os.ReadFile(managed)
		if readErr != nil || hash(contents) != expected {
			result.State = "modified"
			result.Message = "installed files were changed after installation"
			return result, nil
		}
	}
	if manifest.Version != version || !mapsEqual(manifest.Files, asset.Hash) {
		result.State = "outdated"
		result.Message = "installed skill differs from this Autogora binary"
		return result, nil
	}
	result.State = "installed"
	return result, nil
}

func installSkill(target SkillTarget, asset embeddedSkill, options SkillOptions) (SkillResult, error) {
	status, err := inspectSkill(target, asset, options.Version)
	if err != nil {
		return status, err
	}
	if status.State == "installed" {
		status.Message = "already installed"
		return status, nil
	}
	if (status.State == "modified" || status.State == "unmanaged") && !options.Force {
		return status, fmt.Errorf("refusing to overwrite %s at %s; inspect it or retry with --force", status.State, status.Path)
	}
	status.Changed = true
	status.State = "installed"
	if options.DryRun {
		status.Message = "would install"
		return status, nil
	}

	oldManifest, _ := readSkillManifest(status.Path)
	if err := os.MkdirAll(status.Path, 0o755); err != nil {
		return status, err
	}
	for relative, contents := range asset.Files {
		managed, pathErr := managedSkillPath(status.Path, relative)
		if pathErr != nil {
			return status, pathErr
		}
		if err := writeAtomic(managed, contents, 0o644); err != nil {
			return status, err
		}
	}
	for relative, expected := range oldManifest.Files {
		if _, retained := asset.Files[relative]; retained {
			continue
		}
		stalePath, pathErr := managedSkillPath(status.Path, relative)
		if pathErr != nil {
			return status, pathErr
		}
		contents, readErr := os.ReadFile(stalePath)
		if readErr == nil && (options.Force || hash(contents) == expected) {
			_ = os.Remove(stalePath)
		}
	}
	manifest := skillManifest{Schema: 1, Skill: asset.Name, Version: options.Version, Files: asset.Hash}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return status, err
	}
	encoded = append(encoded, '\n')
	if err := writeAtomic(filepath.Join(status.Path, skillManifestName), encoded, 0o644); err != nil {
		return status, err
	}
	status.Message = "installed"
	return status, nil
}

func uninstallSkill(target SkillTarget, asset embeddedSkill, options SkillOptions) (SkillResult, error) {
	status, err := inspectSkill(target, asset, options.Version)
	if err != nil {
		return status, err
	}
	if status.State == "missing" {
		status.Message = "not installed"
		return status, nil
	}
	manifest, manifestErr := readSkillManifest(status.Path)
	if manifestErr != nil {
		return status, fmt.Errorf("refusing to remove an unmanaged skill at %s", status.Path)
	}
	if status.State == "modified" && !options.Force {
		return status, fmt.Errorf("refusing to remove modified skill at %s; inspect it or retry with --force", status.Path)
	}
	status.Changed = true
	status.State = "missing"
	if options.DryRun {
		status.Message = "would uninstall managed files"
		return status, nil
	}
	for relative := range manifest.Files {
		managedPath, pathErr := managedSkillPath(status.Path, relative)
		if pathErr != nil {
			return status, pathErr
		}
		if err := os.Remove(managedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return status, err
		}
		removeEmptyParents(filepath.Dir(managedPath), status.Path)
	}
	if err := os.Remove(filepath.Join(status.Path, skillManifestName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	removeEmptyParents(status.Path, target.Path)
	status.Message = "uninstalled managed files"
	return status, nil
}

func readSkillManifest(skillPath string) (skillManifest, error) {
	contents, err := os.ReadFile(filepath.Join(skillPath, skillManifestName))
	if err != nil {
		return skillManifest{}, err
	}
	var manifest skillManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return skillManifest{}, fmt.Errorf("invalid Autogora skill manifest: %w", err)
	}
	return manifest, nil
}

func writeAtomic(path string, contents []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".autogora-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err == nil {
		return nil
	}
	// Windows cannot replace an existing destination with os.Rename. The
	// fallback keeps the temporary file on the same volume and replaces only
	// the exact managed path.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func managedSkillPath(root, relative string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid managed skill path %q", relative)
	}
	return filepath.Join(root, cleaned), nil
}

func removeEmptyParents(start, stop string) {
	current := start
	for {
		if err := os.Remove(current); err != nil {
			return
		}
		if current == stop {
			return
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(parent+string(filepath.Separator), stop+string(filepath.Separator)) {
			return
		}
		current = parent
	}
}

func normalizeClients(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		for _, client := range strings.Split(value, ",") {
			client = strings.ToLower(strings.TrimSpace(client))
			if client != "" && !seen[client] {
				seen[client] = true
				result = append(result, client)
			}
		}
	}
	sort.Strings(result)
	return result
}

func hash(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
