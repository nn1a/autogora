package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schemaVersion = 10

type Store struct {
	db              *sql.DB
	dbPath          string
	board           string
	attachmentsRoot string
}

func Open(dbPath, board, attachmentsRoot string) (*Store, error) {
	if strings.TrimSpace(board) == "" {
		board = "default"
	}
	resolved := dbPath
	if dbPath != ":memory:" {
		var err error
		resolved, err = filepath.Abs(dbPath)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	if attachmentsRoot == "" {
		attachmentsRoot = filepath.Join(filepath.Dir(resolved), "attachments")
	}
	attachmentsRoot, err := filepath.Abs(attachmentsRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve attachments path: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(resolved))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	store := &Store{db: db, dbPath: resolved, board: board, attachmentsRoot: attachmentsRoot}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func dataSourceName(path string) string {
	var source *url.URL
	if path == ":memory:" {
		source = &url.URL{Scheme: "file", Opaque: "autogora-" + uuid.NewString()}
	} else {
		source = &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	}
	query := source.Query()
	if path == ":memory:" {
		query.Set("mode", "memory")
		query.Set("cache", "shared")
	} else {
		query.Add("_pragma", "journal_mode(WAL)")
	}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Set("_txlock", "immediate")
	source.RawQuery = query.Encode()
	return source.String()
}

func (s *Store) Close() error            { return s.db.Close() }
func (s *Store) DBPath() string          { return s.dbPath }
func (s *Store) Board() string           { return s.board }
func (s *Store) AttachmentsRoot() string { return s.attachmentsRoot }

func (s *Store) initialize(ctx context.Context) error {
	var definition string
	err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&definition)
	switch {
	case err == sql.ErrNoRows:
		if _, err := s.db.ExecContext(ctx, latestSchema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect schema: %w", err)
	case !strings.Contains(definition, "'scheduled'") || !strings.Contains(definition, "idempotency_key"):
		if err := s.migrateLegacySchema(ctx); err != nil {
			return err
		}
	case !strings.Contains(definition, "'cline'") || !strings.Contains(definition, "'gemini'"):
		if err := s.migrateRuntimeSchema(ctx); err != nil {
			return err
		}
	default:
		if _, err := s.db.ExecContext(ctx, latestSchema); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	if err := s.ensureDependencySatisfaction(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

func (s *Store) ensureDependencySatisfaction(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(task_links)")
	if err != nil {
		return fmt.Errorf("inspect task links: %w", err)
	}
	hasColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan task link column: %w", err)
		}
		if name == "satisfied_at" {
			hasColumn = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasColumn {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE task_links ADD COLUMN satisfied_at TEXT"); err != nil {
			return fmt.Errorf("add dependency satisfaction: %w", err)
		}
	}
	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_task_links_child_state ON task_links(child_id, satisfied_at);
		UPDATE task_links
		SET satisfied_at = COALESCE(
			(SELECT MAX(e.created_at) FROM task_events e WHERE e.task_id = task_links.parent_id AND e.kind = 'completed'),
			(SELECT p.updated_at FROM tasks p WHERE p.id = task_links.parent_id)
		)
		WHERE satisfied_at IS NULL
			AND EXISTS (
				SELECT 1 FROM tasks p
				WHERE p.id = task_links.parent_id
					AND (p.status = 'done' OR (p.status = 'archived' AND EXISTS (
						SELECT 1 FROM task_events e WHERE e.task_id = p.id AND e.kind = 'completed'
					)))
			)
	`)
	if err != nil {
		return fmt.Errorf("backfill dependency satisfaction: %w", err)
	}
	return nil
}

func (s *Store) migrateRuntimeSchema(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON"); err != nil {
		return fmt.Errorf("prepare runtime migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_, _ = conn.ExecContext(context.Background(), "PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON")
	}()
	if _, err := conn.ExecContext(ctx, `
		BEGIN IMMEDIATE;
		DROP INDEX IF EXISTS idx_tasks_queue;
		DROP INDEX IF EXISTS idx_tasks_idempotency;
		DROP INDEX IF EXISTS idx_runs_task;
		ALTER TABLE task_runs RENAME TO task_runs_runtime_legacy;
		ALTER TABLE tasks RENAME TO tasks_runtime_legacy;
	`); err != nil {
		return fmt.Errorf("start runtime migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, latestSchema); err != nil {
		return fmt.Errorf("create runtime schema: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO tasks SELECT * FROM tasks_runtime_legacy;
		INSERT INTO task_runs SELECT * FROM task_runs_runtime_legacy;
		DROP TABLE task_runs_runtime_legacy;
		DROP TABLE tasks_runtime_legacy;
	`); err != nil {
		return fmt.Errorf("copy runtime schema: %w", err)
	}
	var violations int
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations > 0 {
		return fmt.Errorf("runtime migration produced %d foreign key violation(s)", violations)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit runtime migration: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) migrateLegacySchema(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("prepare legacy migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys = ON")
	}()
	if _, err := conn.ExecContext(ctx, `
		BEGIN IMMEDIATE;
		ALTER TABLE task_events RENAME TO task_events_legacy;
		ALTER TABLE task_runs RENAME TO task_runs_legacy;
		ALTER TABLE task_comments RENAME TO task_comments_legacy;
		ALTER TABLE task_links RENAME TO task_links_legacy;
		ALTER TABLE tasks RENAME TO tasks_legacy;
		DROP INDEX IF EXISTS idx_tasks_queue;
		DROP INDEX IF EXISTS idx_runs_task;
		DROP INDEX IF EXISTS idx_events_task;
	`); err != nil {
		return fmt.Errorf("start legacy migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, latestSchema); err != nil {
		return fmt.Errorf("create latest schema: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO tasks(
			id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
			priority, workspace, workspace_kind, branch, current_run_id, result,
			scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
			workflow_template_id, current_step_key, block_kind, block_reason,
			block_recurrences, failure_count, max_retries, created_at, updated_at
		)
		SELECT
			id, board, NULL, NULL, title, body, assignee, runtime, status,
			priority, workspace,
			CASE WHEN workspace IS NULL OR workspace = 'scratch' THEN 'scratch' ELSE 'dir' END,
			NULL, current_run_id, NULL, NULL, NULL, '[]', 0, 20,
			NULL, NULL, NULL, NULL, 0, failure_count, max_retries, created_at, updated_at
		FROM tasks_legacy;

		INSERT INTO task_links(parent_id, child_id) SELECT parent_id, child_id FROM task_links_legacy;
		INSERT INTO task_comments SELECT * FROM task_comments_legacy;
		INSERT INTO task_runs(
			id, task_id, worker_id, runtime, status, claim_token, claimed_at,
			claim_expires_at, heartbeat_at, ended_at, pid, log_path, exit_code,
			summary, metadata_json, error
		)
		SELECT
			id, task_id, worker_id, runtime, status, claim_token, claimed_at,
			strftime('%Y-%m-%dT%H:%M:%fZ', claimed_at, '+15 minutes'),
			heartbeat_at, ended_at, NULL, NULL, NULL, summary, metadata_json, error
		FROM task_runs_legacy;
		INSERT INTO task_events SELECT * FROM task_events_legacy;

		DROP TABLE task_events_legacy;
		DROP TABLE task_runs_legacy;
		DROP TABLE task_comments_legacy;
		DROP TABLE task_links_legacy;
		DROP TABLE tasks_legacy;
		COMMIT;
	`); err != nil {
		return fmt.Errorf("migrate legacy data: %w", err)
	}
	committed = true
	return nil
}

const latestSchema = `
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  board TEXT NOT NULL DEFAULT 'default',
  tenant TEXT,
  idempotency_key TEXT,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  assignee TEXT,
  runtime TEXT NOT NULL DEFAULT 'manual' CHECK (runtime IN ('claude', 'codex', 'cline', 'gemini', 'manual')),
  status TEXT NOT NULL CHECK (status IN ('triage', 'todo', 'scheduled', 'ready', 'running', 'blocked', 'review', 'done', 'archived')),
  priority INTEGER NOT NULL DEFAULT 0,
  workspace TEXT,
  workspace_kind TEXT NOT NULL DEFAULT 'scratch' CHECK (workspace_kind IN ('scratch', 'dir', 'worktree')),
  branch TEXT,
  current_run_id TEXT,
  result TEXT,
  scheduled_at TEXT,
  max_runtime_seconds INTEGER CHECK (max_runtime_seconds IS NULL OR max_runtime_seconds >= 1),
  skills_json TEXT NOT NULL DEFAULT '[]',
  goal_mode INTEGER NOT NULL DEFAULT 0 CHECK (goal_mode IN (0, 1)),
  goal_max_turns INTEGER NOT NULL DEFAULT 20 CHECK (goal_max_turns >= 1),
  workflow_template_id TEXT,
  current_step_key TEXT,
  block_kind TEXT CHECK (block_kind IS NULL OR block_kind IN ('dependency', 'needs_input', 'capability', 'transient')),
  block_reason TEXT,
  block_recurrences INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  max_retries INTEGER NOT NULL DEFAULT 2 CHECK (max_retries >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_links (
  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  satisfied_at TEXT,
  PRIMARY KEY (parent_id, child_id),
  CHECK (parent_id <> child_id)
);

CREATE TABLE IF NOT EXISTS task_hierarchy (
  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  child_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
  CHECK (parent_id <> child_id)
);

CREATE TABLE IF NOT EXISTS task_comments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_runs (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  worker_id TEXT NOT NULL,
  runtime TEXT NOT NULL CHECK (runtime IN ('claude', 'codex', 'cline', 'gemini', 'manual')),
  status TEXT NOT NULL,
  claim_token TEXT NOT NULL,
  claimed_at TEXT NOT NULL,
  claim_expires_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  ended_at TEXT,
  pid INTEGER,
  log_path TEXT,
  exit_code INTEGER,
  summary TEXT,
  metadata_json TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS run_workspaces (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('scratch', 'dir', 'worktree')),
  repository_path TEXT,
  base_commit TEXT,
  generated INTEGER NOT NULL DEFAULT 0 CHECK (generated IN (0, 1)),
  prepared_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS resource_leases (
  resource_key TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  acquired_at TEXT NOT NULL
);

CREATE TRIGGER IF NOT EXISTS release_terminal_run_resources
AFTER UPDATE OF status ON task_runs
WHEN NEW.status <> 'running'
BEGIN
  DELETE FROM resource_leases WHERE run_id = NEW.id;
END;

CREATE TABLE IF NOT EXISTS task_attachments (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('file', 'url')),
  name TEXT NOT NULL,
  media_type TEXT,
  size INTEGER,
  sha256 TEXT,
  path TEXT,
  url TEXT,
  created_at TEXT NOT NULL,
  CHECK ((kind = 'file' AND path IS NOT NULL AND url IS NULL) OR
         (kind = 'url' AND url IS NOT NULL AND path IS NULL))
);

CREATE TABLE IF NOT EXISTS task_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
  kind TEXT NOT NULL,
  payload_json TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS notification_subscriptions (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  platform TEXT NOT NULL,
  chat_id TEXT NOT NULL,
  thread_id TEXT NOT NULL DEFAULT '',
  user_id TEXT,
  event_kinds_json TEXT NOT NULL,
  secret TEXT,
  last_event_id INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(task_id, platform, chat_id, thread_id)
);

CREATE TABLE IF NOT EXISTS notification_deliveries (
  id TEXT PRIMARY KEY,
  subscription_id TEXT NOT NULL REFERENCES notification_subscriptions(id) ON DELETE CASCADE,
  event_id INTEGER NOT NULL REFERENCES task_events(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK (status IN ('pending', 'delivering', 'delivered')),
  attempts INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT,
  lease_expires_at TEXT,
  next_attempt_at TEXT NOT NULL,
  last_error TEXT,
  delivered_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(subscription_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_queue ON tasks(board, status, scheduled_at, runtime, priority DESC, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_idempotency ON tasks(board, idempotency_key) WHERE idempotency_key IS NOT NULL AND status <> 'archived';
CREATE INDEX IF NOT EXISTS idx_runs_task ON task_runs(task_id, claimed_at DESC);
CREATE INDEX IF NOT EXISTS idx_run_workspaces_task ON run_workspaces(task_id, prepared_at DESC);
CREATE INDEX IF NOT EXISTS idx_resource_leases_run ON resource_leases(run_id);
CREATE INDEX IF NOT EXISTS idx_task_hierarchy_parent ON task_hierarchy(parent_id, position, child_id);
CREATE INDEX IF NOT EXISTS idx_attachments_task ON task_attachments(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_subscriptions_task ON notification_subscriptions(task_id, platform, chat_id);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due ON notification_deliveries(status, next_attempt_at, lease_expires_at);
`
