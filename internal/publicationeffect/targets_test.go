package publicationeffect

import (
	"errors"
	"reflect"
	"testing"
)

func TestDescriptorTypedTargetsRoundTripCanonicalState(t *testing.T) {
	gitCommonDir := "/srv/repository/.git"
	gitDir := "/srv/repository/.git/worktrees/finalizer"
	worktree := "/srv/worktrees/finalizer"
	gitCommonDirIdentity, err := GitCommonDirIdentityFromCanonicalPath(
		gitCommonDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	gitDirIdentity, err := GitDirIdentityFromCanonicalPath(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	worktreeIdentity, err := WorktreeIdentityFromCanonicalPath(worktree)
	if err != nil {
		t.Fatal(err)
	}
	body, err := DigestPRBody([]byte("Verified finalizer output.\n"))
	if err != nil {
		t.Fatal(err)
	}
	repositoryIdentity, err := RepositoryIdentityFromRemote(
		"ssh://git@example.test:2222/team/game.git",
	)
	if err != nil {
		t.Fatal(err)
	}

	localRef, err := NewLocalRefCAS(LocalRefCASInput{
		GitCommonDirPath: gitCommonDir,
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA1A,
		AfterOID:         testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}
	localWorktree, err := NewLocalWorktreeFF(LocalWorktreeFFInput{
		GitCommonDirPath: gitCommonDir,
		GitDirPath:       gitDir,
		WorktreePath:     worktree,
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA256A,
		AfterOID:         testSHA256B,
	})
	if err != nil {
		t.Fatal(err)
	}
	push, err := NewPRBranchPush(PRBranchPushInput{
		RepositoryIdentity: repositoryIdentity,
		RemoteURL:          "ssh://git@example.test:2222/team/game.git",
		SourceOID:          testSHA1B,
		TargetRef:          "refs/heads/autogora/demo",
		ExpectedOldOID:     testSHA1A,
	})
	if err != nil {
		t.Fatal(err)
	}
	create, err := NewPRCreate(PRCreateInput{
		RepositoryIdentity: repositoryIdentity,
		BaseRef:            "refs/heads/main",
		HeadRef:            "refs/heads/autogora/demo",
		Title:              "Publish cooperative game",
		BodyDigest:         body,
		ExpectedHeadOID:    testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Exercise views on descriptors reconstructed from durable storage, not
	// only on values returned directly by constructors.
	for _, descriptor := range []*Descriptor{
		&localRef,
		&localWorktree,
		&push,
		&create,
	} {
		parsed, parseErr := ParseCanonical(descriptor.CanonicalJSON())
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		*descriptor = parsed
	}

	gotLocalRef, err := localRef.LocalRefCASTarget()
	if err != nil {
		t.Fatal(err)
	}
	if want := (LocalRefCASTarget{
		GitCommonDirIdentity: gitCommonDirIdentity,
		TargetRef:            "refs/heads/main",
		BeforeOID:            testSHA1A,
		AfterOID:             testSHA1B,
	}); !reflect.DeepEqual(gotLocalRef, want) {
		t.Fatalf("local ref target = %#v, want %#v", gotLocalRef, want)
	}

	gotWorktree, err := localWorktree.LocalWorktreeFFTarget()
	if err != nil {
		t.Fatal(err)
	}
	if want := (LocalWorktreeFFTarget{
		GitCommonDirIdentity: gitCommonDirIdentity,
		GitDirIdentity:       gitDirIdentity,
		WorktreeIdentity:     worktreeIdentity,
		TargetRef:            "refs/heads/main",
		BeforeOID:            testSHA256A,
		AfterOID:             testSHA256B,
	}); !reflect.DeepEqual(gotWorktree, want) {
		t.Fatalf("worktree target = %#v, want %#v", gotWorktree, want)
	}

	gotPush, err := push.PRBranchPushTarget()
	if err != nil {
		t.Fatal(err)
	}
	if want := (PRBranchPushTarget{
		RepositoryIdentity: repositoryIdentity,
		Remote: RemoteTarget{
			Transport:      "ssh",
			Host:           "example.test",
			Port:           2222,
			RepositoryPath: "team/game",
		},
		SourceOID:      testSHA1B,
		TargetRef:      "refs/heads/autogora/demo",
		ExpectedOldOID: testSHA1A,
	}); !reflect.DeepEqual(gotPush, want) {
		t.Fatalf("push target = %#v, want %#v", gotPush, want)
	}

	gotCreate, err := create.PRCreateTarget()
	if err != nil {
		t.Fatal(err)
	}
	if want := (PRCreateTarget{
		RepositoryIdentity: repositoryIdentity,
		BaseRef:            "refs/heads/main",
		HeadRef:            "refs/heads/autogora/demo",
		Title:              "Publish cooperative game",
		BodyDigest:         body,
		ExpectedHeadOID:    testSHA1B,
	}); !reflect.DeepEqual(gotCreate, want) {
		t.Fatalf("PR create target = %#v, want %#v", gotCreate, want)
	}
}

func TestDescriptorTypedTargetRejectsWrongKind(t *testing.T) {
	descriptor, err := NewLocalRefCAS(LocalRefCASInput{
		GitCommonDirPath: "/srv/repository/.git",
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA1A,
		AfterOID:         testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := descriptor.PRCreateTarget(); !errors.Is(
		err,
		ErrDescriptorKindMismatch,
	) {
		t.Fatalf("wrong-kind target error = %v", err)
	}
}

func TestDescriptorTypedTargetRejectsUninitializedOrForgedDescriptor(
	t *testing.T,
) {
	if _, err := (Descriptor{}).LocalRefCASTarget(); err == nil {
		t.Fatal("zero descriptor exposed a target")
	}
	valid, err := NewLocalRefCAS(LocalRefCASInput{
		GitCommonDirPath: "/srv/repository/.git",
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA1A,
		AfterOID:         testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}
	forged := valid
	forged.canonical = append([]byte(nil), valid.canonical...)
	forged.canonical[len(forged.canonical)-2] ^= 1
	if _, err := forged.LocalRefCASTarget(); err == nil {
		t.Fatal("forged descriptor exposed a target")
	}
}
