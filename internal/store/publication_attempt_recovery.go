package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

const (
	publicationAttemptRecoveryLegacySchemaVersion = 28
	publicationAttemptRecoverySchemaVersion       = 30
	publicationAttemptRecoveryPageSize            = 100
)

// PublicationAttemptRecoveryCursor is the stable raw-ledger position returned
// by ListPublicationAttemptRecoveryPage. Its zero value means EOF.
type PublicationAttemptRecoveryCursor struct {
	StartedAt string `json:"startedAt,omitempty"`
	ID        string `json:"id,omitempty"`
}

// PublicationAttemptRecoveryPublication is the token-free current publication
// observation needed to compare a ledger intent with durable board state.
type PublicationAttemptRecoveryPublication struct {
	Present                        bool                    `json:"present"`
	ID                             string                  `json:"id,omitempty"`
	Board                          string                  `json:"board,omitempty"`
	ChangeSetID                    string                  `json:"changeSetId,omitempty"`
	Status                         model.PublicationStatus `json:"status,omitempty"`
	Mode                           model.PublicationMode   `json:"mode,omitempty"`
	TargetBranch                   string                  `json:"targetBranch,omitempty"`
	Remote                         string                  `json:"remote,omitempty"`
	BaseCommit                     string                  `json:"baseCommit,omitempty"`
	HeadCommit                     string                  `json:"headCommit,omitempty"`
	DurableRef                     string                  `json:"durableRef,omitempty"`
	ExecutionProvenanceFingerprint string                  `json:"executionProvenanceFingerprint,omitempty"`
	URL                            *string                 `json:"url,omitempty"`
	Error                          *string                 `json:"error,omitempty"`
	ClaimEpoch                     int64                   `json:"claimEpoch,omitempty"`
	ClaimExpiresAt                 *string                 `json:"claimExpiresAt,omitempty"`
	PublishedAt                    *string                 `json:"publishedAt,omitempty"`
	UpdatedAt                      string                  `json:"updatedAt,omitempty"`
}

// PublicationAttemptRecoveryObservation is one raw unresolved attempt and the
// receipt/current-state evidence observed in the same read transaction.
// RecoveryReceipt is intentionally retained: the dispatcher, not the reader,
// decides whether it proves resolution or exposes an integrity failure.
type PublicationAttemptRecoveryObservation struct {
	Attempt     PublicationAttemptRecord              `json:"attempt"`
	Publication PublicationAttemptRecoveryPublication `json:"publication"`
}

type publicationAttemptRecoverySchemaColumn struct {
	dataType   string
	primaryKey int
}

type publicationAttemptRecoverySchemaObject struct {
	objectType string
	table      string
	sql        string
}

var publicationAttemptRecoveryIntentColumns = map[string]publicationAttemptRecoverySchemaColumn{
	"id":                               {dataType: "TEXT", primaryKey: 1},
	"board":                            {dataType: "TEXT"},
	"publication_id":                   {dataType: "TEXT"},
	"source_key":                       {dataType: "TEXT"},
	"change_set_id":                    {dataType: "TEXT"},
	"mode":                             {dataType: "TEXT"},
	"target_branch":                    {dataType: "TEXT"},
	"remote":                           {dataType: "TEXT"},
	"base_commit":                      {dataType: "TEXT"},
	"head_commit":                      {dataType: "TEXT"},
	"durable_ref":                      {dataType: "TEXT"},
	"execution_provenance_fingerprint": {dataType: "TEXT"},
	"effect_fingerprint":               {dataType: "TEXT"},
	"claim_epoch":                      {dataType: "INTEGER"},
	"publication_updated_at":           {dataType: "TEXT"},
	"claim_expires_at":                 {dataType: "TEXT"},
	"session_id":                       {dataType: "TEXT"},
	"gate_generation":                  {dataType: "INTEGER"},
	"started_at":                       {dataType: "TEXT"},
}

var publicationAttemptRecoveryResultColumns = map[string]publicationAttemptRecoverySchemaColumn{
	"attempt_id":             {dataType: "TEXT", primaryKey: 1},
	"board":                  {dataType: "TEXT"},
	"publication_id":         {dataType: "TEXT"},
	"claim_epoch":            {dataType: "INTEGER"},
	"outcome":                {dataType: "TEXT"},
	"executor_status":        {dataType: "TEXT"},
	"error_kind":             {dataType: "TEXT"},
	"result_url":             {dataType: "TEXT"},
	"error":                  {dataType: "TEXT"},
	"publication_updated_at": {dataType: "TEXT"},
	"recorded_at":            {dataType: "TEXT"},
}

var publicationAttemptRecoveryObjects = map[string]publicationAttemptRecoverySchemaObject{
	"publication_attempt_intents": {
		objectType: "table",
		table:      "publication_attempt_intents",
	},
	"publication_attempt_results": {
		objectType: "table",
		table:      "publication_attempt_results",
	},
	"publication_attempt_intents_prevent_update": {
		objectType: "trigger",
		table:      "publication_attempt_intents",
	},
	"publication_attempt_intents_prevent_delete": {
		objectType: "trigger",
		table:      "publication_attempt_intents",
	},
	"publication_attempt_intents_require_v30_evidence": {
		objectType: "trigger",
		table:      "publication_attempt_intents",
	},
	"publication_attempt_results_identity_guard": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"publication_attempt_results_prevent_update": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"publication_attempt_results_prevent_delete": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"publication_attempt_results_require_v30_evidence": {
		objectType: "trigger",
		table:      "publication_attempt_results",
	},
	"idx_publication_attempt_intents_board_started": {
		objectType: "index",
		table:      "publication_attempt_intents",
	},
	"idx_publication_attempt_intents_publication": {
		objectType: "index",
		table:      "publication_attempt_intents",
	},
}

var publicationAttemptRecoveryTriggerContracts = map[string][]string{
	"publication_attempt_intents_prevent_update": {
		"before update on publication_attempt_intents",
		"raise(abort, 'publication attempt intents are immutable')",
	},
	"publication_attempt_intents_prevent_delete": {
		"before delete on publication_attempt_intents",
		"raise(abort, 'publication attempt intents are immutable')",
	},
	"publication_attempt_intents_require_v30_evidence": {
		"before insert on publication_attempt_intents",
		"new.execution_provenance_fingerprint is null",
		"new.started_at not glob",
		"publication attempt intent requires v30 evidence",
	},
	"publication_attempt_results_identity_guard": {
		"before insert on publication_attempt_results",
		"when not exists",
		"from publication_attempt_intents i",
		"i.id = new.attempt_id",
		"i.board = new.board",
		"i.publication_id = new.publication_id",
		"i.claim_epoch = new.claim_epoch",
		"raise(abort, 'publication attempt result does not match its intent')",
	},
	"publication_attempt_results_prevent_update": {
		"before update on publication_attempt_results",
		"raise(abort, 'publication attempt results are immutable')",
	},
	"publication_attempt_results_prevent_delete": {
		"before delete on publication_attempt_results",
		"raise(abort, 'publication attempt results are immutable')",
	},
	"publication_attempt_results_require_v30_evidence": {
		"before insert on publication_attempt_results",
		"new.recorded_at not glob",
		"from publication_attempt_intents i",
		"i.id = new.attempt_id",
		"i.execution_provenance_fingerprint is not null",
		"publication attempt result requires v30 evidence",
	},
}

var publicationAttemptRecoveryTableContracts = map[string][]string{
	"publication_attempt_intents": {
		"id text primary key",
		"source_key text not null unique",
		"unique(board, publication_id, claim_epoch)",
	},
	"publication_attempt_results": {
		"attempt_id text primary key",
		"outcome text not null",
		"executor_status text not null",
	},
}

func publicationAttemptRecoverySchemaArtifacts(
	ctx context.Context,
	q querier,
) (map[string]publicationAttemptRecoverySchemaObject, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT name, type, tbl_name, sql
		FROM sqlite_master
		WHERE name IN (
			'publication_attempt_intents',
			'publication_attempt_results',
			'publication_attempt_intents_prevent_update',
			'publication_attempt_intents_prevent_delete',
			'publication_attempt_intents_require_v30_evidence',
			'publication_attempt_results_identity_guard',
			'publication_attempt_results_prevent_update',
			'publication_attempt_results_prevent_delete',
			'publication_attempt_results_require_v30_evidence',
			'idx_publication_attempt_intents_board_started',
			'idx_publication_attempt_intents_publication'
		)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make(map[string]publicationAttemptRecoverySchemaObject)
	for rows.Next() {
		var name, objectType, table string
		var definition sql.NullString
		if err := rows.Scan(
			&name,
			&objectType,
			&table,
			&definition,
		); err != nil {
			return nil, err
		}
		objects[name] = publicationAttemptRecoverySchemaObject{
			objectType: objectType,
			table:      table,
			sql:        definition.String,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

func normalizedPublicationAttemptRecoverySQL(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func validatePublicationAttemptRecoveryTriggers(
	objects map[string]publicationAttemptRecoverySchemaObject,
) error {
	for name, required := range publicationAttemptRecoveryTriggerContracts {
		object, ok := objects[name]
		if !ok {
			return fmt.Errorf(
				"publication attempt recovery schema is missing trigger %s",
				name,
			)
		}
		definition := normalizedPublicationAttemptRecoverySQL(object.sql)
		if definition == "" {
			return fmt.Errorf(
				"publication attempt recovery trigger %s has no SQL contract",
				name,
			)
		}
		for _, fragment := range required {
			if !strings.Contains(definition, fragment) {
				return fmt.Errorf(
					"publication attempt recovery trigger %s has an incompatible SQL contract",
					name,
				)
			}
		}
	}
	return nil
}

func validatePublicationAttemptRecoveryTableContracts(
	objects map[string]publicationAttemptRecoverySchemaObject,
) error {
	for name, required := range publicationAttemptRecoveryTableContracts {
		object, ok := objects[name]
		if !ok {
			return fmt.Errorf(
				"publication attempt recovery schema is missing table %s",
				name,
			)
		}
		definition := normalizedPublicationAttemptRecoverySQL(object.sql)
		if definition == "" {
			return fmt.Errorf(
				"publication attempt recovery table %s has no SQL contract",
				name,
			)
		}
		for _, fragment := range required {
			if !strings.Contains(definition, fragment) {
				return fmt.Errorf(
					"publication attempt recovery table %s has an incompatible SQL contract",
					name,
				)
			}
		}
	}
	return nil
}

func validatePublicationAttemptRecoveryTable(
	ctx context.Context,
	q querier,
	table string,
	expected map[string]publicationAttemptRecoverySchemaColumn,
) error {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make(map[string]publicationAttemptRecoverySchemaColumn)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&name,
			&dataType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			return err
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if _, duplicate := actual[name]; duplicate {
			return fmt.Errorf(
				"publication attempt recovery schema has duplicate %s column %s",
				table,
				name,
			)
		}
		actual[name] = publicationAttemptRecoverySchemaColumn{
			dataType:   strings.ToUpper(strings.TrimSpace(dataType)),
			primaryKey: primaryKey,
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf(
			"publication attempt recovery schema has %d %s columns, want %d",
			len(actual),
			table,
			len(expected),
		)
	}
	for name, wanted := range expected {
		got, ok := actual[name]
		if !ok {
			return fmt.Errorf(
				"publication attempt recovery schema is missing %s.%s",
				table,
				name,
			)
		}
		if got != wanted {
			return fmt.Errorf(
				"publication attempt recovery schema has incompatible %s.%s",
				table,
				name,
			)
		}
	}
	return nil
}

func validatePublicationAttemptRecoveryIndex(
	ctx context.Context,
	q querier,
	index string,
	expected []string,
) error {
	rows, err := q.QueryContext(ctx, "PRAGMA index_info("+index+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make([]string, 0, len(expected))
	for rows.Next() {
		var sequence, cid int
		var name string
		if err := rows.Scan(&sequence, &cid, &name); err != nil {
			return err
		}
		if sequence != len(actual) {
			return fmt.Errorf(
				"publication attempt recovery index %s has an invalid sequence",
				index,
			)
		}
		actual = append(actual, strings.ToLower(strings.TrimSpace(name)))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf(
			"publication attempt recovery index %s has incompatible columns",
			index,
		)
	}
	for position := range expected {
		if actual[position] != expected[position] {
			return fmt.Errorf(
				"publication attempt recovery index %s has incompatible columns",
				index,
			)
		}
	}
	return nil
}

func publicationAttemptRecoverySupported(
	ctx context.Context,
	q querier,
) (bool, error) {
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf(
			"inspect publication attempt recovery schema version: %w",
			err,
		)
	}
	if version > schemaVersion {
		return false, fmt.Errorf(
			"publication attempt recovery schema version %d is newer than supported version %d",
			version,
			schemaVersion,
		)
	}
	objects, err := publicationAttemptRecoverySchemaArtifacts(ctx, q)
	if err != nil {
		return false, fmt.Errorf(
			"inspect publication attempt recovery schema: %w",
			err,
		)
	}
	if version < publicationAttemptRecoveryLegacySchemaVersion &&
		len(objects) == 0 {
		return false, nil
	}
	fullV30Objects := len(objects) == len(publicationAttemptRecoveryObjects)
	if fullV30Objects {
		for name, wanted := range publicationAttemptRecoveryObjects {
			got, ok := objects[name]
			if !ok || got.objectType != wanted.objectType ||
				got.table != wanted.table {
				fullV30Objects = false
				break
			}
		}
	}
	if version < publicationAttemptRecoverySchemaVersion &&
		!fullV30Objects {
		legacyObjectCount := 0
		for name, wanted := range publicationAttemptRecoveryObjects {
			if strings.Contains(name, "require_v30_evidence") {
				continue
			}
			legacyObjectCount++
			got, ok := objects[name]
			if !ok || got.objectType != wanted.objectType ||
				got.table != wanted.table {
				if version <
					publicationAttemptRecoveryLegacySchemaVersion {
					return false, errors.New(
						"pre-v28 publication attempt recovery schema is partially present",
					)
				}
				return false, errors.New(
					"pre-v30 publication attempt recovery schema is incomplete",
				)
			}
		}
		if len(objects) < legacyObjectCount {
			return false, errors.New(
				"pre-v30 publication attempt recovery schema is incomplete",
			)
		}
		var evidenceRows int
		if err := q.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM publication_attempt_intents)
				+
				(SELECT COUNT(*) FROM publication_attempt_results)
		`).Scan(&evidenceRows); err != nil {
			return false, fmt.Errorf(
				"inspect pre-v30 publication attempt evidence: %w",
				err,
			)
		}
		if evidenceRows != 0 {
			return false, errors.New(
				"pre-v30 publication attempt evidence lacks durable executor-input provenance",
			)
		}
		return false, nil
	}
	if !fullV30Objects {
		return false, errors.New(
			"publication attempt recovery schema is incomplete",
		)
	}
	for name, wanted := range publicationAttemptRecoveryObjects {
		got, ok := objects[name]
		if !ok || got.objectType != wanted.objectType ||
			got.table != wanted.table {
			return false, fmt.Errorf(
				"publication attempt recovery schema has incompatible object %s",
				name,
			)
		}
	}
	if err := validatePublicationAttemptRecoveryTriggers(objects); err != nil {
		return false, err
	}
	if err := validatePublicationAttemptRecoveryTableContracts(
		objects,
	); err != nil {
		return false, err
	}
	if err := validatePublicationAttemptRecoveryTable(
		ctx,
		q,
		"publication_attempt_intents",
		publicationAttemptRecoveryIntentColumns,
	); err != nil {
		return false, err
	}
	if err := validatePublicationAttemptRecoveryTable(
		ctx,
		q,
		"publication_attempt_results",
		publicationAttemptRecoveryResultColumns,
	); err != nil {
		return false, err
	}
	if err := validatePublicationAttemptRecoveryIndex(
		ctx,
		q,
		"idx_publication_attempt_intents_board_started",
		[]string{"board", "started_at", "id"},
	); err != nil {
		return false, err
	}
	if err := validatePublicationAttemptRecoveryIndex(
		ctx,
		q,
		"idx_publication_attempt_intents_publication",
		[]string{"board", "publication_id", "claim_epoch"},
	); err != nil {
		return false, err
	}
	var foreignKeys int
	if err := q.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM pragma_foreign_key_list(
				'publication_attempt_intents'
			))
			+
			(SELECT COUNT(*) FROM pragma_foreign_key_list(
				'publication_attempt_results'
			))
	`).Scan(&foreignKeys); err != nil {
		return false, fmt.Errorf(
			"inspect publication attempt recovery foreign keys: %w",
			err,
		)
	}
	if foreignKeys != 0 {
		return false, errors.New(
			"publication attempt recovery evidence cannot have foreign keys",
		)
	}
	return true, nil
}

func canonicalPublicationAttemptRecoveryTimestamp(
	value string,
	field string,
	required bool,
) (string, error) {
	raw := value
	value, err := normalizedPublicationText(value, field, 128, required)
	if err != nil || value == "" {
		return value, err
	}
	if value != raw {
		return "", fmt.Errorf("%s is not stored canonically", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339", field)
	}
	if parsed.Location() != time.UTC {
		return "", fmt.Errorf("%s is not stored canonically", field)
	}
	shortest := parsed.UTC().Format(time.RFC3339Nano)
	fixedNanoseconds := parsed.UTC().Format(publicationTimestampLayout)
	if value != shortest && value != fixedNanoseconds {
		return "", fmt.Errorf("%s is not stored canonically", field)
	}
	return value, nil
}

func canonicalPublicationAttemptRecoveryFixedTimestamp(
	value string,
	field string,
	required bool,
) (string, error) {
	if !required && value == "" {
		return "", nil
	}
	normalized, err := canonicalPublicationAttemptRecoveryTimestamp(
		value,
		field,
		required,
	)
	if err != nil || normalized == "" {
		return normalized, err
	}
	if normalized != mustPublicationAttemptFixedTimestamp(normalized) {
		return "", fmt.Errorf(
			"%s is not a fixed-width UTC ledger timestamp",
			field,
		)
	}
	return normalized, nil
}

func mustPublicationAttemptFixedTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(publicationTimestampLayout)
}

func canonicalPublicationAttemptRecoveryID(
	value string,
	field string,
	required bool,
) (string, error) {
	raw := value
	value, err := validRecordID(value, field)
	if err != nil {
		return "", err
	}
	if required && value == "" {
		return "", fmt.Errorf("%s cannot be empty", field)
	}
	if value != raw {
		return "", fmt.Errorf("%s is not stored canonically", field)
	}
	return value, nil
}

func normalizePublicationAttemptRecoveryCursor(
	cursor PublicationAttemptRecoveryCursor,
) (PublicationAttemptRecoveryCursor, error) {
	var err error
	cursor.ID, err = canonicalPublicationAttemptRecoveryID(
		cursor.ID,
		"publication attempt recovery cursor ID",
		false,
	)
	if err != nil {
		return PublicationAttemptRecoveryCursor{}, err
	}
	cursor.StartedAt, err = canonicalPublicationAttemptRecoveryFixedTimestamp(
		cursor.StartedAt,
		"publication attempt recovery cursor startedAt",
		false,
	)
	if err != nil {
		return PublicationAttemptRecoveryCursor{}, err
	}
	if cursor.StartedAt == "" && cursor.ID != "" {
		return PublicationAttemptRecoveryCursor{}, errors.New(
			"publication attempt recovery cursor ID requires startedAt",
		)
	}
	if cursor.StartedAt != "" && cursor.ID == "" {
		return PublicationAttemptRecoveryCursor{}, errors.New(
			"publication attempt recovery cursor startedAt requires ID",
		)
	}
	return cursor, nil
}

func validatePublicationAttemptRecoveryIntent(
	value PublicationAttemptIntent,
	board string,
) error {
	for _, id := range []struct {
		value string
		field string
	}{
		{value.ID, "publication attempt recovery intent ID"},
		{value.PublicationID, "publication attempt recovery publication ID"},
		{value.ChangeSetID, "publication attempt recovery change set ID"},
	} {
		if _, err := canonicalPublicationAttemptRecoveryID(
			id.value,
			id.field,
			true,
		); err != nil {
			return err
		}
	}
	rawBoard := value.Board
	normalizedBoard, err := normalizedPublicationText(
		rawBoard,
		"publication attempt recovery board",
		maxPublicationBoardBytes,
		true,
	)
	if err != nil {
		return err
	}
	if normalizedBoard != rawBoard || normalizedBoard != board {
		return errors.New(
			"publication attempt recovery intent has an incompatible board",
		)
	}
	for _, field := range []struct {
		value    string
		name     string
		maxBytes int
	}{
		{value.TargetBranch, "target branch", maxPublicationBranchBytes},
		{value.Remote, "remote", maxPublicationRemoteBytes},
		{value.BaseCommit, "base commit", 1024},
		{value.HeadCommit, "head commit", 1024},
		{value.DurableRef, "durable ref", 2048},
		{value.SessionID, "session ID", 256},
	} {
		normalized, err := normalizedPublicationText(
			field.value,
			"publication attempt recovery "+field.name,
			field.maxBytes,
			true,
		)
		if err != nil {
			return err
		}
		if normalized != field.value {
			return fmt.Errorf(
				"publication attempt recovery %s is not stored canonically",
				field.name,
			)
		}
	}
	if value.Mode != model.PublicationModeLocalFF &&
		value.Mode != model.PublicationModePullRequest {
		return errors.New(
			"publication attempt recovery intent has an invalid mode",
		)
	}
	if value.ClaimEpoch < 1 || value.GateGeneration < 0 {
		return errors.New(
			"publication attempt recovery intent has invalid generations",
		)
	}
	if _, err := canonicalPublicationAttemptRecoveryTimestamp(
		value.PublicationUpdatedAt,
		"publication attempt recovery publication updatedAt",
		true,
	); err != nil {
		return err
	}
	for _, timestamp := range []struct {
		value string
		field string
	}{
		{value.ClaimExpiresAt, "claim expiresAt"},
		{value.StartedAt, "startedAt"},
	} {
		if _, err := canonicalPublicationAttemptRecoveryFixedTimestamp(
			timestamp.value,
			"publication attempt recovery "+timestamp.field,
			true,
		); err != nil {
			return err
		}
	}
	if value.SourceKey != publicationAttemptSourceKey(
		value.Board,
		value.PublicationID,
		value.PublicationUpdatedAt,
		value.ClaimEpoch,
	) {
		return errors.New(
			"publication attempt recovery intent has an invalid source key",
		)
	}
	if value.EffectFingerprint != publicationEffectFingerprint(value) {
		return errors.New(
			"publication attempt recovery intent has an invalid effect fingerprint",
		)
	}
	if !validPublicationAttemptFingerprint(
		value.ExecutionProvenanceFingerprint,
	) {
		return errors.New(
			"publication attempt recovery intent has an invalid execution provenance fingerprint",
		)
	}
	return nil
}

func validatePublicationAttemptRecoveryResult(
	value PublicationAttemptResult,
) error {
	for _, id := range []struct {
		value string
		field string
	}{
		{value.AttemptID, "publication attempt recovery result attempt ID"},
		{value.PublicationID, "publication attempt recovery result publication ID"},
	} {
		if _, err := canonicalPublicationAttemptRecoveryID(
			id.value,
			id.field,
			true,
		); err != nil {
			return err
		}
	}
	rawBoard := value.Board
	normalizedBoard, err := normalizedPublicationText(
		rawBoard,
		"publication attempt recovery result board",
		maxPublicationBoardBytes,
		true,
	)
	if err != nil {
		return err
	}
	if normalizedBoard != rawBoard || value.ClaimEpoch < 1 {
		return errors.New(
			"publication attempt recovery result has an invalid identity",
		)
	}
	if _, err := canonicalPublicationAttemptRecoveryTimestamp(
		value.PublicationUpdatedAt,
		"publication attempt recovery result publication updatedAt",
		true,
	); err != nil {
		return err
	}
	if _, err := canonicalPublicationAttemptRecoveryFixedTimestamp(
		value.RecordedAt,
		"publication attempt recovery result recordedAt",
		true,
	); err != nil {
		return err
	}
	return nil
}

func publicationAttemptRecoveryCurrent(
	ctx context.Context,
	q querier,
	publicationID string,
) (PublicationAttemptRecoveryPublication, error) {
	var value PublicationAttemptRecoveryPublication
	var taskID, repositoryPath, worktreePath string
	var sourceSnapshot []byte
	var rawURL, publicationError, claimExpiresAt, publishedAt sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id, board, task_id, change_set_id, status, mode,
			target_branch, remote, repository_path, worktree_path,
			base_commit, head_commit, durable_ref, source_snapshot_json,
			url, error, claim_epoch, claim_expires_at, published_at, updated_at
		FROM publications WHERE id = ?
	`, publicationID).Scan(
		&value.ID,
		&value.Board,
		&taskID,
		&value.ChangeSetID,
		&value.Status,
		&value.Mode,
		&value.TargetBranch,
		&value.Remote,
		&repositoryPath,
		&worktreePath,
		&value.BaseCommit,
		&value.HeadCommit,
		&value.DurableRef,
		&sourceSnapshot,
		&rawURL,
		&publicationError,
		&value.ClaimEpoch,
		&claimExpiresAt,
		&publishedAt,
		&value.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationAttemptRecoveryPublication{}, nil
	}
	if err != nil {
		return PublicationAttemptRecoveryPublication{}, err
	}
	value.Present = true
	value.URL = stringPointer(rawURL)
	value.Error = stringPointer(publicationError)
	value.ClaimExpiresAt = stringPointer(claimExpiresAt)
	value.PublishedAt = stringPointer(publishedAt)
	executionProvenanceFingerprint, err :=
		publicationExecutionProvenanceFingerprint(model.Publication{
			TaskID:         taskID,
			RepositoryPath: repositoryPath,
			WorktreePath:   worktreePath,
			SourceSnapshot: append(
				json.RawMessage(nil),
				sourceSnapshot...,
			),
		})
	if err != nil {
		return PublicationAttemptRecoveryPublication{}, fmt.Errorf(
			"derive publication attempt recovery current execution provenance: %w",
			err,
		)
	}
	value.ExecutionProvenanceFingerprint =
		executionProvenanceFingerprint

	for _, id := range []struct {
		value string
		field string
	}{
		{value.ID, "publication attempt recovery current publication ID"},
		{value.ChangeSetID, "publication attempt recovery current change set ID"},
	} {
		if _, err := canonicalPublicationAttemptRecoveryID(
			id.value,
			id.field,
			true,
		); err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
	}
	rawBoard := value.Board
	normalizedBoard, err := normalizedPublicationText(
		rawBoard,
		"publication attempt recovery current board",
		maxPublicationBoardBytes,
		true,
	)
	if err != nil || normalizedBoard != rawBoard {
		if err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
		return PublicationAttemptRecoveryPublication{}, errors.New(
			"publication attempt recovery current board is not stored canonically",
		)
	}
	if !model.ValidPublicationStatus(value.Status) ||
		!model.ValidPublicationMode(value.Mode) ||
		value.ClaimEpoch < 0 {
		return PublicationAttemptRecoveryPublication{}, errors.New(
			"publication attempt recovery current publication has invalid state",
		)
	}
	for _, field := range []struct {
		value    string
		name     string
		maxBytes int
	}{
		{value.TargetBranch, "target branch", maxPublicationBranchBytes},
		{value.Remote, "remote", maxPublicationRemoteBytes},
		{value.BaseCommit, "base commit", 1024},
		{value.HeadCommit, "head commit", 1024},
		{value.DurableRef, "durable ref", 2048},
	} {
		normalized, err := normalizedPublicationText(
			field.value,
			"publication attempt recovery current "+field.name,
			field.maxBytes,
			true,
		)
		if err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
		if normalized != field.value {
			return PublicationAttemptRecoveryPublication{}, fmt.Errorf(
				"publication attempt recovery current %s is not stored canonically",
				field.name,
			)
		}
	}
	normalizedURL, err := normalizePublicationURL(value.URL)
	if err != nil || !sameOptionalString(normalizedURL, value.URL) {
		if err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
		return PublicationAttemptRecoveryPublication{}, errors.New(
			"publication attempt recovery current URL is not stored canonically",
		)
	}
	if value.Error != nil {
		normalized, err := normalizedPublicationText(
			*value.Error,
			"publication attempt recovery current error",
			MaxPublicationErrorBytes,
			false,
		)
		if err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
		if normalized != *value.Error {
			return PublicationAttemptRecoveryPublication{}, errors.New(
				"publication attempt recovery current error is not stored canonically",
			)
		}
	}
	for _, timestamp := range []struct {
		value    *string
		field    string
		required bool
	}{
		{&value.UpdatedAt, "current updatedAt", true},
		{value.ClaimExpiresAt, "current claim expiresAt", false},
		{value.PublishedAt, "current publishedAt", false},
	} {
		if timestamp.value == nil {
			continue
		}
		validate := canonicalPublicationAttemptRecoveryTimestamp
		if timestamp.field == "current claim expiresAt" {
			validate = canonicalPublicationAttemptRecoveryFixedTimestamp
		}
		if _, err := validate(
			*timestamp.value,
			"publication attempt recovery "+timestamp.field,
			timestamp.required,
		); err != nil {
			return PublicationAttemptRecoveryPublication{}, err
		}
	}
	if value.Status == model.PublicationPublishing {
		if value.ClaimEpoch < 1 || value.ClaimExpiresAt == nil {
			return PublicationAttemptRecoveryPublication{}, errors.New(
				"publication attempt recovery current publishing state is incomplete",
			)
		}
	} else if value.ClaimExpiresAt != nil {
		return PublicationAttemptRecoveryPublication{}, errors.New(
			"publication attempt recovery current terminal claim is still present",
		)
	}
	return value, nil
}

func publicationAttemptRecoveryObservationByID(
	ctx context.Context,
	q querier,
	board string,
	attemptID string,
) (PublicationAttemptRecoveryObservation, bool, error) {
	intent, err := publicationAttemptIntent(ctx, q, attemptID)
	if errors.Is(err, ErrPublicationAttemptNotFound) {
		return PublicationAttemptRecoveryObservation{}, false, nil
	}
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, err
	}
	if err := validatePublicationAttemptRecoveryIntent(
		intent,
		board,
	); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, err
	}
	record := PublicationAttemptRecord{Intent: intent}
	result, exists, err := publicationAttemptResult(ctx, q, intent.ID)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, err
	}
	if exists {
		if err := validatePublicationAttemptRecoveryResult(result); err != nil {
			return PublicationAttemptRecoveryObservation{}, false, err
		}
		if result.AttemptID != intent.ID ||
			result.Board != intent.Board ||
			result.PublicationID != intent.PublicationID ||
			result.ClaimEpoch != intent.ClaimEpoch {
			return PublicationAttemptRecoveryObservation{}, false, errors.New(
				"publication attempt recovery result does not match its intent",
			)
		}
		record.Result = &result
	}
	receipt, err := publicationRecoveryReceiptForSource(
		ctx,
		q,
		intent.SourceKey,
	)
	switch {
	case err == nil:
		record.RecoveryReceipt = &receipt
	case errors.Is(err, ErrPublicationRecoveryReceiptNotFound):
	default:
		return PublicationAttemptRecoveryObservation{}, false, err
	}
	current, err := publicationAttemptRecoveryCurrent(
		ctx,
		q,
		intent.PublicationID,
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, err
	}
	return PublicationAttemptRecoveryObservation{
		Attempt:     record,
		Publication: current,
	}, true, nil
}

func publicationAttemptRecoveryKeyAfter(
	startedAt string,
	id string,
	cursor PublicationAttemptRecoveryCursor,
) bool {
	return startedAt > cursor.StartedAt ||
		startedAt == cursor.StartedAt && id > cursor.ID
}

// GetPublicationAttemptRecoveryObservation re-observes one exact ledger entry,
// its result (including known outcomes), receipt, and current publication in a
// single read transaction. Callers use it to close the race between a page
// scan and a later quarantine decision.
func (r *PublicationRecoveryReader) GetPublicationAttemptRecoveryObservation(
	ctx context.Context,
	attemptID string,
) (
	PublicationAttemptRecoveryObservation,
	bool,
	bool,
	error,
) {
	if r == nil || r.db == nil {
		return PublicationAttemptRecoveryObservation{}, false, false, errors.New(
			"publication recovery reader is closed",
		)
	}
	if err := ctx.Err(); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	defer tx.Rollback()
	supported, err := publicationAttemptRecoverySupported(ctx, tx)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	if !supported {
		if err := tx.Commit(); err != nil {
			return PublicationAttemptRecoveryObservation{}, false, false, err
		}
		return PublicationAttemptRecoveryObservation{}, false, false, nil
	}
	attemptID, err = canonicalPublicationAttemptRecoveryID(
		attemptID,
		"publication attempt recovery observation ID",
		true,
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	observation, found, err := publicationAttemptRecoveryObservationByID(
		ctx,
		tx,
		r.board,
		attemptID,
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	if err := tx.Commit(); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	return observation, found, true, nil
}

// GetPublicationAttemptRecoveryObservationForPublication finds the ledger
// entry for one exact legacy Publishing tuple. Unlike the page API it includes
// known results, allowing startup recovery to detect an impossible known
// result that still has the original Publishing state.
func (r *PublicationRecoveryReader) GetPublicationAttemptRecoveryObservationForPublication(
	ctx context.Context,
	publicationID string,
	publicationUpdatedAt string,
	claimEpoch int64,
) (
	PublicationAttemptRecoveryObservation,
	bool,
	bool,
	error,
) {
	if r == nil || r.db == nil {
		return PublicationAttemptRecoveryObservation{}, false, false, errors.New(
			"publication recovery reader is closed",
		)
	}
	if err := ctx.Err(); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	defer tx.Rollback()
	supported, err := publicationAttemptRecoverySupported(ctx, tx)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, false, err
	}
	if !supported {
		if err := tx.Commit(); err != nil {
			return PublicationAttemptRecoveryObservation{}, false, false, err
		}
		return PublicationAttemptRecoveryObservation{}, false, false, nil
	}
	publicationID, err = canonicalPublicationAttemptRecoveryID(
		publicationID,
		"publication attempt recovery tuple publication ID",
		true,
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	publicationUpdatedAt, err =
		canonicalPublicationAttemptRecoveryTimestamp(
			publicationUpdatedAt,
			"publication attempt recovery tuple updatedAt",
			true,
		)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	if claimEpoch < 1 {
		return PublicationAttemptRecoveryObservation{}, false, true, errors.New(
			"publication attempt recovery tuple claim epoch must be positive",
		)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id
		FROM publication_attempt_intents
		WHERE board = ? AND publication_id = ?
			AND publication_updated_at = ? AND claim_epoch = ?
		ORDER BY id
		LIMIT 2
	`,
		r.board,
		publicationID,
		publicationUpdatedAt,
		claimEpoch,
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	var attemptIDs []string
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			rows.Close()
			return PublicationAttemptRecoveryObservation{}, false, true, err
		}
		attemptID, err = canonicalPublicationAttemptRecoveryID(
			attemptID,
			"publication attempt recovery tuple attempt ID",
			true,
		)
		if err != nil {
			rows.Close()
			return PublicationAttemptRecoveryObservation{}, false, true, err
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	if err := rows.Close(); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	if len(attemptIDs) > 1 {
		return PublicationAttemptRecoveryObservation{}, false, true, errors.New(
			"publication attempt recovery tuple has multiple ledger entries",
		)
	}
	if len(attemptIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return PublicationAttemptRecoveryObservation{}, false, true, err
		}
		return PublicationAttemptRecoveryObservation{}, false, true, nil
	}
	observation, found, err := publicationAttemptRecoveryObservationByID(
		ctx,
		tx,
		r.board,
		attemptIDs[0],
	)
	if err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	if !found ||
		observation.Attempt.Intent.PublicationID != publicationID ||
		observation.Attempt.Intent.PublicationUpdatedAt !=
			publicationUpdatedAt ||
		observation.Attempt.Intent.ClaimEpoch != claimEpoch {
		return PublicationAttemptRecoveryObservation{}, false, true, errors.New(
			"publication attempt recovery tuple changed within its read transaction",
		)
	}
	if err := tx.Commit(); err != nil {
		return PublicationAttemptRecoveryObservation{}, false, true, err
	}
	return observation, true, true, nil
}

// ListPublicationAttemptRecoveryPage reads at most 101 raw candidate keys and
// returns at most 100 observations. Candidates are intent-only or explicitly
// unknown attempts, including attempts that already have a recovery receipt.
// A false supported result means the database is a complete pre-v28 schema;
// any partial or malformed attempt schema returns an error instead.
func (r *PublicationRecoveryReader) ListPublicationAttemptRecoveryPage(
	ctx context.Context,
	filter PublicationAttemptFilter,
) (
	[]PublicationAttemptRecoveryObservation,
	PublicationAttemptRecoveryCursor,
	bool,
	error,
) {
	if r == nil || r.db == nil {
		return nil, PublicationAttemptRecoveryCursor{}, false, errors.New(
			"publication recovery reader is closed",
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, false, err
	}
	defer tx.Rollback()

	supported, err := publicationAttemptRecoverySupported(ctx, tx)
	if err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, false, err
	}
	if !supported {
		if err := tx.Commit(); err != nil {
			return nil, PublicationAttemptRecoveryCursor{}, false, err
		}
		return nil, PublicationAttemptRecoveryCursor{}, false, nil
	}
	cursor, err := normalizePublicationAttemptRecoveryCursor(
		PublicationAttemptRecoveryCursor{
			StartedAt: filter.AfterStartedAt,
			ID:        filter.AfterID,
		},
	)
	if err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, true, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > publicationAttemptRecoveryPageSize {
		limit = publicationAttemptRecoveryPageSize
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT i.started_at, i.id
		FROM publication_attempt_intents i
		LEFT JOIN publication_attempt_results r ON r.attempt_id = i.id
		WHERE i.board = ?
			AND (
				i.started_at > ?
				OR (i.started_at = ? AND i.id > ?)
			)
			AND (r.attempt_id IS NULL OR r.outcome = 'unknown')
		ORDER BY i.started_at, i.id
		LIMIT ?
	`,
		r.board,
		cursor.StartedAt,
		cursor.StartedAt,
		cursor.ID,
		limit+1,
	)
	if err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, true, err
	}
	keys := make([]PublicationAttemptRecoveryCursor, 0, limit+1)
	previous := cursor
	for rows.Next() {
		var key PublicationAttemptRecoveryCursor
		if err := rows.Scan(&key.StartedAt, &key.ID); err != nil {
			rows.Close()
			return nil, PublicationAttemptRecoveryCursor{}, true, err
		}
		key, err = normalizePublicationAttemptRecoveryCursor(key)
		if err != nil {
			rows.Close()
			return nil, PublicationAttemptRecoveryCursor{}, true, err
		}
		if !publicationAttemptRecoveryKeyAfter(
			key.StartedAt,
			key.ID,
			previous,
		) {
			rows.Close()
			return nil, PublicationAttemptRecoveryCursor{}, true, errors.New(
				"publication attempt recovery cursor did not advance",
			)
		}
		keys = append(keys, key)
		previous = key
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, PublicationAttemptRecoveryCursor{}, true, err
	}
	if err := rows.Close(); err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, true, err
	}

	next := PublicationAttemptRecoveryCursor{}
	if len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	observations := make(
		[]PublicationAttemptRecoveryObservation,
		0,
		len(keys),
	)
	for _, key := range keys {
		observation, found, err :=
			publicationAttemptRecoveryObservationByID(
				ctx,
				tx,
				r.board,
				key.ID,
			)
		if err != nil {
			return nil, PublicationAttemptRecoveryCursor{}, true, err
		}
		if !found ||
			observation.Attempt.Intent.StartedAt != key.StartedAt {
			return nil, PublicationAttemptRecoveryCursor{}, true, errors.New(
				"publication attempt recovery key changed within its read transaction",
			)
		}
		if observation.Attempt.Result != nil &&
			observation.Attempt.Result.Outcome !=
				PublicationAttemptUnknown {
			return nil, PublicationAttemptRecoveryCursor{}, true, errors.New(
				"publication attempt recovery candidate became known within its read transaction",
			)
		}
		if err := validatePublicationAttemptRecoveryIntent(
			observation.Attempt.Intent,
			r.board,
		); err != nil {
			return nil, PublicationAttemptRecoveryCursor{}, true, err
		}
		observations = append(observations, observation)
	}
	if err := tx.Commit(); err != nil {
		return nil, PublicationAttemptRecoveryCursor{}, true, err
	}
	return observations, next, true, nil
}
