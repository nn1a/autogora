package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

type recoveryGitFixture struct {
	root       string
	repository string
	source     string
	base       string
	workspace  model.RunWorkspace
}

func newRecoveryGitFixture(t *testing.T, runID string) recoveryGitFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "init", "-q")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "delete.txt"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "add", "README.md", "delete.txt")
	runWorkspaceGit(t, repository, "commit", "-q", "-m", "base")
	base := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", source, base)
	return recoveryGitFixture{
		root: root, repository: repository, source: source, base: base,
		workspace: model.RunWorkspace{
			RunID: runID, TaskID: "task-recovery", Kind: model.WorkspaceWorktree,
			Path: source, RepositoryPath: &repository, BaseCommit: &base, Generated: true,
		},
	}
}

func recoveryIndexBytes(t *testing.T, worktree string) []byte {
	t.Helper()
	index := runWorkspaceGit(t, worktree, "rev-parse", "--path-format=absolute", "--git-path", "index")
	value, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func recoveryStatus(t *testing.T, worktree string) []byte {
	t.Helper()
	output, err := gitOutputWithEnv(context.Background(), worktree,
		map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func populateMixedRecoveryChanges(t *testing.T, fixture recoveryGitFixture) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.source, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "add", "committed.txt")
	runWorkspaceGit(t, fixture.source, "commit", "-q", "-m", "worker commit")

	if err := os.WriteFile(filepath.Join(fixture.source, "staged.txt"), []byte("staged version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(fixture.source, "staged.txt"), []byte("working tree version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.source, "README.md"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.source, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.source, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "add", "-u", "delete.txt")
}

func captureMixedRecoveryCheckpoint(t *testing.T, fixture recoveryGitFixture) RecoveryCheckpoint {
	t.Helper()
	populateMixedRecoveryChanges(t, fixture)
	checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
		fixture.workspace.TaskID,
		"mixed worker state",
	)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func addRecoveryTarget(t *testing.T, fixture recoveryGitFixture, name, head, runID string) model.RunWorkspace {
	t.Helper()
	path := filepath.Join(fixture.root, name)
	runWorkspaceGit(t, fixture.repository, "worktree", "add", "-q", "--detach", path, head)
	base := head
	return model.RunWorkspace{
		RunID: runID, TaskID: fixture.workspace.TaskID, Kind: model.WorkspaceWorktree,
		Path: path, RepositoryPath: &fixture.repository, BaseCommit: &base, Generated: true,
	}
}

func TestCaptureRecoveryCheckpointPreservesMixedSourceState(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-mixed")
	populateMixedRecoveryChanges(t, fixture)
	for _, name := range []string{" leading.txt", "trailing.txt ", "   "} {
		if err := os.WriteFile(filepath.Join(fixture.source, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sourceHead := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}")
	sourceStatus := recoveryStatus(t, fixture.source)
	sourceIndex := recoveryIndexBytes(t, fixture.source)

	checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
		fixture.workspace.TaskID,
		"mixed worker state",
	)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.RepositoryPath != fixture.repository ||
		checkpoint.WorktreePath != fixture.source ||
		checkpoint.OutputBaseCommit != fixture.base ||
		checkpoint.SourceStartCommit != fixture.base ||
		checkpoint.SourceHeadCommit != sourceHead ||
		checkpoint.DurableRef != "refs/autogora/checkpoints/run-mixed" {
		t.Fatalf("unexpected checkpoint provenance: %+v", checkpoint)
	}
	expectedFiles := []string{
		"README.md", "committed.txt", "delete.txt", "staged.txt", "untracked.txt",
		" leading.txt", "trailing.txt ", "   ",
	}
	if len(checkpoint.ChangedFiles) != len(expectedFiles) {
		t.Fatalf("changed files = %#v, want %#v", checkpoint.ChangedFiles, expectedFiles)
	}
	seen := make(map[string]bool, len(checkpoint.ChangedFiles))
	for _, name := range checkpoint.ChangedFiles {
		seen[name] = true
	}
	for _, name := range expectedFiles {
		if !seen[name] {
			t.Fatalf("changed files lost %q: %#v", name, checkpoint.ChangedFiles)
		}
	}
	if head := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}"); head != sourceHead {
		t.Fatalf("source HEAD changed from %s to %s", sourceHead, head)
	}
	if status := recoveryStatus(t, fixture.source); !bytes.Equal(status, sourceStatus) {
		t.Fatalf("source status changed:\nold %q\nnew %q", sourceStatus, status)
	}
	if index := recoveryIndexBytes(t, fixture.source); !bytes.Equal(index, sourceIndex) {
		t.Fatal("source index bytes changed during checkpoint capture")
	}
	if got := runWorkspaceGit(t, fixture.repository, "show", checkpoint.HeadCommit+":staged.txt"); got != "working tree version" {
		t.Fatalf("checkpoint staged.txt = %q", got)
	}
	if _, err := gitOutputWithEnv(context.Background(), fixture.repository, nil,
		"cat-file", "-e", checkpoint.HeadCommit+":delete.txt"); err == nil {
		t.Fatal("checkpoint retained a staged deletion")
	}
	if err := ValidateRecoveryCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}

	again, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
		fixture.workspace.TaskID,
		"mixed worker state",
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.HeadCommit != checkpoint.HeadCommit {
		t.Fatalf("idempotent checkpoint head changed from %s to %s", checkpoint.HeadCommit, again.HeadCommit)
	}
}

func TestAdoptRecoveryCheckpointIsByteIdenticalAndKeepsFinalOutputBase(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-source")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	sourceHead := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}")
	sourceStatus := recoveryStatus(t, fixture.source)
	sourceIndex := recoveryIndexBytes(t, fixture.source)
	target := addRecoveryTarget(t, fixture, "target", fixture.base, "run-target")
	// SourceHeadCommit is capture-time diagnostics and is intentionally not
	// required in the persisted recovery reservation reconstructed on restart.
	checkpoint.SourceHeadCommit = ""

	adopted, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Mode != RecoveryAdoptionFastForward ||
		adopted.InitialHeadCommit != fixture.base ||
		adopted.CheckpointHeadCommit != checkpoint.HeadCommit ||
		adopted.AdoptedHeadCommit != checkpoint.HeadCommit ||
		adopted.OutputBaseCommit != fixture.base {
		t.Fatalf("unexpected adoption: %+v", adopted)
	}
	checkpointTree := runWorkspaceGit(t, fixture.repository, "rev-parse", checkpoint.HeadCommit+"^{tree}")
	targetTree := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{tree}")
	if targetTree != checkpointTree {
		t.Fatalf("adopted tree %s differs from checkpoint tree %s", targetTree, checkpointTree)
	}
	if head := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}"); head != sourceHead {
		t.Fatalf("source HEAD changed from %s to %s", sourceHead, head)
	}
	if status := recoveryStatus(t, fixture.source); !bytes.Equal(status, sourceStatus) {
		t.Fatalf("source status changed:\nold %q\nnew %q", sourceStatus, status)
	}
	if index := recoveryIndexBytes(t, fixture.source); !bytes.Equal(index, sourceIndex) {
		t.Fatal("source index bytes changed during checkpoint adoption")
	}

	manager := &Manager{}
	if inspection, err := manager.InspectChangesSince(context.Background(), target, adopted.AdoptedHeadCommit); err != nil || inspection.Changed {
		t.Fatalf("fresh adopted start changed=%v err=%v", inspection.Changed, err)
	}
	if inspection, err := manager.InspectChanges(context.Background(), target); err != nil || !inspection.Changed {
		t.Fatalf("output-base inspection changed=%v err=%v", inspection.Changed, err)
	}
	finalSnapshot, err := manager.CaptureChangeSet(context.Background(), target, target.TaskID, "final retry")
	if err != nil {
		t.Fatal(err)
	}
	if finalSnapshot.BaseCommit != fixture.base ||
		finalSnapshot.HeadCommit != checkpoint.HeadCommit ||
		!slices.Equal(finalSnapshot.ChangedFiles, checkpoint.ChangedFiles) {
		t.Fatalf("final full diff lost recovered work: %+v", finalSnapshot)
	}

	second, err := manager.AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if second.Mode != RecoveryAdoptionAlreadyPresent ||
		second.AdoptedHeadCommit != adopted.AdoptedHeadCommit {
		t.Fatalf("idempotent adoption = %+v", second)
	}
}

func TestAdoptRecoveryCheckpointMergesCompatibleNewOutputBase(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-merge")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	prerequisite := addRecoveryTarget(t, fixture, "prerequisite", fixture.base, "run-prerequisite")
	if err := os.WriteFile(filepath.Join(prerequisite.Path, "prerequisite.txt"), []byte("new prerequisite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, prerequisite.Path, "add", "prerequisite.txt")
	runWorkspaceGit(t, prerequisite.Path, "commit", "-q", "-m", "new prerequisite")
	prerequisiteHead := runWorkspaceGit(t, prerequisite.Path, "rev-parse", "HEAD^{commit}")
	target := addRecoveryTarget(t, fixture, "merge-target", prerequisiteHead, "run-merge-target")

	adopted, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Mode != RecoveryAdoptionMerge ||
		adopted.OutputBaseCommit != prerequisiteHead ||
		adopted.AdoptedHeadCommit == checkpoint.HeadCommit {
		t.Fatalf("unexpected merge adoption: %+v", adopted)
	}
	for _, ancestor := range []string{checkpoint.HeadCommit, prerequisiteHead} {
		if ok, err := gitIsAncestor(context.Background(), target.Path, ancestor, adopted.AdoptedHeadCommit); err != nil || !ok {
			t.Fatalf("%s is not retained by %s: %v", ancestor, adopted.AdoptedHeadCommit, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(target.Path, "prerequisite.txt")); err != nil || string(got) != "new prerequisite\n" {
		t.Fatalf("new output base content = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target.Path, "untracked.txt")); err != nil || string(got) != "untracked\n" {
		t.Fatalf("checkpoint content = %q, %v", got, err)
	}
	finalSnapshot, err := (&Manager{}).CaptureChangeSet(context.Background(), target, target.TaskID, "merged retry")
	if err != nil {
		t.Fatal(err)
	}
	if finalSnapshot.BaseCommit != prerequisiteHead {
		t.Fatalf("final output base changed: got %s want %s", finalSnapshot.BaseCommit, prerequisiteHead)
	}
	if !slices.Contains(finalSnapshot.ChangedFiles, "untracked.txt") ||
		slices.Contains(finalSnapshot.ChangedFiles, "prerequisite.txt") {
		t.Fatalf("final diff = %v", finalSnapshot.ChangedFiles)
	}
	if ok, err := gitIsAncestor(context.Background(), target.Path, checkpoint.HeadCommit, finalSnapshot.HeadCommit); err != nil || !ok {
		t.Fatalf("final snapshot dropped checkpoint ancestry: %v", err)
	}
}

func TestAdoptRecoveryCheckpointMergesIndependentEquivalentOutputBase(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-equivalent-base")
	if err := os.WriteFile(filepath.Join(fixture.source, "prerequisite.txt"), []byte("same prerequisite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "add", "prerequisite.txt")
	runWorkspaceGit(t, fixture.source, "commit", "-q", "-m", "equivalent prerequisite A")
	checkpointBase := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}")
	fixture.workspace.BaseCommit = &checkpointBase
	if err := os.WriteFile(filepath.Join(fixture.source, "worker.txt"), []byte("recovered work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		checkpointBase,
		fixture.workspace.TaskID,
		"equivalent base",
	)
	if err != nil {
		t.Fatal(err)
	}

	equivalent := addRecoveryTarget(t, fixture, "equivalent-prerequisite", fixture.base, "run-equivalent-prerequisite")
	if err := os.WriteFile(filepath.Join(equivalent.Path, "prerequisite.txt"), []byte("same prerequisite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, equivalent.Path, "add", "prerequisite.txt")
	runWorkspaceGit(t, equivalent.Path, "commit", "-q", "-m", "equivalent prerequisite B")
	targetBase := runWorkspaceGit(t, equivalent.Path, "rev-parse", "HEAD^{commit}")
	if targetBase == checkpointBase {
		t.Fatal("fixture output bases unexpectedly share a commit")
	}
	checkpointTree := runWorkspaceGit(t, fixture.repository, "rev-parse", checkpointBase+"^{tree}")
	targetTree := runWorkspaceGit(t, fixture.repository, "rev-parse", targetBase+"^{tree}")
	if checkpointTree != targetTree {
		t.Fatalf("fixture output-base trees differ: %s != %s", checkpointTree, targetTree)
	}
	target := addRecoveryTarget(t, fixture, "equivalent-target", targetBase, "run-equivalent-target")

	adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.Mode != RecoveryAdoptionMerge ||
		adoption.AdoptedHeadCommit == checkpoint.HeadCommit ||
		adoption.AdoptedHeadCommit == targetBase {
		t.Fatalf("equivalent bases were not joined with a no-ff merge: %+v", adoption)
	}
	for _, ancestor := range []string{checkpoint.HeadCommit, targetBase} {
		ok, ancestorErr := gitIsAncestor(
			context.Background(),
			target.Path,
			ancestor,
			adoption.AdoptedHeadCommit,
		)
		if ancestorErr != nil || !ok {
			t.Fatalf("adoption dropped %s: %v", ancestor, ancestorErr)
		}
	}
}

func TestAdoptRecoveryCheckpointRejectsOlderTargetOutputBase(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-backward-base")
	if err := os.WriteFile(filepath.Join(fixture.source, "prerequisite.txt"), []byte("new prerequisite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "add", "prerequisite.txt")
	runWorkspaceGit(t, fixture.source, "commit", "-q", "-m", "new checkpoint base")
	checkpointBase := runWorkspaceGit(t, fixture.source, "rev-parse", "HEAD^{commit}")
	fixture.workspace.BaseCommit = &checkpointBase
	if err := os.WriteFile(filepath.Join(fixture.source, "worker.txt"), []byte("worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		checkpointBase,
		fixture.workspace.TaskID,
		"backward base",
	)
	if err != nil {
		t.Fatal(err)
	}
	target := addRecoveryTarget(t, fixture, "backward-target", fixture.base, "run-backward-target")

	_, err = (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("backward-base error = %v", err)
	}
	if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != fixture.base {
		t.Fatalf("backward-base rejection changed target HEAD to %s", head)
	}
}

func TestRecoveryCheckpointCumulativeFailureUsesExactOutputBaseDiff(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-first-failure")
	if err := os.WriteFile(filepath.Join(fixture.source, "README.md"), []byte("first failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.source, "kept.txt"), []byte("keep from first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
		fixture.workspace.TaskID,
		"first failure",
	)
	if err != nil {
		t.Fatal(err)
	}

	secondWorkspace := addRecoveryTarget(
		t,
		fixture,
		"second-attempt",
		fixture.base,
		"run-second-failure",
	)
	firstAdoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
		context.Background(),
		secondWorkspace,
		first,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondWorkspace.Path, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondWorkspace.Path, "second.txt"), []byte("second failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(),
		secondWorkspace,
		firstAdoption.AdoptedHeadCommit,
		secondWorkspace.TaskID,
		"second failure",
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"kept.txt", "second.txt"}
	if !slices.Equal(second.ChangedFiles, expected) {
		t.Fatalf(
			"second checkpoint changed files = %#v, want exact output-base diff %#v",
			second.ChangedFiles,
			expected,
		)
	}
	if slices.Contains(second.ChangedFiles, "README.md") {
		t.Fatalf("reverted first-attempt file leaked into cumulative manifest: %#v", second.ChangedFiles)
	}
	thirdWorkspace := addRecoveryTarget(
		t,
		fixture,
		"third-attempt",
		fixture.base,
		"run-third-attempt",
	)
	secondAdoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
		context.Background(),
		thirdWorkspace,
		second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(thirdWorkspace.Path, "README.md")); err != nil || string(got) != "base\n" {
		t.Fatalf("reverted content was not preserved: %q, %v", got, err)
	}
	for _, ancestor := range []string{first.HeadCommit, second.HeadCommit} {
		ok, ancestorErr := gitIsAncestor(
			context.Background(),
			thirdWorkspace.Path,
			ancestor,
			secondAdoption.AdoptedHeadCommit,
		)
		if ancestorErr != nil || !ok {
			t.Fatalf("cumulative adoption dropped %s: %v", ancestor, ancestorErr)
		}
	}
}

func TestAdoptRecoveryCheckpointRejectsTamperedRefWithoutChangingTarget(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-tampered")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	target := addRecoveryTarget(t, fixture, "tampered-target", fixture.base, "run-tampered-target")
	runWorkspaceGit(t, fixture.repository, "update-ref", checkpoint.DurableRef, fixture.base)

	_, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("tampered ref error = %v", err)
	}
	if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != fixture.base {
		t.Fatalf("target HEAD changed to %s", head)
	}
	if status := recoveryStatus(t, target.Path); len(status) != 0 {
		t.Fatalf("target became dirty: %q", status)
	}
}

func TestValidateRecoveryCheckpointRejectsTamperedAncestry(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-ancestry")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	unrelated := addRecoveryTarget(t, fixture, "unrelated", fixture.base, "run-unrelated")
	runWorkspaceGit(t, unrelated.Path, "checkout", "--orphan", "unrelated-history")
	runWorkspaceGit(t, unrelated.Path, "rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(unrelated.Path, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, unrelated.Path, "add", "other.txt")
	runWorkspaceGit(t, unrelated.Path, "commit", "-q", "-m", "unrelated")
	checkpoint.SourceStartCommit = runWorkspaceGit(t, unrelated.Path, "rev-parse", "HEAD^{commit}")

	err := ValidateRecoveryCheckpoint(context.Background(), checkpoint)
	if err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("tampered ancestry error = %v", err)
	}
}

func TestAdoptRecoveryCheckpointAllowsRemovedSourceWorktree(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-removed-source")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	runWorkspaceGit(
		t,
		fixture.repository,
		"worktree",
		"remove",
		"--force",
		fixture.source,
	)
	if _, err := os.Stat(fixture.source); !os.IsNotExist(err) {
		t.Fatalf("source worktree still exists or cannot be inspected: %v", err)
	}
	if err := ValidateRecoveryCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("durable checkpoint required its removed source: %v", err)
	}
	target := addRecoveryTarget(t, fixture, "removed-source-target", fixture.base, "run-removed-source-target")
	adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
		context.Background(),
		target,
		checkpoint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if adoption.AdoptedHeadCommit != checkpoint.HeadCommit {
		t.Fatalf("removed-source adoption = %+v", adoption)
	}
}

func TestLoadPublishedRecoveryCheckpointAfterSourceWorktreeDisappears(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-published-gap")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	runWorkspaceGit(
		t,
		fixture.repository,
		"worktree",
		"remove",
		"--force",
		fixture.source,
	)

	loaded, err := (&Manager{}).LoadPublishedRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
	)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil ||
		loaded.RunID != checkpoint.RunID ||
		loaded.RepositoryPath != checkpoint.RepositoryPath ||
		loaded.WorktreePath != checkpoint.WorktreePath ||
		loaded.OutputBaseCommit != checkpoint.OutputBaseCommit ||
		loaded.SourceStartCommit != checkpoint.SourceStartCommit ||
		loaded.SourceHeadCommit != "" ||
		loaded.HeadCommit != checkpoint.HeadCommit ||
		loaded.DurableRef != checkpoint.DurableRef ||
		!slices.Equal(loaded.ChangedFiles, checkpoint.ChangedFiles) {
		t.Fatalf("loaded checkpoint = %+v, captured = %+v", loaded, checkpoint)
	}
}

func TestLoadPublishedRecoveryCheckpointReturnsNilWithoutRunRef(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-no-published-checkpoint")
	runWorkspaceGit(
		t,
		fixture.repository,
		"worktree",
		"remove",
		"--force",
		fixture.source,
	)

	loaded, err := (&Manager{}).LoadPublishedRecoveryCheckpoint(
		context.Background(),
		fixture.workspace,
		fixture.base,
	)
	if err != nil || loaded != nil {
		t.Fatalf("missing published checkpoint = %+v, err=%v", loaded, err)
	}
}

func TestReissueAdoptedRecoveryCheckpointAfterTargetDisappears(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-reissue-source")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	target := addRecoveryTarget(
		t,
		fixture,
		"reissue-target",
		fixture.base,
		"run-reissue-target",
	)
	adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
		context.Background(),
		target,
		checkpoint,
	)
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(
		t,
		fixture.repository,
		"worktree",
		"remove",
		"--force",
		target.Path,
	)
	if _, err := os.Stat(target.Path); !os.IsNotExist(err) {
		t.Fatalf("adopted target still exists or cannot be inspected: %v", err)
	}

	reissued, err := (&Manager{}).ReissueAdoptedRecoveryCheckpoint(
		context.Background(),
		checkpoint,
		target.RunID,
		target.Path,
		adoption.OutputBaseCommit,
		adoption.AdoptedHeadCommit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reissued.RunID != target.RunID ||
		reissued.WorktreePath != target.Path ||
		reissued.OutputBaseCommit != adoption.OutputBaseCommit ||
		reissued.SourceStartCommit != adoption.AdoptedHeadCommit ||
		reissued.SourceHeadCommit != adoption.AdoptedHeadCommit ||
		reissued.HeadCommit != adoption.AdoptedHeadCommit ||
		reissued.DurableRef != "refs/autogora/checkpoints/"+target.RunID ||
		!slices.Equal(reissued.ChangedFiles, adoption.ChangedFiles) {
		t.Fatalf("reissued checkpoint = %+v, adoption = %+v", reissued, adoption)
	}
	if err := ValidateRecoveryCheckpoint(
		context.Background(),
		reissued,
	); err != nil {
		t.Fatal(err)
	}
	again, err := (&Manager{}).ReissueAdoptedRecoveryCheckpoint(
		context.Background(),
		checkpoint,
		target.RunID,
		target.Path,
		adoption.OutputBaseCommit,
		adoption.AdoptedHeadCommit,
	)
	if err != nil || again.HeadCommit != reissued.HeadCommit {
		t.Fatalf("idempotent reissue = %+v, err=%v", again, err)
	}
}

func TestReissueAdoptedRecoveryCheckpointRejectsConflictingRunRef(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-reissue-conflict-source")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	const runID = "run-reissue-conflict"
	ref, err := recoveryCheckpointRef(runID)
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.repository, "update-ref", ref, fixture.base)

	_, err = (&Manager{}).ReissueAdoptedRecoveryCheckpoint(
		context.Background(),
		checkpoint,
		runID,
		filepath.Join(fixture.root, "missing-target"),
		fixture.base,
		checkpoint.HeadCommit,
	)
	if err == nil || !strings.Contains(err.Error(), "different work") {
		t.Fatalf("conflicting reissue ref error = %v", err)
	}
	if head := runWorkspaceGit(t, fixture.repository, "rev-parse", ref); head != fixture.base {
		t.Fatalf("conflicting reissue changed ref to %s", head)
	}
}

func TestRecoveryCheckpointRequiresExactWorktreeTopLevel(t *testing.T) {
	t.Run("preserves whitespace in worktree paths", func(t *testing.T) {
		root := t.TempDir()
		repository := filepath.Join(root, " repository ")
		source := filepath.Join(root, " source ")
		targetPath := filepath.Join(root, " target ")
		if err := os.MkdirAll(repository, 0o755); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, repository, "init", "-q")
		if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, repository, "add", "README.md")
		runWorkspaceGit(t, repository, "commit", "-q", "-m", "base")
		base := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")
		runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", source, base)
		if err := os.WriteFile(filepath.Join(source, "worker.txt"), []byte("worker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		workspace := model.RunWorkspace{
			RunID: "run-whitespace-path", TaskID: "task-whitespace-path",
			Kind: model.WorkspaceWorktree, Path: source,
			RepositoryPath: &repository, BaseCommit: &base, Generated: true,
		}
		checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
			context.Background(),
			workspace,
			base,
			workspace.TaskID,
			"whitespace path",
		)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.RepositoryPath != repository || checkpoint.WorktreePath != source {
			t.Fatalf("checkpoint paths were changed: %+v", checkpoint)
		}
		runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", targetPath, base)
		target := model.RunWorkspace{
			RunID: "run-whitespace-target", TaskID: workspace.TaskID,
			Kind: model.WorkspaceWorktree, Path: targetPath,
			RepositoryPath: &repository, BaseCommit: &base, Generated: true,
		}
		if _, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("canonical source alias stores top-level", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-source-alias")
		if err := os.WriteFile(filepath.Join(fixture.source, "worker.txt"), []byte("worker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(fixture.root, "source-alias")
		if err := os.Symlink(fixture.source, alias); err != nil {
			t.Fatal(err)
		}
		workspace := fixture.workspace
		workspace.Path = alias
		checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
			context.Background(),
			workspace,
			fixture.base,
			workspace.TaskID,
			"canonical source",
		)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.WorktreePath != fixture.source {
			t.Fatalf("stored source path = %s, want canonical top-level %s", checkpoint.WorktreePath, fixture.source)
		}
	})

	t.Run("capture source subdirectory", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-source-subdir")
		subdirectory := filepath.Join(fixture.source, "nested")
		if err := os.MkdirAll(subdirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		workspace := fixture.workspace
		workspace.Path = subdirectory
		_, err := (&Manager{}).CaptureRecoveryCheckpoint(
			context.Background(),
			workspace,
			fixture.base,
			workspace.TaskID,
			"subdirectory",
		)
		if err == nil || !strings.Contains(err.Error(), "exact Git worktree top-level") {
			t.Fatalf("source-subdirectory error = %v", err)
		}
	})

	t.Run("persisted source path is audit only", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-persisted-subdir")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		subdirectory := filepath.Join(fixture.source, "nested")
		if err := os.MkdirAll(subdirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		checkpoint.WorktreePath = subdirectory
		err := ValidateRecoveryCheckpoint(context.Background(), checkpoint)
		if err != nil {
			t.Fatalf("audit-only source path blocked immutable validation: %v", err)
		}
	})

	t.Run("target subdirectory", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-target-subdir")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "subdir-target", fixture.base, "run-subdir-target")
		subdirectory := filepath.Join(target.Path, "nested")
		if err := os.MkdirAll(subdirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		target.Path = subdirectory
		_, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err == nil || !strings.Contains(err.Error(), "exact Git worktree top-level") {
			t.Fatalf("target-subdirectory error = %v", err)
		}
	})

	t.Run("same physical worktree alias", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-same-worktree")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		alias := filepath.Join(fixture.root, "source-alias")
		if err := os.Symlink(fixture.source, alias); err != nil {
			t.Fatal(err)
		}
		target := fixture.workspace
		target.RunID = "run-same-worktree-target"
		target.Path = alias
		_, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err == nil || !strings.Contains(err.Error(), "distinct worktree") {
			t.Fatalf("same-worktree error = %v", err)
		}
	})
}

func TestAdoptRecoveryCheckpointRequiresExactTargetBaseObjectID(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-exact-target-base")
	checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
	target := addRecoveryTarget(t, fixture, "abbreviated-base-target", fixture.base, "run-abbreviated-base-target")
	abbreviated := fixture.base[:12]
	target.BaseCommit = &abbreviated

	_, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
	if err == nil || !strings.Contains(err.Error(), "exact full Git object ID") {
		t.Fatalf("abbreviated-target-base error = %v", err)
	}
	if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != fixture.base {
		t.Fatalf("invalid target base changed HEAD to %s", head)
	}
}

func TestRollbackRecoveryCheckpointAdoption(t *testing.T) {
	t.Run("restores initial clean head", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-rollback")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "rollback-target", fixture.base, "run-rollback-target")
		adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := (&Manager{}).RollbackRecoveryCheckpointAdoption(
			context.Background(),
			adoption,
		); err != nil {
			t.Fatal(err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != fixture.base {
			t.Fatalf("rolled-back HEAD = %s, want %s", head, fixture.base)
		}
		if status := recoveryStatus(t, target.Path); len(status) != 0 {
			t.Fatalf("rolled-back target is dirty: %q", status)
		}
		refHead, exists, err := exactRefHead(
			context.Background(),
			fixture.repository,
			checkpoint.DurableRef,
		)
		if err != nil || !exists || refHead != checkpoint.HeadCommit {
			t.Fatalf("rollback changed durable ref: head=%s exists=%v err=%v", refHead, exists, err)
		}
	})

	t.Run("refuses external worktree change", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-rollback-dirty")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "rollback-dirty-target", fixture.base, "run-rollback-dirty-target")
		adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err != nil {
			t.Fatal(err)
		}
		externalPath := filepath.Join(target.Path, "external.txt")
		if err := os.WriteFile(externalPath, []byte("external\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err = (&Manager{}).RollbackRecoveryCheckpointAdoption(
			context.Background(),
			adoption,
		)
		if err == nil || !strings.Contains(err.Error(), "changed after adoption") {
			t.Fatalf("dirty rollback error = %v", err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != adoption.AdoptedHeadCommit {
			t.Fatalf("refused dirty rollback changed HEAD to %s", head)
		}
		if got, readErr := os.ReadFile(externalPath); readErr != nil || string(got) != "external\n" {
			t.Fatalf("refused rollback changed external file: %q, %v", got, readErr)
		}
	})

	t.Run("refuses external head", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-rollback-head")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "rollback-head-target", fixture.base, "run-rollback-head-target")
		adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target.Path, "external.txt"), []byte("external\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, target.Path, "add", "external.txt")
		runWorkspaceGit(t, target.Path, "commit", "-q", "-m", "external")
		externalHead := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}")
		err = (&Manager{}).RollbackRecoveryCheckpointAdoption(
			context.Background(),
			adoption,
		)
		if err == nil || !strings.Contains(err.Error(), "HEAD changed") {
			t.Fatalf("moved-head rollback error = %v", err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != externalHead {
			t.Fatalf("refused moved-head rollback changed HEAD to %s", head)
		}
	})

	t.Run("refuses changed worktree identity", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-rollback-identity")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "rollback-identity-target", fixture.base, "run-rollback-identity-target")
		adoption, err := (&Manager{}).AdoptRecoveryCheckpoint(
			context.Background(),
			target,
			checkpoint,
		)
		if err != nil {
			t.Fatal(err)
		}
		adoption.WorktreeGitDirectory = filepath.Join(fixture.root, "different-git-directory")
		err = (&Manager{}).RollbackRecoveryCheckpointAdoption(
			context.Background(),
			adoption,
		)
		if err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("changed-identity rollback error = %v", err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != adoption.AdoptedHeadCommit {
			t.Fatalf("refused identity rollback changed HEAD to %s", head)
		}
	})
}

func TestAdoptRecoveryCheckpointRejectsIncompatibleBaseAndConflict(t *testing.T) {
	t.Run("incompatible ancestry", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-incompatible")
		checkpoint := captureMixedRecoveryCheckpoint(t, fixture)
		target := addRecoveryTarget(t, fixture, "incompatible-target", fixture.base, "run-incompatible-target")
		runWorkspaceGit(t, target.Path, "checkout", "--orphan", "incompatible-history")
		runWorkspaceGit(t, target.Path, "rm", "-q", "-rf", ".")
		if err := os.WriteFile(filepath.Join(target.Path, "other.txt"), []byte("other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, target.Path, "add", "other.txt")
		runWorkspaceGit(t, target.Path, "commit", "-q", "-m", "incompatible")
		incompatibleBase := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}")
		target.BaseCommit = &incompatibleBase

		_, err := (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
		if err == nil || !strings.Contains(err.Error(), "do not share ancestry") {
			t.Fatalf("incompatible-base error = %v", err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != incompatibleBase {
			t.Fatalf("target HEAD changed to %s", head)
		}
	})

	t.Run("merge conflict rolls back", func(t *testing.T) {
		fixture := newRecoveryGitFixture(t, "run-conflict")
		if err := os.WriteFile(filepath.Join(fixture.source, "README.md"), []byte("checkpoint\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := (&Manager{}).CaptureRecoveryCheckpoint(
			context.Background(), fixture.workspace, fixture.base, fixture.workspace.TaskID, "conflict",
		)
		if err != nil {
			t.Fatal(err)
		}
		prerequisite := addRecoveryTarget(t, fixture, "conflicting-prerequisite", fixture.base, "run-conflicting-prerequisite")
		if err := os.WriteFile(filepath.Join(prerequisite.Path, "README.md"), []byte("prerequisite\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, prerequisite.Path, "commit", "-q", "-am", "conflicting prerequisite")
		prerequisiteHead := runWorkspaceGit(t, prerequisite.Path, "rev-parse", "HEAD^{commit}")
		target := addRecoveryTarget(t, fixture, "conflict-target", prerequisiteHead, "run-conflict-target")
		sourceStatus := recoveryStatus(t, fixture.source)

		_, err = (&Manager{}).AdoptRecoveryCheckpoint(context.Background(), target, checkpoint)
		if err == nil || !strings.Contains(err.Error(), "conflicts in README.md") {
			t.Fatalf("conflict error = %v", err)
		}
		if head := runWorkspaceGit(t, target.Path, "rev-parse", "HEAD^{commit}"); head != prerequisiteHead {
			t.Fatalf("target HEAD after rollback = %s, want %s", head, prerequisiteHead)
		}
		if status := recoveryStatus(t, target.Path); len(status) != 0 {
			t.Fatalf("target status after rollback = %q", status)
		}
		if status := recoveryStatus(t, fixture.source); !bytes.Equal(status, sourceStatus) {
			t.Fatalf("source status changed during rejected adoption: %q", status)
		}
	})
}

func TestCaptureRecoveryCheckpointRejectsUnresolvedConflict(t *testing.T) {
	fixture := newRecoveryGitFixture(t, "run-unmerged")
	other := addRecoveryTarget(t, fixture, "other", fixture.base, "run-other")
	if err := os.WriteFile(filepath.Join(other.Path, "README.md"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, other.Path, "commit", "-q", "-am", "other")
	otherHead := runWorkspaceGit(t, other.Path, "rev-parse", "HEAD^{commit}")
	if err := os.WriteFile(filepath.Join(fixture.source, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, fixture.source, "commit", "-q", "-am", "source")
	command := exec.Command("git", "-C", fixture.source, "merge", "--no-ff", "--no-edit", otherHead)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_MERGE_AUTOEDIT=no")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
		t.Fatalf("fixture did not conflict: %v\n%s", err, output)
	}

	_, err := (&Manager{}).CaptureRecoveryCheckpoint(
		context.Background(), fixture.workspace, fixture.base, fixture.workspace.TaskID, "unmerged",
	)
	if err == nil || !strings.Contains(err.Error(), "unresolved conflicts") {
		t.Fatalf("unresolved-conflict error = %v", err)
	}
	ref, refErr := recoveryCheckpointRef(fixture.workspace.RunID)
	if refErr != nil {
		t.Fatal(refErr)
	}
	if _, exists, refErr := exactRefHead(context.Background(), fixture.repository, ref); refErr != nil || exists {
		t.Fatalf("unexpected checkpoint ref exists=%v err=%v", exists, refErr)
	}
}
