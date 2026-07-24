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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/publicationeffect"
)

var (
	ErrPublicationEffectNotFound       = errors.New("publication effect not found")
	ErrPublicationEffectPermitClosed   = errors.New("publication effect permit is closed")
	ErrPublicationEffectScope          = errors.New("publication effect permit does not cover this effect")
	ErrPublicationEffectSequence       = errors.New("publication effect sequence conflict")
	ErrPublicationEffectAlreadyStarted = errors.New("publication effect command was already released")
	ErrPublicationEffectResultConflict = errors.New("publication effect already has a different result")
	ErrPublicationEffectUnresolved     = errors.New("publication attempt has unresolved command effects")
	ErrPublicationEffectParentOutcome  = errors.New("publication attempt with an unknown command effect requires an unknown result")
)

const (
	MaxPublicationEffectsPerAttempt     = 64
	MaxPublicationEffectDescriptorBytes = publicationeffect.MaxCanonicalJSONBytes
	MaxPublicationEffectEvidenceBytes   = 32 * 1024
)

type PublicationEffectKind string

const (
	PublicationEffectLocalRefCAS     PublicationEffectKind = "local_ref_cas"
	PublicationEffectLocalWorktreeFF PublicationEffectKind = "local_worktree_ff"
	PublicationEffectPRBranchPush    PublicationEffectKind = "pr_branch_push"
	PublicationEffectPRCreate        PublicationEffectKind = "pr_create"
)

type PublicationEffectOutcome string

const (
	PublicationEffectApplied    PublicationEffectOutcome = "applied"
	PublicationEffectNotApplied PublicationEffectOutcome = "not_applied"
	PublicationEffectUnknown    PublicationEffectOutcome = "unknown"
)

// PublicationEffectIntent is durable, credential-free proof that a single
// mutating command was prepared before its process fence could be released.
// Descriptor contains only the caller-supplied canonical target descriptor;
// argv, raw repository paths, request bodies, and credentials are rejected.
type PublicationEffectIntent struct {
	ID                          string                `json:"id"`
	AttemptID                   string                `json:"attemptId"`
	Board                       string                `json:"board"`
	PublicationID               string                `json:"publicationId"`
	ClaimEpoch                  int64                 `json:"claimEpoch"`
	Sequence                    int64                 `json:"sequence"`
	Kind                        PublicationEffectKind `json:"kind"`
	DescriptorVersion           int64                 `json:"descriptorVersion"`
	Descriptor                  json.RawMessage       `json:"descriptor"`
	DescriptorFingerprint       string                `json:"descriptorFingerprint"`
	IdentityFingerprint         string                `json:"identityFingerprint"`
	ParentEffectFingerprint     string                `json:"parentEffectFingerprint"`
	ParentProvenanceFingerprint string                `json:"parentProvenanceFingerprint"`
	PreparedAt                  string                `json:"preparedAt"`
}

type PublicationEffectResult struct {
	EffectID            string                   `json:"effectId"`
	AttemptID           string                   `json:"attemptId"`
	Board               string                   `json:"board"`
	PublicationID       string                   `json:"publicationId"`
	ClaimEpoch          int64                    `json:"claimEpoch"`
	Sequence            int64                    `json:"sequence"`
	IdentityFingerprint string                   `json:"identityFingerprint"`
	Outcome             PublicationEffectOutcome `json:"outcome"`
	Evidence            json.RawMessage          `json:"evidence"`
	EvidenceFingerprint string                   `json:"evidenceFingerprint"`
	ErrorKind           *string                  `json:"errorKind,omitempty"`
	// ErrorDetailFingerprint is the SHA-256 digest of caller-held diagnostics.
	// Raw command output, credentials, and local paths never enter the ledger.
	ErrorDetailFingerprint *string `json:"errorDetailFingerprint,omitempty"`
	RecordedAt             string  `json:"recordedAt"`
}

type PublicationEffectRecord struct {
	Intent PublicationEffectIntent  `json:"intent"`
	Result *PublicationEffectResult `json:"result,omitempty"`
}

type PublicationEffectPrepareInput struct {
	Sequence              int64
	Kind                  PublicationEffectKind
	DescriptorVersion     int64
	Descriptor            json.RawMessage
	DescriptorFingerprint string
}

type PublicationEffectResultInput struct {
	Outcome                PublicationEffectOutcome
	Evidence               json.RawMessage
	EvidenceFingerprint    string
	ErrorKind              string
	ErrorDetailFingerprint string
}

type PublicationEffectFilter struct {
	AfterPreparedAt string
	AfterID         string
	Limit           int
}

type publicationEffectPermitState struct {
	mu       sync.Mutex
	intent   PublicationEffectIntent
	parent   *publicationAttemptPermitState
	started  bool
	finished bool
}

// PublicationEffectPermit is a non-copyable in-memory capability bound to one
// exact PublicationAttemptPermit state. It contains no durable credentials.
type PublicationEffectPermit struct {
	self  *PublicationEffectPermit
	state *publicationEffectPermitState
}

func (p *PublicationEffectPermit) String() string {
	switch {
	case p == nil:
		return "publication effect permit (nil)"
	case p.self != p || p.state == nil:
		return "publication effect permit (invalid)"
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	switch {
	case p.state.finished:
		return "publication effect permit (finished)"
	case p.state.started:
		return "publication effect permit (started)"
	default:
		return "publication effect permit (prepared)"
	}
}

func (p *PublicationEffectPermit) GoString() string { return p.String() }

func (p *PublicationEffectPermit) MarshalJSON() ([]byte, error) {
	return []byte(`{}`), nil
}

func (p *PublicationEffectPermit) Intent() PublicationEffectIntent {
	if p == nil || p.self != p || p.state == nil {
		return PublicationEffectIntent{}
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	return clonePublicationEffectIntent(p.state.intent)
}

func newPublicationEffectPermit(
	intent PublicationEffectIntent,
	parent *publicationAttemptPermitState,
) (*PublicationEffectPermit, error) {
	if parent == nil {
		return nil, ErrPublicationEffectScope
	}
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if parent.effects == nil {
		parent.effects = make(map[string]*publicationEffectPermitState)
	}
	state := parent.effects[intent.ID]
	if state == nil {
		state = &publicationEffectPermitState{
			intent: clonePublicationEffectIntent(intent),
			parent: parent,
		}
		parent.effects[intent.ID] = state
	} else if !samePublicationEffectIntent(state.intent, intent) ||
		state.parent != parent {
		return nil, ErrPublicationEffectScope
	}
	permit := &PublicationEffectPermit{
		state: state,
	}
	permit.self = permit
	return permit, nil
}

func clonePublicationEffectIntent(
	value PublicationEffectIntent,
) PublicationEffectIntent {
	value.Descriptor = append(json.RawMessage(nil), value.Descriptor...)
	return value
}

func clonePublicationEffectResult(
	value PublicationEffectResult,
) PublicationEffectResult {
	value.Evidence = append(json.RawMessage(nil), value.Evidence...)
	if value.ErrorKind != nil {
		errorKind := *value.ErrorKind
		value.ErrorKind = &errorKind
	}
	if value.ErrorDetailFingerprint != nil {
		errorDetailFingerprint := *value.ErrorDetailFingerprint
		value.ErrorDetailFingerprint = &errorDetailFingerprint
	}
	return value
}

func validPublicationEffectKind(value PublicationEffectKind) bool {
	switch value {
	case PublicationEffectLocalRefCAS,
		PublicationEffectLocalWorktreeFF,
		PublicationEffectPRBranchPush,
		PublicationEffectPRCreate:
		return true
	default:
		return false
	}
}

func publicationEffectForbiddenJSONKey(value string) bool {
	value = strings.ToLower(value)
	value = strings.NewReplacer(
		"_", "",
		"-", "",
		".", "",
	).Replace(value)
	switch value {
	case "argv",
		"args",
		"credential",
		"credentials",
		"password",
		"secret",
		"token",
		"worktreepath",
		"rawbody",
		"requestbody",
		"body":
		return true
	default:
		return false
	}
}

func validatePublicationEffectJSONValue(
	value any,
	field string,
) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if publicationEffectForbiddenJSONKey(key) {
				return fmt.Errorf(
					"%s cannot contain sensitive or raw field %q",
					field,
					key,
				)
			}
			if err := validatePublicationEffectJSONValue(
				child,
				field,
			); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validatePublicationEffectJSONValue(
				child,
				field,
			); err != nil {
				return err
			}
		}
	case string:
		// User information in a URI is credential material even if its field
		// name appears harmless.
		scheme := strings.Index(typed, "://")
		at := strings.Index(typed, "@")
		slash := -1
		if scheme >= 0 {
			if suffix := strings.Index(typed[scheme+3:], "/"); suffix >= 0 {
				slash = scheme + 3 + suffix
			}
		}
		if scheme > 0 && at > scheme+3 &&
			(slash < 0 || at < slash) {
			return fmt.Errorf(
				"%s cannot contain a credential-bearing URI",
				field,
			)
		}
		if strings.HasPrefix(typed, "/") ||
			strings.HasPrefix(strings.ToLower(typed), "file://") ||
			(len(typed) >= 3 &&
				((typed[0] >= 'a' && typed[0] <= 'z') ||
					(typed[0] >= 'A' && typed[0] <= 'Z')) &&
				typed[1] == ':' &&
				(typed[2] == '/' || typed[2] == '\\')) {
			return fmt.Errorf(
				"%s cannot contain a raw filesystem path",
				field,
			)
		}
	}
	return nil
}

func canonicalPublicationEffectJSON(
	raw json.RawMessage,
	field string,
	maxBytes int,
) (json.RawMessage, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("%s cannot be empty", field)
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf(
			"%s must be at most %d bytes",
			field,
			maxBytes,
		)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid UTF-8", field)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %w", field, err)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", field)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s contains trailing JSON", field)
		}
		return nil, fmt.Errorf("%s contains trailing data: %w", field, err)
	}
	if err := validatePublicationEffectJSONValue(decoded, field); err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	err := json.Compact(&compact, raw)
	if err != nil {
		return nil, fmt.Errorf("compact %s: %w", field, err)
	}
	canonical := compact.Bytes()
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf(
			"%s must use canonical JSON encoding",
			field,
		)
	}
	return append(json.RawMessage(nil), canonical...), nil
}

func publicationEffectJSONFingerprint(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func normalizePublicationEffectFingerprint(
	value string,
	field string,
) (string, error) {
	original := value
	value = strings.TrimSpace(value)
	if value != original || !validPublicationAttemptFingerprint(value) {
		return "", fmt.Errorf("%s must be a canonical SHA-256 hash", field)
	}
	return value, nil
}

func normalizePublicationEffectPrepareInput(
	input PublicationEffectPrepareInput,
) (PublicationEffectPrepareInput, error) {
	if input.Sequence < 1 ||
		input.Sequence > MaxPublicationEffectsPerAttempt {
		return PublicationEffectPrepareInput{}, fmt.Errorf(
			"publication effect sequence must be between 1 and %d",
			MaxPublicationEffectsPerAttempt,
		)
	}
	if !validPublicationEffectKind(input.Kind) {
		return PublicationEffectPrepareInput{}, fmt.Errorf(
			"invalid mutating publication effect kind: %s",
			input.Kind,
		)
	}
	if input.DescriptorVersion < 1 || input.DescriptorVersion > 16 {
		return PublicationEffectPrepareInput{}, errors.New(
			"publication effect descriptor version must be between 1 and 16",
		)
	}
	descriptor, err := publicationeffect.ParseCanonical(input.Descriptor)
	if err != nil {
		return PublicationEffectPrepareInput{}, fmt.Errorf(
			"invalid publication effect descriptor: %w",
			err,
		)
	}
	if PublicationEffectKind(descriptor.Kind()) != input.Kind {
		return PublicationEffectPrepareInput{}, errors.New(
			"publication effect descriptor kind does not match its bound kind",
		)
	}
	if int64(descriptor.Version()) != input.DescriptorVersion {
		return PublicationEffectPrepareInput{}, errors.New(
			"publication effect descriptor version does not match its bound version",
		)
	}
	input.Descriptor = descriptor.CanonicalJSON()
	input.DescriptorFingerprint, err =
		normalizePublicationEffectFingerprint(
			input.DescriptorFingerprint,
			"publication effect descriptor fingerprint",
		)
	if err != nil {
		return PublicationEffectPrepareInput{}, err
	}
	if input.DescriptorFingerprint != descriptor.Fingerprint() {
		return PublicationEffectPrepareInput{}, errors.New(
			"publication effect descriptor fingerprint does not match its canonical JSON",
		)
	}
	return input, nil
}

func normalizedPublicationEffectErrorKind(
	value string,
	required bool,
) (string, error) {
	original := value
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return "", errors.New(
				"publication effect error kind cannot be empty",
			)
		}
		return "", nil
	}
	if value != original || len(value) > 64 ||
		value[0] < 'a' || value[0] > 'z' {
		return "", errors.New(
			"publication effect error kind must be a canonical lowercase identifier",
		)
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '_' {
			return "", errors.New(
				"publication effect error kind must be a canonical lowercase identifier",
			)
		}
	}
	return value, nil
}

func normalizePublicationEffectResultInput(
	input PublicationEffectResultInput,
) (PublicationEffectResultInput, error) {
	evidence, err := canonicalPublicationEffectJSON(
		input.Evidence,
		"publication effect evidence",
		MaxPublicationEffectEvidenceBytes,
	)
	if err != nil {
		return PublicationEffectResultInput{}, err
	}
	input.Evidence = evidence
	input.EvidenceFingerprint, err =
		normalizePublicationEffectFingerprint(
			input.EvidenceFingerprint,
			"publication effect evidence fingerprint",
		)
	if err != nil {
		return PublicationEffectResultInput{}, err
	}
	if input.EvidenceFingerprint != publicationEffectJSONFingerprint(evidence) {
		return PublicationEffectResultInput{}, errors.New(
			"publication effect evidence fingerprint does not match its canonical JSON",
		)
	}

	switch input.Outcome {
	case PublicationEffectApplied:
		if strings.TrimSpace(input.ErrorKind) != "" ||
			strings.TrimSpace(input.ErrorDetailFingerprint) != "" {
			return PublicationEffectResultInput{}, errors.New(
				"applied publication effect cannot include an error",
			)
		}
		input.ErrorKind = ""
		input.ErrorDetailFingerprint = ""
	case PublicationEffectNotApplied:
		hasKind := strings.TrimSpace(input.ErrorKind) != ""
		hasFingerprint := strings.TrimSpace(
			input.ErrorDetailFingerprint,
		) != ""
		if hasKind != hasFingerprint {
			return PublicationEffectResultInput{}, errors.New(
				"not-applied publication effect requires both error kind and detail fingerprint or neither",
			)
		}
		input.ErrorKind, err = normalizedPublicationEffectErrorKind(
			input.ErrorKind,
			false,
		)
		if err != nil {
			return PublicationEffectResultInput{}, err
		}
		if hasFingerprint {
			input.ErrorDetailFingerprint, err =
				normalizePublicationEffectFingerprint(
					input.ErrorDetailFingerprint,
					"publication effect error detail fingerprint",
				)
			if err != nil {
				return PublicationEffectResultInput{}, err
			}
		} else {
			input.ErrorDetailFingerprint = ""
		}
	case PublicationEffectUnknown:
		input.ErrorKind, err = normalizedPublicationEffectErrorKind(
			input.ErrorKind,
			true,
		)
		if err != nil {
			return PublicationEffectResultInput{}, err
		}
		input.ErrorDetailFingerprint, err =
			normalizePublicationEffectFingerprint(
				input.ErrorDetailFingerprint,
				"publication effect error detail fingerprint",
			)
		if err != nil {
			return PublicationEffectResultInput{}, err
		}
	default:
		return PublicationEffectResultInput{}, fmt.Errorf(
			"invalid publication effect outcome: %s",
			input.Outcome,
		)
	}
	return input, nil
}

func publicationEffectIdentityFingerprint(
	intent PublicationEffectIntent,
) string {
	canonical := strings.Join([]string{
		"publication-command-effect-v1",
		intent.AttemptID,
		intent.Board,
		intent.PublicationID,
		strconv.FormatInt(intent.ClaimEpoch, 10),
		strconv.FormatInt(intent.Sequence, 10),
		string(intent.Kind),
		strconv.FormatInt(intent.DescriptorVersion, 10),
		intent.DescriptorFingerprint,
		intent.ParentEffectFingerprint,
		intent.ParentProvenanceFingerprint,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

const publicationEffectIntentColumns = `e.id, e.attempt_id, e.board,
	e.publication_id, e.claim_epoch, e.sequence, e.kind,
	e.descriptor_version, e.descriptor_json, e.descriptor_fingerprint,
	e.identity_fingerprint, e.parent_effect_fingerprint,
	e.parent_provenance_fingerprint, e.prepared_at`

const publicationEffectResultColumns = `r.effect_id, r.attempt_id, r.board,
	r.publication_id, r.claim_epoch, r.sequence, r.identity_fingerprint,
	r.outcome, r.evidence_json, r.evidence_fingerprint, r.error_kind,
	r.error_detail_fingerprint, r.recorded_at`

func scanPublicationEffectIntent(
	row scanner,
) (PublicationEffectIntent, error) {
	var value PublicationEffectIntent
	var descriptor []byte
	if err := row.Scan(
		&value.ID,
		&value.AttemptID,
		&value.Board,
		&value.PublicationID,
		&value.ClaimEpoch,
		&value.Sequence,
		&value.Kind,
		&value.DescriptorVersion,
		&descriptor,
		&value.DescriptorFingerprint,
		&value.IdentityFingerprint,
		&value.ParentEffectFingerprint,
		&value.ParentProvenanceFingerprint,
		&value.PreparedAt,
	); err != nil {
		return PublicationEffectIntent{}, err
	}
	normalized, err := normalizePublicationEffectPrepareInput(
		PublicationEffectPrepareInput{
			Sequence:              value.Sequence,
			Kind:                  value.Kind,
			DescriptorVersion:     value.DescriptorVersion,
			Descriptor:            descriptor,
			DescriptorFingerprint: value.DescriptorFingerprint,
		},
	)
	if err != nil {
		return PublicationEffectIntent{}, fmt.Errorf(
			"invalid publication effect intent: %w",
			err,
		)
	}
	value.Descriptor = normalized.Descriptor
	if !validPublicationAttemptFingerprint(
		value.ParentEffectFingerprint,
	) || !validPublicationAttemptFingerprint(
		value.ParentProvenanceFingerprint,
	) {
		return PublicationEffectIntent{}, errors.New(
			"publication effect intent has an invalid parent fingerprint",
		)
	}
	if value.IdentityFingerprint !=
		publicationEffectIdentityFingerprint(value) {
		return PublicationEffectIntent{}, errors.New(
			"publication effect intent has an invalid identity fingerprint",
		)
	}
	if _, err := normalizePublicationAttemptLedgerTimestamp(
		value.PreparedAt,
		"publication effect preparedAt",
	); err != nil {
		return PublicationEffectIntent{}, err
	}
	return value, nil
}

func publicationEffectIntent(
	ctx context.Context,
	q querier,
	effectID string,
) (PublicationEffectIntent, error) {
	value, err := scanPublicationEffectIntent(q.QueryRowContext(
		ctx,
		`SELECT `+publicationEffectIntentColumns+`
		 FROM publication_effect_intents e WHERE e.id = ?`,
		strings.TrimSpace(effectID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationEffectIntent{}, fmt.Errorf(
			"%w: %s",
			ErrPublicationEffectNotFound,
			strings.TrimSpace(effectID),
		)
	}
	return value, err
}

func publicationEffectIntentForSequence(
	ctx context.Context,
	q querier,
	attemptID string,
	sequence int64,
) (PublicationEffectIntent, bool, error) {
	value, err := scanPublicationEffectIntent(q.QueryRowContext(
		ctx,
		`SELECT `+publicationEffectIntentColumns+`
		 FROM publication_effect_intents e
		 WHERE e.attempt_id = ? AND e.sequence = ?`,
		attemptID,
		sequence,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationEffectIntent{}, false, nil
	}
	return value, err == nil, err
}

func samePublicationEffectPrepareInput(
	intent PublicationEffectIntent,
	input PublicationEffectPrepareInput,
) bool {
	return intent.Sequence == input.Sequence &&
		intent.Kind == input.Kind &&
		intent.DescriptorVersion == input.DescriptorVersion &&
		bytes.Equal(intent.Descriptor, input.Descriptor) &&
		intent.DescriptorFingerprint == input.DescriptorFingerprint
}

func samePublicationEffectIntent(
	left PublicationEffectIntent,
	right PublicationEffectIntent,
) bool {
	return left.ID == right.ID &&
		left.AttemptID == right.AttemptID &&
		left.Board == right.Board &&
		left.PublicationID == right.PublicationID &&
		left.ClaimEpoch == right.ClaimEpoch &&
		left.Sequence == right.Sequence &&
		left.Kind == right.Kind &&
		left.DescriptorVersion == right.DescriptorVersion &&
		bytes.Equal(left.Descriptor, right.Descriptor) &&
		left.DescriptorFingerprint == right.DescriptorFingerprint &&
		left.IdentityFingerprint == right.IdentityFingerprint &&
		left.ParentEffectFingerprint == right.ParentEffectFingerprint &&
		left.ParentProvenanceFingerprint ==
			right.ParentProvenanceFingerprint &&
		left.PreparedAt == right.PreparedAt
}

func scanPublicationEffectResult(
	row scanner,
) (PublicationEffectResult, error) {
	var value PublicationEffectResult
	var evidence []byte
	var errorKind, errorDetailFingerprint sql.NullString
	if err := row.Scan(
		&value.EffectID,
		&value.AttemptID,
		&value.Board,
		&value.PublicationID,
		&value.ClaimEpoch,
		&value.Sequence,
		&value.IdentityFingerprint,
		&value.Outcome,
		&evidence,
		&value.EvidenceFingerprint,
		&errorKind,
		&errorDetailFingerprint,
		&value.RecordedAt,
	); err != nil {
		return PublicationEffectResult{}, err
	}
	value.Evidence = evidence
	value.ErrorKind = stringPointer(errorKind)
	value.ErrorDetailFingerprint = stringPointer(errorDetailFingerprint)
	input := PublicationEffectResultInput{
		Outcome:             value.Outcome,
		Evidence:            value.Evidence,
		EvidenceFingerprint: value.EvidenceFingerprint,
	}
	if value.ErrorKind != nil {
		input.ErrorKind = *value.ErrorKind
	}
	if value.ErrorDetailFingerprint != nil {
		input.ErrorDetailFingerprint = *value.ErrorDetailFingerprint
	}
	normalized, err := normalizePublicationEffectResultInput(input)
	if err != nil || !samePublicationEffectResultInput(value, normalized) {
		if err != nil {
			return PublicationEffectResult{}, fmt.Errorf(
				"invalid publication effect result: %w",
				err,
			)
		}
		return PublicationEffectResult{}, errors.New(
			"publication effect result is not stored canonically",
		)
	}
	if _, err := normalizePublicationAttemptLedgerTimestamp(
		value.RecordedAt,
		"publication effect recordedAt",
	); err != nil {
		return PublicationEffectResult{}, err
	}
	return value, nil
}

func publicationEffectResult(
	ctx context.Context,
	q querier,
	effectID string,
) (PublicationEffectResult, bool, error) {
	value, err := scanPublicationEffectResult(q.QueryRowContext(
		ctx,
		`SELECT `+publicationEffectResultColumns+`
		 FROM publication_effect_results r WHERE r.effect_id = ?`,
		strings.TrimSpace(effectID),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationEffectResult{}, false, nil
	}
	return value, err == nil, err
}

func samePublicationEffectResultInput(
	value PublicationEffectResult,
	input PublicationEffectResultInput,
) bool {
	if value.Outcome != input.Outcome ||
		!bytes.Equal(value.Evidence, input.Evidence) ||
		value.EvidenceFingerprint != input.EvidenceFingerprint {
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
	switch {
	case value.ErrorDetailFingerprint == nil:
		return input.ErrorDetailFingerprint == ""
	default:
		return *value.ErrorDetailFingerprint ==
			input.ErrorDetailFingerprint
	}
}

func (s *Store) validatePublicationEffectParentCurrent(
	ctx context.Context,
	q querier,
	state *publicationAttemptPermitState,
	stored PublicationAttemptIntent,
	board string,
	requireLive bool,
) (model.Publication, error) {
	if err := validatePublicationAttemptScope(stored, state, board); err != nil {
		return model.Publication{}, err
	}
	current, err := publicationForBoard(
		ctx,
		q,
		stored.PublicationID,
		board,
	)
	if err != nil {
		return model.Publication{}, err
	}
	if current.Status != model.PublicationPublishing ||
		current.ClaimEpoch != stored.ClaimEpoch ||
		current.UpdatedAt != stored.PublicationUpdatedAt ||
		current.ClaimToken != state.claimToken ||
		current.ClaimExpiresAt == nil ||
		*current.ClaimExpiresAt != stored.ClaimExpiresAt ||
		!publicationMatchesAttemptEffect(current, stored) {
		return model.Publication{}, ErrPublicationEffectScope
	}
	if requireLive {
		currentTime, _, err := s.publicationCurrent()
		if err != nil {
			return model.Publication{}, err
		}
		if err := requireLivePublicationClaim(
			current,
			state.claimToken,
			stored.ClaimEpoch,
			currentTime,
		); err != nil {
			return model.Publication{}, err
		}
	}
	return current, nil
}

func validatePublicationEffectAgainstParent(
	effect PublicationEffectIntent,
	parent PublicationAttemptIntent,
) bool {
	return effect.AttemptID == parent.ID &&
		effect.Board == parent.Board &&
		effect.PublicationID == parent.PublicationID &&
		effect.ClaimEpoch == parent.ClaimEpoch &&
		effect.ParentEffectFingerprint == parent.EffectFingerprint &&
		effect.ParentProvenanceFingerprint ==
			parent.ExecutionProvenanceFingerprint &&
		effect.IdentityFingerprint ==
			publicationEffectIdentityFingerprint(effect)
}

func validatePublicationAttemptEffectOutcomes(
	ctx context.Context,
	q querier,
	attemptID string,
	parentOutcome PublicationAttemptOutcome,
) error {
	var unresolvedCount, unknownCount int
	if err := q.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE r.effect_id IS NULL),
			COUNT(*) FILTER (WHERE r.outcome = 'unknown')
		FROM publication_effect_intents e
		LEFT JOIN publication_effect_results r ON r.effect_id = e.id
		WHERE e.attempt_id = ?
	`, attemptID).Scan(&unresolvedCount, &unknownCount); err != nil {
		return fmt.Errorf(
			"inspect publication command effect outcomes: %w",
			err,
		)
	}
	if unresolvedCount != 0 {
		return ErrPublicationEffectUnresolved
	}
	if unknownCount != 0 &&
		parentOutcome != PublicationAttemptUnknown {
		return ErrPublicationEffectParentOutcome
	}
	return nil
}

// PreparePublicationEffect durably inserts one command effect intent before a
// caller may release the command process fence. Repeating the same sequence
// and descriptor is idempotent; changing either at an occupied sequence fails.
func (s *Store) PreparePublicationEffect(
	ctx context.Context,
	attempt *PublicationAttemptPermit,
	input PublicationEffectPrepareInput,
) (
	PublicationEffectIntent,
	*PublicationEffectPermit,
	bool,
	error,
) {
	input, err := normalizePublicationEffectPrepareInput(input)
	if err != nil {
		return PublicationEffectIntent{}, nil, false, err
	}
	if attempt == nil || attempt.state == nil {
		return PublicationEffectIntent{}, nil, false,
			ErrPublicationAttemptPermitClosed
	}
	parentState := attempt.state
	var intent PublicationEffectIntent
	created := false
	err = s.withPublicationAttemptCleanupLock(ctx, parentState, func() error {
		parentState.mu.Lock()
		defer parentState.mu.Unlock()
		if parentState.finished {
			return ErrPublicationAttemptPermitClosed
		}
		board, err := normalizePublicationBoard(
			parentState.intent.Board,
			s.board,
		)
		if err != nil {
			return ErrPublicationEffectScope
		}
		return s.withWrite(ctx, func(tx *sql.Tx) error {
			parent, err := publicationAttemptIntent(
				ctx,
				tx,
				parentState.intent.ID,
			)
			if err != nil {
				return err
			}
			if _, exists, err := publicationAttemptResult(
				ctx,
				tx,
				parent.ID,
			); err != nil {
				return err
			} else if exists {
				return ErrPublicationAttemptPermitClosed
			}
			if _, err := s.validatePublicationEffectParentCurrent(
				ctx,
				tx,
				parentState,
				parent,
				board,
				true,
			); err != nil {
				return err
			}

			existing, exists, err :=
				publicationEffectIntentForSequence(
					ctx,
					tx,
					parent.ID,
					input.Sequence,
				)
			if err != nil {
				return err
			}
			if exists {
				if !validatePublicationEffectAgainstParent(
					existing,
					parent,
				) || !samePublicationEffectPrepareInput(existing, input) {
					return ErrPublicationEffectSequence
				}
				intent = existing
				return nil
			}

			var highestSequence int64
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(MAX(sequence), 0)
				FROM publication_effect_intents
				WHERE attempt_id = ?
			`, parent.ID).Scan(&highestSequence); err != nil {
				return err
			}
			if highestSequence >= MaxPublicationEffectsPerAttempt ||
				input.Sequence != highestSequence+1 {
				return fmt.Errorf(
					"%w: got %d, want %d",
					ErrPublicationEffectSequence,
					input.Sequence,
					highestSequence+1,
				)
			}
			preparedAt, err := publicationAttemptLedgerTimestamp(now())
			if err != nil {
				return err
			}
			intent = PublicationEffectIntent{
				ID:                          newID("pef"),
				AttemptID:                   parent.ID,
				Board:                       parent.Board,
				PublicationID:               parent.PublicationID,
				ClaimEpoch:                  parent.ClaimEpoch,
				Sequence:                    input.Sequence,
				Kind:                        input.Kind,
				DescriptorVersion:           input.DescriptorVersion,
				Descriptor:                  append(json.RawMessage(nil), input.Descriptor...),
				DescriptorFingerprint:       input.DescriptorFingerprint,
				ParentEffectFingerprint:     parent.EffectFingerprint,
				ParentProvenanceFingerprint: parent.ExecutionProvenanceFingerprint,
				PreparedAt:                  preparedAt,
			}
			intent.IdentityFingerprint =
				publicationEffectIdentityFingerprint(intent)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_effect_intents(
					id, attempt_id, board, publication_id, claim_epoch,
					sequence, kind, descriptor_version, descriptor_json,
					descriptor_fingerprint, identity_fingerprint,
					parent_effect_fingerprint,
					parent_provenance_fingerprint, prepared_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				intent.ID,
				intent.AttemptID,
				intent.Board,
				intent.PublicationID,
				intent.ClaimEpoch,
				intent.Sequence,
				intent.Kind,
				intent.DescriptorVersion,
				string(intent.Descriptor),
				intent.DescriptorFingerprint,
				intent.IdentityFingerprint,
				intent.ParentEffectFingerprint,
				intent.ParentProvenanceFingerprint,
				intent.PreparedAt,
			); err != nil {
				return fmt.Errorf(
					"record publication effect intent: %w",
					err,
				)
			}
			created = true
			return nil
		})
	})
	if err != nil {
		return PublicationEffectIntent{}, nil, false, err
	}
	effectPermit, err := newPublicationEffectPermit(intent, parentState)
	if err != nil {
		return PublicationEffectIntent{}, nil, false, err
	}
	return clonePublicationEffectIntent(intent), effectPermit, created, nil
}

func validatePublicationEffectCommandCapability(
	automation *AutomationPermit,
	attempt *PublicationAttemptPermit,
	effect *PublicationEffectPermit,
	board string,
) error {
	if effect == nil || effect.self != effect || effect.state == nil {
		return ErrPublicationEffectPermitClosed
	}
	if attempt == nil || attempt.state == nil {
		return ErrPublicationAttemptPermitClosed
	}
	if effect.state.parent != attempt.state {
		return ErrPublicationEffectScope
	}
	if err := validatePublicationCommandStartCapability(
		automation,
		attempt.state,
		board,
	); err != nil {
		return err
	}
	return nil
}

func releasePublicationEffectCommandFence(
	state *publicationEffectPermitState,
	release func() (bool, error),
) (released bool, resultErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// A panicking release callback cannot prove whether it opened the
			// process fence. Fail closed in memory and preserve the durable
			// unresolved intent for operator recovery.
			state.started = true
			panic(recovered)
		}
	}()
	released, resultErr = release()
	if released {
		state.started = true
	}
	return released, resultErr
}

// WithPublicationEffectCommandStart validates the committed effect intent,
// the fresh automation authority, and the exact live publication tuple before
// invoking release at the process-start linearization point.
func (s *Store) WithPublicationEffectCommandStart(
	ctx context.Context,
	automation *AutomationPermit,
	attempt *PublicationAttemptPermit,
	effect *PublicationEffectPermit,
	release func() (bool, error),
) (released bool, resultErr error) {
	if release == nil {
		return false, errors.New(
			"publication effect command release callback is required",
		)
	}
	if effect == nil || effect.self != effect || effect.state == nil {
		return false, ErrPublicationEffectPermitClosed
	}
	if attempt == nil || attempt.state == nil {
		return false, ErrPublicationAttemptPermitClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	transactionContext := context.WithoutCancel(ctx)
	parentState := attempt.state
	effectState := effect.state
	err := s.withAutomationPermitForBoard(
		ctx,
		automation,
		s.board,
		func() error {
			parentState.mu.Lock()
			defer parentState.mu.Unlock()
			effectState.mu.Lock()
			defer effectState.mu.Unlock()
			board, err := normalizePublicationBoard(
				parentState.intent.Board,
				s.board,
			)
			if err != nil {
				return ErrPublicationEffectScope
			}
			if err := validatePublicationEffectCommandCapability(
				automation,
				attempt,
				effect,
				board,
			); err != nil {
				return err
			}
			if effectState.finished {
				return ErrPublicationEffectPermitClosed
			}
			if effectState.started {
				return ErrPublicationEffectAlreadyStarted
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return s.withWrite(
				transactionContext,
				func(tx *sql.Tx) error {
					parent, err := publicationAttemptIntent(
						transactionContext,
						tx,
						parentState.intent.ID,
					)
					if err != nil {
						return err
					}
					if _, exists, err := publicationAttemptResult(
						transactionContext,
						tx,
						parent.ID,
					); err != nil {
						return err
					} else if exists {
						return ErrPublicationAttemptPermitClosed
					}
					if _, err := s.validatePublicationEffectParentCurrent(
						transactionContext,
						tx,
						parentState,
						parent,
						board,
						true,
					); err != nil {
						return err
					}
					storedEffect, err := publicationEffectIntent(
						transactionContext,
						tx,
						effectState.intent.ID,
					)
					if err != nil {
						return err
					}
					if !samePublicationEffectIntent(
						storedEffect,
						effectState.intent,
					) || !validatePublicationEffectAgainstParent(
						storedEffect,
						parent,
					) {
						return ErrPublicationEffectScope
					}
					if _, exists, err := publicationEffectResult(
						transactionContext,
						tx,
						storedEffect.ID,
					); err != nil {
						return err
					} else if exists {
						return ErrPublicationEffectPermitClosed
					}
					sessionExpiresAt, err :=
						s.validateAutomationPermitLockedWithExpiry(
							transactionContext,
							automation,
						)
					if err != nil {
						return err
					}
					if err := validateAutomationSessionExpiry(
						sessionExpiresAt,
						time.Now().UTC(),
					); err != nil {
						return err
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					released, err = releasePublicationEffectCommandFence(
						effectState,
						release,
					)
					return err
				},
			)
		},
	)
	return released, err
}

// FinishPublicationEffect records one immutable result. It deliberately does
// not require an unexpired claim, but the parent publication and attempt tuple
// must still match exactly. This preserves a known command outcome after the
// lease boundary without attaching it to a newer claim.
func (s *Store) FinishPublicationEffect(
	ctx context.Context,
	permit *PublicationEffectPermit,
	input PublicationEffectResultInput,
) (PublicationEffectResult, error) {
	input, err := normalizePublicationEffectResultInput(input)
	if err != nil {
		return PublicationEffectResult{}, err
	}
	if permit == nil || permit.self != permit || permit.state == nil {
		return PublicationEffectResult{}, ErrPublicationEffectPermitClosed
	}
	effectState := permit.state
	parentState := effectState.parent
	if parentState == nil {
		return PublicationEffectResult{}, ErrPublicationEffectScope
	}
	var result PublicationEffectResult
	err = s.withPublicationAttemptCleanupLock(ctx, parentState, func() error {
		parentState.mu.Lock()
		defer parentState.mu.Unlock()
		effectState.mu.Lock()
		defer effectState.mu.Unlock()
		board, err := normalizePublicationBoard(
			parentState.intent.Board,
			s.board,
		)
		if err != nil {
			return ErrPublicationEffectScope
		}
		finishErr := s.withWriteUnchecked(ctx, func(tx *sql.Tx) error {
			parent, err := publicationAttemptIntent(
				ctx,
				tx,
				parentState.intent.ID,
			)
			if err != nil {
				return err
			}
			if err := validatePublicationAttemptScope(
				parent,
				parentState,
				board,
			); err != nil {
				return err
			}
			storedEffect, err := publicationEffectIntent(
				ctx,
				tx,
				effectState.intent.ID,
			)
			if err != nil {
				return err
			}
			if !samePublicationEffectIntent(
				storedEffect,
				effectState.intent,
			) || !validatePublicationEffectAgainstParent(
				storedEffect,
				parent,
			) {
				return ErrPublicationEffectScope
			}
			existing, exists, err := publicationEffectResult(
				ctx,
				tx,
				storedEffect.ID,
			)
			if err != nil {
				return err
			}
			if exists {
				if existing.AttemptID != storedEffect.AttemptID ||
					existing.Board != storedEffect.Board ||
					existing.PublicationID != storedEffect.PublicationID ||
					existing.ClaimEpoch != storedEffect.ClaimEpoch ||
					existing.Sequence != storedEffect.Sequence ||
					existing.IdentityFingerprint !=
						storedEffect.IdentityFingerprint {
					return ErrPublicationEffectScope
				}
				if !samePublicationEffectResultInput(existing, input) {
					return ErrPublicationEffectResultConflict
				}
				result = clonePublicationEffectResult(existing)
				return nil
			}
			if effectState.finished {
				return ErrPublicationEffectResultConflict
			}
			if (input.Outcome == PublicationEffectApplied ||
				input.Outcome == PublicationEffectUnknown) &&
				!effectState.started {
				return errors.New(
					"publication effect must cross its command start boundary before this outcome",
				)
			}
			if _, exists, err := publicationAttemptResult(
				ctx,
				tx,
				parent.ID,
			); err != nil {
				return err
			} else if exists {
				return ErrPublicationAttemptPermitClosed
			}
			if _, err := s.validatePublicationEffectParentCurrent(
				ctx,
				tx,
				parentState,
				parent,
				board,
				false,
			); err != nil {
				return err
			}
			recordedAt, err := publicationAttemptLedgerTimestamp(now())
			if err != nil {
				return err
			}
			result = PublicationEffectResult{
				EffectID:            storedEffect.ID,
				AttemptID:           storedEffect.AttemptID,
				Board:               storedEffect.Board,
				PublicationID:       storedEffect.PublicationID,
				ClaimEpoch:          storedEffect.ClaimEpoch,
				Sequence:            storedEffect.Sequence,
				IdentityFingerprint: storedEffect.IdentityFingerprint,
				Outcome:             input.Outcome,
				Evidence:            append(json.RawMessage(nil), input.Evidence...),
				EvidenceFingerprint: input.EvidenceFingerprint,
				RecordedAt:          recordedAt,
			}
			if input.ErrorKind != "" {
				errorKind := input.ErrorKind
				result.ErrorKind = &errorKind
			}
			if input.ErrorDetailFingerprint != "" {
				errorDetailFingerprint := input.ErrorDetailFingerprint
				result.ErrorDetailFingerprint = &errorDetailFingerprint
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO publication_effect_results(
					effect_id, attempt_id, board, publication_id,
					claim_epoch, sequence, identity_fingerprint, outcome,
					evidence_json, evidence_fingerprint, error_kind,
					error_detail_fingerprint, recorded_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				result.EffectID,
				result.AttemptID,
				result.Board,
				result.PublicationID,
				result.ClaimEpoch,
				result.Sequence,
				result.IdentityFingerprint,
				result.Outcome,
				string(result.Evidence),
				result.EvidenceFingerprint,
				nullableString(result.ErrorKind),
				nullableString(result.ErrorDetailFingerprint),
				result.RecordedAt,
			); err != nil {
				return fmt.Errorf(
					"record publication effect result: %w",
					err,
				)
			}
			return nil
		})
		if finishErr != nil {
			return finishErr
		}
		effectState.finished = true
		return nil
	})
	if err != nil {
		return PublicationEffectResult{}, err
	}
	return clonePublicationEffectResult(result), nil
}

func (s *Store) GetPublicationEffect(
	ctx context.Context,
	effectID string,
) (PublicationEffectRecord, error) {
	effectID, err := validRecordID(effectID, "publication effect id")
	if err != nil {
		return PublicationEffectRecord{}, err
	}
	if effectID == "" {
		return PublicationEffectRecord{}, errors.New(
			"publication effect id cannot be empty",
		)
	}
	intent, err := publicationEffectIntent(ctx, s.db, effectID)
	if err != nil {
		return PublicationEffectRecord{}, err
	}
	board, err := normalizePublicationBoard(intent.Board, s.board)
	if err != nil || board != intent.Board {
		return PublicationEffectRecord{}, fmt.Errorf(
			"%w: %s",
			ErrPublicationEffectNotFound,
			effectID,
		)
	}
	parent, err := publicationAttemptIntent(ctx, s.db, intent.AttemptID)
	if err != nil || !validatePublicationEffectAgainstParent(intent, parent) {
		if err != nil {
			return PublicationEffectRecord{}, err
		}
		return PublicationEffectRecord{}, ErrPublicationEffectScope
	}
	record := PublicationEffectRecord{
		Intent: clonePublicationEffectIntent(intent),
	}
	result, exists, err := publicationEffectResult(ctx, s.db, effectID)
	if err != nil {
		return PublicationEffectRecord{}, err
	}
	if exists {
		if result.AttemptID != intent.AttemptID ||
			result.Board != intent.Board ||
			result.PublicationID != intent.PublicationID ||
			result.ClaimEpoch != intent.ClaimEpoch ||
			result.Sequence != intent.Sequence ||
			result.IdentityFingerprint != intent.IdentityFingerprint {
			return PublicationEffectRecord{}, ErrPublicationEffectScope
		}
		copied := clonePublicationEffectResult(result)
		record.Result = &copied
	}
	return record, nil
}

// ListUnresolvedPublicationEffects returns prepared intents without a result
// using a stable (preparedAt, ID) keyset for restart recovery inspection.
func (s *Store) ListUnresolvedPublicationEffects(
	ctx context.Context,
	filter PublicationEffectFilter,
) ([]PublicationEffectRecord, error) {
	afterID, err := validRecordID(
		filter.AfterID,
		"publication effect cursor ID",
	)
	if err != nil {
		return nil, err
	}
	afterPreparedAt, err := normalizedPublicationText(
		filter.AfterPreparedAt,
		"publication effect cursor preparedAt",
		128,
		false,
	)
	if err != nil {
		return nil, err
	}
	if afterPreparedAt == "" && afterID != "" {
		return nil, errors.New(
			"publication effect cursor ID requires preparedAt",
		)
	}
	if afterPreparedAt != "" {
		afterPreparedAt, err =
			normalizePublicationAttemptLedgerTimestamp(
				afterPreparedAt,
				"publication effect cursor preparedAt",
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+publicationEffectIntentColumns+`
		FROM publication_effect_intents e
		LEFT JOIN publication_effect_results r ON r.effect_id = e.id
		WHERE e.board = ? AND r.effect_id IS NULL
			AND (
				e.prepared_at > ?
				OR (e.prepared_at = ? AND e.id > ?)
			)
		ORDER BY e.prepared_at, e.id
		LIMIT ?
	`,
		board,
		afterPreparedAt,
		afterPreparedAt,
		afterID,
		filter.Limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]PublicationEffectRecord, 0, filter.Limit)
	for rows.Next() {
		intent, err := scanPublicationEffectIntent(rows)
		if err != nil {
			return nil, err
		}
		parent, err := publicationAttemptIntent(
			ctx,
			s.db,
			intent.AttemptID,
		)
		if err != nil {
			return nil, err
		}
		if !validatePublicationEffectAgainstParent(intent, parent) {
			return nil, ErrPublicationEffectScope
		}
		values = append(values, PublicationEffectRecord{
			Intent: clonePublicationEffectIntent(intent),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

var _ json.Marshaler = (*PublicationEffectPermit)(nil)
