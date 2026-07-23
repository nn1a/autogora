package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestRecordSpawnPersistsProcessIdentity(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "process-identity.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "identity", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	identity := "test-boot:test-start"
	if _, err := opened.RecordSpawnWithIdentity(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, 1234, filepath.Join(t.TempDir(), "worker.log"), identity); err != nil {
		t.Fatal(err)
	}
	stored, err := opened.GetRunProcessIdentity(ctx, claim.Run.ID)
	if err != nil || stored == nil || *stored != identity {
		t.Fatalf("stored identity = %v, err=%v", stored, err)
	}
}

func TestProcessSafetyStateSurvivesEventGarbageCollection(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "process-safety.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "durable process safety", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	scope := RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManagedWithPolicy(ctx, scope, false); err != nil {
		t.Fatal(err)
	}
	identity := "test-boot:durable-start"
	if _, err := opened.RecordSpawnWithIdentity(ctx, scope, 1234, filepath.Join(t.TempDir(), "worker.log"), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.DeferTimedOutRun(ctx, claim.Run.ID, 45, "runtime limit"); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(ctx, "UPDATE task_events SET created_at = '2000-01-01T00:00:00.000Z'"); err != nil {
		t.Fatal(err)
	}
	if removed, err := opened.GarbageCollectEvents(ctx, 0); err != nil || removed == 0 {
		t.Fatalf("garbage collect events: removed=%d, err=%v", removed, err)
	}

	storedIdentity, err := opened.GetRunProcessIdentity(ctx, claim.Run.ID)
	if err != nil || storedIdentity == nil || *storedIdentity != identity {
		t.Fatalf("identity after GC = %v, err=%v", storedIdentity, err)
	}
	policy, err := opened.GetManagedRunWritePolicy(ctx, claim.Run.ID)
	if err != nil || policy == nil || *policy {
		t.Fatalf("policy after GC = %v, err=%v", policy, err)
	}
	reclaim, err := opened.GetDeferredReclaim(ctx, claim.Run.ID)
	if err != nil || reclaim == nil || reclaim.Outcome != model.RunStatusTimedOut || !reclaim.CountFailure || reclaim.Reason != "runtime limit" {
		t.Fatalf("reclaim after GC = %#v, err=%v", reclaim, err)
	}
}

func TestRecordSpawnRejectsCommittedReclaim(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "spawn-reclaim.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "do not spawn", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	if _, err := opened.DeferReclaim(ctx, claim.Run.ID, 30, "operator request"); err != nil {
		t.Fatal(err)
	}
	_, err = opened.RecordSpawnWithIdentity(ctx,
		RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		1234, filepath.Join(t.TempDir(), "worker.log"), "identity",
	)
	if !errors.Is(err, ErrRunTerminationPending) {
		t.Fatalf("record spawn error = %v, want ErrRunTerminationPending", err)
	}
	run, err := getRun(ctx, opened.db, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PID != nil || run.LogPath != nil {
		t.Fatalf("rejected spawn mutated run: %#v", run)
	}
}

func TestConcurrentSpawnAndReclaimHaveSerializableOrdering(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "spawn-reclaim-race.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"

	for iteration := 0; iteration < 20; iteration++ {
		task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "race", Assignee: &assignee, Runtime: model.RuntimeCodex})
		if err != nil {
			t.Fatal(err)
		}
		claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
		if err != nil || claim == nil {
			t.Fatalf("claim %d: %+v, %v", iteration, claim, err)
		}
		start := make(chan struct{})
		var spawnErr, reclaimErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func(iteration int) {
			defer wait.Done()
			<-start
			_, spawnErr = opened.RecordSpawnWithIdentity(ctx,
				RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
				1000+iteration, filepath.Join(t.TempDir(), "worker.log"), "identity",
			)
		}(iteration)
		go func() {
			defer wait.Done()
			<-start
			_, reclaimErr = opened.DeferReclaim(ctx, claim.Run.ID, 30, "operator request")
		}()
		close(start)
		wait.Wait()
		if reclaimErr != nil {
			t.Fatalf("reclaim %d: %v", iteration, reclaimErr)
		}
		if spawnErr != nil && !errors.Is(spawnErr, ErrRunTerminationPending) {
			t.Fatalf("spawn %d: %v", iteration, spawnErr)
		}
		if spawnErr == nil {
			var spawnEvent, reclaimEvent int64
			if err := opened.db.QueryRowContext(ctx,
				"SELECT id FROM task_events WHERE run_id = ? AND kind = 'spawned'", claim.Run.ID,
			).Scan(&spawnEvent); err != nil {
				t.Fatal(err)
			}
			if err := opened.db.QueryRowContext(ctx,
				"SELECT id FROM task_events WHERE run_id = ? AND kind = 'reclaim_deferred'", claim.Run.ID,
			).Scan(&reclaimEvent); err != nil {
				t.Fatal(err)
			}
			if spawnEvent >= reclaimEvent {
				t.Fatalf("iteration %d allowed spawn event %d after reclaim event %d", iteration, spawnEvent, reclaimEvent)
			}
		}
	}
}
