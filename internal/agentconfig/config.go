package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

const (
	SchemaVersion = 1
	configName    = "config.json"
	productName   = "autogora"
)

type Role string

const (
	RoleWorker  Role = "worker"
	RolePlanner Role = "planner"
	RoleJudge   Role = "judge"
)

type Options struct {
	Getenv        func(string) string
	GOOS          string
	HomeDirectory string
}

type Config struct {
	SchemaVersion int        `json:"schemaVersion"`
	Supervisor    Supervisor `json:"supervisor"`
	Defaults      Defaults   `json:"defaults"`
	Agents        []Agent    `json:"agents"`
}

type Supervisor struct {
	AutoStart   bool `json:"autoStart"`
	MaxWorkers  int  `json:"maxWorkers"`
	AllowWrites bool `json:"allowWrites"`
}

type Defaults struct {
	WorkerAgents  []string `json:"workerAgents"`
	PlannerAgents []string `json:"plannerAgents"`
	JudgeAgents   []string `json:"judgeAgents"`
}

type Agent struct {
	ID            string        `json:"id"`
	Runtime       model.Runtime `json:"runtime"`
	Command       string        `json:"command"`
	Model         string        `json:"model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Enabled       bool          `json:"enabled"`
	MaxConcurrent int           `json:"maxConcurrent"`
	Roles         []Role        `json:"roles"`
	Fallbacks     []string      `json:"fallbacks,omitempty"`
}

var agentIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Supervisor:    Supervisor{MaxWorkers: 1},
		Defaults: Defaults{
			WorkerAgents:  []string{},
			PlannerAgents: []string{},
			JudgeAgents:   []string{},
		},
		Agents: []Agent{},
	}
}

func Path(options Options) (string, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if explicit := strings.TrimSpace(getenv("AUTOGORA_CONFIG")); explicit != "" {
		if !filepath.IsAbs(explicit) {
			return "", errors.New("AUTOGORA_CONFIG must be an absolute path")
		}
		return filepath.Clean(explicit), nil
	}
	if dataHome := strings.TrimSpace(getenv("AUTOGORA_DATA_HOME")); dataHome != "" {
		if !filepath.IsAbs(dataHome) {
			return "", errors.New("AUTOGORA_DATA_HOME must be an absolute path")
		}
		return filepath.Join(filepath.Clean(dataHome), configName), nil
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
			return "", fmt.Errorf("resolve user home directory: %w", err)
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
		return "", fmt.Errorf("resolve application data directory: %w", err)
	}
	return filepath.Join(base, productName, configName), nil
}

func Load(options Options) (Config, error) {
	path, err := Path(options)
	if err != nil {
		return Config{}, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read agent configuration %s: %w", path, err)
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode agent configuration %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return Config{}, fmt.Errorf("decode agent configuration %s: %w", path, err)
	}
	config = Normalize(config)
	if err := Validate(config); err != nil {
		return Config{}, fmt.Errorf("validate agent configuration %s: %w", path, err)
	}
	return config, nil
}

func Exists(options Options) (bool, error) {
	path, err := Path(options)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect agent configuration %s: %w", path, err)
}

func Save(options Options, config Config) error {
	path, err := Path(options)
	if err != nil {
		return err
	}
	config = Normalize(config)
	if err := Validate(config); err != nil {
		return fmt.Errorf("validate agent configuration: %w", err)
	}
	contents, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agent configuration: %w", err)
	}
	contents = append(contents, '\n')
	if err := writeAtomic(path, contents); err != nil {
		return fmt.Errorf("save agent configuration %s: %w", path, err)
	}
	return nil
}

func Normalize(config Config) Config {
	if config.SchemaVersion == 0 {
		config.SchemaVersion = SchemaVersion
	}
	if config.Supervisor.MaxWorkers < 1 {
		config.Supervisor.MaxWorkers = 1
	}
	config.Defaults.WorkerAgents = normalizedStrings(config.Defaults.WorkerAgents)
	config.Defaults.PlannerAgents = normalizedStrings(config.Defaults.PlannerAgents)
	config.Defaults.JudgeAgents = normalizedStrings(config.Defaults.JudgeAgents)
	if config.Agents == nil {
		config.Agents = []Agent{}
	}
	for index := range config.Agents {
		agent := &config.Agents[index]
		agent.ID = strings.TrimSpace(agent.ID)
		agent.Runtime = model.Runtime(strings.TrimSpace(string(agent.Runtime)))
		agent.Command = strings.TrimSpace(agent.Command)
		agent.Model = strings.TrimSpace(agent.Model)
		agent.Provider = strings.TrimSpace(agent.Provider)
		if agent.Command == "" {
			agent.Command = string(agent.Runtime)
		}
		if agent.MaxConcurrent < 1 {
			agent.MaxConcurrent = 1
		}
		agent.Roles = normalizedRoles(agent.Roles)
		if len(agent.Roles) == 0 {
			agent.Roles = []Role{RoleWorker}
		}
		agent.Fallbacks = normalizedStrings(agent.Fallbacks)
	}
	return config
}

func Validate(config Config) error {
	if config.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d; expected %d", config.SchemaVersion, SchemaVersion)
	}
	if config.Supervisor.MaxWorkers < 1 {
		return errors.New("supervisor.maxWorkers must be at least 1")
	}
	agents := make(map[string]Agent, len(config.Agents))
	for _, agent := range config.Agents {
		if !agentIDPattern.MatchString(agent.ID) {
			return fmt.Errorf("invalid agent id %q: use 1-64 lowercase alphanumerics and hyphens", agent.ID)
		}
		if _, exists := agents[agent.ID]; exists {
			return fmt.Errorf("duplicate agent id %q", agent.ID)
		}
		if !validWorkerRuntime(agent.Runtime) {
			return fmt.Errorf("agent %q has unsupported worker runtime %q", agent.ID, agent.Runtime)
		}
		if strings.TrimSpace(agent.Command) == "" {
			return fmt.Errorf("agent %q command cannot be empty", agent.ID)
		}
		if agent.MaxConcurrent < 1 {
			return fmt.Errorf("agent %q maxConcurrent must be at least 1", agent.ID)
		}
		if len(agent.Roles) == 0 {
			return fmt.Errorf("agent %q must have at least one role", agent.ID)
		}
		for _, role := range agent.Roles {
			if !validRole(role) {
				return fmt.Errorf("agent %q has unknown role %q", agent.ID, role)
			}
		}
		agents[agent.ID] = agent
	}
	if err := validateDefaultReferences(config.Defaults.WorkerAgents, RoleWorker, agents); err != nil {
		return err
	}
	if err := validateDefaultReferences(config.Defaults.PlannerAgents, RolePlanner, agents); err != nil {
		return err
	}
	if err := validateDefaultReferences(config.Defaults.JudgeAgents, RoleJudge, agents); err != nil {
		return err
	}
	for _, agent := range config.Agents {
		for _, fallback := range agent.Fallbacks {
			if fallback == agent.ID {
				return fmt.Errorf("agent %q cannot fall back to itself", agent.ID)
			}
			if _, exists := agents[fallback]; !exists {
				return fmt.Errorf("agent %q references unknown fallback %q", agent.ID, fallback)
			}
		}
	}
	if err := validateFallbackGraph(config.Agents); err != nil {
		return err
	}
	return nil
}

func (config Config) Find(id string) (Agent, bool) {
	id = strings.TrimSpace(id)
	for _, agent := range config.Agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return Agent{}, false
}

// Effective returns enabled agents that support role. Explicit defaults are
// returned first, followed by the remaining eligible agents in registry order.
func (config Config) Effective(role Role) []Agent {
	config = Normalize(config)
	defaultIDs := config.defaultIDs(role)
	result := make([]Agent, 0, len(config.Agents))
	seen := make(map[string]bool, len(config.Agents))
	appendAgent := func(id string) {
		if seen[id] {
			return
		}
		agent, found := config.Find(id)
		if !found || !agent.Enabled || !hasRole(agent, role) {
			return
		}
		seen[id] = true
		result = append(result, agent)
	}
	for _, id := range defaultIDs {
		appendAgent(id)
	}
	for _, agent := range config.Agents {
		appendAgent(agent.ID)
	}
	return result
}

func (config Config) defaultIDs(role Role) []string {
	switch role {
	case RoleWorker:
		return config.Defaults.WorkerAgents
	case RolePlanner:
		return config.Defaults.PlannerAgents
	case RoleJudge:
		return config.Defaults.JudgeAgents
	default:
		return nil
	}
}

func validWorkerRuntime(value model.Runtime) bool {
	return value == model.RuntimeClaude || value == model.RuntimeCodex || value == model.RuntimeCline || value == model.RuntimeGemini
}

func validRole(role Role) bool {
	return role == RoleWorker || role == RolePlanner || role == RoleJudge
}

func hasRole(agent Agent, role Role) bool {
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

func normalizedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	if result == nil {
		return []string{}
	}
	return result
}

func normalizedRoles(values []Role) []Role {
	result := make([]Role, 0, len(values))
	seen := make(map[Role]bool, len(values))
	for _, value := range values {
		value = Role(strings.TrimSpace(string(value)))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func validateDefaultReferences(ids []string, role Role, agents map[string]Agent) error {
	for _, id := range ids {
		agent, exists := agents[id]
		if !exists {
			return fmt.Errorf("defaults.%sAgents references unknown agent %q", role, id)
		}
		if !hasRole(agent, role) {
			return fmt.Errorf("defaults.%sAgents references agent %q without the %s role", role, id, role)
		}
	}
	return nil
}

func validateFallbackGraph(agents []Agent) error {
	edges := make(map[string][]string, len(agents))
	for _, agent := range agents {
		edges[agent.ID] = agent.Fallbacks
	}
	states := make(map[string]uint8, len(agents))
	stack := make([]string, 0, len(agents))
	var visit func(string) error
	visit = func(id string) error {
		switch states[id] {
		case 1:
			start := 0
			for index, candidate := range stack {
				if candidate == id {
					start = index
					break
				}
			}
			cycle := append(append([]string{}, stack[start:]...), id)
			return fmt.Errorf("fallback cycle: %s", strings.Join(cycle, " -> "))
		case 2:
			return nil
		}
		states[id] = 1
		stack = append(stack, id)
		for _, fallback := range edges[id] {
			if err := visit(fallback); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		states[id] = 2
		return nil
	}
	for _, agent := range agents {
		if err := visit(agent.ID); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, contents []byte) error {
	directory := filepath.Dir(path)
	_, statErr := os.Stat(directory)
	directoryMissing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !directoryMissing {
		return statErr
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if directoryMissing {
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	temporary, err := os.CreateTemp(directory, ".autogora-config-*")
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
	if err := replaceFile(temporaryName, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}
