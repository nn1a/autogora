package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type prerequisiteGitFixture struct {
	repository string
	base       string
	head       string
	handoff    model.PrerequisiteHandoff
}

type resolutionStartFixture struct {
	repository  string
	worktree    string
	initialHead string
	resolution  model.IntegrationResolution
}

func mustGitInput(t *testing.T, directory string, input []byte, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func newResolutionStartFixture(t *testing.T) resolutionStartFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
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
	commitAtBase := func(name, file, contents string) (string, string) {
		t.Helper()
		worktree := filepath.Join(root, name)
		runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", worktree, base)
		if err := os.WriteFile(filepath.Join(worktree, file), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, worktree, "add", file)
		runWorkspaceGit(t, worktree, "commit", "-q", "-m", name)
		return worktree, runWorkspaceGit(t, worktree, "rev-parse", "HEAD^{commit}")
	}
	_, firstHead := commitAtBase("first", "README.md", "first\n")
	_, conflictingHead := commitAtBase("conflicting", "README.md", "second\n")
	_, pendingHead := commitAtBase("pending", "pending.txt", "pending\n")
	firstRef, conflictRef, pendingRef := "refs/autogora/runs/first", "refs/autogora/runs/conflict", "refs/autogora/runs/pending"
	runWorkspaceGit(t, repository, "update-ref", firstRef, firstHead)
	runWorkspaceGit(t, repository, "update-ref", conflictRef, conflictingHead)
	runWorkspaceGit(t, repository, "update-ref", pendingRef, pendingHead)

	target := filepath.Join(root, "target")
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", target, firstHead)
	command := exec.Command("git", "-C", target, "merge", "--no-ff", "--no-edit", conflictingHead)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_MERGE_AUTOEDIT=no")
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
		t.Fatalf("fixture merge did not conflict: %v\n%s", err, output)
	}
	rawIndex, err := gitOutputWithEnv(context.Background(), target, nil, "ls-files", "-u", "-z")
	if err != nil || len(rawIndex) == 0 {
		t.Fatalf("capture fixture conflict: %q, %v", rawIndex, err)
	}
	targets := []model.IntegrationResolutionTarget{
		{
			PrerequisiteID: "conflict", ChangeSetID: "change-conflict",
			HeadCommit: conflictingHead, DurableRef: conflictRef, MergeInProgress: true,
		},
		{
			PrerequisiteID: "pending", ChangeSetID: "change-pending",
			HeadCommit: pendingHead, DurableRef: pendingRef,
		},
	}
	return resolutionStartFixture{
		repository: repository, worktree: target, initialHead: firstHead,
		resolution: model.IntegrationResolution{
			ConflictFingerprint: integrationConflictFingerprintFromHeads(
				rawIndex,
				[]string{conflictingHead, pendingHead},
			),
			WorkspacePath: target,
			Targets:       targets,
		},
	}
}

func newPrerequisiteGitFixture(t *testing.T) prerequisiteGitFixture {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
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
	if err := os.WriteFile(filepath.Join(repository, "parent.txt"), []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "add", "parent.txt")
	runWorkspaceGit(t, repository, "commit", "-q", "-m", "parent")
	head := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")
	runID := "run-parent"
	durableRef, err := durableRunRef(runID)
	if err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "update-ref", durableRef, head)
	return prerequisiteGitFixture{
		repository: repository, base: base, head: head,
		handoff: model.PrerequisiteHandoff{
			PrerequisiteID: "task-parent", DependentID: "task-child", SatisfiedRunID: &runID,
			Run: &model.Run{ID: runID, TaskID: "task-parent", Status: model.RunStatusCompleted},
			ChangeSet: &model.ChangeSet{
				ID: "cs-parent", RunID: runID, TaskID: "task-parent", RepositoryPath: repository,
				BaseCommit: base, HeadCommit: head, DurableRef: durableRef, State: "ready",
			},
		},
	}
}

func TestDirWorkspaceAcceptsPrerequisiteAlreadyInHead(t *testing.T) {
	fixture := newPrerequisiteGitFixture(t)
	workspace := model.RunWorkspace{
		Kind: model.WorkspaceDir, Path: fixture.repository, RepositoryPath: &fixture.repository, BaseCommit: &fixture.head,
	}
	result, err := (&Manager{}).integratePrerequisiteHandoffs(context.Background(), workspace, []model.PrerequisiteHandoff{fixture.handoff})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 || len(result.AlreadyPresent) != 1 || result.EffectiveBaseCommit != fixture.head {
		t.Fatalf("unexpected already-present result: %#v", result)
	}
}

func TestDirWorkspaceBlocksMissingPrerequisiteWithoutMerging(t *testing.T) {
	fixture := newPrerequisiteGitFixture(t)
	runWorkspaceGit(t, fixture.repository, "reset", "--hard", fixture.base)
	workspace := model.RunWorkspace{
		Kind: model.WorkspaceDir, Path: fixture.repository, RepositoryPath: &fixture.repository, BaseCommit: &fixture.base,
	}
	_, err := (&Manager{}).integratePrerequisiteHandoffs(context.Background(), workspace, []model.PrerequisiteHandoff{fixture.handoff})
	var integrationErr *PrerequisiteIntegrationError
	if !errors.As(err, &integrationErr) || integrationErr.Code != IntegrationFailureUnsupportedWorkspace || integrationErr.BlockKind != model.BlockKindCapability {
		t.Fatalf("missing prerequisite was not a capability block: %#v", err)
	}
	if head := runWorkspaceGit(t, fixture.repository, "rev-parse", "HEAD^{commit}"); head != fixture.base {
		t.Fatalf("shared directory was modified: got %s want %s", head, fixture.base)
	}
}

func TestPrerequisiteReferenceMustMatchPinnedRun(t *testing.T) {
	fixture := newPrerequisiteGitFixture(t)
	otherRef := "refs/autogora/runs/run-other"
	runWorkspaceGit(t, fixture.repository, "update-ref", otherRef, fixture.head)
	fixture.handoff.ChangeSet.DurableRef = otherRef
	workspace := model.RunWorkspace{
		Kind: model.WorkspaceDir, Path: fixture.repository, RepositoryPath: &fixture.repository, BaseCommit: &fixture.head,
	}
	_, err := (&Manager{}).integratePrerequisiteHandoffs(context.Background(), workspace, []model.PrerequisiteHandoff{fixture.handoff})
	var integrationErr *PrerequisiteIntegrationError
	if !errors.As(err, &integrationErr) || integrationErr.Code != IntegrationFailureInvalidReference || integrationErr.BlockKind != model.BlockKindCapability {
		t.Fatalf("mismatched durable ref was accepted: %#v", err)
	}
}

func TestAggregateFinalizerChangeSetPreventsRepeatedFanInConflict(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
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

	commitVersion := func(name, contents string) (string, string) {
		t.Helper()
		worktree := filepath.Join(root, name)
		runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", worktree, base)
		if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, worktree, "commit", "-q", "-am", name)
		return worktree, runWorkspaceGit(t, worktree, "rev-parse", "HEAD^{commit}")
	}
	firstWorktree, firstHead := commitVersion("first", "first\n")
	secondWorktree, secondHead := commitVersion("second", "second\n")

	aggregateWorktree := filepath.Join(root, "aggregate")
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", aggregateWorktree, base)
	runWorkspaceGit(t, aggregateWorktree, "merge", "--no-ff", "--no-edit", firstHead)
	conflicting := exec.Command("git", "-C", aggregateWorktree, "merge", "--no-ff", "--no-edit", secondHead)
	conflicting.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_MERGE_AUTOEDIT=no")
	if output, err := conflicting.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
		t.Fatalf("fixture merge did not conflict: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(aggregateWorktree, "README.md"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, aggregateWorktree, "add", "README.md")
	runWorkspaceGit(t, aggregateWorktree, "commit", "-q", "--no-edit")
	aggregateHead := runWorkspaceGit(t, aggregateWorktree, "rev-parse", "HEAD^{commit}")

	handoff := func(id, runID, worktree, head string) model.PrerequisiteHandoff {
		t.Helper()
		ref, err := durableRunRef(runID)
		if err != nil {
			t.Fatal(err)
		}
		runWorkspaceGit(t, repository, "update-ref", ref, head)
		return model.PrerequisiteHandoff{
			PrerequisiteID: id, DependentID: "downstream", SatisfiedRunID: &runID,
			Run: &model.Run{ID: runID, TaskID: id, Status: model.RunStatusCompleted},
			ChangeSet: &model.ChangeSet{
				ID: "cs-" + id, RunID: runID, TaskID: id, RepositoryPath: repository,
				WorktreePath: worktree, BaseCommit: base, HeadCommit: head,
				DurableRef: ref, State: "ready",
			},
		}
	}
	handoffs := []model.PrerequisiteHandoff{
		handoff("a-original", "run-first", firstWorktree, firstHead),
		handoff("b-original", "run-second", secondWorktree, secondHead),
		handoff("z-finalizer", "run-finalizer", aggregateWorktree, aggregateHead),
	}
	target := filepath.Join(root, "downstream")
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", target, base)
	workspace := model.RunWorkspace{
		Kind: model.WorkspaceWorktree, Path: target, RepositoryPath: &repository, BaseCommit: &base,
	}
	result, err := (&Manager{}).integratePrerequisiteHandoffs(context.Background(), workspace, handoffs)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0].PrerequisiteID != "z-finalizer" {
		t.Fatalf("aggregate change set was not integrated first: %+v", result)
	}
	if len(result.AlreadyPresent) != 2 {
		t.Fatalf("original heads were not already present: %+v", result)
	}
	already := []string{result.AlreadyPresent[0].PrerequisiteID, result.AlreadyPresent[1].PrerequisiteID}
	slices.Sort(already)
	if !slices.Equal(already, []string{"a-original", "b-original"}) {
		t.Fatalf("original heads were not recognized through aggregate history: %+v", result)
	}
	for _, head := range []string{firstHead, secondHead, aggregateHead} {
		if _, err := gitOutputWithEnv(context.Background(), target, nil, "merge-base", "--is-ancestor", head, "HEAD"); err != nil {
			t.Fatalf("integrated history dropped %s: %v", head, err)
		}
	}
}

func TestValidateIntegrationResolutionStartAcceptsUnchangedConflict(t *testing.T) {
	fixture := newResolutionStartFixture(t)
	if err := ValidateIntegrationResolutionStart(context.Background(), fixture.resolution); err != nil {
		t.Fatal(err)
	}
}

func TestValidateIntegrationResolutionStartRejectsStaleIndex(t *testing.T) {
	fixture := newResolutionStartFixture(t)
	blob := strings.TrimSpace(string(mustGitInput(t, fixture.worktree, []byte("changed conflict stage\n"),
		"hash-object", "-w", "--stdin")))
	indexLine := fmt.Sprintf("100644 %s 2\tREADME.md\n", blob)
	mustGitInput(t, fixture.worktree, []byte(indexLine), "update-index", "--index-info")
	err := ValidateIntegrationResolutionStart(context.Background(), fixture.resolution)
	if err == nil || !strings.Contains(err.Error(), "fingerprint changed") {
		t.Fatalf("stale conflict error = %v", err)
	}
}

func TestValidateIntegrationResolutionStartRejectsAbortedMerge(t *testing.T) {
	fixture := newResolutionStartFixture(t)
	runWorkspaceGit(t, fixture.worktree, "merge", "--abort")
	err := ValidateIntegrationResolutionStart(context.Background(), fixture.resolution)
	if err == nil || !strings.Contains(err.Error(), "no longer in progress") {
		t.Fatalf("aborted conflict error = %v", err)
	}
}

func TestValidateIntegrationResolutionStartRejectsMovedDurableRef(t *testing.T) {
	fixture := newResolutionStartFixture(t)
	runWorkspaceGit(t, fixture.repository, "update-ref",
		fixture.resolution.Targets[1].DurableRef, fixture.initialHead)
	err := ValidateIntegrationResolutionStart(context.Background(), fixture.resolution)
	if err == nil || !strings.Contains(err.Error(), "durable ref") ||
		!strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed durable ref error = %v", err)
	}
}

func TestRollbackIntegrationResetsAfterMergeAbortFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based git fault injection is Unix-only")
	}
	fixture := newResolutionStartFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "reset-called")
	wrapper := filepath.Join(bin, "git")
	quotedGit := "'" + strings.ReplaceAll(realGit, "'", "'\"'\"'") + "'"
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = merge ] && [ \"$4\" = --abort ]; then exit 73; fi\n" +
		"if [ \"$3\" = reset ] && [ \"$4\" = --hard ]; then : > " + quotedMarker + "; fi\n" +
		"exec " + quotedGit + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = rollbackIntegration(context.Background(), fixture.worktree, fixture.initialHead)
	if err == nil || !strings.Contains(err.Error(), "abort prerequisite merge") {
		t.Fatalf("rollback error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reset was not attempted after abort failure: %v", err)
	}
	if head := runWorkspaceGit(t, fixture.worktree, "rev-parse", "HEAD^{commit}"); head != fixture.initialHead {
		t.Fatalf("rollback HEAD = %s, want %s", head, fixture.initialHead)
	}
	if unmerged := runWorkspaceGit(t, fixture.worktree, "ls-files", "-u"); unmerged != "" {
		t.Fatalf("rollback left unresolved index: %q", unmerged)
	}
}

func TestIntegratePrerequisiteChangeSetsReportsInitialHEADFailure(t *testing.T) {
	ctx := context.Background()
	opened, err := store.Open(":memory:", "default", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "finalizer"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "missing worktree", Assignee: &assignee, Runtime: model.RuntimeCline,
		WorkflowRole: model.WorkflowRoleFinalizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v, %v", claim, err)
	}
	claim.Workspace = &model.RunWorkspace{
		RunID: claim.Run.ID, TaskID: task.Task.ID, Kind: model.WorkspaceWorktree,
		Path: filepath.Join(t.TempDir(), "missing"),
	}
	_, err = (&Manager{}).IntegratePrerequisiteChangeSets(ctx, opened, claim)
	if err == nil || !strings.Contains(err.Error(), "capture initial finalizer worktree HEAD") {
		t.Fatalf("initial HEAD error = %v", err)
	}
}

func TestOrderPrerequisiteChangeSetsHandlesLargeFanInWithoutArgvGrowth(t *testing.T) {
	fixture := newPrerequisiteGitFixture(t)
	const count = 60000
	handoffs := make([]model.PrerequisiteHandoff, 0, count)
	for index := count - 1; index >= 0; index-- {
		handoff := fixture.handoff
		handoff.PrerequisiteID = fmt.Sprintf("task-%05d", index)
		copy := *handoff.ChangeSet
		copy.ID = fmt.Sprintf("change-%05d", index)
		handoff.ChangeSet = &copy
		handoffs = append(handoffs, handoff)
	}
	ordered, err := orderPrerequisiteChangeSets(context.Background(), fixture.repository, handoffs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != count ||
		ordered[0].PrerequisiteID != "task-00000" ||
		ordered[len(ordered)-1].PrerequisiteID != "task-59999" {
		t.Fatalf("large fan-in ordering was not deterministic: first=%s last=%s count=%d",
			ordered[0].PrerequisiteID, ordered[len(ordered)-1].PrerequisiteID, len(ordered))
	}
}

func TestIntegrationConflictFingerprintUsesIndexAndUniqueTargetHeadsOnly(t *testing.T) {
	headA, headB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	handoff := func(prerequisiteID, changeSetID, head, ref string) model.PrerequisiteHandoff {
		return model.PrerequisiteHandoff{
			PrerequisiteID: prerequisiteID,
			ChangeSet: &model.ChangeSet{
				ID: changeSetID, HeadCommit: head, DurableRef: ref,
			},
		}
	}
	rawIndex := []byte("100644 aaaa 1\tREADME.md\x00100644 bbbb 2\tbad-\xff-name\x00")
	original := integrationConflictFingerprint(rawIndex, []model.PrerequisiteHandoff{
		handoff("old-parent-a", "old-change-a", headA, "refs/autogora/runs/old-a"),
		handoff("old-parent-b", "old-change-b", headB, "refs/autogora/runs/old-b"),
	})
	recreated := integrationConflictFingerprint(rawIndex, []model.PrerequisiteHandoff{
		handoff("new-parent-b", "new-change-b", strings.ToUpper(headB), "refs/autogora/runs/new-b"),
		handoff("new-parent-a", "new-change-a", headA, "refs/autogora/runs/new-a"),
		handoff("duplicate-head", "duplicate-change", headA, "refs/autogora/runs/duplicate"),
	})
	if original != recreated {
		t.Fatalf("ephemeral handoff identity reset fingerprint: %s != %s", original, recreated)
	}
	changedIndex := integrationConflictFingerprint(
		[]byte("100644 aaaa 1\tREADME.md\x00100644 cccc 2\tbad-\xff-name\x00"),
		[]model.PrerequisiteHandoff{handoff("a", "a", headA, "refs/autogora/runs/a"), handoff("b", "b", headB, "refs/autogora/runs/b")},
	)
	if changedIndex == original {
		t.Fatal("changed unmerged stage/blob identity did not reset fingerprint")
	}
	changedHead := integrationConflictFingerprint(rawIndex, []model.PrerequisiteHandoff{
		handoff("a", "a", headA, "refs/autogora/runs/a"),
		handoff("c", "c", strings.Repeat("c", 40), "refs/autogora/runs/c"),
	})
	if changedHead == original {
		t.Fatal("changed pending target head did not reset fingerprint")
	}
}

func TestIntegrationResolutionManifestKeepsLargeFanInOutOfTransport(t *testing.T) {
	fixture := newPrerequisiteGitFixture(t)
	worktree := filepath.Join(t.TempDir(), "finalizer")
	runWorkspaceGit(t, fixture.repository, "worktree", "add", "-q", "--detach", worktree, fixture.base)
	runID := "finalizer-run-1"
	repository := fixture.repository
	claim := &model.ClaimedTask{
		Task: model.TaskDetail{Task: model.Task{ID: "finalizer-task", WorkflowRole: model.WorkflowRoleFinalizer}},
		Run:  model.Run{ID: runID, TaskID: "finalizer-task"},
		Workspace: &model.RunWorkspace{
			RunID: runID, TaskID: "finalizer-task", Path: worktree,
			Kind: model.WorkspaceWorktree, RepositoryPath: &repository,
			BaseCommit: &fixture.base, Generated: true,
		},
	}
	const targetCount = 10000
	targets := make([]model.IntegrationResolutionTarget, 0, targetCount)
	for index := range targetCount {
		targets = append(targets, model.IntegrationResolutionTarget{
			PrerequisiteID:  fmt.Sprintf("parent-%05d", index),
			ChangeSetID:     fmt.Sprintf("change-%05d", index),
			HeadCommit:      fixture.head,
			DurableRef:      fmt.Sprintf("refs/autogora/runs/parent-%05d", index),
			MergeInProgress: index == 0,
		})
	}
	conflicts := make([]string, 0, 350)
	for index := range 350 {
		conflicts = append(conflicts, fmt.Sprintf("conflicts/%03d-%s.txt", index, strings.Repeat("x", 1100)))
	}
	resolution := &model.IntegrationResolution{
		ConflictFingerprint: strings.Repeat("d", 64),
		WorkspacePath:       worktree, Targets: targets, ConflictingFiles: conflicts,
	}
	if err := writeIntegrationResolutionManifest(context.Background(), claim, resolution); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(resolution.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v", info.Mode())
	}
	encoded, err := os.ReadFile(resolution.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > model.IntegrationResolutionManifestMaxBytes {
		t.Fatalf("manifest size = %d", len(encoded))
	}
	digest := sha256.Sum256(encoded)
	if resolution.ManifestSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("manifest digest = %s", resolution.ManifestSHA256)
	}
	var manifest model.IntegrationResolutionManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Targets) != targetCount || resolution.TargetCount != targetCount {
		t.Fatalf("manifest dropped targets: manifest=%d resolution=%d", len(manifest.Targets), resolution.TargetCount)
	}
	if len(manifest.ConflictingFiles) != 200 ||
		manifest.ConflictingFilesOmitted != 150 ||
		resolution.ConflictingFilesOmitted != 150 {
		t.Fatalf("conflict summary was not bounded: manifest=%d omitted=%d resolution=%d",
			len(manifest.ConflictingFiles), manifest.ConflictingFilesOmitted, resolution.ConflictingFilesOmitted)
	}
	transport, err := json.Marshal(resolution)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transport), "parent-09999") || strings.Contains(string(transport), "conflicts/") {
		t.Fatalf("host-only handoff leaked into transport: %d bytes", len(transport))
	}
}
