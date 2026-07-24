package boards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
)

var boardSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var ErrBoardMutationInProgress = errors.New("board metadata mutation is in progress")
var ErrBoardSettingsConflict = errors.New("board orchestration settings changed")

type Profile struct {
	Name          string        `json:"name"`
	Runtime       model.Runtime `json:"runtime"`
	Model         string        `json:"model,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Description   string        `json:"description"`
	Disabled      bool          `json:"disabled,omitempty"`
	MaxConcurrent int           `json:"maxConcurrent,omitempty"`
	Priority      int           `json:"priority,omitempty"`
	Fallbacks     []string      `json:"fallbacks,omitempty"`
}

type CoordinationMode string

const (
	CoordinationModeObserve CoordinationMode = "observe"
	CoordinationModeAssist  CoordinationMode = "assist"
	CoordinationModeAuto    CoordinationMode = "auto"
)

const (
	MinCoordinationIdleSeconds        = 30
	MaxCoordinationCallsPerHour       = 100
	MaxCoordinationActionsPerIncident = 20
)

type PublicationMode string

const (
	PublicationModeManual      PublicationMode = "manual"
	PublicationModeLocalFF     PublicationMode = "local_ff"
	PublicationModePullRequest PublicationMode = "pull_request"
)

type CoordinationSettings struct {
	Mode                  CoordinationMode `json:"mode"`
	Profile               *string          `json:"profile"`
	IdleSeconds           int              `json:"idleSeconds"`
	MaxCallsPerHour       int              `json:"maxCallsPerHour"`
	MaxActionsPerIncident int              `json:"maxActionsPerIncident"`
}

type PublicationSettings struct {
	Mode            PublicationMode `json:"mode"`
	TargetBranch    string          `json:"targetBranch"`
	Remote          string          `json:"remote"`
	RequireApproval bool            `json:"requireApproval"`
}

type AutopilotSettings struct {
	Enabled         bool                 `json:"enabled"`
	AutoPlan        bool                 `json:"autoPlan"`
	AutoExecute     bool                 `json:"autoExecute"`
	WorkspaceWrites bool                 `json:"workspaceWrites"`
	Coordination    CoordinationSettings `json:"coordination"`
	Publication     PublicationSettings  `json:"publication"`
}

type OrchestrationSettings struct {
	AutoDecompose        bool              `json:"autoDecompose"`
	AutoDecomposePerTick int               `json:"autoDecomposePerTick"`
	AutoPromoteChildren  bool              `json:"autoPromoteChildren"`
	PlannerRuntime       model.Runtime     `json:"plannerRuntime"`
	PlannerModel         string            `json:"plannerModel,omitempty"`
	PlannerProvider      string            `json:"plannerProvider,omitempty"`
	DefaultProfile       *string           `json:"defaultProfile"`
	FinalizerProfile     *string           `json:"finalizerProfile"`
	Profiles             []Profile         `json:"profiles"`
	Autopilot            AutopilotSettings `json:"autopilot"`
}

type Metadata struct {
	Slug            string                   `json:"slug"`
	Name            string                   `json:"name"`
	Description     string                   `json:"description"`
	Icon            string                   `json:"icon"`
	Color           string                   `json:"color"`
	DefaultWorkdir  *string                  `json:"defaultWorkdir"`
	CreatedAt       *string                  `json:"createdAt"`
	Archived        bool                     `json:"archived"`
	DBPath          string                   `json:"dbPath"`
	WorkspaceRoot   string                   `json:"workspaceRoot"`
	AttachmentsRoot string                   `json:"attachmentsRoot"`
	LogsRoot        string                   `json:"logsRoot"`
	Orchestration   OrchestrationSettings    `json:"orchestration"`
	Counts          map[model.TaskStatus]int `json:"counts,omitempty"`
	listedIdentity  *listedBoardIdentity
}

type listedBoardIdentity struct {
	slug            string
	archived        bool
	dbPath          string
	dbInfo          os.FileInfo
	archiveRootPath string
	archiveRootInfo os.FileInfo
	directoryPath   string
	directoryInfo   os.FileInfo
	createdAt       string
	hasCreatedAt    bool
}

type Update struct {
	Name           *string
	Description    *string
	Icon           *string
	Color          *string
	DefaultWorkdir store.OptionalString
	Orchestration  *OrchestrationUpdate
}

type OrchestrationUpdate struct {
	AutoDecompose        *bool
	AutoDecomposePerTick *int
	AutoPromoteChildren  *bool
	PlannerRuntime       *model.Runtime
	PlannerModel         *string
	PlannerProvider      *string
	DefaultProfile       store.OptionalString
	FinalizerProfile     store.OptionalString
	Profiles             *[]Profile
	Autopilot            *AutopilotUpdate
}

type AutopilotUpdate struct {
	Enabled         *bool
	AutoPlan        *bool
	AutoExecute     *bool
	WorkspaceWrites *bool
	Coordination    *CoordinationUpdate
	Publication     *PublicationUpdate
}

type CoordinationUpdate struct {
	Mode                  *CoordinationMode
	Profile               store.OptionalString
	IdleSeconds           *int
	MaxCallsPerHour       *int
	MaxActionsPerIncident *int
}

type PublicationUpdate struct {
	Mode            *PublicationMode
	TargetBranch    *string
	Remote          *string
	RequireApproval *bool
}

type RemoveResult struct {
	Slug     string `json:"slug"`
	Archived bool   `json:"archived"`
	Path     string `json:"path"`
}

type Manager struct {
	defaultDBPath string
	home          string
	boardsRoot    string
	currentPath   string
}

func (m *Manager) withBoardMutation(
	ctx context.Context,
	slug string,
	remove bool,
	mutate func(*store.Store) error,
) (err error) {
	metadataLock, acquired, lockErr := acquireBoardMutationLock(m.boardMetadataLockPath(slug), true)
	if lockErr != nil {
		return fmt.Errorf("lock board %s metadata: %w", slug, lockErr)
	}
	if !acquired {
		return fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug)
	}
	lifecycleLock, acquired, lifecycleErr := acquireBoardMutationLock(m.boardLifecycleLockPath(slug), remove)
	if lifecycleErr != nil || !acquired {
		closeErr := metadataLock.Close()
		if lifecycleErr != nil {
			return errors.Join(fmt.Errorf("lock board %s lifecycle: %w", slug, lifecycleErr), closeErr)
		}
		if remove {
			return errors.Join(fmt.Errorf("%w: %s has open stores", store.ErrBoardBusy, slug), closeErr)
		}
		return errors.Join(fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug), closeErr)
	}

	var coordination *store.Store
	if slug == "default" {
		coordination, err = m.openStoreUnlocked(ctx, "default")
	} else {
		coordination, err = m.OpenCoordinationStore(ctx)
	}
	if err != nil {
		return errors.Join(
			fmt.Errorf("open coordination store for board %s: %w", slug, err),
			lifecycleLock.Close(),
			metadataLock.Close(),
		)
	}
	defer func() {
		err = errors.Join(err, coordination.Close(), lifecycleLock.Close(), metadataLock.Close())
	}()
	if slug != "default" && m.Exists(slug) {
		if err := m.clearStaleRemovalGuards(ctx, slug, coordination); err != nil {
			return err
		}
	}
	return mutate(coordination)
}

func NewManager(defaultDBPath string) (*Manager, error) {
	resolved, err := filepath.Abs(defaultDBPath)
	if err != nil {
		return nil, err
	}
	home := filepath.Dir(resolved)
	return &Manager{defaultDBPath: resolved, home: home, boardsRoot: filepath.Join(home, "boards"), currentPath: filepath.Join(home, "current")}, nil
}

func (m *Manager) boardMetadataLockPath(slug string) string {
	return filepath.Join(m.home, ".locks", "boards", slug+".metadata.lock")
}

func (m *Manager) boardLifecycleLockPath(slug string) string {
	return filepath.Join(m.home, ".locks", "boards", slug+".lifecycle.lock")
}

// archivedRecoveryLockPath coordinates every manager-owned archive mutation
// with exact archived readers/writers. Future archive GC/import operations
// must take this lock exclusively before changing the archive root.
func (m *Manager) archivedRecoveryLockPath() string {
	return filepath.Join(
		m.home,
		".locks",
		"boards",
		"_archived.recovery.lock",
	)
}

func (m *Manager) currentLockPath() string {
	return filepath.Join(m.home, ".locks", "current.lock")
}

func NormalizeSlug(value string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(value))
	if !boardSlug.MatchString(slug) {
		return "", fmt.Errorf("invalid board slug %q: use 1-64 lowercase alphanumerics, hyphens, or underscores", value)
	}
	return slug, nil
}

func defaultName(slug string) string {
	parts := strings.FieldsFunc(strings.ReplaceAll(slug, "_", "-"), func(r rune) bool { return r == '-' })
	for index, part := range parts {
		if part != "" {
			parts[index] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, " ")
}

func defaultOrchestration() OrchestrationSettings {
	return OrchestrationSettings{AutoDecompose: true, AutoDecomposePerTick: 3, AutoPromoteChildren: true,
		PlannerRuntime: model.RuntimeCodex, Profiles: []Profile{}, Autopilot: defaultAutopilot()}
}

func defaultAutopilot() AutopilotSettings {
	return AutopilotSettings{
		Enabled: true, AutoPlan: true, AutoExecute: true,
		Coordination: CoordinationSettings{
			Mode: CoordinationModeObserve, IdleSeconds: 300,
			MaxCallsPerHour: 4, MaxActionsPerIncident: 3,
		},
		Publication: PublicationSettings{
			Mode: PublicationModeManual, TargetBranch: "main",
			Remote: "origin", RequireApproval: true,
		},
	}
}

func validWorkerRuntime(runtime model.Runtime) bool {
	return runtime == model.RuntimeClaude || runtime == model.RuntimeCodex || runtime == model.RuntimeCline || runtime == model.RuntimeGemini
}

func normalizeOrchestration(value OrchestrationSettings) OrchestrationSettings {
	if value.Autopilot.Coordination.Mode == "" && value.Autopilot.Publication.Mode == "" {
		value.Autopilot = defaultAutopilot()
	}
	if value.AutoDecomposePerTick < 1 {
		value.AutoDecomposePerTick = 3
	}
	if !validWorkerRuntime(value.PlannerRuntime) {
		value.PlannerRuntime = model.RuntimeCodex
	}
	profiles := make([]Profile, 0, len(value.Profiles))
	seen := map[string]bool{}
	for _, profile := range value.Profiles {
		profile.Name = strings.TrimSpace(profile.Name)
		profile.Model = strings.TrimSpace(profile.Model)
		profile.Provider = strings.TrimSpace(profile.Provider)
		profile.Description = strings.TrimSpace(profile.Description)
		if profile.MaxConcurrent < 0 {
			profile.MaxConcurrent = 0
		}
		fallbacks := make([]string, 0, len(profile.Fallbacks))
		fallbackSeen := map[string]bool{}
		for _, fallback := range profile.Fallbacks {
			fallback = strings.TrimSpace(fallback)
			if fallback != "" && fallback != profile.Name && !fallbackSeen[fallback] {
				fallbackSeen[fallback] = true
				fallbacks = append(fallbacks, fallback)
			}
		}
		profile.Fallbacks = fallbacks
		if profile.Name != "" && validWorkerRuntime(profile.Runtime) && !seen[profile.Name] {
			seen[profile.Name] = true
			profiles = append(profiles, profile)
		}
	}
	value.Profiles = profiles
	value.Autopilot = normalizeAutopilot(value.Autopilot)
	return value
}

func normalizeAutopilot(value AutopilotSettings) AutopilotSettings {
	defaults := defaultAutopilot()
	switch value.Coordination.Mode {
	case CoordinationModeObserve, CoordinationModeAssist, CoordinationModeAuto:
	default:
		value.Coordination.Mode = defaults.Coordination.Mode
	}
	if value.Coordination.Profile != nil {
		profile := strings.TrimSpace(*value.Coordination.Profile)
		if profile == "" {
			value.Coordination.Profile = nil
		} else {
			value.Coordination.Profile = &profile
		}
	}
	if value.Coordination.IdleSeconds < MinCoordinationIdleSeconds {
		value.Coordination.IdleSeconds = defaults.Coordination.IdleSeconds
	}
	if value.Coordination.MaxCallsPerHour < 1 {
		value.Coordination.MaxCallsPerHour = defaults.Coordination.MaxCallsPerHour
	} else if value.Coordination.MaxCallsPerHour > MaxCoordinationCallsPerHour {
		value.Coordination.MaxCallsPerHour = MaxCoordinationCallsPerHour
	}
	if value.Coordination.MaxActionsPerIncident < 1 {
		value.Coordination.MaxActionsPerIncident = defaults.Coordination.MaxActionsPerIncident
	} else if value.Coordination.MaxActionsPerIncident > MaxCoordinationActionsPerIncident {
		value.Coordination.MaxActionsPerIncident = MaxCoordinationActionsPerIncident
	}
	switch value.Publication.Mode {
	case PublicationModeManual, PublicationModeLocalFF, PublicationModePullRequest:
	default:
		value.Publication.Mode = defaults.Publication.Mode
	}
	value.Publication.TargetBranch = strings.TrimSpace(value.Publication.TargetBranch)
	if value.Publication.TargetBranch == "" {
		value.Publication.TargetBranch = defaults.Publication.TargetBranch
	}
	value.Publication.Remote = strings.TrimSpace(value.Publication.Remote)
	if value.Publication.Remote == "" {
		value.Publication.Remote = defaults.Publication.Remote
	}
	return value
}

// ValidateOrchestrationUpdate validates the mutable orchestration values before
// normalization can replace an invalid value with a default. Interactive
// clients use this together with CompareAndSwapOrchestration so every settings
// surface observes the same bounds.
func ValidateOrchestrationUpdate(update *OrchestrationUpdate) error {
	if update == nil {
		return nil
	}
	if update.AutoDecomposePerTick != nil &&
		(*update.AutoDecomposePerTick < 1 || *update.AutoDecomposePerTick > 100) {
		return errors.New("autoDecomposePerTick must be between 1 and 100")
	}
	if update.PlannerRuntime != nil && !validWorkerRuntime(*update.PlannerRuntime) {
		return fmt.Errorf("invalid planner runtime: %s", *update.PlannerRuntime)
	}
	if update.Profiles != nil {
		if len(*update.Profiles) > 200 {
			return errors.New("profiles cannot exceed 200 entries")
		}
		seen := make(map[string]bool, len(*update.Profiles))
		for _, profile := range *update.Profiles {
			name := strings.TrimSpace(profile.Name)
			if name == "" || !validWorkerRuntime(profile.Runtime) {
				return errors.New("profile requires a name and worker runtime")
			}
			if seen[name] {
				return fmt.Errorf("duplicate profile name: %s", name)
			}
			seen[name] = true
			if profile.MaxConcurrent < 0 {
				return fmt.Errorf("profile %s maxConcurrent cannot be negative", name)
			}
			fallbacks := map[string]bool{}
			for _, fallback := range profile.Fallbacks {
				fallback = strings.TrimSpace(fallback)
				if fallback == "" {
					return fmt.Errorf("profile %s has an empty fallback", name)
				}
				if fallback == name {
					return fmt.Errorf("profile %s cannot fall back to itself", name)
				}
				if fallbacks[fallback] {
					return fmt.Errorf("profile %s repeats fallback %s", name, fallback)
				}
				fallbacks[fallback] = true
			}
		}
	}
	autopilot := update.Autopilot
	if autopilot == nil {
		return nil
	}
	if coordination := autopilot.Coordination; coordination != nil {
		if coordination.Mode != nil && *coordination.Mode != CoordinationModeObserve &&
			*coordination.Mode != CoordinationModeAssist && *coordination.Mode != CoordinationModeAuto {
			return errors.New("coordination mode must be observe, assist, or auto")
		}
		if coordination.IdleSeconds != nil && *coordination.IdleSeconds < MinCoordinationIdleSeconds {
			return fmt.Errorf("coordination idleSeconds must be at least %d", MinCoordinationIdleSeconds)
		}
		if coordination.MaxCallsPerHour != nil &&
			(*coordination.MaxCallsPerHour < 1 || *coordination.MaxCallsPerHour > MaxCoordinationCallsPerHour) {
			return fmt.Errorf("coordination maxCallsPerHour must be between 1 and %d", MaxCoordinationCallsPerHour)
		}
		if coordination.MaxActionsPerIncident != nil &&
			(*coordination.MaxActionsPerIncident < 1 || *coordination.MaxActionsPerIncident > MaxCoordinationActionsPerIncident) {
			return fmt.Errorf("coordination maxActionsPerIncident must be between 1 and %d", MaxCoordinationActionsPerIncident)
		}
	}
	if publication := autopilot.Publication; publication != nil && publication.Mode != nil &&
		*publication.Mode != PublicationModeManual && *publication.Mode != PublicationModeLocalFF &&
		*publication.Mode != PublicationModePullRequest {
		return errors.New("publication mode must be manual, local_ff, or pull_request")
	}
	return nil
}

func (m *Manager) BoardDir(board string) (string, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return "", err
	}
	return filepath.Join(m.boardsRoot, slug), nil
}
func (m *Manager) DBPath(board string) (string, error) {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return "", err
	}
	if slug == "default" {
		return m.defaultDBPath, nil
	}
	directory, _ := m.BoardDir(slug)
	return filepath.Join(directory, "autogora.db"), nil
}
func (m *Manager) WorkspaceRoot(board string) (string, error) {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return "", err
	}
	if slug == "default" {
		return filepath.Join(m.home, "workspaces"), nil
	}
	directory, _ := m.BoardDir(slug)
	return filepath.Join(directory, "workspaces"), nil
}
func (m *Manager) AttachmentsRoot(board string) (string, error) {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return "", err
	}
	if slug == "default" {
		return filepath.Join(m.home, "attachments"), nil
	}
	directory, _ := m.BoardDir(slug)
	return filepath.Join(directory, "attachments"), nil
}
func (m *Manager) LogsRoot(board string) (string, error) {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return "", err
	}
	if slug == "default" {
		return filepath.Join(m.home, "logs"), nil
	}
	directory, _ := m.BoardDir(slug)
	return filepath.Join(directory, "logs"), nil
}
func (m *Manager) metadataPath(board string) (string, error) {
	directory, err := m.BoardDir(defaultBoard(board))
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "board.json"), nil
}
func defaultBoard(value string) string {
	if strings.TrimSpace(value) == "" {
		return "default"
	}
	return value
}

func (m *Manager) Exists(board string) bool {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return false
	}
	if slug == "default" {
		return true
	}
	dbPath, _ := m.DBPath(slug)
	metadataPath, _ := m.metadataPath(slug)
	return fileExists(dbPath) || fileExists(metadataPath)
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

type persistedMetadata struct {
	Slug           string                `json:"slug"`
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	Icon           string                `json:"icon"`
	Color          string                `json:"color"`
	DefaultWorkdir *string               `json:"defaultWorkdir"`
	CreatedAt      *string               `json:"createdAt"`
	Archived       bool                  `json:"archived"`
	Orchestration  OrchestrationSettings `json:"orchestration"`
}

func readPersisted(path string) persistedMetadata {
	contents, err := os.ReadFile(path)
	if err != nil {
		return persistedMetadata{}
	}
	var value persistedMetadata
	if json.Unmarshal(contents, &value) != nil {
		return persistedMetadata{}
	}
	return value
}

func (m *Manager) Read(board string) (Metadata, error) {
	slug, err := NormalizeSlug(defaultBoard(board))
	if err != nil {
		return Metadata{}, err
	}
	metadataPath, _ := m.metadataPath(slug)
	raw := readPersisted(metadataPath)
	dbPath, _ := m.DBPath(slug)
	workspaceRoot, _ := m.WorkspaceRoot(slug)
	attachmentsRoot, _ := m.AttachmentsRoot(slug)
	logsRoot, _ := m.LogsRoot(slug)
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		name = defaultName(slug)
	}
	orchestration := raw.Orchestration
	if orchestration.PlannerRuntime == "" && orchestration.AutoDecomposePerTick == 0 && orchestration.Profiles == nil &&
		orchestration.DefaultProfile == nil && orchestration.FinalizerProfile == nil {
		orchestration = defaultOrchestration()
	} else {
		orchestration = normalizeOrchestration(orchestration)
	}
	return Metadata{Slug: slug, Name: name, Description: raw.Description, Icon: raw.Icon, Color: raw.Color,
		DefaultWorkdir: raw.DefaultWorkdir, CreatedAt: raw.CreatedAt, Archived: raw.Archived,
		DBPath: dbPath, WorkspaceRoot: workspaceRoot, AttachmentsRoot: attachmentsRoot,
		LogsRoot: logsRoot, Orchestration: orchestration}, nil
}

func applyOrchestration(current OrchestrationSettings, update *OrchestrationUpdate) OrchestrationSettings {
	if update == nil {
		return current
	}
	if update.AutoDecompose != nil {
		current.AutoDecompose = *update.AutoDecompose
	}
	if update.AutoDecomposePerTick != nil {
		current.AutoDecomposePerTick = *update.AutoDecomposePerTick
	}
	if update.AutoPromoteChildren != nil {
		current.AutoPromoteChildren = *update.AutoPromoteChildren
	}
	if update.PlannerRuntime != nil {
		current.PlannerRuntime = *update.PlannerRuntime
	}
	if update.PlannerModel != nil {
		current.PlannerModel = strings.TrimSpace(*update.PlannerModel)
	}
	if update.PlannerProvider != nil {
		current.PlannerProvider = strings.TrimSpace(*update.PlannerProvider)
	}
	if update.DefaultProfile.Set {
		current.DefaultProfile = update.DefaultProfile.Value
	}
	if update.FinalizerProfile.Set {
		current.FinalizerProfile = update.FinalizerProfile.Value
	}
	if update.Profiles != nil {
		current.Profiles = *update.Profiles
	}
	current.Autopilot = applyAutopilot(current.Autopilot, update.Autopilot)
	return normalizeOrchestration(current)
}

func applyAutopilot(current AutopilotSettings, update *AutopilotUpdate) AutopilotSettings {
	if update == nil {
		return current
	}
	if update.Enabled != nil {
		current.Enabled = *update.Enabled
	}
	if update.AutoPlan != nil {
		current.AutoPlan = *update.AutoPlan
	}
	if update.AutoExecute != nil {
		current.AutoExecute = *update.AutoExecute
	}
	if update.WorkspaceWrites != nil {
		current.WorkspaceWrites = *update.WorkspaceWrites
	}
	if coordination := update.Coordination; coordination != nil {
		if coordination.Mode != nil {
			current.Coordination.Mode = *coordination.Mode
		}
		if coordination.Profile.Set {
			current.Coordination.Profile = coordination.Profile.Value
		}
		if coordination.IdleSeconds != nil {
			current.Coordination.IdleSeconds = *coordination.IdleSeconds
		}
		if coordination.MaxCallsPerHour != nil {
			current.Coordination.MaxCallsPerHour = *coordination.MaxCallsPerHour
		}
		if coordination.MaxActionsPerIncident != nil {
			current.Coordination.MaxActionsPerIncident = *coordination.MaxActionsPerIncident
		}
	}
	if publication := update.Publication; publication != nil {
		if publication.Mode != nil {
			current.Publication.Mode = *publication.Mode
		}
		if publication.TargetBranch != nil {
			current.Publication.TargetBranch = *publication.TargetBranch
		}
		if publication.Remote != nil {
			current.Publication.Remote = *publication.Remote
		}
		if publication.RequireApproval != nil {
			current.Publication.RequireApproval = *publication.RequireApproval
		}
	}
	return normalizeAutopilot(current)
}

func (m *Manager) write(board string, update Update, archived *bool) (Metadata, error) {
	metadata, err := m.Read(board)
	if err != nil {
		return Metadata{}, err
	}
	if update.Name != nil && strings.TrimSpace(*update.Name) != "" {
		metadata.Name = strings.TrimSpace(*update.Name)
	}
	if update.Description != nil {
		metadata.Description = *update.Description
	}
	if update.Icon != nil {
		metadata.Icon = *update.Icon
	}
	if update.Color != nil {
		metadata.Color = *update.Color
	}
	if update.DefaultWorkdir.Set {
		metadata.DefaultWorkdir = update.DefaultWorkdir.Value
	}
	metadata.Orchestration = applyOrchestration(metadata.Orchestration, update.Orchestration)
	if archived != nil {
		metadata.Archived = *archived
	}
	if metadata.CreatedAt == nil {
		value := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		metadata.CreatedAt = &value
	}
	persisted := persistedMetadata{Slug: metadata.Slug, Name: metadata.Name, Description: metadata.Description,
		Icon: metadata.Icon, Color: metadata.Color, DefaultWorkdir: metadata.DefaultWorkdir,
		CreatedAt: metadata.CreatedAt, Archived: metadata.Archived, Orchestration: metadata.Orchestration}
	contents, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return Metadata{}, err
	}
	contents = append(contents, '\n')
	path, _ := m.metadataPath(metadata.Slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Metadata{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".board-*.json")
	if err != nil {
		return Metadata{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return Metadata{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Metadata{}, err
	}
	if err := temporary.Close(); err != nil {
		return Metadata{}, err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) Create(ctx context.Context, board string, update Update) (Metadata, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	err = m.withBoardMutation(ctx, slug, false, func(coordination *store.Store) error {
		var createErr error
		metadata, createErr = m.create(ctx, slug, update, coordination)
		return createErr
	})
	return metadata, err
}

func (m *Manager) create(
	ctx context.Context,
	slug string,
	update Update,
	coordination *store.Store,
) (Metadata, error) {
	metadata, err := m.write(slug, update, nil)
	if err != nil {
		return Metadata{}, err
	}
	opened, err := m.openStoreUnlocked(ctx, slug)
	if err != nil {
		return Metadata{}, err
	}
	if err := opened.Close(); err != nil {
		return Metadata{}, err
	}
	for _, path := range []string{metadata.WorkspaceRoot, metadata.AttachmentsRoot, metadata.LogsRoot} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return Metadata{}, err
		}
	}
	if slug != "default" {
		if err := coordination.ClearBoardRemovalTombstone(ctx, slug); err != nil {
			return Metadata{}, fmt.Errorf("clear removal tombstone for recreated board %s: %w", slug, err)
		}
	}
	return metadata, nil
}

func (m *Manager) Update(board string, update Update) (Metadata, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	err = m.withBoardMutation(context.Background(), slug, false, func(_ *store.Store) error {
		var updateErr error
		metadata, updateErr = m.update(slug, update)
		return updateErr
	})
	return metadata, err
}

// CompareAndSwapOrchestration updates one board only when its normalized
// orchestration settings still match the snapshot shown to the caller. The
// board metadata lock makes the comparison and write one serialized mutation.
func (m *Manager) CompareAndSwapOrchestration(
	ctx context.Context,
	board string,
	expected OrchestrationSettings,
	update OrchestrationUpdate,
) (Metadata, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return Metadata{}, err
	}
	if err := ValidateOrchestrationUpdate(&update); err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	err = m.withBoardMutation(ctx, slug, false, func(_ *store.Store) error {
		current, readErr := m.Read(slug)
		if readErr != nil {
			return readErr
		}
		if !reflect.DeepEqual(current.Orchestration, normalizeOrchestration(expected)) {
			return fmt.Errorf("%w: %s", ErrBoardSettingsConflict, slug)
		}
		var updateErr error
		metadata, updateErr = m.update(slug, Update{Orchestration: &update})
		return updateErr
	})
	return metadata, err
}

func (m *Manager) update(slug string, update Update) (Metadata, error) {
	if !m.Exists(slug) {
		return Metadata{}, fmt.Errorf("board not found: %s", slug)
	}
	return m.write(slug, update, nil)
}

func attachListedBoardIdentity(
	metadata *Metadata,
	archivedDirectory string,
) error {
	if metadata == nil {
		return errors.New("listed board metadata is required")
	}
	slug, err := NormalizeSlug(metadata.Slug)
	if err != nil {
		return err
	}
	dbPath, err := filepath.Abs(strings.TrimSpace(metadata.DBPath))
	if err != nil || strings.TrimSpace(metadata.DBPath) == "" {
		return errors.New("listed board database path is required")
	}
	dbInfo, err := os.Lstat(dbPath)
	if err != nil {
		return fmt.Errorf("inspect listed board %s database: %w", slug, err)
	}
	if !dbInfo.Mode().IsRegular() ||
		dbInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"listed board %s database must be a regular file",
			slug,
		)
	}
	identity := &listedBoardIdentity{
		slug: slug, archived: metadata.Archived,
		dbPath: dbPath, dbInfo: dbInfo,
	}
	if metadata.CreatedAt != nil {
		identity.createdAt = *metadata.CreatedAt
		identity.hasCreatedAt = true
	}
	if metadata.Archived {
		directoryPath, err := filepath.Abs(archivedDirectory)
		if err != nil || strings.TrimSpace(archivedDirectory) == "" {
			return errors.New("listed archived board directory is required")
		}
		archiveRootPath := filepath.Dir(directoryPath)
		archiveRootInfo, err := os.Lstat(archiveRootPath)
		if err != nil {
			return fmt.Errorf(
				"inspect listed archived board root for %s: %w",
				slug,
				err,
			)
		}
		if !archiveRootInfo.IsDir() ||
			archiveRootInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"listed archived board root for %s must be a real directory",
				slug,
			)
		}
		directoryInfo, err := os.Lstat(directoryPath)
		if err != nil {
			return fmt.Errorf(
				"inspect listed archived board %s directory: %w",
				slug,
				err,
			)
		}
		if !directoryInfo.IsDir() ||
			directoryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"listed archived board %s directory must be a real directory",
				slug,
			)
		}
		identity.archiveRootPath = archiveRootPath
		identity.archiveRootInfo = archiveRootInfo
		identity.directoryPath = directoryPath
		identity.directoryInfo = directoryInfo
	}
	metadata.DBPath = dbPath
	metadata.listedIdentity = identity
	return nil
}

func listedCreatedAtMatches(
	identity *listedBoardIdentity,
	createdAt *string,
) bool {
	if identity == nil {
		return false
	}
	if createdAt == nil {
		return !identity.hasCreatedAt
	}
	return identity.hasCreatedAt && identity.createdAt == *createdAt
}

func (m *Manager) listActiveMetadata(
	ctx context.Context,
	slug string,
) (metadata Metadata, err error) {
	metadataLock, acquired, lockErr := acquireBoardMutationLock(
		m.boardMetadataLockPath(slug),
		false,
	)
	if lockErr != nil {
		return Metadata{}, fmt.Errorf(
			"lock board %s metadata for recovery inventory: %w",
			slug,
			lockErr,
		)
	}
	if !acquired {
		return Metadata{}, fmt.Errorf(
			"%w: %s",
			ErrBoardMutationInProgress,
			slug,
		)
	}
	lifecycleLock, acquired, lifecycleErr := acquireBoardMutationLock(
		m.boardLifecycleLockPath(slug),
		false,
	)
	if lifecycleErr != nil || !acquired {
		closeErr := metadataLock.Close()
		if lifecycleErr != nil {
			return Metadata{}, errors.Join(
				fmt.Errorf(
					"lock board %s lifecycle for recovery inventory: %w",
					slug,
					lifecycleErr,
				),
				closeErr,
			)
		}
		return Metadata{}, errors.Join(
			fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug),
			closeErr,
		)
	}
	defer func() {
		err = errors.Join(
			err,
			lifecycleLock.Close(),
			metadataLock.Close(),
		)
	}()
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	metadata, err = m.Read(slug)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.Archived {
		return Metadata{}, fmt.Errorf(
			"active recovery inventory found archived metadata for board %s",
			slug,
		)
	}
	if err := attachListedBoardIdentity(&metadata, ""); err != nil {
		return Metadata{}, err
	}
	current, err := m.Read(slug)
	if err != nil {
		return Metadata{}, err
	}
	if current.Archived ||
		current.DBPath != metadata.DBPath ||
		!listedCreatedAtMatches(metadata.listedIdentity, current.CreatedAt) {
		return Metadata{}, fmt.Errorf(
			"board %s changed during recovery inventory",
			slug,
		)
	}
	return metadata, nil
}

// ListMetadata returns active and, when requested, archived board locations
// without opening their databases. Callers that inspect archived databases
// must pass these values to OpenListedPublicationRecoveryReader so the path
// remains constrained to the manager-owned archive root.
func (m *Manager) ListMetadata(
	ctx context.Context,
	includeArchived bool,
) ([]Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slugs := map[string]bool{"default": true}
	entries, err := os.ReadDir(m.boardsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "_archived" && boardSlug.MatchString(entry.Name()) && m.Exists(entry.Name()) {
			slugs[entry.Name()] = true
		}
	}
	ordered := make([]string, 0, len(slugs))
	for slug := range slugs {
		ordered = append(ordered, slug)
	}
	sort.Strings(ordered)
	for index, slug := range ordered {
		if slug == "default" {
			ordered = append([]string{"default"}, append(ordered[:index], ordered[index+1:]...)...)
			break
		}
	}
	result := make([]Metadata, 0, len(ordered))
	for _, slug := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		metadata, err := m.listActiveMetadata(ctx, slug)
		if err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	if !includeArchived {
		return result, nil
	}
	archivedRoot := filepath.Join(m.boardsRoot, "_archived")
	archivedRootInfo, err := os.Lstat(archivedRoot)
	if err == nil {
		if !archivedRootInfo.IsDir() ||
			archivedRootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New(
				"archived recovery inventory root must be a real directory",
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	archivedEntries, err := os.ReadDir(archivedRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range archivedEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := filepath.Join(archivedRoot, entry.Name())
		entryInfo, err := os.Lstat(directory)
		if err != nil {
			return nil, fmt.Errorf(
				"inspect archived recovery inventory entry %s: %w",
				entry.Name(),
				err,
			)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf(
				"archived recovery inventory contains a symbolic link: %s",
				entry.Name(),
			)
		}
		if !entryInfo.IsDir() {
			continue
		}
		raw := readPersisted(filepath.Join(directory, "board.json"))
		slug := raw.Slug
		if slug == "" {
			slug = regexp.MustCompile(`-\d+$`).ReplaceAllString(entry.Name(), "")
		}
		metadata := Metadata{Slug: slug, Name: raw.Name, Description: raw.Description, Icon: raw.Icon, Color: raw.Color, DefaultWorkdir: raw.DefaultWorkdir, CreatedAt: raw.CreatedAt, Archived: true, DBPath: filepath.Join(directory, "autogora.db"), WorkspaceRoot: filepath.Join(directory, "workspaces"), AttachmentsRoot: filepath.Join(directory, "attachments"), LogsRoot: filepath.Join(directory, "logs"), Orchestration: normalizeOrchestration(raw.Orchestration)}
		if err := attachListedBoardIdentity(&metadata, directory); err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	return result, nil
}

func (m *Manager) List(ctx context.Context, includeArchived bool) ([]Metadata, error) {
	result, err := m.ListMetadata(ctx, includeArchived)
	if err != nil {
		return nil, err
	}
	for index := range result {
		if result[index].Archived {
			continue
		}
		opened, err := m.OpenStore(ctx, result[index].Slug)
		if err != nil {
			return nil, err
		}
		counts, countErr := opened.CountTasksByStatus(ctx, result[index].Slug)
		closeErr := opened.Close()
		if countErr != nil {
			return nil, errors.Join(countErr, closeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		result[index].Counts = counts
	}
	return result, nil
}

func (m *Manager) Current() string {
	if value := strings.TrimSpace(os.Getenv("AUTOGORA_BOARD")); value != "" {
		if slug, err := NormalizeSlug(value); err == nil && m.Exists(slug) {
			return slug
		}
	}
	if contents, err := os.ReadFile(m.currentPath); err == nil {
		if slug, err := NormalizeSlug(string(contents)); err == nil && m.Exists(slug) {
			return slug
		}
	}
	return "default"
}

func (m *Manager) Resolve(explicit string) (string, error) {
	value := explicit
	if strings.TrimSpace(value) == "" {
		value = m.Current()
	}
	slug, err := NormalizeSlug(value)
	if err != nil {
		return "", err
	}
	if !m.Exists(slug) {
		return "", fmt.Errorf("board not found: %s", slug)
	}
	return slug, nil
}

func (m *Manager) Switch(board string) (Metadata, error) {
	value := board
	if strings.TrimSpace(value) == "" {
		value = m.Current()
	}
	slug, err := NormalizeSlug(value)
	if err != nil {
		return Metadata{}, err
	}
	metadataLock, acquired, err := acquireBoardMutationLock(m.boardMetadataLockPath(slug), false)
	if err != nil {
		return Metadata{}, fmt.Errorf("lock board %s metadata while switching: %w", slug, err)
	}
	if !acquired {
		return Metadata{}, fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug)
	}
	defer metadataLock.Close()
	lifecycleLock, acquired, err := acquireBoardMutationLock(m.boardLifecycleLockPath(slug), false)
	if err != nil {
		return Metadata{}, fmt.Errorf("lock board %s lifecycle while switching: %w", slug, err)
	}
	if !acquired {
		return Metadata{}, fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug)
	}
	defer lifecycleLock.Close()
	if !m.Exists(slug) {
		return Metadata{}, fmt.Errorf("board not found: %s", slug)
	}
	metadata, err := m.Read(slug)
	if err != nil {
		return Metadata{}, err
	}
	currentLock, err := m.lockCurrentSelection()
	if err != nil {
		return Metadata{}, err
	}
	defer currentLock.Close()
	if err := m.writeCurrentSelection(slug); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) lockCurrentSelection() (*boardMutationLock, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		lock, acquired, err := acquireBoardMutationLock(m.currentLockPath(), true)
		if err != nil {
			return nil, fmt.Errorf("lock current board selection: %w", err)
		}
		if acquired {
			return lock, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("current board selection is being updated")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeCurrentSelection atomically replaces the current-board file. The
// caller must hold currentLockPath exclusively.
func (m *Manager) writeCurrentSelection(slug string) error {
	if err := os.MkdirAll(filepath.Dir(m.currentPath), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.currentPath), ".current-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write([]byte(slug + "\n")); err != nil {
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
	if err := os.Rename(temporaryPath, m.currentPath); err != nil {
		return err
	}
	return nil
}

// resetCurrentAfterRemoval preserves a newer user selection. It updates the
// file only when it still names the board that was just removed.
func (m *Manager) resetCurrentAfterRemoval(removed string) error {
	currentLock, err := m.lockCurrentSelection()
	if err != nil {
		return err
	}
	defer currentLock.Close()
	contents, err := os.ReadFile(m.currentPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(contents)) != removed {
		return nil
	}
	return m.writeCurrentSelection("default")
}

func (m *Manager) hasArchived(board string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(m.boardsRoot, "_archived"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw := readPersisted(filepath.Join(m.boardsRoot, "_archived", entry.Name(), "board.json"))
		if raw.Slug == board {
			return true, nil
		}
		if raw.Slug == "" && regexp.MustCompile(`-\d+$`).ReplaceAllString(entry.Name(), "") == board {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) releaseRemovalGuards(ctx context.Context, board string, local, coordination store.BoardRemovalGuard) error {
	var releaseErrors []error
	if local.Token != "" {
		dbPath, _ := m.DBPath(board)
		attachments, _ := m.AttachmentsRoot(board)
		opened, err := store.Open(dbPath, board, attachments)
		if err != nil {
			releaseErrors = append(releaseErrors, fmt.Errorf("open board store to release removal guard: %w", err))
		} else {
			released, releaseErr := opened.ReleaseBoardRemovalGuard(ctx, local)
			closeErr := opened.Close()
			if releaseErr != nil || closeErr != nil {
				releaseErrors = append(releaseErrors, errors.Join(releaseErr, closeErr))
			} else if !released {
				releaseErrors = append(releaseErrors, errors.New("local board removal guard changed before release"))
			}
		}
	}
	if coordination.Token != "" {
		opened, err := m.OpenCoordinationStore(ctx)
		if err != nil {
			releaseErrors = append(releaseErrors, fmt.Errorf("open coordination store to release removal guard: %w", err))
		} else {
			released, releaseErr := opened.ReleaseBoardRemovalGuard(ctx, coordination)
			closeErr := opened.Close()
			if releaseErr != nil || closeErr != nil {
				releaseErrors = append(releaseErrors, errors.Join(releaseErr, closeErr))
			} else if !released {
				releaseErrors = append(releaseErrors, errors.New("coordination board removal guard changed before release"))
			}
		}
	}
	return errors.Join(releaseErrors...)
}

func (m *Manager) clearStaleRemovalGuards(
	ctx context.Context,
	slug string,
	coordination *store.Store,
) error {
	opened, err := m.openStoreUnlocked(ctx, slug)
	if err != nil {
		return fmt.Errorf("open board store to recover interrupted removal: %w", err)
	}
	localGuard, guardErr := opened.HasBoardRemovalGuard(ctx, slug)
	if guardErr != nil {
		_ = opened.Close()
		return fmt.Errorf("inspect interrupted local removal for board %s: %w", slug, guardErr)
	}
	if localGuard {
		liveProcesses, liveErr := countLiveTerminalProcesses(ctx, opened)
		if liveErr != nil {
			_ = opened.Close()
			return fmt.Errorf("inspect terminal processes before recovering board %s: %w", slug, liveErr)
		}
		if liveProcesses > 0 {
			_ = opened.Close()
			return &store.BoardBusyError{Board: slug, LiveTerminalProcesses: liveProcesses}
		}
		if err := opened.ClearLocalBoardRemovalGuard(ctx, slug); err != nil {
			_ = opened.Close()
			return fmt.Errorf("clear interrupted local removal for board %s: %w", slug, err)
		}
	}
	closeErr := opened.Close()
	if closeErr != nil {
		return fmt.Errorf("close recovered board %s: %w", slug, closeErr)
	}
	coordinationGuard, err := coordination.HasBoardRemovalGuard(ctx, slug)
	if err != nil {
		return fmt.Errorf("inspect interrupted coordination removal for board %s: %w", slug, err)
	}
	if coordinationGuard {
		if err := coordination.ClearBoardRemovalTombstone(ctx, slug); err != nil {
			return fmt.Errorf("clear interrupted coordination removal for board %s: %w", slug, err)
		}
	}
	return nil
}

func countLiveTerminalProcesses(ctx context.Context, opened *store.Store) (int, error) {
	owners, err := opened.ListTerminalRunProcesses(ctx)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, owner := range owners {
		if runcontrol.ProcessMayStillBeRunning(owner.PID, owner.ProcessIdentity) {
			live++
		}
	}
	return live, nil
}

func (m *Manager) validateRemovalGuards(
	ctx context.Context,
	slug string,
	coordinationStore *store.Store,
	localGuard store.BoardRemovalGuard,
	coordinationGuard store.BoardRemovalGuard,
) error {
	coordinationActive, err := coordinationStore.HasExactBoardRemovalGuard(ctx, coordinationGuard)
	if err != nil {
		return fmt.Errorf("validate coordination removal guard: %w", err)
	}
	if !coordinationActive {
		return fmt.Errorf("%w: coordination guard changed for %s", store.ErrBoardRemovalInProgress, slug)
	}
	opened, err := m.openStoreUnlocked(ctx, slug)
	if err != nil {
		return fmt.Errorf("open board store to validate removal guard: %w", err)
	}
	localActive, validationErr := opened.HasExactBoardRemovalGuard(ctx, localGuard)
	closeErr := opened.Close()
	if validationErr != nil || closeErr != nil {
		return errors.Join(validationErr, closeErr)
	}
	if !localActive {
		return fmt.Errorf("%w: local guard changed for %s", store.ErrBoardRemovalInProgress, slug)
	}
	return nil
}

func (m *Manager) Remove(board string, hardDelete bool) (RemoveResult, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return RemoveResult{}, err
	}
	if slug == "default" {
		return RemoveResult{}, errors.New("the default board cannot be removed")
	}
	ctx := context.Background()
	// Acquire the global automation lock before any board metadata or lifecycle
	// lock. Recovery confirmation takes the same locks in that order, so
	// reversing them here could deadlock while the board is being removed.
	authority, err := m.openStoreUnlocked(ctx, "default")
	if err != nil {
		return RemoveResult{}, fmt.Errorf(
			"open automation authority before removing board %s: %w",
			slug,
			err,
		)
	}
	var result RemoveResult
	err = authority.RunWithAutomationGateOpen(ctx, func() error {
		return m.withBoardMutation(ctx, slug, true, func(coordination *store.Store) error {
			var removeErr error
			result, removeErr = m.remove(ctx, slug, hardDelete, coordination)
			return removeErr
		})
	})
	return result, errors.Join(err, authority.Close())
}

func (m *Manager) cleanupTerminalGlobalLeases(
	ctx context.Context,
	slug string,
	coordination *store.Store,
) (err error) {
	boardStore, err := m.openStoreUnlocked(ctx, slug)
	if err != nil {
		return fmt.Errorf("open board store to inspect global leases: %w", err)
	}
	defer func() {
		err = errors.Join(err, boardStore.Close())
	}()

	terminalRuns, err := boardStore.ListTerminalRunProcesses(ctx)
	if err != nil {
		return fmt.Errorf("list terminal run processes for board %s: %w", slug, err)
	}
	terminalByID := make(map[string]store.RunProcessOwner, len(terminalRuns))
	for _, owner := range terminalRuns {
		terminalByID[owner.RunID] = owner
	}

	slots, err := coordination.ListGlobalAgentSlotsForBoard(ctx, slug)
	if err != nil {
		return fmt.Errorf("list global agent leases for board %s: %w", slug, err)
	}
	for _, slot := range slots {
		if slot.OwnerKind != store.AgentSlotOwnerWorker || slot.RunID == nil {
			continue
		}
		owner, terminal := terminalByID[*slot.RunID]
		if !terminal || runcontrol.ProcessMayStillBeRunning(owner.PID, owner.ProcessIdentity) {
			continue
		}
		if _, releaseErr := coordination.ReleaseGlobalAgentSlot(ctx, slot); releaseErr != nil {
			return fmt.Errorf("release terminal global agent lease for run %s: %w", *slot.RunID, releaseErr)
		}
	}

	leases, err := coordination.ListGlobalWorkspaceLeases(ctx)
	if err != nil {
		return fmt.Errorf("list global workspace leases for board %s: %w", slug, err)
	}
	for _, lease := range leases {
		if lease.Board != slug {
			continue
		}
		owner, terminal := terminalByID[lease.RunID]
		if !terminal || runcontrol.ProcessMayStillBeRunning(owner.PID, owner.ProcessIdentity) {
			continue
		}
		if _, releaseErr := coordination.ReleaseGlobalWorkspaceLease(ctx, lease); releaseErr != nil {
			return fmt.Errorf("release terminal global workspace lease for run %s: %w", lease.RunID, releaseErr)
		}
	}
	return nil
}

func (m *Manager) remove(
	ctx context.Context,
	slug string,
	hardDelete bool,
	coordinationStore *store.Store,
) (RemoveResult, error) {
	if slug == "default" {
		return RemoveResult{}, errors.New("the default board cannot be removed")
	}
	if !m.Exists(slug) {
		archived, archivedErr := m.hasArchived(slug)
		if archivedErr != nil {
			return RemoveResult{}, fmt.Errorf("inspect archived boards: %w", archivedErr)
		}
		if archived {
			return RemoveResult{}, fmt.Errorf("board is already archived: %s", slug)
		}
		return RemoveResult{}, fmt.Errorf("board not found: %s", slug)
	}
	source, _ := m.BoardDir(slug)
	if err := m.cleanupTerminalGlobalLeases(ctx, slug, coordinationStore); err != nil {
		return RemoveResult{}, fmt.Errorf("clean terminal global leases before removing board %s: %w", slug, err)
	}
	coordinationGuard, err := coordinationStore.AcquireBoardRemovalGuard(ctx, slug)
	if err != nil {
		return RemoveResult{}, fmt.Errorf("check global leases before removing board %s: %w", slug, err)
	}

	boardStore, err := m.openStoreUnlocked(ctx, slug)
	if err != nil {
		releaseErr := m.releaseRemovalGuards(ctx, slug, store.BoardRemovalGuard{}, coordinationGuard)
		return RemoveResult{}, fmt.Errorf("open board store before removal: %w", errors.Join(err, releaseErr))
	}
	localGuard, guardErr := boardStore.AcquireBoardRemovalGuard(ctx, slug)
	liveProcesses := 0
	var liveErr error
	if guardErr == nil {
		liveProcesses, liveErr = countLiveTerminalProcesses(ctx, boardStore)
	}
	closeErr := boardStore.Close()
	if guardErr != nil || liveErr != nil || closeErr != nil {
		releaseErr := m.releaseRemovalGuards(ctx, slug, localGuard, coordinationGuard)
		return RemoveResult{}, fmt.Errorf(
			"check active work before removing board %s: %w",
			slug,
			errors.Join(guardErr, liveErr, closeErr, releaseErr),
		)
	}
	if liveProcesses > 0 {
		releaseErr := m.releaseRemovalGuards(ctx, slug, localGuard, coordinationGuard)
		return RemoveResult{}, errors.Join(
			&store.BoardBusyError{Board: slug, LiveTerminalProcesses: liveProcesses},
			releaseErr,
		)
	}
	rollback := func(cause error) (RemoveResult, error) {
		releaseErr := m.releaseRemovalGuards(ctx, slug, localGuard, coordinationGuard)
		return RemoveResult{}, errors.Join(cause, releaseErr)
	}

	if hardDelete {
		deletingRoot := filepath.Join(m.boardsRoot, "_deleting")
		if err := os.MkdirAll(deletingRoot, 0o755); err != nil {
			return rollback(fmt.Errorf("prepare hard delete: %w", err))
		}
		if err := m.validateRemovalGuards(ctx, slug, coordinationStore, localGuard, coordinationGuard); err != nil {
			return rollback(err)
		}
		staged := filepath.Join(deletingRoot, fmt.Sprintf("%s-%d", slug, time.Now().UnixMilli()))
		if err := os.Rename(source, staged); err != nil {
			return rollback(fmt.Errorf("stage board for hard delete: %w", err))
		}
		_ = m.resetCurrentAfterRemoval(slug)
		if err := os.RemoveAll(staged); err != nil {
			return RemoveResult{}, fmt.Errorf("board was staged at %s but could not be deleted: %w", staged, err)
		}
		return RemoveResult{Slug: slug, Path: source}, nil
	}
	previous, err := m.Read(slug)
	if err != nil {
		return rollback(err)
	}
	archived := true
	if _, err := m.write(slug, Update{}, &archived); err != nil {
		return rollback(err)
	}
	archiveLock, acquired, archiveLockErr := acquireBoardMutationLock(
		m.archivedRecoveryLockPath(),
		true,
	)
	if archiveLockErr != nil || !acquired {
		_, restoreErr := m.write(slug, Update{}, &previous.Archived)
		if archiveLockErr == nil {
			archiveLockErr = ErrBoardMutationInProgress
		}
		return rollback(errors.Join(archiveLockErr, restoreErr))
	}
	archivedRoot := filepath.Join(m.boardsRoot, "_archived")
	if err := os.MkdirAll(archivedRoot, 0o755); err != nil {
		_, restoreErr := m.write(slug, Update{}, &previous.Archived)
		return rollback(errors.Join(
			err,
			restoreErr,
			archiveLock.Close(),
		))
	}
	if err := m.validateRemovalGuards(ctx, slug, coordinationStore, localGuard, coordinationGuard); err != nil {
		_, restoreErr := m.write(slug, Update{}, &previous.Archived)
		return rollback(errors.Join(
			err,
			restoreErr,
			archiveLock.Close(),
		))
	}
	target := filepath.Join(archivedRoot, fmt.Sprintf("%s-%d", slug, time.Now().UnixMilli()))
	if err := os.Rename(source, target); err != nil {
		_, restoreErr := m.write(slug, Update{}, &previous.Archived)
		return rollback(errors.Join(
			err,
			restoreErr,
			archiveLock.Close(),
		))
	}
	_ = m.resetCurrentAfterRemoval(slug)
	return RemoveResult{Slug: slug, Archived: true, Path: target},
		archiveLock.Close()
}

func (m *Manager) OpenStore(ctx context.Context, board string) (*store.Store, error) {
	slug := board
	if strings.TrimSpace(slug) == "" {
		slug = m.Current()
	}
	normalized, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	metadataLock, acquired, lockErr := acquireBoardMutationLock(m.boardMetadataLockPath(normalized), false)
	if lockErr != nil {
		return nil, fmt.Errorf("lock board %s metadata while opening its store: %w", normalized, lockErr)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: %s", ErrBoardMutationInProgress, normalized)
	}
	lifecycleLock, acquired, lifecycleErr := acquireBoardMutationLock(m.boardLifecycleLockPath(normalized), false)
	if lifecycleErr != nil || !acquired {
		closeErr := metadataLock.Close()
		if lifecycleErr != nil {
			return nil, errors.Join(
				fmt.Errorf("lock board %s lifecycle while opening its store: %w", normalized, lifecycleErr),
				closeErr,
			)
		}
		return nil, errors.Join(fmt.Errorf("%w: %s", ErrBoardMutationInProgress, normalized), closeErr)
	}
	opened, openErr := m.openStoreUnlocked(ctx, normalized)
	if openErr != nil {
		return nil, errors.Join(openErr, lifecycleLock.Close(), metadataLock.Close())
	}
	opened.SetCloseHook(lifecycleLock.Close)
	if normalized != "default" {
		coordination, coordinationErr := m.OpenCoordinationStore(ctx)
		if coordinationErr != nil {
			return nil, errors.Join(coordinationErr, opened.Close(), metadataLock.Close())
		}
		recoveryErr := m.clearStaleRemovalGuards(ctx, normalized, coordination)
		closeErr := coordination.Close()
		if errors.Is(recoveryErr, store.ErrBoardBusy) {
			// Keep the durable guard and return a read-capable Store so the
			// operator can inspect the orphaned process before retrying.
			recoveryErr = nil
		}
		if recoveryErr != nil || closeErr != nil {
			return nil, errors.Join(recoveryErr, closeErr, opened.Close(), metadataLock.Close())
		}
	}
	if err := metadataLock.Close(); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

func validateListedBoardIdentity(
	metadata Metadata,
	slug string,
	archived bool,
) (*listedBoardIdentity, error) {
	identity := metadata.listedIdentity
	if identity == nil {
		return nil, errors.New(
			"board metadata was not produced by the recovery inventory",
		)
	}
	if metadata.Slug != slug ||
		metadata.Archived != archived ||
		identity.slug != slug ||
		identity.archived != archived ||
		identity.dbInfo == nil ||
		!listedCreatedAtMatches(identity, metadata.CreatedAt) {
		return nil, errors.New(
			"board metadata does not match its listed recovery identity",
		)
	}
	dbPath, err := filepath.Abs(strings.TrimSpace(metadata.DBPath))
	if err != nil || strings.TrimSpace(metadata.DBPath) == "" {
		return nil, errors.New("listed board database path is required")
	}
	if dbPath != identity.dbPath {
		return nil, errors.New(
			"board database path does not match its listed recovery identity",
		)
	}
	return identity, nil
}

func (m *Manager) openActiveListedPublicationRecoveryReader(
	ctx context.Context,
	metadata Metadata,
	slug string,
) (*store.PublicationRecoveryReader, error) {
	identity, err := validateListedBoardIdentity(
		metadata,
		slug,
		false,
	)
	if err != nil {
		return nil, err
	}
	expectedDBPath, err := m.DBPath(slug)
	if err != nil {
		return nil, err
	}
	expectedDBPath, err = filepath.Abs(expectedDBPath)
	if err != nil {
		return nil, fmt.Errorf(
			"resolve active board %s database: %w",
			slug,
			err,
		)
	}
	if expectedDBPath != identity.dbPath {
		return nil, errors.New(
			"active board database is outside its manager-owned location",
		)
	}

	metadataLock, acquired, lockErr := acquireBoardMutationLock(
		m.boardMetadataLockPath(slug),
		false,
	)
	if lockErr != nil {
		return nil, fmt.Errorf(
			"lock board %s metadata for publication recovery: %w",
			slug,
			lockErr,
		)
	}
	if !acquired {
		return nil, fmt.Errorf(
			"%w: %s",
			ErrBoardMutationInProgress,
			slug,
		)
	}
	lifecycleLock, acquired, lifecycleErr := acquireBoardMutationLock(
		m.boardLifecycleLockPath(slug),
		false,
	)
	if lifecycleErr != nil || !acquired {
		closeErr := metadataLock.Close()
		if lifecycleErr != nil {
			return nil, errors.Join(
				fmt.Errorf(
					"lock board %s lifecycle for publication recovery: %w",
					slug,
					lifecycleErr,
				),
				closeErr,
			)
		}
		return nil, errors.Join(
			fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug),
			closeErr,
		)
	}
	closeLocks := func(cause error) error {
		return errors.Join(
			cause,
			lifecycleLock.Close(),
			metadataLock.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, closeLocks(err)
	}
	current, err := m.Read(slug)
	if err != nil {
		return nil, closeLocks(err)
	}
	currentDBPath, err := filepath.Abs(current.DBPath)
	if err != nil {
		return nil, closeLocks(err)
	}
	if current.Slug != slug ||
		current.Archived ||
		currentDBPath != identity.dbPath ||
		!listedCreatedAtMatches(identity, current.CreatedAt) {
		return nil, closeLocks(fmt.Errorf(
			"active board %s changed since recovery inventory",
			slug,
		))
	}
	dbInfo, err := os.Lstat(identity.dbPath)
	if err != nil {
		return nil, closeLocks(fmt.Errorf(
			"inspect active board %s database: %w",
			slug,
			err,
		))
	}
	if !dbInfo.Mode().IsRegular() ||
		dbInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.dbInfo, dbInfo) {
		return nil, closeLocks(fmt.Errorf(
			"active board %s database changed since recovery inventory",
			slug,
		))
	}
	opened, err := store.OpenPublicationRecoveryReader(
		ctx,
		identity.dbPath,
		slug,
	)
	if err != nil {
		return nil, closeLocks(err)
	}
	opened.SetCloseHook(lifecycleLock.Close)
	dbInfoAfterOpen, statErr := os.Lstat(identity.dbPath)
	if statErr != nil ||
		!dbInfoAfterOpen.Mode().IsRegular() ||
		dbInfoAfterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.dbInfo, dbInfoAfterOpen) {
		if statErr == nil {
			statErr = fmt.Errorf(
				"active board %s database changed while opening recovery reader",
				slug,
			)
		} else {
			statErr = fmt.Errorf(
				"reinspect active board %s database: %w",
				slug,
				statErr,
			)
		}
		return nil, errors.Join(
			statErr,
			opened.Close(),
			metadataLock.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(
			err,
			opened.Close(),
			metadataLock.Close(),
		)
	}
	if err := metadataLock.Close(); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

// OpenListedPublicationRecoveryReader opens an active board by slug or an
// archived board through the exact manager-owned location returned by
// ListMetadata. It is query-only: startup ownership checks never migrate or
// otherwise mutate an archived database. Exact quarantine evidence is written
// separately through the dispatcher's shared default-board authority.
func (m *Manager) OpenListedPublicationRecoveryReader(
	ctx context.Context,
	metadata Metadata,
) (*store.PublicationRecoveryReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slug, err := NormalizeSlug(metadata.Slug)
	if err != nil {
		return nil, err
	}
	if !metadata.Archived {
		return m.openActiveListedPublicationRecoveryReader(
			ctx,
			metadata,
			slug,
		)
	}
	archiveLock, acquired, lockErr := acquireBoardMutationLock(
		m.archivedRecoveryLockPath(),
		false,
	)
	if lockErr != nil {
		return nil, fmt.Errorf(
			"lock archived publication recovery inventory: %w",
			lockErr,
		)
	}
	if !acquired {
		return nil, ErrBoardMutationInProgress
	}
	closeArchiveLock := func(cause error) error {
		return errors.Join(cause, archiveLock.Close())
	}
	archiveRoot, err := filepath.Abs(filepath.Join(m.boardsRoot, "_archived"))
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"resolve archived board root: %w",
			err,
		))
	}
	dbPath, err := filepath.Abs(strings.TrimSpace(metadata.DBPath))
	if err != nil || strings.TrimSpace(metadata.DBPath) == "" {
		return nil, closeArchiveLock(errors.New(
			"archived board database path is required",
		))
	}
	relative, err := filepath.Rel(archiveRoot, dbPath)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"resolve archived board database: %w",
			err,
		))
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[0] == "." ||
		parts[0] == ".." || parts[1] != "autogora.db" {
		return nil, closeArchiveLock(errors.New(
			"archived board database is outside its manager-owned location",
		))
	}
	directory := filepath.Join(archiveRoot, parts[0])
	identity, err := validateListedBoardIdentity(metadata, slug, true)
	if err != nil {
		return nil, closeArchiveLock(err)
	}
	if identity.directoryInfo == nil ||
		identity.directoryPath != directory {
		return nil, closeArchiveLock(errors.New(
			"archived board directory does not match its listed recovery identity",
		))
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"inspect archived board directory: %w",
			err,
		))
	}
	if !directoryInfo.IsDir() ||
		directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, closeArchiveLock(errors.New(
			"archived board directory must be a real directory",
		))
	}
	if !os.SameFile(identity.directoryInfo, directoryInfo) {
		return nil, closeArchiveLock(errors.New(
			"archived board directory changed since recovery inventory",
		))
	}
	dbInfo, err := os.Lstat(dbPath)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"inspect archived board database: %w",
			err,
		))
	}
	if !dbInfo.Mode().IsRegular() || dbInfo.Mode()&os.ModeSymlink != 0 {
		return nil, closeArchiveLock(errors.New(
			"archived board database must be a regular file",
		))
	}
	if !os.SameFile(identity.dbInfo, dbInfo) {
		return nil, closeArchiveLock(errors.New(
			"archived board database changed since recovery inventory",
		))
	}
	canonicalRoot, err := filepath.EvalSymlinks(archiveRoot)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"resolve archived board root links: %w",
			err,
		))
	}
	canonicalDB, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"resolve archived board database links: %w",
			err,
		))
	}
	canonicalRelative, err := filepath.Rel(canonicalRoot, canonicalDB)
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"validate archived board database links: %w",
			err,
		))
	}
	canonicalParts := strings.Split(
		canonicalRelative,
		string(filepath.Separator),
	)
	if len(canonicalParts) != 2 ||
		canonicalParts[0] != parts[0] ||
		canonicalParts[1] != "autogora.db" {
		return nil, closeArchiveLock(errors.New(
			"archived board database resolves outside its manager-owned location",
		))
	}
	contents, err := os.ReadFile(filepath.Join(directory, "board.json"))
	if err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"read archived board metadata: %w",
			err,
		))
	}
	var raw persistedMetadata
	if err := json.Unmarshal(contents, &raw); err != nil {
		return nil, closeArchiveLock(fmt.Errorf(
			"decode archived board metadata: %w",
			err,
		))
	}
	persistedSlug, err := NormalizeSlug(raw.Slug)
	if err != nil ||
		persistedSlug != slug ||
		!raw.Archived ||
		!listedCreatedAtMatches(identity, raw.CreatedAt) {
		return nil, closeArchiveLock(errors.New(
			"archived board metadata does not match its listed identity",
		))
	}
	opened, err := store.OpenPublicationRecoveryReader(ctx, dbPath, slug)
	if err != nil {
		return nil, closeArchiveLock(err)
	}
	opened.SetCloseHook(archiveLock.Close)
	directoryInfoAfterOpen, directoryStatErr := os.Lstat(directory)
	dbInfoAfterOpen, dbStatErr := os.Lstat(dbPath)
	if directoryStatErr != nil ||
		dbStatErr != nil ||
		!directoryInfoAfterOpen.IsDir() ||
		directoryInfoAfterOpen.Mode()&os.ModeSymlink != 0 ||
		!dbInfoAfterOpen.Mode().IsRegular() ||
		dbInfoAfterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.directoryInfo, directoryInfoAfterOpen) ||
		!os.SameFile(identity.dbInfo, dbInfoAfterOpen) {
		return nil, errors.Join(
			errors.New(
				"archived board changed while opening recovery reader",
			),
			directoryStatErr,
			dbStatErr,
			opened.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

// ApplyListedPublicationRecovery is the only board-manager path that opens an
// archived database for an operator write. It accepts only the opaque listed
// identity returned by ListMetadata, revalidates that identity before and
// after opening the database, applies one exact publication recovery, and
// closes the Store without exposing it to callers.
func (m *Manager) ApplyListedPublicationRecovery(
	ctx context.Context,
	metadata Metadata,
	permit *store.AutomationRecoveryPermit,
	input store.PublicationRecoveryInput,
) (store.PublicationRecoveryResult, error) {
	results, err := m.ApplyListedPublicationRecoveries(
		ctx,
		metadata,
		permit,
		[]store.PublicationRecoveryInput{input},
	)
	if err != nil {
		return store.PublicationRecoveryResult{}, err
	}
	if len(results) != 1 {
		return store.PublicationRecoveryResult{}, errors.New(
			"operator publication recovery returned an invalid result count",
		)
	}
	return results[0], nil
}

// ApplyListedPublicationRecoveries amortizes identity verification and Store
// opening for a board while keeping each recovery in its own Store
// transaction. A later source failure therefore remains safely resumable from
// the immutable receipts committed for earlier sources.
func (m *Manager) ApplyListedPublicationRecoveries(
	ctx context.Context,
	metadata Metadata,
	permit *store.AutomationRecoveryPermit,
	inputs []store.PublicationRecoveryInput,
) ([]store.PublicationRecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, errors.New(
			"operator publication recovery requires at least one source",
		)
	}
	if len(inputs) > 1000 {
		return nil, errors.New(
			"operator publication recovery cannot exceed 1000 sources",
		)
	}
	slug, err := NormalizeSlug(metadata.Slug)
	if err != nil {
		return nil, err
	}
	if metadata.Archived {
		return m.applyArchivedListedPublicationRecoveries(
			ctx,
			metadata,
			slug,
			permit,
			inputs,
		)
	}
	return m.applyActiveListedPublicationRecoveries(
		ctx,
		metadata,
		slug,
		permit,
		inputs,
	)
}

func (m *Manager) applyActiveListedPublicationRecoveries(
	ctx context.Context,
	metadata Metadata,
	slug string,
	permit *store.AutomationRecoveryPermit,
	inputs []store.PublicationRecoveryInput,
) (results []store.PublicationRecoveryResult, resultErr error) {
	identity, err := validateListedBoardIdentity(metadata, slug, false)
	if err != nil {
		return nil, err
	}
	expectedDBPath, err := m.DBPath(slug)
	if err != nil {
		return nil, err
	}
	expectedDBPath, err = filepath.Abs(expectedDBPath)
	if err != nil {
		return nil, fmt.Errorf(
			"resolve active board %s recovery database: %w",
			slug,
			err,
		)
	}
	if expectedDBPath != identity.dbPath {
		return nil, errors.New(
			"active board recovery database is outside its manager-owned location",
		)
	}

	metadataLock, acquired, lockErr := acquireBoardMutationLock(
		m.boardMetadataLockPath(slug),
		false,
	)
	if lockErr != nil {
		return nil, fmt.Errorf(
			"lock board %s metadata for operator recovery: %w",
			slug,
			lockErr,
		)
	}
	if !acquired {
		return nil, fmt.Errorf(
			"%w: %s",
			ErrBoardMutationInProgress,
			slug,
		)
	}
	lifecycleLock, acquired, lifecycleErr := acquireBoardMutationLock(
		m.boardLifecycleLockPath(slug),
		false,
	)
	if lifecycleErr != nil || !acquired {
		closeErr := metadataLock.Close()
		if lifecycleErr != nil {
			return nil, errors.Join(
				fmt.Errorf(
					"lock board %s lifecycle for operator recovery: %w",
					slug,
					lifecycleErr,
				),
				closeErr,
			)
		}
		return nil, errors.Join(
			fmt.Errorf("%w: %s", ErrBoardMutationInProgress, slug),
			closeErr,
		)
	}
	closeLocks := func(cause error) error {
		return errors.Join(
			cause,
			lifecycleLock.Close(),
			metadataLock.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, closeLocks(err)
	}
	current, err := m.Read(slug)
	if err != nil {
		return nil, closeLocks(err)
	}
	currentDBPath, err := filepath.Abs(current.DBPath)
	if err != nil {
		return nil, closeLocks(err)
	}
	if current.Slug != slug ||
		current.Archived ||
		currentDBPath != identity.dbPath ||
		!listedCreatedAtMatches(identity, current.CreatedAt) {
		return nil, closeLocks(fmt.Errorf(
			"active board %s changed since operator recovery inventory",
			slug,
		))
	}
	dbInfo, err := os.Lstat(identity.dbPath)
	if err != nil {
		return nil, closeLocks(fmt.Errorf(
			"inspect active board %s operator recovery database: %w",
			slug,
			err,
		))
	}
	if !dbInfo.Mode().IsRegular() ||
		dbInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.dbInfo, dbInfo) {
		return nil, closeLocks(fmt.Errorf(
			"active board %s database changed since operator recovery inventory",
			slug,
		))
	}
	attachmentsRoot, err := m.AttachmentsRoot(slug)
	if err != nil {
		return nil, closeLocks(err)
	}
	opened, err := store.Open(identity.dbPath, slug, attachmentsRoot)
	if err != nil {
		return nil, closeLocks(err)
	}
	opened.SetCloseHook(lifecycleLock.Close)
	dbInfoAfterOpen, statErr := os.Lstat(identity.dbPath)
	if statErr != nil ||
		!dbInfoAfterOpen.Mode().IsRegular() ||
		dbInfoAfterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.dbInfo, dbInfoAfterOpen) {
		if statErr == nil {
			statErr = fmt.Errorf(
				"active board %s database changed while opening operator recovery writer",
				slug,
			)
		} else {
			statErr = fmt.Errorf(
				"reinspect active board %s operator recovery database: %w",
				slug,
				statErr,
			)
		}
		return nil, errors.Join(
			statErr,
			opened.Close(),
			metadataLock.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(
			err,
			opened.Close(),
			metadataLock.Close(),
		)
	}
	results = make([]store.PublicationRecoveryResult, 0, len(inputs))
	for _, input := range inputs {
		result, err := opened.ApplyPublicationRecovery(ctx, permit, input)
		if err != nil {
			resultErr = fmt.Errorf(
				"apply publication recovery %s on board %s: %w",
				input.SourceKey,
				slug,
				err,
			)
			break
		}
		results = append(results, result)
	}
	dbInfoAfterApply, identityErr := os.Lstat(identity.dbPath)
	if identityErr == nil &&
		(!dbInfoAfterApply.Mode().IsRegular() ||
			dbInfoAfterApply.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(identity.dbInfo, dbInfoAfterApply)) {
		identityErr = errors.New(
			"active board database changed during operator recovery",
		)
	}
	if identityErr != nil {
		resultErr = errors.Join(
			resultErr,
			fmt.Errorf(
				"revalidate active board %s after operator recovery: %w",
				slug,
				identityErr,
			),
		)
	}
	resultErr = errors.Join(
		resultErr,
		opened.Close(),
		metadataLock.Close(),
	)
	return results, resultErr
}

func (m *Manager) validateArchivedListedRecoveryLocation(
	metadata Metadata,
	slug string,
) (*listedBoardIdentity, string, string, error) {
	identity, err := validateListedBoardIdentity(metadata, slug, true)
	if err != nil {
		return nil, "", "", err
	}
	archiveRoot, err := filepath.Abs(filepath.Join(m.boardsRoot, "_archived"))
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"resolve archived operator recovery root: %w",
			err,
		)
	}
	if identity.archiveRootInfo == nil ||
		identity.archiveRootPath != archiveRoot {
		return nil, "", "", errors.New(
			"archived board root does not match its listed recovery identity",
		)
	}
	rootInfo, err := os.Lstat(archiveRoot)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"inspect archived operator recovery root: %w",
			err,
		)
	}
	if !rootInfo.IsDir() ||
		rootInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.archiveRootInfo, rootInfo) {
		return nil, "", "", errors.New(
			"archived board root changed since recovery inventory",
		)
	}
	relative, err := filepath.Rel(archiveRoot, identity.dbPath)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"resolve archived operator recovery database: %w",
			err,
		)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[0] == "." ||
		parts[0] == ".." || parts[1] != "autogora.db" {
		return nil, "", "", errors.New(
			"archived operator recovery database is outside its manager-owned location",
		)
	}
	directory := filepath.Join(archiveRoot, parts[0])
	if identity.directoryInfo == nil ||
		identity.directoryPath != directory {
		return nil, "", "", errors.New(
			"archived board directory does not match its listed recovery identity",
		)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"inspect archived operator recovery directory: %w",
			err,
		)
	}
	if !directoryInfo.IsDir() ||
		directoryInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.directoryInfo, directoryInfo) {
		return nil, "", "", errors.New(
			"archived board directory changed since recovery inventory",
		)
	}
	dbInfo, err := os.Lstat(identity.dbPath)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"inspect archived operator recovery database: %w",
			err,
		)
	}
	if !dbInfo.Mode().IsRegular() ||
		dbInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(identity.dbInfo, dbInfo) {
		return nil, "", "", errors.New(
			"archived board database changed since recovery inventory",
		)
	}
	canonicalRoot, err := filepath.EvalSymlinks(archiveRoot)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"resolve archived operator recovery root links: %w",
			err,
		)
	}
	canonicalDB, err := filepath.EvalSymlinks(identity.dbPath)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"resolve archived operator recovery database links: %w",
			err,
		)
	}
	canonicalRelative, err := filepath.Rel(canonicalRoot, canonicalDB)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"validate archived operator recovery database links: %w",
			err,
		)
	}
	canonicalParts := strings.Split(
		canonicalRelative,
		string(filepath.Separator),
	)
	if len(canonicalParts) != 2 ||
		canonicalParts[0] != parts[0] ||
		canonicalParts[1] != "autogora.db" {
		return nil, "", "", errors.New(
			"archived operator recovery database resolves outside its manager-owned location",
		)
	}
	boardMetadataPath := filepath.Join(directory, "board.json")
	boardMetadataInfo, err := os.Lstat(boardMetadataPath)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"inspect archived operator recovery metadata: %w",
			err,
		)
	}
	if !boardMetadataInfo.Mode().IsRegular() ||
		boardMetadataInfo.Mode()&os.ModeSymlink != 0 {
		return nil, "", "", errors.New(
			"archived operator recovery metadata must be a regular file",
		)
	}
	contents, err := os.ReadFile(boardMetadataPath)
	if err != nil {
		return nil, "", "", fmt.Errorf(
			"read archived operator recovery metadata: %w",
			err,
		)
	}
	var raw persistedMetadata
	if err := json.Unmarshal(contents, &raw); err != nil {
		return nil, "", "", fmt.Errorf(
			"decode archived operator recovery metadata: %w",
			err,
		)
	}
	persistedSlug, err := NormalizeSlug(raw.Slug)
	if err != nil ||
		persistedSlug != slug ||
		!raw.Archived ||
		!listedCreatedAtMatches(identity, raw.CreatedAt) {
		return nil, "", "", errors.New(
			"archived board metadata does not match its listed recovery identity",
		)
	}
	return identity, directory, boardMetadataPath, nil
}

func (m *Manager) applyArchivedListedPublicationRecoveries(
	ctx context.Context,
	metadata Metadata,
	slug string,
	permit *store.AutomationRecoveryPermit,
	inputs []store.PublicationRecoveryInput,
) (results []store.PublicationRecoveryResult, resultErr error) {
	archiveLock, acquired, lockErr := acquireBoardMutationLock(
		m.archivedRecoveryLockPath(),
		false,
	)
	if lockErr != nil {
		return nil, fmt.Errorf(
			"lock archived operator recovery writer: %w",
			lockErr,
		)
	}
	if !acquired {
		return nil, ErrBoardMutationInProgress
	}
	closeArchiveLock := func(cause error) error {
		return errors.Join(cause, archiveLock.Close())
	}
	identity, directory, _, err := m.validateArchivedListedRecoveryLocation(
		metadata,
		slug,
	)
	if err != nil {
		return nil, closeArchiveLock(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, closeArchiveLock(err)
	}
	opened, err := store.Open(
		identity.dbPath,
		slug,
		filepath.Join(directory, "attachments"),
	)
	if err != nil {
		return nil, closeArchiveLock(err)
	}
	currentIdentity, currentDirectory, _, validationErr :=
		m.validateArchivedListedRecoveryLocation(metadata, slug)
	if validationErr != nil ||
		currentIdentity.dbPath != identity.dbPath ||
		currentDirectory != directory {
		if validationErr == nil {
			validationErr = errors.New(
				"archived board changed while opening operator recovery writer",
			)
		}
		return nil, errors.Join(
			validationErr,
			opened.Close(),
			archiveLock.Close(),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(
			err,
			opened.Close(),
			archiveLock.Close(),
		)
	}
	results = make([]store.PublicationRecoveryResult, 0, len(inputs))
	for _, input := range inputs {
		result, err := opened.ApplyPublicationRecovery(ctx, permit, input)
		if err != nil {
			resultErr = fmt.Errorf(
				"apply publication recovery %s on archived board %s: %w",
				input.SourceKey,
				slug,
				err,
			)
			break
		}
		results = append(results, result)
	}
	if _, _, _, identityErr :=
		m.validateArchivedListedRecoveryLocation(metadata, slug); identityErr != nil {
		resultErr = errors.Join(
			resultErr,
			fmt.Errorf(
				"revalidate archived board %s after operator recovery: %w",
				slug,
				identityErr,
			),
		)
	}
	resultErr = errors.Join(
		resultErr,
		opened.Close(),
		archiveLock.Close(),
	)
	return results, resultErr
}

func (m *Manager) openStoreUnlocked(ctx context.Context, board string) (*store.Store, error) {
	slug := board
	if strings.TrimSpace(slug) == "" {
		slug = m.Current()
	}
	normalized, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	if normalized != "default" && !m.Exists(normalized) {
		return nil, fmt.Errorf("board not found: %s", normalized)
	}
	dbPath, _ := m.DBPath(normalized)
	attachments, _ := m.AttachmentsRoot(normalized)
	opened, err := store.Open(dbPath, normalized, attachments)
	if err != nil {
		return nil, err
	}
	if err := opened.ConfigureAutomationGate(store.AutomationGateConfig{
		AuthorityDBPath: m.defaultDBPath,
	}); err != nil {
		return nil, errors.Join(err, opened.Close())
	}
	return opened, nil
}

// OpenCoordinationStore opens the default database shared by every board.
// Cross-board resources must be coordinated here rather than in a board-local
// task database.
func (m *Manager) OpenCoordinationStore(ctx context.Context) (*store.Store, error) {
	return m.OpenStore(ctx, "default")
}
