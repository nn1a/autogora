package agentcoord

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

func claimedWorkerRun(t *testing.T, ctx context.Context, opened *store.Store, title string) *model.ClaimedTask {
	t.Helper()
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: title, Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim %s: %+v, %v", title, claim, err)
	}
	return claim
}

func TestWorkerSlotReclaimsOnlyVerifiedTerminalOwner(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, board := range []string{"alpha", "beta"} {
		if _, err := manager.Create(ctx, board, boards.Update{}); err != nil {
			t.Fatal(err)
		}
	}
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alpha.Close()
	beta, err := manager.OpenStore(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	defer beta.Close()
	alphaRun := claimedWorkerRun(t, ctx, alpha, "alpha worker")
	betaRun := claimedWorkerRun(t, ctx, beta, "beta worker")
	coordinator := New(manager)
	alphaLease, acquired, err := coordinator.AcquireWorker(ctx, "shared-agent", 1, "alpha", alphaRun.Run.ID)
	if err != nil || !acquired {
		t.Fatalf("alpha slot = %+v, acquired=%v, err=%v", alphaLease, acquired, err)
	}
	if lease, acquired, err := coordinator.AcquireWorker(ctx, "shared-agent", 1, "beta", betaRun.Run.ID); err != nil || acquired || lease != nil {
		t.Fatalf("running owner was reclaimed: %+v, acquired=%v, err=%v", lease, acquired, err)
	}
	process := exec.Command(os.Args[0], "-test.run=TestAgentCoordProcessHelper")
	process.Env = append(os.Environ(), "AUTOGORA_AGENTCOORD_HELPER=1")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = process.Process.Kill()
		_, _ = process.Process.Wait()
	}()
	identity, err := processidentity.Capture(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.RunScope{RunID: alphaRun.Run.ID, ClaimToken: alphaRun.ClaimToken}
	if _, err := alpha.RecordSpawnWithIdentity(ctx, scope, process.Process.Pid, filepath.Join(t.TempDir(), "worker.log"), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.FailRun(ctx, scope, "finished", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if lease, acquired, err := coordinator.AcquireWorker(ctx, "shared-agent", 1, "beta", betaRun.Run.ID); err != nil || acquired || lease != nil {
		t.Fatalf("terminal owner with live process was reclaimed: %+v, acquired=%v, err=%v", lease, acquired, err)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	betaLease, acquired, err := coordinator.AcquireWorker(ctx, "shared-agent", 1, "beta", betaRun.Run.ID)
	if err != nil || !acquired || betaLease == nil || betaLease.Slot.Board != "beta" {
		t.Fatalf("terminal owner was not reclaimed: %+v, acquired=%v, err=%v", betaLease, acquired, err)
	}
	if err := alphaLease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := coordination.ListGlobalAgentSlots(ctx, "shared-agent")
	coordination.Close()
	if err != nil || len(slots) != 1 || slots[0].LeaseToken != betaLease.Slot.LeaseToken {
		t.Fatalf("stale worker release changed current owner: %+v, %v", slots, err)
	}
}

func TestAgentCoordProcessHelper(t *testing.T) {
	if os.Getenv("AUTOGORA_AGENTCOORD_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestEphemeralSlotBoundsExpiryAndReleasesAfterCancellation(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := New(manager)
	before := time.Now().UTC()
	plannerLease, acquired, err := coordinator.AcquireEphemeral(ctx, "shared-agent", 1, store.AgentSlotOwnerPlanner, "default", time.Hour)
	if err != nil || !acquired || plannerLease == nil || plannerLease.Slot.ExpiresAt == nil {
		t.Fatalf("planner slot = %+v, acquired=%v, err=%v", plannerLease, acquired, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, *plannerLease.Slot.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := expiresAt.Sub(before); remaining > MaxEphemeralSlotTTL+time.Second || remaining < MaxEphemeralSlotTTL-time.Second {
		t.Fatalf("bounded planner expiry = %s", remaining)
	}
	if lease, acquired, err := coordinator.AcquireEphemeral(ctx, "shared-agent", 1, store.AgentSlotOwnerJudge, "default", time.Minute); err != nil || acquired || lease != nil {
		t.Fatalf("judge bypassed planner capacity: %+v, acquired=%v, err=%v", lease, acquired, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := plannerLease.Release(canceled); err != nil {
		t.Fatalf("canceled release: %v", err)
	}
	judgeLease, acquired, err := coordinator.AcquireEphemeral(ctx, "shared-agent", 1, store.AgentSlotOwnerJudge, "default", time.Second)
	if err != nil || !acquired || judgeLease == nil {
		t.Fatalf("judge slot after release = %+v, acquired=%v, err=%v", judgeLease, acquired, err)
	}
	if judgeLease.Slot.ExpiresAt == nil {
		t.Fatal("judge slot has no bounded expiry")
	}
}

func TestEphemeralSlotOutlivesMaximumPlannerTimeout(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := New(manager)
	current := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return current }
	const maximumPlannerTimeout = 10 * time.Minute
	requestedTTL := maximumPlannerTimeout + EphemeralSlotCleanupGrace
	if requestedTTL >= MaxEphemeralSlotTTL {
		t.Fatalf("maximum planner TTL %s must remain below ephemeral bound %s", requestedTTL, MaxEphemeralSlotTTL)
	}

	lease, acquired, err := coordinator.AcquireEphemeral(
		ctx, "maximum-timeout-agent", 1, store.AgentSlotOwnerPlanner, "default", requestedTTL,
	)
	if err != nil || !acquired || lease == nil || lease.Slot.ExpiresAt == nil {
		t.Fatalf("maximum-timeout slot = %+v, acquired=%v, err=%v", lease, acquired, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, *lease.Slot.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if want := current.Add(requestedTTL); !expiresAt.Equal(want) {
		t.Fatalf("maximum-timeout slot expires at %s, want %s", expiresAt, want)
	}
	if !expiresAt.After(current.Add(maximumPlannerTimeout)) {
		t.Fatalf("slot expiry %s does not outlive planner timeout", expiresAt)
	}
}
