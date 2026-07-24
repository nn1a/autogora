package publicationeffect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// LocalRefCASInput describes one git update-ref compare-and-swap.
//
// Supply exactly one git-common-dir path or identity. Paths must already be
// canonical; constructors do not access the filesystem.
type LocalRefCASInput struct {
	GitCommonDirPath     string
	GitCommonDirIdentity string
	TargetRef            string
	BeforeOID            string
	AfterOID             string
}

type localRefCASTarget struct {
	GitCommonDirIdentity string `json:"gitCommonDirIdentity"`
	TargetRef            string `json:"targetRef"`
	BeforeOID            string `json:"beforeOid"`
	AfterOID             string `json:"afterOid"`
}

// NewLocalRefCAS creates a descriptor for git update-ref.
func NewLocalRefCAS(input LocalRefCASInput) (Descriptor, error) {
	identity, err := resolveLocalIdentity(
		input.GitCommonDirPath,
		input.GitCommonDirIdentity,
		GitCommonDirIdentityFromCanonicalPath,
		gitCommonDirIdentityPrefix,
		"git common dir identity",
	)
	if err != nil {
		return Descriptor{}, err
	}
	return newLocalRefCASFromTarget(localRefCASTarget{
		GitCommonDirIdentity: identity,
		TargetRef:            input.TargetRef,
		BeforeOID:            input.BeforeOID,
		AfterOID:             input.AfterOID,
	})
}

func newLocalRefCASFromTarget(target localRefCASTarget) (Descriptor, error) {
	if err := validateLocalIdentity(
		target.GitCommonDirIdentity,
		gitCommonDirIdentityPrefix,
		"git common dir identity",
	); err != nil {
		return Descriptor{}, err
	}
	if err := validateCanonicalRef(target.TargetRef, "target ref", false); err != nil {
		return Descriptor{}, err
	}
	if err := validateOID(target.BeforeOID, "before object ID"); err != nil {
		return Descriptor{}, err
	}
	if err := validateOID(target.AfterOID, "after object ID"); err != nil {
		return Descriptor{}, err
	}
	if err := validateSameOIDFormat(
		target.BeforeOID,
		target.AfterOID,
		"local ref CAS object IDs",
	); err != nil {
		return Descriptor{}, err
	}
	return newDescriptor(KindLocalRefCAS, target)
}

// LocalWorktreeFFInput describes one git merge --ff-only in a specific
// worktree and checked-out branch.
type LocalWorktreeFFInput struct {
	GitCommonDirPath     string
	GitCommonDirIdentity string
	GitDirPath           string
	GitDirIdentity       string
	WorktreePath         string
	WorktreeIdentity     string
	TargetRef            string
	BeforeOID            string
	AfterOID             string
}

type localWorktreeFFTarget struct {
	GitCommonDirIdentity string `json:"gitCommonDirIdentity"`
	GitDirIdentity       string `json:"gitDirIdentity"`
	WorktreeIdentity     string `json:"worktreeIdentity"`
	TargetRef            string `json:"targetRef"`
	BeforeOID            string `json:"beforeOid"`
	AfterOID             string `json:"afterOid"`
}

// NewLocalWorktreeFF creates a descriptor for git merge --ff-only.
func NewLocalWorktreeFF(input LocalWorktreeFFInput) (Descriptor, error) {
	gitIdentity, err := resolveLocalIdentity(
		input.GitCommonDirPath,
		input.GitCommonDirIdentity,
		GitCommonDirIdentityFromCanonicalPath,
		gitCommonDirIdentityPrefix,
		"git common dir identity",
	)
	if err != nil {
		return Descriptor{}, err
	}
	gitDirIdentity, err := resolveLocalIdentity(
		input.GitDirPath,
		input.GitDirIdentity,
		GitDirIdentityFromCanonicalPath,
		gitDirIdentityPrefix,
		"private git dir identity",
	)
	if err != nil {
		return Descriptor{}, err
	}
	worktreeIdentity, err := resolveLocalIdentity(
		input.WorktreePath,
		input.WorktreeIdentity,
		WorktreeIdentityFromCanonicalPath,
		worktreeIdentityPrefix,
		"worktree identity",
	)
	if err != nil {
		return Descriptor{}, err
	}
	return newLocalWorktreeFFFromTarget(localWorktreeFFTarget{
		GitCommonDirIdentity: gitIdentity,
		GitDirIdentity:       gitDirIdentity,
		WorktreeIdentity:     worktreeIdentity,
		TargetRef:            input.TargetRef,
		BeforeOID:            input.BeforeOID,
		AfterOID:             input.AfterOID,
	})
}

func newLocalWorktreeFFFromTarget(target localWorktreeFFTarget) (Descriptor, error) {
	if err := validateLocalIdentity(
		target.GitCommonDirIdentity,
		gitCommonDirIdentityPrefix,
		"git common dir identity",
	); err != nil {
		return Descriptor{}, err
	}
	if err := validateLocalIdentity(
		target.GitDirIdentity,
		gitDirIdentityPrefix,
		"private git dir identity",
	); err != nil {
		return Descriptor{}, err
	}
	if err := validateLocalIdentity(
		target.WorktreeIdentity,
		worktreeIdentityPrefix,
		"worktree identity",
	); err != nil {
		return Descriptor{}, err
	}
	if err := validateCanonicalRef(target.TargetRef, "target ref", true); err != nil {
		return Descriptor{}, err
	}
	if err := validateOID(target.BeforeOID, "before object ID"); err != nil {
		return Descriptor{}, err
	}
	if err := validateOID(target.AfterOID, "after object ID"); err != nil {
		return Descriptor{}, err
	}
	if err := validateSameOIDFormat(
		target.BeforeOID,
		target.AfterOID,
		"worktree fast-forward object IDs",
	); err != nil {
		return Descriptor{}, err
	}
	return newDescriptor(KindLocalWorktreeFF, target)
}

func resolveLocalIdentity(
	path string,
	identity string,
	fromPath func(string) (string, error),
	prefix string,
	field string,
) (string, error) {
	if (path == "") == (identity == "") {
		return "", fmt.Errorf(
			"supply exactly one canonical path or %s",
			field,
		)
	}
	if path != "" {
		return fromPath(path)
	}
	if err := validateLocalIdentity(identity, prefix, field); err != nil {
		return "", err
	}
	return identity, nil
}

// PRBranchPushInput describes one exact git push target. ExpectedAbsent and
// ExpectedOldOID are mutually exclusive CAS preconditions.
type PRBranchPushInput struct {
	RepositoryIdentity string
	RemoteURL          string
	SourceOID          string
	TargetRef          string
	ExpectedAbsent     bool
	ExpectedOldOID     string
}

type pushCAS struct {
	Mode string `json:"mode"`
	OID  string `json:"oid,omitempty"`
}

type prBranchPushTarget struct {
	RepositoryIdentity string       `json:"repositoryIdentity"`
	Remote             remoteTarget `json:"remote"`
	SourceOID          string       `json:"sourceOid"`
	TargetRef          string       `json:"targetRef"`
	Expected           pushCAS      `json:"expected"`
}

// NewPRBranchPush creates a descriptor for one git push.
func NewPRBranchPush(input PRBranchPushInput) (Descriptor, error) {
	remote, err := parseRemoteTarget(input.RemoteURL)
	if err != nil {
		return Descriptor{}, err
	}
	expected := pushCAS{Mode: "exact", OID: input.ExpectedOldOID}
	if input.ExpectedAbsent {
		if input.ExpectedOldOID != "" {
			return Descriptor{}, errors.New(
				"push target cannot expect both an absent and an exact old ref",
			)
		}
		expected = pushCAS{Mode: "absent"}
	} else if input.ExpectedOldOID == "" {
		return Descriptor{}, errors.New(
			"push target requires either absent or exact-old CAS",
		)
	}
	return newPRBranchPushFromTarget(prBranchPushTarget{
		RepositoryIdentity: input.RepositoryIdentity,
		Remote:             remote,
		SourceOID:          input.SourceOID,
		TargetRef:          input.TargetRef,
		Expected:           expected,
	})
}

func newPRBranchPushFromTarget(target prBranchPushTarget) (Descriptor, error) {
	if err := validateRepositoryIdentity(target.RepositoryIdentity); err != nil {
		return Descriptor{}, err
	}
	if err := validateRemoteTarget(target.Remote); err != nil {
		return Descriptor{}, err
	}
	if target.RepositoryIdentity != target.Remote.repositoryIdentity() {
		return Descriptor{}, errors.New(
			"push remote does not match the explicit repository identity",
		)
	}
	if err := validateOID(target.SourceOID, "source object ID"); err != nil {
		return Descriptor{}, err
	}
	if err := validateCanonicalRef(target.TargetRef, "target ref", true); err != nil {
		return Descriptor{}, err
	}
	switch target.Expected.Mode {
	case "absent":
		if target.Expected.OID != "" {
			return Descriptor{}, errors.New(
				"absent push CAS cannot include an old object ID",
			)
		}
	case "exact":
		if err := validateOID(
			target.Expected.OID,
			"expected old object ID",
		); err != nil {
			return Descriptor{}, err
		}
		if err := validateSameOIDFormat(
			target.SourceOID,
			target.Expected.OID,
			"push object IDs",
		); err != nil {
			return Descriptor{}, err
		}
	default:
		return Descriptor{}, errors.New(
			"push CAS mode must be absent or exact",
		)
	}
	return newDescriptor(KindPRBranchPush, target)
}

func validateRemoteTarget(target remoteTarget) error {
	if target.Transport != "https" && target.Transport != "ssh" {
		return errors.New("remote target transport must be https or ssh")
	}
	host, port, err := canonicalRemoteHost(
		target.Host,
		portString(target.Port),
		target.Transport,
	)
	if err != nil {
		return err
	}
	if host != target.Host || port != target.Port {
		return errors.New("remote target host is not canonical")
	}
	path, err := canonicalRepositoryPath(target.RepositoryPath)
	if err != nil {
		return err
	}
	if path != target.RepositoryPath {
		return errors.New("remote target repository path is not canonical")
	}
	return nil
}

func portString(port uint16) string {
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("%d", port)
}

// BodyDigest is the only PR body material retained by a descriptor.
type BodyDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// DigestPRBody validates a bounded textual PR body and returns its digest.
// Newlines and tabs are allowed; NUL and other control characters are not.
func DigestPRBody(body []byte) (BodyDigest, error) {
	if len(body) == 0 {
		return BodyDigest{}, errors.New("pull request body cannot be empty")
	}
	if len(body) > MaxPRBodyBytes {
		return BodyDigest{}, fmt.Errorf(
			"pull request body must be at most %d bytes",
			MaxPRBodyBytes,
		)
	}
	if !utf8.Valid(body) {
		return BodyDigest{}, errors.New("pull request body must be valid UTF-8")
	}
	for _, current := range string(body) {
		if current == 0 {
			return BodyDigest{}, errors.New("pull request body cannot contain NUL")
		}
		if unicode.IsControl(current) &&
			current != '\n' && current != '\r' && current != '\t' {
			return BodyDigest{}, errors.New(
				"pull request body cannot contain non-text control characters",
			)
		}
	}
	sum := sha256.Sum256(body)
	return BodyDigest{
		SHA256: hex.EncodeToString(sum[:]),
		Bytes:  int64(len(body)),
	}, nil
}

// PRCreateInput describes one gh pr create target. BodyDigest must be produced
// by DigestPRBody; the raw body is never retained.
type PRCreateInput struct {
	RepositoryIdentity string
	BaseRef            string
	HeadRef            string
	Title              string
	BodyDigest         BodyDigest
	ExpectedHeadOID    string
}

type prCreateTarget struct {
	RepositoryIdentity string     `json:"repositoryIdentity"`
	BaseRef            string     `json:"baseRef"`
	HeadRef            string     `json:"headRef"`
	Title              string     `json:"title"`
	BodyDigest         BodyDigest `json:"bodyDigest"`
	ExpectedHeadOID    string     `json:"expectedHeadOid"`
}

// NewPRCreate creates a descriptor for gh pr create against an explicit repo.
func NewPRCreate(input PRCreateInput) (Descriptor, error) {
	return newPRCreateFromTarget(prCreateTarget{
		RepositoryIdentity: input.RepositoryIdentity,
		BaseRef:            input.BaseRef,
		HeadRef:            input.HeadRef,
		Title:              input.Title,
		BodyDigest:         input.BodyDigest,
		ExpectedHeadOID:    input.ExpectedHeadOID,
	})
}

func newPRCreateFromTarget(target prCreateTarget) (Descriptor, error) {
	if err := validatePRRepositoryIdentity(target.RepositoryIdentity); err != nil {
		return Descriptor{}, err
	}
	if err := validateCanonicalRef(target.BaseRef, "base ref", true); err != nil {
		return Descriptor{}, err
	}
	if err := validateCanonicalRef(target.HeadRef, "head ref", true); err != nil {
		return Descriptor{}, err
	}
	if err := validateTitle(target.Title); err != nil {
		return Descriptor{}, err
	}
	if !validSHA256(target.BodyDigest.SHA256) {
		return Descriptor{}, errors.New(
			"pull request body must have a lowercase SHA-256 digest",
		)
	}
	if target.BodyDigest.Bytes <= 0 ||
		target.BodyDigest.Bytes > MaxPRBodyBytes {
		return Descriptor{}, fmt.Errorf(
			"pull request body size must be between 1 and %d bytes",
			MaxPRBodyBytes,
		)
	}
	if err := validateOID(target.ExpectedHeadOID, "expected head object ID"); err != nil {
		return Descriptor{}, err
	}
	return newDescriptor(KindPRCreate, target)
}
