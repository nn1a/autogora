package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type dashboardCoordinationFixture struct {
	Incident model.CoordinationIncident
	Proposal model.CoordinationProposal
	Revision int64
}

func seedDashboardAwaitingProposal(
	t *testing.T,
	server *Server,
	board string,
	actions json.RawMessage,
) dashboardCoordinationFixture {
	t.Helper()
	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	state, err := opened.GetBoardGraphState(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	incident, created, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
		Board: board, Trigger: model.CoordinationTriggerGraphStalled,
		Severity:              model.CoordinationSeverityWarning,
		ExpectedGraphRevision: &state.Revision,
		Summary:               "Coordinator recovery needs approval",
	})
	if err != nil || !created {
		t.Fatalf("create incident: created=%v incident=%+v err=%v", created, incident, err)
	}
	claimTime := time.Now().UTC()
	incident, claimed, err := opened.ClaimCoordinationIncident(ctx, incident.ID, store.ClaimCoordinationIncidentInput{
		ExpectedGraphRevision: &state.Revision,
		TTL:                   time.Minute,
		Current:               claimTime,
	})
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v incident=%+v err=%v", claimed, incident, err)
	}
	proposal, created, err := opened.CreateCoordinationProposal(ctx, store.CreateCoordinationProposalInput{
		IncidentID: incident.ID, CoordinatorAgent: "dashboard-test",
		CoordinatorModel: "test-model", CoordinatorProvider: "test-provider",
		Status: model.CoordinationProposalValidating, ExpectedGraphRevision: &state.Revision,
		ClaimToken: incident.ClaimToken, Current: claimTime.Add(time.Second),
		Summary: "Apply recovery", Rationale: "The proposal satisfies the recovery policy.",
		Actions: actions,
	})
	if err != nil || !created {
		t.Fatalf("create proposal: created=%v proposal=%+v err=%v", created, proposal, err)
	}
	proposal, err = opened.TransitionCoordinationProposal(ctx, proposal.ID, store.TransitionCoordinationProposalInput{
		ExpectedStatus:        model.CoordinationProposalValidating,
		Status:                model.CoordinationProposalValidated,
		ExpectedGraphRevision: &state.Revision,
		ClaimToken:            incident.ClaimToken,
		Current:               claimTime.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	awaiting, err := opened.RequestCoordinationApproval(ctx, proposal.ID, store.RequestCoordinationApprovalInput{
		ExpectedGraphRevision: &state.Revision,
		ClaimToken:            incident.ClaimToken,
		Current:               claimTime.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return dashboardCoordinationFixture{
		Incident: awaiting.Incident, Proposal: awaiting.Proposal, Revision: state.Revision,
	}
}

func coordinationMutationBody(fixture dashboardCoordinationFixture) map[string]any {
	return map[string]any{
		"expectedUpdatedAt":     fixture.Proposal.UpdatedAt,
		"expectedGraphRevision": fixture.Revision,
	}
}

func TestCoordinationProposalApproveAppliesAtomically(t *testing.T) {
	server := startTestServer(t)
	fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
	path := "/api/coordination/proposals/" + fixture.Proposal.ID + "/approve?board=default"
	body := coordinationMutationBody(fixture)

	response, value := apiRequest(t, server, http.MethodPost, path, body)
	result := mapValue(t, value)
	if response.StatusCode != http.StatusOK ||
		mapValue(t, result["proposal"])["status"] != string(model.CoordinationProposalApplied) ||
		mapValue(t, result["incident"])["status"] != string(model.CoordinationIncidentResolved) {
		t.Fatalf("approve response: %d %#v", response.StatusCode, result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "claimToken") {
		t.Fatalf("approval response leaked claim token: %s", encoded)
	}

	duplicate, duplicateValue := apiRequest(t, server, http.MethodPost, path, body)
	if duplicate.StatusCode != http.StatusConflict ||
		!strings.Contains(strings.ToLower(mapValue(t, duplicateValue)["error"].(string)), "conflict") {
		t.Fatalf("duplicate approval: %d %#v", duplicate.StatusCode, duplicateValue)
	}
}

func TestCoordinationProposalApproveRejectsStaleVersionAndGraph(t *testing.T) {
	t.Run("proposal version", func(t *testing.T) {
		server := startTestServer(t)
		fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
		body := coordinationMutationBody(fixture)
		body["expectedUpdatedAt"] = "2000-01-01T00:00:00Z"
		response, value := apiRequest(
			t, server, http.MethodPost,
			"/api/coordination/proposals/"+fixture.Proposal.ID+"/approve?board=default", body,
		)
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("stale approval: %d %#v", response.StatusCode, value)
		}
		opened, err := server.manager.OpenStore(context.Background(), "default")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		proposal, err := opened.GetCoordinationProposal(context.Background(), fixture.Proposal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if proposal.Status != model.CoordinationProposalAwaitingApproval {
			t.Fatalf("stale approval changed proposal: %+v", proposal)
		}
	})

	t.Run("graph revision", func(t *testing.T) {
		server := startTestServer(t)
		ctx := context.Background()
		fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
		opened, err := server.manager.OpenStore(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "graph parent"})
		if err != nil {
			t.Fatal(err)
		}
		child, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "graph child"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		response, value := apiRequest(
			t, server, http.MethodPost,
			"/api/coordination/proposals/"+fixture.Proposal.ID+"/approve?board=default",
			coordinationMutationBody(fixture),
		)
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("stale graph approval: %d %#v", response.StatusCode, value)
		}
		opened, err = server.manager.OpenStore(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		proposal, err := opened.GetCoordinationProposal(ctx, fixture.Proposal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if proposal.Status != model.CoordinationProposalAwaitingApproval {
			t.Fatalf("stale graph approval changed proposal: %+v", proposal)
		}
	})
}

func TestCoordinationProposalApproveConflictSupersedesAndReopens(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "stale apply target", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	priority := 7
	actions, err := json.Marshal([]map[string]any{{
		"kind": "update_priority", "taskId": task.Task.ID,
		"expectedUpdatedAt": task.Task.UpdatedAt, "priority": priority,
		"reason": "Raise the recovery task priority",
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := seedDashboardAwaitingProposal(t, server, "default", actions)
	opened, err = server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "changed after coordinator snapshot"
	if _, err := opened.UpdateTask(ctx, task.Task.ID, store.UpdateTaskInput{
		ExpectedUpdatedAt: &task.Task.UpdatedAt, Title: &newTitle,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	response, value := apiRequest(
		t, server, http.MethodPost,
		"/api/coordination/proposals/"+fixture.Proposal.ID+"/approve?board=default",
		coordinationMutationBody(fixture),
	)
	result := mapValue(t, value)
	if response.StatusCode != http.StatusConflict ||
		result["code"] != "coordination_apply_conflict" ||
		result["retryable"] != true ||
		mapValue(t, result["proposal"])["status"] != string(model.CoordinationProposalSuperseded) ||
		mapValue(t, result["incident"])["status"] != string(model.CoordinationIncidentOpen) {
		t.Fatalf("apply conflict response: %d %#v", response.StatusCode, result)
	}
	if _, leaked := mapValue(t, result["incident"])["claimToken"]; leaked {
		t.Fatalf("apply conflict leaked claim token: %#v", result)
	}

	opened, err = server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	proposal, err := opened.GetCoordinationProposal(ctx, fixture.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := opened.GetCoordinationIncident(ctx, fixture.Incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != model.CoordinationProposalSuperseded ||
		incident.Status != model.CoordinationIncidentOpen {
		t.Fatalf("apply conflict left hidden state: proposal=%+v incident=%+v", proposal, incident)
	}
}

func TestCoordinationProposalRejectAndDismissAreAtomic(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		server := startTestServer(t)
		fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
		path := "/api/coordination/proposals/" + fixture.Proposal.ID + "/reject?board=default"
		body := coordinationMutationBody(fixture)
		response, value := apiRequest(t, server, http.MethodPost, path, body)
		result := mapValue(t, value)
		if response.StatusCode != http.StatusOK ||
			mapValue(t, result["proposal"])["status"] != string(model.CoordinationProposalRejected) ||
			mapValue(t, result["incident"])["status"] != string(model.CoordinationIncidentDismissed) {
			t.Fatalf("reject response: %d %#v", response.StatusCode, result)
		}
		duplicate, _ := apiRequest(t, server, http.MethodPost, path, body)
		if duplicate.StatusCode != http.StatusConflict {
			t.Fatalf("duplicate reject status = %d", duplicate.StatusCode)
		}
	})

	t.Run("dismiss open incident", func(t *testing.T) {
		server := startTestServer(t)
		ctx := context.Background()
		opened, err := server.manager.OpenStore(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}
		revision := int64(0)
		incident, created, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
			Trigger:               model.CoordinationTriggerAgentExhausted,
			Severity:              model.CoordinationSeverityWarning,
			ExpectedGraphRevision: &revision,
			Summary:               "No agent is currently available",
		})
		if err != nil || !created {
			t.Fatalf("create open incident: created=%v incident=%+v err=%v", created, incident, err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		path := "/api/coordination/incidents/" + incident.ID + "/dismiss?board=default"
		body := map[string]any{"expectedGraphRevision": 0}
		response, value := apiRequest(t, server, http.MethodPost, path, body)
		result := mapValue(t, value)
		if response.StatusCode != http.StatusOK ||
			mapValue(t, result["incident"])["status"] != string(model.CoordinationIncidentDismissed) {
			t.Fatalf("dismiss response: %d %#v", response.StatusCode, result)
		}
		duplicate, _ := apiRequest(t, server, http.MethodPost, path, body)
		if duplicate.StatusCode != http.StatusConflict {
			t.Fatalf("duplicate dismiss status = %d", duplicate.StatusCode)
		}
	})
}

func TestCoordinationProposalRetrySupersedesStaleSnapshot(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
	opened, err := server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new graph parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new graph child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	path := "/api/coordination/proposals/" + fixture.Proposal.ID + "/retry?board=default"
	body := coordinationMutationBody(fixture)
	response, value := apiRequest(t, server, http.MethodPost, path, body)
	result := mapValue(t, value)
	if response.StatusCode != http.StatusOK || result["retryScheduled"] != true ||
		mapValue(t, result["proposal"])["status"] != string(model.CoordinationProposalSuperseded) ||
		mapValue(t, result["incident"])["status"] != string(model.CoordinationIncidentOpen) ||
		mapValue(t, result["incident"])["graphRevision"] != float64(1) ||
		mapValue(t, result["graphState"])["revision"] != float64(1) {
		t.Fatalf("retry response: %d %#v", response.StatusCode, result)
	}
	duplicate, _ := apiRequest(t, server, http.MethodPost, path, body)
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate retry status = %d", duplicate.StatusCode)
	}
}

func TestCoordinationMutationEnforcesBoardScopeAndVersionFields(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	if _, err := server.manager.Create(ctx, "other", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	fixture := seedDashboardAwaitingProposal(t, server, "default", json.RawMessage(`[]`))
	path := "/api/coordination/proposals/" + fixture.Proposal.ID + "/reject?board=other"
	response, value := apiRequest(t, server, http.MethodPost, path, coordinationMutationBody(fixture))
	if response.StatusCode != http.StatusNotFound ||
		!strings.Contains(strings.ToLower(mapValue(t, value)["error"].(string)), "not found") {
		t.Fatalf("wrong board response: %d %#v", response.StatusCode, value)
	}

	missing, missingValue := apiRequest(
		t, server, http.MethodPost,
		"/api/coordination/proposals/"+fixture.Proposal.ID+"/approve?board=default",
		map[string]any{"expectedUpdatedAt": fixture.Proposal.UpdatedAt},
	)
	if missing.StatusCode != http.StatusBadRequest ||
		!strings.Contains(mapValue(t, missingValue)["error"].(string), "expectedGraphRevision") {
		t.Fatalf("missing revision response: %d %#v", missing.StatusCode, missingValue)
	}
	injected, injectedValue := apiRequest(
		t, server, http.MethodPost,
		"/api/coordination/proposals/"+fixture.Proposal.ID+"/approve?board=default",
		map[string]any{
			"expectedUpdatedAt":     fixture.Proposal.UpdatedAt,
			"expectedGraphRevision": fixture.Revision,
			"claimToken":            "must-never-be-accepted",
		},
	)
	if injected.StatusCode != http.StatusBadRequest {
		t.Fatalf("claim token input response: %d %#v", injected.StatusCode, injectedValue)
	}
}
