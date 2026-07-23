package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestCoordinationCLIInspectsIncidentsAndProposals(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	state, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
		ID: "ci-cli", Trigger: model.CoordinationTriggerRepeatedBlock,
		ExpectedGraphRevision: &state.Revision, Summary: "Task blocked repeatedly",
		Details: json.RawMessage(`{"recurrences":2}`),
	})
	if err != nil {
		t.Fatal(err)
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
	proposal, _, err := opened.CreateCoordinationProposal(ctx, store.CreateCoordinationProposalInput{
		ID: "cp-cli", IncidentID: incident.ID, CoordinatorAgent: "claude",
		ExpectedGraphRevision: &state.Revision, ClaimToken: incident.ClaimToken,
		Current: claimTime.Add(time.Second), Summary: "Change route",
		Rationale: "The current route cannot proceed.", Actions: json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	status := runApp(t, app, "coordination", "status", "--db", dbPath)
	if !strings.Contains(status, `"graphState"`) || !strings.Contains(status, incident.ID) ||
		strings.Contains(status, "claimToken") {
		t.Fatalf("unexpected coordination status: %s", status)
	}
	listed := runApp(t, app, "coordination", "list", "--db", dbPath, "--trigger", "repeated_block")
	if !strings.Contains(listed, incident.ID) {
		t.Fatalf("incident missing from list: %s", listed)
	}
	shown := runApp(t, app, "coordination", "show", incident.ID, "--db", dbPath)
	if !strings.Contains(shown, proposal.ID) {
		t.Fatalf("proposal missing from incident: %s", shown)
	}
	proposalOutput := runApp(t, app, "coordination", "proposal", proposal.ID, "--db", dbPath)
	if !strings.Contains(proposalOutput, `"coordinatorAgent": "claude"`) {
		t.Fatalf("unexpected proposal output: %s", proposalOutput)
	}
	if err := app.Run(context.Background(), []string{
		"coordination", "list", "--db", dbPath, "--status", "bogus",
	}); err == nil || !strings.Contains(err.Error(), "invalid coordination incident status") {
		t.Fatalf("invalid status was accepted: %v", err)
	}
}

func TestCoordinationHelpIsDiscoverable(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	for _, args := range [][]string{{"coordination", "--help"}, {"help", "coordination"}} {
		output := runApp(t, app, args...)
		if !strings.HasPrefix(output, "autogora coordination") || !strings.Contains(output, "proposal <id>") {
			t.Fatalf("coordination help output = %q", output)
		}
	}
}
