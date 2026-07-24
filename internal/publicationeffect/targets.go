package publicationeffect

import (
	"errors"
	"fmt"
)

var ErrDescriptorKindMismatch = errors.New(
	"publication effect descriptor kind does not match the requested target",
)

// LocalRefCASTarget is the credential-free, typed view of one local ref
// compare-and-swap. It contains identities rather than filesystem paths.
type LocalRefCASTarget struct {
	GitCommonDirIdentity string
	TargetRef            string
	BeforeOID            string
	AfterOID             string
}

// LocalWorktreeFFTarget is the credential-free, typed view of one checked-out
// worktree fast-forward. Its three identities distinguish the repository,
// linked-worktree metadata, and worktree path without persisting those paths.
type LocalWorktreeFFTarget struct {
	GitCommonDirIdentity string
	GitDirIdentity       string
	WorktreeIdentity     string
	TargetRef            string
	BeforeOID            string
	AfterOID             string
}

// RemoteTarget is the canonical, credential-free remote bound into a
// publication effect. RepositoryPath never has a leading slash or .git suffix.
type RemoteTarget struct {
	Transport      string
	Host           string
	Port           uint16
	RepositoryPath string
}

// PRBranchPushTarget is the typed view of an exact remote ref update.
// ExpectedAbsent and ExpectedOldOID remain mutually exclusive.
type PRBranchPushTarget struct {
	RepositoryIdentity string
	Remote             RemoteTarget
	SourceOID          string
	TargetRef          string
	ExpectedAbsent     bool
	ExpectedOldOID     string
}

// PRCreateTarget is the typed view of one pull-request creation effect. Only
// the body digest and size are retained; the raw body is never exposed.
type PRCreateTarget struct {
	RepositoryIdentity string
	BaseRef            string
	HeadRef            string
	Title              string
	BodyDigest         BodyDigest
	ExpectedHeadOID    string
}

// LocalRefCASTarget returns the validated target of a local-ref descriptor.
func (d Descriptor) LocalRefCASTarget() (LocalRefCASTarget, error) {
	var target localRefCASTarget
	if err := d.decodeTarget(KindLocalRefCAS, &target); err != nil {
		return LocalRefCASTarget{}, err
	}
	return LocalRefCASTarget{
		GitCommonDirIdentity: target.GitCommonDirIdentity,
		TargetRef:            target.TargetRef,
		BeforeOID:            target.BeforeOID,
		AfterOID:             target.AfterOID,
	}, nil
}

// LocalWorktreeFFTarget returns the validated target of a worktree
// fast-forward descriptor.
func (d Descriptor) LocalWorktreeFFTarget() (LocalWorktreeFFTarget, error) {
	var target localWorktreeFFTarget
	if err := d.decodeTarget(KindLocalWorktreeFF, &target); err != nil {
		return LocalWorktreeFFTarget{}, err
	}
	return LocalWorktreeFFTarget{
		GitCommonDirIdentity: target.GitCommonDirIdentity,
		GitDirIdentity:       target.GitDirIdentity,
		WorktreeIdentity:     target.WorktreeIdentity,
		TargetRef:            target.TargetRef,
		BeforeOID:            target.BeforeOID,
		AfterOID:             target.AfterOID,
	}, nil
}

// PRBranchPushTarget returns the validated target of a remote branch push
// descriptor.
func (d Descriptor) PRBranchPushTarget() (PRBranchPushTarget, error) {
	var target prBranchPushTarget
	if err := d.decodeTarget(KindPRBranchPush, &target); err != nil {
		return PRBranchPushTarget{}, err
	}
	return PRBranchPushTarget{
		RepositoryIdentity: target.RepositoryIdentity,
		Remote: RemoteTarget{
			Transport:      target.Remote.Transport,
			Host:           target.Remote.Host,
			Port:           target.Remote.Port,
			RepositoryPath: target.Remote.RepositoryPath,
		},
		SourceOID:      target.SourceOID,
		TargetRef:      target.TargetRef,
		ExpectedAbsent: target.Expected.Mode == "absent",
		ExpectedOldOID: target.Expected.OID,
	}, nil
}

// PRCreateTarget returns the validated target of a pull-request creation
// descriptor.
func (d Descriptor) PRCreateTarget() (PRCreateTarget, error) {
	var target prCreateTarget
	if err := d.decodeTarget(KindPRCreate, &target); err != nil {
		return PRCreateTarget{}, err
	}
	return PRCreateTarget{
		RepositoryIdentity: target.RepositoryIdentity,
		BaseRef:            target.BaseRef,
		HeadRef:            target.HeadRef,
		Title:              target.Title,
		BodyDigest:         target.BodyDigest,
		ExpectedHeadOID:    target.ExpectedHeadOID,
	}, nil
}

func (d Descriptor) decodeTarget(expected Kind, target any) error {
	if err := d.valid(); err != nil {
		return err
	}
	if d.kind != expected {
		return fmt.Errorf(
			"%w: got %s, want %s",
			ErrDescriptorKindMismatch,
			d.kind,
			expected,
		)
	}
	parsed, err := ParseCanonical(d.canonical)
	if err != nil {
		return err
	}
	if parsed.kind != d.kind || parsed.fingerprint != d.fingerprint {
		return errors.New(
			"publication effect descriptor target does not match its identity",
		)
	}
	var envelope canonicalEnvelope
	if err := decodeStrictJSON(parsed.canonical, &envelope); err != nil {
		return fmt.Errorf("decode publication effect target envelope: %w", err)
	}
	if envelope.Kind != expected {
		return errors.New(
			"publication effect descriptor target kind is inconsistent",
		)
	}
	if err := decodeStrictJSON(envelope.Target, target); err != nil {
		return fmt.Errorf("decode %s publication effect target: %w", expected, err)
	}
	return nil
}
