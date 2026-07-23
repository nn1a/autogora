package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

type prerequisiteGitFixture struct {
	repository string
	base       string
	head       string
	handoff    model.PrerequisiteHandoff
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
