package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestCoordinationAPIListsAndScopesIncidentProposals(t *testing.T) {
	server := startTestServer(t)
	ctx := context.Background()
	opened, err := server.manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	state, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	incident, created, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
		ID: "ci-dashboard", Trigger: model.CoordinationTriggerGraphStalled,
		Severity: model.CoordinationSeverityWarning, ExpectedGraphRevision: &state.Revision,
		Summary: "No runnable work", Details: json.RawMessage(`{"idleSeconds":300}`),
	})
	if err != nil || !created {
		t.Fatalf("create incident = %+v, created=%v, err=%v", incident, created, err)
	}
	proposal, created, err := opened.CreateCoordinationProposal(ctx, store.CreateCoordinationProposalInput{
		ID: "cp-dashboard", IncidentID: incident.ID, CoordinatorAgent: "codex",
		CoordinatorModel: "gpt-test", Status: model.CoordinationProposalDraft,
		ExpectedGraphRevision: &state.Revision, Summary: "Inspect route",
		Rationale: "The graph has no runnable task.", Actions: json.RawMessage(`[]`),
	})
	if err != nil || !created {
		t.Fatalf("create proposal = %+v, created=%v, err=%v", proposal, created, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	response, value := apiRequest(t, server, http.MethodGet, "/api/coordination?board=default", nil)
	summary := mapValue(t, value)
	if response.StatusCode != http.StatusOK || summary["activeCount"] != float64(1) ||
		mapValue(t, summary["policy"])["mode"] != "observe" ||
		mapValue(t, summary["graphState"])["revision"] != float64(state.Revision) {
		t.Fatalf("unexpected coordination summary: %d %#v", response.StatusCode, summary)
	}
	incidents := arrayValue(t, summary["incidents"])
	if len(incidents) != 1 || mapValue(t, incidents[0])["id"] != incident.ID {
		t.Fatalf("incident missing from summary: %#v", incidents)
	}
	if _, leaked := mapValue(t, incidents[0])["claimToken"]; leaked {
		t.Fatalf("incident claim token leaked: %#v", incidents[0])
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/coordination/incidents/"+incident.ID+"?board=default", nil)
	detail := mapValue(t, value)
	if response.StatusCode != http.StatusOK || mapValue(t, detail["incident"])["id"] != incident.ID {
		t.Fatalf("unexpected incident detail: %d %#v", response.StatusCode, detail)
	}
	proposals := arrayValue(t, detail["proposals"])
	if len(proposals) != 1 || mapValue(t, proposals[0])["id"] != proposal.ID {
		t.Fatalf("proposal missing from incident detail: %#v", proposals)
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/coordination/proposals/"+proposal.ID+"?board=default", nil)
	proposalDetail := mapValue(t, value)
	if response.StatusCode != http.StatusOK || mapValue(t, proposalDetail["proposal"])["coordinatorAgent"] != "codex" ||
		mapValue(t, proposalDetail["incident"])["id"] != incident.ID {
		t.Fatalf("unexpected proposal detail: %d %#v", response.StatusCode, proposalDetail)
	}

	response, value = apiRequest(t, server, http.MethodGet, "/api/coordination/incidents?board=default&status=bogus", nil)
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(mapValue(t, value)["error"].(string), "invalid coordination incident status") {
		t.Fatalf("invalid filter response: %d %#v", response.StatusCode, value)
	}
	response, _ = apiRequest(t, server, http.MethodPost, "/api/coordination?board=default", map[string]any{})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("coordination write status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
}
