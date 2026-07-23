package boards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

var boardSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

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

type OrchestrationSettings struct {
	AutoDecompose        bool          `json:"autoDecompose"`
	AutoDecomposePerTick int           `json:"autoDecomposePerTick"`
	AutoPromoteChildren  bool          `json:"autoPromoteChildren"`
	PlannerRuntime       model.Runtime `json:"plannerRuntime"`
	PlannerModel         string        `json:"plannerModel,omitempty"`
	PlannerProvider      string        `json:"plannerProvider,omitempty"`
	DefaultProfile       *string       `json:"defaultProfile"`
	OrchestratorProfile  *string       `json:"orchestratorProfile"`
	Profiles             []Profile     `json:"profiles"`
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
	OrchestratorProfile  store.OptionalString
	Profiles             *[]Profile
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

func NewManager(defaultDBPath string) (*Manager, error) {
	resolved, err := filepath.Abs(defaultDBPath)
	if err != nil {
		return nil, err
	}
	home := filepath.Dir(resolved)
	return &Manager{defaultDBPath: resolved, home: home, boardsRoot: filepath.Join(home, "boards"), currentPath: filepath.Join(home, "current")}, nil
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
		PlannerRuntime: model.RuntimeCodex, Profiles: []Profile{}}
}

func validWorkerRuntime(runtime model.Runtime) bool {
	return runtime == model.RuntimeClaude || runtime == model.RuntimeCodex || runtime == model.RuntimeCline || runtime == model.RuntimeGemini
}

func normalizeOrchestration(value OrchestrationSettings) OrchestrationSettings {
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
	return value
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
		orchestration.DefaultProfile == nil && orchestration.OrchestratorProfile == nil {
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
	if update.OrchestratorProfile.Set {
		current.OrchestratorProfile = update.OrchestratorProfile.Value
	}
	if update.Profiles != nil {
		current.Profiles = *update.Profiles
	}
	return normalizeOrchestration(current)
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
	metadata, err := m.write(slug, update, nil)
	if err != nil {
		return Metadata{}, err
	}
	opened, err := m.OpenStore(ctx, slug)
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
	return metadata, nil
}

func (m *Manager) Update(board string, update Update) (Metadata, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return Metadata{}, err
	}
	if !m.Exists(slug) {
		return Metadata{}, fmt.Errorf("board not found: %s", slug)
	}
	return m.write(slug, update, nil)
}

func (m *Manager) List(ctx context.Context, includeArchived bool) ([]Metadata, error) {
	slugs := map[string]bool{"default": true}
	entries, err := os.ReadDir(m.boardsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "_archived" && boardSlug.MatchString(entry.Name()) {
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
		metadata, err := m.Read(slug)
		if err != nil {
			return nil, err
		}
		opened, err := m.OpenStore(ctx, slug)
		if err != nil {
			return nil, err
		}
		counts, countErr := opened.CountTasksByStatus(ctx, slug)
		closeErr := opened.Close()
		if countErr != nil {
			return nil, countErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		metadata.Counts = counts
		result = append(result, metadata)
	}
	if !includeArchived {
		return result, nil
	}
	archivedRoot := filepath.Join(m.boardsRoot, "_archived")
	archivedEntries, err := os.ReadDir(archivedRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range archivedEntries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(archivedRoot, entry.Name())
		raw := readPersisted(filepath.Join(directory, "board.json"))
		slug := raw.Slug
		if slug == "" {
			slug = regexp.MustCompile(`-\d+$`).ReplaceAllString(entry.Name(), "")
		}
		metadata := Metadata{Slug: slug, Name: raw.Name, Description: raw.Description, Icon: raw.Icon, Color: raw.Color, DefaultWorkdir: raw.DefaultWorkdir, CreatedAt: raw.CreatedAt, Archived: true, DBPath: filepath.Join(directory, "autogora.db"), WorkspaceRoot: filepath.Join(directory, "workspaces"), AttachmentsRoot: filepath.Join(directory, "attachments"), LogsRoot: filepath.Join(directory, "logs"), Orchestration: normalizeOrchestration(raw.Orchestration)}
		result = append(result, metadata)
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
	slug, err := m.Resolve(board)
	if err != nil {
		return Metadata{}, err
	}
	if err := os.MkdirAll(filepath.Dir(m.currentPath), 0o755); err != nil {
		return Metadata{}, err
	}
	if err := os.WriteFile(m.currentPath, []byte(slug+"\n"), 0o644); err != nil {
		return Metadata{}, err
	}
	return m.Read(slug)
}

func (m *Manager) Remove(board string, hardDelete bool) (RemoveResult, error) {
	slug, err := NormalizeSlug(board)
	if err != nil {
		return RemoveResult{}, err
	}
	if slug == "default" {
		return RemoveResult{}, errors.New("the default board cannot be removed")
	}
	if !m.Exists(slug) {
		return RemoveResult{}, fmt.Errorf("board not found: %s", slug)
	}
	source, _ := m.BoardDir(slug)
	wasCurrent := m.Current() == slug
	if hardDelete {
		if err := os.RemoveAll(source); err != nil {
			return RemoveResult{}, err
		}
		if wasCurrent {
			if _, err := m.Switch("default"); err != nil {
				return RemoveResult{}, err
			}
		}
		return RemoveResult{Slug: slug, Path: source}, nil
	}
	archived := true
	if _, err := m.write(slug, Update{}, &archived); err != nil {
		return RemoveResult{}, err
	}
	archivedRoot := filepath.Join(m.boardsRoot, "_archived")
	if err := os.MkdirAll(archivedRoot, 0o755); err != nil {
		return RemoveResult{}, err
	}
	target := filepath.Join(archivedRoot, fmt.Sprintf("%s-%d", slug, time.Now().UnixMilli()))
	if err := os.Rename(source, target); err != nil {
		return RemoveResult{}, err
	}
	if wasCurrent {
		if _, err := m.Switch("default"); err != nil {
			return RemoveResult{}, err
		}
	}
	return RemoveResult{Slug: slug, Archived: true, Path: target}, nil
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
	if normalized != "default" && !m.Exists(normalized) {
		return nil, fmt.Errorf("board not found: %s", normalized)
	}
	dbPath, _ := m.DBPath(normalized)
	attachments, _ := m.AttachmentsRoot(normalized)
	return store.Open(dbPath, normalized, attachments)
}
