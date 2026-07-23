package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func runWorkspaceGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Autogora", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Autogora", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestHasChangesDetectsCleanCommittedWorkRelativeToBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktree := filepath.Join(root, "worktree")
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
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", worktree, base)

	workspace := model.RunWorkspace{
		Kind: model.WorkspaceWorktree, Path: worktree,
		RepositoryPath: &repository, BaseCommit: &base,
	}
	manager := &Manager{}
	if changed, err := manager.HasChanges(ctx, workspace); err != nil || changed {
		t.Fatalf("fresh worktree changed=%v err=%v", changed, err)
	}

	if err := os.WriteFile(filepath.Join(worktree, "committed.txt"), []byte("committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, worktree, "add", "committed.txt")
	runWorkspaceGit(t, worktree, "commit", "-q", "-m", "worker commit")
	if status := runWorkspaceGit(t, worktree, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture worktree is not clean: %q", status)
	}
	if changed, err := manager.HasChanges(ctx, workspace); err != nil || !changed {
		t.Fatalf("clean committed work changed=%v err=%v", changed, err)
	}

	effectiveBase := runWorkspaceGit(t, worktree, "rev-parse", "HEAD^{commit}")
	workspace.BaseCommit = &effectiveBase
	if changed, err := manager.HasChanges(ctx, workspace); err != nil || changed {
		t.Fatalf("effective base was not honored: changed=%v err=%v", changed, err)
	}
}
