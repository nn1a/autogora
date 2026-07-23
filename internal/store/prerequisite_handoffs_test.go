package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func insertChangeSetForTest(t *testing.T, store *Store, runID, taskID, suffix string) string {
	t.Helper()
	id := "cs_" + suffix
	_, err := store.db.Exec(`INSERT INTO task_change_sets(
		id, run_id, task_id, repository_path, worktree_path, base_commit, head_commit,
		durable_ref, state, changed_files_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?)`, id, runID, taskID,
		"/repo", "/worktree/"+suffix, "base-"+suffix, "head-"+suffix,
		"refs/autogora/runs/"+runID, `["file-`+suffix+`.txt"]`, now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func completeClaimedTask(t *testing.T, store *Store, taskID, summary string) string {
	t.Helper()
	ctx := context.Background()
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: taskID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%v err=%v", claim, err)
	}
	if _, err := store.CompleteRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, CompletionInput{Summary: summary}); err != nil {
		t.Fatal(err)
	}
	return claim.Run.ID
}

func TestPrerequisiteHandoffsPinTheSatisfyingRunAndChangeSet(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parent, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Produce change", Assignee: stringValue("builder"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Consume existing edge", Assignee: stringValue("reviewer"), Runtime: model.RuntimeClaude,
		Parents: []string{parent.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstRunID := completeClaimedTask(t, store, parent.Task.ID, "first completion")
	firstChangeSetID := insertChangeSetForTest(t, store, firstRunID, parent.Task.ID, "first")

	after, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Consume late edge", Assignee: stringValue("reviewer"), Runtime: model.RuntimeClaude,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkTasks(ctx, parent.Task.ID, after.Task.ID); err != nil {
		t.Fatal(err)
	}

	for _, dependentID := range []string{before.Task.ID, after.Task.ID} {
		handoffs, err := store.ListPrerequisiteHandoffs(ctx, dependentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(handoffs) != 1 || handoffs[0].SatisfiedRunID == nil || *handoffs[0].SatisfiedRunID != firstRunID ||
			handoffs[0].Run == nil || handoffs[0].Run.ID != firstRunID || handoffs[0].Run.Summary == nil ||
			*handoffs[0].Run.Summary != "first completion" || handoffs[0].ChangeSet == nil ||
			handoffs[0].ChangeSet.ID != firstChangeSetID {
			t.Fatalf("unexpected handoff for %s: %+v", dependentID, handoffs)
		}
	}

	// An explicit Ready transition records deliberate rerun intent, so the
	// recent-success guard does not impose an arbitrary one-hour delay.
	ready := model.TaskStatusReady
	if _, err := store.UpdateTask(ctx, parent.Task.ID, UpdateTaskInput{Status: &ready}); err != nil {
		t.Fatal(err)
	}
	secondRunID := completeClaimedTask(t, store, parent.Task.ID, "second completion")
	insertChangeSetForTest(t, store, secondRunID, parent.Task.ID, "second")

	detail, err := store.GetTask(ctx, before.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.PrerequisiteHandoffs) != 1 || detail.PrerequisiteHandoffs[0].SatisfiedRunID == nil ||
		*detail.PrerequisiteHandoffs[0].SatisfiedRunID != firstRunID || detail.PrerequisiteHandoffs[0].ChangeSet == nil ||
		detail.PrerequisiteHandoffs[0].ChangeSet.ID != firstChangeSetID {
		t.Fatalf("newer completion replaced pinned task handoff: %+v", detail.PrerequisiteHandoffs)
	}
	graph, err := store.RelationshipGraph(ctx, before.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pinnedEdge *model.DependencyEdge
	for index := range graph.Dependencies {
		if graph.Dependencies[index].DependentID == before.Task.ID {
			pinnedEdge = &graph.Dependencies[index]
		}
	}
	if pinnedEdge == nil || pinnedEdge.SatisfiedRunID == nil || *pinnedEdge.SatisfiedRunID != firstRunID {
		t.Fatalf("graph omitted satisfying run: %+v", graph.Dependencies)
	}
	workerContext, err := store.BuildWorkerContext(ctx, before.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(workerContext, "first completion") || strings.Contains(workerContext, "second completion") {
		t.Fatalf("worker context did not use the pinned handoff:\n%s", workerContext)
	}
}

func TestSchemaMigrationBackfillsDependencySatisfyingRun(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "version-15.db")
	db, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := strings.Replace(latestSchema,
		"  satisfied_run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,\n", "", 1)
	if _, err := db.ExecContext(ctx, oldSchema); err != nil {
		t.Fatal(err)
	}
	const completedAt = "2026-07-23T01:02:03.000Z"
	_, err = db.ExecContext(ctx, `
		INSERT INTO tasks(id, title, runtime, status, created_at, updated_at)
		VALUES ('t_parent', 'Parent', 'codex', 'done', ?, ?),
		       ('t_child', 'Child', 'claude', 'ready', ?, ?);
		INSERT INTO task_runs(id, task_id, worker_id, runtime, status, claim_token,
			claimed_at, claim_expires_at, heartbeat_at, ended_at, summary)
		VALUES ('r_parent', 't_parent', 'worker', 'codex', 'completed', 'token', ?, ?, ?, ?, 'pinned result');
		INSERT INTO task_events(task_id, run_id, kind, payload_json, created_at)
		VALUES ('t_parent', 'r_parent', 'completed', '{}', ?);
		INSERT INTO task_runs(id, task_id, worker_id, runtime, status, claim_token,
			claimed_at, claim_expires_at, heartbeat_at, ended_at, summary)
		VALUES ('r_later', 't_parent', 'worker', 'codex', 'completed', 'later-token',
			'2026-07-24T01:02:03.000Z', '2026-07-24T01:02:03.000Z',
			'2026-07-24T01:02:03.000Z', '2026-07-24T01:02:03.000Z', 'later result');
		INSERT INTO task_events(task_id, run_id, kind, payload_json, created_at)
		VALUES ('t_parent', 'r_later', 'completed', '{}', '2026-07-24T01:02:03.000Z');
		INSERT INTO task_links(parent_id, child_id, satisfied_at)
		VALUES ('t_parent', 't_child', ?);
		PRAGMA user_version = 15;
	`, completedAt, completedAt, completedAt, completedAt,
		completedAt, completedAt, completedAt, completedAt, completedAt, completedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	handoffs, err := migrated.ListPrerequisiteHandoffs(ctx, "t_child")
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].SatisfiedRunID == nil || *handoffs[0].SatisfiedRunID != "r_parent" ||
		handoffs[0].Run == nil || handoffs[0].Run.ID != "r_parent" {
		t.Fatalf("migration did not backfill satisfying run: %+v", handoffs)
	}
	var version int
	if err := migrated.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
}
