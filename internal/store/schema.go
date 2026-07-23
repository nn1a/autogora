package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schemaVersion = 19

type Store struct {
	db              *sql.DB
	dbPath          string
	board           string
	attachmentsRoot string
	closeOnce       sync.Once
	closeHook       func() error
	closeErr        error
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

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		dbErr := s.db.Close()
		var hookErr error
		if s.closeHook != nil {
			hookErr = s.closeHook()
		}
		s.closeErr = errors.Join(dbErr, hookErr)
	})
	return s.closeErr
}

// SetCloseHook lets the board manager hold an operating-system shared lock for
// the Store lifetime. It must be called before the Store is returned to users.
func (s *Store) SetCloseHook(hook func() error) { s.closeHook = hook }

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
		// Runtime migration preserves workflow_role by name. Real v17 tables
		// need the additive column before their rows can be copied.
		if err := s.ensureTaskWorkflowRole(ctx); err != nil {
			return err
		}
		if err := s.migrateRuntimeSchema(ctx); err != nil {
			return err
		}
	default:
		if _, err := s.db.ExecContext(ctx, latestSchema); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	if err := s.ensureTaskWorkflowRole(ctx); err != nil {
		return err
	}
	if err := s.ensureDependencySatisfaction(ctx); err != nil {
		return err
	}
	if err := s.ensureBoardGraphState(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

func (s *Store) ensureBoardGraphState(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO board_graph_state(board, revision, updated_at)
		SELECT board, 0, COALESCE(MAX(updated_at), '') FROM tasks GROUP BY board
	`); err != nil {
		return fmt.Errorf("backfill board graph state: %w", err)
	}
	return nil
}

func (s *Store) ensureTaskWorkflowRole(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(tasks)")
	if err != nil {
		return fmt.Errorf("inspect task workflow role: %w", err)
	}
	hasWorkflowRole := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan task column: %w", err)
		}
		if name == "workflow_role" {
			hasWorkflowRole = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect task workflow role: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasWorkflowRole {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		ALTER TABLE tasks
		ADD COLUMN workflow_role TEXT NOT NULL DEFAULT 'worker'
			CHECK (workflow_role IN ('worker', 'reviewer', 'finalizer', 'control'))
	`); err != nil {
		return fmt.Errorf("add task workflow role: %w", err)
	}
	return nil
}

func (s *Store) ensureDependencySatisfaction(ctx context.Context) error {
	var currentVersion int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(task_links)")
	if err != nil {
		return fmt.Errorf("inspect task links: %w", err)
	}
	hasSatisfiedAt := false
	hasSatisfiedRunID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan task link column: %w", err)
		}
		switch name {
		case "satisfied_at":
			hasSatisfiedAt = true
		case "satisfied_run_id":
			hasSatisfiedRunID = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasSatisfiedAt {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE task_links ADD COLUMN satisfied_at TEXT"); err != nil {
			return fmt.Errorf("add dependency satisfaction: %w", err)
		}
	}
	if !hasSatisfiedRunID {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE task_links ADD COLUMN satisfied_run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL"); err != nil {
			return fmt.Errorf("add dependency satisfying run: %w", err)
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
	if currentVersion < 16 {
		_, err = s.db.ExecContext(ctx, `
			UPDATE task_links AS link
			SET satisfied_run_id = COALESCE(
				(
					SELECT event.run_id
					FROM task_events event
					JOIN task_runs run ON run.id = event.run_id
					WHERE event.task_id = link.parent_id
						AND event.kind = 'completed'
						AND event.run_id IS NOT NULL
						AND run.task_id = link.parent_id
					ORDER BY ABS(julianday(event.created_at) - julianday(link.satisfied_at)), event.id
					LIMIT 1
				),
				(
					SELECT run.id
					FROM task_runs run
					WHERE run.task_id = link.parent_id AND run.status = 'completed'
					ORDER BY ABS(julianday(COALESCE(run.ended_at, run.heartbeat_at)) - julianday(link.satisfied_at)), run.claimed_at
					LIMIT 1
				)
			)
			WHERE link.satisfied_at IS NOT NULL AND link.satisfied_run_id IS NULL
		`)
		if err != nil {
			return fmt.Errorf("backfill dependency satisfying run: %w", err)
		}
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
		INSERT INTO tasks(
			id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
			priority, workspace, workspace_kind, branch, current_run_id, result,
			scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
			workflow_template_id, current_step_key, block_kind, block_reason,
			block_recurrences, failure_count, max_retries, created_at, updated_at,
			workflow_role
		)
		SELECT
			id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
			priority, workspace, workspace_kind, branch, current_run_id, result,
			scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
			workflow_template_id, current_step_key, block_kind, block_reason,
			block_recurrences, failure_count, max_retries, created_at, updated_at,
			workflow_role
		FROM tasks_runtime_legacy;
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
  updated_at TEXT NOT NULL,
  workflow_role TEXT NOT NULL DEFAULT 'worker' CHECK (workflow_role IN ('worker', 'reviewer', 'finalizer', 'control'))
);

CREATE TABLE IF NOT EXISTS task_links (
  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  satisfied_at TEXT,
  satisfied_run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
  PRIMARY KEY (parent_id, child_id),
  CHECK (parent_id <> child_id)
);

CREATE TABLE IF NOT EXISTS task_hierarchy (
  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  child_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
  CHECK (parent_id <> child_id)
);

CREATE TABLE IF NOT EXISTS board_graph_state (
  board TEXT PRIMARY KEY,
  revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS coordination_incidents (
  id TEXT PRIMARY KEY,
  board TEXT NOT NULL,
  root_task_id TEXT,
  task_id TEXT,
  trigger TEXT NOT NULL CHECK (trigger IN ('repeated_block', 'retry_exhausted', 'graph_stalled', 'integration_conflict', 'agent_exhausted')),
  severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error', 'critical')),
  status TEXT NOT NULL CHECK (status IN ('open', 'coordinating', 'awaiting_approval', 'applying', 'resolved', 'dismissed', 'failed')),
  graph_revision INTEGER NOT NULL CHECK (graph_revision >= 0),
  summary TEXT NOT NULL,
  details_json TEXT NOT NULL DEFAULT '{}',
  claim_token TEXT,
  claim_expires_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (
    (status = 'coordinating' AND claim_token IS NOT NULL AND claim_token <> '' AND claim_expires_at IS NOT NULL)
    OR
    (status <> 'coordinating' AND claim_token IS NULL AND claim_expires_at IS NULL)
  )
);

CREATE TABLE IF NOT EXISTS coordination_proposals (
  id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES coordination_incidents(id) ON DELETE CASCADE,
  coordinator_agent TEXT NOT NULL,
  coordinator_model TEXT NOT NULL DEFAULT '',
  coordinator_provider TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('draft', 'validating', 'validated', 'awaiting_approval', 'approved', 'rejected', 'superseded', 'applying', 'applied', 'failed')),
  expected_graph_revision INTEGER NOT NULL CHECK (expected_graph_revision >= 0),
  summary TEXT NOT NULL,
  rationale TEXT NOT NULL,
  actions_json TEXT NOT NULL DEFAULT '[]',
  validation_errors_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  applied_at TEXT
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

CREATE TABLE IF NOT EXISTS run_agent_configs (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  profile TEXT NOT NULL,
  runtime TEXT NOT NULL CHECK (runtime IN ('claude', 'codex', 'cline', 'gemini', 'manual')),
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  source TEXT NOT NULL,
  fallback_from TEXT,
  configured_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_health (
  agent_id TEXT PRIMARY KEY,
  status TEXT NOT NULL CHECK (status IN ('unknown', 'ready', 'missing', 'auth_required', 'rate_limited', 'unhealthy')),
  cooldown_until TEXT,
  last_error TEXT,
  last_run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_leases (
  name TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  renewed_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS resource_leases (
  resource_key TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
  path TEXT NOT NULL,
  acquired_at TEXT NOT NULL
);

-- Cross-board coordination lives in the default board database. Run IDs belong
-- to separate board databases, so this table intentionally has no foreign keys.
CREATE TABLE IF NOT EXISTS global_workspace_leases (
  resource_key TEXT PRIMARY KEY,
  board TEXT NOT NULL,
  run_id TEXT NOT NULL,
  path TEXT NOT NULL,
  lease_token TEXT NOT NULL,
  acquired_at TEXT NOT NULL
);

-- Agent slots coordinate concurrency across every board. Run IDs belong to
-- board-local databases, so this table intentionally has no foreign keys.
CREATE TABLE IF NOT EXISTS global_agent_slots (
  agent_id TEXT NOT NULL,
  slot INTEGER NOT NULL CHECK (slot >= 1),
  owner_kind TEXT NOT NULL CHECK (owner_kind IN ('worker', 'planner', 'judge')),
  board TEXT NOT NULL,
  run_id TEXT,
  owner_id TEXT NOT NULL UNIQUE,
  lease_token TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  expires_at TEXT,
  PRIMARY KEY (agent_id, slot),
  CHECK (
    (owner_kind = 'worker' AND run_id IS NOT NULL AND expires_at IS NULL) OR
    (owner_kind IN ('planner', 'judge') AND expires_at IS NOT NULL)
  )
);

-- Board removal uses barriers in both the board-local database and the default
-- coordination database. The coordination row remains as a tombstone until a
-- new board with the same slug has been fully initialized.
CREATE TABLE IF NOT EXISTS board_removal_guards (
  board TEXT NOT NULL,
  scope TEXT NOT NULL CHECK (scope IN ('local', 'coordination')),
  token TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  PRIMARY KEY (board, scope)
);

CREATE TABLE IF NOT EXISTS run_terminal_requests (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('complete', 'block')),
  summary TEXT,
  result TEXT,
  metadata_json TEXT,
  artifacts_json TEXT NOT NULL DEFAULT '[]',
  block_kind TEXT,
  reason TEXT,
  requested_at TEXT NOT NULL,
  finalized_at TEXT
);

CREATE TABLE IF NOT EXISTS managed_runs (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  registered_at TEXT NOT NULL
);

-- Process safety state must outlive task-event retention. A PID is never
-- trusted without the exact process-start identity captured for that spawn.
CREATE TABLE IF NOT EXISTS run_process_identities (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  process_identity TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

-- A missing row means that the write policy predates durable policy tracking.
CREATE TABLE IF NOT EXISTS managed_run_policies (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  allow_writes INTEGER NOT NULL CHECK (allow_writes IN (0, 1)),
  recorded_at TEXT NOT NULL
);

-- Administrative termination is coordination state, not merely an audit
-- event. Spawn registration and supervisor recovery both consult this row.
CREATE TABLE IF NOT EXISTS run_reclaim_requests (
  run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  reason TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('reclaimed', 'timed_out')),
  count_failure INTEGER NOT NULL CHECK (count_failure IN (0, 1)),
  requested_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_change_sets (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  repository_path TEXT NOT NULL,
  worktree_path TEXT NOT NULL,
  base_commit TEXT NOT NULL,
  head_commit TEXT NOT NULL,
  durable_ref TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('ready', 'no_change')),
  changed_files_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL
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
CREATE INDEX IF NOT EXISTS idx_run_agent_configs_task ON run_agent_configs(task_id, configured_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_health_due ON agent_health(status, cooldown_until);
CREATE INDEX IF NOT EXISTS idx_service_leases_expiry ON service_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_resource_leases_run ON resource_leases(run_id);
CREATE INDEX IF NOT EXISTS idx_global_workspace_leases_owner ON global_workspace_leases(board, run_id);
CREATE INDEX IF NOT EXISTS idx_global_agent_slots_expiry ON global_agent_slots(owner_kind, expires_at);
CREATE INDEX IF NOT EXISTS idx_terminal_requests_pending ON run_terminal_requests(finalized_at, requested_at);
CREATE INDEX IF NOT EXISTS idx_change_sets_task ON task_change_sets(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_hierarchy_parent ON task_hierarchy(parent_id, position, child_id);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_board_status ON coordination_incidents(board, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_task ON coordination_incidents(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_root ON coordination_incidents(root_task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_claim_due ON coordination_incidents(status, claim_expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_coordination_incidents_active_dedupe
  ON coordination_incidents(board, trigger, IFNULL(root_task_id, ''), IFNULL(task_id, ''))
  WHERE status IN ('open', 'coordinating', 'awaiting_approval', 'applying');
CREATE INDEX IF NOT EXISTS idx_coordination_proposals_incident ON coordination_proposals(incident_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_attachments_task ON task_attachments(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_subscriptions_task ON notification_subscriptions(task_id, platform, chat_id);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due ON notification_deliveries(status, next_attempt_at, lease_expires_at);
`
