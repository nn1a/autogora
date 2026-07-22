package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/nn1a/kanban/internal/boards"
	"github.com/nn1a/kanban/internal/model"
)

type Options struct {
	EventRetentionDays     int
	LogRetentionDays       int
	WorkspaceRetentionDays int
}

type Result struct {
	Board             string   `json:"board"`
	EventsDeleted     int64    `json:"eventsDeleted"`
	LogsDeleted       []string `json:"logsDeleted"`
	WorkspacesDeleted []string `json:"workspacesDeleted"`
}

func cutoff(days int) time.Time {
	return time.Now().Add(-time.Duration(max(0, days)) * 24 * time.Hour)
}

func Collect(ctx context.Context, manager *boards.Manager, board string, options Options) (Result, error) {
	opened, err := manager.OpenStore(ctx, board)
	if err != nil {
		return Result{}, err
	}
	defer opened.Close()
	result := Result{Board: board, LogsDeleted: []string{}, WorkspacesDeleted: []string{}}
	result.EventsDeleted, err = opened.GarbageCollectEvents(ctx, options.EventRetentionDays)
	if err != nil {
		return Result{}, err
	}
	logsRoot, err := manager.LogsRoot(board)
	if err != nil {
		return Result{}, err
	}
	entries, err := os.ReadDir(logsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	logCutoff := cutoff(options.LogRetentionDays)
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			path := filepath.Join(logsRoot, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return Result{}, err
			}
			if info.ModTime().Before(logCutoff) {
				if err := os.Remove(path); err != nil {
					return Result{}, err
				}
				result.LogsDeleted = append(result.LogsDeleted, path)
			}
		}
	}
	workspaceRoot, err := manager.WorkspaceRoot(board)
	if err != nil {
		return Result{}, err
	}
	entries, err = os.ReadDir(workspaceRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	workspaceCutoff := cutoff(options.WorkspaceRetentionDays)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return Result{}, err
		}
		if !info.ModTime().Before(workspaceCutoff) {
			continue
		}
		detail, err := opened.GetTask(ctx, entry.Name())
		if err != nil {
			continue
		}
		if detail.Task.WorkspaceKind != model.WorkspaceScratch ||
			(detail.Task.Status != model.TaskStatusDone && detail.Task.Status != model.TaskStatusArchived) {
			continue
		}
		path := filepath.Join(workspaceRoot, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return Result{}, err
		}
		result.WorkspacesDeleted = append(result.WorkspacesDeleted, path)
	}
	return result, nil
}
