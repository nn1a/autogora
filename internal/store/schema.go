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
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schemaVersion = 26

type Store struct {
	db               *sql.DB
	dbPath           string
	board            string
	attachmentsRoot  string
	publicationClock func() time.Time
	automation       *automationGateRuntime
	// Test seam for simulating a crash after source resolution is durable but
	// before the global gate is cleared.
	automationAfterConfirmationPhaseOne func() error
	closeOnce                           sync.Once
	closeHook                           func() error
	closeErr                            error
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

	store := &Store{
		db: db, dbPath: resolved, board: board, attachmentsRoot: attachmentsRoot,
		publicationClock: func() time.Time { return time.Now().UTC() },
	}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if board == "default" {
		if err := store.ConfigureAutomationGate(AutomationGateConfig{
			AuthorityDBPath: resolved,
		}); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure automation gate: %w", err)
		}
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
		var automationErr error
		if s.automation != nil && s.automation.authorityOwned {
			automationErr = s.automation.authorityDB.Close()
		}
		if s.automation != nil && s.automation.ephemeralLock {
			if err := os.Remove(s.automation.lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				automationErr = errors.Join(
					automationErr,
					secretSafeAutomationLockError(err),
				)
			}
		}
		dbErr := s.db.Close()
		var hookErr error
		if s.closeHook != nil {
			hookErr = s.closeHook()
		}
		s.closeErr = errors.Join(automationErr, dbErr, hookErr)
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
	if err := s.requireSupportedCoordinationProposalSchema(ctx); err != nil {
		return err
	}
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
	if err := s.ensurePublicationClaimEpochSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureDependencySatisfaction(ctx); err != nil {
		return err
	}
	if err := s.ensureBoardGraphState(ctx); err != nil {
		return err
	}
	if err := s.ensureCoordinationIncidentTriggerSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureIntegrationResolutionResolvedSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureRunRecoveryFenceSchema(ctx); err != nil {
		return err
	}
	if err := s.validateRecoveryCompletionInvariants(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

func (s *Store) ensurePublicationClaimEpochSchema(ctx context.Context) (resultErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open publication claim epoch migration connection: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin publication claim epoch migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, "PRAGMA table_info(publications)")
	if err != nil {
		return fmt.Errorf("inspect publication schema: %w", err)
	}
	tableExists := false
	hasClaimEpoch := false
	for rows.Next() {
		tableExists = true
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&name,
			&columnType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan publication schema: %w", err)
		}
		if name == "claim_epoch" {
			hasClaimEpoch = true
			if !strings.EqualFold(strings.TrimSpace(columnType), "INTEGER") ||
				notNull != 1 ||
				fmt.Sprint(defaultValue) != "0" {
				rows.Close()
				return errors.New(
					"incompatible publication schema: claim_epoch must be an INTEGER NOT NULL DEFAULT 0",
				)
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect publication schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !tableExists {
		return errors.New("publication table is missing after schema initialization")
	}
	if !hasClaimEpoch {
		if _, err := conn.ExecContext(
			ctx,
			`ALTER TABLE publications
			 ADD COLUMN claim_epoch INTEGER NOT NULL DEFAULT 0
			 CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 0)`,
		); err != nil {
			return fmt.Errorf("add publication claim epoch: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS publications_claim_epoch_insert_guard;
		CREATE TRIGGER publications_claim_epoch_insert_guard
		BEFORE INSERT ON publications
		WHEN typeof(NEW.claim_epoch) <> 'integer' OR NEW.claim_epoch < 0
		BEGIN
			SELECT RAISE(ABORT, 'publication claim_epoch must be a non-negative integer');
		END;

		DROP TRIGGER IF EXISTS publications_claim_epoch_update_guard;
		CREATE TRIGGER publications_claim_epoch_update_guard
		BEFORE UPDATE OF claim_epoch ON publications
		WHEN typeof(NEW.claim_epoch) <> 'integer' OR NEW.claim_epoch < 0
		BEGIN
			SELECT RAISE(ABORT, 'publication claim_epoch must be a non-negative integer');
		END;
	`); err != nil {
		return fmt.Errorf("ensure publication claim epoch guards: %w", err)
	}
	var invalidRows int
	if err := conn.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM publications
		 WHERE typeof(claim_epoch) <> 'integer' OR claim_epoch < 0`,
	).Scan(&invalidRows); err != nil {
		return fmt.Errorf("validate publication claim epochs: %w", err)
	}
	if invalidRows != 0 {
		return fmt.Errorf(
			"incompatible publication schema: %d invalid claim epochs",
			invalidRows,
		)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit publication claim epoch migration: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) ensureRunRecoveryFenceSchema(ctx context.Context) (resultErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open run recovery fence migration connection: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin run recovery fence migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var definition string
	if err := conn.QueryRowContext(
		ctx,
		`SELECT sql FROM sqlite_master
		 WHERE type = 'table' AND name = 'run_reclaim_requests'`,
	).Scan(&definition); err != nil {
		return fmt.Errorf("inspect run recovery fence schema: %w", err)
	}
	current := strings.ToLower(definition)
	hasDurableFenceState := strings.Contains(current, "requires_operator") &&
		strings.Contains(current, "diagnostic_code") &&
		strings.Contains(current, "fence_token") &&
		strings.Contains(current, "fence_generation") &&
		strings.Contains(current, "host_acknowledged_at") &&
		strings.Contains(current, "recovery_owner_token") &&
		strings.Contains(current, "recovery_owner_expires_at") &&
		strings.Contains(current, "'crashed'")
	if !strings.Contains(current, "requires_operator") ||
		!strings.Contains(current, "diagnostic_code") ||
		!strings.Contains(current, "fence_token") ||
		!strings.Contains(current, "fence_generation") ||
		!strings.Contains(current, "host_acknowledged_at") ||
		!strings.Contains(current, "host_acknowledged_fence_token") ||
		!strings.Contains(current, "recovery_owner_token") ||
		!strings.Contains(current, "recovery_owner_expires_at") ||
		!strings.Contains(current, "operator_quiesced_at") ||
		!strings.Contains(current, "operator_quiesced_by") ||
		!strings.Contains(current, "operator_quiescence_reason") ||
		!strings.Contains(current, "operator_quiesced_generation") ||
		!strings.Contains(current, "operator_confirmed_worker_stopped") ||
		!strings.Contains(current, "operator_confirmed_host_writes_stopped") ||
		!strings.Contains(current, "operator_observed_heartbeat_at") ||
		!strings.Contains(current, "operator_observed_claim_expires_at") ||
		!strings.Contains(current, "operator_observed_pid") ||
		!strings.Contains(current, "operator_observed_process_identity") ||
		!strings.Contains(current, "'crashed'") {
		if _, err := conn.ExecContext(
			ctx,
			`ALTER TABLE run_reclaim_requests
			 RENAME TO run_reclaim_requests_before_v25`,
		); err != nil {
			return fmt.Errorf("rename old run recovery fence table: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `CREATE TABLE run_reclaim_requests (
			run_id TEXT PRIMARY KEY REFERENCES task_runs(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			reason TEXT NOT NULL,
			outcome TEXT NOT NULL CHECK (outcome IN ('reclaimed', 'timed_out', 'crashed')),
			count_failure INTEGER NOT NULL CHECK (count_failure IN (0, 1)),
			requested_at TEXT NOT NULL,
			requires_operator INTEGER NOT NULL DEFAULT 0 CHECK (requires_operator IN (0, 1)),
			diagnostic_code TEXT,
			fence_token TEXT NOT NULL,
			fence_generation INTEGER NOT NULL DEFAULT 1 CHECK (fence_generation >= 1),
			host_acknowledged_at TEXT,
			host_acknowledged_fence_token TEXT,
			recovery_owner_token TEXT,
			recovery_owner_expires_at TEXT,
			operator_quiesced_at TEXT,
			operator_quiesced_by TEXT,
			operator_quiescence_reason TEXT,
			operator_quiesced_generation INTEGER,
			operator_confirmed_worker_stopped INTEGER NOT NULL DEFAULT 0
				CHECK (operator_confirmed_worker_stopped IN (0, 1)),
			operator_confirmed_host_writes_stopped INTEGER NOT NULL DEFAULT 0
				CHECK (operator_confirmed_host_writes_stopped IN (0, 1)),
			operator_observed_heartbeat_at TEXT,
			operator_observed_claim_expires_at TEXT,
			operator_observed_pid INTEGER,
			operator_observed_process_identity TEXT,
			CHECK (
				(recovery_owner_token IS NULL AND recovery_owner_expires_at IS NULL) OR
				(recovery_owner_token IS NOT NULL AND recovery_owner_expires_at IS NOT NULL)
			),
			CHECK (
				(host_acknowledged_at IS NULL AND host_acknowledged_fence_token IS NULL) OR
				(host_acknowledged_at IS NOT NULL AND host_acknowledged_fence_token IS NOT NULL)
			),
			CHECK (
				(operator_quiesced_at IS NULL
				 AND operator_quiesced_by IS NULL
				 AND operator_quiescence_reason IS NULL
				 AND operator_quiesced_generation IS NULL
				 AND operator_confirmed_worker_stopped = 0
				 AND operator_confirmed_host_writes_stopped = 0
				 AND operator_observed_heartbeat_at IS NULL
				 AND operator_observed_claim_expires_at IS NULL
				 AND operator_observed_pid IS NULL
				 AND operator_observed_process_identity IS NULL)
				OR
				(operator_quiesced_at IS NOT NULL
				 AND operator_quiesced_by IS NOT NULL
				 AND operator_quiescence_reason IS NOT NULL
				 AND operator_quiesced_generation = fence_generation
				 AND operator_confirmed_worker_stopped = 1
				 AND operator_confirmed_host_writes_stopped = 1
				 AND operator_observed_heartbeat_at IS NOT NULL
				 AND operator_observed_claim_expires_at IS NOT NULL)
			)
		)`); err != nil {
			return fmt.Errorf("create run recovery fence table: %w", err)
		}
		if hasDurableFenceState {
			if _, err := conn.ExecContext(ctx, `INSERT INTO run_reclaim_requests(
					run_id, expires_at, reason, outcome, count_failure, requested_at,
					requires_operator, diagnostic_code, fence_token,
					fence_generation, host_acknowledged_at,
					host_acknowledged_fence_token,
					recovery_owner_token, recovery_owner_expires_at
				)
				SELECT run_id, expires_at, reason, outcome, count_failure, requested_at,
					requires_operator, diagnostic_code, fence_token,
					fence_generation, host_acknowledged_at,
					CASE WHEN host_acknowledged_at IS NULL THEN NULL ELSE fence_token END,
					recovery_owner_token, recovery_owner_expires_at
				FROM run_reclaim_requests_before_v25`); err != nil {
				return fmt.Errorf("copy durable run recovery fences: %w", err)
			}
		} else {
			if _, err := conn.ExecContext(ctx, `INSERT INTO run_reclaim_requests(
					run_id, expires_at, reason, outcome, count_failure, requested_at,
					requires_operator, diagnostic_code, fence_token,
					fence_generation, host_acknowledged_at,
					host_acknowledged_fence_token,
					recovery_owner_token, recovery_owner_expires_at
				)
				SELECT run_id, expires_at, reason, outcome, count_failure, requested_at,
					0, NULL, lower(hex(randomblob(24))), 1, NULL, NULL, NULL, NULL
				FROM run_reclaim_requests_before_v25`); err != nil {
				return fmt.Errorf("copy legacy run recovery fences: %w", err)
			}
		}
		if _, err := conn.ExecContext(
			ctx,
			"DROP TABLE run_reclaim_requests_before_v25",
		); err != nil {
			return fmt.Errorf("drop old run recovery fence table: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit run recovery fence migration: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) ensureIntegrationResolutionResolvedSchema(ctx context.Context) (resultErr error) {
	// Serialize the inspection and additive migration. Two processes may open
	// the same v23 database at once; checking outside the write transaction
	// lets both observe the missing column before one ALTER wins.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open integration resolution migration connection: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin integration resolution migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, "PRAGMA table_info(integration_resolution_attempts)")
	if err != nil {
		return fmt.Errorf("inspect integration resolution schema: %w", err)
	}
	tableExists := false
	hasResolvedAt := false
	for rows.Next() {
		tableExists = true
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&name,
			&columnType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan integration resolution schema: %w", err)
		}
		if name == "resolved_at" {
			hasResolvedAt = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect integration resolution schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !tableExists {
		return errors.New("integration resolution ledger is missing after schema initialization")
	}
	if !hasResolvedAt {
		if _, err := conn.ExecContext(
			ctx,
			"ALTER TABLE integration_resolution_attempts ADD COLUMN resolved_at TEXT",
		); err != nil {
			return fmt.Errorf("add integration resolution resolved timestamp: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, integrationResolutionInvariantTriggers); err != nil {
		return fmt.Errorf("ensure integration resolution invariants: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit integration resolution migration: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) validateRecoveryCompletionInvariants(ctx context.Context) error {
	var taskID, checkpointID, state string
	err := s.db.QueryRowContext(ctx, `SELECT task.id, checkpoint.id, checkpoint.state
		FROM tasks task
		JOIN recovery_checkpoints checkpoint ON checkpoint.task_id = task.id
		WHERE task.status = 'done'
			AND checkpoint.state IN ('pending', 'reserved', 'adopted')
		ORDER BY task.id, checkpoint.id
		LIMIT 1`).Scan(&taskID, &checkpointID, &state)
	if err == nil {
		return fmt.Errorf(
			"incompatible recovery state: done task %s has active checkpoint %s in state %s",
			taskID,
			checkpointID,
			state,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("validate done task recovery state: %w", err)
	}

	var runID string
	err = s.db.QueryRowContext(ctx, `SELECT resolution.run_id
		FROM integration_resolution_attempts resolution
		LEFT JOIN recovery_checkpoints checkpoint
			ON checkpoint.reserved_run_id = resolution.run_id
		LEFT JOIN run_workspaces workspace ON workspace.run_id = resolution.run_id
		WHERE
			(
				resolution.resolved_at IS NOT NULL
				AND (
					resolution.attempt IS NULL
					OR resolution.started_at IS NULL
					OR checkpoint.id IS NULL
					OR checkpoint.state NOT IN ('adopted', 'consumed', 'superseded')
					OR checkpoint.adopted_output_base_commit IS NULL
					OR checkpoint.adopted_head_commit IS NULL
					OR workspace.base_commit IS NULL
					OR workspace.base_commit <> checkpoint.adopted_output_base_commit
				)
			)
			OR
			(
				resolution.resolved_at IS NULL
				AND checkpoint.state IN ('adopted', 'consumed')
			)
		ORDER BY resolution.run_id
		LIMIT 1`).Scan(&runID)
	if err == nil {
		return fmt.Errorf(
			"incompatible integration recovery state for run %s",
			runID,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("validate integration recovery state: %w", err)
	}
	return nil
}

func (s *Store) requireSupportedCoordinationProposalSchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(coordination_proposals)")
	if err != nil {
		return fmt.Errorf("inspect coordination proposal schema: %w", err)
	}
	defer rows.Close()

	tableExists := false
	hasAttemptID := false
	for rows.Next() {
		tableExists = true
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan coordination proposal schema: %w", err)
		}
		if name == "attempt_id" {
			hasAttemptID = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect coordination proposal schema: %w", err)
	}
	if tableExists && !hasAttemptID {
		return errors.New("incompatible coordination schema: coordination_proposals.attempt_id is missing; this pre-release database cannot be upgraded, use a fresh store or reset the data directory")
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

func (s *Store) ensureCoordinationIncidentTriggerSchema(ctx context.Context) (resultErr error) {
	var definition string
	err := s.db.QueryRowContext(
		ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'coordination_incidents'",
	).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect coordination incident schema: %w", err)
	}
	normalized := strings.Join(strings.Fields(strings.ToLower(definition)), " ")
	if strings.Contains(normalized, "'run_invariant'") {
		return nil
	}
	legacyTriggerCheck := strings.Join(strings.Fields(strings.ToLower(
		"trigger TEXT NOT NULL CHECK (trigger IN ('repeated_block', 'retry_exhausted', 'graph_stalled', 'integration_conflict', 'agent_exhausted'))",
	)), " ")
	if !strings.Contains(normalized, legacyTriggerCheck) {
		return errors.New(
			"incompatible coordination incident schema: trigger constraint cannot be upgraded safely",
		)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open coordination incident migration connection: %w", err)
	}
	foreignKeysDisabled := false
	var tx *sql.Tx
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if tx != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
		if foreignKeysDisabled {
			_, restoreErr := conn.ExecContext(cleanupCtx, "PRAGMA foreign_keys = ON")
			if restoreErr != nil {
				resultErr = errors.Join(
					resultErr,
					fmt.Errorf("restore coordination incident foreign keys: %w", restoreErr),
				)
			}
		}
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("prepare coordination incident migration: %w", err)
	}
	foreignKeysDisabled = true
	tx, err = conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start coordination incident migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE coordination_incidents_rebuild `+coordinationIncidentTableDefinition+`;
		INSERT INTO coordination_incidents_rebuild(
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
		ALTER TABLE coordination_incidents_rebuild RENAME TO coordination_incidents;
		`+coordinationIncidentIndexes); err != nil {
		return fmt.Errorf("rebuild coordination incident schema: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check rebuilt coordination incident foreign keys: %w", err)
	}
	violations := 0
	for rows.Next() {
		violations++
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return errors.Join(rowsErr, closeErr)
	}
	if violations != 0 {
		return fmt.Errorf(
			"coordination incident migration produced %d foreign key violation(s)",
			violations,
		)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit coordination incident migration: %w", err)
	}
	tx = nil
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("restore coordination incident foreign keys: %w", err)
	}
	foreignKeysDisabled = false
	var foreignKeysEnabled int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeysEnabled); err != nil {
		return fmt.Errorf("verify coordination incident foreign keys: %w", err)
	}
	if foreignKeysEnabled != 1 {
		return errors.New("coordination incident migration left foreign keys disabled")
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

const coordinationIncidentTableDefinition = `(
  id TEXT PRIMARY KEY,
  board TEXT NOT NULL,
  root_task_id TEXT,
  task_id TEXT,
  trigger TEXT NOT NULL CHECK (trigger IN ('repeated_block', 'retry_exhausted', 'graph_stalled', 'integration_conflict', 'agent_exhausted', 'run_invariant')),
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
)`

const coordinationIncidentIndexes = `
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_board_status ON coordination_incidents(board, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_task ON coordination_incidents(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_root ON coordination_incidents(root_task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_incidents_claim_due ON coordination_incidents(status, claim_expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_coordination_incidents_active_dedupe
  ON coordination_incidents(board, trigger, IFNULL(root_task_id, ''), IFNULL(task_id, ''))
  WHERE status IN ('open', 'coordinating', 'awaiting_approval', 'applying');`

const integrationResolutionInvariantTriggers = `
CREATE TRIGGER IF NOT EXISTS integration_resolution_insert_requires_started_recovery
BEFORE INSERT ON integration_resolution_attempts
WHEN NEW.resolved_at IS NOT NULL AND (
  NEW.attempt IS NULL
  OR NEW.started_at IS NULL
  OR NOT EXISTS (
    SELECT 1 FROM recovery_checkpoints checkpoint
    WHERE checkpoint.reserved_run_id = NEW.run_id
      AND checkpoint.state = 'adopted'
  )
)
BEGIN
  SELECT RAISE(ABORT, 'resolved integration requires started adopted recovery');
END;

CREATE TRIGGER IF NOT EXISTS integration_resolution_update_requires_started_recovery
BEFORE UPDATE OF attempt, started_at, resolved_at ON integration_resolution_attempts
WHEN
  (OLD.resolved_at IS NOT NULL AND OLD.resolved_at IS NOT NEW.resolved_at)
  OR
  (
    NEW.resolved_at IS NOT NULL
    AND (
      NEW.attempt IS NULL
      OR NEW.started_at IS NULL
      OR NOT EXISTS (
        SELECT 1 FROM recovery_checkpoints checkpoint
        WHERE checkpoint.reserved_run_id = NEW.run_id
          AND checkpoint.state = 'adopted'
      )
    )
  )
BEGIN
  SELECT RAISE(ABORT, 'resolved integration requires immutable started adopted recovery');
END;

-- ConfirmRecoveryAfterIntegrationResolution intentionally updates the
-- workspace base before it records resolved_at in the same transaction. Once
-- that marker exists, changing the base would break the durable bridge between
-- the resolved graph, adopted checkpoint, and Finalizer verification turn.
CREATE TRIGGER IF NOT EXISTS run_workspace_prevent_confirmed_integration_base_change
BEFORE UPDATE OF base_commit ON run_workspaces
WHEN OLD.base_commit IS NOT NEW.base_commit AND EXISTS (
  SELECT 1 FROM integration_resolution_attempts resolution
  WHERE resolution.run_id = OLD.run_id
    AND resolution.resolved_at IS NOT NULL
)
BEGIN
  SELECT RAISE(ABORT, 'run workspace base is immutable after integration recovery confirmation');
END;`

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

CREATE TABLE IF NOT EXISTS coordination_incidents ` + coordinationIncidentTableDefinition + `;

CREATE TABLE IF NOT EXISTS coordination_proposals (
  id TEXT PRIMARY KEY,
  incident_id TEXT NOT NULL REFERENCES coordination_incidents(id) ON DELETE CASCADE,
  attempt_id TEXT UNIQUE REFERENCES coordination_attempts(id) ON DELETE CASCADE,
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

CREATE TABLE IF NOT EXISTS coordination_attempts (
  id TEXT PRIMARY KEY CHECK (length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
  incident_id TEXT NOT NULL REFERENCES coordination_incidents(id) ON DELETE CASCADE
    CHECK (length(CAST(incident_id AS BLOB)) BETWEEN 1 AND 128),
  board TEXT NOT NULL CHECK (length(CAST(board AS BLOB)) BETWEEN 1 AND 128),
  status TEXT NOT NULL CHECK (status IN ('started', 'succeeded', 'failed')),
  selected_agent TEXT NOT NULL DEFAULT '' CHECK (length(CAST(selected_agent AS BLOB)) <= 128),
  selected_runtime TEXT NOT NULL DEFAULT '' CHECK (selected_runtime IN ('', 'claude', 'codex', 'cline', 'gemini', 'manual')),
  selected_model TEXT NOT NULL DEFAULT '' CHECK (length(CAST(selected_model AS BLOB)) <= 256),
  selected_provider TEXT NOT NULL DEFAULT '' CHECK (length(CAST(selected_provider AS BLOB)) <= 128),
  selected_source TEXT NOT NULL DEFAULT '' CHECK (length(CAST(selected_source AS BLOB)) <= 128),
  error TEXT CHECK (error IS NULL OR length(CAST(error AS BLOB)) <= 4096),
  started_at TEXT NOT NULL,
  ended_at TEXT,
  CHECK (
    (status = 'started' AND ended_at IS NULL AND error IS NULL)
    OR
    (status = 'succeeded' AND ended_at IS NOT NULL AND error IS NULL)
    OR
    (status = 'failed' AND ended_at IS NOT NULL)
  )
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

-- A recovery checkpoint is intentionally separate from task_change_sets. It
-- preserves an unsuccessful run's partial Git result without completing the
-- task or satisfying any dependency. At most one checkpoint can be pending,
-- reserved, or adopted for a task.
CREATE TABLE IF NOT EXISTS recovery_checkpoints (
  id TEXT PRIMARY KEY CHECK (length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  source_run_id TEXT NOT NULL UNIQUE
    REFERENCES task_runs(id) ON DELETE NO ACTION DEFERRABLE INITIALLY DEFERRED,
  repository_path TEXT NOT NULL CHECK (length(CAST(repository_path AS BLOB)) BETWEEN 1 AND 4096),
  worktree_path TEXT NOT NULL CHECK (length(CAST(worktree_path AS BLOB)) BETWEEN 1 AND 4096),
  output_base_commit TEXT NOT NULL CHECK (length(CAST(output_base_commit AS BLOB)) BETWEEN 1 AND 128),
  start_commit TEXT NOT NULL CHECK (length(CAST(start_commit AS BLOB)) BETWEEN 1 AND 128),
  head_commit TEXT NOT NULL CHECK (length(CAST(head_commit AS BLOB)) BETWEEN 1 AND 128),
  durable_ref TEXT NOT NULL CHECK (
    length(CAST(durable_ref AS BLOB)) BETWEEN 1 AND 4096
    AND durable_ref = 'refs/autogora/checkpoints/' || source_run_id
  ),
  changed_files_json TEXT NOT NULL DEFAULT '[]'
    CHECK (
      length(CAST(changed_files_json AS BLOB)) <= 16777216
      AND json_valid(changed_files_json)
      AND CASE
        WHEN json_valid(changed_files_json) THEN json_type(changed_files_json) = 'array'
        ELSE 0
      END
    ),
  task_updated_at TEXT NOT NULL,
  task_spec_fingerprint TEXT NOT NULL
    CHECK (length(task_spec_fingerprint) = 64 AND task_spec_fingerprint NOT GLOB '*[^0-9a-f]*'),
  prerequisite_fingerprint TEXT NOT NULL
    CHECK (length(prerequisite_fingerprint) = 64 AND prerequisite_fingerprint NOT GLOB '*[^0-9a-f]*'),
  state TEXT NOT NULL CHECK (state IN ('pending', 'reserved', 'adopted', 'consumed', 'superseded')),
  reserved_run_id TEXT UNIQUE REFERENCES task_runs(id) ON DELETE RESTRICT,
  reservation_token TEXT UNIQUE,
  reserved_at TEXT,
  last_released_run_id TEXT REFERENCES task_runs(id) ON DELETE RESTRICT,
  last_release_token TEXT,
  last_released_at TEXT,
  adopted_output_base_commit TEXT CHECK (
    adopted_output_base_commit IS NULL OR length(CAST(adopted_output_base_commit AS BLOB)) BETWEEN 1 AND 128
  ),
  adopted_head_commit TEXT CHECK (
    adopted_head_commit IS NULL OR length(CAST(adopted_head_commit AS BLOB)) BETWEEN 1 AND 128
  ),
  adopted_at TEXT,
  consumed_at TEXT,
  superseded_at TEXT,
  superseded_by_id TEXT REFERENCES recovery_checkpoints(id) ON DELETE RESTRICT,
  supersede_reason TEXT CHECK (
    supersede_reason IS NULL OR length(CAST(supersede_reason AS BLOB)) BETWEEN 1 AND 2000
  ),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (source_run_id <> IFNULL(reserved_run_id, '')),
  CHECK (
    (last_released_run_id IS NULL AND last_release_token IS NULL AND last_released_at IS NULL)
    OR
    (last_released_run_id IS NOT NULL AND last_release_token IS NOT NULL
      AND last_release_token <> '' AND last_released_at IS NOT NULL)
  ),
  CHECK (
    (state = 'pending'
      AND reserved_run_id IS NULL AND reservation_token IS NULL AND reserved_at IS NULL
      AND adopted_output_base_commit IS NULL AND adopted_head_commit IS NULL AND adopted_at IS NULL
      AND consumed_at IS NULL AND superseded_at IS NULL
      AND superseded_by_id IS NULL AND supersede_reason IS NULL)
    OR
    (state = 'reserved'
      AND reserved_run_id IS NOT NULL AND reservation_token IS NOT NULL AND reservation_token <> ''
      AND reserved_at IS NOT NULL AND adopted_output_base_commit IS NULL
      AND adopted_head_commit IS NULL AND adopted_at IS NULL
      AND consumed_at IS NULL AND superseded_at IS NULL
      AND superseded_by_id IS NULL AND supersede_reason IS NULL)
    OR
    (state = 'adopted'
      AND reserved_run_id IS NOT NULL AND reservation_token IS NOT NULL AND reservation_token <> ''
      AND reserved_at IS NOT NULL AND adopted_output_base_commit IS NOT NULL
      AND adopted_head_commit IS NOT NULL AND adopted_at IS NOT NULL
      AND consumed_at IS NULL AND superseded_at IS NULL
      AND superseded_by_id IS NULL AND supersede_reason IS NULL)
    OR
    (state = 'consumed'
      AND reserved_run_id IS NOT NULL AND reservation_token IS NOT NULL AND reservation_token <> ''
      AND reserved_at IS NOT NULL AND adopted_output_base_commit IS NOT NULL
      AND adopted_head_commit IS NOT NULL AND adopted_at IS NOT NULL
      AND consumed_at IS NOT NULL AND superseded_at IS NULL
      AND superseded_by_id IS NULL AND supersede_reason IS NULL)
    OR
    (state = 'superseded'
      AND consumed_at IS NULL AND superseded_at IS NOT NULL AND supersede_reason IS NOT NULL
      AND (
        (reserved_run_id IS NULL AND reservation_token IS NULL AND reserved_at IS NULL
          AND adopted_output_base_commit IS NULL AND adopted_head_commit IS NULL AND adopted_at IS NULL)
        OR
        (reserved_run_id IS NOT NULL AND reservation_token IS NOT NULL AND reservation_token <> ''
          AND reserved_at IS NOT NULL AND adopted_output_base_commit IS NOT NULL
          AND adopted_head_commit IS NOT NULL AND adopted_at IS NOT NULL)
      ))
  )
);

-- Application code already updates only lifecycle columns. These triggers make
-- the immutable provenance and bounded transition graph durable invariants
-- even if a future caller issues a broader UPDATE.
CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_source_binding
BEFORE INSERT ON recovery_checkpoints
WHEN NOT EXISTS (
  SELECT 1
  FROM task_runs source
  JOIN tasks task ON task.id = source.task_id
  WHERE source.id = NEW.source_run_id
    AND source.task_id = NEW.task_id
    AND source.status = 'running'
    AND task.status = 'running'
    AND task.current_run_id = source.id
)
BEGIN
  SELECT RAISE(ABORT, 'recovery checkpoint source run must actively own its task');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_changed_files_integrity
BEFORE INSERT ON recovery_checkpoints
WHEN CASE
  WHEN json_valid(NEW.changed_files_json)
    AND json_type(NEW.changed_files_json) = 'array'
  THEN
    (SELECT COUNT(*) FROM json_each(NEW.changed_files_json)) > 10000
    OR EXISTS (
      SELECT 1
      FROM json_each(NEW.changed_files_json)
      WHERE type <> 'text'
        OR length(CAST(value AS BLOB)) NOT BETWEEN 1 AND 4096
        OR instr(value, char(0)) > 0
    )
  ELSE 0
END
BEGIN
  SELECT RAISE(ABORT, 'recovery checkpoint changed files must be bounded text paths');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_immutable_provenance
BEFORE UPDATE OF task_id, source_run_id, repository_path, worktree_path,
  output_base_commit, start_commit, head_commit, durable_ref,
  changed_files_json, task_updated_at, task_spec_fingerprint,
  prerequisite_fingerprint ON recovery_checkpoints
WHEN OLD.task_id IS NOT NEW.task_id
  OR OLD.source_run_id IS NOT NEW.source_run_id
  OR OLD.repository_path IS NOT NEW.repository_path
  OR OLD.worktree_path IS NOT NEW.worktree_path
  OR OLD.output_base_commit IS NOT NEW.output_base_commit
  OR OLD.start_commit IS NOT NEW.start_commit
  OR OLD.head_commit IS NOT NEW.head_commit
  OR OLD.durable_ref IS NOT NEW.durable_ref
  OR OLD.changed_files_json IS NOT NEW.changed_files_json
  OR OLD.task_updated_at IS NOT NEW.task_updated_at
  OR OLD.task_spec_fingerprint IS NOT NEW.task_spec_fingerprint
  OR OLD.prerequisite_fingerprint IS NOT NEW.prerequisite_fingerprint
BEGIN
  SELECT RAISE(ABORT, 'recovery checkpoint provenance is immutable');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_state_transition
BEFORE UPDATE OF state ON recovery_checkpoints
WHEN OLD.state <> NEW.state AND NOT (
  (OLD.state = 'pending' AND NEW.state IN ('reserved', 'superseded'))
  OR (OLD.state = 'reserved' AND NEW.state IN ('pending', 'adopted'))
  OR (OLD.state = 'adopted' AND NEW.state IN ('consumed', 'superseded'))
)
BEGIN
  SELECT RAISE(ABORT, 'invalid recovery checkpoint state transition');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_reservation_ownership
BEFORE UPDATE OF reserved_run_id, reservation_token, reserved_at ON recovery_checkpoints
WHEN NOT (
  (OLD.state = 'pending' AND NEW.state = 'reserved' AND EXISTS (
    SELECT 1
    FROM task_runs reservation
    JOIN tasks task ON task.id = reservation.task_id
    WHERE reservation.id = NEW.reserved_run_id
      AND reservation.task_id = OLD.task_id
      AND reservation.status = 'running'
      AND task.status = 'running'
      AND task.current_run_id = reservation.id
  ))
  OR (OLD.state = 'reserved' AND NEW.state = 'pending')
  OR (
    OLD.reserved_run_id IS NEW.reserved_run_id
    AND OLD.reservation_token IS NEW.reservation_token
    AND OLD.reserved_at IS NEW.reserved_at
  )
)
BEGIN
  SELECT RAISE(ABORT, 'recovery checkpoint reservation ownership is immutable within a state');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_release_provenance
BEFORE UPDATE OF last_released_run_id, last_release_token, last_released_at ON recovery_checkpoints
WHEN NOT (
  (OLD.state = 'pending' AND NEW.state = 'reserved'
    AND NEW.last_released_run_id IS NULL
    AND NEW.last_release_token IS NULL
    AND NEW.last_released_at IS NULL)
  OR
  (OLD.state = 'reserved' AND NEW.state = 'pending'
    AND NEW.last_released_run_id IS OLD.reserved_run_id
    AND NEW.last_release_token IS OLD.reservation_token
    AND NEW.last_released_at IS NOT NULL)
  OR (
    OLD.last_released_run_id IS NEW.last_released_run_id
    AND OLD.last_release_token IS NEW.last_release_token
    AND OLD.last_released_at IS NEW.last_released_at
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid recovery checkpoint release provenance');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_adoption_immutable
BEFORE UPDATE OF adopted_output_base_commit, adopted_head_commit, adopted_at ON recovery_checkpoints
WHEN OLD.adopted_head_commit IS NOT NULL
  AND (OLD.adopted_output_base_commit IS NOT NEW.adopted_output_base_commit
    OR OLD.adopted_head_commit IS NOT NEW.adopted_head_commit
    OR OLD.adopted_at IS NOT NEW.adopted_at)
BEGIN
  SELECT RAISE(ABORT, 'recovery checkpoint adoption is immutable');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_terminal_immutable
BEFORE UPDATE ON recovery_checkpoints
WHEN OLD.state IN ('consumed', 'superseded') AND (
  OLD.state IS NOT NEW.state
  OR OLD.reserved_run_id IS NOT NEW.reserved_run_id
  OR OLD.reservation_token IS NOT NEW.reservation_token
  OR OLD.reserved_at IS NOT NEW.reserved_at
  OR OLD.last_released_run_id IS NOT NEW.last_released_run_id
  OR OLD.last_release_token IS NOT NEW.last_release_token
  OR OLD.last_released_at IS NOT NEW.last_released_at
  OR OLD.adopted_output_base_commit IS NOT NEW.adopted_output_base_commit
  OR OLD.adopted_head_commit IS NOT NEW.adopted_head_commit
  OR OLD.adopted_at IS NOT NEW.adopted_at
  OR OLD.consumed_at IS NOT NEW.consumed_at
  OR OLD.superseded_at IS NOT NEW.superseded_at
  OR (OLD.superseded_by_id IS NOT NULL AND OLD.superseded_by_id IS NOT NEW.superseded_by_id)
  OR OLD.supersede_reason IS NOT NEW.supersede_reason
)
BEGIN
  SELECT RAISE(ABORT, 'terminal recovery checkpoint is immutable');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_superseded_by_integrity
BEFORE UPDATE OF superseded_by_id ON recovery_checkpoints
WHEN OLD.superseded_by_id IS NOT NEW.superseded_by_id AND NOT (
  OLD.state = 'superseded'
  AND NEW.state = 'superseded'
  AND OLD.superseded_by_id IS NULL
  AND NEW.superseded_by_id IS NOT NULL
  AND NEW.superseded_by_id <> OLD.id
  AND EXISTS (
    SELECT 1 FROM recovery_checkpoints replacement
    WHERE replacement.id = NEW.superseded_by_id
      AND replacement.task_id = OLD.task_id
      AND replacement.source_run_id = OLD.reserved_run_id
      AND replacement.repository_path = OLD.repository_path
      AND replacement.output_base_commit = OLD.adopted_output_base_commit
      AND replacement.start_commit = OLD.adopted_head_commit
      AND replacement.task_spec_fingerprint = OLD.task_spec_fingerprint
      AND replacement.prerequisite_fingerprint = OLD.prerequisite_fingerprint
      AND replacement.state = 'pending'
  )
)
BEGIN
  SELECT RAISE(ABORT, 'invalid recovery checkpoint replacement');
END;

-- An unadopted reservation contains no workspace mutation and can safely
-- return to pending when a generic crash/reclaim path terminalizes its run.
-- Once adoption changed the workspace, terminalization must instead consume a
-- successful result or atomically replace the checkpoint with cumulative work.
CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_require_adopted_resolution
BEFORE UPDATE OF status ON task_runs
WHEN OLD.status = 'running' AND NEW.status <> 'running' AND EXISTS (
  SELECT 1 FROM recovery_checkpoints
  WHERE reserved_run_id = OLD.id AND state = 'adopted'
)
BEGIN
  SELECT RAISE(ABORT, 'adopted recovery checkpoint must be consumed or superseded before terminalizing run');
END;

CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_release_unadopted_terminal_run
AFTER UPDATE OF status ON task_runs
WHEN OLD.status = 'running' AND NEW.status <> 'running' AND EXISTS (
  SELECT 1 FROM recovery_checkpoints
  WHERE reserved_run_id = OLD.id AND state = 'reserved'
)
BEGIN
  INSERT INTO task_events(task_id, run_id, kind, payload_json, created_at)
  SELECT task_id, OLD.id, 'recovery_checkpoint_released',
    json_object(
      'checkpointId', id,
      'reason', 'active run terminalized before checkpoint adoption',
      'automatic', 1
    ),
    COALESCE(NEW.ended_at, NEW.heartbeat_at, OLD.heartbeat_at)
  FROM recovery_checkpoints
  WHERE reserved_run_id = OLD.id AND state = 'reserved';

  UPDATE recovery_checkpoints
  SET state = 'pending',
    last_released_run_id = OLD.id,
    last_release_token = reservation_token,
    last_released_at = COALESCE(NEW.ended_at, NEW.heartbeat_at, OLD.heartbeat_at),
    reserved_run_id = NULL,
    reservation_token = NULL,
    reserved_at = NULL,
    updated_at = COALESCE(NEW.ended_at, NEW.heartbeat_at, OLD.heartbeat_at)
  WHERE reserved_run_id = OLD.id AND state = 'reserved';
END;

-- A task cannot become Done while partial work still needs a recovery
-- decision. FinalizeRunTerminal consumes an adopted checkpoint before updating
-- the task, so valid successful completion satisfies this invariant.
CREATE TRIGGER IF NOT EXISTS recovery_checkpoint_prevent_done_with_active
BEFORE UPDATE OF status ON tasks
WHEN NEW.status = 'done' AND EXISTS (
  SELECT 1 FROM recovery_checkpoints
  WHERE task_id = NEW.id AND state IN ('pending', 'reserved', 'adopted')
)
BEGIN
  SELECT RAISE(ABORT, 'task cannot be done with an active recovery checkpoint');
END;

-- Conflict-resolution preparations and started attempts are control-plane
-- safety state. They survive event retention so GC cannot reset retry bounds.
CREATE TABLE IF NOT EXISTS integration_resolution_attempts (
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  conflict_fingerprint TEXT NOT NULL
    CHECK (length(conflict_fingerprint) = 64 AND conflict_fingerprint NOT GLOB '*[^0-9a-f]*'),
  run_id TEXT NOT NULL UNIQUE REFERENCES task_runs(id) ON DELETE CASCADE,
  attempt INTEGER CHECK (attempt IS NULL OR attempt >= 1),
  max_attempts INTEGER NOT NULL CHECK (max_attempts >= 1),
  workspace_path TEXT NOT NULL,
  prerequisite_id TEXT NOT NULL,
  change_set_id TEXT NOT NULL,
  conflicting_files_json TEXT NOT NULL DEFAULT '[]',
  prepared_at TEXT NOT NULL,
  started_at TEXT,
  resolved_at TEXT,
  CHECK (
    (attempt IS NULL AND started_at IS NULL) OR
    (attempt IS NOT NULL AND started_at IS NOT NULL)
  ),
  CHECK (resolved_at IS NULL OR (attempt IS NOT NULL AND started_at IS NOT NULL)),
  CHECK (attempt IS NULL OR attempt <= max_attempts),
  PRIMARY KEY (task_id, conflict_fingerprint, run_id),
  UNIQUE (task_id, conflict_fingerprint, attempt)
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

-- Availability checks can finish out of order. Reserve a generation before
-- each check and only apply observations newer than the last applied result.
-- Keeping reservation and application counters separate means an abandoned
-- check does not prevent an older in-flight check from reporting its result.
CREATE TABLE IF NOT EXISTS agent_health_observation_sequences (
  agent_id TEXT PRIMARY KEY,
  next_generation INTEGER NOT NULL DEFAULT 0 CHECK (next_generation >= 0),
  applied_generation INTEGER NOT NULL DEFAULT 0 CHECK (applied_generation >= 0),
  CHECK (applied_generation <= next_generation)
);

-- Auto-planning is a bounded scheduler operation, not part of an interactive
-- Specify or Decompose request. Persist its claim and retry budget so process
-- restarts and concurrent one-shot dispatchers cannot duplicate model calls.
CREATE TABLE IF NOT EXISTS auto_decompose_state (
  task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  task_updated_at TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 32),
  max_attempts INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 32),
  next_attempt_at TEXT,
  claim_token TEXT,
  claim_expires_at TEXT,
  last_error TEXT CHECK (last_error IS NULL OR length(CAST(last_error AS BLOB)) <= 2000),
  updated_at TEXT NOT NULL,
  CHECK (
    (claim_token IS NULL AND claim_expires_at IS NULL)
    OR
    (claim_token IS NOT NULL AND claim_token <> '' AND claim_expires_at IS NOT NULL)
  )
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

CREATE TABLE IF NOT EXISTS automation_quarantine_gate (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
  generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  permit_token TEXT NOT NULL CHECK (permit_token <> ''),
  activated_at TEXT,
  cleared_at TEXT,
  confirmation_started_at TEXT,
  confirmation_actor TEXT,
  confirmation_reason TEXT,
  confirmation_helpers_stopped INTEGER NOT NULL DEFAULT 0
    CHECK (confirmation_helpers_stopped IN (0, 1)),
  confirmation_external_writes_stopped INTEGER NOT NULL DEFAULT 0
    CHECK (confirmation_external_writes_stopped IN (0, 1)),
  CHECK (
    (confirmation_started_at IS NULL
      AND confirmation_actor IS NULL
      AND confirmation_reason IS NULL
      AND confirmation_helpers_stopped = 0
      AND confirmation_external_writes_stopped = 0)
    OR
    (confirmation_started_at IS NOT NULL
      AND confirmation_actor IS NOT NULL
      AND confirmation_reason IS NOT NULL
      AND confirmation_helpers_stopped = 1
      AND confirmation_external_writes_stopped = 1)
  )
);

INSERT OR IGNORE INTO automation_quarantine_gate(
  singleton, active, generation, permit_token
) VALUES (1, 0, 0, lower(hex(randomblob(32))));

CREATE TABLE IF NOT EXISTS automation_quarantine_sources (
  source_key TEXT PRIMARY KEY CHECK (length(source_key) = 64),
  generation INTEGER NOT NULL CHECK (generation >= 1),
  board TEXT NOT NULL CHECK (length(CAST(board AS BLOB)) BETWEEN 1 AND 128),
  kind TEXT NOT NULL CHECK (length(CAST(kind AS BLOB)) BETWEEN 1 AND 64),
  source_id TEXT NOT NULL CHECK (length(CAST(source_id AS BLOB)) BETWEEN 1 AND 256),
  observed_updated_at TEXT NOT NULL DEFAULT ''
    CHECK (length(CAST(observed_updated_at AS BLOB)) <= 128),
  observed_claim_epoch TEXT NOT NULL DEFAULT ''
    CHECK (length(CAST(observed_claim_epoch AS BLOB)) <= 128),
  diagnostic_code TEXT NOT NULL
    CHECK (length(CAST(diagnostic_code AS BLOB)) BETWEEN 1 AND 128),
  disposition TEXT NOT NULL DEFAULT 'active'
    CHECK (disposition IN ('active', 'superseded', 'abandoned')),
  observed_at TEXT NOT NULL,
  resolved_at TEXT,
  resolved_by TEXT,
  resolution_reason TEXT,
  resolved_generation INTEGER,
  CHECK (observed_updated_at <> '' OR observed_claim_epoch <> ''),
  CHECK (
    (disposition = 'active'
      AND resolved_at IS NULL
      AND resolved_by IS NULL
      AND resolution_reason IS NULL
      AND resolved_generation IS NULL)
    OR
    (disposition <> 'active'
      AND resolved_at IS NOT NULL
      AND resolved_by IS NOT NULL
      AND resolution_reason IS NOT NULL
      AND resolved_generation IS NOT NULL)
  )
);

CREATE TABLE IF NOT EXISTS automation_dispatcher_sessions (
  session_id TEXT PRIMARY KEY
    CHECK (length(CAST(session_id AS BLOB)) BETWEEN 1 AND 256),
  board TEXT NOT NULL CHECK (length(CAST(board AS BLOB)) BETWEEN 1 AND 128),
  lease_token TEXT NOT NULL CHECK (lease_token <> ''),
  registered_at TEXT NOT NULL,
  renewed_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  released_at TEXT
);

CREATE TABLE IF NOT EXISTS automation_dispatcher_acks (
  session_id TEXT NOT NULL REFERENCES automation_dispatcher_sessions(session_id)
    ON DELETE CASCADE,
  generation INTEGER NOT NULL CHECK (generation >= 1),
  acknowledged_at TEXT NOT NULL,
  PRIMARY KEY(session_id, generation)
);

-- Agent slots coordinate concurrency across every board. Run IDs belong to
-- board-local databases, so this table intentionally has no foreign keys.
CREATE TABLE IF NOT EXISTS global_agent_slots (
  agent_id TEXT NOT NULL,
  slot INTEGER NOT NULL CHECK (slot >= 1),
  owner_kind TEXT NOT NULL CHECK (owner_kind IN ('worker', 'planner', 'coordinator', 'judge')),
  board TEXT NOT NULL,
  run_id TEXT,
  owner_id TEXT NOT NULL UNIQUE,
  lease_token TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  expires_at TEXT,
  PRIMARY KEY (agent_id, slot),
  CHECK (
    (owner_kind = 'worker' AND run_id IS NOT NULL AND expires_at IS NULL) OR
    (owner_kind IN ('planner', 'coordinator', 'judge') AND expires_at IS NOT NULL)
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
  outcome TEXT NOT NULL CHECK (outcome IN ('reclaimed', 'timed_out', 'crashed')),
  count_failure INTEGER NOT NULL CHECK (count_failure IN (0, 1)),
  requested_at TEXT NOT NULL,
  requires_operator INTEGER NOT NULL DEFAULT 0 CHECK (requires_operator IN (0, 1)),
  diagnostic_code TEXT,
  fence_token TEXT NOT NULL,
  fence_generation INTEGER NOT NULL DEFAULT 1 CHECK (fence_generation >= 1),
  host_acknowledged_at TEXT,
  host_acknowledged_fence_token TEXT,
  recovery_owner_token TEXT,
  recovery_owner_expires_at TEXT,
  operator_quiesced_at TEXT,
  operator_quiesced_by TEXT,
  operator_quiescence_reason TEXT,
  operator_quiesced_generation INTEGER,
  operator_confirmed_worker_stopped INTEGER NOT NULL DEFAULT 0
    CHECK (operator_confirmed_worker_stopped IN (0, 1)),
  operator_confirmed_host_writes_stopped INTEGER NOT NULL DEFAULT 0
    CHECK (operator_confirmed_host_writes_stopped IN (0, 1)),
  operator_observed_heartbeat_at TEXT,
  operator_observed_claim_expires_at TEXT,
  operator_observed_pid INTEGER,
  operator_observed_process_identity TEXT,
  CHECK (
    (recovery_owner_token IS NULL AND recovery_owner_expires_at IS NULL) OR
    (recovery_owner_token IS NOT NULL AND recovery_owner_expires_at IS NOT NULL)
  ),
  CHECK (
    (host_acknowledged_at IS NULL AND host_acknowledged_fence_token IS NULL) OR
    (host_acknowledged_at IS NOT NULL AND host_acknowledged_fence_token IS NOT NULL)
  ),
  CHECK (
    (operator_quiesced_at IS NULL
      AND operator_quiesced_by IS NULL
      AND operator_quiescence_reason IS NULL
      AND operator_quiesced_generation IS NULL
      AND operator_confirmed_worker_stopped = 0
      AND operator_confirmed_host_writes_stopped = 0
      AND operator_observed_heartbeat_at IS NULL
      AND operator_observed_claim_expires_at IS NULL
      AND operator_observed_pid IS NULL
      AND operator_observed_process_identity IS NULL)
    OR
    (operator_quiesced_at IS NOT NULL
      AND operator_quiesced_by IS NOT NULL
      AND operator_quiescence_reason IS NOT NULL
      AND operator_quiesced_generation = fence_generation
      AND operator_confirmed_worker_stopped = 1
      AND operator_confirmed_host_writes_stopped = 1
      AND operator_observed_heartbeat_at IS NOT NULL
      AND operator_observed_claim_expires_at IS NOT NULL)
  )
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

CREATE TABLE IF NOT EXISTS publications (
  id TEXT PRIMARY KEY CHECK (length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
  board TEXT NOT NULL CHECK (length(CAST(board AS BLOB)) BETWEEN 1 AND 128),
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
  change_set_id TEXT NOT NULL UNIQUE REFERENCES task_change_sets(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK (status IN (
    'pending', 'awaiting_approval', 'publishing', 'published',
    'no_change', 'failed', 'superseded'
  )),
  mode TEXT NOT NULL CHECK (mode IN ('manual', 'local_ff', 'pull_request')),
  target_branch TEXT NOT NULL,
  remote TEXT NOT NULL,
  require_approval INTEGER NOT NULL CHECK (require_approval IN (0, 1)),
  repository_path TEXT NOT NULL,
  worktree_path TEXT NOT NULL,
  base_commit TEXT NOT NULL,
  head_commit TEXT NOT NULL,
  durable_ref TEXT NOT NULL,
  policy_snapshot_json TEXT NOT NULL,
  source_snapshot_json TEXT NOT NULL,
  url TEXT,
  error TEXT,
  claim_epoch INTEGER NOT NULL DEFAULT 0
    CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 0),
  claim_token TEXT,
  claim_expires_at TEXT,
  approved_at TEXT,
  published_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (
    (status = 'publishing' AND claim_token IS NOT NULL AND claim_token <> '' AND claim_expires_at IS NOT NULL)
    OR
    (status <> 'publishing' AND claim_token IS NULL AND claim_expires_at IS NULL)
  ),
  CHECK (status <> 'failed' OR (error IS NOT NULL AND error <> '')),
  CHECK (status <> 'superseded' OR (error IS NOT NULL AND error <> '')),
  CHECK (status <> 'published' OR published_at IS NOT NULL),
  CHECK (status <> 'no_change' OR published_at IS NOT NULL)
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
CREATE INDEX IF NOT EXISTS idx_recovery_checkpoints_task
  ON recovery_checkpoints(task_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_recovery_checkpoints_active_task
  ON recovery_checkpoints(task_id)
  WHERE state IN ('pending', 'reserved', 'adopted');
CREATE INDEX IF NOT EXISTS idx_recovery_checkpoints_reserved_run
  ON recovery_checkpoints(reserved_run_id)
  WHERE reserved_run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_integration_resolution_attempts_task
  ON integration_resolution_attempts(task_id, conflict_fingerprint, attempt DESC);
CREATE INDEX IF NOT EXISTS idx_run_agent_configs_task ON run_agent_configs(task_id, configured_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_health_due ON agent_health(status, cooldown_until);
CREATE INDEX IF NOT EXISTS idx_auto_decompose_due ON auto_decompose_state(next_attempt_at, claim_expires_at);
CREATE INDEX IF NOT EXISTS idx_service_leases_expiry ON service_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_resource_leases_run ON resource_leases(run_id);
CREATE INDEX IF NOT EXISTS idx_global_workspace_leases_owner ON global_workspace_leases(board, run_id);
CREATE INDEX IF NOT EXISTS idx_automation_quarantine_sources_identity
  ON automation_quarantine_sources(board, kind, source_id, generation DESC);
CREATE INDEX IF NOT EXISTS idx_automation_quarantine_sources_active
  ON automation_quarantine_sources(disposition, generation DESC);
CREATE INDEX IF NOT EXISTS idx_automation_dispatcher_sessions_expiry
  ON automation_dispatcher_sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_global_agent_slots_expiry ON global_agent_slots(owner_kind, expires_at);
CREATE INDEX IF NOT EXISTS idx_terminal_requests_pending ON run_terminal_requests(finalized_at, requested_at);
CREATE INDEX IF NOT EXISTS idx_change_sets_task ON task_change_sets(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_publications_board_status ON publications(board, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_publications_task ON publications(board, task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_publications_run ON publications(board, run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_publications_claim_due ON publications(board, status, claim_expires_at);
CREATE INDEX IF NOT EXISTS idx_task_hierarchy_parent ON task_hierarchy(parent_id, position, child_id);
` + coordinationIncidentIndexes + `
CREATE INDEX IF NOT EXISTS idx_coordination_proposals_incident ON coordination_proposals(incident_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_attempts_board_started ON coordination_attempts(board, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_coordination_attempts_incident ON coordination_attempts(incident_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_attachments_task ON task_attachments(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_subscriptions_task ON notification_subscriptions(task_id, platform, chat_id);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due ON notification_deliveries(status, next_attempt_at, lease_expires_at);
`
