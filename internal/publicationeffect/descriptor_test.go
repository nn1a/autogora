package publicationeffect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	testSHA1A   = "1111111111111111111111111111111111111111"
	testSHA1B   = "2222222222222222222222222222222222222222"
	testSHA256A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSHA256B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestKindsAreExactlyTheFourMutations(t *testing.T) {
	want := []Kind{
		KindLocalRefCAS,
		KindLocalWorktreeFF,
		KindPRBranchPush,
		KindPRCreate,
	}
	first := Kinds()
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("kinds = %#v, want %#v", first, want)
	}
	first[0] = "modified_by_caller"
	if got := Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("caller mutated package kinds: %#v", got)
	}
}

func TestCanonicalDescriptorsRoundTripAndHideSensitiveInputs(t *testing.T) {
	gitPath := "/srv/repositories/demo/.git"
	gitDirPath := "/srv/repositories/demo/.git/worktrees/demo-main"
	worktreePath := "/srv/worktrees/demo-main"
	bodyMarker := "private-body-marker\n\nImplementation details."
	body, err := DigestPRBody([]byte(bodyMarker))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := RepositoryIdentityFromRemote(
		"git@github.com:nn1a/autogora.git",
	)
	if err != nil {
		t.Fatal(err)
	}
	descriptors := map[Kind]Descriptor{}
	descriptors[KindLocalRefCAS], err = NewLocalRefCAS(LocalRefCASInput{
		GitCommonDirPath: gitPath,
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA1A,
		AfterOID:         testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptors[KindLocalWorktreeFF], err = NewLocalWorktreeFF(
		LocalWorktreeFFInput{
			GitCommonDirPath: gitPath,
			GitDirPath:       gitDirPath,
			WorktreePath:     worktreePath,
			TargetRef:        "refs/heads/main",
			BeforeOID:        testSHA256A,
			AfterOID:         testSHA256B,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	descriptors[KindPRBranchPush], err = NewPRBranchPush(PRBranchPushInput{
		RepositoryIdentity: repository,
		RemoteURL:          "git@github.com:nn1a/autogora.git",
		SourceOID:          testSHA1B,
		TargetRef:          "refs/heads/autogora/publication-1",
		ExpectedAbsent:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptors[KindPRCreate], err = NewPRCreate(PRCreateInput{
		RepositoryIdentity: repository,
		BaseRef:            "refs/heads/main",
		HeadRef:            "refs/heads/autogora/publication-1",
		Title:              "Publish completed work",
		BodyDigest:         body,
		ExpectedHeadOID:    testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}

	gitIdentity, err := GitCommonDirIdentityFromCanonicalPath(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	worktreeIdentity, err := WorktreeIdentityFromCanonicalPath(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	gitDirIdentity, err := GitDirIdentityFromCanonicalPath(gitDirPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON := map[Kind]string{
		KindLocalRefCAS: fmt.Sprintf(
			`{"version":1,"kind":"local_ref_cas","target":{"gitCommonDirIdentity":%q,"targetRef":"refs/heads/main","beforeOid":"%s","afterOid":"%s"}}`,
			gitIdentity,
			testSHA1A,
			testSHA1B,
		),
		KindLocalWorktreeFF: fmt.Sprintf(
			`{"version":1,"kind":"local_worktree_ff","target":{"gitCommonDirIdentity":%q,"gitDirIdentity":%q,"worktreeIdentity":%q,"targetRef":"refs/heads/main","beforeOid":"%s","afterOid":"%s"}}`,
			gitIdentity,
			gitDirIdentity,
			worktreeIdentity,
			testSHA256A,
			testSHA256B,
		),
		KindPRBranchPush: fmt.Sprintf(
			`{"version":1,"kind":"pr_branch_push","target":{"repositoryIdentity":"github.com/nn1a/autogora","remote":{"transport":"ssh","host":"github.com","repositoryPath":"nn1a/autogora"},"sourceOid":"%s","targetRef":"refs/heads/autogora/publication-1","expected":{"mode":"absent"}}}`,
			testSHA1B,
		),
		KindPRCreate: fmt.Sprintf(
			`{"version":1,"kind":"pr_create","target":{"repositoryIdentity":"github.com/nn1a/autogora","baseRef":"refs/heads/main","headRef":"refs/heads/autogora/publication-1","title":"Publish completed work","bodyDigest":{"sha256":"%s","bytes":%d},"expectedHeadOid":"%s"}}`,
			body.SHA256,
			body.Bytes,
			testSHA1B,
		),
	}
	// These values make accidental schema or field-order drift visible. A
	// deliberate schema change must increment SchemaVersion.
	expectedFingerprint := map[Kind]string{
		KindLocalRefCAS:     "67527e7244649e5ddd1ac97f83ee77249339726d4f45769b3cf8a6815560370c",
		KindLocalWorktreeFF: "2ff01d6f80545aba6453f267cf1e85d9148d7a500224592032aa1fc4e43ecf33",
		KindPRBranchPush:    "df09a464ae2dea631b5cbc68cfa6878c311ae76358e9d5f7c4e24cd0690d78b6",
		KindPRCreate:        "7c6860289be9c048d0ac17de383749b5cb083e082d5fde0b5475d6b96a89417a",
	}

	for kind, descriptor := range descriptors {
		t.Run(string(kind), func(t *testing.T) {
			raw := descriptor.CanonicalJSON()
			if got, want := string(raw), expectedJSON[kind]; got != want {
				t.Fatalf("canonical JSON:\n got: %s\nwant: %s", got, want)
			}
			if len(raw) > MaxCanonicalJSONBytes {
				t.Fatalf("canonical JSON is unbounded: %d", len(raw))
			}
			if descriptor.Kind() != kind || descriptor.Version() != SchemaVersion {
				t.Fatalf(
					"metadata = (%q, %d), want (%q, %d)",
					descriptor.Kind(),
					descriptor.Version(),
					kind,
					SchemaVersion,
				)
			}
			if expectedFingerprint[kind] != "" &&
				descriptor.Fingerprint() != expectedFingerprint[kind] {
				t.Fatalf(
					"fingerprint = %s, want %s",
					descriptor.Fingerprint(),
					expectedFingerprint[kind],
				)
			}
			if len(descriptor.Fingerprint()) != 64 ||
				descriptor.Fingerprint() != strings.ToLower(
					descriptor.Fingerprint(),
				) {
				t.Fatalf("invalid fingerprint %q", descriptor.Fingerprint())
			}
			parsed, parseErr := ParseCanonical(raw)
			if parseErr != nil {
				t.Fatalf("parse canonical descriptor: %v", parseErr)
			}
			if parsed.Fingerprint() != descriptor.Fingerprint() ||
				!bytes.Equal(parsed.CanonicalJSON(), raw) {
				t.Fatal("round trip changed descriptor identity")
			}
			marshaled, marshalErr := json.Marshal(descriptor)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !bytes.Equal(marshaled, raw) {
				t.Fatalf("MarshalJSON changed canonical bytes: %s", marshaled)
			}

			changed := descriptor.CanonicalJSON()
			changed[0] = '['
			if descriptor.CanonicalJSON()[0] != '{' {
				t.Fatal("caller mutated descriptor through CanonicalJSON")
			}

			for _, secret := range []string{
				gitPath,
				gitDirPath,
				worktreePath,
				bodyMarker,
				"git@",
				"argv",
				"--body",
			} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("canonical JSON exposed %q: %s", secret, raw)
				}
			}
		})
	}

	if descriptors[KindLocalRefCAS].Fingerprint() ==
		descriptors[KindLocalWorktreeFF].Fingerprint() {
		t.Fatal("different mutation kinds share a fingerprint")
	}
}

func TestLocalPathIdentitiesAreCanonicalBoundedAndDomainSeparated(t *testing.T) {
	gitIdentity, err := GitCommonDirIdentityFromCanonicalPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := GitCommonDirIdentityFromCanonicalPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	worktreeIdentity, err := WorktreeIdentityFromCanonicalPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	gitDirIdentity, err := GitDirIdentityFromCanonicalPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	if gitIdentity != repeated {
		t.Fatal("same canonical path produced different identities")
	}
	if gitIdentity == worktreeIdentity ||
		gitIdentity == gitDirIdentity ||
		worktreeIdentity == gitDirIdentity {
		t.Fatal("identity domains are not separated")
	}
	if !strings.HasPrefix(gitIdentity, gitCommonDirIdentityPrefix) ||
		!strings.HasPrefix(gitDirIdentity, gitDirIdentityPrefix) ||
		!strings.HasPrefix(worktreeIdentity, worktreeIdentityPrefix) {
		t.Fatalf(
			"unexpected identity formats: %q, %q, %q",
			gitIdentity,
			gitDirIdentity,
			worktreeIdentity,
		)
	}

	invalid := []string{
		"",
		"relative/.git",
		"/repo/../repo/.git",
		"/",
		"/repo/\x00.git",
		"/repo/\x1b.git",
		string([]byte{'/', 'r', 0xff}),
		"/" + strings.Repeat("a", maxCanonicalPathBytes),
	}
	for _, path := range invalid {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			if _, err := GitCommonDirIdentityFromCanonicalPath(path); err == nil {
				t.Fatalf("accepted invalid canonical path %q", path)
			}
		})
	}
}

func TestRemoteRepositoryIdentityCanonicalization(t *testing.T) {
	valid := map[string]string{
		"https://github.com/owner/repo.git":              "github.com/owner/repo",
		"ssh://git@github.com/owner/repo.git":            "github.com/owner/repo",
		"git@github.com:owner/repo.git":                  "github.com/owner/repo",
		"github.com:owner/repo":                          "github.com/owner/repo",
		"ssh://git@example.com:2222/group/subgroup/repo": "example.com:2222/group/subgroup/repo",
		"ssh://git@example.com:443/group/repo":           "example.com:443/group/repo",
		"https://example.com:8443/group/repo":            "example.com:8443/group/repo",
		"ssh://git@[2001:db8::1]/owner/repo.git":         "[2001:db8::1]/owner/repo",
		"ssh://git@[2001:db8::1]:2222/owner/repo.git":    "[2001:db8::1]:2222/owner/repo",
	}
	for raw, want := range valid {
		t.Run(raw, func(t *testing.T) {
			got, err := RepositoryIdentityFromRemote(raw)
			if err != nil {
				t.Fatalf("derive repository identity: %v", err)
			}
			if got != want {
				t.Fatalf("identity = %q, want %q", got, want)
			}
			if err := validateRepositoryIdentity(got); err != nil {
				t.Fatalf("derived identity is not accepted: %v", err)
			}
		})
	}

	invalid := []string{
		"",
		"https://token@github.com/owner/repo.git",
		"https://user:password@github.com/owner/repo.git",
		"ssh://alice@github.com/owner/repo.git",
		"alice@github.com:owner/repo.git",
		"ssh://git:password@github.com/owner/repo.git",
		"http://github.com/owner/repo.git",
		"file:///tmp/repo.git",
		"HTTPS://github.com/owner/repo.git",
		"https://GitHub.com/owner/repo.git",
		"https://github.com:443/owner/repo.git",
		"ssh://git@github.com:22/owner/repo.git",
		"https://github.com/repo.git",
		"https://github.com/.owner/repo.git",
		"https://github.com/owner/../repo.git",
		"https://github.com/owner/repo.git?token=secret",
		"https://github.com/owner/repo.git#fragment",
		"https://github.com/owner%2frepo.git",
		"https://github.com/owner/repo name.git",
		"git@github.com:owner/repo.git:refs/heads/main",
		"github.com:owner/repo.git evil.example:owner/repo",
		"https://github.com/owner/\x00repo.git",
		"https://github.com/owner/\x1brepo.git",
		strings.Repeat("a", maxRemoteURLBytes+1),
	}
	for _, raw := range invalid {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			if identity, err := RepositoryIdentityFromRemote(raw); err == nil {
				t.Fatalf("accepted unsafe remote %q as %q", raw, identity)
			}
		})
	}
}

func TestLocalDescriptorsRejectInvalidIdentityRefAndOID(t *testing.T) {
	gitIdentity, err := GitCommonDirIdentityFromCanonicalPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	worktreeIdentity, err := WorktreeIdentityFromCanonicalPath("/worktree")
	if err != nil {
		t.Fatal(err)
	}
	gitDirIdentity, err := GitDirIdentityFromCanonicalPath(
		"/repo/.git/worktrees/worktree",
	)
	if err != nil {
		t.Fatal(err)
	}
	validRef := LocalRefCASInput{
		GitCommonDirIdentity: gitIdentity,
		TargetRef:            "refs/tags/release-v1",
		BeforeOID:            testSHA1A,
		AfterOID:             testSHA1B,
	}
	if _, err := NewLocalRefCAS(validRef); err != nil {
		t.Fatalf("valid ref CAS rejected: %v", err)
	}

	tests := []struct {
		name  string
		input LocalRefCASInput
	}{
		{name: "no identity", input: LocalRefCASInput{
			TargetRef: "refs/heads/main", BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "path and identity", input: LocalRefCASInput{
			GitCommonDirPath: "/repo/.git", GitCommonDirIdentity: gitIdentity,
			TargetRef: "refs/heads/main", BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "wrong identity domain", input: LocalRefCASInput{
			GitCommonDirIdentity: worktreeIdentity, TargetRef: "refs/heads/main",
			BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "uppercase identity digest", input: LocalRefCASInput{
			GitCommonDirIdentity: strings.ToUpper(gitIdentity),
			TargetRef:            "refs/heads/main", BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "short ref", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "main",
			BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "ref whitespace", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/my branch",
			BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "ref traversal", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/a..b",
			BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "ref lock", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/main.lock",
			BeforeOID: testSHA1A, AfterOID: testSHA1B,
		}},
		{name: "short oid", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/main",
			BeforeOID: "abc", AfterOID: testSHA1B,
		}},
		{name: "uppercase oid", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/main",
			BeforeOID: strings.ToUpper(
				"abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			),
			AfterOID: testSHA1B,
		}},
		{name: "zero oid", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/main",
			BeforeOID: strings.Repeat("0", 40), AfterOID: testSHA1B,
		}},
		{name: "mixed oid formats", input: LocalRefCASInput{
			GitCommonDirIdentity: gitIdentity, TargetRef: "refs/heads/main",
			BeforeOID: testSHA1A, AfterOID: testSHA256B,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if descriptor, err := NewLocalRefCAS(test.input); err == nil {
				t.Fatalf("accepted invalid local ref descriptor: %s", descriptor.CanonicalJSON())
			}
		})
	}

	validWorktree := LocalWorktreeFFInput{
		GitCommonDirIdentity: gitIdentity,
		GitDirIdentity:       gitDirIdentity,
		WorktreeIdentity:     worktreeIdentity,
		TargetRef:            "refs/heads/main",
		BeforeOID:            testSHA1A,
		AfterOID:             testSHA1B,
	}
	if _, err := NewLocalWorktreeFF(validWorktree); err != nil {
		t.Fatalf("valid worktree descriptor rejected: %v", err)
	}
	validWorktree.TargetRef = "refs/tags/release"
	if _, err := NewLocalWorktreeFF(validWorktree); err == nil {
		t.Fatal("worktree fast-forward accepted a non-head target")
	}
	validWorktree.TargetRef = "refs/heads/main"
	validWorktree.WorktreeIdentity = gitIdentity
	if _, err := NewLocalWorktreeFF(validWorktree); err == nil {
		t.Fatal("worktree accepted a git-common-dir identity")
	}
	validWorktree.WorktreeIdentity = worktreeIdentity
	validWorktree.GitDirIdentity = worktreeIdentity
	if _, err := NewLocalWorktreeFF(validWorktree); err == nil {
		t.Fatal("worktree accepted a worktree identity as private git dir")
	}
}

func TestPushDescriptorRejectsAmbiguousOrMutableTargets(t *testing.T) {
	repository := "github.com/owner/repo"
	valid := PRBranchPushInput{
		RepositoryIdentity: repository,
		RemoteURL:          "https://github.com/owner/repo.git",
		SourceOID:          testSHA256A,
		TargetRef:          "refs/heads/autogora/task-1",
		ExpectedOldOID:     testSHA256B,
	}
	exact, err := NewPRBranchPush(valid)
	if err != nil {
		t.Fatalf("valid exact-CAS push rejected: %v", err)
	}
	absentInput := valid
	absentInput.ExpectedOldOID = ""
	absentInput.ExpectedAbsent = true
	absent, err := NewPRBranchPush(absentInput)
	if err != nil {
		t.Fatalf("valid absent-CAS push rejected: %v", err)
	}
	if exact.Fingerprint() == absent.Fingerprint() {
		t.Fatal("different push CAS policies share a fingerprint")
	}

	tests := []struct {
		name   string
		mutate func(*PRBranchPushInput)
	}{
		{name: "no CAS", mutate: func(value *PRBranchPushInput) {
			value.ExpectedOldOID = ""
		}},
		{name: "two CAS modes", mutate: func(value *PRBranchPushInput) {
			value.ExpectedAbsent = true
		}},
		{name: "repository mismatch", mutate: func(value *PRBranchPushInput) {
			value.RepositoryIdentity = "github.com/owner/other"
		}},
		{name: "repository URL identity", mutate: func(value *PRBranchPushInput) {
			value.RepositoryIdentity = "https://github.com/owner/repo"
		}},
		{name: "credential in remote", mutate: func(value *PRBranchPushInput) {
			value.RemoteURL = "https://token@github.com/owner/repo.git"
		}},
		{name: "multiple remote targets", mutate: func(value *PRBranchPushInput) {
			value.RemoteURL = "github.com:owner/repo.git evil:owner/repo"
		}},
		{name: "target refspec", mutate: func(value *PRBranchPushInput) {
			value.TargetRef = "refs/heads/a:refs/heads/b"
		}},
		{name: "partial source oid", mutate: func(value *PRBranchPushInput) {
			value.SourceOID = testSHA1A[:12]
		}},
		{name: "zero expected oid", mutate: func(value *PRBranchPushInput) {
			value.ExpectedOldOID = strings.Repeat("0", 40)
		}},
		{name: "mixed oid formats", mutate: func(value *PRBranchPushInput) {
			value.ExpectedOldOID = testSHA1A
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.mutate(&input)
			if descriptor, err := NewPRBranchPush(input); err == nil {
				t.Fatalf("accepted ambiguous push: %s", descriptor.CanonicalJSON())
			}
		})
	}
}

func TestPRCreateRetainsOnlyValidatedBodyDigest(t *testing.T) {
	rawBody := []byte("## Summary\n\nPrivate implementation marker.\n")
	digest, err := DigestPRBody(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	if digest.Bytes != int64(len(rawBody)) || !validSHA256(digest.SHA256) {
		t.Fatalf("invalid body digest: %+v", digest)
	}
	input := PRCreateInput{
		RepositoryIdentity: "github.com/owner/repo",
		BaseRef:            "refs/heads/main",
		HeadRef:            "refs/heads/autogora/task-1",
		Title:              "Publish task 1",
		BodyDigest:         digest,
		ExpectedHeadOID:    testSHA1A,
	}
	descriptor, err := NewPRCreate(input)
	if err != nil {
		t.Fatalf("valid PR create rejected: %v", err)
	}
	if bytes.Contains(descriptor.CanonicalJSON(), rawBody) ||
		bytes.Contains(descriptor.CanonicalJSON(), []byte("Private implementation")) {
		t.Fatalf("PR body leaked into descriptor: %s", descriptor.CanonicalJSON())
	}

	invalidBodies := [][]byte{
		nil,
		{},
		{0xff},
		[]byte("body\x00"),
		[]byte("body\x1b"),
		bytes.Repeat([]byte("x"), MaxPRBodyBytes+1),
	}
	for index, body := range invalidBodies {
		t.Run(fmt.Sprintf("body_%d", index), func(t *testing.T) {
			if value, err := DigestPRBody(body); err == nil {
				t.Fatalf("accepted invalid PR body: %+v", value)
			}
		})
	}

	tests := []struct {
		name   string
		mutate func(*PRCreateInput)
	}{
		{name: "noncanonical repo", mutate: func(value *PRCreateInput) {
			value.RepositoryIdentity = "GitHub.com/owner/repo"
		}},
		{name: "uppercase repo path", mutate: func(value *PRCreateInput) {
			value.RepositoryIdentity = "github.com/Owner/repo"
		}},
		{name: "nested repo path", mutate: func(value *PRCreateInput) {
			value.RepositoryIdentity = "github.com/group/owner/repo"
		}},
		{name: "tag base", mutate: func(value *PRCreateInput) {
			value.BaseRef = "refs/tags/release"
		}},
		{name: "short head", mutate: func(value *PRCreateInput) {
			value.HeadRef = "feature"
		}},
		{name: "empty title", mutate: func(value *PRCreateInput) {
			value.Title = ""
		}},
		{name: "padded title", mutate: func(value *PRCreateInput) {
			value.Title = " padded "
		}},
		{name: "control title", mutate: func(value *PRCreateInput) {
			value.Title = "title\nsecond command"
		}},
		{name: "oversized title", mutate: func(value *PRCreateInput) {
			value.Title = strings.Repeat("x", maxTitleBytes+1)
		}},
		{name: "bad body digest", mutate: func(value *PRCreateInput) {
			value.BodyDigest.SHA256 = strings.Repeat("g", 64)
		}},
		{name: "uppercase body digest", mutate: func(value *PRCreateInput) {
			value.BodyDigest.SHA256 = strings.ToUpper(
				value.BodyDigest.SHA256,
			)
		}},
		{name: "empty body size", mutate: func(value *PRCreateInput) {
			value.BodyDigest.Bytes = 0
		}},
		{name: "oversized body size", mutate: func(value *PRCreateInput) {
			value.BodyDigest.Bytes = MaxPRBodyBytes + 1
		}},
		{name: "bad expected head", mutate: func(value *PRCreateInput) {
			value.ExpectedHeadOID = "HEAD"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := input
			test.mutate(&changed)
			if value, err := NewPRCreate(changed); err == nil {
				t.Fatalf("accepted invalid PR descriptor: %s", value.CanonicalJSON())
			}
		})
	}
}

func TestParseCanonicalRejectsAlternateJSONAndSecretBearingFields(t *testing.T) {
	descriptor, err := NewLocalRefCAS(LocalRefCASInput{
		GitCommonDirPath: "/repo/.git",
		TargetRef:        "refs/heads/main",
		BeforeOID:        testSHA1A,
		AfterOID:         testSHA1B,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(descriptor.CanonicalJSON())
	targetStart := `"target":{`
	invalid := map[string][]byte{
		"empty":               nil,
		"leading whitespace":  []byte(" " + raw),
		"trailing whitespace": []byte(raw + "\n"),
		"trailing JSON":       []byte(raw + `{}`),
		"wrong version":       []byte(strings.Replace(raw, `"version":1`, `"version":2`, 1)),
		"unknown kind":        []byte(strings.Replace(raw, `"local_ref_cas"`, `"shell_command"`, 1)),
		"outer argv": []byte(strings.Replace(
			raw,
			`,"kind":`,
			`,"argv":["git","update-ref"],"kind":`,
			1,
		)),
		"target argv": []byte(strings.Replace(
			raw,
			targetStart,
			`"target":{"argv":["git","update-ref"],`,
			1,
		)),
		"target token": []byte(strings.Replace(
			raw,
			targetStart,
			`"target":{"token":"secret",`,
			1,
		)),
		"missing target": []byte(strings.Replace(
			raw,
			`,"target":`+raw[strings.Index(raw, targetStart)+len(`"target":`):],
			"",
			1,
		)),
		"duplicate version": []byte(strings.Replace(
			raw,
			`{"version":1`,
			`{"version":1,"version":1`,
			1,
		)),
		"reordered envelope": []byte(fmt.Sprintf(
			`{"kind":"%s","version":1,"target":%s}`,
			KindLocalRefCAS,
			raw[strings.Index(raw, targetStart)+len(`"target":`):len(raw)-1],
		)),
	}
	for name, value := range invalid {
		t.Run(name, func(t *testing.T) {
			if parsed, err := ParseCanonical(value); err == nil {
				t.Fatalf("accepted noncanonical/unsafe JSON: %s", parsed.CanonicalJSON())
			}
		})
	}
	oversized := bytes.Repeat([]byte("x"), MaxCanonicalJSONBytes+1)
	if _, err := ParseCanonical(oversized); err == nil {
		t.Fatal("accepted oversized canonical JSON")
	}

	var zero Descriptor
	if _, err := json.Marshal(zero); err == nil {
		t.Fatal("marshaled an uninitialized descriptor")
	}
	if zero.Version() != 0 || zero.Kind() != "" ||
		zero.Fingerprint() != "" || zero.CanonicalJSON() != nil {
		t.Fatalf("zero descriptor has initialized state: %+v", zero)
	}
}

func TestDescriptorFingerprintBindsEveryEffectField(t *testing.T) {
	base := PRCreateInput{
		RepositoryIdentity: "github.com/owner/repo",
		BaseRef:            "refs/heads/main",
		HeadRef:            "refs/heads/autogora/task-1",
		Title:              "Publish task 1",
		BodyDigest: BodyDigest{
			SHA256: strings.Repeat("a", 64),
			Bytes:  10,
		},
		ExpectedHeadOID: testSHA1A,
	}
	original, err := NewPRCreate(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*PRCreateInput){
		func(value *PRCreateInput) {
			value.RepositoryIdentity = "github.com/owner/other"
		},
		func(value *PRCreateInput) {
			value.BaseRef = "refs/heads/release"
		},
		func(value *PRCreateInput) {
			value.HeadRef = "refs/heads/autogora/task-2"
		},
		func(value *PRCreateInput) {
			value.Title = "Publish task 2"
		},
		func(value *PRCreateInput) {
			value.BodyDigest.SHA256 = strings.Repeat("b", 64)
		},
		func(value *PRCreateInput) {
			value.BodyDigest.Bytes++
		},
		func(value *PRCreateInput) {
			value.ExpectedHeadOID = testSHA1B
		},
	}
	seen := map[string]struct{}{original.Fingerprint(): {}}
	for index, mutate := range mutations {
		changedInput := base
		mutate(&changedInput)
		changed, err := NewPRCreate(changedInput)
		if err != nil {
			t.Fatalf("mutation %d became invalid: %v", index, err)
		}
		if changed.Fingerprint() == original.Fingerprint() {
			t.Fatalf("mutation %d did not change fingerprint", index)
		}
		if _, duplicate := seen[changed.Fingerprint()]; duplicate {
			t.Fatalf("mutation %d produced a duplicate fingerprint", index)
		}
		seen[changed.Fingerprint()] = struct{}{}
	}
}
