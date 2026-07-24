package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func downgradeCoordinationIncidentSchemaToVersion21(
	t *testing.T,
	path string,
) {
	t.Helper()
	ctx := context.Background()
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	version21Definition := strings.Replace(
		coordinationIncidentTableDefinition,
		", 'run_invariant'",
		"",
		1,
	)
	if version21Definition == coordinationIncidentTableDefinition {
		tx.Rollback()
		conn.Close()
		raw.Close()
		t.Fatal("test did not remove run_invariant from the v21 table definition")
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE coordination_incidents_v21 `+version21Definition+`;
		INSERT INTO coordination_incidents_v21(
			id, board, root_task_id, task_id, trigger, severity, status,
			graph_revision, summary, details_json, claim_token, claim_expires_at,
			created_at, updated_at
		)
		SELECT
			id, board, root_task_id, task_id, trigger, severity, status,
			graph_revision, summary, details_json, claim_token, claim_expires_at,
			created_at, updated_at
		FROM coordination_incidents;
		DROP TABLE coordination_incidents;
		ALTER TABLE coordination_incidents_v21 RENAME TO coordination_incidents;
		`+coordinationIncidentIndexes+`
		PRAGMA user_version = 21;
	`); err != nil {
		tx.Rollback()
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSchema24UpgradesVersion21CoordinationIncidentTriggerWithoutLosingBindings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident := createAttemptTestIncident(
		t,
		initial,
		"default",
		model.CoordinationTriggerIntegrationConflict,
	)
	current := time.Now().UTC()
	reserved, err := initial.ReserveCoordinationAttempt(
		ctx,
		reserveAttemptInput("schema-22-bound-attempt", incident, incident.GraphRevision, current),
	)
	if err != nil || !reserved.Reserved {
		initial.Close()
		t.Fatalf("reserve bound attempt: %+v, %v", reserved, err)
	}
	revision := reserved.Incident.GraphRevision
	proposal, created, err := initial.CreateCoordinationProposal(
		ctx,
		CreateCoordinationProposalInput{
			IncidentID: reserved.Incident.ID, AttemptID: &reserved.Attempt.ID,
			CoordinatorAgent:      "schema-22-coordinator",
			Status:                model.CoordinationProposalValidating,
			ExpectedGraphRevision: &revision,
			ClaimToken:            reserved.Incident.ClaimToken,
			Current:               current.Add(time.Second),
			Summary:               "Preserve the bound proposal",
			Rationale:             "The v21 to v22 rebuild must retain every binding.",
		},
	)
	if err != nil || !created {
		initial.Close()
		t.Fatalf("create bound proposal: created=%t value=%+v error=%v", created, proposal, err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeCoordinationIncidentSchemaToVersion21(t, path)

	reopened, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version int
	var definition string
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(
		ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'coordination_incidents'",
	).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if version != 26 || schemaVersion != 26 ||
		!strings.Contains(definition, "'run_invariant'") {
		t.Fatalf(
			"schema upgrade = version:%d constant:%d definition:%q",
			version,
			schemaVersion,
			definition,
		)
	}
	preserved, err := reopened.GetCoordinationIncident(ctx, reserved.Incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := reopened.ListCoordinationAttempts(
		ctx,
		CoordinationAttemptFilter{IncidentID: reserved.Incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := reopened.ListCoordinationProposals(
		ctx,
		CoordinationProposalFilter{IncidentID: reserved.Incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Status != reserved.Incident.Status ||
		preserved.ClaimToken != reserved.Incident.ClaimToken ||
		preserved.ClaimExpiresAt == nil || reserved.Incident.ClaimExpiresAt == nil ||
		*preserved.ClaimExpiresAt != *reserved.Incident.ClaimExpiresAt ||
		len(attempts) != 1 || attempts[0].ID != reserved.Attempt.ID ||
		len(proposals) != 1 || proposals[0].ID != proposal.ID ||
		proposals[0].AttemptID == nil || *proposals[0].AttemptID != reserved.Attempt.ID {
		t.Fatalf(
			"v21 rows were not preserved: incident=%+v attempts=%+v proposals=%+v",
			preserved,
			attempts,
			proposals,
		)
	}
	runInvariant, created, err := reopened.CreateCoordinationIncident(
		ctx,
		CreateCoordinationIncidentInput{
			Board: "default", Trigger: model.CoordinationTriggerRunInvariant,
			ExpectedGraphRevision: revisionPointer(revision),
			Summary:               "A run invariant failed",
		},
	)
	if err != nil || !created || runInvariant.Trigger != model.CoordinationTriggerRunInvariant {
		t.Fatalf("create run-invariant incident: created=%t value=%+v error=%v", created, runInvariant, err)
	}
	rows, err := reopened.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	hasViolation := rows.Next()
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		t.Fatal(errors.Join(rowsErr, closeErr))
	}
	if hasViolation {
		t.Fatal("v21 to v22 migration left a foreign key violation")
	}
	if _, err := reopened.db.ExecContext(
		ctx,
		"DELETE FROM coordination_incidents WHERE id = ?",
		reserved.Incident.ID,
	); err != nil {
		t.Fatal(err)
	}
	var attemptCount, proposalCount int
	if err := reopened.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM coordination_attempts WHERE id = ?",
		reserved.Attempt.ID,
	).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM coordination_proposals WHERE id = ?",
		proposal.ID,
	).Scan(&proposalCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 0 || proposalCount != 0 {
		t.Fatalf(
			"rebuilt incident cascade = attempts:%d proposals:%d",
			attemptCount,
			proposalCount,
		)
	}
}

func TestSchema22RollsBackVersion21IncidentRebuildOnForeignKeyViolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "autogora.db")
	initial, err := Open(path, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	incident := createAttemptTestIncident(
		t,
		initial,
		"default",
		model.CoordinationTriggerIntegrationConflict,
	)
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeCoordinationIncidentSchemaToVersion21(t, path)

	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO coordination_attempts(
			id, incident_id, board, status,
			selected_agent, selected_runtime, selected_model, selected_provider,
			selected_source, error, started_at, ended_at
		) VALUES (
			'orphan-v21-attempt', 'missing-v21-incident', 'default', 'failed',
			'', '', '', '', '', 'injected v21 corruption', ?, ?
		)
	`, timestamp, timestamp); err != nil {
		conn.Close()
		raw.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, "default", "")
	if err == nil {
		reopened.Close()
		t.Fatal("corrupt v21 database unexpectedly migrated")
	}
	if !strings.Contains(err.Error(), "foreign key violation") {
		t.Fatalf("corrupt v21 migration error = %v", err)
	}

	inspection, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var definition, summary string
	var version, incidentCount, orphanCount, rebuildCount int
	if err := inspection.QueryRowContext(
		ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'coordination_incidents'",
	).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(
		ctx,
		"SELECT summary FROM coordination_incidents WHERE id = ?",
		incident.ID,
	).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM coordination_incidents WHERE id = ?",
		incident.ID,
	).Scan(&incidentCount); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM coordination_attempts WHERE id = 'orphan-v21-attempt'",
	).Scan(&orphanCount); err != nil {
		t.Fatal(err)
	}
	if err := inspection.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'coordination_incidents_rebuild'",
	).Scan(&rebuildCount); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(definition, "'run_invariant'") ||
		version != 21 ||
		incidentCount != 1 ||
		summary != incident.Summary ||
		orphanCount != 1 ||
		rebuildCount != 0 {
		t.Fatalf(
			"failed migration changed v21 source: version=%d definition=%q incident=%d summary=%q orphan=%d rebuild=%d",
			version,
			definition,
			incidentCount,
			summary,
			orphanCount,
			rebuildCount,
		)
	}
}
