package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

// CompleteRun preserves the dispatcher's process-exit boundary for managed
// runs. An independently claimed worktree has no supervising process, so this
// method captures its final change set before completing it synchronously.
func (m *Manager) CompleteRun(ctx context.Context, opened *store.Store, scope store.RunScope, completion store.CompletionInput) (model.TaskDetail, error) {
	managed, err := opened.IsRunManaged(ctx, scope.RunID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if managed {
		return opened.CompleteRun(ctx, scope, completion)
	}
	prepared, err := opened.GetRunWorkspace(ctx, scope.RunID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if prepared == nil || prepared.Kind != model.WorkspaceWorktree {
		return opened.CompleteRun(ctx, scope, completion)
	}
	detail, err := opened.GetTask(ctx, prepared.TaskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	if _, err := opened.RequestRunCompletion(ctx, scope, completion); err != nil {
		return model.TaskDetail{}, err
	}
	snapshot, captureErr := m.CaptureChangeSet(ctx, *prepared, detail.Task.ID, detail.Task.Title)
	if captureErr == nil {
		captureErr = m.VerifyPrerequisiteChangeSets(ctx, opened, detail.Task.ID, *prepared, snapshot.HeadCommit)
	}
	if captureErr == nil {
		_, captureErr = opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
			RunID: scope.RunID, RepositoryPath: snapshot.RepositoryPath, WorktreePath: snapshot.WorktreePath,
			BaseCommit: snapshot.BaseCommit, HeadCommit: snapshot.HeadCommit, DurableRef: snapshot.DurableRef,
			State: snapshot.State, ChangedFiles: snapshot.ChangedFiles,
		})
	}
	if captureErr == nil {
		completed, finalizeErr := opened.FinalizeRunTerminal(ctx, scope, 0)
		if finalizeErr == nil {
			return completed, nil
		}
		captureErr = fmt.Errorf("finalize captured completion: %w", finalizeErr)
	}
	return preserveDirectCompletion(opened, scope, prepared.Path, captureErr)
}

func preserveDirectCompletion(opened *store.Store, scope store.RunScope, workspacePath string, cause error) (model.TaskDetail, error) {
	durable, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reason := "Direct completion could not be captured; review the preserved workspace at " + workspacePath + ": " + cause.Error()
	discardErr := opened.DiscardRunTerminalRequest(durable, scope, "direct completion capture failed")
	if discardErr != nil {
		return model.TaskDetail{}, errors.Join(cause, discardErr)
	}
	blocked, blockErr := opened.BlockRun(durable, scope, store.BlockInput{Reason: reason, Kind: model.BlockKindNeedsInput})
	return blocked, errors.Join(cause, blockErr)
}
