package projectdata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	ProductDirectory = "autogora"
	DatabaseName     = "autogora.db"
)

type Options struct {
	WorkingDirectory string
	Getenv           func(string) string
	GOOS             string
	HomeDirectory    string
}

type Project struct {
	Root      string `json:"root"`
	CommonDir string `json:"gitCommonDir,omitempty"`
	ID        string `json:"id"`
}

type Location struct {
	Project     Project `json:"project"`
	AppDataRoot string  `json:"appDataRoot"`
	DataRoot    string  `json:"dataRoot"`
	DBPath      string  `json:"dbPath"`
	Source      string  `json:"source"`
	LocatorPath string  `json:"-"`
}

type persistedLocation struct {
	Schema   int    `json:"schema"`
	Project  string `json:"project"`
	DataRoot string `json:"dataRoot"`
}

func Resolve(options Options) (Location, error) {
	project, err := DiscoverProject(options.WorkingDirectory)
	if err != nil {
		return Location{}, err
	}
	appRoot, err := appDataRoot(options)
	if err != nil {
		return Location{}, err
	}
	location := defaultLocation(project, appRoot)
	persisted, err := readPersisted(location.LocatorPath)
	if errors.Is(err, os.ErrNotExist) {
		return location, nil
	}
	if err != nil {
		return Location{}, fmt.Errorf("read project data location: %w", err)
	}
	if persisted.Schema != 1 || persisted.Project != project.ID || strings.TrimSpace(persisted.DataRoot) == "" {
		return Location{}, fmt.Errorf("invalid project data location file: %s", location.LocatorPath)
	}
	dataRoot, err := filepath.Abs(persisted.DataRoot)
	if err != nil {
		return Location{}, err
	}
	if err := validateDataRoot(project, dataRoot); err != nil {
		return Location{}, fmt.Errorf("invalid persisted data root: %w", err)
	}
	location.DataRoot = dataRoot
	location.DBPath = filepath.Join(dataRoot, DatabaseName)
	location.Source = "project_override"
	return location, nil
}

func Default(options Options) (Location, error) {
	project, err := DiscoverProject(options.WorkingDirectory)
	if err != nil {
		return Location{}, err
	}
	appRoot, err := appDataRoot(options)
	if err != nil {
		return Location{}, err
	}
	return defaultLocation(project, appRoot), nil
}

func Configure(options Options, value string) (Location, error) {
	location, err := Default(options)
	if err != nil {
		return Location{}, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return Location{}, errors.New("data directory cannot be empty")
	}
	dataRoot := value
	if !filepath.IsAbs(dataRoot) {
		dataRoot = filepath.Join(location.Project.Root, dataRoot)
	}
	dataRoot, err = filepath.Abs(dataRoot)
	if err != nil {
		return Location{}, err
	}
	if err := validateDataRoot(location.Project, dataRoot); err != nil {
		return Location{}, err
	}
	location.DataRoot = dataRoot
	location.DBPath = filepath.Join(dataRoot, DatabaseName)
	location.Source = "project_override"
	if samePath(dataRoot, filepath.Join(location.AppDataRoot, "projects", location.Project.ID)) {
		location.Source = "project_default"
		if err := removeLocator(location.LocatorPath); err != nil {
			return Location{}, err
		}
		return location, nil
	}
	persisted := persistedLocation{Schema: 1, Project: location.Project.ID, DataRoot: dataRoot}
	if err := writeJSONAtomic(location.LocatorPath, persisted); err != nil {
		return Location{}, fmt.Errorf("save project data location: %w", err)
	}
	return location, nil
}

func Reset(options Options) (Location, error) {
	location, err := Default(options)
	if err != nil {
		return Location{}, err
	}
	if err := removeLocator(location.LocatorPath); err != nil {
		return Location{}, err
	}
	return location, nil
}

func DiscoverProject(start string) (Project, error) {
	if strings.TrimSpace(start) == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return Project{}, err
		}
	}
	start, err := canonicalExistingPath(start)
	if err != nil {
		return Project{}, err
	}
	root, commonDir, gitErr := discoverGit(start)
	if gitErr != nil {
		root = findRepositoryRoot(start)
		commonDir = ""
		if fileExists(filepath.Join(root, ".git")) {
			commonDir = filepath.Join(root, ".git")
		}
	}
	root, err = canonicalExistingPath(root)
	if err != nil {
		return Project{}, err
	}
	identity := root
	if commonDir != "" {
		commonDir, err = canonicalPath(commonDir)
		if err != nil {
			return Project{}, err
		}
		identity = commonDir
	}
	name := filepath.Base(root)
	if filepath.Base(commonDir) == ".git" {
		name = filepath.Base(filepath.Dir(commonDir))
	}
	id := projectID(name, identity, runtime.GOOS)
	return Project{Root: root, CommonDir: commonDir, ID: id}, nil
}

func defaultLocation(project Project, appRoot string) Location {
	dataRoot := filepath.Join(appRoot, "projects", project.ID)
	return Location{
		Project: project, AppDataRoot: appRoot, DataRoot: dataRoot,
		DBPath: filepath.Join(dataRoot, DatabaseName), Source: "project_default",
		LocatorPath: filepath.Join(appRoot, "locations", project.ID+".json"),
	}
}

func appDataRoot(options Options) (string, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if explicit := strings.TrimSpace(getenv("AUTOGORA_DATA_HOME")); explicit != "" {
		if !filepath.IsAbs(explicit) {
			return "", errors.New("AUTOGORA_DATA_HOME must be an absolute path")
		}
		return filepath.Clean(explicit), nil
	}
	goos := options.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	home := strings.TrimSpace(options.HomeDirectory)
	if home == "" {
		home = strings.TrimSpace(getenv("HOME"))
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}
	var base string
	switch goos {
	case "windows":
		base = strings.TrimSpace(getenv("LOCALAPPDATA"))
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support")
	default:
		base = strings.TrimSpace(getenv("XDG_DATA_HOME"))
		if !filepath.IsAbs(base) {
			base = filepath.Join(home, ".local", "share")
		}
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, ProductDirectory), nil
}

func discoverGit(start string) (string, string, error) {
	rootOutput, err := exec.Command("git", "-C", start, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", "", err
	}
	root := strings.TrimSpace(string(rootOutput))
	commonOutput, err := exec.Command("git", "-C", start, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", "", err
	}
	commonDir := strings.TrimSpace(string(commonOutput))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(start, commonDir)
	}
	return root, commonDir, nil
}

func findRepositoryRoot(start string) string {
	current := start
	for {
		if fileExists(filepath.Join(current, ".git")) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return start
		}
		current = parent
	}
}

func projectID(name, identity, goos string) string {
	identity = filepath.Clean(identity)
	if goos == "windows" {
		identity = strings.ToLower(identity)
	}
	digest := sha256.Sum256([]byte(identity))
	return slug(name) + "-" + hex.EncodeToString(digest[:])[:12]
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	lastDash := false
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if valid {
			result.WriteRune(character)
			lastDash = false
		} else if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	cleaned := strings.Trim(result.String(), "-")
	if cleaned == "" {
		cleaned = "project"
	}
	if len(cleaned) > 40 {
		cleaned = strings.TrimRight(cleaned[:40], "-")
	}
	return cleaned
}

func validateDataRoot(project Project, dataRoot string) error {
	dataRoot = filepath.Clean(dataRoot)
	if samePath(dataRoot, project.Root) {
		return errors.New("data directory cannot be the project root")
	}
	gitPath := filepath.Join(project.Root, ".git")
	if within(dataRoot, gitPath) || project.CommonDir != "" && within(dataRoot, project.CommonDir) {
		return errors.New("data directory cannot be inside Git's internal directory")
	}
	return nil
}

func within(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func canonicalExistingPath(path string) (string, error) {
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if evaluated, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		return evaluated, nil
	}
	return filepath.Clean(absolute), nil
}

func readPersisted(path string) (persistedLocation, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return persistedLocation{}, err
	}
	var value persistedLocation
	if err := json.Unmarshal(contents, &value); err != nil {
		return persistedLocation{}, err
	}
	return value, nil
}

func writeJSONAtomic(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".autogora-location-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err == nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temporaryName, path)
}

func removeLocator(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
