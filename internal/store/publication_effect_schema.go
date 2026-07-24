package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const publicationEffectSchema = `
CREATE TABLE IF NOT EXISTS publication_effect_intents (
  id TEXT PRIMARY KEY
    CHECK (
      typeof(id) = 'text'
      AND length(CAST(id AS BLOB)) BETWEEN 1 AND 128
      AND instr(id, char(0)) = 0
    ),
  attempt_id TEXT NOT NULL
    CHECK (
      typeof(attempt_id) = 'text'
      AND length(CAST(attempt_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(attempt_id, char(0)) = 0
    ),
  board TEXT NOT NULL
    CHECK (
      typeof(board) = 'text'
      AND length(CAST(board AS BLOB)) BETWEEN 1 AND 128
      AND instr(board, char(0)) = 0
    ),
  publication_id TEXT NOT NULL
    CHECK (
      typeof(publication_id) = 'text'
      AND length(CAST(publication_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(publication_id, char(0)) = 0
    ),
  claim_epoch INTEGER NOT NULL
    CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 1),
  sequence INTEGER NOT NULL
    CHECK (
      typeof(sequence) = 'integer'
      AND sequence BETWEEN 1 AND 64
    ),
  kind TEXT NOT NULL
    CHECK (
      kind IN (
        'local_ref_cas',
        'local_worktree_ff',
        'pr_branch_push',
        'pr_create'
      )
    ),
  descriptor_version INTEGER NOT NULL
    CHECK (
      typeof(descriptor_version) = 'integer'
      AND descriptor_version BETWEEN 1 AND 16
    ),
  descriptor_json TEXT NOT NULL
    CHECK (
      typeof(descriptor_json) = 'text'
      AND length(CAST(descriptor_json AS BLOB)) BETWEEN 2 AND 16384
      AND json_valid(descriptor_json)
      AND CASE
        WHEN json_valid(descriptor_json)
        THEN json_type(descriptor_json) = 'object'
        ELSE 0
      END
    ),
  descriptor_fingerprint TEXT NOT NULL
    CHECK (
      typeof(descriptor_fingerprint) = 'text'
      AND length(CAST(descriptor_fingerprint AS BLOB)) = 64
      AND descriptor_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  identity_fingerprint TEXT NOT NULL UNIQUE
    CHECK (
      typeof(identity_fingerprint) = 'text'
      AND length(CAST(identity_fingerprint AS BLOB)) = 64
      AND identity_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  parent_effect_fingerprint TEXT NOT NULL
    CHECK (
      typeof(parent_effect_fingerprint) = 'text'
      AND length(CAST(parent_effect_fingerprint AS BLOB)) = 64
      AND parent_effect_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  parent_provenance_fingerprint TEXT NOT NULL
    CHECK (
      typeof(parent_provenance_fingerprint) = 'text'
      AND length(CAST(parent_provenance_fingerprint AS BLOB)) = 64
      AND parent_provenance_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  prepared_at TEXT NOT NULL
    CHECK (
      typeof(prepared_at) = 'text'
      AND length(CAST(prepared_at AS BLOB)) = 30
      AND instr(prepared_at, char(0)) = 0
      AND prepared_at GLOB
        '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
    ),
  UNIQUE(attempt_id, sequence),
  FOREIGN KEY(attempt_id)
    REFERENCES publication_attempt_intents(id)
    ON DELETE RESTRICT
);

CREATE TRIGGER IF NOT EXISTS publication_effect_intents_parent_guard
BEFORE INSERT ON publication_effect_intents
WHEN NOT EXISTS (
  SELECT 1 FROM publication_attempt_intents i
  WHERE i.id = NEW.attempt_id
    AND i.board = NEW.board
    AND i.publication_id = NEW.publication_id
    AND i.claim_epoch = NEW.claim_epoch
    AND i.effect_fingerprint = NEW.parent_effect_fingerprint
    AND i.execution_provenance_fingerprint =
      NEW.parent_provenance_fingerprint
    AND NOT EXISTS (
      SELECT 1 FROM publication_attempt_results r
      WHERE r.attempt_id = i.id
    )
)
BEGIN
  SELECT RAISE(
    ABORT,
    'publication effect intent does not match its parent attempt'
  );
END;

CREATE TRIGGER IF NOT EXISTS publication_effect_intents_prevent_update
BEFORE UPDATE ON publication_effect_intents
BEGIN
  SELECT RAISE(ABORT, 'publication effect intents are immutable');
END;

CREATE TRIGGER IF NOT EXISTS publication_effect_intents_prevent_delete
BEFORE DELETE ON publication_effect_intents
BEGIN
  SELECT RAISE(ABORT, 'publication effect intents are immutable');
END;

CREATE TABLE IF NOT EXISTS publication_effect_results (
  effect_id TEXT PRIMARY KEY
    CHECK (
      typeof(effect_id) = 'text'
      AND length(CAST(effect_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(effect_id, char(0)) = 0
    ),
  attempt_id TEXT NOT NULL
    CHECK (
      typeof(attempt_id) = 'text'
      AND length(CAST(attempt_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(attempt_id, char(0)) = 0
    ),
  board TEXT NOT NULL
    CHECK (
      typeof(board) = 'text'
      AND length(CAST(board AS BLOB)) BETWEEN 1 AND 128
      AND instr(board, char(0)) = 0
    ),
  publication_id TEXT NOT NULL
    CHECK (
      typeof(publication_id) = 'text'
      AND length(CAST(publication_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(publication_id, char(0)) = 0
    ),
  claim_epoch INTEGER NOT NULL
    CHECK (typeof(claim_epoch) = 'integer' AND claim_epoch >= 1),
  sequence INTEGER NOT NULL
    CHECK (
      typeof(sequence) = 'integer'
      AND sequence BETWEEN 1 AND 64
    ),
  identity_fingerprint TEXT NOT NULL
    CHECK (
      typeof(identity_fingerprint) = 'text'
      AND length(CAST(identity_fingerprint AS BLOB)) = 64
      AND identity_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  outcome TEXT NOT NULL
    CHECK (outcome IN ('applied', 'not_applied', 'unknown')),
  evidence_json TEXT NOT NULL
    CHECK (
      typeof(evidence_json) = 'text'
      AND length(CAST(evidence_json AS BLOB)) BETWEEN 2 AND 32768
      AND json_valid(evidence_json)
      AND CASE
        WHEN json_valid(evidence_json)
        THEN json_type(evidence_json) = 'object'
        ELSE 0
      END
    ),
  evidence_fingerprint TEXT NOT NULL
    CHECK (
      typeof(evidence_fingerprint) = 'text'
      AND length(CAST(evidence_fingerprint AS BLOB)) = 64
      AND evidence_fingerprint NOT GLOB '*[^0-9a-f]*'
    ),
  error_kind TEXT
    CHECK (
      error_kind IS NULL
      OR (
        typeof(error_kind) = 'text'
        AND length(CAST(error_kind AS BLOB)) BETWEEN 1 AND 64
        AND error_kind NOT GLOB '*[^a-z0-9_]*'
      )
    ),
  error_detail_fingerprint TEXT
    CHECK (
      error_detail_fingerprint IS NULL
      OR (
        typeof(error_detail_fingerprint) = 'text'
        AND length(CAST(error_detail_fingerprint AS BLOB)) = 64
        AND error_detail_fingerprint NOT GLOB '*[^0-9a-f]*'
      )
    ),
  recorded_at TEXT NOT NULL
    CHECK (
      typeof(recorded_at) = 'text'
      AND length(CAST(recorded_at AS BLOB)) = 30
      AND instr(recorded_at, char(0)) = 0
      AND recorded_at GLOB
        '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
    ),
  CHECK (
    (outcome = 'applied'
      AND error_kind IS NULL
      AND error_detail_fingerprint IS NULL)
    OR
    (outcome = 'not_applied'
      AND (
        (error_kind IS NULL AND error_detail_fingerprint IS NULL)
        OR (
          error_kind IS NOT NULL
          AND error_detail_fingerprint IS NOT NULL
        )
      ))
    OR
    (outcome = 'unknown'
      AND error_kind IS NOT NULL
      AND error_detail_fingerprint IS NOT NULL)
  ),
  FOREIGN KEY(effect_id)
    REFERENCES publication_effect_intents(id)
    ON DELETE RESTRICT,
  FOREIGN KEY(attempt_id)
    REFERENCES publication_attempt_intents(id)
    ON DELETE RESTRICT
);

CREATE TRIGGER IF NOT EXISTS publication_effect_results_identity_guard
BEFORE INSERT ON publication_effect_results
WHEN NOT EXISTS (
  SELECT 1 FROM publication_effect_intents i
  WHERE i.id = NEW.effect_id
    AND i.attempt_id = NEW.attempt_id
    AND i.board = NEW.board
    AND i.publication_id = NEW.publication_id
    AND i.claim_epoch = NEW.claim_epoch
    AND i.sequence = NEW.sequence
    AND i.identity_fingerprint = NEW.identity_fingerprint
)
BEGIN
  SELECT RAISE(
    ABORT,
    'publication effect result does not match its intent'
  );
END;

CREATE TRIGGER IF NOT EXISTS publication_effect_results_prevent_update
BEFORE UPDATE ON publication_effect_results
BEGIN
  SELECT RAISE(ABORT, 'publication effect results are immutable');
END;

CREATE TRIGGER IF NOT EXISTS publication_effect_results_prevent_delete
BEFORE DELETE ON publication_effect_results
BEGIN
  SELECT RAISE(ABORT, 'publication effect results are immutable');
END;

CREATE TRIGGER IF NOT EXISTS publication_attempt_results_require_resolved_effects
BEFORE INSERT ON publication_attempt_results
WHEN EXISTS (
  SELECT 1 FROM publication_effect_intents e
  LEFT JOIN publication_effect_results r ON r.effect_id = e.id
  WHERE e.attempt_id = NEW.attempt_id
    AND r.effect_id IS NULL
)
BEGIN
  SELECT RAISE(
    ABORT,
    'publication attempt has unresolved command effects'
  );
END;

CREATE TRIGGER IF NOT EXISTS publication_attempt_results_require_unknown_effect_outcome
BEFORE INSERT ON publication_attempt_results
WHEN NEW.outcome <> 'unknown'
  AND EXISTS (
    SELECT 1 FROM publication_effect_intents e
    INNER JOIN publication_effect_results r ON r.effect_id = e.id
    WHERE e.attempt_id = NEW.attempt_id
      AND r.outcome = 'unknown'
  )
BEGIN
  SELECT RAISE(
    ABORT,
    'publication attempt with an unknown command effect requires an unknown result'
  );
END;

CREATE INDEX IF NOT EXISTS idx_publication_effect_intents_attempt_sequence
  ON publication_effect_intents(attempt_id, sequence);
CREATE INDEX IF NOT EXISTS idx_publication_effect_intents_board_prepared
  ON publication_effect_intents(board, prepared_at, id);
`

type publicationEffectColumnContract struct {
	name       string
	columnType string
	notNull    int
	primaryKey int
}

var publicationEffectTableContracts = map[string][]publicationEffectColumnContract{
	"publication_effect_intents": {
		{"id", "TEXT", 0, 1},
		{"attempt_id", "TEXT", 1, 0},
		{"board", "TEXT", 1, 0},
		{"publication_id", "TEXT", 1, 0},
		{"claim_epoch", "INTEGER", 1, 0},
		{"sequence", "INTEGER", 1, 0},
		{"kind", "TEXT", 1, 0},
		{"descriptor_version", "INTEGER", 1, 0},
		{"descriptor_json", "TEXT", 1, 0},
		{"descriptor_fingerprint", "TEXT", 1, 0},
		{"identity_fingerprint", "TEXT", 1, 0},
		{"parent_effect_fingerprint", "TEXT", 1, 0},
		{"parent_provenance_fingerprint", "TEXT", 1, 0},
		{"prepared_at", "TEXT", 1, 0},
	},
	"publication_effect_results": {
		{"effect_id", "TEXT", 0, 1},
		{"attempt_id", "TEXT", 1, 0},
		{"board", "TEXT", 1, 0},
		{"publication_id", "TEXT", 1, 0},
		{"claim_epoch", "INTEGER", 1, 0},
		{"sequence", "INTEGER", 1, 0},
		{"identity_fingerprint", "TEXT", 1, 0},
		{"outcome", "TEXT", 1, 0},
		{"evidence_json", "TEXT", 1, 0},
		{"evidence_fingerprint", "TEXT", 1, 0},
		{"error_kind", "TEXT", 0, 0},
		{"error_detail_fingerprint", "TEXT", 0, 0},
		{"recorded_at", "TEXT", 1, 0},
	},
}

var publicationEffectObjectContracts = map[string]struct {
	objectType string
	table      string
}{
	"publication_effect_intents_parent_guard": {
		objectType: "trigger",
		table:      "publication_effect_intents",
	},
	"publication_effect_intents_prevent_update": {
		objectType: "trigger",
		table:      "publication_effect_intents",
	},
	"publication_effect_intents_prevent_delete": {
		objectType: "trigger",
		table:      "publication_effect_intents",
	},
	"publication_effect_results_identity_guard": {
		objectType: "trigger",
		table:      "publication_effect_results",
	},
	"publication_effect_results_prevent_update": {
		objectType: "trigger",
		table:      "publication_effect_results",
	},
	"publication_effect_results_prevent_delete": {
		objectType: "trigger",
		table:      "publication_effect_results",
	},
	"publication_attempt_results_require_resolved_effects": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"publication_attempt_results_require_unknown_effect_outcome": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"idx_publication_effect_intents_attempt_sequence": {
		objectType: "index",
		table:      "publication_effect_intents",
	},
	"idx_publication_effect_intents_board_prepared": {
		objectType: "index",
		table:      "publication_effect_intents",
	},
}

var publicationEffectTriggerNameContracts = map[string][]string{
	"publication_effect_intents": {
		"publication_effect_intents_parent_guard",
		"publication_effect_intents_prevent_delete",
		"publication_effect_intents_prevent_update",
	},
	"publication_effect_results": {
		"publication_effect_results_identity_guard",
		"publication_effect_results_prevent_delete",
		"publication_effect_results_prevent_update",
	},
	"publication_attempt_results": {
		"publication_attempt_results_identity_guard",
		"publication_attempt_results_prevent_delete",
		"publication_attempt_results_prevent_update",
		"publication_attempt_results_require_resolved_effects",
		"publication_attempt_results_require_unknown_effect_outcome",
		"publication_attempt_results_require_v30_evidence",
	},
}

// SQLite creates sql-less autoindexes for PRIMARY KEY and UNIQUE constraints.
// The exact table DDL above owns those constraints. This allowlist covers every
// explicit index attached to an effect table.
var publicationEffectExplicitIndexNameContracts = map[string][]string{
	"publication_effect_intents": {
		"idx_publication_effect_intents_attempt_sequence",
		"idx_publication_effect_intents_board_prepared",
	},
	"publication_effect_results": {},
}

type publicationEffectForeignKeyContract struct {
	parent   string
	from     string
	to       string
	onUpdate string
	onDelete string
	match    string
}

var publicationEffectForeignKeyContracts = map[string][]publicationEffectForeignKeyContract{
	"publication_effect_intents": {
		{
			parent:   "publication_attempt_intents",
			from:     "attempt_id",
			to:       "id",
			onUpdate: "NO ACTION",
			onDelete: "RESTRICT",
			match:    "NONE",
		},
	},
	"publication_effect_results": {
		{
			parent:   "publication_effect_intents",
			from:     "effect_id",
			to:       "id",
			onUpdate: "NO ACTION",
			onDelete: "RESTRICT",
			match:    "NONE",
		},
		{
			parent:   "publication_attempt_intents",
			from:     "attempt_id",
			to:       "id",
			onUpdate: "NO ACTION",
			onDelete: "RESTRICT",
			match:    "NONE",
		},
	},
}

func publicationEffectSQLWordByte(value byte) bool {
	return (value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9') ||
		value == '_' || value == '$'
}

func publicationEffectSQLSpaceByte(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func publicationEffectQuotedSQLToken(value string, start int) (string, int) {
	quote := value[start]
	endQuote := quote
	if quote == '[' {
		endQuote = ']'
	}
	index := start + 1
	for index < len(value) {
		if value[index] != endQuote {
			index++
			continue
		}
		if index+1 < len(value) && value[index+1] == endQuote {
			index += 2
			continue
		}
		index++
		return value[start:index], index
	}
	// sqlite_master only contains parseable SQL. Retaining an unterminated
	// token still makes a malformed source definition compare unequal.
	return value[start:], len(value)
}

func appendPublicationEffectSQLToken(
	result *strings.Builder,
	token string,
) {
	result.WriteString(strconv.Itoa(len(token)))
	result.WriteByte(':')
	result.WriteString(token)
}

// compactPublicationEffectSQL ignores whitespace and the case of unquoted SQL
// words. Quoted strings, blobs, and identifiers remain byte-for-byte exact:
// lowercasing the whole statement would otherwise turn the distinct literals
// 'unknown' and 'UNKNOWN' into the same schema contract.
func compactPublicationEffectSQL(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		if publicationEffectSQLSpaceByte(value[index]) {
			index++
			continue
		}
		switch value[index] {
		case '\'', '"', '`', '[':
			token, next := publicationEffectQuotedSQLToken(value, index)
			appendPublicationEffectSQLToken(&result, token)
			index = next
			continue
		}
		if publicationEffectSQLWordByte(value[index]) {
			start := index
			for index < len(value) &&
				publicationEffectSQLWordByte(value[index]) {
				index++
			}
			appendPublicationEffectSQLToken(
				&result,
				strings.ToLower(value[start:index]),
			)
			continue
		}
		appendPublicationEffectSQLToken(
			&result,
			value[index:index+1],
		)
		index++
	}
	return result.String()
}

func expectedPublicationEffectObjectSQL(
	objectType string,
	name string,
) (string, error) {
	marker := "CREATE " + strings.ToUpper(objectType) +
		" IF NOT EXISTS " + name
	start := strings.Index(publicationEffectSchema, marker)
	if start < 0 {
		return "", fmt.Errorf(
			"publication effect schema source lacks %s %s",
			objectType,
			name,
		)
	}
	remainder := publicationEffectSchema[start:]
	var terminator string
	switch objectType {
	case "table":
		terminator = "\n);"
	case "trigger":
		terminator = "\nEND;"
	case "index":
		terminator = ";"
	default:
		return "", fmt.Errorf(
			"unsupported publication effect object type %s",
			objectType,
		)
	}
	end := strings.Index(remainder, terminator)
	if end < 0 {
		return "", fmt.Errorf(
			"publication effect schema source has incomplete %s %s",
			objectType,
			name,
		)
	}
	statement := remainder[:end+len(terminator)]
	statement = strings.TrimSuffix(statement, ";")
	statement = strings.Replace(
		statement,
		" IF NOT EXISTS ",
		" ",
		1,
	)
	return compactPublicationEffectSQL(statement), nil
}

func publicationEffectForeignKeys(
	ctx context.Context,
	q querier,
	table string,
) ([]publicationEffectForeignKeyContract, error) {
	rows, err := q.QueryContext(
		ctx,
		"PRAGMA foreign_key_list("+table+")",
	)
	if err != nil {
		return nil, fmt.Errorf("inspect %s foreign keys: %w", table, err)
	}
	defer rows.Close()
	values := make([]publicationEffectForeignKeyContract, 0, 2)
	for rows.Next() {
		var id, sequence int
		var value publicationEffectForeignKeyContract
		if err := rows.Scan(
			&id,
			&sequence,
			&value.parent,
			&value.from,
			&value.to,
			&value.onUpdate,
			&value.onDelete,
			&value.match,
		); err != nil {
			return nil, fmt.Errorf(
				"scan %s foreign keys: %w",
				table,
				err,
			)
		}
		if sequence != 0 {
			return nil, fmt.Errorf(
				"incompatible %s schema: composite foreign key",
				table,
			)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect %s foreign keys: %w", table, err)
	}
	return values, nil
}

func samePublicationEffectForeignKeySet(
	actual []publicationEffectForeignKeyContract,
	expected []publicationEffectForeignKeyContract,
) bool {
	if len(actual) != len(expected) {
		return false
	}
	remaining := append(
		[]publicationEffectForeignKeyContract(nil),
		expected...,
	)
	for _, candidate := range actual {
		found := -1
		for index, expectedCandidate := range remaining {
			if candidate == expectedCandidate {
				found = index
				break
			}
		}
		if found < 0 {
			return false
		}
		remaining = append(remaining[:found], remaining[found+1:]...)
	}
	return len(remaining) == 0
}

func publicationEffectSchemaObjectNames(
	ctx context.Context,
	q querier,
	objectType string,
	table string,
	explicitIndexesOnly bool,
) ([]string, error) {
	query := `
		SELECT name
		FROM sqlite_master
		WHERE type = ? AND tbl_name = ?
	`
	if explicitIndexesOnly {
		query += " AND sql IS NOT NULL"
	}
	query += " ORDER BY name"
	rows, err := q.QueryContext(ctx, query, objectType, table)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect %s set for %s: %w",
			objectType,
			table,
			err,
		)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf(
				"scan %s set for %s: %w",
				objectType,
				table,
				err,
			)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"inspect %s set for %s: %w",
			objectType,
			table,
			err,
		)
	}
	return names, nil
}

func validatePublicationEffectSchemaObjectNameSets(
	ctx context.Context,
	q querier,
) error {
	for table, expected := range publicationEffectTriggerNameContracts {
		actual, err := publicationEffectSchemaObjectNames(
			ctx,
			q,
			"trigger",
			table,
			false,
		)
		if err != nil {
			return err
		}
		if !slices.Equal(actual, expected) {
			return fmt.Errorf(
				"incompatible publication effect trigger set for %s: got %v, want %v",
				table,
				actual,
				expected,
			)
		}
	}
	for table, expected := range publicationEffectExplicitIndexNameContracts {
		actual, err := publicationEffectSchemaObjectNames(
			ctx,
			q,
			"index",
			table,
			true,
		)
		if err != nil {
			return err
		}
		if !slices.Equal(actual, expected) {
			return fmt.Errorf(
				"incompatible publication effect explicit index set for %s: got %v, want %v",
				table,
				actual,
				expected,
			)
		}
	}

	rows, err := q.QueryContext(ctx, `
		SELECT name, tbl_name
		FROM sqlite_master
		WHERE type = 'index'
			AND sql IS NOT NULL
			AND name GLOB 'idx_publication_effect_*'
		ORDER BY name
	`)
	if err != nil {
		return fmt.Errorf(
			"inspect publication effect owned index set: %w",
			err,
		)
	}
	defer rows.Close()
	ownedIndexes := make(map[string]string, 2)
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			return fmt.Errorf(
				"scan publication effect owned index set: %w",
				err,
			)
		}
		ownedIndexes[name] = table
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf(
			"inspect publication effect owned index set: %w",
			err,
		)
	}
	for name, contract := range publicationEffectObjectContracts {
		if contract.objectType != "index" {
			continue
		}
		table, exists := ownedIndexes[name]
		if !exists || table != contract.table {
			return fmt.Errorf(
				"incompatible publication effect owned index %s",
				name,
			)
		}
		delete(ownedIndexes, name)
	}
	if len(ownedIndexes) != 0 {
		return fmt.Errorf(
			"incompatible publication effect owned index set: unexpected %v",
			ownedIndexes,
		)
	}
	return nil
}

func validatePublicationEffectSchema(
	ctx context.Context,
	q querier,
) error {
	for table, expected := range publicationEffectTableContracts {
		rows, err := q.QueryContext(ctx, "PRAGMA table_info("+table+")")
		if err != nil {
			return fmt.Errorf("inspect %s columns: %w", table, err)
		}
		actual := make([]publicationEffectColumnContract, 0, len(expected))
		for rows.Next() {
			var cid int
			var column publicationEffectColumnContract
			var defaultValue sql.NullString
			if err := rows.Scan(
				&cid,
				&column.name,
				&column.columnType,
				&column.notNull,
				&defaultValue,
				&column.primaryKey,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s columns: %w", table, err)
			}
			column.columnType = strings.ToUpper(
				strings.TrimSpace(column.columnType),
			)
			actual = append(actual, column)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("inspect %s columns: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close %s columns: %w", table, err)
		}
		if len(actual) != len(expected) {
			return fmt.Errorf(
				"incompatible %s schema: got %d columns, want %d",
				table,
				len(actual),
				len(expected),
			)
		}
		for index := range expected {
			if actual[index] != expected[index] {
				return fmt.Errorf(
					"incompatible %s schema at column %d: got %+v, want %+v",
					table,
					index,
					actual[index],
					expected[index],
				)
			}
		}
		var definition string
		if err := q.QueryRowContext(ctx, `
			SELECT sql FROM sqlite_master
			WHERE type = 'table' AND name = ?
		`, table).Scan(&definition); err != nil {
			return fmt.Errorf("inspect %s contract: %w", table, err)
		}
		expectedDefinition, err :=
			expectedPublicationEffectObjectSQL("table", table)
		if err != nil {
			return err
		}
		if compactPublicationEffectSQL(definition) != expectedDefinition {
			return fmt.Errorf(
				"incompatible %s schema definition",
				table,
			)
		}
		foreignKeys, err := publicationEffectForeignKeys(ctx, q, table)
		if err != nil {
			return err
		}
		if !samePublicationEffectForeignKeySet(
			foreignKeys,
			publicationEffectForeignKeyContracts[table],
		) {
			return fmt.Errorf(
				"incompatible %s foreign key contract",
				table,
			)
		}
	}

	for name, contract := range publicationEffectObjectContracts {
		var objectType, table, definition string
		if err := q.QueryRowContext(ctx, `
			SELECT type, tbl_name, sql FROM sqlite_master
			WHERE name = ?
		`, name).Scan(&objectType, &table, &definition); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf(
					"incompatible publication effect schema: %s is missing",
					name,
				)
			}
			return fmt.Errorf("inspect publication effect object %s: %w", name, err)
		}
		if objectType != contract.objectType || table != contract.table {
			return fmt.Errorf(
				"incompatible publication effect object %s",
				name,
			)
		}
		expectedDefinition, err :=
			expectedPublicationEffectObjectSQL(contract.objectType, name)
		if err != nil {
			return err
		}
		if compactPublicationEffectSQL(definition) != expectedDefinition {
			return fmt.Errorf(
				"incompatible publication effect object %s definition",
				name,
			)
		}
	}
	return validatePublicationEffectSchemaObjectNameSets(ctx, q)
}

func requireZeroPublicationEffectIntegrityCount(
	ctx context.Context,
	q querier,
	label string,
	query string,
) error {
	var count int
	if err := q.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if count != 0 {
		return fmt.Errorf(
			"publication effect ledger integrity failure: %s (%d rows)",
			label,
			count,
		)
	}
	return nil
}

func validatePublicationEffectLedgerData(
	ctx context.Context,
	q querier,
) error {
	foreignKeyRows, err := q.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf(
			"inspect publication effect foreign key integrity: %w",
			err,
		)
	}
	for foreignKeyRows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKeyID int
		if err := foreignKeyRows.Scan(
			&table,
			&rowID,
			&parent,
			&foreignKeyID,
		); err != nil {
			foreignKeyRows.Close()
			return fmt.Errorf(
				"scan publication effect foreign key integrity: %w",
				err,
			)
		}
		foreignKeyRows.Close()
		return fmt.Errorf(
			"publication effect ledger integrity failure: foreign key %s[%d] -> %s at row %v",
			table,
			foreignKeyID,
			parent,
			rowID,
		)
	}
	if err := foreignKeyRows.Err(); err != nil {
		foreignKeyRows.Close()
		return fmt.Errorf(
			"inspect publication effect foreign key integrity: %w",
			err,
		)
	}
	if err := foreignKeyRows.Close(); err != nil {
		return fmt.Errorf(
			"close publication effect foreign key integrity: %w",
			err,
		)
	}

	checks := []struct {
		label string
		query string
	}{
		{
			label: "orphan or mismatched effect intent parent",
			query: `
				SELECT COUNT(*) FROM publication_effect_intents e
				LEFT JOIN publication_attempt_intents i
					ON i.id = e.attempt_id
				WHERE i.id IS NULL
					OR i.board IS NOT e.board
					OR i.publication_id IS NOT e.publication_id
					OR i.claim_epoch IS NOT e.claim_epoch
					OR i.effect_fingerprint IS NOT
						e.parent_effect_fingerprint
					OR i.execution_provenance_fingerprint IS NOT
						e.parent_provenance_fingerprint
			`,
		},
		{
			label: "non-contiguous effect sequence",
			query: `
				SELECT COUNT(*) FROM (
					SELECT attempt_id
					FROM publication_effect_intents
					GROUP BY attempt_id
					HAVING MIN(sequence) <> 1
						OR MAX(sequence) <> COUNT(*)
				)
			`,
		},
		{
			label: "orphan or mismatched effect result identity",
			query: `
				SELECT COUNT(*) FROM publication_effect_results r
				LEFT JOIN publication_effect_intents e
					ON e.id = r.effect_id
				WHERE e.id IS NULL
					OR e.attempt_id IS NOT r.attempt_id
					OR e.board IS NOT r.board
					OR e.publication_id IS NOT r.publication_id
					OR e.claim_epoch IS NOT r.claim_epoch
					OR e.sequence IS NOT r.sequence
					OR e.identity_fingerprint IS NOT
						r.identity_fingerprint
			`,
		},
		{
			label: "parent result with unresolved effect",
			query: `
				SELECT COUNT(*)
				FROM publication_attempt_results p
				INNER JOIN publication_effect_intents e
					ON e.attempt_id = p.attempt_id
				LEFT JOIN publication_effect_results r
					ON r.effect_id = e.id
				WHERE r.effect_id IS NULL
			`,
		},
		{
			label: "known parent result with unknown effect",
			query: `
				SELECT COUNT(*)
				FROM publication_attempt_results p
				INNER JOIN publication_effect_intents e
					ON e.attempt_id = p.attempt_id
				INNER JOIN publication_effect_results r
					ON r.effect_id = e.id
				WHERE p.outcome <> 'unknown'
					AND r.outcome = 'unknown'
			`,
		},
	}
	for _, check := range checks {
		if err := requireZeroPublicationEffectIntegrityCount(
			ctx,
			q,
			check.label,
			check.query,
		); err != nil {
			return err
		}
	}

	intentRows, err := q.QueryContext(ctx, `
		SELECT `+publicationEffectIntentColumns+`
		FROM publication_effect_intents e
		ORDER BY e.attempt_id, e.sequence
	`)
	if err != nil {
		return fmt.Errorf(
			"inspect publication effect intent evidence: %w",
			err,
		)
	}
	for intentRows.Next() {
		if _, err := scanPublicationEffectIntent(intentRows); err != nil {
			intentRows.Close()
			return fmt.Errorf(
				"publication effect ledger integrity failure: invalid intent: %w",
				err,
			)
		}
	}
	if err := intentRows.Err(); err != nil {
		intentRows.Close()
		return fmt.Errorf(
			"inspect publication effect intent evidence: %w",
			err,
		)
	}
	if err := intentRows.Close(); err != nil {
		return fmt.Errorf(
			"close publication effect intent evidence: %w",
			err,
		)
	}

	resultRows, err := q.QueryContext(ctx, `
		SELECT `+publicationEffectResultColumns+`
		FROM publication_effect_results r
		ORDER BY r.effect_id
	`)
	if err != nil {
		return fmt.Errorf(
			"inspect publication effect result evidence: %w",
			err,
		)
	}
	for resultRows.Next() {
		if _, err := scanPublicationEffectResult(resultRows); err != nil {
			resultRows.Close()
			return fmt.Errorf(
				"publication effect ledger integrity failure: invalid result: %w",
				err,
			)
		}
	}
	if err := resultRows.Err(); err != nil {
		resultRows.Close()
		return fmt.Errorf(
			"inspect publication effect result evidence: %w",
			err,
		)
	}
	if err := resultRows.Close(); err != nil {
		return fmt.Errorf(
			"close publication effect result evidence: %w",
			err,
		)
	}
	return nil
}

// ensurePublicationEffectSchema is called before and after the older schema
// migrations. The first call atomically upgrades a real v30 database. The
// second creates the same objects for empty and older stores once their v30
// parent attempt contract exists.
func (s *Store) ensurePublicationEffectSchema(
	ctx context.Context,
	createForLegacy bool,
) (resultErr error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf(
			"open publication effect migration connection: %w",
			err,
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, conn.Close())
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin publication effect migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(
		ctx,
		"PRAGMA user_version",
	).Scan(&version); err != nil {
		return fmt.Errorf(
			"inspect schema version during publication effect migration: %w",
			err,
		)
	}
	if version > schemaVersion {
		return fmt.Errorf(
			"database schema version %d is newer than supported version %d",
			version,
			schemaVersion,
		)
	}

	var parentTableCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'publication_attempt_intents'
	`).Scan(&parentTableCount); err != nil {
		return fmt.Errorf(
			"inspect publication effect parent schema: %w",
			err,
		)
	}
	if parentTableCount == 0 {
		if version >= 30 {
			return errors.New(
				"incompatible v30 publication schema: publication attempt intents are missing",
			)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf(
				"commit deferred publication effect migration: %w",
				err,
			)
		}
		committed = true
		return nil
	}
	if version < 30 && !createForLegacy {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf(
				"commit deferred legacy publication effect migration: %w",
				err,
			)
		}
		committed = true
		return nil
	}

	var effectTableCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table'
			AND name IN (
				'publication_effect_intents',
				'publication_effect_results'
			)
	`).Scan(&effectTableCount); err != nil {
		return fmt.Errorf("inspect publication effect tables: %w", err)
	}
	if version == schemaVersion && effectTableCount != 2 {
		return errors.New(
			"incompatible v31 publication effect schema: ledger tables are missing",
		)
	}
	if version < schemaVersion {
		if _, err := conn.ExecContext(ctx, publicationEffectSchema); err != nil {
			return fmt.Errorf("create publication effect schema: %w", err)
		}
	}
	if err := validatePublicationEffectSchema(ctx, conn); err != nil {
		return err
	}
	if err := validatePublicationEffectLedgerData(ctx, conn); err != nil {
		return err
	}
	if version == 30 {
		if _, err := conn.ExecContext(
			ctx,
			"PRAGMA user_version = 31",
		); err != nil {
			return fmt.Errorf("set publication effect schema version: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit publication effect migration: %w", err)
	}
	committed = true
	return nil
}
