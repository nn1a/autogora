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
	t     *testing.T
	steps []expectedCommand
	index int
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
			[]string{"check-ref-format", "--branch", fixture.branch}, ""),
	)
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
			args: []string{"push", "--porcelain", "origin", fixture.head + ":" + ref},
		},
		expectedCommand{
			directory: fixture.repository, file: "git",
			args:   []string{"ls-remote", "--heads", "origin", ref},
			output: CommandOutput{Stdout: fixture.head + "\t" + ref + "\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "list", "--head", fixture.branch, "--base", "main",
				"--state", "open", "--limit", "100", "--json", "url,headRefOid",
			},
			output: CommandOutput{Stdout: "[]\n"},
		},
		expectedCommand{
			directory: fixture.repository, file: "gh",
			args: []string{
				"pr", "create", "--base", "main", "--head", fixture.branch,
				"--title", "Autogora: publish Task / Unsafe Name",
				"--body", pullRequestBody(validatedPublication{
					publication: fixture.publication, head: fixture.head,
				}),
			},
			output: CommandOutput{Stdout: "https://example.test/owner/repo/pull/42\n"},
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
	if result.Status != ResultPublished || result.Branch != fixture.branch ||
		result.URL == nil || *result.URL != "https://example.test/owner/repo/pull/42" {
		t.Fatalf("pull-request result = %+v", result)
	}
	for _, step := range steps {
		joined := strings.Join(step.args, " ")
		for _, forbidden := range []string{"--force", "reset", "stash", "rebase"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("unsafe command was issued: %s %s", step.file, joined)
			}
		}
	}
}

func TestPullRequestCreateRaceReusesNewOpenPR(t *testing.T) {
	fixture := newFakePullRequestFixture(t)
	steps := fakePullRequestPreflight(fixture)
	ref := "refs/heads/" + fixture.branch
	url := "https://example.test/owner/repo/pull/99"
	listArgs := []string{
		"pr", "list", "--head", fixture.branch, "--base", "main",
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
				"pr", "create", "--base", "main", "--head", fixture.branch,
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
				"pr", "list", "--head", fixture.branch, "--base", "main",
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
