package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type Manager struct {
	boards *boards.Manager
	cwd    string
}

func New(manager *boards.Manager) *Manager { return &Manager{boards: manager} }

func (m *Manager) SetWorkingDirectory(path string) { m.cwd = path }

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
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func gitRoot(ctx context.Context, path string) (string, bool) {
	output, err := commandOutput(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	if err != nil || output == "" {
		return "", false
	}
	resolved, err := filepath.Abs(output)
	return resolved, err == nil
}

func addWorktree(ctx context.Context, repository, target string, branch *string) error {
	if isNonEmptyDirectory(target) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	args := []string{"-C", repository, "worktree", "add"}
	if branch != nil && strings.TrimSpace(*branch) != "" {
		name := strings.TrimSpace(*branch)
		if _, err := commandOutput(ctx, "git", "-C", repository, "show-ref", "--verify", "refs/heads/"+name); err == nil {
			args = append(args, target, name)
		} else {
			args = append(args, "-b", name, target, "HEAD")
		}
	} else {
		args = append(args, "--detach", target, "HEAD")
	}
	if _, err := commandOutput(ctx, "git", args...); err != nil {
		return fmt.Errorf("unable to create git worktree: %w", err)
	}
	return nil
}

func (m *Manager) workingDirectory() (string, error) {
	if m.cwd != "" {
		return filepath.Abs(m.cwd)
	}
	return os.Getwd()
}

func (m *Manager) Prepare(ctx context.Context, opened *store.Store, claim *model.ClaimedTask) (*model.ClaimedTask, error) {
	if claim == nil {
		return nil, errors.New("claim cannot be nil")
	}
	task := claim.Task.Task
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
	if task.Workspace != nil && *task.Workspace != "" && *task.Workspace != "scratch" {
		requested := *task.Workspace
		if requested == "worktree" || strings.HasPrefix(requested, "worktree:") {
			kind = model.WorkspaceWorktree
			target := strings.TrimPrefix(requested, "worktree:")
			if target != "worktree" && target != "" {
				path, err = requireAbsolute(target, "worktree target")
			} else {
				path = filepath.Join(workspaceRoot, task.ID)
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
			if err := addWorktree(ctx, repository, path, task.Branch); err != nil {
				return nil, err
			}
		} else {
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
			path = filepath.Join(workspaceRoot, task.ID)
			if err := addWorktree(ctx, repository, path, task.Branch); err != nil {
				return nil, err
			}
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
		} else {
			kind = model.WorkspaceScratch
			path = filepath.Join(workspaceRoot, task.ID)
			if err := os.MkdirAll(path, 0o755); err != nil {
				return nil, err
			}
		}
	}
	detail, err := opened.BindRunWorkspace(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, path, kind)
	if err != nil {
		return nil, err
	}
	claim.Task = detail
	return claim, nil
}

func (m *Manager) Cleanup(task model.Task) (bool, error) {
	if task.WorkspaceKind != model.WorkspaceScratch || task.Workspace == nil {
		return false, nil
	}
	root, err := m.boards.WorkspaceRoot(task.Board)
	if err != nil {
		return false, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false, err
	}
	target, err := filepath.Abs(*task.Workspace)
	if err != nil {
		return false, err
	}
	expected := filepath.Join(root, task.ID)
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
