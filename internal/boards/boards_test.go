package boards

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBoardRemovalRejectsActiveRunsForArchiveAndHardDelete(t *testing.T) {
	for _, hardDelete := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "hard-delete"}[hardDelete], func(t *testing.T) {
			ctx := context.Background()
			manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Create(ctx, "active", Update{}); err != nil {
				t.Fatal(err)
			}
			opened, err := manager.OpenStore(ctx, "active")
			if err != nil {
				t.Fatal(err)
			}
			assignee := "worker"
			task, err := opened.CreateTask(ctx, store.CreateTaskInput{
				Title: "active work", Assignee: &assignee, Runtime: model.RuntimeCodex,
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
			if err != nil || claim == nil {
				t.Fatalf("claim active work: %+v, %v", claim, err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Remove("active", hardDelete); !errors.Is(err, store.ErrBoardBusy) {
				t.Fatalf("remove active board error = %v, want ErrBoardBusy", err)
			}
			if !manager.Exists("active") {
				t.Fatal("busy board was removed")
			}
			metadata, err := manager.Read("active")
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Archived {
				t.Fatal("rejected archive left the board marked archived")
			}

			opened, err = manager.OpenStore(ctx, "active")
			if err != nil {
				t.Fatal(err)
			}
			_, failErr := opened.FailRun(ctx,
				store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
				"test complete", store.FailRunOptions{})
			closeErr := opened.Close()
			if failErr != nil || closeErr != nil {
				t.Fatal(errors.Join(failErr, closeErr))
			}
			if _, err := manager.Remove("active", hardDelete); err != nil {
				t.Fatalf("remove idle board after rejected attempt: %v", err)
			}
		})
	}
}

func TestBoardRemovalRejectsBoardOwnedGlobalLeases(t *testing.T) {
	tests := []struct {
		name string
		seed func(context.Context, *store.Store) (func(context.Context, *store.Store) error, error)
	}{
		{
			name: "agent",
			seed: func(ctx context.Context, coordination *store.Store) (func(context.Context, *store.Store) error, error) {
				slot, acquired, err := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
					AgentID: "planner", Limit: 1, OwnerKind: store.AgentSlotOwnerPlanner,
					Board: "leased", OwnerID: "planner:test", TTL: time.Minute, Current: time.Now(),
				})
				if err != nil {
					return nil, err
				}
				if !acquired {
					return nil, errors.New("global agent lease was not acquired")
				}
				return func(ctx context.Context, coordination *store.Store) error {
					released, err := coordination.ReleaseGlobalAgentSlot(ctx, slot)
					if err == nil && !released {
						return errors.New("global agent lease changed before release")
					}
					return err
				}, nil
			},
		},
		{
			name: "workspace",
			seed: func(ctx context.Context, coordination *store.Store) (func(context.Context, *store.Store) error, error) {
				lease, acquired, err := coordination.AcquireGlobalWorkspaceLease(
					ctx, "leased", "run-test", filepath.Join(t.TempDir(), "workspace"),
				)
				if err != nil {
					return nil, err
				}
				if !acquired {
					return nil, errors.New("global workspace lease was not acquired")
				}
				return func(ctx context.Context, coordination *store.Store) error {
					released, err := coordination.ReleaseGlobalWorkspaceLease(ctx, lease)
					if err == nil && !released {
						return errors.New("global workspace lease changed before release")
					}
					return err
				}, nil
			},
		},
	}
	for _, test := range tests {
		for _, hardDelete := range []bool{false, true} {
			t.Run(test.name+"/"+map[bool]string{false: "archive", true: "hard-delete"}[hardDelete], func(t *testing.T) {
				ctx := context.Background()
				manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.Create(ctx, "leased", Update{}); err != nil {
					t.Fatal(err)
				}
				coordination, err := manager.OpenCoordinationStore(ctx)
				if err != nil {
					t.Fatal(err)
				}
				release, seedErr := test.seed(ctx, coordination)
				closeErr := coordination.Close()
				if seedErr != nil || closeErr != nil {
					t.Fatal(errors.Join(seedErr, closeErr))
				}

				if _, err := manager.Remove("leased", hardDelete); !errors.Is(err, store.ErrBoardBusy) {
					t.Fatalf("remove leased board error = %v, want ErrBoardBusy", err)
				}
				if !manager.Exists("leased") {
					t.Fatal("leased board was removed")
				}

				coordination, err = manager.OpenCoordinationStore(ctx)
				if err != nil {
					t.Fatal(err)
				}
				releaseErr := release(ctx, coordination)
				closeErr = coordination.Close()
				if releaseErr != nil || closeErr != nil {
					t.Fatal(errors.Join(releaseErr, closeErr))
				}
				if _, err := manager.Remove("leased", hardDelete); err != nil {
					t.Fatalf("remove board after lease release: %v", err)
				}
			})
		}
	}
}

func TestArchivedBoardTombstoneBlocksStaleLeasesUntilRecreated(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "reusable", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("reusable", false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("reusable", true); err == nil || !strings.Contains(err.Error(), "already archived") {
		t.Fatalf("second removal error = %v, want already archived", err)
	}

	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, leaseErr := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: "planner", Limit: 1, OwnerKind: store.AgentSlotOwnerPlanner,
		Board: "reusable", OwnerID: "stale-planner", TTL: time.Minute, Current: time.Now(),
	})
	closeErr := coordination.Close()
	if !errors.Is(leaseErr, store.ErrBoardRemovalInProgress) || closeErr != nil {
		t.Fatalf("stale lease error = %v, close = %v", leaseErr, closeErr)
	}

	if _, err := manager.Create(ctx, "reusable", Update{}); err != nil {
		t.Fatal(err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slot, acquired, leaseErr := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: "planner", Limit: 1, OwnerKind: store.AgentSlotOwnerPlanner,
		Board: "reusable", OwnerID: "new-planner", TTL: time.Minute, Current: time.Now(),
	})
	if leaseErr != nil || !acquired {
		coordination.Close()
		t.Fatalf("recreated board lease = %+v, acquired=%v, err=%v", slot, acquired, leaseErr)
	}
	_, releaseErr := coordination.ReleaseGlobalAgentSlot(ctx, slot)
	closeErr = coordination.Close()
	if releaseErr != nil || closeErr != nil {
		t.Fatal(errors.Join(releaseErr, closeErr))
	}
}

func TestBoardMutationLeaseSerializesMetadataChanges(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	original := "Original"
	if _, err := manager.Create(ctx, "locked", Update{Name: &original}); err != nil {
		t.Fatal(err)
	}

	lock, acquired, err := acquireBoardMutationLock(manager.boardMetadataLockPath("locked"), true)
	if err != nil || !acquired {
		t.Fatalf("acquire competing board mutation lease: acquired=%v err=%v", acquired, err)
	}

	changed := "Changed"
	if _, err := manager.Create(ctx, "locked", Update{Name: &changed}); !errors.Is(err, ErrBoardMutationInProgress) {
		t.Fatalf("create behind mutation lease error = %v", err)
	}
	if _, err := manager.Update("locked", Update{Name: &changed}); !errors.Is(err, ErrBoardMutationInProgress) {
		t.Fatalf("update behind mutation lease error = %v", err)
	}
	if _, err := manager.Remove("locked", false); !errors.Is(err, ErrBoardMutationInProgress) {
		t.Fatalf("remove behind mutation lease error = %v", err)
	}
	metadata, err := manager.Read("locked")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != original {
		t.Fatalf("locked metadata changed to %q", metadata.Name)
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update("locked", Update{Name: &changed}); err != nil {
		t.Fatalf("update after mutation lease release: %v", err)
	}
}

func TestBoardListIgnoresEmptyBoardDirectories(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "default", Update{}); err != nil {
		t.Fatal(err)
	}
	ghostRoot := filepath.Join(manager.boardsRoot, "ghost")
	if err := os.MkdirAll(filepath.Join(ghostRoot, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}

	listed, err := manager.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Slug != "default" {
		t.Fatalf("empty directory became a board: %+v", listed)
	}
	if fileExists(filepath.Join(ghostRoot, "autogora.db")) {
		t.Fatal("listing an empty board directory created a database")
	}
}

func TestCreateClearsTombstoneForExistingBoard(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "existing", Update{}); err != nil {
		t.Fatal(err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.AcquireBoardRemovalGuard(ctx, "existing"); err != nil {
		coordination.Close()
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
	local, err := manager.openStoreUnlocked(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.AcquireBoardRemovalGuard(ctx, "existing"); err != nil {
		local.Close()
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Create(ctx, "existing", Update{}); err != nil {
		t.Fatalf("retry existing board creation: %v", err)
	}
	local, err = manager.OpenStore(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.CreateTask(ctx, store.CreateTaskInput{Title: "write after recovery"}); err != nil {
		local.Close()
		t.Fatalf("local guard survived recreation: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slot, acquired, leaseErr := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: "planner", Limit: 1, OwnerKind: store.AgentSlotOwnerPlanner,
		Board: "existing", OwnerID: "planner-existing", TTL: time.Minute, Current: time.Now(),
	})
	if leaseErr != nil || !acquired {
		coordination.Close()
		t.Fatalf("lease after idempotent creation = %+v, acquired=%v err=%v", slot, acquired, leaseErr)
	}
	_, releaseErr := coordination.ReleaseGlobalAgentSlot(ctx, slot)
	closeErr := coordination.Close()
	if releaseErr != nil || closeErr != nil {
		t.Fatal(errors.Join(releaseErr, closeErr))
	}
}

func TestOpenStoreRecoversInterruptedRemovalGuards(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "recoverable", Update{}); err != nil {
		t.Fatal(err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordination.AcquireBoardRemovalGuard(ctx, "recoverable"); err != nil {
		coordination.Close()
		t.Fatal(err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}
	local, err := manager.openStoreUnlocked(ctx, "recoverable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.AcquireBoardRemovalGuard(ctx, "recoverable"); err != nil {
		local.Close()
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := manager.OpenStore(ctx, "recoverable")
	if err != nil {
		t.Fatalf("open board after interrupted removal: %v", err)
	}
	if _, err := recovered.CreateTask(ctx, store.CreateTaskInput{Title: "write after automatic recovery"}); err != nil {
		recovered.Close()
		t.Fatalf("write after automatic recovery: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guarded, guardErr := coordination.HasBoardRemovalGuard(ctx, "recoverable")
	closeErr := coordination.Close()
	if guardErr != nil || closeErr != nil {
		t.Fatal(errors.Join(guardErr, closeErr))
	}
	if guarded {
		t.Fatal("coordination tombstone survived automatic recovery")
	}
}

func TestBoardRemovalCleansTerminalRunGlobalLeases(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "finished", Update{}); err != nil {
		t.Fatal(err)
	}
	boardStore, err := manager.OpenStore(ctx, "finished")
	if err != nil {
		t.Fatal(err)
	}
	worker := "worker"
	task, err := boardStore.CreateTask(ctx, store.CreateTaskInput{
		Title: "finished work", Assignee: &worker, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		boardStore.Close()
		t.Fatal(err)
	}
	claim, err := boardStore.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		boardStore.Close()
		t.Fatalf("claim finished work: %+v, %v", claim, err)
	}
	if _, err := boardStore.FailRun(
		ctx,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		"finished for cleanup test",
		store.FailRunOptions{},
	); err != nil {
		boardStore.Close()
		t.Fatal(err)
	}
	if err := boardStore.Close(); err != nil {
		t.Fatal(err)
	}

	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runID := claim.Run.ID
	slot, acquired, err := coordination.AcquireGlobalAgentSlot(ctx, store.AcquireGlobalAgentSlotInput{
		AgentID: "codex", Limit: 1, OwnerKind: store.AgentSlotOwnerWorker,
		Board: "finished", RunID: &runID, OwnerID: "worker:" + runID, Current: time.Now(),
	})
	if err != nil || !acquired {
		coordination.Close()
		t.Fatalf("seed terminal agent lease = %+v, acquired=%v err=%v", slot, acquired, err)
	}
	workspaceLease, acquired, err := coordination.AcquireGlobalWorkspaceLease(
		ctx, "finished", runID, filepath.Join(t.TempDir(), "workspace"),
	)
	if err != nil || !acquired {
		coordination.Close()
		t.Fatalf("seed terminal workspace lease = %+v, acquired=%v err=%v", workspaceLease, acquired, err)
	}
	if err := coordination.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Remove("finished", false); err != nil {
		t.Fatalf("remove board with terminal global leases: %v", err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slots, slotErr := coordination.ListGlobalAgentSlotsForBoard(ctx, "finished")
	leases, leaseErr := coordination.ListGlobalWorkspaceLeases(ctx)
	closeErr := coordination.Close()
	if slotErr != nil || leaseErr != nil || closeErr != nil {
		t.Fatal(errors.Join(slotErr, leaseErr, closeErr))
	}
	if len(slots) != 0 {
		t.Fatalf("terminal agent leases remain: %+v", slots)
	}
	for _, lease := range leases {
		if lease.Board == "finished" {
			t.Fatalf("terminal workspace lease remains: %+v", lease)
		}
	}
}

func TestBoardLifecycleLockAllowsUpdatesAndBlocksRemoval(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "opened", Update{}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "opened")
	if err != nil {
		t.Fatal(err)
	}
	name := "Updated While Open"
	if _, err := manager.Update("opened", Update{Name: &name}); err != nil {
		opened.Close()
		t.Fatalf("metadata update while store is open: %v", err)
	}
	if _, err := manager.Remove("opened", false); !errors.Is(err, store.ErrBoardBusy) {
		opened.Close()
		t.Fatalf("remove while store is open error = %v", err)
	}
	if !manager.Exists("opened") {
		opened.Close()
		t.Fatal("open board was removed")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove("opened", false); err != nil {
		t.Fatalf("remove after store close: %v", err)
	}
}

func TestSwitchDoesNotSelectBoardDuringMutation(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "first", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "second", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Switch("first"); err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := acquireBoardMutationLock(manager.boardMetadataLockPath("second"), true)
	if err != nil || !acquired {
		t.Fatalf("lock second board mutation: acquired=%v err=%v", acquired, err)
	}
	if _, err := manager.Switch("second"); !errors.Is(err, ErrBoardMutationInProgress) {
		lock.Close()
		t.Fatalf("switch during board mutation error = %v", err)
	}
	if current := manager.Current(); current != "first" {
		lock.Close()
		t.Fatalf("failed switch changed current board to %q", current)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Switch("second"); err != nil {
		t.Fatalf("switch after mutation lock release: %v", err)
	}
}

func TestRemovalCurrentResetPreservesNewerSelection(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "removed", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "newer", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Switch("removed"); err != nil {
		t.Fatal(err)
	}

	currentLock, acquired, err := acquireBoardMutationLock(manager.currentLockPath(), true)
	if err != nil || !acquired {
		t.Fatalf("lock current selection: acquired=%v err=%v", acquired, err)
	}
	resetResult := make(chan error, 1)
	go func() {
		resetResult <- manager.resetCurrentAfterRemoval("removed")
	}()
	if err := manager.writeCurrentSelection("newer"); err != nil {
		currentLock.Close()
		t.Fatal(err)
	}
	if err := currentLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-resetResult; err != nil {
		t.Fatal(err)
	}
	if current := manager.Current(); current != "newer" {
		t.Fatalf("removal reset overwrote newer selection with %q", current)
	}
}

func TestBoardRemovalRejectsLiveTerminalProcess(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "orphaned", Update{}); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "orphaned")
	if err != nil {
		t.Fatal(err)
	}
	worker := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "terminal but alive", Assignee: &worker, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		opened.Close()
		t.Fatalf("claim terminal process task: %+v, %v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if _, err := opened.RecordSpawn(ctx, scope, os.Getpid(), filepath.Join(t.TempDir(), "worker.log")); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := opened.FailRun(ctx, scope, "externally marked terminal", store.FailRunOptions{}); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Remove("orphaned", false); !errors.Is(err, store.ErrBoardBusy) {
		t.Fatalf("remove live terminal process error = %v", err)
	}
	if !manager.Exists("orphaned") {
		t.Fatal("board with live terminal process was removed")
	}
}
