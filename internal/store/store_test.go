package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/nn1a/kanban/internal/model"
	_ "modernc.org/sqlite"
)

func stringValue(value string) *string { return &value }

func TestCreateListAndReadTaskUsingExistingJSONContract(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parent, err := store.CreateTask(ctx, CreateTaskInput{Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Child", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Parents: []string{parent.Task.ID}, Skills: []string{"review", "review", " testing "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Status != model.TaskStatusTodo {
		t.Fatalf("dependent task status = %q, want todo", child.Task.Status)
	}
	if len(child.Parents) != 1 || child.Parents[0].ID != parent.Task.ID {
		t.Fatalf("dependency was not persisted: %+v", child.Parents)
	}
	if len(child.Task.Skills) != 2 || child.Task.Skills[1] != "testing" {
		t.Fatalf("skills were not normalized: %#v", child.Task.Skills)
	}
	if _, err := store.AddComment(ctx, child.Task.ID, "human", "preserve this handoff"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetTask(ctx, child.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Comments) != 1 || loaded.Comments[0].Body != "preserve this handoff" {
		t.Fatalf("comment was not persisted: %+v", loaded.Comments)
	}
	listed, err := store.ListTasks(ctx, ListTaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d tasks, want 2", len(listed))
	}
	stats, err := store.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.ByStatus[model.TaskStatusTodo] != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestIdempotencyKeyReturnsExistingTask(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := "nightly-review"
	first, err := store.CreateTask(ctx, CreateTaskInput{Title: "First", IdempotencyKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, CreateTaskInput{Title: "Second", IdempotencyKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	if first.Task.ID != second.Task.ID || second.Task.Title != "First" {
		t.Fatalf("idempotency returned different task: first=%+v second=%+v", first.Task, second.Task)
	}
}

func TestLegacyMVPDatabaseMigratesWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	legacy, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(ctx, `
		CREATE TABLE tasks (
		  id TEXT PRIMARY KEY, board TEXT NOT NULL DEFAULT 'default', title TEXT NOT NULL,
		  body TEXT NOT NULL DEFAULT '', assignee TEXT, runtime TEXT NOT NULL,
		  status TEXT NOT NULL CHECK (status IN ('triage','todo','ready','running','blocked','done','archived')),
		  priority INTEGER NOT NULL DEFAULT 0, workspace TEXT, current_run_id TEXT,
		  failure_count INTEGER NOT NULL DEFAULT 0, max_retries INTEGER NOT NULL DEFAULT 2,
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE task_links (
		  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  PRIMARY KEY(parent_id, child_id)
		);
		CREATE TABLE task_comments (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  author TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL
		);
		CREATE TABLE task_runs (
		  id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  worker_id TEXT NOT NULL, runtime TEXT NOT NULL, status TEXT NOT NULL,
		  claim_token TEXT NOT NULL, claimed_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL,
		  ended_at TEXT, summary TEXT, metadata_json TEXT, error TEXT
		);
		CREATE TABLE task_events (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
		  kind TEXT NOT NULL, payload_json TEXT, created_at TEXT NOT NULL
		);
		INSERT INTO tasks VALUES (
		  't_parent','default','legacy parent','body','worker','codex','done',1,'/tmp/work',NULL,0,2,
		  '2026-07-20T00:00:00.000Z','2026-07-20T00:01:00.000Z'
		);
		INSERT INTO tasks VALUES (
		  't_child','default','legacy child','','worker','claude','todo',0,NULL,NULL,0,2,
		  '2026-07-20T00:02:00.000Z','2026-07-20T00:02:00.000Z'
		);
		INSERT INTO task_links VALUES ('t_parent','t_child');
		INSERT INTO task_comments(task_id,author,body,created_at) VALUES ('t_child','human','keep this','2026-07-20T00:03:00.000Z');
		INSERT INTO task_runs VALUES (
		  'r_old','t_parent','worker','codex','completed','secret','2026-07-20T00:00:00.000Z',
		  '2026-07-20T00:01:00.000Z','2026-07-20T00:01:00.000Z','done','{}',NULL
		);
		INSERT INTO task_events(task_id,run_id,kind,payload_json,created_at) VALUES (
		  't_parent','r_old','completed','{}','2026-07-20T00:01:00.000Z'
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	parent, err := migrated.GetTask(ctx, "t_parent")
	if err != nil {
		t.Fatal(err)
	}
	child, err := migrated.GetTask(ctx, "t_child")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Task.WorkspaceKind != model.WorkspaceDir || len(parent.Runs) != 1 || parent.Runs[0].ID != "r_old" {
		t.Fatalf("parent migration lost data: %+v", parent)
	}
	if len(child.Parents) != 1 || len(child.Comments) != 1 || child.Comments[0].Body != "keep this" {
		t.Fatalf("child migration lost data: %+v", child)
	}
	var version int
	if err := migrated.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
}
