package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func revisionPointer(value int64) *int64 { return &value }

func TestCoordinationIncidentDedupRefreshAndProposalLifecycle(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	root, err := opened.CreateTask(ctx, CreateTaskInput{Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "blocked task"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := opened.CreateTask(ctx, CreateTaskInput{Title: "next task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, root.Task.ID, task.Task.ID); err != nil {
		t.Fatal(err)
	}

	incident, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		RootTaskID: &root.Task.ID, TaskID: &task.Task.ID,
		Trigger: model.CoordinationTriggerRepeatedBlock, Severity: model.CoordinationSeverityWarning,
		ExpectedGraphRevision: revisionPointer(1), Summary: "Task blocked repeatedly",
		Details: []byte(`{"recurrences":3}`),
	})
	if err != nil || !created {
		t.Fatalf("create incident: created=%v value=%+v err=%v", created, incident, err)
	}
	if incident.GraphRevision != 1 || incident.Status != model.CoordinationIncidentOpen {
		t.Fatalf("incident = %+v", incident)
	}

	summary := "Task is still blocked"
	updatedIncident, err := opened.UpdateCoordinationIncident(ctx, incident.ID, UpdateCoordinationIncidentInput{
		ExpectedUpdatedAt: &incident.UpdatedAt, Summary: &summary,
	})
	if err != nil || updatedIncident.Summary != summary {
		t.Fatalf("update incident: %+v %v", updatedIncident, err)
	}

	// An unrelated topology update must refresh the existing active incident
	// instead of opening a duplicate at the new revision.
	if _, err := opened.LinkTasks(ctx, task.Task.ID, next.Task.ID); err != nil {
		t.Fatal(err)
	}
	refreshed, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		RootTaskID: &root.Task.ID, TaskID: &task.Task.ID,
		Trigger: model.CoordinationTriggerRepeatedBlock, Severity: model.CoordinationSeverityError,
		ExpectedGraphRevision: revisionPointer(2), Summary: "Repeated block remains",
		Details: []byte(`{"recurrences":4}`),
	})
	if err != nil || created {
		t.Fatalf("refresh incident: created=%v value=%+v err=%v", created, refreshed, err)
	}
	if refreshed.ID != incident.ID || refreshed.GraphRevision != 2 ||
		refreshed.Severity != model.CoordinationSeverityError || refreshed.Summary != "Repeated block remains" {
		t.Fatalf("active incident was not refreshed: %+v", refreshed)
	}
	incidents, err := opened.ListCoordinationIncidents(ctx, CoordinationIncidentFilter{
		Status: model.CoordinationIncidentOpen,
	})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("active incident dedupe list = %+v, %v", incidents, err)
	}

	claimTime := time.Now().UTC()
	currentIncident, claimed, err := opened.ClaimCoordinationIncident(ctx, refreshed.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(2),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v value=%+v err=%v", claimed, currentIncident, err)
	}
	proposal, created, err := opened.CreateCoordinationProposal(ctx, CreateCoordinationProposalInput{
		IncidentID: currentIncident.ID, CoordinatorAgent: "codex-coordinator",
		CoordinatorModel: "gpt-5.4", CoordinatorProvider: "openai",
		ExpectedGraphRevision: revisionPointer(2),
		ClaimToken:            currentIncident.ClaimToken,
		Current:               claimTime.Add(time.Second),
		Summary:               "Add a conflict-resolution task",
		Rationale:             "A dedicated integration step can preserve both changes.",
		Actions:               []byte(`[]`),
	})
	if err != nil || !created {
		t.Fatalf("create proposal: created=%v value=%+v err=%v", created, proposal, err)
	}
	newSummary := "Add and gate a conflict-resolution task"
	newActions := jsonRaw(`[]`)
	proposal, err = opened.UpdateCoordinationProposal(ctx, proposal.ID, UpdateCoordinationProposalInput{
		ExpectedStatus: model.CoordinationProposalDraft, ExpectedGraphRevision: revisionPointer(2),
		ClaimToken: currentIncident.ClaimToken, Current: claimTime.Add(time.Second),
		Summary: &newSummary, Actions: &newActions,
	})
	if err != nil || proposal.Summary != newSummary {
		t.Fatalf("update proposal: %+v %v", proposal, err)
	}

	proposalTransitions := []model.CoordinationProposalStatus{
		model.CoordinationProposalValidating,
		model.CoordinationProposalValidated,
	}
	for _, status := range proposalTransitions {
		proposal, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, TransitionCoordinationProposalInput{
			ExpectedStatus: proposal.Status, Status: status, ExpectedGraphRevision: revisionPointer(2),
			ClaimToken: currentIncident.ClaimToken, Current: claimTime.Add(time.Second),
		})
		if err != nil {
			t.Fatalf("transition proposal to %s: %v", status, err)
		}
	}
	approval, err := opened.RequestCoordinationApproval(ctx, proposal.ID, RequestCoordinationApprovalInput{
		ExpectedGraphRevision: revisionPointer(2),
		ClaimToken:            currentIncident.ClaimToken,
		Current:               claimTime.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("request proposal approval: %v", err)
	}
	approval, err = opened.ApproveCoordinationProposal(ctx, proposal.ID, ApproveCoordinationProposalInput{
		ExpectedUpdatedAt:     approval.Proposal.UpdatedAt,
		ExpectedGraphRevision: revisionPointer(2),
	})
	if err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	applied, err := opened.ApplyCoordinationProposal(ctx, proposal.ID, ApplyCoordinationProposalInput{
		Authorization:         CoordinationApplyApproved,
		ExpectedGraphRevision: revisionPointer(2),
		Current:               claimTime.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("apply proposal: %v", err)
	}
	proposal, currentIncident = applied.Proposal, applied.Incident
	if proposal.AppliedAt == nil {
		t.Fatal("applied proposal has no appliedAt")
	}
	if currentIncident.Status != model.CoordinationIncidentResolved {
		t.Fatalf("applied incident = %+v", currentIncident)
	}
	proposals, err := opened.ListCoordinationProposals(ctx, CoordinationProposalFilter{
		IncidentID: incident.ID, Status: model.CoordinationProposalApplied,
	})
	if err != nil || len(proposals) != 1 || proposals[0].Summary != newSummary {
		t.Fatalf("applied proposal list = %+v, %v", proposals, err)
	}

	reopened, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		RootTaskID: &root.Task.ID, TaskID: &task.Task.ID,
		Trigger: model.CoordinationTriggerRepeatedBlock, Severity: model.CoordinationSeverityWarning,
		ExpectedGraphRevision: revisionPointer(2), Summary: "Block recurred after resolution",
	})
	if err != nil || !created || reopened.ID == incident.ID {
		t.Fatalf("terminal incident did not allow a new occurrence: created=%v value=%+v err=%v", created, reopened, err)
	}
}

func jsonRaw(value string) json.RawMessage { return json.RawMessage(value) }

func TestCoordinationIncidentClaimReclaimsAfterCrashAndRejectsStaleOwner(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Summary: "Graph has no runnable task",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimTime := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	first, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !claimed || first.ClaimToken == "" || first.ClaimExpiresAt == nil {
		t.Fatalf("first claim: claimed=%v value=%+v err=%v", claimed, first, err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "claimToken") || strings.Contains(string(encoded), first.ClaimToken) {
		t.Fatalf("claim token leaked through JSON: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"claimExpiresAt"`) {
		t.Fatalf("claim expiry missing from JSON: %s", encoded)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	beforeExpiry, claimed, err := reopened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
		Current:               claimTime.Add(time.Minute - time.Nanosecond),
	})
	if err != nil || claimed || beforeExpiry.ClaimToken != first.ClaimToken {
		t.Fatalf("live crash claim was stolen: claimed=%v value=%+v err=%v", claimed, beforeExpiry, err)
	}
	_, err = reopened.TransitionCoordinationIncident(ctx, incident.ID, TransitionCoordinationIncidentInput{
		ExpectedStatus: model.CoordinationIncidentCoordinating,
		Status:         model.CoordinationIncidentResolved,
		ClaimToken:     first.ClaimToken,
		Current:        claimTime.Add(time.Minute),
	})
	if !errors.Is(err, ErrCoordinationClaimExpired) {
		t.Fatalf("expired caller transition error = %v", err)
	}

	second, claimed, err := reopened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
		Current:               claimTime.Add(time.Minute),
	})
	if err != nil || !claimed || second.ClaimToken == "" || second.ClaimToken == first.ClaimToken {
		t.Fatalf("expired crash claim not reclaimed: claimed=%v value=%+v err=%v", claimed, second, err)
	}
	_, err = reopened.TransitionCoordinationIncident(ctx, incident.ID, TransitionCoordinationIncidentInput{
		ExpectedStatus: model.CoordinationIncidentCoordinating,
		Status:         model.CoordinationIncidentResolved,
		ClaimToken:     first.ClaimToken,
		Current:        claimTime.Add(time.Minute + time.Second),
	})
	if !errors.Is(err, ErrCoordinationClaimNotOwner) {
		t.Fatalf("stale owner transition error = %v", err)
	}
	stillClaimed, err := reopened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillClaimed.ClaimToken != second.ClaimToken || stillClaimed.Status != model.CoordinationIncidentCoordinating {
		t.Fatalf("stale owner changed reclaimed incident: %+v", stillClaimed)
	}
	resolved, err := reopened.TransitionCoordinationIncident(ctx, incident.ID, TransitionCoordinationIncidentInput{
		ExpectedStatus: model.CoordinationIncidentCoordinating,
		Status:         model.CoordinationIncidentResolved,
		ClaimToken:     second.ClaimToken,
		Current:        claimTime.Add(time.Minute + time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ClaimToken != "" || resolved.ClaimExpiresAt != nil ||
		resolved.Status != model.CoordinationIncidentResolved {
		t.Fatalf("resolved claim was not cleared: %+v", resolved)
	}
}

func TestCoordinationIncidentExpiredClaimRebasesToCurrentGraph(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "focus"})
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		TaskID: &task.Task.ID, Trigger: model.CoordinationTriggerGraphStalled,
		Summary: "Graph has no runnable task", ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimTime := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	first, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0), TTL: time.Minute, Current: claimTime,
	})
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v value=%+v err=%v", claimed, first, err)
	}
	parent, err := opened.CreateTask(ctx, CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, task.Task.ID); err != nil {
		t.Fatal(err)
	}

	live, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(1), TTL: time.Minute,
		Current: claimTime.Add(time.Minute - time.Nanosecond),
	})
	if err != nil || claimed || live.ClaimToken != first.ClaimToken || live.GraphRevision != 0 {
		t.Fatalf("live stale claim was rewritten: claimed=%v value=%+v err=%v", claimed, live, err)
	}
	reclaimed, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(1), TTL: time.Minute,
		Current: claimTime.Add(time.Minute),
	})
	if err != nil || !claimed || reclaimed.ClaimToken == first.ClaimToken ||
		reclaimed.GraphRevision != 1 {
		t.Fatalf("expired stale claim was not rebased: claimed=%v value=%+v err=%v", claimed, reclaimed, err)
	}
}

func TestCoordinationIncidentClaimGuardsAndAtomicity(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	seed, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := seed.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerRetryExhausted, Summary: "Retry budget exhausted",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := seed.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerAgentExhausted, Summary: "No agent",
		Status: model.CoordinationIncidentCoordinating,
	}); err == nil {
		t.Fatal("direct coordinating incident creation succeeded")
	}
	if _, err := seed.TransitionCoordinationIncident(ctx, incident.ID, TransitionCoordinationIncidentInput{
		ExpectedStatus: model.CoordinationIncidentOpen,
		Status:         model.CoordinationIncidentCoordinating,
	}); err == nil {
		t.Fatal("direct coordinating transition succeeded")
	}
	for _, ttl := range []time.Duration{
		MinCoordinationIncidentClaimTTL - time.Nanosecond,
		MaxCoordinationIncidentClaimTTL + time.Nanosecond,
	} {
		if _, _, err := seed.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: revisionPointer(0), TTL: ttl, Current: time.Now(),
		}); err == nil {
			t.Fatalf("out-of-range claim TTL %s succeeded", ttl)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	firstStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()

	start := make(chan struct{})
	type claimResult struct {
		incident model.CoordinationIncident
		claimed  bool
		err      error
	}
	results := make(chan claimResult, 2)
	claim := func(opened *Store) {
		<-start
		value, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: revisionPointer(0),
			TTL:                   MinCoordinationIncidentClaimTTL,
			Current:               time.Date(2030, time.February, 3, 4, 5, 6, 0, time.UTC),
		})
		results <- claimResult{incident: value, claimed: claimed, err: err}
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		claim(firstStore)
	}()
	go func() {
		defer workers.Done()
		claim(secondStore)
	}()
	close(start)
	workers.Wait()
	close(results)

	claimedCount := 0
	var winningToken string
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent claim: %v", result.err)
		}
		if result.claimed {
			claimedCount++
			winningToken = result.incident.ClaimToken
		}
	}
	if claimedCount != 1 || winningToken == "" {
		t.Fatalf("concurrent claims won %d times, token=%q", claimedCount, winningToken)
	}
	stored, err := firstStore.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ClaimToken != winningToken {
		t.Fatalf("stored claim token = %q, want %q", stored.ClaimToken, winningToken)
	}
}

func TestCoordinationIncidentClaimRequiresCurrentGraphAndLiveDetectionDoesNotRewriteLease(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Summary: "No runnable task",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := opened.CreateTask(ctx, CreateTaskInput{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := opened.CreateTask(ctx, CreateTaskInput{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, first.Task.ID, second.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
	}); !errors.Is(err, ErrGraphRevisionConflict) {
		t.Fatalf("stale graph claim error = %v", err)
	}
	refreshed, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Summary: "Still no runnable task",
		ExpectedGraphRevision: revisionPointer(1),
	})
	if err != nil || created || refreshed.GraphRevision != 1 {
		t.Fatalf("refresh after graph change: created=%v value=%+v err=%v", created, refreshed, err)
	}
	claimTime := time.Now().UTC()
	claimedIncident, claimed, err := opened.ClaimCoordinationIncident(ctx, refreshed.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(1),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !claimed {
		t.Fatalf("claim refreshed incident: claimed=%v value=%+v err=%v", claimed, claimedIncident, err)
	}
	third, err := opened.CreateTask(ctx, CreateTaskInput{Title: "third"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, second.Task.ID, third.Task.ID); err != nil {
		t.Fatal(err)
	}
	detected, created, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Severity: model.CoordinationSeverityCritical,
		Summary: "Detector observed a newer graph", ExpectedGraphRevision: revisionPointer(2),
	})
	if err != nil || created {
		t.Fatalf("live detection: created=%v value=%+v err=%v", created, detected, err)
	}
	if detected.GraphRevision != claimedIncident.GraphRevision ||
		detected.Summary != claimedIncident.Summary ||
		detected.ClaimToken != claimedIncident.ClaimToken {
		t.Fatalf("live detection rewrote claimed incident: before=%+v after=%+v", claimedIncident, detected)
	}
}

func TestCoordinationIncidentPreservesTaskIdentityAfterTaskDeletion(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "recoverable task"})
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		RootTaskID: &task.Task.ID, TaskID: &task.Task.ID,
		Trigger: model.CoordinationTriggerRepeatedBlock, Summary: "Task repeatedly blocked",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.DeleteTask(ctx, task.Task.ID); err != nil {
		t.Fatalf("delete task with retained incident history: %v", err)
	}
	stored, err := opened.GetCoordinationIncident(ctx, incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TaskID == nil || *stored.TaskID != task.Task.ID ||
		stored.RootTaskID == nil || *stored.RootTaskID != task.Task.ID {
		t.Fatalf("task deletion erased incident identity: %+v", stored)
	}
}

func TestCoordinationProposalRejectsStaleGraphRevision(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, _, err := opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerGraphStalled, Summary: "Graph has no runnable task",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimTime := time.Date(2031, time.January, 2, 3, 4, 5, 0, time.UTC)
	incident, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: revisionPointer(0),
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v incident=%+v err=%v", claimed, incident, err)
	}
	proposal, _, err := opened.CreateCoordinationProposal(ctx, CreateCoordinationProposalInput{
		IncidentID: incident.ID, CoordinatorAgent: "claude", ExpectedGraphRevision: revisionPointer(0),
		ClaimToken: incident.ClaimToken, Current: claimTime.Add(time.Second),
		Summary: "Create a recovery task", Rationale: "The graph needs a new runnable entrypoint.",
		Actions: []byte(`[{"type":"create_task"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "parent"})
	child, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "child"})
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	reason := "Stale update must fail"
	_, err = opened.UpdateCoordinationProposal(ctx, proposal.ID, UpdateCoordinationProposalInput{
		ExpectedStatus: model.CoordinationProposalDraft, ExpectedGraphRevision: revisionPointer(0), Rationale: &reason,
		ClaimToken: incident.ClaimToken, Current: claimTime.Add(time.Second),
	})
	if !errors.Is(err, ErrGraphRevisionConflict) {
		t.Fatalf("stale proposal update error = %v", err)
	}
	_, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, TransitionCoordinationProposalInput{
		ExpectedStatus:        model.CoordinationProposalDraft,
		Status:                model.CoordinationProposalValidating,
		ExpectedGraphRevision: revisionPointer(0),
		ClaimToken:            incident.ClaimToken,
		Current:               claimTime.Add(time.Second),
	})
	if !errors.Is(err, ErrGraphRevisionConflict) {
		t.Fatalf("stale proposal transition error = %v", err)
	}
	supersededPair, err := opened.SupersedeCoordinationProposal(ctx, proposal.ID, SupersedeCoordinationProposalInput{
		ExpectedUpdatedAt: proposal.UpdatedAt,
		ClaimToken:        incident.ClaimToken,
		Current:           claimTime.Add(time.Second),
	})
	if err != nil || supersededPair.Proposal.Status != model.CoordinationProposalSuperseded ||
		supersededPair.Incident.Status != model.CoordinationIncidentOpen {
		t.Fatalf("supersede stale proposal: %+v %v", supersededPair, err)
	}
	_, _, err = opened.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerAgentExhausted, Summary: "No agent is available",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if !errors.Is(err, ErrGraphRevisionConflict) {
		t.Fatalf("stale incident detection error = %v", err)
	}
}

func TestSchema18MigrationAddsCoordinationPersistenceAndGraphState(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/autogora.db"
	opened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.CreateTask(ctx, CreateTaskInput{Title: "existing task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.db.ExecContext(ctx, `
		DROP TABLE coordination_proposals;
		DROP TABLE coordination_incidents;
		DROP TABLE board_graph_state;
		PRAGMA user_version = 18;
	`); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	state, err := migrated.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || state.UpdatedAt == "" {
		t.Fatalf("migrated graph state = %+v", state)
	}
	var version int
	if err := migrated.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 19 {
		t.Fatalf("schema version = %d, want 19", version)
	}
	var agentSlotDefinition string
	if err := migrated.db.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'global_agent_slots'",
	).Scan(&agentSlotDefinition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agentSlotDefinition, "'coordinator'") {
		t.Fatalf("schema 19 global agent slots do not accept coordinator owners: %s", agentSlotDefinition)
	}
	incident, created, err := migrated.CreateCoordinationIncident(ctx, CreateCoordinationIncidentInput{
		Trigger: model.CoordinationTriggerRetryExhausted, Summary: "Retry budget exhausted",
		ExpectedGraphRevision: revisionPointer(0),
	})
	if err != nil || !created || incident.GraphRevision != 0 {
		t.Fatalf("migrated coordination storage unusable: created=%v value=%+v err=%v", created, incident, err)
	}
}
