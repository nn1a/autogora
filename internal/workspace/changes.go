package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type ChangeSnapshot struct {
	RepositoryPath string
	WorktreePath   string
	BaseCommit     string
	HeadCommit     string
	DurableRef     string
	State          string
	ChangedFiles   []string
}

// ChangeInspection describes workspace state relative to the commit recorded
// when the run workspace was prepared. HeadCommit is populated whenever a
// worktree HEAD can be resolved, including when another part of the inspection
// fails, so recovery messages can identify the exact work that needs review.
type ChangeInspection struct {
	Changed    bool
	HeadCommit string
}

func (m *Manager) inspectChangesFromCommit(
	ctx context.Context,
	workspace model.RunWorkspace,
	startCommit string,
	missingStartError string,
) (ChangeInspection, error) {
	if workspace.Kind == model.WorkspaceScratch {
		entries, err := os.ReadDir(workspace.Path)
		if err != nil {
			return ChangeInspection{}, err
		}
		return ChangeInspection{Changed: len(entries) > 0}, nil
	}
	if workspace.Kind != model.WorkspaceWorktree {
		return ChangeInspection{}, nil
	}
	if workspace.RepositoryPath == nil || strings.TrimSpace(*workspace.RepositoryPath) == "" {
		return ChangeInspection{}, errors.New("prepared worktree is missing its repository")
	}
	if err := validateWorktree(ctx, *workspace.RepositoryPath, workspace.Path); err != nil {
		return ChangeInspection{}, err
	}
	output, err := gitOutputWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return ChangeInspection{}, err
	}
	inspection := ChangeInspection{Changed: len(output) > 0}
	head, err := gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return inspection, err
	}
	inspection.HeadCommit = head
	if strings.TrimSpace(startCommit) == "" {
		return inspection, errors.New(missingStartError)
	}
	start, err := gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", strings.TrimSpace(startCommit)+"^{commit}")
	if err != nil {
		return inspection, err
	}
	inspection.Changed = inspection.Changed || !strings.EqualFold(head, start)
	return inspection, nil
}

// InspectChanges reports whether the worktree differs from its persisted
// output base. Completion capture deliberately uses this same base so the
// final change set includes prerequisite integration and all recovered work.
func (m *Manager) InspectChanges(ctx context.Context, workspace model.RunWorkspace) (ChangeInspection, error) {
	start := ""
	if workspace.BaseCommit != nil {
		start = *workspace.BaseCommit
	}
	return m.inspectChangesFromCommit(ctx, workspace, start, "prepared worktree is missing its base commit")
}

// InspectChangesSince reports changes made after a supplied run-start commit.
// This is intentionally separate from InspectChanges: a recovery attempt can
// start at an adopted checkpoint while its final output base remains the
// prerequisite-integrated commit used by CaptureChangeSet.
func (m *Manager) InspectChangesSince(
	ctx context.Context,
	workspace model.RunWorkspace,
	startCommit string,
) (ChangeInspection, error) {
	return m.inspectChangesFromCommit(ctx, workspace, startCommit, "run start commit is empty")
}

func (m *Manager) HasChanges(ctx context.Context, workspace model.RunWorkspace) (bool, error) {
	inspection, err := m.InspectChanges(ctx, workspace)
	return inspection.Changed, err
}

func (m *Manager) HasChangesSince(
	ctx context.Context,
	workspace model.RunWorkspace,
	startCommit string,
) (bool, error) {
	inspection, err := m.InspectChangesSince(ctx, workspace, startCommit)
	return inspection.Changed, err
}

func gitOutputWithEnv(ctx context.Context, directory string, environment map[string]string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = make([]string, 0, len(os.Environ())+len(environment))
	for _, item := range os.Environ() {
		key := item
		if split := strings.IndexByte(item, '='); split >= 0 {
			key = item[:split]
		}
		if _, overridden := environment[key]; !overridden {
			command.Env = append(command.Env, item)
		}
	}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func gitTextWithEnv(ctx context.Context, directory string, environment map[string]string, args ...string) (string, error) {
	output, err := gitOutputWithEnv(ctx, directory, environment, args...)
	return strings.TrimSpace(string(output)), err
}

func durableRunRef(runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.ContainsAny(runID, " ~^:?*[\\") || strings.Contains(runID, "..") {
		return "", errors.New("invalid run id for durable git ref")
	}
	return "refs/autogora/runs/" + runID, nil
}

func boundedCommitSubject(taskID, title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if len(title) > 120 {
		title = title[:120]
	}
	if title == "" {
		title = "task changes"
	}
	return fmt.Sprintf("autogora(%s): %s", taskID, title)
}

func splitNullTerminated(value []byte) []string {
	result := make([]string, 0)
	for _, item := range bytes.Split(value, []byte{0}) {
		if len(item) != 0 {
			result = append(result, string(item))
		}
	}
	return result
}

func (m *Manager) CaptureChangeSet(ctx context.Context, workspace model.RunWorkspace, taskID, title string) (ChangeSnapshot, error) {
	if workspace.Kind != model.WorkspaceWorktree || workspace.RepositoryPath == nil || workspace.BaseCommit == nil {
		return ChangeSnapshot{}, errors.New("change capture requires a prepared git worktree")
	}
	if err := validateWorktree(ctx, *workspace.RepositoryPath, workspace.Path); err != nil {
		return ChangeSnapshot{}, err
	}
	unmerged, err := gitTextWithEnv(ctx, workspace.Path, nil, "ls-files", "-u")
	if err != nil {
		return ChangeSnapshot{}, err
	}
	if unmerged != "" {
		return ChangeSnapshot{}, errors.New("worktree contains unresolved merge conflicts")
	}
	temporary, err := os.CreateTemp("", "autogora-index-*")
	if err != nil {
		return ChangeSnapshot{}, err
	}
	indexPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return ChangeSnapshot{}, err
	}
	if err := os.Remove(indexPath); err != nil {
		return ChangeSnapshot{}, err
	}
	defer os.Remove(indexPath)
	defer os.Remove(indexPath + ".lock")
	env := map[string]string{
		"GIT_INDEX_FILE": indexPath, "GIT_TERMINAL_PROMPT": "0",
		"GIT_AUTHOR_NAME": "Autogora", "GIT_AUTHOR_EMAIL": "autogora@localhost",
		"GIT_COMMITTER_NAME": "Autogora", "GIT_COMMITTER_EMAIL": "autogora@localhost",
	}
	currentHead, err := gitTextWithEnv(ctx, workspace.Path, env, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return ChangeSnapshot{}, err
	}
	if _, err := gitOutputWithEnv(ctx, workspace.Path, env, "read-tree", currentHead); err != nil {
		return ChangeSnapshot{}, err
	}
	if _, err := gitOutputWithEnv(ctx, workspace.Path, env, "add", "-A", "--", "."); err != nil {
		return ChangeSnapshot{}, err
	}
	tree, err := gitTextWithEnv(ctx, workspace.Path, env, "write-tree")
	if err != nil {
		return ChangeSnapshot{}, err
	}
	currentTree, err := gitTextWithEnv(ctx, workspace.Path, env, "rev-parse", currentHead+"^{tree}")
	if err != nil {
		return ChangeSnapshot{}, err
	}
	head := currentHead
	if tree != currentTree {
		head, err = gitTextWithEnv(ctx, workspace.Path, env, "-c", "user.name=Autogora", "-c", "user.email=autogora@localhost",
			"commit-tree", tree, "-p", currentHead, "-m", boundedCommitSubject(taskID, title))
		if err != nil {
			return ChangeSnapshot{}, err
		}
	}
	if _, err := gitOutputWithEnv(ctx, workspace.Path, env, "merge-base", "--is-ancestor", *workspace.BaseCommit, head); err != nil {
		return ChangeSnapshot{}, errors.New("worker history no longer contains the prepared base commit")
	}
	ref, err := durableRunRef(workspace.RunID)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	existing, existingErr := gitTextWithEnv(ctx, *workspace.RepositoryPath, env, "show-ref", "--verify", "--hash", ref)
	if existingErr == nil {
		if existing != head {
			return ChangeSnapshot{}, fmt.Errorf("durable ref %s already points to a different commit", ref)
		}
	} else if _, err := gitOutputWithEnv(ctx, *workspace.RepositoryPath, env, "update-ref", ref, head, ""); err != nil {
		return ChangeSnapshot{}, err
	}
	changedOutput, err := gitOutputWithEnv(ctx, workspace.Path, env, "diff", "--name-only", "-z", *workspace.BaseCommit, head)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	changedFiles := splitNullTerminated(changedOutput)
	state := "ready"
	if len(changedFiles) == 0 {
		state = "no_change"
	}
	return ChangeSnapshot{
		RepositoryPath: *workspace.RepositoryPath, WorktreePath: workspace.Path,
		BaseCommit: *workspace.BaseCommit, HeadCommit: head, DurableRef: ref,
		State: state, ChangedFiles: changedFiles,
	}, nil
}
