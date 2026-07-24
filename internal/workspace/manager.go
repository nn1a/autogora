package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
)

const hostGitCommandLimit = 10 * time.Minute

type Manager struct {
	boards      *boards.Manager
	cwd         string
	allowWrites bool
}

func New(manager *boards.Manager) *Manager { return &Manager{boards: manager} }

func (m *Manager) SetWorkingDirectory(path string) { m.cwd = path }
func (m *Manager) SetAllowWrites(value bool)       { m.allowWrites = value }

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

func requireAbsolute(path, label string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("%s must be an absolute path: %s", label, path)
	}
	return filepath.Clean(expanded), nil
}

func isNonEmptyDirectory(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := workspaceCommand(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func workspaceCommand(ctx context.Context, name string, args ...string) *processguard.Command {
	return processguard.NewCommandContext(ctx, hostGitCommandLimit, name, args...)
}

func gitRoot(ctx context.Context, path string) (string, bool) {
	output, err := commandOutput(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	if err != nil || output == "" {
		return "", false
	}
	resolved, err := filepath.Abs(output)
	return resolved, err == nil
}

func gitCommit(ctx context.Context, repository, revision string) (string, error) {
	return commandOutput(ctx, "git", "-C", repository, "rev-parse", "--verify", revision+"^{commit}")
}

func canonicalPath(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if evaluated, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = evaluated
	}
	return filepath.Clean(resolved), nil
}

func validateWorktree(ctx context.Context, repository, target string) error {
	repositoryRoot, ok := gitRoot(ctx, repository)
	if !ok {
		return fmt.Errorf("workspace repository is no longer available: %s", repository)
	}
	targetRoot, ok := gitRoot(ctx, target)
	if !ok {
		return fmt.Errorf("prepared worktree is no longer a git worktree: %s", target)
	}
	repositoryCommon, err := commandOutput(ctx, "git", "-C", repositoryRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	targetCommon, err := commandOutput(ctx, "git", "-C", targetRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	repositoryCommon, err = canonicalPath(repositoryCommon)
	if err != nil {
		return err
	}
	targetCommon, err = canonicalPath(targetCommon)
	if err != nil {
		return err
	}
	if repositoryCommon != targetCommon {
		return fmt.Errorf("prepared worktree belongs to a different repository: %s", target)
	}
	return nil
}

func addWorktree(ctx context.Context, repository, target string, branch *string) (string, error) {
	if isNonEmptyDirectory(target) {
		return "", fmt.Errorf("refusing to adopt a non-empty untracked worktree target: %s", target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	start := "HEAD"
	if branch != nil && strings.TrimSpace(*branch) != "" {
		candidate := strings.TrimSpace(*branch)
		if _, err := gitCommit(ctx, repository, candidate); err == nil {
			start = candidate
		}
	}
	base, err := gitCommit(ctx, repository, start)
	if err != nil {
		return "", fmt.Errorf("resolve worktree base: %w", err)
	}
	args := []string{"-C", repository, "worktree", "add", "--detach", target, base}
	if _, err := commandOutput(ctx, "git", args...); err != nil {
		return "", fmt.Errorf("unable to create git worktree: %w", err)
	}
	return base, nil
}

func (m *Manager) workingDirectory() (string, error) {
	if m.cwd != "" {
		return filepath.Abs(m.cwd)
	}
	return os.Getwd()
}

func (m *Manager) acquireWriteLease(ctx context.Context, opened *store.Store, scope store.RunScope, workspace model.RunWorkspace) error {
	if !m.allowWrites || workspace.Kind != model.WorkspaceDir {
		return nil
	}
	path := workspace.Path
	if workspace.RepositoryPath != nil {
		path = *workspace.RepositoryPath
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return err
	}
	if _, err := opened.AcquireWorkspaceLease(ctx, scope, canonical); err != nil {
		return err
	}
	coordination, err := m.boards.OpenCoordinationStore(ctx)
	if err != nil {
		return err
	}
	defer coordination.Close()
	lease, acquired, err := coordination.AcquireGlobalWorkspaceLease(ctx, opened.Board(), scope.RunID, canonical)
	if err != nil {
		return err
	}
	if acquired {
		return nil
	}
	owner, err := m.boards.OpenStore(ctx, lease.Board)
	if err != nil {
		return &store.ResourceBusyError{Path: canonical, OwnerBoard: lease.Board, OwnerRunID: lease.RunID}
	}
	terminal, statusErr := owner.IsRunTerminal(ctx, lease.RunID)
	processMayStillRun := true
	if statusErr == nil && terminal {
		inspection, inspectErr := owner.GetRun(ctx, lease.RunID)
		var identity *string
		var identityErr error
		if inspectErr == nil {
			identity, identityErr = owner.GetRunProcessIdentity(ctx, lease.RunID)
		}
		if inspectErr != nil || identityErr != nil {
			statusErr = errors.Join(inspectErr, identityErr)
		} else {
			processMayStillRun = runcontrol.ProcessMayStillBeRunning(inspection.Run.PID, identity)
		}
	}
	closeErr := owner.Close()
	if statusErr != nil || closeErr != nil || !terminal || processMayStillRun {
		return &store.ResourceBusyError{Path: canonical, OwnerBoard: lease.Board, OwnerRunID: lease.RunID}
	}
	_, err = coordination.ReleaseGlobalWorkspaceLease(ctx, lease)
	if err != nil {
		return err
	}
	lease, acquired, err = coordination.AcquireGlobalWorkspaceLease(ctx, opened.Board(), scope.RunID, canonical)
	if err != nil {
		return err
	}
	if !acquired {
		return &store.ResourceBusyError{Path: canonical, OwnerBoard: lease.Board, OwnerRunID: lease.RunID}
	}
	return nil
}

func (m *Manager) Prepare(ctx context.Context, opened *store.Store, claim *model.ClaimedTask) (*model.ClaimedTask, error) {
	if claim == nil {
		return nil, errors.New("claim cannot be nil")
	}
	task := claim.Task.Task
	if existing, err := opened.GetRunWorkspace(ctx, claim.Run.ID); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Kind == model.WorkspaceWorktree {
			if existing.RepositoryPath == nil {
				return nil, errors.New("prepared worktree is missing its repository")
			}
			if err := validateWorktree(ctx, *existing.RepositoryPath, existing.Path); err != nil {
				return nil, err
			}
		} else if info, err := os.Stat(existing.Path); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("prepared workspace is no longer available: %s", existing.Path)
		}
		if err := m.acquireWriteLease(ctx, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, *existing); err != nil {
			return nil, err
		}
		claim.Workspace = existing
		return claim, nil
	}
	metadata, err := m.boards.Read(task.Board)
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := m.boards.WorkspaceRoot(task.Board)
	if err != nil {
		return nil, err
	}
	kind := model.WorkspaceScratch
	path := ""
	generated := false
	var repositoryPath, baseCommit *string
	if task.WorkspaceKind == model.WorkspaceWorktree || (task.Workspace != nil && (*task.Workspace == "worktree" || strings.HasPrefix(*task.Workspace, "worktree:"))) {
		kind = model.WorkspaceWorktree
		target := ""
		if task.Workspace != nil {
			target = strings.TrimPrefix(*task.Workspace, "worktree:")
		}
		if target != "worktree" && target != "" {
			var explicitRoot string
			explicitRoot, err = requireAbsolute(target, "worktree root")
			if err == nil {
				// An explicit location is a stable root, not a one-shot checkout.
				// Every attempt receives its own child so preserved work from a
				// failed run cannot make all later retries impossible.
				path = filepath.Join(explicitRoot, claim.Run.ID)
				generated = true
			}
		} else {
			path = filepath.Join(workspaceRoot, task.ID, claim.Run.ID)
			generated = true
		}
		if err != nil {
			return nil, err
		}
		source, err := m.workingDirectory()
		if err != nil {
			return nil, err
		}
		if metadata.DefaultWorkdir != nil {
			source, err = requireAbsolute(*metadata.DefaultWorkdir, "board default workdir")
			if err != nil {
				return nil, err
			}
		}
		repository, ok := gitRoot(ctx, source)
		if !ok {
			return nil, fmt.Errorf("worktree workspace requires a git repository: %s", source)
		}
		base, err := addWorktree(ctx, repository, path, task.Branch)
		if err != nil {
			return nil, err
		}
		repositoryPath, baseCommit = &repository, &base
	} else if task.Workspace != nil && *task.Workspace != "" && *task.Workspace != "scratch" {
		requested := *task.Workspace
		kind = model.WorkspaceDir
		requested = strings.TrimPrefix(requested, "dir:")
		path, err = requireAbsolute(requested, "dir workspace")
		if err != nil {
			return nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("dir workspace does not exist: %s", path)
		}
		if repository, ok := gitRoot(ctx, path); ok {
			base, baseErr := gitCommit(ctx, repository, "HEAD")
			if baseErr == nil {
				repositoryPath, baseCommit = &repository, &base
			}
		}
	} else {
		defaultGitRoot := ""
		if metadata.DefaultWorkdir != nil {
			expanded, expandErr := expandHome(*metadata.DefaultWorkdir)
			if expandErr == nil {
				defaultGitRoot, _ = gitRoot(ctx, expanded)
			}
		}
		if task.WorkspaceKind == model.WorkspaceWorktree || defaultGitRoot != "" {
			kind = model.WorkspaceWorktree
			source, err := m.workingDirectory()
			if err != nil {
				return nil, err
			}
			if metadata.DefaultWorkdir != nil {
				source, err = requireAbsolute(*metadata.DefaultWorkdir, "board default workdir")
				if err != nil {
					return nil, err
				}
			}
			repository, ok := gitRoot(ctx, source)
			if !ok {
				return nil, fmt.Errorf("worktree workspace requires a git repository: %s", source)
			}
			path = filepath.Join(workspaceRoot, task.ID, claim.Run.ID)
			generated = true
			base, err := addWorktree(ctx, repository, path, task.Branch)
			if err != nil {
				return nil, err
			}
			repositoryPath, baseCommit = &repository, &base
		} else if metadata.DefaultWorkdir != nil {
			kind = model.WorkspaceDir
			path, err = requireAbsolute(*metadata.DefaultWorkdir, "board default workdir")
			if err != nil {
				return nil, err
			}
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("board default workdir does not exist: %s", path)
			}
			if repository, ok := gitRoot(ctx, path); ok {
				base, baseErr := gitCommit(ctx, repository, "HEAD")
				if baseErr == nil {
					repositoryPath, baseCommit = &repository, &base
				}
			}
		} else {
			kind = model.WorkspaceScratch
			path = filepath.Join(workspaceRoot, task.ID, claim.Run.ID)
			generated = true
			if err := os.MkdirAll(path, 0o755); err != nil {
				return nil, err
			}
		}
	}
	bound, err := opened.BindRunWorkspace(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.BindRunWorkspaceInput{
		Path: path, Kind: kind, RepositoryPath: repositoryPath, BaseCommit: baseCommit, Generated: generated,
	})
	if err != nil {
		return nil, err
	}
	if err := m.acquireWriteLease(ctx, opened, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, bound); err != nil {
		return nil, err
	}
	claim.Workspace = &bound
	return claim, nil
}

func (m *Manager) Cleanup(board string, workspace model.RunWorkspace) (bool, error) {
	if workspace.Kind != model.WorkspaceScratch || !workspace.Generated {
		return false, nil
	}
	root, err := m.boards.WorkspaceRoot(board)
	if err != nil {
		return false, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false, err
	}
	target, err := filepath.Abs(workspace.Path)
	if err != nil {
		return false, err
	}
	expected := filepath.Join(root, workspace.TaskID, workspace.RunID)
	if target != expected || !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return false, fmt.Errorf("refusing to clean an untrusted scratch path: %s", target)
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.RemoveAll(target); err != nil {
		return false, err
	}
	return true, nil
}
