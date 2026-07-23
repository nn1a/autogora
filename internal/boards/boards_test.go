package boards

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestBoardsIsolateStorageAndArchiveRecoverably(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "default", Update{}); err != nil {
		t.Fatal(err)
	}
	name := "Product API"
	project, err := manager.Create(ctx, "project-api", Update{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != name || project.Orchestration.PlannerRuntime != model.RuntimeCodex {
		t.Fatalf("unexpected project metadata: %+v", project)
	}
	projectStore, err := manager.OpenStore(ctx, "project-api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projectStore.CreateTask(ctx, store.CreateTaskInput{Title: "Project task"}); err != nil {
		t.Fatal(err)
	}
	projectStore.Close()
	defaultStore, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defaultTasks, err := defaultStore.ListTasks(ctx, store.ListTaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defaultStore.Close()
	if len(defaultTasks) != 0 {
		t.Fatalf("default board leaked project tasks: %+v", defaultTasks)
	}
	if _, err := manager.Switch("project-api"); err != nil {
		t.Fatal(err)
	}
	if manager.Current() != "project-api" {
		t.Fatalf("current board = %q", manager.Current())
	}
	listed, err := manager.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[1].Counts[model.TaskStatusTodo] != 1 {
		t.Fatalf("unexpected board list: %+v", listed)
	}
	removed, err := manager.Remove("project-api", false)
	if err != nil {
		t.Fatal(err)
	}
	if !removed.Archived || manager.Current() != "default" || manager.Exists("project-api") {
		t.Fatalf("recoverable removal failed: %+v", removed)
	}
	listed, err = manager.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || !listed[1].Archived || listed[1].Slug != "project-api" {
		t.Fatalf("archived board missing: %+v", listed)
	}
}

func TestBoardSlugAndEnvironmentSelection(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "release_2026", Update{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOGORA_BOARD", "release_2026")
	if manager.Current() != "release_2026" {
		t.Fatalf("environment board = %q", manager.Current())
	}
	if _, err := NormalizeSlug("Invalid Board"); err == nil {
		t.Fatal("invalid slug was accepted")
	}
}

func TestBoardPersistsExplicitAgentModelsAndAvailability(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	plannerModel, plannerProvider := "planner-model", "openrouter"
	profiles := []Profile{{Name: "implementer", Runtime: model.RuntimeCline, Model: "worker-model", Provider: "openrouter",
		Description: "implements changes", MaxConcurrent: 2, Priority: 10, Fallbacks: []string{"backup", "backup", "implementer"}},
		{Name: "backup", Runtime: model.RuntimeClaude, Disabled: true}}
	if _, err := manager.Create(ctx, "default", Update{Orchestration: &OrchestrationUpdate{
		PlannerModel: &plannerModel, PlannerProvider: &plannerProvider, Profiles: &profiles,
	}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Orchestration.PlannerModel != plannerModel || loaded.Orchestration.PlannerProvider != plannerProvider ||
		len(loaded.Orchestration.Profiles) != 2 || loaded.Orchestration.Profiles[0].Model != "worker-model" ||
		loaded.Orchestration.Profiles[0].MaxConcurrent != 2 || len(loaded.Orchestration.Profiles[0].Fallbacks) != 1 ||
		loaded.Orchestration.Profiles[1].Disabled != true {
		t.Fatalf("agent settings were not normalized and persisted: %#v", loaded.Orchestration)
	}
}
