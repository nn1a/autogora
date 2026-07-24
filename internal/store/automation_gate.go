package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrAutomationQuarantined     = errors.New("automatic mutations are quarantined")
	ErrAutomationGateConflict    = errors.New("automation quarantine changed")
	ErrAutomationHostNotIdle     = errors.New("automation host is not idle")
	ErrAutomationPermitClosed    = errors.New("automation permit is closed")
	ErrAutomationGateNotReady    = errors.New("automation gate is not configured")
	ErrAutomationSourceConflict  = errors.New("automation quarantine source changed")
	ErrAutomationLockUnavailable = errors.New("automation coordination lock is unavailable")
	ErrAutomationPermitScope     = errors.New("automation permit does not cover this board")
)

func secretSafeAutomationLockError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrAutomationLockUnavailable
	}
}

const (
	maxAutomationBoardBytes      = 128
	maxAutomationKindBytes       = 64
	maxAutomationSourceIDBytes   = 256
	maxAutomationEpochBytes      = 128
	maxAutomationClaimEpochBytes = 20
	maxAutomationDiagnostic      = 128
	maxAutomationActorBytes      = 128
	maxAutomationReasonBytes     = 2048
	maxAutomationSessionIDBytes  = 256

	MinAutomationSessionTTL = 5 * time.Second
	MaxAutomationSessionTTL = 15 * time.Minute

	automationTimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type automationGateRuntime struct {
	authorityDB    *sql.DB
	authorityPath  string
	lockPath       string
	authorityOwned bool
	ephemeralLock  bool
}

// AutomationGateConfig points every board Store at the default coordination
// database. The operating-system lock path is derived from the canonical
// authority path so callers cannot accidentally split one authority across
// multiple locks.
type AutomationGateConfig struct {
	AuthorityDBPath string `json:"-"`
}

func (c AutomationGateConfig) String() string {
	return fmt.Sprintf(
		"automation gate config (authority=%t)",
		strings.TrimSpace(c.AuthorityDBPath) != "",
	)
}

func (c AutomationGateConfig) GoString() string { return c.String() }

func canonicalAutomationAuthorityPath(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve automation authority")
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err == nil {
		return canonical, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("resolve automation authority")
	}

	// A database may not exist yet on the first open. Resolve the nearest
	// existing ancestor so aliases through a symlinked parent still derive the
	// same lock identity.
	parent := filepath.Dir(resolved)
	suffix := []string{filepath.Base(resolved)}
	for {
		canonicalParent, parentErr := filepath.EvalSymlinks(parent)
		if parentErr == nil {
			parts := append([]string{canonicalParent}, suffix...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(parentErr, os.ErrNotExist) {
			return "", errors.New("resolve automation authority")
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", errors.New("resolve automation authority")
		}
		suffix = append([]string{filepath.Base(parent)}, suffix...)
		parent = next
	}
}

// ConfigureAutomationGate must be called before a Store is made available to
// concurrent callers. Boards.Manager does this for default and named boards.
func (s *Store) ConfigureAutomationGate(config AutomationGateConfig) error {
	authorityPath := strings.TrimSpace(config.AuthorityDBPath)
	if authorityPath == "" {
		return errors.New("automation gate authority is required")
	}
	lockPath := ""
	var err error
	if authorityPath == ":memory:" {
		if existing := s.automation; existing != nil &&
			existing.authorityPath == authorityPath {
			return nil
		}
		lockPath = filepath.Join(
			os.TempDir(),
			"autogora-"+uuid.NewString()+".automation.lock",
		)
	} else {
		authorityPath, err = canonicalAutomationAuthorityPath(authorityPath)
		if err != nil {
			return err
		}
		lockPath = authorityPath + ".automation.lock"
	}

	if existing := s.automation; existing != nil &&
		existing.authorityPath == authorityPath && existing.lockPath == lockPath {
		return nil
	}

	runtime := &automationGateRuntime{
		authorityPath: authorityPath,
		lockPath:      lockPath,
		ephemeralLock: authorityPath == ":memory:",
	}
	storePath := s.dbPath
	if storePath != ":memory:" {
		storePath, err = canonicalAutomationAuthorityPath(storePath)
		if err != nil {
			return err
		}
	}
	if authorityPath == storePath {
		runtime.authorityDB = s.db
	} else {
		if authorityPath == ":memory:" {
			return errors.New("a board Store cannot attach a separate in-memory automation authority")
		}
		runtime.authorityDB, err = sql.Open("sqlite", dataSourceName(authorityPath))
		if err != nil {
			return fmt.Errorf("open automation authority: %w", err)
		}
		runtime.authorityDB.SetMaxOpenConns(4)
		runtime.authorityDB.SetMaxIdleConns(2)
		runtime.authorityOwned = true
		if err := verifyAutomationAuthority(context.Background(), runtime.authorityDB); err != nil {
			_ = runtime.authorityDB.Close()
			return err
		}
	}
	if previous := s.automation; previous != nil && previous.authorityOwned {
		if err := previous.authorityDB.Close(); err != nil {
			if runtime.authorityOwned {
				_ = runtime.authorityDB.Close()
			}
			return fmt.Errorf("replace automation authority: %w", err)
		}
	}
	s.automation = runtime
	return nil
}

func verifyAutomationAuthority(ctx context.Context, db *sql.DB) error {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'automation_quarantine_gate'`).Scan(&count); err != nil {
		return fmt.Errorf("inspect automation authority: %w", err)
	}
	if count != 1 {
		return ErrAutomationGateNotReady
	}
	return nil
}

func (s *Store) automationRuntime() (*automationGateRuntime, error) {
	if s.automation == nil || s.automation.authorityDB == nil ||
		s.automation.authorityPath == "" || s.automation.lockPath == "" {
		return nil, ErrAutomationGateNotReady
	}
	return s.automation, nil
}

type automationGateRecord struct {
	Active                        bool
	Generation                    int64
	PermitToken                   string
	ActivatedAt                   *string
	ClearedAt                     *string
	ConfirmationStartedAt         *string
	ConfirmationActor             *string
	ConfirmationReason            *string
	ConfirmationHelpersStopped    bool
	ConfirmationExternalWritesOff bool
}

// AutomationQuarantine is a secret-safe view of the global gate. It never
// includes the permit token or lock location.
type AutomationQuarantine struct {
	Active                bool    `json:"active"`
	Generation            int64   `json:"generation"`
	ActivatedAt           *string `json:"activatedAt,omitempty"`
	ClearedAt             *string `json:"clearedAt,omitempty"`
	ActiveSourceCount     int     `json:"activeSourceCount"`
	ConfirmationPending   bool    `json:"confirmationPending"`
	ConfirmationStartedAt *string `json:"confirmationStartedAt,omitempty"`
	ConfirmationActor     *string `json:"confirmationActor,omitempty"`
}

func scanAutomationGate(row scanner) (automationGateRecord, error) {
	var record automationGateRecord
	var active, helpersStopped, externalWritesStopped int
	var activatedAt, clearedAt sql.NullString
	var confirmationStartedAt, actor, reason sql.NullString
	err := row.Scan(
		&active,
		&record.Generation,
		&record.PermitToken,
		&activatedAt,
		&clearedAt,
		&confirmationStartedAt,
		&actor,
		&reason,
		&helpersStopped,
		&externalWritesStopped,
	)
	record.Active = active != 0
	record.ActivatedAt = stringPointer(activatedAt)
	record.ClearedAt = stringPointer(clearedAt)
	record.ConfirmationStartedAt = stringPointer(confirmationStartedAt)
	record.ConfirmationActor = stringPointer(actor)
	record.ConfirmationReason = stringPointer(reason)
	record.ConfirmationHelpersStopped = helpersStopped != 0
	record.ConfirmationExternalWritesOff = externalWritesStopped != 0
	return record, err
}

func readAutomationGate(ctx context.Context, q querier) (automationGateRecord, error) {
	record, err := scanAutomationGate(q.QueryRowContext(ctx, `
		SELECT active, generation, permit_token, activated_at, cleared_at,
			confirmation_started_at, confirmation_actor, confirmation_reason,
			confirmation_helpers_stopped, confirmation_external_writes_stopped
		FROM automation_quarantine_gate WHERE singleton = 1
	`))
	if errors.Is(err, sql.ErrNoRows) {
		return automationGateRecord{}, ErrAutomationGateNotReady
	}
	return record, err
}

func publicAutomationGate(
	ctx context.Context,
	q querier,
	record automationGateRecord,
) (AutomationQuarantine, error) {
	var sourceCount int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_quarantine_sources WHERE disposition = 'active'`,
	).Scan(&sourceCount); err != nil {
		return AutomationQuarantine{}, err
	}
	return AutomationQuarantine{
		Active:                record.Active,
		Generation:            record.Generation,
		ActivatedAt:           record.ActivatedAt,
		ClearedAt:             record.ClearedAt,
		ActiveSourceCount:     sourceCount,
		ConfirmationPending:   record.ConfirmationStartedAt != nil && record.Active,
		ConfirmationStartedAt: record.ConfirmationStartedAt,
		ConfirmationActor:     record.ConfirmationActor,
	}, nil
}

// GetAutomationQuarantine reads the coordination database authority directly;
// board-local mirrors are intentionally not used.
func (s *Store) GetAutomationQuarantine(ctx context.Context) (AutomationQuarantine, error) {
	runtime, err := s.automationRuntime()
	if err != nil {
		return AutomationQuarantine{}, err
	}
	record, err := readAutomationGate(ctx, runtime.authorityDB)
	if err != nil {
		return AutomationQuarantine{}, err
	}
	return publicAutomationGate(ctx, runtime.authorityDB, record)
}

// AutomationPermit holds a shared operating-system lock only for a short
// automatic mutation or process-start boundary. It must not be retained for a
// worker's lifetime.
type AutomationPermit struct {
	mu            sync.Mutex
	lock          automationFileLock
	authorityPath string
	lockPath      string
	generation    int64
	token         string
	sessionID     string
	sessionBoard  string
	sessionToken  string
	closed        bool
	closeErr      error
}

func (p *AutomationPermit) String() string {
	if p == nil {
		return "automation permit (nil)"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return "automation permit (closed)"
	}
	return "automation permit (open)"
}

func (p *AutomationPermit) GoString() string { return p.String() }

func (p *AutomationPermit) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		if p.lock != nil {
			p.closeErr = p.lock.Close()
			p.lock = nil
		}
		p.token = ""
		p.sessionID = ""
		p.sessionBoard = ""
		p.sessionToken = ""
	}
	return p.closeErr
}

func readAutomationPermitState(
	ctx context.Context,
	q querier,
	lease AutomationDispatcherSessionLease,
	current string,
) (automationGateRecord, bool, error) {
	var record automationGateRecord
	var active, helpersStopped, externalWritesStopped, sessionLive int
	var activatedAt, clearedAt sql.NullString
	var confirmationStartedAt, actor, reason sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT g.active, g.generation, g.permit_token, g.activated_at,
			g.cleared_at, g.confirmation_started_at, g.confirmation_actor,
			g.confirmation_reason, g.confirmation_helpers_stopped,
			g.confirmation_external_writes_stopped,
			EXISTS(
				SELECT 1 FROM automation_dispatcher_sessions s
				WHERE s.session_id = ? AND s.board = ? AND s.lease_token = ?
					AND s.released_at IS NULL AND s.expires_at > ?
			)
		FROM automation_quarantine_gate g WHERE g.singleton = 1
	`,
		lease.SessionID,
		lease.Board,
		lease.leaseToken,
		current,
	).Scan(
		&active,
		&record.Generation,
		&record.PermitToken,
		&activatedAt,
		&clearedAt,
		&confirmationStartedAt,
		&actor,
		&reason,
		&helpersStopped,
		&externalWritesStopped,
		&sessionLive,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return automationGateRecord{}, false, ErrAutomationGateNotReady
	}
	record.Active = active != 0
	record.ActivatedAt = stringPointer(activatedAt)
	record.ClearedAt = stringPointer(clearedAt)
	record.ConfirmationStartedAt = stringPointer(confirmationStartedAt)
	record.ConfirmationActor = stringPointer(actor)
	record.ConfirmationReason = stringPointer(reason)
	record.ConfirmationHelpersStopped = helpersStopped != 0
	record.ConfirmationExternalWritesOff = externalWritesStopped != 0
	return record, sessionLive != 0, err
}

// AcquireAutomationPermitForSession waits context-sensitively for a shared
// lock and captures the open gate generation only for an exact live dispatcher
// session. Session changes and quarantine activation take the exclusive side
// of the same lock.
func (s *Store) AcquireAutomationPermitForSession(
	ctx context.Context,
	lease AutomationDispatcherSessionLease,
) (*AutomationPermit, error) {
	if err := validateAutomationSessionLease(lease); err != nil {
		return nil, err
	}
	runtime, err := s.automationRuntime()
	if err != nil {
		return nil, err
	}
	lock, err := acquireAutomationFileLock(ctx, runtime.lockPath, false)
	if err != nil {
		return nil, fmt.Errorf("acquire automation permit: %w", err)
	}
	current := time.Now().UTC().Format(automationTimestampLayout)
	record, sessionLive, err := readAutomationPermitState(
		ctx,
		runtime.authorityDB,
		lease,
		current,
	)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !sessionLive {
		return nil, errors.Join(ErrAutomationHostNotIdle, lock.Close())
	}
	if record.Active {
		return nil, errors.Join(
			&AutomationQuarantinedError{Generation: record.Generation},
			lock.Close(),
		)
	}
	return &AutomationPermit{
		lock:          lock,
		authorityPath: runtime.authorityPath,
		lockPath:      runtime.lockPath,
		generation:    record.Generation,
		token:         record.PermitToken,
		sessionID:     lease.SessionID,
		sessionBoard:  lease.Board,
		sessionToken:  lease.leaseToken,
	}, nil
}

type AutomationQuarantinedError struct {
	Generation int64
}

func (e *AutomationQuarantinedError) Error() string {
	return fmt.Sprintf("%s at generation %d", ErrAutomationQuarantined, e.Generation)
}

func (e *AutomationQuarantinedError) Unwrap() error { return ErrAutomationQuarantined }

// ValidateAutomationPermit rechecks the authority while the permit still
// holds its shared lock. Neither its token nor lock path is formatted.
func (s *Store) ValidateAutomationPermit(
	ctx context.Context,
	permit *AutomationPermit,
) error {
	if permit == nil {
		return ErrAutomationPermitClosed
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	return s.validateAutomationPermitLocked(ctx, permit)
}

func (s *Store) validateAutomationPermitLocked(
	ctx context.Context,
	permit *AutomationPermit,
) error {
	runtime, err := s.automationRuntime()
	if err != nil {
		return err
	}
	if permit.closed || permit.lock == nil || permit.token == "" {
		return ErrAutomationPermitClosed
	}
	if permit.sessionID == "" || permit.sessionBoard == "" || permit.sessionToken == "" {
		return ErrAutomationHostNotIdle
	}
	if permit.authorityPath != runtime.authorityPath || permit.lockPath != runtime.lockPath {
		return errors.New("automation permit belongs to another authority")
	}
	record, sessionLive, err := readAutomationPermitState(
		ctx,
		runtime.authorityDB,
		AutomationDispatcherSessionLease{
			SessionID:  permit.sessionID,
			leaseToken: permit.sessionToken,
			Board:      permit.sessionBoard,
		},
		time.Now().UTC().Format(automationTimestampLayout),
	)
	if err != nil {
		return err
	}
	if !sessionLive {
		return ErrAutomationHostNotIdle
	}
	if record.Active {
		return &AutomationQuarantinedError{Generation: record.Generation}
	}
	if record.Generation != permit.generation || record.PermitToken != permit.token {
		return ErrAutomationGateConflict
	}
	return nil
}

// WithAutomationPermit revalidates a session-bound permit and keeps concurrent
// Close from releasing its shared OS lock until mutate returns. Callers use it
// for automatic start boundaries that do not have a dedicated guarded wrapper.
func (s *Store) WithAutomationPermit(
	ctx context.Context,
	permit *AutomationPermit,
	mutate func() error,
) error {
	return s.withAutomationPermitForBoard(ctx, permit, s.board, mutate)
}

func (s *Store) withAutomationPermitForBoard(
	ctx context.Context,
	permit *AutomationPermit,
	board string,
	mutate func() error,
) error {
	if permit == nil {
		return ErrAutomationPermitClosed
	}
	if mutate == nil {
		return errors.New("automation permit mutation cannot be nil")
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if err := s.validateAutomationPermitLocked(ctx, permit); err != nil {
		return err
	}
	targetBoard := strings.TrimSpace(board)
	if targetBoard == "" ||
		(permit.sessionBoard != "*" && permit.sessionBoard != targetBoard) {
		return ErrAutomationPermitScope
	}
	return mutate()
}

func boundedAutomationText(value, field string, maxBytes int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return "", fmt.Errorf("%s cannot be empty", field)
		}
		return "", nil
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("%s must be valid UTF-8 without NUL", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("%s must be at most %d bytes", field, maxBytes)
	}
	return value, nil
}

type AutomationQuarantineSourceInput struct {
	Board             string `json:"board"`
	Kind              string `json:"kind"`
	SourceID          string `json:"sourceId"`
	ObservedUpdatedAt string `json:"observedUpdatedAt,omitempty"`
	// ObservedClaimEpoch is a decimal, monotonically increasing attempt epoch.
	// It must never contain a publication claim token.
	ObservedClaimEpoch string `json:"observedClaimEpoch,omitempty"`
	DiagnosticCode     string `json:"diagnosticCode"`
}

type AutomationQuarantineSource struct {
	SourceKey          string  `json:"sourceKey"`
	Generation         int64   `json:"generation"`
	Board              string  `json:"board"`
	Kind               string  `json:"kind"`
	SourceID           string  `json:"sourceId"`
	ObservedUpdatedAt  string  `json:"observedUpdatedAt,omitempty"`
	ObservedClaimEpoch string  `json:"observedClaimEpoch,omitempty"`
	DiagnosticCode     string  `json:"diagnosticCode"`
	Disposition        string  `json:"disposition"`
	ObservedAt         string  `json:"observedAt"`
	ResolvedAt         *string `json:"resolvedAt,omitempty"`
	ResolvedBy         *string `json:"resolvedBy,omitempty"`
	ResolutionReason   *string `json:"resolutionReason,omitempty"`
	ResolvedGeneration *int64  `json:"resolvedGeneration,omitempty"`
}

type AutomationQuarantineSourceFilter struct {
	Board      string
	Kind       string
	SourceID   string
	ActiveOnly bool
	Limit      int
}

func normalizeAutomationSource(
	input AutomationQuarantineSourceInput,
) (AutomationQuarantineSourceInput, string, error) {
	var err error
	input.Board, err = boundedAutomationText(input.Board, "source board", maxAutomationBoardBytes, true)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	input.Kind, err = boundedAutomationText(input.Kind, "source kind", maxAutomationKindBytes, true)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	input.SourceID, err = boundedAutomationText(input.SourceID, "source ID", maxAutomationSourceIDBytes, true)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	input.ObservedUpdatedAt, err = boundedAutomationText(
		input.ObservedUpdatedAt, "source observed update", maxAutomationEpochBytes, false,
	)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	input.ObservedClaimEpoch, err = normalizeAutomationClaimEpoch(input.ObservedClaimEpoch)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	if input.ObservedUpdatedAt == "" && input.ObservedClaimEpoch == "" {
		return AutomationQuarantineSourceInput{}, "", errors.New(
			"source requires an observed update or claim epoch",
		)
	}
	input.DiagnosticCode, err = boundedAutomationText(
		input.DiagnosticCode, "source diagnostic code", maxAutomationDiagnostic, true,
	)
	if err != nil {
		return AutomationQuarantineSourceInput{}, "", err
	}
	canonical := strings.Join([]string{
		input.Board,
		input.Kind,
		input.SourceID,
		input.ObservedUpdatedAt,
		input.ObservedClaimEpoch,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return input, hex.EncodeToString(sum[:]), nil
}

func normalizeAutomationClaimEpoch(value string) (string, error) {
	value, err := boundedAutomationText(
		value,
		"source observed claim epoch",
		maxAutomationClaimEpochBytes,
		false,
	)
	if err != nil || value == "" {
		return value, err
	}
	epoch, err := strconv.ParseUint(value, 10, 64)
	if err != nil || epoch == 0 {
		return "", errors.New(
			"source observed claim epoch must be a positive decimal integer",
		)
	}
	return strconv.FormatUint(epoch, 10), nil
}

func scanAutomationSource(row scanner) (AutomationQuarantineSource, error) {
	var value AutomationQuarantineSource
	var resolvedAt, resolvedBy, resolutionReason sql.NullString
	var resolvedGeneration sql.NullInt64
	err := row.Scan(
		&value.SourceKey,
		&value.Generation,
		&value.Board,
		&value.Kind,
		&value.SourceID,
		&value.ObservedUpdatedAt,
		&value.ObservedClaimEpoch,
		&value.DiagnosticCode,
		&value.Disposition,
		&value.ObservedAt,
		&resolvedAt,
		&resolvedBy,
		&resolutionReason,
		&resolvedGeneration,
	)
	value.ResolvedAt = stringPointer(resolvedAt)
	value.ResolvedBy = stringPointer(resolvedBy)
	value.ResolutionReason = stringPointer(resolutionReason)
	if resolvedGeneration.Valid {
		generation := resolvedGeneration.Int64
		value.ResolvedGeneration = &generation
	}
	return value, err
}

const automationSourceColumns = `source_key, generation, board, kind, source_id,
	observed_updated_at, observed_claim_epoch, diagnostic_code, disposition,
	observed_at, resolved_at, resolved_by, resolution_reason, resolved_generation`

func listAutomationSources(
	ctx context.Context,
	q querier,
	filter AutomationQuarantineSourceFilter,
) ([]AutomationQuarantineSource, error) {
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if value := strings.TrimSpace(filter.Board); value != "" {
		clauses = append(clauses, "board = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.Kind); value != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.SourceID); value != "" {
		clauses = append(clauses, "source_id = ?")
		args = append(args, value)
	}
	if filter.ActiveOnly {
		clauses = append(clauses, "disposition = 'active'")
	}
	query := "SELECT " + automationSourceColumns + " FROM automation_quarantine_sources"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY generation DESC, observed_at DESC, source_key"
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query += " LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AutomationQuarantineSource, 0)
	for rows.Next() {
		value, err := scanAutomationSource(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// ListAutomationQuarantineSources supports publication recovery by exact
// board/kind/source identity and includes resolved generations/dispositions.
func (s *Store) ListAutomationQuarantineSources(
	ctx context.Context,
	filter AutomationQuarantineSourceFilter,
) ([]AutomationQuarantineSource, error) {
	runtime, err := s.automationRuntime()
	if err != nil {
		return nil, err
	}
	filter.Board, err = boundedAutomationText(
		filter.Board, "source filter board", maxAutomationBoardBytes, false,
	)
	if err != nil {
		return nil, err
	}
	filter.Kind, err = boundedAutomationText(
		filter.Kind, "source filter kind", maxAutomationKindBytes, false,
	)
	if err != nil {
		return nil, err
	}
	filter.SourceID, err = boundedAutomationText(
		filter.SourceID, "source filter ID", maxAutomationSourceIDBytes, false,
	)
	if err != nil {
		return nil, err
	}
	if filter.Limit < 0 || filter.Limit > 1000 {
		return nil, errors.New("source filter limit must be between 0 and 1000")
	}
	return listAutomationSources(ctx, runtime.authorityDB, filter)
}

func randomAutomationToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ActivateAutomationQuarantine serializes source observation and gate rotation
// with an exclusive operating-system lock.
func (s *Store) ActivateAutomationQuarantine(
	ctx context.Context,
	input AutomationQuarantineSourceInput,
) (value AutomationQuarantine, activated bool, resultErr error) {
	if err := s.requireCoordinationStore(); err != nil {
		return AutomationQuarantine{}, false, err
	}
	runtime, err := s.automationRuntime()
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if runtime.authorityDB != s.db {
		return AutomationQuarantine{}, false, errors.New(
			"automation quarantine activation requires the authority Store",
		)
	}
	input, sourceKey, err := normalizeAutomationSource(input)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	lock, err := acquireAutomationFileLock(ctx, runtime.lockPath, true)
	if err != nil {
		return AutomationQuarantine{}, false, fmt.Errorf("lock automation quarantine: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Close())
	}()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM automation_quarantine_sources WHERE source_key = ?
	)`, sourceKey).Scan(&exists); err != nil {
		return AutomationQuarantine{}, false, err
	}
	record, err := readAutomationGate(ctx, tx)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if exists {
		value, err := publicAutomationGate(ctx, tx, record)
		return value, false, err
	}
	var activeSourceCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_quarantine_sources WHERE disposition = 'active'`,
	).Scan(&activeSourceCount); err != nil {
		return AutomationQuarantine{}, false, err
	}
	if activeSourceCount >= 1000 {
		return AutomationQuarantine{}, false, errors.New(
			"automation quarantine has too many active sources",
		)
	}
	token, err := randomAutomationToken()
	if err != nil {
		return AutomationQuarantine{}, false, fmt.Errorf(
			"generate automation generation token: %w",
			err,
		)
	}
	generation := record.Generation + 1
	timestamp := time.Now().UTC().Format(automationTimestampLayout)
	if _, err := tx.ExecContext(ctx, `INSERT INTO automation_quarantine_sources(
		source_key, generation, board, kind, source_id, observed_updated_at,
		observed_claim_epoch, diagnostic_code, disposition, observed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
		sourceKey, generation, input.Board, input.Kind, input.SourceID,
		input.ObservedUpdatedAt, input.ObservedClaimEpoch, input.DiagnosticCode,
		timestamp,
	); err != nil {
		return AutomationQuarantine{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE automation_quarantine_gate
		SET active = 1, generation = ?, permit_token = ?, activated_at = ?,
			cleared_at = NULL, confirmation_started_at = NULL,
			confirmation_actor = NULL, confirmation_reason = NULL,
			confirmation_helpers_stopped = 0,
			confirmation_external_writes_stopped = 0
		WHERE singleton = 1`,
		generation, token, timestamp,
	); err != nil {
		return AutomationQuarantine{}, false, err
	}
	record, err = readAutomationGate(ctx, tx)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	value, err = publicAutomationGate(ctx, tx, record)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AutomationQuarantine{}, false, err
	}
	committed = true
	return value, true, nil
}

type AutomationSourceDisposition string

const (
	AutomationSourceSuperseded AutomationSourceDisposition = "superseded"
	AutomationSourceAbandoned  AutomationSourceDisposition = "abandoned"
)

type AutomationQuarantineSourceResolution struct {
	SourceKey          string                      `json:"sourceKey"`
	ObservedUpdatedAt  string                      `json:"observedUpdatedAt,omitempty"`
	ObservedClaimEpoch string                      `json:"observedClaimEpoch,omitempty"`
	Disposition        AutomationSourceDisposition `json:"disposition"`
}

type AutomationQuarantineSnapshot struct {
	Gate    AutomationQuarantine         `json:"gate"`
	Sources []AutomationQuarantineSource `json:"sources"`
}

type AutomationQuarantineConfirmation struct {
	Generation            int64                                                     `json:"generation"`
	Actor                 string                                                    `json:"actor"`
	Reason                string                                                    `json:"reason"`
	HelpersStopped        bool                                                      `json:"helpersStopped"`
	ExternalWritesStopped bool                                                      `json:"externalWritesStopped"`
	Sources               []AutomationQuarantineSourceResolution                    `json:"sources"`
	Guard                 func(context.Context, AutomationQuarantineSnapshot) error `json:"-"`
}

type AutomationGateConflictError struct {
	ExpectedGeneration int64
	ActualGeneration   int64
	Active             bool
}

func (e *AutomationGateConflictError) Error() string {
	return fmt.Sprintf(
		"%s: expected generation %d, current generation %d (active=%t)",
		ErrAutomationGateConflict,
		e.ExpectedGeneration,
		e.ActualGeneration,
		e.Active,
	)
}

func (e *AutomationGateConflictError) Unwrap() error { return ErrAutomationGateConflict }

func normalizeAutomationConfirmation(
	input AutomationQuarantineConfirmation,
) (AutomationQuarantineConfirmation, error) {
	var err error
	if input.Generation < 1 {
		return AutomationQuarantineConfirmation{}, errors.New(
			"automation quarantine generation must be positive",
		)
	}
	input.Actor, err = boundedAutomationText(
		input.Actor, "confirmation actor", maxAutomationActorBytes, true,
	)
	if err != nil {
		return AutomationQuarantineConfirmation{}, err
	}
	input.Reason, err = boundedAutomationText(
		input.Reason, "confirmation reason", maxAutomationReasonBytes, true,
	)
	if err != nil {
		return AutomationQuarantineConfirmation{}, err
	}
	if !input.HelpersStopped || !input.ExternalWritesStopped {
		return AutomationQuarantineConfirmation{}, errors.New(
			"confirmation requires stopped helpers and stopped external writes",
		)
	}
	if len(input.Sources) > 1000 {
		return AutomationQuarantineConfirmation{}, errors.New(
			"confirmation has too many source resolutions",
		)
	}
	seen := make(map[string]bool, len(input.Sources))
	for index := range input.Sources {
		resolution := &input.Sources[index]
		resolution.SourceKey, err = boundedAutomationText(
			resolution.SourceKey, "source key", 64, true,
		)
		if err != nil {
			return AutomationQuarantineConfirmation{}, err
		}
		if len(resolution.SourceKey) != 64 {
			return AutomationQuarantineConfirmation{}, errors.New("source key is invalid")
		}
		resolution.ObservedUpdatedAt, err = boundedAutomationText(
			resolution.ObservedUpdatedAt, "source observed update", maxAutomationEpochBytes, false,
		)
		if err != nil {
			return AutomationQuarantineConfirmation{}, err
		}
		resolution.ObservedClaimEpoch, err = normalizeAutomationClaimEpoch(
			resolution.ObservedClaimEpoch,
		)
		if err != nil {
			return AutomationQuarantineConfirmation{}, err
		}
		if resolution.ObservedUpdatedAt == "" && resolution.ObservedClaimEpoch == "" {
			return AutomationQuarantineConfirmation{}, errors.New(
				"source resolution requires an observed update or claim epoch",
			)
		}
		if resolution.Disposition != AutomationSourceSuperseded &&
			resolution.Disposition != AutomationSourceAbandoned {
			if len(resolution.Disposition) > 32 {
				return AutomationQuarantineConfirmation{}, errors.New(
					"source disposition is invalid",
				)
			}
			return AutomationQuarantineConfirmation{}, fmt.Errorf(
				"invalid source disposition: %s",
				resolution.Disposition,
			)
		}
		if seen[resolution.SourceKey] {
			return AutomationQuarantineConfirmation{}, errors.New(
				"duplicate source resolution",
			)
		}
		seen[resolution.SourceKey] = true
	}
	sort.Slice(input.Sources, func(i, j int) bool {
		return input.Sources[i].SourceKey < input.Sources[j].SourceKey
	})
	return input, nil
}

func confirmationMatches(
	record automationGateRecord,
	input AutomationQuarantineConfirmation,
) bool {
	return record.ConfirmationStartedAt != nil &&
		record.ConfirmationActor != nil && *record.ConfirmationActor == input.Actor &&
		record.ConfirmationReason != nil && *record.ConfirmationReason == input.Reason &&
		record.ConfirmationHelpersStopped == input.HelpersStopped &&
		record.ConfirmationExternalWritesOff == input.ExternalWritesStopped
}

func exactResolutionSet(
	sources []AutomationQuarantineSource,
	resolutions []AutomationQuarantineSourceResolution,
	allowResolved bool,
	input AutomationQuarantineConfirmation,
) error {
	if len(sources) != len(resolutions) {
		return fmt.Errorf(
			"%w: exact active source set is required",
			ErrAutomationSourceConflict,
		)
	}
	byKey := make(map[string]AutomationQuarantineSourceResolution, len(resolutions))
	for _, resolution := range resolutions {
		byKey[resolution.SourceKey] = resolution
	}
	for _, source := range sources {
		resolution, ok := byKey[source.SourceKey]
		if !ok ||
			resolution.ObservedUpdatedAt != source.ObservedUpdatedAt ||
			resolution.ObservedClaimEpoch != source.ObservedClaimEpoch {
			return fmt.Errorf(
				"%w: exact source observation is required",
				ErrAutomationSourceConflict,
			)
		}
		if allowResolved {
			if source.ResolvedGeneration == nil ||
				*source.ResolvedGeneration != input.Generation ||
				source.ResolvedBy == nil || *source.ResolvedBy != input.Actor ||
				source.ResolutionReason == nil || *source.ResolutionReason != input.Reason ||
				source.Disposition != string(resolution.Disposition) {
				return fmt.Errorf(
					"%w: resolved source confirmation differs",
					ErrAutomationSourceConflict,
				)
			}
		} else if source.Disposition != "active" {
			return fmt.Errorf("%w: source is no longer active", ErrAutomationSourceConflict)
		}
	}
	return nil
}

func liveSessionsWithoutAck(
	ctx context.Context,
	q querier,
	generation int64,
	current string,
) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.session_id
		FROM automation_dispatcher_sessions s
		LEFT JOIN automation_dispatcher_acks a
			ON a.session_id = s.session_id AND a.generation = ?
		WHERE s.released_at IS NULL AND s.expires_at > ?
			AND a.session_id IS NULL
		ORDER BY s.session_id
	`, generation, current)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		result = append(result, sessionID)
	}
	return result, rows.Err()
}

// ConfirmAutomationQuarantine first resolves the exact active source set in a
// committed transaction and only then clears the gate in a second transaction.
// Repeating the same exact confirmation safely completes a phase-one crash.
func (s *Store) ConfirmAutomationQuarantine(
	ctx context.Context,
	input AutomationQuarantineConfirmation,
) (value AutomationQuarantine, cleared bool, resultErr error) {
	if err := s.requireCoordinationStore(); err != nil {
		return AutomationQuarantine{}, false, err
	}
	runtime, err := s.automationRuntime()
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if runtime.authorityDB != s.db {
		return AutomationQuarantine{}, false, errors.New(
			"automation quarantine confirmation requires the authority Store",
		)
	}
	input, err = normalizeAutomationConfirmation(input)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	lock, err := acquireAutomationFileLock(ctx, runtime.lockPath, true)
	if err != nil {
		return AutomationQuarantine{}, false, fmt.Errorf(
			"lock automation confirmation: %w",
			err,
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Close())
	}()

	record, err := readAutomationGate(ctx, s.db)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if record.Generation != input.Generation {
		return AutomationQuarantine{}, false, &AutomationGateConflictError{
			ExpectedGeneration: input.Generation,
			ActualGeneration:   record.Generation,
			Active:             record.Active,
		}
	}
	if !record.Active {
		if !confirmationMatches(record, input) {
			return AutomationQuarantine{}, false, &AutomationGateConflictError{
				ExpectedGeneration: input.Generation,
				ActualGeneration:   record.Generation,
				Active:             false,
			}
		}
		sources, err := listAutomationSources(ctx, s.db, AutomationQuarantineSourceFilter{
			Limit: 1000,
		})
		if err != nil {
			return AutomationQuarantine{}, false, err
		}
		resolved := make([]AutomationQuarantineSource, 0, len(input.Sources))
		wanted := make(map[string]bool, len(input.Sources))
		for _, resolution := range input.Sources {
			wanted[resolution.SourceKey] = true
		}
		for _, source := range sources {
			if wanted[source.SourceKey] {
				resolved = append(resolved, source)
			}
		}
		if err := exactResolutionSet(resolved, input.Sources, true, input); err != nil {
			return AutomationQuarantine{}, false, err
		}
		value, err := publicAutomationGate(ctx, s.db, record)
		return value, false, err
	}

	activeSources, err := listAutomationSources(ctx, s.db, AutomationQuarantineSourceFilter{
		ActiveOnly: true,
		Limit:      1000,
	})
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	phaseOneComplete := record.ConfirmationStartedAt != nil && len(activeSources) == 0
	var confirmationSources []AutomationQuarantineSource
	if phaseOneComplete {
		if !confirmationMatches(record, input) {
			return AutomationQuarantine{}, false, ErrAutomationGateConflict
		}
		allSources, err := listAutomationSources(ctx, s.db, AutomationQuarantineSourceFilter{
			Limit: 1000,
		})
		if err != nil {
			return AutomationQuarantine{}, false, err
		}
		wanted := make(map[string]bool, len(input.Sources))
		for _, resolution := range input.Sources {
			wanted[resolution.SourceKey] = true
		}
		for _, source := range allSources {
			if wanted[source.SourceKey] {
				confirmationSources = append(confirmationSources, source)
			}
		}
		if err := exactResolutionSet(
			confirmationSources,
			input.Sources,
			true,
			input,
		); err != nil {
			return AutomationQuarantine{}, false, err
		}
	} else {
		if err := exactResolutionSet(activeSources, input.Sources, false, input); err != nil {
			return AutomationQuarantine{}, false, err
		}
		confirmationSources = activeSources
	}

	current := time.Now().UTC().Format(automationTimestampLayout)
	unacknowledged, err := liveSessionsWithoutAck(
		ctx,
		s.db,
		input.Generation,
		current,
	)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if len(unacknowledged) > 0 {
		return AutomationQuarantine{}, false, fmt.Errorf(
			"%w: %d live dispatcher session(s) have not acknowledged generation %d",
			ErrAutomationHostNotIdle,
			len(unacknowledged),
			input.Generation,
		)
	}
	publicGate, err := publicAutomationGate(ctx, s.db, record)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if input.Guard != nil {
		if err := input.Guard(ctx, AutomationQuarantineSnapshot{
			Gate:    publicGate,
			Sources: append([]AutomationQuarantineSource(nil), confirmationSources...),
		}); err != nil {
			return AutomationQuarantine{}, false, fmt.Errorf(
				"automation confirmation guard: %w",
				err,
			)
		}
	}

	if !phaseOneComplete {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return AutomationQuarantine{}, false, err
		}
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		currentRecord, err := readAutomationGate(ctx, tx)
		if err != nil {
			return AutomationQuarantine{}, false, err
		}
		if !currentRecord.Active || currentRecord.Generation != input.Generation ||
			currentRecord.ConfirmationStartedAt != nil {
			return AutomationQuarantine{}, false, ErrAutomationGateConflict
		}
		for _, resolution := range input.Sources {
			result, err := tx.ExecContext(ctx, `
				UPDATE automation_quarantine_sources
				SET disposition = ?, resolved_at = ?, resolved_by = ?,
					resolution_reason = ?, resolved_generation = ?
				WHERE source_key = ? AND observed_updated_at = ?
					AND observed_claim_epoch = ? AND disposition = 'active'
			`,
				resolution.Disposition,
				current,
				input.Actor,
				input.Reason,
				input.Generation,
				resolution.SourceKey,
				resolution.ObservedUpdatedAt,
				resolution.ObservedClaimEpoch,
			)
			if err != nil {
				return AutomationQuarantine{}, false, err
			}
			changed, err := result.RowsAffected()
			if err != nil || changed != 1 {
				if err != nil {
					return AutomationQuarantine{}, false, err
				}
				return AutomationQuarantine{}, false, ErrAutomationSourceConflict
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE automation_quarantine_gate
			SET confirmation_started_at = ?, confirmation_actor = ?,
				confirmation_reason = ?, confirmation_helpers_stopped = 1,
				confirmation_external_writes_stopped = 1
			WHERE singleton = 1 AND active = 1 AND generation = ?`,
			current,
			input.Actor,
			input.Reason,
			input.Generation,
		); err != nil {
			return AutomationQuarantine{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return AutomationQuarantine{}, false, err
		}
		committed = true
		if s.automationAfterConfirmationPhaseOne != nil {
			if err := s.automationAfterConfirmationPhaseOne(); err != nil {
				return AutomationQuarantine{}, false, err
			}
		}
	}

	token, err := randomAutomationToken()
	if err != nil {
		return AutomationQuarantine{}, false, fmt.Errorf(
			"generate cleared automation generation token: %w",
			err,
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	currentRecord, err := readAutomationGate(ctx, tx)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if !currentRecord.Active || currentRecord.Generation != input.Generation ||
		!confirmationMatches(currentRecord, input) {
		return AutomationQuarantine{}, false, ErrAutomationGateConflict
	}
	var activeCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM automation_quarantine_sources WHERE disposition = 'active'`,
	).Scan(&activeCount); err != nil {
		return AutomationQuarantine{}, false, err
	}
	if activeCount != 0 {
		return AutomationQuarantine{}, false, ErrAutomationSourceConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE automation_quarantine_gate
		SET active = 0, permit_token = ?, cleared_at = ?
		WHERE singleton = 1 AND active = 1 AND generation = ?`,
		token,
		current,
		input.Generation,
	); err != nil {
		return AutomationQuarantine{}, false, err
	}
	currentRecord, err = readAutomationGate(ctx, tx)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	value, err = publicAutomationGate(ctx, tx, currentRecord)
	if err != nil {
		return AutomationQuarantine{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AutomationQuarantine{}, false, err
	}
	committed = true
	return value, true, nil
}

type AutomationDispatcherSessionLease struct {
	SessionID    string `json:"sessionId"`
	Board        string `json:"board"`
	leaseToken   string
	RegisteredAt string  `json:"registeredAt"`
	RenewedAt    string  `json:"renewedAt"`
	ExpiresAt    string  `json:"expiresAt"`
	ReleasedAt   *string `json:"releasedAt,omitempty"`
}

func (l AutomationDispatcherSessionLease) String() string {
	return fmt.Sprintf(
		"automation dispatcher session (id=%q, board=%q, expires=%q, released=%t)",
		l.SessionID,
		l.Board,
		l.ExpiresAt,
		l.ReleasedAt != nil,
	)
}

func (l AutomationDispatcherSessionLease) GoString() string { return l.String() }

func normalizeAutomationSession(
	board string,
	sessionID string,
	ttl time.Duration,
) (string, string, string, string, error) {
	var err error
	board, err = boundedAutomationText(board, "dispatcher board", maxAutomationBoardBytes, true)
	if err != nil {
		return "", "", "", "", err
	}
	sessionID, err = boundedAutomationText(
		sessionID,
		"dispatcher session ID",
		maxAutomationSessionIDBytes,
		true,
	)
	if err != nil {
		return "", "", "", "", err
	}
	if ttl < MinAutomationSessionTTL || ttl > MaxAutomationSessionTTL {
		return "", "", "", "", fmt.Errorf(
			"dispatcher session TTL must be between %s and %s",
			MinAutomationSessionTTL,
			MaxAutomationSessionTTL,
		)
	}
	current := time.Now().UTC()
	expires := current.Add(ttl)
	if current.Year() < 0 || current.Year() > 9999 ||
		expires.Year() < 0 || expires.Year() > 9999 {
		return "", "", "", "", errors.New(
			"dispatcher session time must fit RFC3339",
		)
	}
	return board,
		sessionID,
		current.Format(automationTimestampLayout),
		expires.Format(automationTimestampLayout),
		nil
}

func scanAutomationDispatcherSession(
	row scanner,
) (AutomationDispatcherSessionLease, error) {
	var lease AutomationDispatcherSessionLease
	var releasedAt sql.NullString
	err := row.Scan(
		&lease.SessionID,
		&lease.Board,
		&lease.leaseToken,
		&lease.RegisteredAt,
		&lease.RenewedAt,
		&lease.ExpiresAt,
		&releasedAt,
	)
	lease.ReleasedAt = stringPointer(releasedAt)
	return lease, err
}

const automationDispatcherSessionColumns = `session_id, board, lease_token,
	registered_at, renewed_at, expires_at, released_at`

func (s *Store) withAutomationAuthorityLock(
	ctx context.Context,
	exclusive bool,
	fn func(*sql.Tx) error,
) (resultErr error) {
	if err := s.requireCoordinationStore(); err != nil {
		return err
	}
	runtime, err := s.automationRuntime()
	if err != nil {
		return err
	}
	if runtime.authorityDB != s.db {
		return errors.New("automation session requires the authority Store")
	}
	lock, err := acquireAutomationFileLock(ctx, runtime.lockPath, exclusive)
	if err != nil {
		return fmt.Errorf("lock automation session: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Close())
	}()
	return s.withWrite(ctx, fn)
}

func (s *Store) RegisterAutomationDispatcherSession(
	ctx context.Context,
	board string,
	sessionID string,
	ttl time.Duration,
) (AutomationDispatcherSessionLease, bool, error) {
	board, sessionID, timestamp, expiresAt, err := normalizeAutomationSession(
		board,
		sessionID,
		ttl,
	)
	if err != nil {
		return AutomationDispatcherSessionLease{}, false, err
	}
	token, err := randomAutomationToken()
	if err != nil {
		return AutomationDispatcherSessionLease{}, false, err
	}
	var lease AutomationDispatcherSessionLease
	acquired := false
	err = s.withAutomationAuthorityLock(ctx, true, func(tx *sql.Tx) error {
		gate, err := readAutomationGate(ctx, tx)
		if err != nil {
			return err
		}
		if gate.Active {
			return &AutomationQuarantinedError{Generation: gate.Generation}
		}
		lease, err = scanAutomationDispatcherSession(tx.QueryRowContext(ctx,
			"SELECT "+automationDispatcherSessionColumns+
				" FROM automation_dispatcher_sessions WHERE session_id = ?",
			sessionID,
		))
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO automation_dispatcher_sessions(
				session_id, board, lease_token, registered_at, renewed_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
				sessionID,
				board,
				token,
				timestamp,
				timestamp,
				expiresAt,
			)
			if err != nil {
				return err
			}
			lease = AutomationDispatcherSessionLease{
				SessionID:    sessionID,
				Board:        board,
				leaseToken:   token,
				RegisteredAt: timestamp,
				RenewedAt:    timestamp,
				ExpiresAt:    expiresAt,
			}
			acquired = true
			return nil
		}
		if err != nil {
			return err
		}
		// Session IDs identify one process lifetime and are never recycled.
		// Keeping the row also keeps its generation acknowledgements auditable.
		lease.leaseToken = ""
		return nil
	})
	return lease, acquired, err
}

func validateAutomationSessionLease(
	lease AutomationDispatcherSessionLease,
) error {
	if strings.TrimSpace(lease.SessionID) == "" ||
		strings.TrimSpace(lease.Board) == "" ||
		strings.TrimSpace(lease.leaseToken) == "" {
		return errors.New("exact dispatcher session lease is required")
	}
	return nil
}

func (s *Store) RenewAutomationDispatcherSession(
	ctx context.Context,
	lease AutomationDispatcherSessionLease,
	ttl time.Duration,
) (AutomationDispatcherSessionLease, error) {
	if err := validateAutomationSessionLease(lease); err != nil {
		return AutomationDispatcherSessionLease{}, err
	}
	board, sessionID, timestamp, expiresAt, err := normalizeAutomationSession(
		lease.Board,
		lease.SessionID,
		ttl,
	)
	if err != nil {
		return AutomationDispatcherSessionLease{}, err
	}
	var renewed AutomationDispatcherSessionLease
	err = s.withAutomationAuthorityLock(ctx, true, func(tx *sql.Tx) error {
		currentLease, err := scanAutomationDispatcherSession(tx.QueryRowContext(ctx,
			"SELECT "+automationDispatcherSessionColumns+
				" FROM automation_dispatcher_sessions WHERE session_id = ?",
			sessionID,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAutomationHostNotIdle
		}
		if err != nil {
			return err
		}
		if currentLease.leaseToken != lease.leaseToken ||
			currentLease.Board != board ||
			currentLease.ReleasedAt != nil ||
			currentLease.ExpiresAt <= timestamp {
			return ErrAutomationHostNotIdle
		}
		if _, err := tx.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
			SET renewed_at = ?, expires_at = ?
			WHERE session_id = ? AND board = ? AND lease_token = ?
				AND expires_at > ?`,
			timestamp,
			expiresAt,
			sessionID,
			board,
			lease.leaseToken,
			timestamp,
		); err != nil {
			return err
		}
		renewed = currentLease
		renewed.RenewedAt = timestamp
		renewed.ExpiresAt = expiresAt
		return nil
	})
	return renewed, err
}

func (s *Store) ReleaseAutomationDispatcherSession(
	ctx context.Context,
	lease AutomationDispatcherSessionLease,
) (bool, error) {
	if err := validateAutomationSessionLease(lease); err != nil {
		return false, err
	}
	released := false
	timestamp := time.Now().UTC().Format(automationTimestampLayout)
	err := s.withAutomationAuthorityLock(ctx, true, func(tx *sql.Tx) error {
		currentLease, err := scanAutomationDispatcherSession(tx.QueryRowContext(ctx,
			"SELECT "+automationDispatcherSessionColumns+
				" FROM automation_dispatcher_sessions WHERE session_id = ?",
			lease.SessionID,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if currentLease.Board != lease.Board ||
			currentLease.leaseToken != lease.leaseToken {
			return nil
		}
		if currentLease.ReleasedAt != nil {
			released = true
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE automation_dispatcher_sessions
			SET released_at = ?, expires_at = CASE
				WHEN expires_at > ? THEN ? ELSE expires_at END
			WHERE session_id = ? AND board = ? AND lease_token = ?
				AND released_at IS NULL`,
			timestamp,
			timestamp,
			timestamp,
			lease.SessionID,
			lease.Board,
			lease.leaseToken,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		released = err == nil && changed == 1
		return err
	})
	return released, err
}

func (s *Store) AcknowledgeAutomationQuarantine(
	ctx context.Context,
	lease AutomationDispatcherSessionLease,
	generation int64,
) error {
	if err := validateAutomationSessionLease(lease); err != nil {
		return err
	}
	if generation < 1 {
		return errors.New("automation quarantine generation must be positive")
	}
	current := time.Now().UTC()
	if current.Year() < 0 || current.Year() > 9999 {
		return errors.New("dispatcher acknowledgement time must fit RFC3339")
	}
	timestamp := current.Format(automationTimestampLayout)
	return s.withAutomationAuthorityLock(ctx, true, func(tx *sql.Tx) error {
		record, err := readAutomationGate(ctx, tx)
		if err != nil {
			return err
		}
		if !record.Active || record.Generation != generation {
			return &AutomationGateConflictError{
				ExpectedGeneration: generation,
				ActualGeneration:   record.Generation,
				Active:             record.Active,
			}
		}
		currentLease, err := scanAutomationDispatcherSession(tx.QueryRowContext(ctx,
			"SELECT "+automationDispatcherSessionColumns+
				" FROM automation_dispatcher_sessions WHERE session_id = ?",
			lease.SessionID,
		))
		if err != nil {
			return ErrAutomationHostNotIdle
		}
		if currentLease.Board != lease.Board ||
			currentLease.leaseToken != lease.leaseToken ||
			currentLease.ReleasedAt != nil ||
			currentLease.ExpiresAt <= timestamp {
			return ErrAutomationHostNotIdle
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_dispatcher_acks(
			session_id, generation, acknowledged_at
		) VALUES (?, ?, ?)
		ON CONFLICT(session_id, generation) DO NOTHING`,
			lease.SessionID,
			generation,
			timestamp,
		)
		return err
	})
}

func (s *Store) ClaimTaskAutomated(
	ctx context.Context,
	permit *AutomationPermit,
	input ClaimOptions,
) (*model.ClaimedTask, error) {
	var claimed *model.ClaimedTask
	board := normalizedBoard(input.Board, s.board)
	err := s.withAutomationPermitForBoard(ctx, permit, board, func() error {
		var claimErr error
		claimed, claimErr = s.ClaimTask(ctx, input)
		return claimErr
	})
	return claimed, err
}

func (s *Store) ClaimPublicationAutomated(
	ctx context.Context,
	permit *AutomationPermit,
	id string,
	input ClaimPublicationInput,
) (model.Publication, bool, error) {
	var publication model.Publication
	var claimed bool
	err := s.withAutomationPermitForBoard(ctx, permit, s.board, func() error {
		var claimErr error
		publication, claimed, claimErr = s.ClaimPublication(ctx, id, input)
		return claimErr
	})
	return publication, claimed, err
}

func (s *Store) AcquireGlobalWorkspaceLeaseAutomated(
	ctx context.Context,
	permit *AutomationPermit,
	board string,
	runID string,
	path string,
) (GlobalWorkspaceLease, bool, error) {
	var lease GlobalWorkspaceLease
	var acquired bool
	err := s.withAutomationPermitForBoard(ctx, permit, board, func() error {
		var acquireErr error
		lease, acquired, acquireErr = s.AcquireGlobalWorkspaceLease(
			ctx,
			board,
			runID,
			path,
		)
		return acquireErr
	})
	return lease, acquired, err
}

func (s *Store) RenewManagedRunLeaseAutomated(
	ctx context.Context,
	permit *AutomationPermit,
	scope RunScope,
) (model.Run, error) {
	var run model.Run
	err := s.withAutomationPermitForBoard(ctx, permit, s.board, func() error {
		var renewErr error
		run, renewErr = s.RenewManagedRunLease(ctx, scope)
		return renewErr
	})
	return run, err
}
