package agenthealth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestRouterSharesGlobalHealthAndIsolatesBoardHealth(t *testing.T) {
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

	message := "subscription quota exhausted"
	if _, err := New(manager, alpha).Set(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthRateLimited, LastError: &message,
	}, true); err != nil {
		t.Fatal(err)
	}
	shared, err := New(manager, beta).Get(ctx, "global-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if shared.Status != model.AgentHealthRateLimited || shared.LastError == nil || *shared.LastError != message {
		t.Fatalf("shared global health = %#v", shared)
	}
	localView, err := New(manager, beta).Get(ctx, "global-worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if localView.Status != model.AgentHealthUnknown {
		t.Fatalf("global health leaked into board-local lookup = %#v", localView)
	}

	if _, err := New(manager, alpha).Set(ctx, store.SetAgentHealthInput{
		AgentID: "board-worker", Status: model.AgentHealthUnhealthy,
	}, false); err != nil {
		t.Fatal(err)
	}
	isolated, err := New(manager, beta).Get(ctx, "board-worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Status != model.AgentHealthUnknown {
		t.Fatalf("board-local health crossed boards = %#v", isolated)
	}
}

func TestRouterRejectsOlderGlobalWorkerObservationAcrossBoards(t *testing.T) {
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

	alphaHealth := New(manager, alpha)
	betaHealth := New(manager, beta)
	older, err := alphaHealth.Begin(ctx, "global-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := betaHealth.Begin(ctx, "global-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	cooldown := "2030-01-02T03:04:05Z"
	if update, err := betaHealth.Apply(ctx, newer, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthRateLimited, CooldownUntil: &cooldown,
	}, true); err != nil || !update.Applied {
		t.Fatalf("apply newer global failure: update=%+v err=%v", update, err)
	}

	record, err := alphaHealth.RecordWorkerObservation(ctx, older, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthReady,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if record.Applied {
		t.Fatalf("stale worker success was applied: %+v", record)
	}
	shared, err := betaHealth.Get(ctx, "global-worker", true)
	if err != nil {
		t.Fatal(err)
	}
	if shared.Status != model.AgentHealthRateLimited || shared.CooldownUntil == nil {
		t.Fatalf("stale success replaced shared cooldown: %+v", shared)
	}
	local, err := alphaHealth.Get(ctx, "global-worker", false)
	if err != nil {
		t.Fatal(err)
	}
	if local.Status != model.AgentHealthUnknown {
		t.Fatalf("stale authoritative result leaked into local audit: %+v", local)
	}
}

func TestRouterRecordsGlobalWorkerWithoutCrossBoardRunReference(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alpha.Close()
	assignee := "global-worker"
	task, err := alpha.CreateTask(ctx, store.CreateTaskInput{
		Title: "global worker audit", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := alpha.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%#v err=%v", claim, err)
	}
	record, err := New(manager, alpha).RecordWorker(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthReady, LastRunID: &claim.Run.ID,
	}, true)
	if err != nil || record.AuditError != nil {
		t.Fatal(errors.Join(err, record.AuditError))
	}

	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	global, getErr := coordination.GetAgentHealth(ctx, "global-worker")
	closeErr := coordination.Close()
	if getErr != nil || closeErr != nil {
		t.Fatal(errors.Join(getErr, closeErr))
	}
	if global.Status != model.AgentHealthReady || global.LastRunID != nil {
		t.Fatalf("global worker health retained board run reference = %#v", global)
	}
	local, err := alpha.GetAgentHealth(ctx, "global-worker")
	if err != nil {
		t.Fatal(err)
	}
	if local.LastRunID == nil || *local.LastRunID != claim.Run.ID {
		t.Fatalf("local worker audit lost run reference = %#v", local)
	}
}

func TestRouterPreservesDefaultBoardRunReference(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "global-worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "default worker linkage", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%#v err=%v", claim, err)
	}
	record, err := New(manager, opened).RecordWorker(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthReady, LastRunID: &claim.Run.ID,
	}, true)
	if err != nil || record.AuditError != nil {
		t.Fatal(errors.Join(err, record.AuditError))
	}
	health, err := opened.GetAgentHealth(ctx, "global-worker")
	if err != nil {
		t.Fatal(err)
	}
	if health.LastRunID == nil || *health.LastRunID != claim.Run.ID {
		t.Fatalf("default global health lost its valid run reference: %#v", health)
	}
}

func TestRouterKeepsLocalAuditFailureNonAuthoritative(t *testing.T) {
	ctx := context.Background()
	manager, err := boards.NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}
	runID := "board-run-is-only-for-the-optional-audit"
	record, authoritativeErr := New(manager, alpha).RecordWorker(ctx, store.SetAgentHealthInput{
		AgentID: "global-worker", Status: model.AgentHealthUnhealthy, LastRunID: &runID,
	}, true)
	if authoritativeErr != nil {
		t.Fatalf("authoritative global write failed with local audit: %v", authoritativeErr)
	}
	if record.AuditError == nil {
		t.Fatal("closed local store did not report an audit failure")
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	global, getErr := coordination.GetAgentHealth(ctx, "global-worker")
	closeErr := coordination.Close()
	if getErr != nil || closeErr != nil {
		t.Fatal(errors.Join(getErr, closeErr))
	}
	if global.Status != model.AgentHealthUnhealthy || global.LastRunID != nil {
		t.Fatalf("authoritative global observation was not preserved: %#v", global)
	}
}
