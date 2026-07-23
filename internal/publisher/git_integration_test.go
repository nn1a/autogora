package publisher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type gitFixture struct {
	repository  string
	worktree    string
	base        string
	head        string
	durableRef  string
	publication model.Publication
}

type recordedCommand struct {
	file string
	args []string
}

type recordingExecRunner struct {
	commands []recordedCommand
}

func (r *recordingExecRunner) Run(
	ctx context.Context,
	directory string,
	file string,
	args ...string,
) (CommandOutput, error) {
	r.commands = append(r.commands, recordedCommand{
		file: file, args: append([]string(nil), args...),
	})
	return (ExecRunner{}).Run(ctx, directory, file, args...)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Publisher Test")
	runGit(t, repository, "config", "user.email", "publisher@example.test")
	writeTestFile(t, filepath.Join(repository, "README.md"), "base\n")
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "base")
	base := runGit(t, repository, "rev-parse", "HEAD")

	worktree := filepath.Join(root, "source worktree")
	runGit(t, repository, "worktree", "add", "--detach", worktree, base)
	runGit(t, worktree, "config", "user.name", "Publisher Test")
	runGit(t, worktree, "config", "user.email", "publisher@example.test")
	writeTestFile(t, filepath.Join(worktree, "result.txt"), "captured\n")
	runGit(t, worktree, "add", "result.txt")
	runGit(t, worktree, "commit", "-m", "captured result")
	head := runGit(t, worktree, "rev-parse", "HEAD")
	durableRef := "refs/autogora/runs/run-publisher-test"
	runGit(t, repository, "update-ref", durableRef, head)
	return gitFixture{
		repository: repository, worktree: worktree, base: base, head: head,
		durableRef: durableRef,
		publication: model.Publication{
			ID: "pub-test", Board: "default", TaskID: "task-123",
			RunID: "run-publisher-test", ChangeSetID: "change-test",
			Mode: model.PublicationModeLocalFF, TargetBranch: "main", Remote: "origin",
			RepositoryPath: repository, WorktreePath: worktree,
			BaseCommit: base, HeadCommit: head, DurableRef: durableRef,
		},
	}
}

func TestLocalFFCheckedOutTargetAndIdempotency(t *testing.T) {
	fixture := newGitFixture(t)
	result, err := Execute(context.Background(), fixture.publication, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ResultPublished || result.URL != nil {
		t.Fatalf("publication result = %+v", result)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != fixture.head {
		t.Fatalf("main = %s, want %s", current, fixture.head)
	}
	content, err := os.ReadFile(filepath.Join(fixture.repository, "result.txt"))
	if err != nil || string(content) != "captured\n" {
		t.Fatalf("checked-out files were not updated: %q, %v", content, err)
	}
	second, err := Execute(context.Background(), fixture.publication, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != ResultAlreadyPublished {
		t.Fatalf("idempotent result = %+v", second)
	}
}

func TestLocalFFUncheckedTargetUsesCompareAndSwap(t *testing.T) {
	fixture := newGitFixture(t)
	runGit(t, fixture.repository, "branch", "release", fixture.base)
	fixture.publication.TargetBranch = "release"
	runner := &recordingExecRunner{}
	result, err := Execute(context.Background(), fixture.publication, Options{
		Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ResultPublished {
		t.Fatalf("publication result = %+v", result)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "release"); current != fixture.head {
		t.Fatalf("release = %s, want %s", current, fixture.head)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != fixture.base {
		t.Fatalf("checked-out main moved to %s", current)
	}
	if _, err := os.Stat(filepath.Join(fixture.repository, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("unchecked branch changed working files: %v", err)
	}
	want := []string{
		"update-ref", "refs/heads/release", fixture.head, fixture.base,
	}
	found := false
	for _, command := range runner.commands {
		if command.file == "git" && reflect.DeepEqual(command.args, want) {
			found = true
		}
		for _, argument := range command.args {
			if argument == "--force" || argument == "reset" || argument == "stash" ||
				argument == "rebase" {
				t.Fatalf("unsafe Git operation = %s %q", command.file, command.args)
			}
		}
	}
	if !found {
		t.Fatalf("compare-and-swap update-ref was not used: %+v", runner.commands)
	}
}

func TestLocalFFRefusesDirtyCheckedOutTarget(t *testing.T) {
	fixture := newGitFixture(t)
	writeTestFile(t, filepath.Join(fixture.repository, "untracked.txt"), "user work\n")
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrDirtyWorktree) {
		t.Fatalf("dirty worktree error = %v", err)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != fixture.base {
		t.Fatalf("dirty target moved to %s", current)
	}
}

func TestLocalFFRefusesDivergedTarget(t *testing.T) {
	fixture := newGitFixture(t)
	writeTestFile(t, filepath.Join(fixture.repository, "main-only.txt"), "diverged\n")
	runGit(t, fixture.repository, "add", "main-only.txt")
	runGit(t, fixture.repository, "commit", "-m", "diverge main")
	diverged := runGit(t, fixture.repository, "rev-parse", "main")
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrNonFastForward) {
		t.Fatalf("non-fast-forward error = %v", err)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != diverged {
		t.Fatalf("diverged target moved to %s", current)
	}
}

func TestPublicationIsAlreadyCompleteWhenTargetContainsHead(t *testing.T) {
	fixture := newGitFixture(t)
	runGit(t, fixture.repository, "merge", "--ff-only", fixture.head)
	writeTestFile(t, filepath.Join(fixture.repository, "after-publication.txt"), "later\n")
	runGit(t, fixture.repository, "add", "after-publication.txt")
	runGit(t, fixture.repository, "commit", "-m", "advance target")
	targetHead := runGit(t, fixture.repository, "rev-parse", "main")

	for _, mode := range []model.PublicationMode{
		model.PublicationModeLocalFF,
		model.PublicationModePullRequest,
	} {
		publication := fixture.publication
		publication.Mode = mode
		result, err := Execute(context.Background(), publication, Options{})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if result.Status != ResultAlreadyPublished {
			t.Fatalf("%s result = %+v", mode, result)
		}
		if current := runGit(t, fixture.repository, "rev-parse", "main"); current != targetHead {
			t.Fatalf("%s moved target from %s to %s", mode, targetHead, current)
		}
	}
}

func TestPublicationRequiresExactDurableRef(t *testing.T) {
	fixture := newGitFixture(t)
	runGit(t, fixture.repository, "update-ref", fixture.durableRef, fixture.base, fixture.head)
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("durable ref error = %v", err)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != fixture.base {
		t.Fatalf("target moved after source mismatch: %s", current)
	}
}

func TestPublicationRequiresCommonRepository(t *testing.T) {
	fixture := newGitFixture(t)
	other := newGitFixture(t)
	fixture.publication.WorktreePath = other.worktree
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrRepository) {
		t.Fatalf("common repository error = %v", err)
	}
}

func TestPublicationRejectsUnsafeTargetBranch(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.publication.TargetBranch = "--upload-pack=malicious"
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe target error = %v", err)
	}
}

func TestPublicationDoesNotTreatNestedRefAsTarget(t *testing.T) {
	fixture := newGitFixture(t)
	runGit(t, fixture.repository, "branch", "release/nested", fixture.base)
	fixture.publication.TargetBranch = "release"
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrRepository) {
		t.Fatalf("missing exact target error = %v", err)
	}
}

func TestPullRequestRejectsUnsafeRemoteName(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.publication.Mode = model.PublicationModePullRequest
	fixture.publication.Remote = "--upload-pack=malicious"
	_, err := Execute(context.Background(), fixture.publication, Options{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe remote error = %v", err)
	}
	if current := runGit(t, fixture.repository, "rev-parse", "main"); current != fixture.base {
		t.Fatalf("unsafe remote moved target to %s", current)
	}
}
