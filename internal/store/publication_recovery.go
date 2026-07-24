package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nn1a/autogora/internal/model"
	_ "modernc.org/sqlite"
)

// PublicationRecoveryReader is a query-only startup view. It deliberately
// bypasses Store initialization so archived databases are never migrated or
// otherwise changed merely because a dispatcher checks ownership.
type PublicationRecoveryReader struct {
	db        *sql.DB
	board     string
	closeOnce sync.Once
	closeHook func() error
	closeErr  error
}

const publicationRecoveryPageSize = 100

func OpenPublicationRecoveryReader(
	ctx context.Context,
	dbPath string,
	board string,
) (*PublicationRecoveryReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	board, err := normalizePublicationBoard(board, board)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(dbPath) == "" || dbPath == ":memory:" {
		return nil, errors.New(
			"publication recovery requires a persistent database path",
		)
	}
	resolved, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve publication recovery database: %w", err)
	}
	source := &url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}
	query := source.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	source.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", source.String())
	if err != nil {
		return nil, fmt.Errorf("open publication recovery database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeWith := func(cause error) (*PublicationRecoveryReader, error) {
		return nil, errors.Join(cause, db.Close())
	}
	if err := db.PingContext(ctx); err != nil {
		return closeWith(fmt.Errorf("read publication recovery database: %w", err))
	}
	var tableName string
	err = db.QueryRowContext(
		ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name = 'publications'`,
	).Scan(&tableName)
	if errors.Is(err, sql.ErrNoRows) {
		return closeWith(errors.New(
			"publication recovery schema is missing publications table",
		))
	}
	if err != nil {
		return closeWith(fmt.Errorf("inspect publication recovery schema: %w", err))
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(publications)")
	if err != nil {
		return closeWith(fmt.Errorf("inspect publication columns: %w", err))
	}
	columns := make(map[string]string)
	for rows.Next() {
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
			return closeWith(fmt.Errorf("scan publication columns: %w", err))
		}
		columns[strings.ToLower(strings.TrimSpace(name))] =
			strings.ToUpper(strings.TrimSpace(columnType))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return closeWith(fmt.Errorf("inspect publication columns: %w", err))
	}
	if err := rows.Close(); err != nil {
		return closeWith(err)
	}
	for _, required := range []string{
		"id",
		"board",
		"status",
		"updated_at",
		"claim_epoch",
	} {
		if _, ok := columns[required]; !ok {
			return closeWith(fmt.Errorf(
				"publication recovery schema is missing %s",
				required,
			))
		}
	}
	if columns["claim_epoch"] != "INTEGER" {
		return closeWith(errors.New(
			"publication recovery claim_epoch must be INTEGER",
		))
	}
	var receiptTableName string
	err = db.QueryRowContext(
		ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name = 'publication_recovery_receipts'`,
	).Scan(&receiptTableName)
	if errors.Is(err, sql.ErrNoRows) {
		return closeWith(errors.New(
			"publication recovery schema is missing publication_recovery_receipts table",
		))
	}
	if err != nil {
		return closeWith(fmt.Errorf(
			"inspect publication recovery receipt schema: %w",
			err,
		))
	}
	receiptRows, err := db.QueryContext(
		ctx,
		"PRAGMA table_info(publication_recovery_receipts)",
	)
	if err != nil {
		return closeWith(fmt.Errorf(
			"inspect publication recovery receipt columns: %w",
			err,
		))
	}
	receiptColumns := make(map[string]string)
	for receiptRows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := receiptRows.Scan(
			&cid,
			&name,
			&columnType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			receiptRows.Close()
			return closeWith(fmt.Errorf(
				"scan publication recovery receipt columns: %w",
				err,
			))
		}
		receiptColumns[strings.ToLower(strings.TrimSpace(name))] =
			strings.ToUpper(strings.TrimSpace(columnType))
	}
	if err := receiptRows.Err(); err != nil {
		receiptRows.Close()
		return closeWith(fmt.Errorf(
			"inspect publication recovery receipt columns: %w",
			err,
		))
	}
	if err := receiptRows.Close(); err != nil {
		return closeWith(err)
	}
	for _, required := range []string{
		"source_key",
		"first_generation",
		"publication_id",
		"observed_updated_at",
		"observed_claim_epoch",
		"outcome",
		"disposition",
		"result_url",
		"actor",
		"reason",
		"recovered_at",
		"result_updated_at",
	} {
		if _, ok := receiptColumns[required]; !ok {
			return closeWith(fmt.Errorf(
				"publication recovery receipt schema is missing %s",
				required,
			))
		}
	}
	if receiptColumns["first_generation"] != "INTEGER" ||
		receiptColumns["observed_claim_epoch"] != "INTEGER" {
		return closeWith(errors.New(
			"publication recovery receipt generations must be INTEGER",
		))
	}
	var immutableTriggerCount int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE type = 'trigger' AND name IN (
			'publication_recovery_receipts_prevent_update',
			'publication_recovery_receipts_prevent_delete'
		 )`,
	).Scan(&immutableTriggerCount); err != nil {
		return closeWith(fmt.Errorf(
			"inspect publication recovery receipt immutability: %w",
			err,
		))
	}
	if immutableTriggerCount != 2 {
		return closeWith(errors.New(
			"publication recovery receipt immutable triggers are missing",
		))
	}
	return &PublicationRecoveryReader{
		db:    db,
		board: board,
	}, nil
}

func (r *PublicationRecoveryReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closeErr = r.db.Close()
		if r.closeHook != nil {
			r.closeErr = errors.Join(r.closeErr, r.closeHook())
		}
	})
	return r.closeErr
}

// SetCloseHook lets Boards.Manager retain an active board's lifecycle lock
// until the query-only reader releases its SQLite handle.
func (r *PublicationRecoveryReader) SetCloseHook(hook func() error) {
	r.closeHook = hook
}

// GetPublicationRecoveryReceipt returns immutable recovery evidence without
// opening a writable Store. Archived board inventory uses this to resume a
// cross-database operator confirmation after phase one was interrupted.
func (r *PublicationRecoveryReader) GetPublicationRecoveryReceipt(
	ctx context.Context,
	sourceKey string,
) (PublicationRecoveryReceipt, bool, error) {
	if r == nil || r.db == nil {
		return PublicationRecoveryReceipt{}, false, errors.New(
			"publication recovery reader is closed",
		)
	}
	sourceKey, err := normalizePublicationRecoverySourceKey(sourceKey)
	if err != nil {
		return PublicationRecoveryReceipt{}, false, err
	}
	value, err := publicationRecoveryReceiptForSource(ctx, r.db, sourceKey)
	if errors.Is(err, ErrPublicationRecoveryReceiptNotFound) {
		return PublicationRecoveryReceipt{}, false, nil
	}
	return value, err == nil, err
}

// GetPublicationForRecovery returns only the token-free identity and state
// fields needed to locate an operator-recovery target.
func (r *PublicationRecoveryReader) GetPublicationForRecovery(
	ctx context.Context,
	id string,
) (model.Publication, bool, error) {
	if r == nil || r.db == nil {
		return model.Publication{}, false, errors.New(
			"publication recovery reader is closed",
		)
	}
	id, err := validRecordID(id, "publication recovery ID")
	if err != nil {
		return model.Publication{}, false, err
	}
	if id == "" {
		return model.Publication{}, false, errors.New(
			"publication recovery ID cannot be empty",
		)
	}
	var value model.Publication
	var rawURL, publicationError, publishedAt sql.NullString
	err = r.db.QueryRowContext(
		ctx,
		`SELECT id, board, status, updated_at, claim_epoch, url, error,
			published_at
		 FROM publications WHERE id = ?`,
		id,
	).Scan(
		&value.ID,
		&value.Board,
		&value.Status,
		&value.UpdatedAt,
		&value.ClaimEpoch,
		&rawURL,
		&publicationError,
		&publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Publication{}, false, nil
	}
	if err != nil {
		return model.Publication{}, false, err
	}
	rawID := value.ID
	normalizedID, err := validRecordID(rawID, "publication recovery ID")
	if err != nil || normalizedID == "" {
		if err == nil {
			err = errors.New("publication recovery ID cannot be empty")
		}
		return model.Publication{}, false, err
	}
	if normalizedID != rawID {
		return model.Publication{}, false, errors.New(
			"publication recovery ID is not stored canonically",
		)
	}
	if value.Board != r.board {
		return model.Publication{}, false, fmt.Errorf(
			"publication %s belongs to board %s, not %s",
			value.ID,
			value.Board,
			r.board,
		)
	}
	if !model.ValidPublicationStatus(value.Status) {
		return model.Publication{}, false, fmt.Errorf(
			"publication %s has invalid status %s",
			value.ID,
			value.Status,
		)
	}
	rawUpdatedAt := value.UpdatedAt
	normalizedUpdatedAt, err := normalizedPublicationText(
		rawUpdatedAt,
		"publication recovery updatedAt",
		128,
		true,
	)
	if err != nil {
		return model.Publication{}, false, err
	}
	if normalizedUpdatedAt != rawUpdatedAt {
		return model.Publication{}, false, errors.New(
			"publication recovery updatedAt is not stored canonically",
		)
	}
	if value.ClaimEpoch < 0 ||
		(value.Status == model.PublicationPublishing && value.ClaimEpoch < 1) {
		return model.Publication{}, false, fmt.Errorf(
			"publication %s has invalid claim epoch",
			value.ID,
		)
	}
	value.URL = stringPointer(rawURL)
	normalizedURL, err := normalizePublicationURL(value.URL)
	if err != nil {
		return model.Publication{}, false, err
	}
	if !sameOptionalString(normalizedURL, value.URL) {
		return model.Publication{}, false, errors.New(
			"publication recovery URL is not stored canonically",
		)
	}
	value.Error = stringPointer(publicationError)
	if value.Error != nil {
		normalizedError, err := normalizedPublicationText(
			*value.Error,
			"publication recovery error",
			MaxPublicationErrorBytes,
			false,
		)
		if err != nil {
			return model.Publication{}, false, err
		}
		if normalizedError != *value.Error {
			return model.Publication{}, false, errors.New(
				"publication recovery error is not stored canonically",
			)
		}
	}
	value.PublishedAt = stringPointer(publishedAt)
	if value.PublishedAt != nil {
		normalizedPublishedAt, err := normalizedPublicationText(
			*value.PublishedAt,
			"publication recovery publishedAt",
			128,
			true,
		)
		if err != nil {
			return model.Publication{}, false, err
		}
		if normalizedPublishedAt != *value.PublishedAt {
			return model.Publication{}, false, errors.New(
				"publication recovery publishedAt is not stored canonically",
			)
		}
	}
	return value, true, nil
}

// ListPublishingAfter returns one token-free keyset page and its next cursor.
// An empty next cursor proves that the full publishing set was consumed.
func (r *PublicationRecoveryReader) ListPublishingAfter(
	ctx context.Context,
	afterID string,
) ([]model.Publication, string, error) {
	if r == nil || r.db == nil {
		return nil, "", errors.New("publication recovery reader is closed")
	}
	afterID, err := validRecordID(afterID, "publication recovery cursor")
	if err != nil {
		return nil, "", err
	}
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, board, status, updated_at, claim_epoch
		 FROM publications
		 WHERE status = 'publishing' AND id > ?
		 ORDER BY id
		 LIMIT ?`,
		afterID,
		publicationRecoveryPageSize+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	result := make([]model.Publication, 0, publicationRecoveryPageSize+1)
	for rows.Next() {
		var value model.Publication
		if err := rows.Scan(
			&value.ID,
			&value.Board,
			&value.Status,
			&value.UpdatedAt,
			&value.ClaimEpoch,
		); err != nil {
			return nil, "", err
		}
		rawID := value.ID
		normalizedID, err := validRecordID(rawID, "publication recovery ID")
		if err != nil || normalizedID == "" {
			if err == nil {
				err = errors.New("publication recovery ID cannot be empty")
			}
			return nil, "", err
		}
		if normalizedID != rawID {
			return nil, "", errors.New(
				"publication recovery ID is not stored canonically",
			)
		}
		rawUpdatedAt := value.UpdatedAt
		normalizedUpdatedAt, err := normalizedPublicationText(
			rawUpdatedAt,
			"publication recovery updatedAt",
			128,
			true,
		)
		if err != nil {
			return nil, "", err
		}
		if normalizedUpdatedAt != rawUpdatedAt {
			return nil, "", errors.New(
				"publication recovery updatedAt is not stored canonically",
			)
		}
		if value.Board != r.board ||
			value.Status != model.PublicationPublishing ||
			value.ClaimEpoch <= 0 {
			return nil, "", fmt.Errorf(
				"publication %s has incompatible publishing ownership evidence",
				value.ID,
			)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(result) > publicationRecoveryPageSize {
		result = result[:publicationRecoveryPageSize]
		next = result[len(result)-1].ID
	}
	return result, next, nil
}
