package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type coordinationCLIFixture struct {
	Incident model.CoordinationIncident
	Proposal model.CoordinationProposal
	Revision int64
}

func createCoordinationCLIFixture(
	t *testing.T,
	dbPath, board, suffix string,
) coordinationCLIFixture {
	t.Helper()
	ctx := context.Background()
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	state, err := opened.GetBoardGraphState(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	incident, _, err := opened.CreateCoordinationIncident(ctx, store.CreateCoordinationIncidentInput{
		ID: "ci-cli-" + suffix, Board: board,
		Trigger:               model.CoordinationTriggerGraphStalled,
		ExpectedGraphRevision: &state.Revision, Summary: "Coordinator needs a human decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimTime := time.Now().UTC()
	incident, claimed, err := opened.ClaimCoordinationIncident(
		ctx,
		incident.ID,
		store.ClaimCoordinationIncidentInput{
			ExpectedGraphRevision: &state.Revision,
			TTL:                   time.Minute,
			Current:               claimTime,
		},
	)
	if err != nil || !claimed {
		t.Fatalf("claim incident: claimed=%v value=%+v err=%v", claimed, incident, err)
	}
	proposal, _, err := opened.CreateCoordinationProposal(ctx, store.CreateCoordinationProposalInput{
		ID: "cp-cli-" + suffix, IncidentID: incident.ID, CoordinatorAgent: "codex-coordinator",
		Status: model.CoordinationProposalValidating, ExpectedGraphRevision: &state.Revision,
		ClaimToken: incident.ClaimToken, Current: claimTime.Add(time.Second),
		Summary: "Apply a bounded recovery", Rationale: "The deterministic path is exhausted.",
		Actions: json.RawMessage(`[{
			"kind":"create_task",
			"reason":"create bounded recovery work",
			"task":{
				"key":"cli-recovery","title":"CLI recovery","body":"Run the approved recovery.",
				"assignee":"codex-worker","runtime":"codex","workflowRole":"worker",
				"priority":1,"prerequisites":[],"dependents":[]
			},
			"expectedTaskVersions":{}
		}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err = opened.TransitionCoordinationProposal(
		ctx,
		proposal.ID,
		store.TransitionCoordinationProposalInput{
			ExpectedStatus:        model.CoordinationProposalValidating,
			Status:                model.CoordinationProposalValidated,
			ExpectedGraphRevision: &state.Revision,
			ClaimToken:            incident.ClaimToken,
			Current:               claimTime.Add(2 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := opened.RequestCoordinationApproval(
		ctx,
		proposal.ID,
		store.RequestCoordinationApprovalInput{
			ExpectedGraphRevision: &state.Revision,
			ClaimToken:            incident.ClaimToken,
			Current:               claimTime.Add(3 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return coordinationCLIFixture{
		Incident: approval.Incident, Proposal: approval.Proposal, Revision: state.Revision,
	}
}

func coordinationCLIApp(t *testing.T) (string, *App) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)
	return dbPath, app
}

func decodeCoordinationMutationOutput(
	t *testing.T,
	output string,
) coordinationMutationOutput {
	t.Helper()
	var value coordinationMutationOutput
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode coordination mutation output %q: %v", output, err)
	}
	return value
}

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
		if !strings.HasPrefix(output, "autogora coordination") ||
			!strings.Contains(output, "approve <proposal-id>") ||
			!strings.Contains(output, "claim tokens are internal") {
			t.Fatalf("coordination help output = %q", output)
		}
	}
}

func TestCoordinationCLIApproveAppliesProposal(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
	fixture := createCoordinationCLIFixture(t, dbPath, "default", "approve")
	output := runApp(
		t,
		app,
		"coordination", "approve", fixture.Proposal.ID,
		"--db", dbPath,
		"--updated-at", fixture.Proposal.UpdatedAt,
		"--graph-revision", strconv.FormatInt(fixture.Revision, 10),
	)
	result := decodeCoordinationMutationOutput(t, output)
	if result.Action != "approve" || result.Outcome != "applied" ||
		result.Proposal == nil ||
		result.Proposal.Status != model.CoordinationProposalApplied ||
		result.Incident.Status != model.CoordinationIncidentResolved {
		t.Fatalf("approve output = %+v", result)
	}
	if strings.Contains(output, "claimToken") {
		t.Fatalf("approve output leaked a claim token: %s", output)
	}

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	proposal, err := opened.GetCoordinationProposal(ctx, fixture.Proposal.ID)
	if err != nil || proposal.Status != model.CoordinationProposalApplied {
		t.Fatalf("stored approved proposal = %+v, %v", proposal, err)
	}
	incident, err := opened.GetCoordinationIncident(ctx, fixture.Incident.ID)
	if err != nil || incident.Status != model.CoordinationIncidentResolved {
		t.Fatalf("stored approved incident = %+v, %v", incident, err)
	}
}

func TestCoordinationCLIApproveSupersedesWhenGraphChangesBeforeApply(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
	fixture := createCoordinationCLIFixture(t, dbPath, "default", "stale-apply")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT OR IGNORE INTO board_graph_state(board, revision, updated_at)
		VALUES ('default', 0, '2030-01-01T00:00:00.000000000Z')
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TRIGGER bump_graph_after_cli_approval
		AFTER UPDATE OF status ON coordination_proposals
		WHEN NEW.id = 'cp-cli-stale-apply'
			AND OLD.status = 'awaiting_approval' AND NEW.status = 'approved'
		BEGIN
			UPDATE board_graph_state
			SET revision = revision + 1,
				updated_at = '2030-01-01T00:00:01.000000000Z'
			WHERE board = 'default';
		END
	`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	output := runApp(
		t,
		app,
		"coordination", "approve", fixture.Proposal.ID,
		"--db", dbPath,
		"--updated-at", fixture.Proposal.UpdatedAt,
		"--graph-revision", strconv.FormatInt(fixture.Revision, 10),
	)
	result := decodeCoordinationMutationOutput(t, output)
	if result.Outcome != "superseded" || result.Proposal == nil ||
		result.Proposal.Status != model.CoordinationProposalSuperseded ||
		result.Incident.Status != model.CoordinationIncidentOpen ||
		result.Incident.GraphRevision != fixture.Revision+1 {
		t.Fatalf("stale apply recovery = %+v", result)
	}
}

func TestCoordinationCLIRejectUsesSelectedBoard(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "team", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	fixture := createCoordinationCLIFixture(t, dbPath, "team", "reject")
	output := runApp(
		t,
		app,
		"coordination", "reject", fixture.Proposal.ID,
		"--db", dbPath, "--board", "team",
		"--updated-at", fixture.Proposal.UpdatedAt,
		"--graph-revision", strconv.FormatInt(fixture.Revision, 10),
	)
	result := decodeCoordinationMutationOutput(t, output)
	if result.Action != "reject" || result.Outcome != "rejected" ||
		result.Proposal == nil ||
		result.Proposal.Status != model.CoordinationProposalRejected ||
		result.Incident.Status != model.CoordinationIncidentDismissed ||
		result.Incident.Board != "team" {
		t.Fatalf("reject output = %+v", result)
	}
}

func TestCoordinationCLIDismissesOnlyOpenIncident(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
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
		ID: "ci-cli-dismiss", Trigger: model.CoordinationTriggerGraphStalled,
		ExpectedGraphRevision: &state.Revision, Summary: "Dismiss this incident",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	output := runApp(
		t,
		app,
		"coordination", "dismiss", incident.ID,
		"--db", dbPath,
		"--graph-revision", strconv.FormatInt(state.Revision, 10),
	)
	result := decodeCoordinationMutationOutput(t, output)
	if result.Outcome != "dismissed" ||
		result.Incident.Status != model.CoordinationIncidentDismissed {
		t.Fatalf("dismiss output = %+v", result)
	}
	err = app.Run(ctx, []string{
		"coordination", "dismiss", incident.ID,
		"--db", dbPath,
		"--graph-revision", strconv.FormatInt(state.Revision, 10),
	})
	if err == nil || !strings.Contains(err.Error(), "coordination dismiss conflict") {
		t.Fatalf("terminal incident was dismissed again: %v", err)
	}
}

func TestCoordinationCLIRetrySupersedesAtCurrentGraph(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
	fixture := createCoordinationCLIFixture(t, dbPath, "default", "retry")
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "new prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "dependent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.LinkTasks(ctx, parent.Task.ID, child.Task.ID); err != nil {
		t.Fatal(err)
	}
	currentState, err := opened.GetBoardGraphState(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	output := runApp(
		t,
		app,
		"coordination", "retry", fixture.Proposal.ID,
		"--db", dbPath,
		"--updated-at", fixture.Proposal.UpdatedAt,
	)
	result := decodeCoordinationMutationOutput(t, output)
	if result.Outcome != "reopened" || result.Proposal == nil ||
		result.Proposal.Status != model.CoordinationProposalSuperseded ||
		result.Incident.Status != model.CoordinationIncidentOpen ||
		result.Incident.GraphRevision != currentState.Revision {
		t.Fatalf("retry output = %+v", result)
	}
}

func TestCoordinationCLIMutationsValidateVersionsAndRejectClaimTokens(t *testing.T) {
	ctx := context.Background()
	dbPath, app := coordinationCLIApp(t)
	fixture := createCoordinationCLIFixture(t, dbPath, "default", "validation")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing updated at",
			args: []string{
				"coordination", "approve", fixture.Proposal.ID, "--db", dbPath,
				"--graph-revision", strconv.FormatInt(fixture.Revision, 10),
			},
			want: "requires --updated-at",
		},
		{
			name: "missing graph revision",
			args: []string{
				"coordination", "approve", fixture.Proposal.ID, "--db", dbPath,
				"--updated-at", fixture.Proposal.UpdatedAt,
			},
			want: "requires --graph-revision",
		},
		{
			name: "stale proposal version",
			args: []string{
				"coordination", "approve", fixture.Proposal.ID, "--db", dbPath,
				"--updated-at", "2000-01-01T00:00:00Z",
				"--graph-revision", strconv.FormatInt(fixture.Revision, 10),
			},
			want: "coordination approve conflict",
		},
		{
			name: "invalid graph revision",
			args: []string{
				"coordination", "reject", fixture.Proposal.ID, "--db", dbPath,
				"--updated-at", fixture.Proposal.UpdatedAt,
				"--graph-revision", "-1",
			},
			want: "non-negative integer",
		},
		{
			name: "claim token",
			args: []string{
				"coordination", "retry", fixture.Proposal.ID, "--db", dbPath,
				"--updated-at", fixture.Proposal.UpdatedAt,
				"--claim-token", "must-not-be-accepted",
			},
			want: "do not accept claim tokens",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := app.Run(ctx, test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	proposal, err := opened.GetCoordinationProposal(ctx, fixture.Proposal.ID)
	if err != nil || proposal.Status != model.CoordinationProposalAwaitingApproval {
		t.Fatalf("invalid mutations changed proposal: %+v, %v", proposal, err)
	}
}
