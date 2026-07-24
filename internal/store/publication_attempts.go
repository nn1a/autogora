package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
)

var (
	ErrPublicationAttemptNotFound       = errors.New("publication attempt not found")
	ErrPublicationAttemptPermitClosed   = errors.New("publication attempt permit is closed")
	ErrPublicationAttemptScope          = errors.New("publication attempt permit does not cover this attempt")
	ErrPublicationAttemptResultConflict = errors.New("publication attempt already has a different result")
)

type PublicationAttemptOutcome string

const (
	PublicationAttemptPublished PublicationAttemptOutcome = "published"
	PublicationAttemptFailed    PublicationAttemptOutcome = "failed"
	PublicationAttemptUnknown   PublicationAttemptOutcome = "unknown"
)

type PublicationExecutorStatus string

const (
	PublicationExecutorPublished        PublicationExecutorStatus = "published"
	PublicationExecutorAlreadyPublished PublicationExecutorStatus = "already_published"
	PublicationExecutorFailed           PublicationExecutorStatus = "failed"
	PublicationExecutorManualRequired   PublicationExecutorStatus = "manual_required"
	PublicationExecutorUnknown          PublicationExecutorStatus = "unknown"
)

type PublicationAttemptErrorKind string

const (
	PublicationErrorInvalidInput          PublicationAttemptErrorKind = "invalid_input"
	PublicationErrorManualMode            PublicationAttemptErrorKind = "manual_mode"
	PublicationErrorRepository            PublicationAttemptErrorKind = "repository"
	PublicationErrorSourceChanged         PublicationAttemptErrorKind = "source_changed"
	PublicationErrorNonFastForward        PublicationAttemptErrorKind = "non_fast_forward"
	PublicationErrorDirtyWorktree         PublicationAttemptErrorKind = "dirty_worktree"
	PublicationErrorRemoteConflict        PublicationAttemptErrorKind = "remote_conflict"
	PublicationErrorCommandTimeout        PublicationAttemptErrorKind = "command_timeout"
	PublicationErrorTeardownUnconfirmed   PublicationAttemptErrorKind = "teardown_unconfirmed"
	PublicationErrorCommandFailed         PublicationAttemptErrorKind = "command_failed"
	PublicationErrorCanceled              PublicationAttemptErrorKind = "canceled"
	PublicationErrorCommandStartBlocked   PublicationAttemptErrorKind = "command_start_blocked"
	PublicationErrorCommandStartUncertain PublicationAttemptErrorKind = "command_start_uncertain"
	PublicationErrorInternal              PublicationAttemptErrorKind = "internal"
	PublicationErrorUnknown               PublicationAttemptErrorKind = "unknown"
)

// PublicationAttemptIntent is credential-free evidence that an exact
// publication claim crossed the durable automatic execution boundary.
// ExecutionProvenanceFingerprint binds the durable task, repository path,
// worktree path, and canonical source snapshot without exposing those inputs.
// Command-level repository and remote endpoint identity require separate
// execution receipts.
type PublicationAttemptIntent struct {
	ID                             string                `json:"id"`
	Board                          string                `json:"board"`
	PublicationID                  string                `json:"publicationId"`
	SourceKey                      string                `json:"sourceKey"`
	ChangeSetID                    string                `json:"changeSetId"`
	Mode                           model.PublicationMode `json:"mode"`
	TargetBranch                   string                `json:"targetBranch"`
	Remote                         string                `json:"remote"`
	BaseCommit                     string                `json:"baseCommit"`
	HeadCommit                     string                `json:"headCommit"`
	DurableRef                     string                `json:"durableRef"`
	ExecutionProvenanceFingerprint string                `json:"executionProvenanceFingerprint"`
	EffectFingerprint              string                `json:"effectFingerprint"`
	ClaimEpoch                     int64                 `json:"claimEpoch"`
	PublicationUpdatedAt           string                `json:"publicationUpdatedAt"`
	ClaimExpiresAt                 string                `json:"claimExpiresAt"`
	SessionID                      string                `json:"sessionId"`
	GateGeneration                 int64                 `json:"gateGeneration"`
	StartedAt                      string                `json:"startedAt"`
}

// PublicationAttemptResult is an immutable outcome receipt. Unknown means the
// host cannot prove the external effect and deliberately leaves the
// publication in Publishing for exact operator recovery.
type PublicationAttemptResult struct {
	AttemptID            string                       `json:"attemptId"`
	Board                string                       `json:"board"`
	PublicationID        string                       `json:"publicationId"`
	ClaimEpoch           int64                        `json:"claimEpoch"`
	Outcome              PublicationAttemptOutcome    `json:"outcome"`
	ExecutorStatus       PublicationExecutorStatus    `json:"executorStatus"`
	ErrorKind            *PublicationAttemptErrorKind `json:"errorKind,omitempty"`
	URL                  *string                      `json:"url,omitempty"`
	Error                *string                      `json:"error,omitempty"`
	PublicationUpdatedAt string                       `json:"publicationUpdatedAt"`
	RecordedAt           string                       `json:"recordedAt"`
}

type PublicationAttemptRecord struct {
	Intent          PublicationAttemptIntent    `json:"intent"`
	Result          *PublicationAttemptResult   `json:"result,omitempty"`
	RecoveryReceipt *PublicationRecoveryReceipt `json:"recoveryReceipt,omitempty"`
}

type PublicationAttemptFilter struct {
	AfterStartedAt string
	AfterID        string
	Limit          int
}

type PublicationAttemptResultInput struct {
	Outcome        PublicationAttemptOutcome
	ExecutorStatus PublicationExecutorStatus
	ErrorKind      PublicationAttemptErrorKind
	URL            *string
	Error          string
}

type publicationAttemptPermitState struct {
	mu       sync.Mutex
	finished bool

	intent  PublicationAttemptIntent
	effects map[string]*publicationEffectPermitState

	claimToken string

	authorityPath string
	lockPath      string
	gateToken     string
	sessionBoard  string
	sessionToken  string
}

// PublicationAttemptPermit is an in-memory capability for one exact durable
// attempt. Copies share revocation state. Its credentials are private and
// never included in formatting or JSON.
type PublicationAttemptPermit struct {
	state *publicationAttemptPermitState
}

func (p *PublicationAttemptPermit) String() string {
	if p == nil || p.state == nil {
		return "publication attempt permit (nil)"
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if p.state.finished {
		return "publication attempt permit (finished)"
	}
	return "publication attempt permit (open)"
}

func (p *PublicationAttemptPermit) GoString() string { return p.String() }

func (p *PublicationAttemptPermit) MarshalJSON() ([]byte, error) {
	return []byte(`{}`), nil
}

// Intent returns a credential-free copy suitable for quarantine evidence and
// audit logging.
func (p *PublicationAttemptPermit) Intent() PublicationAttemptIntent {
	if p == nil || p.state == nil {
		return PublicationAttemptIntent{}
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	return p.state.intent
}

const publicationAttemptIntentColumns = `i.id, i.board, i.publication_id,
	i.source_key, i.change_set_id, i.mode, i.target_branch, i.remote,
	i.base_commit, i.head_commit, i.durable_ref,
	i.execution_provenance_fingerprint, i.effect_fingerprint, i.claim_epoch,
	i.publication_updated_at, i.claim_expires_at, i.session_id,
	i.gate_generation, i.started_at`

const publicationAttemptResultColumns = `r.attempt_id, r.board,
	r.publication_id, r.claim_epoch, r.outcome, r.executor_status, r.error_kind,
	r.result_url, r.error, r.publication_updated_at, r.recorded_at`

func scanPublicationAttemptIntent(row scanner) (PublicationAttemptIntent, error) {
	var value PublicationAttemptIntent
	var executionProvenanceFingerprint sql.NullString
	err := row.Scan(
		&value.ID,
		&value.Board,
		&value.PublicationID,
		&value.SourceKey,
		&value.ChangeSetID,
		&value.Mode,
		&value.TargetBranch,
		&value.Remote,
		&value.BaseCommit,
		&value.HeadCommit,
		&value.DurableRef,
		&executionProvenanceFingerprint,
		&value.EffectFingerprint,
		&value.ClaimEpoch,
		&value.PublicationUpdatedAt,
		&value.ClaimExpiresAt,
		&value.SessionID,
		&value.GateGeneration,
		&value.StartedAt,
	)
	if err != nil {
		return PublicationAttemptIntent{}, err
	}
	if !executionProvenanceFingerprint.Valid ||
		executionProvenanceFingerprint.String == "" {
		return PublicationAttemptIntent{}, errors.New(
			"publication attempt intent lacks v30 execution provenance",
		)
	}
	value.ExecutionProvenanceFingerprint =
		executionProvenanceFingerprint.String
	if !validPublicationAttemptFingerprint(
		value.ExecutionProvenanceFingerprint,
	) {
		return PublicationAttemptIntent{}, errors.New(
			"publication attempt intent has an invalid execution provenance fingerprint",
		)
	}
	if value.Mode != model.PublicationModeLocalFF &&
		value.Mode != model.PublicationModePullRequest {
		return PublicationAttemptIntent{}, errors.New(
			"publication attempt intent has an invalid mode",
		)
	}
	if value.SourceKey != publicationAttemptSourceKey(
		value.Board,
		value.PublicationID,
		value.PublicationUpdatedAt,
		value.ClaimEpoch,
	) {
		return PublicationAttemptIntent{}, errors.New(
			"publication attempt intent has an invalid source key",
		)
	}
	if value.EffectFingerprint != publicationEffectFingerprint(value) {
		return PublicationAttemptIntent{}, errors.New(
			"publication attempt intent has an invalid effect fingerprint",
		)
	}
	if _, err := normalizePublicationAttemptLedgerTimestamp(
		value.StartedAt,
		"publication attempt startedAt",
	); err != nil {
		return PublicationAttemptIntent{}, err
	}
	return value, err
}

func publicationAttemptSourceKey(
	board string,
	publicationID string,
	publicationUpdatedAt string,
	claimEpoch int64,
) string {
	canonical := strings.Join([]string{
		board,
		"publication",
		publicationID,
		publicationUpdatedAt,
		strconv.FormatInt(claimEpoch, 10),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func publicationEffectFingerprint(intent PublicationAttemptIntent) string {
	canonical := strings.Join([]string{
		"publication-effect-v2",
		intent.Board,
		intent.PublicationID,
		intent.ChangeSetID,
		string(intent.Mode),
		intent.TargetBranch,
		intent.Remote,
		intent.BaseCommit,
		intent.HeadCommit,
		intent.DurableRef,
		intent.ExecutionProvenanceFingerprint,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func validPublicationAttemptFingerprint(value string) bool {
	if len(value) != sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalPublicationSourceSnapshot(
	raw json.RawMessage,
) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New(
			"publication source snapshot cannot be empty",
		)
	}
	if len(raw) > MaxPublicationSourceSnapshotBytes {
		return nil, fmt.Errorf(
			"publication source snapshot must be at most %d bytes",
			MaxPublicationSourceSnapshotBytes,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf(
			"publication source snapshot must be valid JSON: %w",
			err,
		)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, errors.New(
			"publication source snapshot must be a JSON object",
		)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New(
				"publication source snapshot contains trailing JSON",
			)
		}
		return nil, fmt.Errorf(
			"publication source snapshot contains trailing data: %w",
			err,
		)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf(
			"canonicalize publication source snapshot: %w",
			err,
		)
	}
	return canonical, nil
}

func publicationExecutionProvenanceFingerprint(
	value model.Publication,
) (string, error) {
	taskID, err := validRecordID(
		value.TaskID,
		"publication attempt task id",
	)
	if err != nil || taskID == "" {
		if err != nil {
			return "", err
		}
		return "", errors.New("publication attempt task id cannot be empty")
	}
	if err := validatePublicationAttemptExecutionPath(
		value.RepositoryPath,
		"publication attempt repository path",
	); err != nil {
		return "", err
	}
	if err := validatePublicationAttemptExecutionPath(
		value.WorktreePath,
		"publication attempt worktree path",
	); err != nil {
		return "", err
	}
	sourceSnapshot, err := canonicalPublicationSourceSnapshot(
		value.SourceSnapshot,
	)
	if err != nil {
		return "", err
	}
	sourceSum := sha256.Sum256(sourceSnapshot)
	canonical := strings.Join([]string{
		"publication-execution-provenance-v1",
		taskID,
		value.RepositoryPath,
		value.WorktreePath,
		hex.EncodeToString(sourceSum[:]),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func validatePublicationAttemptExecutionPath(
	value string,
	field string,
) error {
	if value == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s cannot be empty", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s cannot contain NUL", field)
	}
	if len(value) > 4096 {
		return fmt.Errorf("%s must be at most 4096 bytes", field)
	}
	return nil
}

func normalizePublicationAttemptLedgerTimestamp(
	value string,
	field string,
) (string, error) {
	original := value
	value, err := normalizedPublicationText(value, field, 128, true)
	if err != nil {
		return "", err
	}
	if value != original {
		return "", fmt.Errorf(
			"%s is not a fixed-width UTC ledger timestamp",
			field,
		)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	if parsed.Year() < 0 || parsed.Year() > 9999 ||
		value != parsed.UTC().Format(publicationTimestampLayout) {
		return "", fmt.Errorf(
			"%s is not a fixed-width UTC ledger timestamp",
			field,
		)
	}
	return value, nil
}

func publicationAttemptLedgerTimestamp(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf(
			"derive publication attempt ledger timestamp: %w",
			err,
		)
	}
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return "", errors.New(
			"publication attempt ledger timestamp must fit RFC3339",
		)
	}
	return parsed.UTC().Format(publicationTimestampLayout), nil
}

func scanPublicationAttemptResult(row scanner) (PublicationAttemptResult, error) {
	var value PublicationAttemptResult
	var rawErrorKind, rawURL, resultError sql.NullString
	err := row.Scan(
		&value.AttemptID,
		&value.Board,
		&value.PublicationID,
		&value.ClaimEpoch,
		&value.Outcome,
		&value.ExecutorStatus,
		&rawErrorKind,
		&rawURL,
		&resultError,
		&value.PublicationUpdatedAt,
		&value.RecordedAt,
	)
	value.URL = stringPointer(rawURL)
	value.Error = stringPointer(resultError)
	if rawErrorKind.Valid {
		kind := PublicationAttemptErrorKind(rawErrorKind.String)
		value.ErrorKind = &kind
	}
	if err != nil {
		return PublicationAttemptResult{}, err
	}
	if _, err := normalizePublicationAttemptLedgerTimestamp(
		value.RecordedAt,
		"publication attempt result recordedAt",
	); err != nil {
		return PublicationAttemptResult{}, err
	}
	input := PublicationAttemptResultInput{
		Outcome:        value.Outcome,
		ExecutorStatus: value.ExecutorStatus,
		URL:            value.URL,
	}
	if value.ErrorKind != nil {
		input.ErrorKind = *value.ErrorKind
	}
	if value.Error != nil {
		input.Error = *value.Error
	}
	normalized, err := normalizePublicationAttemptResult(input)
	if err != nil || !samePublicationAttemptResult(value, normalized) {
		if err != nil {
			return PublicationAttemptResult{}, fmt.Errorf(
				"invalid publication attempt result: %w",
				err,
			)
		}
		return PublicationAttemptResult{}, errors.New(
			"publication attempt result is not stored canonically",
		)
	}
	return value, nil
}

func publicationAttemptIntent(
	ctx context.Context,
	q querier,
	attemptID string,
) (PublicationAttemptIntent, error) {
	value, err := scanPublicationAttemptIntent(q.QueryRowContext(
		ctx,
		`SELECT `+publicationAttemptIntentColumns+`
		 FROM publication_attempt_intents i WHERE i.id = ?`,
		strings.TrimSpace(attemptID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationAttemptIntent{}, fmt.Errorf(
			"%w: %s",
			ErrPublicationAttemptNotFound,
			strings.TrimSpace(attemptID),
		)
	}
	return value, err
}

func publicationAttemptResult(
	ctx context.Context,
	q querier,
	attemptID string,
) (PublicationAttemptResult, bool, error) {
	value, err := scanPublicationAttemptResult(q.QueryRowContext(
		ctx,
		`SELECT `+publicationAttemptResultColumns+`
		 FROM publication_attempt_results r WHERE r.attempt_id = ?`,
		strings.TrimSpace(attemptID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationAttemptResult{}, false, nil
	}
	return value, err == nil, err
}

func samePublicationAttemptIntent(
	left PublicationAttemptIntent,
	right PublicationAttemptIntent,
) bool {
	return left == right
}

func publicationMatchesAttemptEffect(
	value model.Publication,
	intent PublicationAttemptIntent,
) bool {
	executionProvenanceFingerprint, err :=
		publicationExecutionProvenanceFingerprint(value)
	if err != nil {
		return false
	}
	return value.Board == intent.Board &&
		value.ID == intent.PublicationID &&
		value.ChangeSetID == intent.ChangeSetID &&
		value.Mode == intent.Mode &&
		value.TargetBranch == intent.TargetBranch &&
		value.Remote == intent.Remote &&
		value.BaseCommit == intent.BaseCommit &&
		value.HeadCommit == intent.HeadCommit &&
		value.DurableRef == intent.DurableRef &&
		executionProvenanceFingerprint ==
			intent.ExecutionProvenanceFingerprint &&
		publicationEffectFingerprint(intent) == intent.EffectFingerprint
}

func samePublicationAttemptResult(
	value PublicationAttemptResult,
	input PublicationAttemptResultInput,
) bool {
	if value.Outcome != input.Outcome ||
		value.ExecutorStatus != input.ExecutorStatus {
		return false
	}
	switch {
	case value.ErrorKind == nil:
		if input.ErrorKind != "" {
			return false
		}
	case *value.ErrorKind != input.ErrorKind:
		return false
	}
	if !sameOptionalString(value.URL, input.URL) {
		return false
	}
	switch {
	case value.Error == nil:
		return input.Error == ""
	default:
		return *value.Error == input.Error
	}
}

func normalizePublicationAttemptResult(
	input PublicationAttemptResultInput,
) (PublicationAttemptResultInput, error) {
	rawURL, err := normalizePublicationURL(input.URL)
	if err != nil {
		return PublicationAttemptResultInput{}, err
	}
	input.URL = rawURL
	switch input.Outcome {
	case PublicationAttemptPublished:
		if input.ExecutorStatus != PublicationExecutorPublished &&
			input.ExecutorStatus != PublicationExecutorAlreadyPublished {
			return PublicationAttemptResultInput{}, errors.New(
				"published publication attempt requires a published executor status",
			)
		}
		if input.ErrorKind != "" {
			return PublicationAttemptResultInput{}, errors.New(
				"published publication attempt cannot include an error kind",
			)
		}
		if strings.TrimSpace(input.Error) != "" {
			return PublicationAttemptResultInput{}, errors.New(
				"published publication attempt cannot include an error",
			)
		}
		input.Error = ""
	case PublicationAttemptFailed:
		if input.ExecutorStatus != PublicationExecutorFailed &&
			input.ExecutorStatus != PublicationExecutorManualRequired {
			return PublicationAttemptResultInput{}, errors.New(
				"failed publication attempt requires a failed executor status",
			)
		}
		if !validPublicationAttemptErrorKind(input.ErrorKind) {
			return PublicationAttemptResultInput{}, fmt.Errorf(
				"invalid publication attempt error kind: %s",
				input.ErrorKind,
			)
		}
		if input.URL != nil {
			return PublicationAttemptResultInput{}, fmt.Errorf(
				"%s publication attempt cannot include a URL",
				input.Outcome,
			)
		}
		input.Error, err = boundedPublicationError(input.Error)
		if err != nil {
			return PublicationAttemptResultInput{}, err
		}
	case PublicationAttemptUnknown:
		if input.ExecutorStatus != PublicationExecutorUnknown {
			return PublicationAttemptResultInput{}, errors.New(
				"unknown publication attempt requires an unknown executor status",
			)
		}
		if !validPublicationAttemptErrorKind(input.ErrorKind) {
			return PublicationAttemptResultInput{}, fmt.Errorf(
				"invalid publication attempt error kind: %s",
				input.ErrorKind,
			)
		}
		if input.URL != nil {
			return PublicationAttemptResultInput{}, fmt.Errorf(
				"%s publication attempt cannot include a URL",
				input.Outcome,
			)
		}
		input.Error, err = boundedPublicationError(input.Error)
		if err != nil {
			return PublicationAttemptResultInput{}, err
		}
	default:
		return PublicationAttemptResultInput{}, fmt.Errorf(
			"invalid publication attempt outcome: %s",
			input.Outcome,
		)
	}
	return input, nil
}

func validPublicationAttemptErrorKind(value PublicationAttemptErrorKind) bool {
	switch value {
	case PublicationErrorInvalidInput,
		PublicationErrorManualMode,
		PublicationErrorRepository,
		PublicationErrorSourceChanged,
		PublicationErrorNonFastForward,
		PublicationErrorDirtyWorktree,
		PublicationErrorRemoteConflict,
		PublicationErrorCommandTimeout,
		PublicationErrorTeardownUnconfirmed,
		PublicationErrorCommandFailed,
		PublicationErrorCanceled,
		PublicationErrorCommandStartBlocked,
		PublicationErrorCommandStartUncertain,
		PublicationErrorInternal,
		PublicationErrorUnknown:
		return true
	default:
		return false
	}
}

func nullablePublicationAttemptErrorKind(value *PublicationAttemptErrorKind) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func normalizePublicationAttemptEffect(
	value model.Publication,
) (model.Publication, error) {
	var err error
	value.TaskID, err = validRecordID(
		value.TaskID,
		"publication attempt task id",
	)
	if err != nil || value.TaskID == "" {
		if err != nil {
			return model.Publication{}, err
		}
		return model.Publication{}, errors.New(
			"publication attempt task id cannot be empty",
		)
	}
	if err := validatePublicationAttemptExecutionPath(
		value.RepositoryPath,
		"publication attempt repository path",
	); err != nil {
		return model.Publication{}, err
	}
	if err := validatePublicationAttemptExecutionPath(
		value.WorktreePath,
		"publication attempt worktree path",
	); err != nil {
		return model.Publication{}, err
	}
	if _, err := canonicalPublicationSourceSnapshot(
		value.SourceSnapshot,
	); err != nil {
		return model.Publication{}, err
	}
	value.ChangeSetID, err = validRecordID(
		value.ChangeSetID,
		"publication attempt change set id",
	)
	if err != nil || value.ChangeSetID == "" {
		if err != nil {
			return model.Publication{}, err
		}
		return model.Publication{}, errors.New(
			"publication attempt change set id cannot be empty",
		)
	}
	value.TargetBranch, err = normalizedPublicationText(
		value.TargetBranch,
		"publication attempt target branch",
		maxPublicationBranchBytes,
		true,
	)
	if err != nil {
		return model.Publication{}, err
	}
	value.Remote, err = normalizedPublicationText(
		value.Remote,
		"publication attempt remote",
		maxPublicationRemoteBytes,
		true,
	)
	if err != nil {
		return model.Publication{}, err
	}
	value.BaseCommit, err = normalizedPublicationText(
		value.BaseCommit,
		"publication attempt base commit",
		1024,
		true,
	)
	if err != nil {
		return model.Publication{}, err
	}
	value.HeadCommit, err = normalizedPublicationText(
		value.HeadCommit,
		"publication attempt head commit",
		1024,
		true,
	)
	if err != nil {
		return model.Publication{}, err
	}
	value.DurableRef, err = normalizedPublicationText(
		value.DurableRef,
		"publication attempt durable ref",
		2048,
		true,
	)
	if err != nil {
		return model.Publication{}, err
	}
	return value, nil
}

// BeginAutomatedPublicationAttempt atomically claims a Pending publication and
// stores its immutable execution intent while an exact live session permit
// holds the global automation coordination lock.
func (s *Store) BeginAutomatedPublicationAttempt(
	ctx context.Context,
	permit *AutomationPermit,
	id string,
	input ClaimPublicationInput,
) (model.Publication, *PublicationAttemptPermit, bool, error) {
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return model.Publication{}, nil, false, err
	}
	if input.TTL < MinPublicationClaimTTL || input.TTL > MaxPublicationClaimTTL {
		return model.Publication{}, nil, false, fmt.Errorf(
			"publication claim TTL must be between %s and %s",
			MinPublicationClaimTTL,
			MaxPublicationClaimTTL,
		)
	}

	var value model.Publication
	var operation *PublicationAttemptPermit
	claimed := false
	err = s.withAutomationPermitForBoard(ctx, permit, board, func() error {
		return s.withWrite(ctx, func(tx *sql.Tx) error {
			current, err := publicationForBoard(ctx, tx, id, board)
			if err != nil {
				return err
			}
			if current.Status == model.PublicationPublishing {
				value = publicPublication(current)
				return nil
			}
			if err := requirePublicationVersion(
				current,
				input.ExpectedUpdatedAt,
			); err != nil {
				return err
			}
			if err := requirePublicationState(
				current,
				model.PublicationPending,
			); err != nil {
				return err
			}
			if current.Mode == model.PublicationModeManual {
				return errors.New(
					"automated publication attempt requires an automatic publication mode",
				)
			}
			current, err = normalizePublicationAttemptEffect(current)
			if err != nil {
				return err
			}
			if current.ClaimEpoch == math.MaxInt64 {
				return errors.New("publication claim epoch is exhausted")
			}
			currentTime, _, err := s.publicationCurrent()
			if err != nil {
				return err
			}
			expires := currentTime.Add(input.TTL)
			if expires.Year() < 0 || expires.Year() > 9999 {
				return errors.New("publication claim expiry must fit RFC3339")
			}
			expiresAt := expires.Format(publicationTimestampLayout)
			claimCredential, err := claimToken()
			if err != nil {
				return fmt.Errorf("generate publication claim token: %w", err)
			}
			timestamp := now()
			startedAt, err := publicationAttemptLedgerTimestamp(timestamp)
			if err != nil {
				return err
			}
			attemptID := newID("pat")
			update, err := tx.ExecContext(ctx, `
				UPDATE publications
				SET status = 'publishing', error = NULL,
					claim_epoch = claim_epoch + 1, claim_token = ?,
					claim_expires_at = ?, updated_at = ?
				WHERE id = ? AND board = ? AND status = 'pending'
					AND updated_at = ? AND claim_epoch = ?
					AND claim_epoch < ? AND claim_token IS NULL
					AND claim_expires_at IS NULL
			`,
				claimCredential,
				expiresAt,
				timestamp,
				current.ID,
				board,
				current.UpdatedAt,
				current.ClaimEpoch,
				int64(math.MaxInt64),
			)
			if err != nil {
				return err
			}
			changed, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return &PublicationUpdateConflictError{
					ID: current.ID, Expected: current.UpdatedAt, Actual: "unknown",
				}
			}
			current.ClaimEpoch++
			executionProvenanceFingerprint, err :=
				publicationExecutionProvenanceFingerprint(current)
			if err != nil {
				return err
			}
			intent := PublicationAttemptIntent{
				ID:                             attemptID,
				Board:                          board,
				PublicationID:                  current.ID,
				ChangeSetID:                    current.ChangeSetID,
				Mode:                           current.Mode,
				TargetBranch:                   current.TargetBranch,
				Remote:                         current.Remote,
				BaseCommit:                     current.BaseCommit,
				HeadCommit:                     current.HeadCommit,
				DurableRef:                     current.DurableRef,
				ExecutionProvenanceFingerprint: executionProvenanceFingerprint,
				ClaimEpoch:                     current.ClaimEpoch,
				PublicationUpdatedAt:           timestamp,
				ClaimExpiresAt:                 expiresAt,
				SessionID:                      permit.sessionID,
				GateGeneration:                 permit.generation,
				StartedAt:                      startedAt,
			}
			intent.SourceKey = publicationAttemptSourceKey(
				intent.Board,
				intent.PublicationID,
				intent.PublicationUpdatedAt,
				intent.ClaimEpoch,
			)
			intent.EffectFingerprint = publicationEffectFingerprint(intent)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_attempt_intents(
					id, board, publication_id, source_key, change_set_id, mode,
					target_branch, remote, base_commit, head_commit, durable_ref,
					execution_provenance_fingerprint, effect_fingerprint,
					claim_epoch, publication_updated_at, claim_expires_at,
					session_id, gate_generation, started_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				intent.ID,
				intent.Board,
				intent.PublicationID,
				intent.SourceKey,
				intent.ChangeSetID,
				intent.Mode,
				intent.TargetBranch,
				intent.Remote,
				intent.BaseCommit,
				intent.HeadCommit,
				intent.DurableRef,
				intent.ExecutionProvenanceFingerprint,
				intent.EffectFingerprint,
				intent.ClaimEpoch,
				intent.PublicationUpdatedAt,
				intent.ClaimExpiresAt,
				intent.SessionID,
				intent.GateGeneration,
				intent.StartedAt,
			); err != nil {
				return fmt.Errorf("record publication attempt intent: %w", err)
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				"publication_claimed",
				map[string]any{
					"publicationId":  current.ID,
					"claimEpoch":     current.ClaimEpoch,
					"claimExpiresAt": expiresAt,
					"attemptId":      attemptID,
				},
				&current.RunID,
			); err != nil {
				return err
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				"publication_attempt_started",
				map[string]any{
					"publicationId":  current.ID,
					"claimEpoch":     current.ClaimEpoch,
					"attemptId":      attemptID,
					"sessionId":      permit.sessionID,
					"gateGeneration": permit.generation,
				},
				&current.RunID,
			); err != nil {
				return err
			}
			operation = &PublicationAttemptPermit{
				state: &publicationAttemptPermitState{
					intent:        intent,
					claimToken:    claimCredential,
					authorityPath: permit.authorityPath,
					lockPath:      permit.lockPath,
					gateToken:     permit.token,
					sessionBoard:  permit.sessionBoard,
					sessionToken:  permit.sessionToken,
				},
			}
			current.Status = model.PublicationPublishing
			current.Error = nil
			current.ClaimToken = claimCredential
			current.ClaimExpiresAt = &expiresAt
			current.UpdatedAt = timestamp
			value = publicPublication(current)
			claimed = true
			return nil
		})
	})
	if err != nil {
		return model.Publication{}, nil, false, err
	}
	return value, operation, claimed, nil
}

func (s *Store) withPublicationAttemptCleanupLock(
	ctx context.Context,
	state *publicationAttemptPermitState,
	run func() error,
) (resultErr error) {
	if state == nil {
		return ErrPublicationAttemptPermitClosed
	}
	runtime, err := s.automationRuntime()
	if err != nil {
		return err
	}
	if runtime.authorityPath != state.authorityPath ||
		runtime.lockPath != state.lockPath {
		return ErrPublicationAttemptScope
	}
	lock, err := acquireAutomationFileLock(ctx, runtime.lockPath, false)
	if err != nil {
		return fmt.Errorf("lock publication attempt cleanup: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Close())
	}()
	return run()
}

func validatePublicationAttemptScope(
	stored PublicationAttemptIntent,
	state *publicationAttemptPermitState,
	board string,
) error {
	if state == nil || state.intent.ID == "" ||
		state.intent.Board != board ||
		state.claimToken == "" && !state.finished ||
		state.gateToken == "" && !state.finished ||
		state.sessionToken == "" && !state.finished ||
		(state.sessionBoard != "*" && state.sessionBoard != board) ||
		!samePublicationAttemptIntent(stored, state.intent) {
		return ErrPublicationAttemptScope
	}
	return nil
}

func clearPublicationAttemptCredentials(state *publicationAttemptPermitState) {
	state.finished = true
	state.claimToken = ""
	state.gateToken = ""
	state.sessionToken = ""
}

func validatePublicationCommandStartCapability(
	automation *AutomationPermit,
	state *publicationAttemptPermitState,
	board string,
) error {
	if automation == nil {
		return ErrAutomationPermitClosed
	}
	if state == nil {
		return ErrPublicationAttemptPermitClosed
	}
	if state.finished {
		return ErrPublicationAttemptPermitClosed
	}
	if state.intent.ID == "" ||
		state.intent.Board != board ||
		state.intent.SessionID == "" ||
		state.intent.GateGeneration < 0 ||
		state.claimToken == "" ||
		state.gateToken == "" ||
		state.sessionToken == "" ||
		state.authorityPath == "" ||
		state.lockPath == "" ||
		automation.authorityPath != state.authorityPath ||
		automation.lockPath != state.lockPath ||
		automation.generation != state.intent.GateGeneration ||
		automation.token != state.gateToken ||
		automation.sessionID != state.intent.SessionID ||
		automation.sessionBoard != state.sessionBoard ||
		automation.sessionToken != state.sessionToken ||
		(state.sessionBoard != "*" && state.sessionBoard != board) {
		return ErrPublicationAttemptScope
	}
	return nil
}

// WithPublicationAttemptCommandStart revalidates one exact automatic
// publication attempt and invokes release at the irreversible process-start
// boundary. The caller supplies a fresh session AutomationPermit for every
// command and closes it after this method returns.
//
// Lock order is the AutomationPermit's already-held authority OS shared lock,
// its mutex, the attempt mutex, then the board's immediate transaction. The
// release callback must only open an already-started process fence; it must not
// call Store methods or close either permit.
func (s *Store) WithPublicationAttemptCommandStart(
	ctx context.Context,
	automation *AutomationPermit,
	attempt *PublicationAttemptPermit,
	release func() (bool, error),
) (released bool, resultErr error) {
	if release == nil {
		return false, errors.New(
			"publication command release callback is required",
		)
	}
	if attempt == nil || attempt.state == nil {
		return false, ErrPublicationAttemptPermitClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	transactionContext := context.WithoutCancel(ctx)
	state := attempt.state
	err := s.withAutomationPermitForBoard(
		ctx,
		automation,
		s.board,
		func() error {
			state.mu.Lock()
			defer state.mu.Unlock()
			board, err := normalizePublicationBoard(
				state.intent.Board,
				s.board,
			)
			if err != nil {
				return ErrPublicationAttemptScope
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validatePublicationCommandStartCapability(
				automation,
				state,
				board,
			); err != nil {
				return err
			}
			return s.withWrite(transactionContext, func(tx *sql.Tx) error {
				storedIntent, err := publicationAttemptIntent(
					transactionContext,
					tx,
					state.intent.ID,
				)
				if err != nil {
					return err
				}
				if err := validatePublicationAttemptScope(
					storedIntent,
					state,
					board,
				); err != nil {
					return err
				}
				if state.finished {
					return ErrPublicationAttemptPermitClosed
				}
				if _, exists, err := publicationAttemptResult(
					transactionContext,
					tx,
					storedIntent.ID,
				); err != nil {
					return err
				} else if exists {
					return ErrPublicationAttemptPermitClosed
				}
				current, err := publicationForBoard(
					transactionContext,
					tx,
					storedIntent.PublicationID,
					board,
				)
				if err != nil {
					return err
				}
				if current.Status != model.PublicationPublishing ||
					current.ClaimEpoch != storedIntent.ClaimEpoch ||
					current.ClaimToken != state.claimToken ||
					current.ClaimExpiresAt == nil {
					return ErrPublicationAttemptScope
				}
				if current.UpdatedAt != storedIntent.PublicationUpdatedAt ||
					*current.ClaimExpiresAt != storedIntent.ClaimExpiresAt ||
					!publicationMatchesAttemptEffect(current, storedIntent) {
					return ErrPublicationAttemptScope
				}
				// Session expiry advances without taking the authority lock.
				// Keep the exact stored expiry so it can be sampled again
				// after every other validation that may wait.
				sessionExpiresAt, err :=
					s.validateAutomationPermitLockedWithExpiry(
						transactionContext,
						automation,
					)
				if err != nil {
					return err
				}
				currentTime, _, err := s.publicationCurrent()
				if err != nil {
					return err
				}
				if err := requireLivePublicationClaim(
					current,
					state.claimToken,
					storedIntent.ClaimEpoch,
					currentTime,
				); err != nil {
					return err
				}
				if err := validateAutomationSessionExpiry(
					sessionExpiresAt,
					time.Now().UTC(),
				); err != nil {
					return err
				}
				// This is the caller-cancellation linearization point. If
				// cancellation wins, the process fence stays closed. If
				// release wins, its result is authoritative and the detached
				// transaction keeps the write barrier until release returns.
				if err := ctx.Err(); err != nil {
					return err
				}
				released, err = release()
				return err
			})
		},
	)
	return released, err
}

// FinishAutomatedPublicationAttempt stores an immutable exact result. Known
// results and the terminal publication transition commit together. It accepts
// a matching result after claim expiry because cleanup must not turn a known
// external outcome into uncertainty. It still takes the authority's shared OS
// lock so quarantine source validation cannot race the terminal transition.
func (s *Store) FinishAutomatedPublicationAttempt(
	ctx context.Context,
	permit *PublicationAttemptPermit,
	input PublicationAttemptResultInput,
) (PublicationAttemptResult, error) {
	input, err := normalizePublicationAttemptResult(input)
	if err != nil {
		return PublicationAttemptResult{}, err
	}
	if permit == nil || permit.state == nil {
		return PublicationAttemptResult{}, ErrPublicationAttemptPermitClosed
	}
	state := permit.state
	var result PublicationAttemptResult
	err = s.withPublicationAttemptCleanupLock(ctx, state, func() error {
		// Global lock order is authority OS lock -> operation state -> board
		// transaction. Command-start validation uses the same order so an
		// exclusive quarantine waiter cannot form a three-way lock cycle.
		state.mu.Lock()
		defer state.mu.Unlock()
		board, err := normalizePublicationBoard(state.intent.Board, s.board)
		if err != nil {
			return ErrPublicationAttemptScope
		}
		finishErr := s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
			storedIntent, err := publicationAttemptIntent(
				ctx,
				tx,
				state.intent.ID,
			)
			if err != nil {
				return err
			}
			if err := validatePublicationAttemptScope(
				storedIntent,
				state,
				board,
			); err != nil {
				return err
			}
			existing, exists, err := publicationAttemptResult(
				ctx,
				tx,
				storedIntent.ID,
			)
			if err != nil {
				return err
			}
			if exists {
				if existing.Board != storedIntent.Board ||
					existing.PublicationID != storedIntent.PublicationID ||
					existing.ClaimEpoch != storedIntent.ClaimEpoch {
					return ErrPublicationAttemptScope
				}
				if !samePublicationAttemptResult(existing, input) {
					return ErrPublicationAttemptResultConflict
				}
				result = existing
				return nil
			}
			if state.finished {
				return ErrPublicationAttemptResultConflict
			}
			if err := validatePublicationAttemptEffectOutcomes(
				ctx,
				tx,
				storedIntent.ID,
				input.Outcome,
			); err != nil {
				return err
			}

			current, err := publicationForBoard(
				ctx,
				tx,
				storedIntent.PublicationID,
				board,
			)
			if err != nil {
				return err
			}
			if current.Status != model.PublicationPublishing ||
				current.ClaimEpoch != storedIntent.ClaimEpoch ||
				current.UpdatedAt != storedIntent.PublicationUpdatedAt ||
				current.ClaimToken != state.claimToken ||
				current.ClaimExpiresAt == nil ||
				*current.ClaimExpiresAt != storedIntent.ClaimExpiresAt ||
				!publicationMatchesAttemptEffect(current, storedIntent) {
				return ErrPublicationAttemptScope
			}

			resultTimestamp := now()
			recordedAt, err := publicationAttemptLedgerTimestamp(
				resultTimestamp,
			)
			if err != nil {
				return err
			}
			resultUpdatedAt := current.UpdatedAt
			switch input.Outcome {
			case PublicationAttemptPublished:
				update, err := tx.ExecContext(ctx, `
					UPDATE publications
					SET status = 'published', url = ?, error = NULL,
						claim_token = NULL, claim_expires_at = NULL,
						published_at = ?, updated_at = ?
					WHERE id = ? AND board = ? AND status = 'publishing'
						AND updated_at = ? AND claim_epoch = ?
						AND claim_token = ? AND claim_expires_at = ?
				`,
					nullableString(input.URL),
					resultTimestamp,
					resultTimestamp,
					current.ID,
					board,
					current.UpdatedAt,
					current.ClaimEpoch,
					current.ClaimToken,
					*current.ClaimExpiresAt,
				)
				if err != nil {
					return err
				}
				changed, err := update.RowsAffected()
				if err != nil {
					return err
				}
				if changed != 1 {
					return ErrPublicationAttemptScope
				}
				resultUpdatedAt = resultTimestamp
			case PublicationAttemptFailed:
				update, err := tx.ExecContext(ctx, `
					UPDATE publications
					SET status = 'failed', url = NULL, error = ?,
						claim_token = NULL, claim_expires_at = NULL,
						updated_at = ?
					WHERE id = ? AND board = ? AND status = 'publishing'
						AND updated_at = ? AND claim_epoch = ?
						AND claim_token = ? AND claim_expires_at = ?
				`,
					input.Error,
					resultTimestamp,
					current.ID,
					board,
					current.UpdatedAt,
					current.ClaimEpoch,
					current.ClaimToken,
					*current.ClaimExpiresAt,
				)
				if err != nil {
					return err
				}
				changed, err := update.RowsAffected()
				if err != nil {
					return err
				}
				if changed != 1 {
					return ErrPublicationAttemptScope
				}
				resultUpdatedAt = resultTimestamp
			case PublicationAttemptUnknown:
				// Preserve the exact Publishing tuple for quarantine recovery.
			default:
				return errors.New("unreachable publication attempt outcome")
			}

			result = PublicationAttemptResult{
				AttemptID:            storedIntent.ID,
				Board:                storedIntent.Board,
				PublicationID:        storedIntent.PublicationID,
				ClaimEpoch:           storedIntent.ClaimEpoch,
				Outcome:              input.Outcome,
				ExecutorStatus:       input.ExecutorStatus,
				URL:                  clonePublicationRecoveryString(input.URL),
				PublicationUpdatedAt: resultUpdatedAt,
				RecordedAt:           recordedAt,
			}
			if input.ErrorKind != "" {
				errorKind := input.ErrorKind
				result.ErrorKind = &errorKind
			}
			if input.Error != "" {
				result.Error = &input.Error
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_attempt_results(
					attempt_id, board, publication_id, claim_epoch, outcome,
					executor_status, error_kind, result_url, error,
					publication_updated_at, recorded_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				result.AttemptID,
				result.Board,
				result.PublicationID,
				result.ClaimEpoch,
				result.Outcome,
				result.ExecutorStatus,
				nullablePublicationAttemptErrorKind(result.ErrorKind),
				nullableString(result.URL),
				nullableString(result.Error),
				result.PublicationUpdatedAt,
				result.RecordedAt,
			); err != nil {
				return fmt.Errorf("record publication attempt result: %w", err)
			}

			eventKind := "publication_attempt_unknown"
			payload := map[string]any{
				"publicationId":  current.ID,
				"attemptId":      storedIntent.ID,
				"claimEpoch":     current.ClaimEpoch,
				"outcome":        input.Outcome,
				"executorStatus": input.ExecutorStatus,
			}
			if input.ErrorKind != "" {
				payload["errorKind"] = input.ErrorKind
			}
			switch input.Outcome {
			case PublicationAttemptPublished:
				eventKind = "publication_completed"
				payload["mode"] = current.Mode
				payload["url"] = input.URL
			case PublicationAttemptFailed:
				eventKind = "publication_failed"
				payload["error"] = input.Error
			case PublicationAttemptUnknown:
				payload["error"] = input.Error
			}
			if err := appendEvent(
				ctx,
				tx,
				current.TaskID,
				eventKind,
				payload,
				&current.RunID,
			); err != nil {
				return err
			}
			return nil
		})
		if finishErr != nil {
			return finishErr
		}
		clearPublicationAttemptCredentials(state)
		return nil
	})
	if err != nil {
		return PublicationAttemptResult{}, err
	}
	return result, nil
}

func (s *Store) GetPublicationAttempt(
	ctx context.Context,
	attemptID string,
) (PublicationAttemptRecord, error) {
	attemptID, err := validRecordID(attemptID, "publication attempt id")
	if err != nil {
		return PublicationAttemptRecord{}, err
	}
	if attemptID == "" {
		return PublicationAttemptRecord{}, errors.New(
			"publication attempt id cannot be empty",
		)
	}
	intent, err := publicationAttemptIntent(ctx, s.db, attemptID)
	if err != nil {
		return PublicationAttemptRecord{}, err
	}
	board, err := normalizePublicationBoard(intent.Board, s.board)
	if err != nil || board != intent.Board {
		return PublicationAttemptRecord{}, fmt.Errorf(
			"%w: %s",
			ErrPublicationAttemptNotFound,
			attemptID,
		)
	}
	result, exists, err := publicationAttemptResult(ctx, s.db, attemptID)
	if err != nil {
		return PublicationAttemptRecord{}, err
	}
	record := PublicationAttemptRecord{Intent: intent}
	if exists {
		record.Result = &result
	}
	recovery, err := publicationRecoveryReceiptForSource(
		ctx,
		s.db,
		intent.SourceKey,
	)
	switch {
	case err == nil:
		if recovery.PublicationID != intent.PublicationID ||
			recovery.ObservedUpdatedAt != intent.PublicationUpdatedAt ||
			recovery.ObservedClaimEpoch != intent.ClaimEpoch {
			return PublicationAttemptRecord{}, errors.New(
				"publication recovery receipt does not match its attempt",
			)
		}
		record.RecoveryReceipt = &recovery
	case errors.Is(err, ErrPublicationRecoveryReceiptNotFound):
	default:
		return PublicationAttemptRecord{}, err
	}
	return record, nil
}

func (s *Store) listPublicationAttemptCandidatePage(
	ctx context.Context,
	board string,
	afterStartedAt string,
	afterID string,
	limit int,
) ([]PublicationAttemptRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+publicationAttemptIntentColumns+`,
			r.attempt_id, r.board, r.publication_id, r.claim_epoch,
			r.outcome, r.executor_status, r.error_kind, r.result_url, r.error,
			r.publication_updated_at, r.recorded_at
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
	`, board, afterStartedAt, afterStartedAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := make([]PublicationAttemptRecord, 0, limit)
	for rows.Next() {
		var record PublicationAttemptRecord
		var result PublicationAttemptResult
		var executionProvenanceFingerprint sql.NullString
		var resultAttemptID, resultBoard, resultPublicationID sql.NullString
		var resultOutcome, resultExecutorStatus, resultErrorKind sql.NullString
		var resultURL, resultError sql.NullString
		var resultUpdatedAt, resultRecordedAt sql.NullString
		var resultClaimEpoch sql.NullInt64
		if err := rows.Scan(
			&record.Intent.ID,
			&record.Intent.Board,
			&record.Intent.PublicationID,
			&record.Intent.SourceKey,
			&record.Intent.ChangeSetID,
			&record.Intent.Mode,
			&record.Intent.TargetBranch,
			&record.Intent.Remote,
			&record.Intent.BaseCommit,
			&record.Intent.HeadCommit,
			&record.Intent.DurableRef,
			&executionProvenanceFingerprint,
			&record.Intent.EffectFingerprint,
			&record.Intent.ClaimEpoch,
			&record.Intent.PublicationUpdatedAt,
			&record.Intent.ClaimExpiresAt,
			&record.Intent.SessionID,
			&record.Intent.GateGeneration,
			&record.Intent.StartedAt,
			&resultAttemptID,
			&resultBoard,
			&resultPublicationID,
			&resultClaimEpoch,
			&resultOutcome,
			&resultExecutorStatus,
			&resultErrorKind,
			&resultURL,
			&resultError,
			&resultUpdatedAt,
			&resultRecordedAt,
		); err != nil {
			return nil, err
		}
		if !executionProvenanceFingerprint.Valid ||
			executionProvenanceFingerprint.String == "" {
			return nil, errors.New(
				"publication attempt intent lacks v30 execution provenance",
			)
		}
		record.Intent.ExecutionProvenanceFingerprint =
			executionProvenanceFingerprint.String
		if !validPublicationAttemptFingerprint(
			record.Intent.ExecutionProvenanceFingerprint,
		) {
			return nil, errors.New(
				"publication attempt intent has invalid execution provenance",
			)
		}
		if resultAttemptID.Valid {
			if !resultBoard.Valid || !resultPublicationID.Valid ||
				!resultClaimEpoch.Valid || !resultOutcome.Valid ||
				!resultExecutorStatus.Valid ||
				!resultUpdatedAt.Valid || !resultRecordedAt.Valid {
				return nil, errors.New(
					"publication attempt result has incomplete identity",
				)
			}
			result = PublicationAttemptResult{
				AttemptID:            resultAttemptID.String,
				Board:                resultBoard.String,
				PublicationID:        resultPublicationID.String,
				ClaimEpoch:           resultClaimEpoch.Int64,
				Outcome:              PublicationAttemptOutcome(resultOutcome.String),
				ExecutorStatus:       PublicationExecutorStatus(resultExecutorStatus.String),
				PublicationUpdatedAt: resultUpdatedAt.String,
				RecordedAt:           resultRecordedAt.String,
			}
			if resultErrorKind.Valid {
				kind := PublicationAttemptErrorKind(resultErrorKind.String)
				result.ErrorKind = &kind
			}
			result.URL = stringPointer(resultURL)
			result.Error = stringPointer(resultError)
			record.Result = &result
		}
		if record.Intent.SourceKey != publicationAttemptSourceKey(
			record.Intent.Board,
			record.Intent.PublicationID,
			record.Intent.PublicationUpdatedAt,
			record.Intent.ClaimEpoch,
		) || record.Intent.EffectFingerprint != publicationEffectFingerprint(
			record.Intent,
		) {
			return nil, errors.New(
				"publication attempt intent has invalid derived evidence",
			)
		}
		if _, err := normalizePublicationAttemptLedgerTimestamp(
			record.Intent.StartedAt,
			"publication attempt startedAt",
		); err != nil {
			return nil, err
		}
		if record.Result != nil {
			if _, err := normalizePublicationAttemptLedgerTimestamp(
				record.Result.RecordedAt,
				"publication attempt result recordedAt",
			); err != nil {
				return nil, err
			}
			resultInput := PublicationAttemptResultInput{
				Outcome:        record.Result.Outcome,
				ExecutorStatus: record.Result.ExecutorStatus,
				URL:            record.Result.URL,
			}
			if record.Result.ErrorKind != nil {
				resultInput.ErrorKind = *record.Result.ErrorKind
			}
			if record.Result.Error != nil {
				resultInput.Error = *record.Result.Error
			}
			normalized, err := normalizePublicationAttemptResult(resultInput)
			if err != nil ||
				!samePublicationAttemptResult(*record.Result, normalized) ||
				record.Result.AttemptID != record.Intent.ID ||
				record.Result.Board != record.Intent.Board ||
				record.Result.PublicationID != record.Intent.PublicationID ||
				record.Result.ClaimEpoch != record.Intent.ClaimEpoch {
				if err != nil {
					return nil, fmt.Errorf(
						"invalid publication attempt result: %w",
						err,
					)
				}
				return nil, errors.New(
					"publication attempt result does not match its intent",
				)
			}
		}
		values = append(values, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) unresolvedPublicationAttempt(
	ctx context.Context,
	board string,
	record PublicationAttemptRecord,
) (PublicationAttemptRecord, bool, error) {
	latestResult, exists, err := publicationAttemptResult(
		ctx,
		s.db,
		record.Intent.ID,
	)
	if err != nil {
		return PublicationAttemptRecord{}, false, err
	}
	if exists {
		if latestResult.AttemptID != record.Intent.ID ||
			latestResult.Board != record.Intent.Board ||
			latestResult.PublicationID != record.Intent.PublicationID ||
			latestResult.ClaimEpoch != record.Intent.ClaimEpoch {
			return PublicationAttemptRecord{}, false, errors.New(
				"publication attempt result does not match its intent",
			)
		}
		if latestResult.Outcome != PublicationAttemptUnknown {
			return PublicationAttemptRecord{}, false, nil
		}
		record.Result = &latestResult
	}

	recovery, err := publicationRecoveryReceiptForSource(
		ctx,
		s.db,
		record.Intent.SourceKey,
	)
	if errors.Is(err, ErrPublicationRecoveryReceiptNotFound) {
		return record, true, nil
	}
	if err != nil {
		return PublicationAttemptRecord{}, false, err
	}
	record.RecoveryReceipt = &recovery
	exactReceipt := recovery.PublicationID == record.Intent.PublicationID &&
		recovery.ObservedUpdatedAt == record.Intent.PublicationUpdatedAt &&
		recovery.ObservedClaimEpoch == record.Intent.ClaimEpoch
	if !exactReceipt {
		return record, true, nil
	}
	current, err := publicationForBoard(
		ctx,
		s.db,
		record.Intent.PublicationID,
		board,
	)
	if errors.Is(err, ErrPublicationNotFound) {
		// Exact immutable recovery evidence survives the deliberate task
		// cascade that removes an already-terminal publication.
		return PublicationAttemptRecord{}, false, nil
	}
	if err != nil {
		return PublicationAttemptRecord{}, false, err
	}
	if current.Status == model.PublicationPublishing &&
		current.ClaimEpoch == record.Intent.ClaimEpoch &&
		current.UpdatedAt == record.Intent.PublicationUpdatedAt {
		// A receipt beside the original Publishing tuple means the durable
		// terminal transition did not complete. Keep it visible as an
		// integrity failure instead of treating the receipt as sufficient.
		return record, true, nil
	}
	// The exact source was operator-adjudicated and the publication has moved
	// away from that source. A later explicit retry or newer claim must not
	// resurrect the old attempt.
	return PublicationAttemptRecord{}, false, nil
}

// ListUnresolvedPublicationAttempts returns intent-only and explicitly unknown
// attempts using a stable (startedAt, ID) keyset. Limit applies to returned
// unresolved attempts, not raw ledger rows; internally the scan continues
// across recovered pages until the requested limit is filled or raw EOF is
// reached. Callers continue with the last returned intent's StartedAt and ID.
func (s *Store) ListUnresolvedPublicationAttempts(
	ctx context.Context,
	filter PublicationAttemptFilter,
) ([]PublicationAttemptRecord, error) {
	afterID, err := validRecordID(filter.AfterID, "publication attempt cursor")
	if err != nil {
		return nil, err
	}
	afterStartedAt, err := normalizedPublicationText(
		filter.AfterStartedAt,
		"publication attempt cursor startedAt",
		128,
		false,
	)
	if err != nil {
		return nil, err
	}
	if afterStartedAt == "" && afterID != "" {
		return nil, errors.New(
			"publication attempt cursor ID requires startedAt",
		)
	}
	if afterStartedAt != "" {
		afterStartedAt, err = normalizePublicationAttemptLedgerTimestamp(
			afterStartedAt,
			"publication attempt cursor startedAt",
		)
		if err != nil {
			return nil, err
		}
	}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	board, err := normalizePublicationBoard("", s.board)
	if err != nil {
		return nil, err
	}

	const rawPageSize = 100
	unresolved := make([]PublicationAttemptRecord, 0, filter.Limit)
	scanStartedAt := afterStartedAt
	scanID := afterID
	for len(unresolved) < filter.Limit {
		values, err := s.listPublicationAttemptCandidatePage(
			ctx,
			board,
			scanStartedAt,
			scanID,
			rawPageSize,
		)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			break
		}
		for _, candidate := range values {
			scanStartedAt = candidate.Intent.StartedAt
			scanID = candidate.Intent.ID
			record, remains, err := s.unresolvedPublicationAttempt(
				ctx,
				board,
				candidate,
			)
			if err != nil {
				return nil, err
			}
			if remains {
				unresolved = append(unresolved, record)
				if len(unresolved) == filter.Limit {
					return unresolved, nil
				}
			}
		}
		if len(values) < rawPageSize {
			break
		}
	}
	return unresolved, nil
}

// Compile-time check that adding exported fields to the permit cannot silently
// make credentials JSON-visible.
var _ json.Marshaler = (*PublicationAttemptPermit)(nil)
