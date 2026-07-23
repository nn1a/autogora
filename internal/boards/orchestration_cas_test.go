package boards

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestCompareAndSwapOrchestrationRejectsStaleSettings(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	changedModel := "newer-model"
	if _, err := manager.Update("default", Update{Orchestration: &OrchestrationUpdate{
		PlannerModel: &changedModel,
	}}); err != nil {
		t.Fatal(err)
	}
	runtime := model.RuntimeClaude
	if _, err := manager.CompareAndSwapOrchestration(ctx, "default", original.Orchestration, OrchestrationUpdate{
		PlannerRuntime: &runtime,
	}); !errors.Is(err, ErrBoardSettingsConflict) {
		t.Fatalf("stale settings error = %v", err)
	}
	loaded, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Orchestration.PlannerModel != changedModel ||
		loaded.Orchestration.PlannerRuntime != original.Orchestration.PlannerRuntime {
		t.Fatalf("stale update changed board settings: %#v", loaded.Orchestration)
	}
}

func TestCompareAndSwapOrchestrationValidatesAndPersistsAtomically(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := manager.Read("default")
	if err != nil {
		t.Fatal(err)
	}
	invalidTick := 0
	if _, err := manager.CompareAndSwapOrchestration(ctx, "default", original.Orchestration, OrchestrationUpdate{
		AutoDecomposePerTick: &invalidTick,
	}); err == nil {
		t.Fatal("invalid settings were normalized instead of rejected")
	}
	profiles := []Profile{{Name: "local", Runtime: model.RuntimeCline, Model: "worker-model"}}
	defaultProfile := "local"
	autoExecute := false
	updated, err := manager.CompareAndSwapOrchestration(ctx, "default", original.Orchestration, OrchestrationUpdate{
		Profiles:       &profiles,
		DefaultProfile: optionalBoardString(defaultProfile),
		Autopilot:      &AutopilotUpdate{AutoExecute: &autoExecute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Orchestration.Profiles) != 1 ||
		updated.Orchestration.Profiles[0].Name != "local" ||
		updated.Orchestration.DefaultProfile == nil ||
		*updated.Orchestration.DefaultProfile != "local" ||
		updated.Orchestration.Autopilot.AutoExecute {
		t.Fatalf("CAS update was not persisted: %#v", updated.Orchestration)
	}
}

func optionalBoardString(value string) store.OptionalString {
	return store.OptionalString{Set: true, Value: &value}
}
