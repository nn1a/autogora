package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func agentSlotString(value string) *string { return &value }

func TestGlobalAgentSlotConcurrentLimitOneHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "coordination.db")
	stores := make([]*Store, 2)
	for index := range stores {
		var err error
		stores[index], err = Open(dbPath, "default", "")
		if err != nil {
			t.Fatal(err)
		}
		defer stores[index].Close()
	}

	current := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)
	type result struct {
		slot     GlobalAgentSlot
		acquired bool
		err      error
	}
	results := make([]result, len(stores))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, opened := range stores {
		wait.Add(1)
		go func(index int, opened *Store) {
			defer wait.Done()
			<-start
			runID := "run-a"
			ownerID := "worker-a"
			if index == 1 {
				runID = "run-b"
				ownerID = "worker-b"
			}
			results[index].slot, results[index].acquired, results[index].err = opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
				AgentID: "codex", Limit: 1, OwnerKind: AgentSlotOwnerWorker,
				Board: "alpha", RunID: &runID, OwnerID: ownerID, Current: current,
			})
		}(index, opened)
	}
	close(start)
	wait.Wait()

	winners := 0
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("contender %d: %v", index, result.err)
		}
		if result.acquired {
			winners++
			if result.slot.Slot != 1 || result.slot.LeaseToken == "" {
				t.Fatalf("winner slot = %+v", result.slot)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("acquired winners = %d, want 1: %+v", winners, results)
	}
	slots, err := stores[0].ListGlobalAgentSlots(ctx, "codex")
	if err != nil || len(slots) != 1 {
		t.Fatalf("persisted slots = %+v, %v", slots, err)
	}
}

func TestGlobalAgentSlotLoweredLimitCountsSlotsAboveLimit(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	acquired := make([]GlobalAgentSlot, 0, 3)
	for index := 1; index <= 3; index++ {
		runID := "run-" + string(rune('0'+index))
		slot, won, err := opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
			AgentID: "claude", Limit: 3, OwnerKind: AgentSlotOwnerWorker,
			Board: "alpha", RunID: &runID, OwnerID: "owner-" + runID, Current: current,
		})
		if err != nil || !won || slot.Slot != index {
			t.Fatalf("acquire slot %d = %+v, acquired=%v, err=%v", index, slot, won, err)
		}
		acquired = append(acquired, slot)
	}
	for _, slot := range acquired[:2] {
		if released, err := opened.ReleaseGlobalAgentSlot(ctx, slot); err != nil || !released {
			t.Fatalf("release slot %d = %v, %v", slot.Slot, released, err)
		}
	}

	runID := "run-new"
	slot, won, err := opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "claude", Limit: 1, OwnerKind: AgentSlotOwnerWorker,
		Board: "beta", RunID: &runID, OwnerID: "owner-new", Current: current,
	})
	if err != nil || won || slot != (GlobalAgentSlot{}) {
		t.Fatalf("lowered limit admitted another owner: slot=%+v acquired=%v err=%v", slot, won, err)
	}
	slots, err := opened.ListGlobalAgentSlots(ctx, "claude")
	if err != nil || len(slots) != 1 || slots[0].Slot != 3 {
		t.Fatalf("remaining high slot = %+v, %v", slots, err)
	}
}

func TestGlobalAgentSlotExactReleaseProtectsReacquiredLease(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	runID := "run-a"
	input := AcquireGlobalAgentSlotInput{
		AgentID: "codex", Limit: 1, OwnerKind: AgentSlotOwnerWorker,
		Board: "alpha", RunID: &runID, OwnerID: "worker-a", Current: current,
	}
	initial, acquired, err := opened.AcquireGlobalAgentSlot(ctx, input)
	if err != nil || !acquired {
		t.Fatalf("initial acquire = %+v, acquired=%v, err=%v", initial, acquired, err)
	}
	repeated, acquired, err := opened.AcquireGlobalAgentSlot(ctx, input)
	if err != nil || !acquired || repeated.LeaseToken != initial.LeaseToken {
		t.Fatalf("idempotent acquire = %+v, acquired=%v, err=%v", repeated, acquired, err)
	}
	wrong := initial
	wrong.LeaseToken = "stale-token"
	if released, err := opened.ReleaseGlobalAgentSlot(ctx, wrong); err != nil || released {
		t.Fatalf("wrong-token release = %v, %v", released, err)
	}
	wrong = initial
	wrong.Board = "other-board"
	if released, err := opened.ReleaseGlobalAgentSlot(ctx, wrong); err != nil || released {
		t.Fatalf("wrong-owner release = %v, %v", released, err)
	}
	if released, err := opened.ReleaseGlobalAgentSlot(ctx, initial); err != nil || !released {
		t.Fatalf("initial release = %v, %v", released, err)
	}
	replacement, acquired, err := opened.AcquireGlobalAgentSlot(ctx, input)
	if err != nil || !acquired || replacement.LeaseToken == initial.LeaseToken {
		t.Fatalf("replacement acquire = %+v, acquired=%v, err=%v", replacement, acquired, err)
	}
	if released, err := opened.ReleaseGlobalAgentSlot(ctx, initial); err != nil || released {
		t.Fatalf("stale ABA release = %v, %v", released, err)
	}
	slots, err := opened.ListGlobalAgentSlots(ctx, "codex")
	if err != nil || len(slots) != 1 || slots[0].LeaseToken != replacement.LeaseToken {
		t.Fatalf("replacement lease was not preserved: %+v, %v", slots, err)
	}
}

func TestGlobalAgentSlotExpiryCleanupSkipsWorkersAndRunsDuringAcquire(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	current := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)

	planner, acquired, err := opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "claude", Limit: 1, OwnerKind: AgentSlotOwnerPlanner,
		Board: "alpha", OwnerID: "planner-a", TTL: time.Minute, Current: current,
	})
	if err != nil || !acquired || planner.ExpiresAt == nil || planner.RunID != nil {
		t.Fatalf("planner acquire = %+v, acquired=%v, err=%v", planner, acquired, err)
	}
	replacement, acquired, err := opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "claude", Limit: 1, OwnerKind: AgentSlotOwnerJudge,
		Board: "beta", OwnerID: "judge-a", TTL: time.Minute, Current: current.Add(time.Minute),
	})
	if err != nil || !acquired || replacement.Slot != planner.Slot || replacement.LeaseToken == planner.LeaseToken {
		t.Fatalf("acquire did not clean expired planner: %+v, acquired=%v, err=%v", replacement, acquired, err)
	}

	runID := "run-worker"
	worker, acquired, err := opened.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "codex", Limit: 1, OwnerKind: AgentSlotOwnerWorker,
		Board: "alpha", RunID: &runID, OwnerID: "worker-a", Current: current,
	})
	if err != nil || !acquired || worker.ExpiresAt != nil {
		t.Fatalf("worker acquire = %+v, acquired=%v, err=%v", worker, acquired, err)
	}
	removed, err := opened.CleanupExpiredGlobalAgentSlots(ctx, current.Add(2*time.Minute))
	if err != nil || removed != 1 {
		t.Fatalf("explicit expiry cleanup removed %d, err=%v", removed, err)
	}
	workers, err := opened.ListGlobalAgentSlots(ctx, "codex")
	if err != nil || len(workers) != 1 || workers[0].LeaseToken != worker.LeaseToken {
		t.Fatalf("non-expiring worker was removed: %+v, %v", workers, err)
	}
}

func TestGlobalAgentSlotValidationAndCoordinationScope(t *testing.T) {
	ctx := context.Background()
	local, err := Open(":memory:", "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	current := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	runID := "run-a"
	validWorker := AcquireGlobalAgentSlotInput{
		AgentID: "codex", Limit: 1, OwnerKind: AgentSlotOwnerWorker,
		Board: "alpha", RunID: &runID, OwnerID: "worker-a", Current: current,
	}
	if _, _, err := local.AcquireGlobalAgentSlot(ctx, validWorker); err == nil {
		t.Fatal("board-local store accepted a global agent slot")
	}
	if _, err := local.ListGlobalAgentSlots(ctx, "codex"); err == nil {
		t.Fatal("board-local store listed global agent slots")
	}
	if _, err := local.CleanupExpiredGlobalAgentSlots(ctx, current); err == nil {
		t.Fatal("board-local store cleaned global agent slots")
	}

	coordination, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer coordination.Close()
	missingRun := validWorker
	missingRun.RunID = nil
	if _, _, err := coordination.AcquireGlobalAgentSlot(ctx, missingRun); err == nil {
		t.Fatal("worker without a run ID was accepted")
	}
	expiringWorker := validWorker
	expiringWorker.TTL = time.Minute
	if _, _, err := coordination.AcquireGlobalAgentSlot(ctx, expiringWorker); err == nil {
		t.Fatal("expiring worker was accepted")
	}
	if _, _, err := coordination.AcquireGlobalAgentSlot(ctx, AcquireGlobalAgentSlotInput{
		AgentID: "codex", Limit: 1, OwnerKind: AgentSlotOwnerPlanner,
		Board: "alpha", OwnerID: "planner-a", Current: current,
	}); err == nil {
		t.Fatal("non-expiring planner was accepted")
	}

	rows, err := coordination.db.QueryContext(ctx, "PRAGMA foreign_key_list(global_agent_slots)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("global agent slot table unexpectedly has a foreign key")
	}
}
