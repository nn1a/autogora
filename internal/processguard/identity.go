package processguard

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	// DurableIdentityVersion identifies the kernel identity fields captured for
	// one fenced Linux guard.
	DurableIdentityVersion = 2
	// DurableTeardownReceiptVersion identifies the fixed on-disk receipt
	// written only after the guard has proved its descendant set quiescent.
	DurableTeardownReceiptVersion = 2

	durableIdentifierBytes        = 32
	durableReceiptPublicKeyBytes  = ed25519.PublicKeySize
	durableReceiptSignatureBytes  = ed25519.SignatureSize
	maxDurableReceiptBytes        = 2048
	canonicalBootIDTextBytes      = 36
	durableReceiptSignatureDomain = "autogora.processguard.durable-teardown-receipt.v2\x00"
)

var (
	// ErrDurableProcessIdentityUnavailable means this platform cannot expose a
	// restart-safe identity for a fenced guard.
	ErrDurableProcessIdentityUnavailable = errors.New(
		"durable process identity is unavailable",
	)
	// ErrDurableTeardownReceiptUnavailable means this platform cannot write a
	// restart-safe teardown receipt.
	ErrDurableTeardownReceiptUnavailable = errors.New(
		"durable teardown receipt is unavailable",
	)
	// ErrDurableReceiptAlreadyClaimed means a one-shot receipt configuration
	// or its underlying inode has already been reserved for another command.
	ErrDurableReceiptAlreadyClaimed = errors.New(
		"durable teardown receipt is already claimed",
	)
	// ErrDurableReceiptIdentityMismatch means a valid receipt belongs to a
	// different fenced execution and must not authorize recovery.
	ErrDurableReceiptIdentityMismatch = errors.New(
		"durable teardown receipt identity does not match the expected execution",
	)
	// ErrDurableReceiptSignatureInvalid means the receipt does not carry a
	// valid signature from the guard key published in its durable identity.
	ErrDurableReceiptSignatureInvalid = errors.New(
		"durable teardown receipt signature is invalid",
	)
	// ErrExactProcessSignalUnavailable means the host cannot bind a signal to
	// the exact durable guard process without a reusable numeric PID.
	ErrExactProcessSignalUnavailable = errors.New(
		"exact durable process signal is unavailable",
	)
	// ErrDurableProcessIdentityChanged means the PID currently visible to the
	// host no longer matches the durable guard identity.
	ErrDurableProcessIdentityChanged = errors.New(
		"durable process identity changed",
	)
)

// DurableIdentity binds a fenced guard PID to one kernel process instance.
// BootID and the PID namespace prevent a PID/start-tick tuple from being
// confused across reboot or namespace replacement. ExecutionID and ReceiptID
// bind the process to one command effect and one pre-opened receipt file.
//
// The structure contains no argv, environment, filesystem path, or credential
// material and is safe to persist.
type DurableIdentity struct {
	Version            int    `json:"version"`
	BootID             string `json:"bootId"`
	PIDNamespaceDevice uint64 `json:"pidNamespaceDevice"`
	PIDNamespaceInode  uint64 `json:"pidNamespaceInode"`
	GuardPID           int    `json:"guardPid"`
	StartTimeTicks     uint64 `json:"startTimeTicks"`
	ProcessGroupID     int    `json:"processGroupId"`
	ExecutionID        string `json:"executionId"`
	ReceiptID          string `json:"receiptId"`
	ReceiptPublicKey   string `json:"receiptPublicKey"`
}

// Validate checks the complete canonical durable identity contract without
// consulting live process state.
func (identity DurableIdentity) Validate() error {
	if identity.Version != DurableIdentityVersion {
		return fmt.Errorf(
			"unsupported durable process identity version %d",
			identity.Version,
		)
	}
	if !validCanonicalBootID(identity.BootID) {
		return errors.New("durable process identity boot ID is not canonical")
	}
	if identity.PIDNamespaceDevice == 0 ||
		identity.PIDNamespaceInode == 0 {
		return errors.New(
			"durable process identity requires a PID namespace device and inode",
		)
	}
	if identity.GuardPID <= 0 {
		return errors.New("durable process identity guard PID must be positive")
	}
	if identity.StartTimeTicks == 0 {
		return errors.New(
			"durable process identity start-time ticks must be positive",
		)
	}
	if identity.ProcessGroupID != identity.GuardPID {
		return errors.New(
			"durable process identity process group must equal the guard PID",
		)
	}
	if err := validateDurableIdentifier(
		identity.ExecutionID,
		"durable process execution ID",
	); err != nil {
		return err
	}
	if err := validateDurableIdentifier(
		identity.ReceiptID,
		"durable process receipt ID",
	); err != nil {
		return err
	}
	if identity.ExecutionID == identity.ReceiptID {
		return errors.New(
			"durable process execution and receipt IDs must be domain-separated",
		)
	}
	if err := validateCanonicalHex(
		identity.ReceiptPublicKey,
		durableReceiptPublicKeyBytes,
		"durable receipt public key",
	); err != nil {
		return err
	}
	return nil
}

// DurableReceiptConfig binds a caller-owned, pre-opened empty regular file to
// one fenced command. The constructor seals its metadata; the fenced-command
// constructor later opens the independent private lease. The caller retains
// ownership of the original descriptor. Both IDs are safe random identifiers,
// not credentials.
type DurableReceiptConfig struct {
	File        *os.File
	ExecutionID string
	ReceiptID   string
	claim       *durableReceiptClaim
}

type durableReceiptClaim struct {
	mu          sync.Mutex
	consumed    bool
	file        *os.File
	fileInfo    os.FileInfo
	executionID string
	receiptID   string
}

// NewDurableIdentifier returns a lowercase 256-bit random identifier suitable
// for ExecutionID or ReceiptID.
func NewDurableIdentifier() (string, error) {
	var value [durableIdentifierBytes]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", fmt.Errorf("generate durable process identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// NewDurableReceiptConfig generates domain-separated identifiers for a
// pre-opened receipt file.
func NewDurableReceiptConfig(file *os.File) (DurableReceiptConfig, error) {
	executionID, err := NewDurableIdentifier()
	if err != nil {
		return DurableReceiptConfig{}, err
	}
	receiptID, err := NewDurableIdentifier()
	if err != nil {
		return DurableReceiptConfig{}, err
	}
	config := DurableReceiptConfig{
		File:        file,
		ExecutionID: executionID,
		ReceiptID:   receiptID,
	}
	config.claim = &durableReceiptClaim{
		file:        file,
		executionID: executionID,
		receiptID:   receiptID,
	}
	if err := config.validate(); err != nil {
		return DurableReceiptConfig{}, err
	}
	if err := validateDurableReceiptConfigPlatform(file); err != nil {
		return DurableReceiptConfig{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return DurableReceiptConfig{}, fmt.Errorf(
			"seal durable teardown receipt file identity: %w",
			err,
		)
	}
	config.claim.fileInfo = info
	return config, nil
}

func (config DurableReceiptConfig) validate() error {
	if config.claim == nil {
		return errors.New(
			"durable teardown receipt config must be created by NewDurableReceiptConfig",
		)
	}
	if config.File != config.claim.file ||
		config.ExecutionID != config.claim.executionID ||
		config.ReceiptID != config.claim.receiptID {
		return errors.New(
			"durable teardown receipt config changed after creation",
		)
	}
	if config.File == nil {
		return errors.New("durable teardown receipt file is required")
	}
	if err := validateDurableIdentifier(
		config.ExecutionID,
		"durable process execution ID",
	); err != nil {
		return err
	}
	if err := validateDurableIdentifier(
		config.ReceiptID,
		"durable process receipt ID",
	); err != nil {
		return err
	}
	if config.ExecutionID == config.ReceiptID {
		return errors.New(
			"durable process execution and receipt IDs must be domain-separated",
		)
	}
	info, err := config.File.Stat()
	if err != nil {
		return fmt.Errorf("inspect durable teardown receipt file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New(
			"durable teardown receipt must be a pre-opened regular file",
		)
	}
	if info.Size() != 0 {
		return errors.New("durable teardown receipt file must be empty")
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		return errors.New(
			"durable teardown receipt file permissions must be 0600",
		)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New(
			"durable teardown receipt file cannot use special permission bits",
		)
	}
	if config.claim.fileInfo != nil &&
		!os.SameFile(config.claim.fileInfo, info) {
		return errors.New(
			"durable teardown receipt file identity changed after creation",
		)
	}
	return nil
}

func (config DurableReceiptConfig) consume() error {
	if config.claim == nil {
		return errors.New(
			"durable teardown receipt config must be created by NewDurableReceiptConfig",
		)
	}
	config.claim.mu.Lock()
	defer config.claim.mu.Unlock()
	if config.claim.consumed {
		return ErrDurableReceiptAlreadyClaimed
	}
	config.claim.consumed = true
	if config.File != config.claim.file ||
		config.ExecutionID != config.claim.executionID ||
		config.ReceiptID != config.claim.receiptID {
		return errors.New(
			"durable teardown receipt config changed after creation",
		)
	}
	return config.validate()
}

// DurableTeardownReceipt is written by the guard only after it has proved
// that no guarded descendant remains. Released means the guard consumed the
// fence byte, not merely that FencedCommand.Release wrote it. A true return
// from Release must therefore never substitute for this durable observation.
type DurableTeardownReceipt struct {
	Version   int             `json:"version"`
	Identity  DurableIdentity `json:"identity"`
	Released  bool            `json:"released"`
	Quiescent bool            `json:"quiescent"`
	Signature string          `json:"signature"`
}

type durableTeardownReceiptPayload struct {
	Version   int             `json:"version"`
	Identity  DurableIdentity `json:"identity"`
	Released  bool            `json:"released"`
	Quiescent bool            `json:"quiescent"`
}

func (receipt DurableTeardownReceipt) payload() durableTeardownReceiptPayload {
	return durableTeardownReceiptPayload{
		Version:   receipt.Version,
		Identity:  receipt.Identity,
		Released:  receipt.Released,
		Quiescent: receipt.Quiescent,
	}
}

func (receipt DurableTeardownReceipt) validatePayload() error {
	if receipt.Version != DurableTeardownReceiptVersion {
		return fmt.Errorf(
			"unsupported durable teardown receipt version %d",
			receipt.Version,
		)
	}
	if err := receipt.Identity.Validate(); err != nil {
		return fmt.Errorf("validate durable teardown receipt identity: %w", err)
	}
	if !receipt.Quiescent {
		return errors.New(
			"durable teardown receipt does not attest quiescence",
		)
	}
	return nil
}

func (receipt DurableTeardownReceipt) signedMessage() ([]byte, error) {
	if err := receipt.validatePayload(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(receipt.payload())
	if err != nil {
		return nil, fmt.Errorf(
			"encode durable teardown receipt signature payload: %w",
			err,
		)
	}
	message := make(
		[]byte,
		0,
		len(durableReceiptSignatureDomain)+len(payload),
	)
	message = append(message, durableReceiptSignatureDomain...)
	message = append(message, payload...)
	return message, nil
}

func signDurableTeardownReceipt(
	receipt DurableTeardownReceipt,
	privateKey ed25519.PrivateKey,
) (DurableTeardownReceipt, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return DurableTeardownReceipt{}, errors.New(
			"durable teardown receipt signing key is invalid",
		)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if receipt.Identity.ReceiptPublicKey != hex.EncodeToString(publicKey) {
		return DurableTeardownReceipt{}, errors.New(
			"durable teardown receipt signing key does not match the identity",
		)
	}
	message, err := receipt.signedMessage()
	if err != nil {
		return DurableTeardownReceipt{}, err
	}
	receipt.Signature = hex.EncodeToString(ed25519.Sign(privateKey, message))
	return receipt, nil
}

func (receipt DurableTeardownReceipt) verifySignature() error {
	if err := receipt.validatePayload(); err != nil {
		return err
	}
	publicKey, err := decodeCanonicalHex(
		receipt.Identity.ReceiptPublicKey,
		durableReceiptPublicKeyBytes,
		"durable receipt public key",
	)
	if err != nil {
		return err
	}
	if err := validateCanonicalHex(
		receipt.Signature,
		durableReceiptSignatureBytes,
		"durable teardown receipt signature",
	); err != nil {
		return err
	}
	signature, err := hex.DecodeString(receipt.Signature)
	if err != nil {
		return err
	}
	message, err := receipt.signedMessage()
	if err != nil {
		return err
	}
	if !ed25519.Verify(
		ed25519.PublicKey(publicKey),
		message,
		signature,
	) {
		return ErrDurableReceiptSignatureInvalid
	}
	return nil
}

// CanonicalJSON returns the fixed, compact receipt encoding after verifying
// its guard signature.
func (receipt DurableTeardownReceipt) CanonicalJSON() ([]byte, error) {
	if err := receipt.verifySignature(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("encode durable teardown receipt: %w", err)
	}
	if len(encoded) > maxDurableReceiptBytes {
		return nil, errors.New("durable teardown receipt is oversized")
	}
	return encoded, nil
}

// ParseDurableTeardownReceipt accepts only the exact fixed-schema encoding.
// Unknown fields, whitespace variants, reordered fields, duplicate fields,
// and trailing JSON are rejected by reconstructing the canonical bytes.
func ParseDurableTeardownReceipt(
	raw []byte,
) (DurableTeardownReceipt, error) {
	if len(raw) == 0 {
		return DurableTeardownReceipt{},
			errors.New("durable teardown receipt is empty")
	}
	if len(raw) > maxDurableReceiptBytes {
		return DurableTeardownReceipt{},
			errors.New("durable teardown receipt is oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var receipt DurableTeardownReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return DurableTeardownReceipt{},
			fmt.Errorf("decode durable teardown receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return DurableTeardownReceipt{},
				errors.New("durable teardown receipt contains multiple JSON values")
		}
		return DurableTeardownReceipt{},
			fmt.Errorf("decode durable teardown receipt trailing data: %w", err)
	}
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		return DurableTeardownReceipt{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return DurableTeardownReceipt{},
			errors.New("durable teardown receipt is not canonical")
	}
	return receipt, nil
}

// ParseDurableTeardownReceiptForIdentity parses the strict fixed schema and
// requires every identity field to match the caller's persisted expectation.
// Recovery callers should prefer this function over an unbound Parse.
func ParseDurableTeardownReceiptForIdentity(
	raw []byte,
	expected DurableIdentity,
) (DurableTeardownReceipt, error) {
	if err := expected.Validate(); err != nil {
		return DurableTeardownReceipt{}, fmt.Errorf(
			"validate expected durable process identity: %w",
			err,
		)
	}
	receipt, err := ParseDurableTeardownReceipt(raw)
	if err != nil {
		return DurableTeardownReceipt{}, err
	}
	if receipt.Identity != expected {
		return DurableTeardownReceipt{},
			ErrDurableReceiptIdentityMismatch
	}
	return receipt, nil
}

func canonicalDurableIdentityJSON(
	identity DurableIdentity,
) ([]byte, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("encode durable process identity: %w", err)
	}
	if len(encoded) > maxDurableReceiptBytes {
		return nil, errors.New("durable process identity is oversized")
	}
	return encoded, nil
}

func parseCanonicalDurableIdentity(
	raw []byte,
) (DurableIdentity, error) {
	if len(raw) == 0 {
		return DurableIdentity{}, errors.New(
			"durable process identity handshake is empty",
		)
	}
	if len(raw) > maxDurableReceiptBytes {
		return DurableIdentity{}, errors.New(
			"durable process identity handshake is oversized",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var identity DurableIdentity
	if err := decoder.Decode(&identity); err != nil {
		return DurableIdentity{}, fmt.Errorf(
			"decode durable process identity handshake: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return DurableIdentity{}, errors.New(
				"durable process identity handshake contains multiple JSON values",
			)
		}
		return DurableIdentity{}, fmt.Errorf(
			"decode durable process identity handshake trailing data: %w",
			err,
		)
	}
	canonical, err := canonicalDurableIdentityJSON(identity)
	if err != nil {
		return DurableIdentity{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return DurableIdentity{}, errors.New(
			"durable process identity handshake is not canonical",
		)
	}
	return identity, nil
}

// DurableProcessObservation classifies the current kernel view of a persisted
// identity. Only ExactLive identifies the original guard. Absent and Reused
// do not prove descendant teardown; callers still need a valid receipt or a
// DifferentBoot observation before probing an external effect.
type DurableProcessObservation string

const (
	DurableProcessExactLive          DurableProcessObservation = "exact_live"
	DurableProcessAbsent             DurableProcessObservation = "absent"
	DurableProcessReused             DurableProcessObservation = "reused"
	DurableProcessDifferentBoot      DurableProcessObservation = "different_boot"
	DurableProcessDifferentNamespace DurableProcessObservation = "different_namespace"
)

func validateDurableIdentifier(value, field string) error {
	return validateCanonicalHex(value, durableIdentifierBytes, field)
}

func decodeCanonicalHex(
	value string,
	byteLength int,
	field string,
) ([]byte, error) {
	if len(value) != byteLength*2 || value != strings.ToLower(value) {
		return nil, fmt.Errorf(
			"%s must be a lowercase %d-bit hexadecimal value",
			field,
			byteLength*8,
		)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != byteLength {
		return nil, fmt.Errorf(
			"%s must be a lowercase %d-bit hexadecimal value",
			field,
			byteLength*8,
		)
	}
	return decoded, nil
}

func validateCanonicalHex(value string, byteLength int, field string) error {
	decoded, err := decodeCanonicalHex(value, byteLength, field)
	if err != nil {
		return err
	}
	allZero := true
	for _, current := range decoded {
		if current != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return fmt.Errorf("%s cannot be all zero", field)
	}
	return nil
}

func validCanonicalBootID(value string) bool {
	if len(value) != canonicalBootIDTextBytes ||
		value != strings.ToLower(value) {
		return false
	}
	for index, current := range value {
		switch index {
		case 8, 13, 18, 23:
			if current != '-' {
				return false
			}
		default:
			if (current < '0' || current > '9') &&
				(current < 'a' || current > 'f') {
				return false
			}
		}
	}
	return value != "00000000-0000-0000-0000-000000000000"
}
