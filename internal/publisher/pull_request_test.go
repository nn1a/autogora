package publisher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/publicationeffect"
)

type expectedCommand struct {
	directory string
	file      string
	args      []string
	output    CommandOutput
	err       error
}

type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return "exit status" }
func (e fakeExitError) ExitCode() int { return e.code }

type scriptedRunner struct {
	t          *testing.T
	steps      []expectedCommand
	index      int
	gatedCalls int
}

func (r *scriptedRunner) Run(
	ctx context.Context,
	directory string,
	file string,
	args ...string,
) (CommandOutput, error) {
	r.t.Helper()
	if _, available := ctx.Deadline(); !available {
		r.t.Fatal("command context has no deadline")
	}
	if r.index >= len(r.steps) {
		r.t.Fatalf("unexpected command: %s %q", file, args)
	}
	step := r.steps[r.index]
	r.index++
	if directory != step.directory || file != step.file ||
		!reflect.DeepEqual(args, step.args) {
		r.t.Fatalf(
			"command %d = dir %q, %s %q\nwant dir %q, %s %q",
			r.index, directory, file, args,
			step.directory, step.file, step.args,
		)
	}
	return step.output, step.err
}

func (r *scriptedRunner) RunGated(
	ctx context.Context,
	directory string,
	file string,
	gate CommandReleaseGate,
	args ...string,
) (CommandOutput, error) {
	r.t.Helper()
	r.gatedCalls++
	releaseCalled := false
	released, gateErr := gate(ctx, func() (bool, error) {
		releaseCalled = true
		return true, nil
	})
	if released && !releaseCalled {
		gateErr = errors.Join(
			gateErr,
			errors.New("test gate reported release without invoking it"),
		)
		released = false
	}
	if !released {
		return CommandOutput{}, newCommandStartError(false, gateErr)
	}
	output, runErr := r.Run(ctx, directory, file, args...)
	if gateErr != nil {
		return output, newCommandStartError(
			true,
			errors.Join(gateErr, runErr),
		)
	}
	return output, runErr
}

func (r *scriptedRunner) assertDone() {
	r.t.Helper()
	if r.index != len(r.steps) {
		r.t.Fatalf("executed %d of %d commands", r.index, len(r.steps))
	}
}

type fakePullRequestFixture struct {
	publication model.Publication
	repository  string
	worktree    string
	common      string
	base        string
	head        string
	targetHead  string
	branch      string
}

func newFakePullRequestFixture(t *testing.T) fakePullRequestFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktree := filepath.Join(root, "source")
	common := filepath.Join(root, "common")
	for _, directory := range []string{repository, worktree, common} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	targetHead := strings.Repeat("c", 40)
	return fakePullRequestFixture{
		repository: repository, worktree: worktree, common: common,
		base: base, head: head, targetHead: targetHead,
		branch: "autogora/task-unsafe-name-bbbbbbbb",
		publication: model.Publication{
			ID: "pub-pr", Board: "default", TaskID: "Task / Unsafe Name",
			RunID: "run-pr", ChangeSetID: "change-pr",
			Mode:         model.PublicationModePullRequest,
			TargetBranch: "main", Remote: "origin",
			RepositoryPath: repository, WorktreePath: worktree,
			BaseCommit: base, HeadCommit: head,
			DurableRef: "refs/autogora/runs/run-pr",
		},
	}
}

func fakeValidationSteps(fixture fakePullRequestFixture) []expectedCommand {
	git := func(
		directory string,
		args []string,
		stdout string,
	) expectedCommand {
		return expectedCommand{
			directory: directory, file: "git", args: args,
			output: CommandOutput{Stdout: stdout},
		}
	}
	return []expectedCommand{
		git(fixture.repository,
			[]string{"rev-parse", "--path-format=absolute", "--show-toplevel"},
			fixture.repository+"\n"),
		git(fixture.worktree,
			[]string{"rev-parse", "--path-format=absolute", "--show-toplevel"},
			fixture.worktree+"\n"),
		git(fixture.repository,
			[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"},
			fixture.common+"\n"),
		git(fixture.worktree,
			[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"},
			fixture.common+"\n"),
		git(fixture.repository,
			[]string{"rev-parse", "--verify", "--end-of-options", fixture.head + "^{commit}"},
			fixture.head+"\n"),
		git(fixture.repository,
			[]string{"rev-parse", "--verify", "--end-of-options", fixture.base + "^{commit}"},
			fixture.base+"\n"),
		git(fixture.repository,
			[]string{"check-ref-format", fixture.publication.DurableRef}, ""),
		git(fixture.repository,
			[]string{"rev-parse", "--verify", "--end-of-options",
				fixture.publication.DurableRef + "^{commit}"},
			fixture.head+"\n"),
		git(fixture.repository,
			[]string{"check-ref-format", "--branch", "main"}, "main\n"),
		git(fixture.repository,
			[]string{"for-each-ref", "--format=%(objectname)%00%(refname)", "--", "refs/heads/main"},
			fixture.targetHead+"\x00refs/heads/main\n"),
		git(fixture.repository,
			[]string{"merge-base", "--is-ancestor", fixture.base, fixture.head}, ""),
		{
			directory: fixture.repository, file: "git",
			args: []string{
				"merge-base", "--is-ancestor", fixture.head, fixture.targetHead,
			},
			err: fakeExitError{code: 1},
		},
	}
}

func fakePullRequestPreflight(fixture fakePullRequestFixture) []expectedCommand {
	steps := fakeValidationSteps(fixture)
	git := func(
		directory string,
		args []string,
		stdout string,
	) expectedCommand {
		return expectedCommand{
			directory: directory, file: "git", args: args,
			output: CommandOutput{Stdout: stdout},
		}
	}
	return append(steps,
		git(fixture.repository,
			[]string{"remote", "get-url", "--all", "origin"},
			"https://example.test/owner/repo.git\n"),
		git(fixture.repository,
			[]string{"remote", "get-url", "--push", "--all", "origin"},
			"git@example.test:owner/repo.git\n"),
		git(fixture.repository,
			[]string{"check-ref-format", "--branch", fixture.branch}, ""),
	)
}

func TestParseRemoteRepositoryIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		value    string
		selector string
		identity string
		valid    bool
	}{
		{
			name: "https", value: "https://GitHub.com/Owner/Repo.git",
			selector: "github.com/Owner/Repo",
			identity: "github.com/owner/repo",
			valid:    true,
		},
		{
			name: "ssh URL", value: "ssh://git@github.example/Owner/Repo.git",
			selector: "github.example/Owner/Repo",
			identity: "github.example/owner/repo",
			valid:    true,
		},
		{
			name: "scp", value: "git@github.example:Owner/Repo.git",
			selector: "github.example/Owner/Repo",
			identity: "github.example/owner/repo",
			valid:    true,
		},
		{name: "https credentials", value: "https://token@github.com/owner/repo.git"},
		{name: "https password", value: "https://user:secret@github.com/owner/repo.git"},
		{name: "ssh password", value: "ssh://git:secret@github.com/owner/repo.git"},
		{name: "ssh non git user", value: "ssh://person@github.com/owner/repo.git"},
		{name: "scp non git user", value: "person@github.com:owner/repo.git"},
		{name: "query", value: "https://github.com/owner/repo.git?token=secret"},
		{name: "fragment", value: "https://github.com/owner/repo.git#fragment"},
		{name: "escaped path", value: "https://github.com/owner%2Frepo.git"},
		{name: "nested path", value: "https://github.com/group/owner/repo.git"},
		{name: "local path", value: "/tmp/owner/repo.git"},
		{name: "file URL", value: "file:///tmp/owner/repo.git"},
		{name: "port", value: "https://github.com:443/owner/repo.git"},
		{name: "control", value: "https://github.com/owner/repo.git\n"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			identity, err := parseRemoteRepositoryIdentity(test.value)
			if !test.valid {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("invalid remote error=%v identity=%+v", err, identity)
				}
				return
			}
			if err != nil || identity.selector != test.selector ||
				identity.identity != test.identity {
				t.Fatalf(
					"remote identity=%+v err=%v want selector=%q identity=%q",
					identity,
					err,
					test.selector,
					test.identity,
				)
			}
		})
	}
}

func TestPullRequestURLIsBoundToConfiguredRepository(t *testing.T) {
	t.Parallel()
	remote := publicationRemote{
		repositorySelector: "example.test/Owner/Repo",
		repositoryIdentity: "example.test/owner/repo",
	}
	for _, test := range []struct {
		name  string
		value string
		valid bool
	}{
		{
			name:  "exact repository",
			value: "https://example.test/Owner/Repo/pull/42",
			valid: true,
		},
		{
			name:  "case insensitive repository",
			value: "https://EXAMPLE.test/owner/repo/pull/7",
			valid: true,
		},
		{name: "wrong host", value: "https://other.test/owner/repo/pull/42"},
		{name: "wrong owner", value: "https://example.test/other/repo/pull/42"},
		{name: "wrong repo", value: "https://example.test/owner/other/pull/42"},
		{name: "zero number", value: "https://example.test/owner/repo/pull/0"},
		{name: "negative number", value: "https://example.test/owner/repo/pull/-1"},
		{name: "query", value: "https://example.test/owner/repo/pull/42?token=x"},
		{name: "fragment", value: "https://example.test/owner/repo/pull/42#diff"},
		{name: "credentials", value: "https://token@example.test/owner/repo/pull/42"},
		{name: "insecure", value: "http://example.test/owner/repo/pull/42"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value, err := validPullRequestURL(test.value, remote)
			if test.valid {
				if err != nil || value != test.value {
					t.Fatalf("valid URL=%q err=%v", value, err)
				}
				return
			}
			if !errors.Is(err, ErrCommandFailed) {
				t.Fatalf("invalid URL value=%q err=%v", value, err)
			}
		})
	}
}

func TestPullRequestRejectsAmbiguousOrDifferentRemoteTargets(t *testing.T) {
	for _, test := range []struct {
		name  string
		fetch string
		push  string
	}{
		{
			name: "multiple fetch URLs",
			fetch: "https://example.test/owner/repo.git\n" +
				"https://mirror.test/owner/repo.git\n",
			push: "git@example.test:owner/repo.git\n",
		},
		{
			name:  "multiple push URLs",
			fetch: "https://example.test/owner/repo.git\n",
			push: "git@example.test:owner/repo.git\n" +
				"git@mirror.test:owner/repo.git\n",
		},
		{
			name:  "different target",
			fetch: "https://example.test/owner/repo.git\n",
			push:  "git@example.test:owner/other.git\n",
		},
		{
			name:  "embedded credential",
			fetch: "https://token@example.test/owner/repo.git\n",
			push:  "git@example.test:owner/repo.git\n",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			fixture := newFakePullRequestFixture(t)
			steps := fakeValidationSteps(fixture)
			steps = append(
				steps,
				expectedCommand{
					directory: fixture.repository,
					file:      "git",
					args:      []string{"remote", "get-url", "--all", "origin"},
					output:    CommandOutput{Stdout: test.fetch},
				},
				expectedCommand{
					directory: fixture.repository,
					file:      "git",
					args: []string{
						"remote", "get-url", "--push", "--all", "origin",
					},
					output: CommandOutput{Stdout: test.push},
				},
			)
			runner := &scriptedRunner{t: t, steps: steps}
			if _, err := Execute(
				context.Background(),
				fixture.publication,
				Options{Runner: runner},
			); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("remote target error=%v", err)
			}
			runner.assertDone()
		})
	}
}

func TestPullRequestPushCASRaceReusesMatchingRemoteBranch(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	ref := "refs/heads/" + fixture.branch
	remote := publicationRemote{
		name:               "origin",
		repositorySelector: "example.test/owner/repo",
		repositoryIdentity: "example.test/owner/repo",
		pushURL:            "git@example.test:owner/repo.git",
	}
	runner := &scriptedRunner{
		t: t,
		steps: []expectedCommand{
			{
				directory: fixture.repository,
				file:      "git",
				args: []string{
					"for-each-ref", "--format=%(objectname)%00%(refname)",
					"--", ref,
				},
			},
			{
				directory: fixture.repository,
				file:      "git",
				args:      []string{"ls-remote", "--heads", "origin", ref},
			},
			{
				directory: fixture.repository,
				file:      "git",
				args: []string{
					"push", "--porcelain",
					"--force-with-lease=" + ref + ":",
					"origin", fixture.head + ":" + ref,
				},
				err: fakeExitError{code: 1},
			},
			{
				directory: fixture.repository,
				file:      "git",
				args:      []string{"ls-remote", "--heads", "origin", ref},
				output: CommandOutput{
					Stdout: fixture.head + "\t" + ref + "\n",
				},
			},
		},
	}
	engine := New(Options{Runner: runner})
	existed, err := engine.ensurePullRequestBranch(
		context.Background(),
		validatedPublication{
			repository: fixture.repository,
			head:       fixture.head,
		},
		remote,
		fixture.branch,
	)
	if err != nil || !existed {
		t.Fatalf("CAS race existed=%t err=%v", existed, err)
	}
	runner.assertDone()
}

func TestPullRequestAllowsDivergedTargetAndCreatesPR(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"ls-remote", "--heads", "origin", ref},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{
				"push", "--porcelain", "--force-with-lease=" + ref + ":",
				"origin", fixture.head + ":" + ref,
			},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args:   []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{Stdout: fixture.head + "\t" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "list", "--repo", "example.test/owner/repo",
				"--head", fixture.branch, "--base", "main",
				"--state", "open", "--limit", "100", "--json", "url,headRefOid",
			},
			output: CommandOutput{Stdout: "[]\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "create", "--repo", "example.test/owner/repo",
				"--base", "main", "--head", fixture.branch,
				"--title", "Autogora: publish Task / Unsafe Name",
				"--body", pullRequestBody(validatedPublication{
					publication: fixture.publication, head: fixture.head,
				}),
			},
			output: CommandOutput{Stdout: "https://example.test/owner/repo/pull/42\n"},
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	effects := &recordingEffectExecutor{runner: runner}
	result, err := Execute(context.Background(), fixture.publication, Options{
		Runner: runner, EffectExecutor: effects,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.assertDone()
	if result.Status != ResultPublished || result.Branch != fixture.branch ||
		result.URL == nil || *result.URL != "https://example.test/owner/repo/pull/42" {
		t.Fatalf("pull-request result = %+v", result)
	}
	if len(effects.effects) != 2 {
		t.Fatalf("pull-request effects = %+v", effects.effects)
	}
	expectedPush, err := publicationeffect.NewPRBranchPush(
		publicationeffect.PRBranchPushInput{
			RepositoryIdentity: "example.test/owner/repo",
			RemoteURL:          "git@example.test:owner/repo.git",
			SourceOID:          fixture.head,
			TargetRef:          ref,
			ExpectedAbsent:     true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	body := pullRequestBody(validatedPublication{
		publication: fixture.publication,
		head:        fixture.head,
	})
	bodyDigest, err := publicationeffect.DigestPRBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	expectedCreate, err := publicationeffect.NewPRCreate(
		publicationeffect.PRCreateInput{
			RepositoryIdentity: "example.test/owner/repo",
			BaseRef:            "refs/heads/main",
			HeadRef:            ref,
			Title:              "Autogora: publish Task / Unsafe Name",
			BodyDigest:         bodyDigest,
			ExpectedHeadOID:    fixture.head,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range []publicationeffect.Descriptor{
		expectedPush,
		expectedCreate,
	} {
		got := effects.effects[index].descriptor
		if got.Kind() != expected.Kind() ||
			got.Fingerprint() != expected.Fingerprint() {
			t.Fatalf(
				"effect %d kind=%s fingerprint=%s, want %s %s",
				index,
				got.Kind(),
				got.Fingerprint(),
				expected.Kind(),
				expected.Fingerprint(),
			)
		}
	}
	foundAbsentRefLease := false
	for _, step := range steps {
		joined := strings.Join(step.args, " ")
		for _, argument := range step.args {
			if argument == "--force" {
				t.Fatalf("unsafe command was issued: %s %s", step.file, joined)
			}
			if argument == "--force-with-lease="+ref+":" {
				foundAbsentRefLease = true
			}
		}
		for _, forbidden := range []string{"reset", "stash", "rebase"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("unsafe command was issued: %s %s", step.file, joined)
			}
		}
	}
	if !foundAbsentRefLease {
		t.Fatal("pull-request branch push did not use an absent-ref lease")
	}
}

func TestPullRequestCreateRaceReusesNewOpenPR(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	url := "https://example.test/owner/repo/pull/99"
	listArgs := []string{
		"pr", "list", "--repo", "example.test/owner/repo",
		"--head", fixture.branch, "--base", "main",
		"--state", "open", "--limit", "100", "--json", "url,headRefOid",
	}
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{
				"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref,
			},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{
				Stdout: fixture.head + "\t" + ref + "\n",
			},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh", args: listArgs,
			output: CommandOutput{Stdout: "[]\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "create", "--repo", "example.test/owner/repo",
				"--base", "main", "--head", fixture.branch,
				"--title", "Autogora: publish Task / Unsafe Name",
				"--body", pullRequestBody(validatedPublication{
					publication: fixture.publication, head: fixture.head,
				}),
			},
			output: CommandOutput{Stderr: "a pull request already exists"},
			err:    fakeExitError{code: 1},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh", args: listArgs,
			output: CommandOutput{Stdout: `[{"url":"` + url +
				`","headRefOid":"` + fixture.head + `"}]`},
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	result, err := Execute(context.Background(), fixture.publication, Options{
		Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.assertDone()
	if result.URL == nil || *result.URL != url {
		t.Fatalf("race result = %+v", result)
	}
}

func TestPullRequestReusesExistingBranchAndPR(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	url := "https://example.test/owner/repo/pull/7"
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args:   []string{"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref},
			output: CommandOutput{Stdout: fixture.head + "\x00" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args:   []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{Stdout: fixture.head + "\t" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "list", "--repo", "example.test/owner/repo",
				"--head", fixture.branch, "--base", "main",
				"--state", "open", "--limit", "100", "--json", "url,headRefOid",
			},
			output: CommandOutput{Stdout: `[{"url":"` + url +
				`","headRefOid":"` + fixture.head + `"}]`},
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	result, err := Execute(context.Background(), fixture.publication, Options{
		Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.assertDone()
	if result.Status != ResultAlreadyPublished || result.URL == nil ||
		*result.URL != url {
		t.Fatalf("reused pull-request result = %+v", result)
	}
}

func TestPullRequestRefusesRemoteBranchCollision(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{
				Stdout: strings.Repeat("c", 40) + "\t" + ref + "\n",
			},
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	_, err := Execute(context.Background(), fixture.publication, Options{
		Runner: runner,
	})
	if !errors.Is(err, ErrRemoteConflict) {
		t.Fatalf("remote collision error = %v", err)
	}
	runner.assertDone()
}

func TestPullRequestReadOnlyReuseBypassesEffectExecutor(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	url := "https://example.test/owner/repo/pull/7"
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{
				"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref,
			},
			output: CommandOutput{Stdout: fixture.head + "\x00" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{
				Stdout: fixture.head + "\t" + ref + "\n",
			},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "list", "--repo", "example.test/owner/repo",
				"--head", fixture.branch, "--base", "main",
				"--state", "open", "--limit", "100", "--json", "url,headRefOid",
			},
			output: CommandOutput{Stdout: `[{"url":"` + url +
				`","headRefOid":"` + fixture.head + `"}]`},
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	result, err := Execute(
		context.Background(),
		fixture.publication,
		Options{
			Runner: runner,
			EffectExecutor: EffectExecutorFunc(func(
				context.Context,
				publicationeffect.Descriptor,
				EffectCommand,
			) (CommandOutput, error) {
				t.Fatal("read-only publication called the effect executor")
				return CommandOutput{}, nil
			}),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runner.assertDone()
	if result.Status != ResultAlreadyPublished ||
		runner.gatedCalls != 0 {
		t.Fatalf(
			"read-only result=%+v gatedCalls=%d commands=%d",
			result,
			runner.gatedCalls,
			len(steps),
		)
	}
}

func TestPullRequestCreateStartDenialDoesNotReconcile(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	createArgs := []string{
		"pr", "create", "--repo", "example.test/owner/repo",
		"--base", "main", "--head", fixture.branch,
		"--title", "Autogora: publish Task / Unsafe Name",
		"--body", pullRequestBody(validatedPublication{
			publication: fixture.publication, head: fixture.head,
		}),
	}
	steps = append(steps,
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{
				"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref,
			},
			output: CommandOutput{Stdout: fixture.head + "\x00" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args: []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{
				Stdout: fixture.head + "\t" + ref + "\n",
			},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "list", "--repo", "example.test/owner/repo",
				"--head", fixture.branch, "--base", "main",
				"--state", "open", "--limit", "100", "--json", "url,headRefOid",
			},
			output: CommandOutput{Stdout: "[]\n"},
		},
		expectedCommand{
			directory: fixture.repository,
			file:      "gh",
			args:      createArgs,
		},
	)
	runner := &scriptedRunner{t: t, steps: steps}
	gateCause := errors.New("create authorization was revoked")
	gateCalls := 0
	_, err := Execute(
		context.Background(),
		fixture.publication,
		Options{
			Runner: runner,
			ReleaseGate: func(
				_ context.Context,
				release CommandRelease,
			) (bool, error) {
				gateCalls++
				return false, gateCause
			},
		},
	)
	var startErr *CommandStartError
	if !errors.As(err, &startErr) || startErr.Released ||
		!errors.Is(err, gateCause) ||
		errors.Is(err, ErrCommandFailed) {
		t.Fatalf("create start error=%v detail=%#v", err, startErr)
	}
	if gateCalls != 1 || runner.gatedCalls != 1 ||
		runner.index != len(steps)-1 {
		t.Fatalf(
			"create denial reconciled: gates=%d runnerGates=%d commands=%d",
			gateCalls,
			runner.gatedCalls,
			runner.index,
		)
	}
}
