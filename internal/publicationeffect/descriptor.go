// Package publicationeffect defines durable, secret-free identities for the
// external mutations a publication may perform.
//
// Descriptors are intentionally not command lines. They bind the semantic
// target and compare-and-swap preconditions of one mutation without retaining
// credentials, PR bodies, or raw argv. Constructors validate and canonicalize
// every descriptor before it can be fingerprinted or persisted.
package publicationeffect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// SchemaVersion is included in every canonical descriptor.
	SchemaVersion = 1

	// MaxCanonicalJSONBytes bounds descriptors accepted from durable storage.
	MaxCanonicalJSONBytes = 16 * 1024
	// MaxPRBodyBytes bounds a body before it is reduced to its digest.
	MaxPRBodyBytes = 1024 * 1024
)

// Kind identifies one of the only mutating command effects tracked by the
// publication pipeline.
type Kind string

const (
	KindLocalRefCAS     Kind = "local_ref_cas"
	KindLocalWorktreeFF Kind = "local_worktree_ff"
	KindPRBranchPush    Kind = "pr_branch_push"
	KindPRCreate        Kind = "pr_create"
)

var mutatingKinds = [...]Kind{
	KindLocalRefCAS,
	KindLocalWorktreeFF,
	KindPRBranchPush,
	KindPRCreate,
}

// Kinds returns the complete, stable set of publication mutation kinds.
func Kinds() []Kind {
	result := make([]Kind, len(mutatingKinds))
	copy(result, mutatingKinds[:])
	return result
}

// Descriptor is an immutable-by-API canonical mutation description.
//
// Its JSON representation is safe to persist: it never contains a raw command
// line, remote credentials, a local filesystem path, or a PR body.
type Descriptor struct {
	kind        Kind
	canonical   []byte
	fingerprint string
}

// Kind returns the mutation kind.
func (d Descriptor) Kind() Kind {
	return d.kind
}

// Version returns the canonical descriptor schema version.
func (d Descriptor) Version() int {
	if len(d.canonical) == 0 {
		return 0
	}
	return SchemaVersion
}

// CanonicalJSON returns an independent copy of the versioned canonical JSON.
func (d Descriptor) CanonicalJSON() []byte {
	return append([]byte(nil), d.canonical...)
}

// Fingerprint returns the lowercase SHA-256 digest of CanonicalJSON.
func (d Descriptor) Fingerprint() string {
	return d.fingerprint
}

// MarshalJSON emits the already validated canonical representation.
func (d Descriptor) MarshalJSON() ([]byte, error) {
	if err := d.valid(); err != nil {
		return nil, err
	}
	return d.CanonicalJSON(), nil
}

func (d Descriptor) valid() error {
	if !validKind(d.kind) || len(d.canonical) == 0 ||
		len(d.canonical) > MaxCanonicalJSONBytes {
		return errors.New("publication effect descriptor is uninitialized")
	}
	sum := sha256.Sum256(d.canonical)
	if d.fingerprint != hex.EncodeToString(sum[:]) {
		return errors.New("publication effect descriptor fingerprint is invalid")
	}
	return nil
}

type canonicalEnvelope struct {
	Version int             `json:"version"`
	Kind    Kind            `json:"kind"`
	Target  json.RawMessage `json:"target"`
}

func newDescriptor(kind Kind, target any) (Descriptor, error) {
	if !validKind(kind) {
		return Descriptor{}, fmt.Errorf("unsupported publication effect kind %q", kind)
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		return Descriptor{}, fmt.Errorf("encode publication effect target: %w", err)
	}
	canonical, err := json.Marshal(canonicalEnvelope{
		Version: SchemaVersion,
		Kind:    kind,
		Target:  targetJSON,
	})
	if err != nil {
		return Descriptor{}, fmt.Errorf("encode publication effect descriptor: %w", err)
	}
	if len(canonical) > MaxCanonicalJSONBytes {
		return Descriptor{}, fmt.Errorf(
			"publication effect descriptor must be at most %d bytes",
			MaxCanonicalJSONBytes,
		)
	}
	sum := sha256.Sum256(canonical)
	return Descriptor{
		kind:        kind,
		canonical:   canonical,
		fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// ParseCanonical validates a descriptor read from durable storage.
//
// The input must be byte-for-byte canonical. Whitespace variants, reordered
// fields, unknown fields (including argv), and trailing JSON are rejected so
// one semantic effect has exactly one fingerprint.
func ParseCanonical(raw []byte) (Descriptor, error) {
	if len(raw) == 0 {
		return Descriptor{}, errors.New("publication effect descriptor cannot be empty")
	}
	if len(raw) > MaxCanonicalJSONBytes {
		return Descriptor{}, fmt.Errorf(
			"publication effect descriptor must be at most %d bytes",
			MaxCanonicalJSONBytes,
		)
	}
	var envelope canonicalEnvelope
	if err := decodeStrictJSON(raw, &envelope); err != nil {
		return Descriptor{}, fmt.Errorf("decode publication effect descriptor: %w", err)
	}
	if envelope.Version != SchemaVersion {
		return Descriptor{}, fmt.Errorf(
			"unsupported publication effect descriptor version %d",
			envelope.Version,
		)
	}

	var descriptor Descriptor
	var err error
	switch envelope.Kind {
	case KindLocalRefCAS:
		var target localRefCASTarget
		if err = decodeStrictJSON(envelope.Target, &target); err == nil {
			descriptor, err = newLocalRefCASFromTarget(target)
		}
	case KindLocalWorktreeFF:
		var target localWorktreeFFTarget
		if err = decodeStrictJSON(envelope.Target, &target); err == nil {
			descriptor, err = newLocalWorktreeFFFromTarget(target)
		}
	case KindPRBranchPush:
		var target prBranchPushTarget
		if err = decodeStrictJSON(envelope.Target, &target); err == nil {
			descriptor, err = newPRBranchPushFromTarget(target)
		}
	case KindPRCreate:
		var target prCreateTarget
		if err = decodeStrictJSON(envelope.Target, &target); err == nil {
			descriptor, err = newPRCreateFromTarget(target)
		}
	default:
		return Descriptor{}, fmt.Errorf(
			"unsupported publication effect kind %q",
			envelope.Kind,
		)
	}
	if err != nil {
		return Descriptor{}, fmt.Errorf(
			"validate %s publication effect target: %w",
			envelope.Kind,
			err,
		)
	}
	if !bytes.Equal(raw, descriptor.canonical) {
		return Descriptor{}, errors.New(
			"publication effect descriptor is not canonical JSON",
		)
	}
	return descriptor, nil
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func validKind(kind Kind) bool {
	switch kind {
	case KindLocalRefCAS, KindLocalWorktreeFF, KindPRBranchPush, KindPRCreate:
		return true
	default:
		return false
	}
}
