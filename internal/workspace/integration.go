package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const (
	IntegrationFailureConflict             = "conflict"
	IntegrationFailureDirtyWorkspace       = "dirty_workspace"
	IntegrationFailureForeignRepository    = "foreign_repository"
	IntegrationFailureInvalidReference     = "invalid_reference"
	IntegrationFailureHistoryRewrite       = "history_rewrite"
	IntegrationFailureMerge                = "merge_failed"
	IntegrationFailureUnsupportedWorkspace = "unsupported_workspace"
)

// PrerequisiteIntegrationError carries the task block policy and actionable
// details for an integration failure. Callers can use errors.As and pass
// Reason and BlockKind directly to Store.BlockRun.
type PrerequisiteIntegrationError struct {
	Code             string
	BlockKind        model.BlockKind
	Reason           string
	WorkspacePath    string
	PrerequisiteID   string
	ChangeSetID      string
	DurableRef       string
	ConflictingFiles []string
	Cause            error
}

func (e *PrerequisiteIntegrationError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

func (e *PrerequisiteIntegrationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type IntegratedPrerequisite struct {
	PrerequisiteID string
	ChangeSetID    string
	HeadCommit     string
}

type PrerequisiteIntegrationResult struct {
	Applied             []IntegratedPrerequisite
	AlreadyPresent      []IntegratedPrerequisite
	EffectiveBaseCommit string
}

func integrationItem(handoff model.PrerequisiteHandoff) IntegratedPrerequisite {
	return IntegratedPrerequisite{
		PrerequisiteID: handoff.PrerequisiteID,
		ChangeSetID:    handoff.ChangeSet.ID,
		HeadCommit:     handoff.ChangeSet.HeadCommit,
	}
}

func integrationError(code string, kind model.BlockKind, workspace model.RunWorkspace, handoff *model.PrerequisiteHandoff, reason string, cause error) *PrerequisiteIntegrationError {
	value := &PrerequisiteIntegrationError{Code: code, BlockKind: kind, Reason: reason, WorkspacePath: workspace.Path, Cause: cause}
	if handoff != nil {
		value.PrerequisiteID = handoff.PrerequisiteID
		if handoff.ChangeSet != nil {
			value.ChangeSetID = handoff.ChangeSet.ID
			value.DurableRef = handoff.ChangeSet.DurableRef
		}
	}
	return value
}

func prerequisiteChangeSets(handoffs []model.PrerequisiteHandoff) []model.PrerequisiteHandoff {
	result := make([]model.PrerequisiteHandoff, 0, len(handoffs))
	for _, handoff := range handoffs {
		if handoff.ChangeSet != nil {
			result = append(result, handoff)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PrerequisiteID != result[j].PrerequisiteID {
			return result[i].PrerequisiteID < result[j].PrerequisiteID
		}
		return result[i].ChangeSet.ID < result[j].ChangeSet.ID
	})
	return result
}

func gitCommonDirectory(ctx context.Context, path string) (string, error) {
	common, err := gitTextWithEnv(ctx, path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return canonicalPath(common)
}

func gitIsAncestor(ctx context.Context, directory, ancestor, descendant string) (bool, error) {
	_, err := gitOutputWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func validObjectID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func integrationGitEnvironment() map[string]string {
	return map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "GIT_MERGE_AUTOEDIT": "no", "GIT_EDITOR": "true",
		"GIT_AUTHOR_NAME": "Autogora", "GIT_AUTHOR_EMAIL": "autogora@localhost",
		"GIT_COMMITTER_NAME": "Autogora", "GIT_COMMITTER_EMAIL": "autogora@localhost",
	}
}

func rollbackIntegration(ctx context.Context, directory, initialHead string) error {
	if _, err := gitOutputWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "-q", "MERGE_HEAD"); err == nil {
		if _, err := gitOutputWithEnv(ctx, directory, integrationGitEnvironment(), "merge", "--abort"); err != nil {
			return err
		}
	}
	_, err := gitOutputWithEnv(ctx, directory, integrationGitEnvironment(), "reset", "--hard", initialHead)
	return err
}

func addRollbackFailure(integrationErr *PrerequisiteIntegrationError, rollbackErr error) *PrerequisiteIntegrationError {
	if rollbackErr == nil {
		return integrationErr
	}
	integrationErr.Reason += "; Autogora could not restore the prepared worktree: " + rollbackErr.Error()
	integrationErr.Cause = errors.Join(integrationErr.Cause, rollbackErr)
	return integrationErr
}

func verifyChangeSetReference(ctx context.Context, childCommon string, workspace model.RunWorkspace, handoff model.PrerequisiteHandoff) *PrerequisiteIntegrationError {
	changeSet := handoff.ChangeSet
	if changeSet == nil {
		return nil
	}
	expectedRef, refErr := durableRunRef(changeSet.RunID)
	validProvenance := refErr == nil && handoff.SatisfiedRunID != nil && handoff.Run != nil &&
		changeSet.RunID == *handoff.SatisfiedRunID && changeSet.RunID == handoff.Run.ID &&
		changeSet.TaskID == handoff.PrerequisiteID && handoff.Run.TaskID == handoff.PrerequisiteID &&
		handoff.Run.Status == model.RunStatusCompleted &&
		changeSet.DurableRef == expectedRef && (changeSet.State == "ready" || changeSet.State == "no_change")
	if !validProvenance {
		return integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("prerequisite change set %s from task %s has inconsistent run provenance", changeSet.ID, handoff.PrerequisiteID), refErr)
	}
	parentCommon, err := gitCommonDirectory(ctx, changeSet.RepositoryPath)
	if err != nil {
		return integrationError(IntegrationFailureForeignRepository, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("prerequisite change set %s from task %s has an unavailable Git repository", changeSet.ID, handoff.PrerequisiteID), err)
	}
	if parentCommon != childCommon {
		return integrationError(IntegrationFailureForeignRepository, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("prerequisite change set %s from task %s belongs to a different Git repository", changeSet.ID, handoff.PrerequisiteID), nil)
	}
	if !validObjectID(changeSet.HeadCommit) {
		return integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("prerequisite change set %s from task %s has invalid Git provenance", changeSet.ID, handoff.PrerequisiteID), nil)
	}
	if _, err := gitOutputWithEnv(ctx, changeSet.RepositoryPath, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"check-ref-format", changeSet.DurableRef); err != nil {
		return integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("prerequisite change set %s from task %s has an invalid durable ref %s", changeSet.ID, handoff.PrerequisiteID, changeSet.DurableRef), err)
	}
	refHead, err := gitTextWithEnv(ctx, changeSet.RepositoryPath, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", changeSet.DurableRef+"^{commit}")
	if err != nil {
		return integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("durable ref %s for prerequisite change set %s is missing", changeSet.DurableRef, changeSet.ID), err)
	}
	if !strings.EqualFold(refHead, strings.TrimSpace(changeSet.HeadCommit)) {
		return integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
			fmt.Sprintf("durable ref %s for prerequisite change set %s resolves to %s, expected %s", changeSet.DurableRef, changeSet.ID, refHead, changeSet.HeadCommit), nil)
	}
	return nil
}

func (m *Manager) integratePrerequisiteHandoffs(ctx context.Context, workspace model.RunWorkspace, handoffs []model.PrerequisiteHandoff) (PrerequisiteIntegrationResult, error) {
	result := PrerequisiteIntegrationResult{Applied: []IntegratedPrerequisite{}, AlreadyPresent: []IntegratedPrerequisite{}}
	changeSets := prerequisiteChangeSets(handoffs)
	if len(changeSets) == 0 {
		return result, nil
	}
	if workspace.Kind != model.WorkspaceWorktree && workspace.Kind != model.WorkspaceDir {
		reason := fmt.Sprintf("%s workspace cannot apply %d prerequisite change set(s); use an isolated worktree", workspace.Kind, len(changeSets))
		return result, integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, nil, reason, nil)
	}
	if workspace.RepositoryPath == nil || workspace.BaseCommit == nil {
		return result, integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, nil,
			"prepared worktree is missing its repository or effective base commit", nil)
	}
	if err := validateWorktree(ctx, *workspace.RepositoryPath, workspace.Path); err != nil {
		return result, integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, nil,
			"prepared worktree is no longer available", err)
	}
	initialHead, err := gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return result, err
	}
	base, err := gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", strings.TrimSpace(*workspace.BaseCommit)+"^{commit}")
	if err != nil {
		return result, err
	}
	if initialHead != base {
		return result, integrationError(IntegrationFailureDirtyWorkspace, model.BlockKindNeedsInput, workspace, nil,
			fmt.Sprintf("prepared workspace HEAD %s differs from its effective base %s", initialHead, base), nil)
	}
	childCommon, err := gitCommonDirectory(ctx, workspace.Path)
	if err != nil {
		return result, err
	}
	if workspace.Kind == model.WorkspaceDir {
		for _, handoff := range changeSets {
			if integrationErr := verifyChangeSetReference(ctx, childCommon, workspace, handoff); integrationErr != nil {
				return result, integrationErr
			}
			item := integrationItem(handoff)
			ancestor, err := gitIsAncestor(ctx, workspace.Path, item.HeadCommit, initialHead)
			if err != nil {
				return result, integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
					fmt.Sprintf("cannot inspect prerequisite change set %s from task %s", item.ChangeSetID, item.PrerequisiteID), err)
			}
			if !ancestor {
				return result, integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, &handoff,
					fmt.Sprintf("shared directory does not contain prerequisite change set %s from task %s; update it or use an isolated worktree", item.ChangeSetID, item.PrerequisiteID), nil)
			}
			result.AlreadyPresent = append(result.AlreadyPresent, item)
		}
		result.EffectiveBaseCommit = initialHead
		return result, nil
	}
	status, err := gitOutputWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return result, err
	}
	if len(status) > 0 {
		return result, integrationError(IntegrationFailureDirtyWorkspace, model.BlockKindNeedsInput, workspace, nil,
			"prepared worktree contains changes before prerequisite integration", nil)
	}
	for _, handoff := range changeSets {
		if integrationErr := verifyChangeSetReference(ctx, childCommon, workspace, handoff); integrationErr != nil {
			return result, addRollbackFailure(integrationErr, rollbackIntegration(ctx, workspace.Path, initialHead))
		}
		item := integrationItem(handoff)
		ancestor, err := gitIsAncestor(ctx, workspace.Path, item.HeadCommit, "HEAD")
		if err != nil {
			integrationErr := integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
				fmt.Sprintf("cannot inspect prerequisite change set %s from task %s", item.ChangeSetID, item.PrerequisiteID), err)
			return result, addRollbackFailure(integrationErr, rollbackIntegration(ctx, workspace.Path, initialHead))
		}
		if ancestor {
			result.AlreadyPresent = append(result.AlreadyPresent, item)
			continue
		}
		message := fmt.Sprintf("autogora: integrate prerequisite %s (%s)", item.PrerequisiteID, item.ChangeSetID)
		_, mergeErr := gitOutputWithEnv(ctx, workspace.Path, integrationGitEnvironment(),
			"-c", "commit.gpgSign=false", "merge", "--no-ff", "--no-edit", "--no-stat", "-m", message, item.HeadCommit)
		if mergeErr != nil {
			conflictOutput, _ := gitOutputWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
				"diff", "--name-only", "--diff-filter=U", "-z")
			conflicts := splitNullTerminated(conflictOutput)
			sort.Strings(conflicts)
			code, kind := IntegrationFailureMerge, model.BlockKindCapability
			reason := fmt.Sprintf("failed to integrate prerequisite change set %s from task %s", item.ChangeSetID, item.PrerequisiteID)
			if len(conflicts) > 0 {
				code, kind = IntegrationFailureConflict, model.BlockKindNeedsInput
				reason = fmt.Sprintf("prerequisite change set %s from task %s conflicts in %s", item.ChangeSetID, item.PrerequisiteID, strings.Join(conflicts, ", "))
			}
			integrationErr := integrationError(code, kind, workspace, &handoff, reason, mergeErr)
			integrationErr.ConflictingFiles = conflicts
			return result, addRollbackFailure(integrationErr, rollbackIntegration(ctx, workspace.Path, initialHead))
		}
		result.Applied = append(result.Applied, item)
	}
	result.EffectiveBaseCommit, err = gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		_ = rollbackIntegration(ctx, workspace.Path, initialHead)
		return PrerequisiteIntegrationResult{}, err
	}
	return result, nil
}

// IntegratePrerequisiteChangeSets applies every pinned prerequisite change set
// before a worker starts and advances the run's effective base only after the
// complete fan-in succeeds.
func (m *Manager) IntegratePrerequisiteChangeSets(ctx context.Context, opened *store.Store, claim *model.ClaimedTask) (PrerequisiteIntegrationResult, error) {
	if opened == nil {
		return PrerequisiteIntegrationResult{}, errors.New("store cannot be nil")
	}
	if claim == nil {
		return PrerequisiteIntegrationResult{}, errors.New("claim cannot be nil")
	}
	handoffs, err := opened.ListPrerequisiteHandoffs(ctx, claim.Task.Task.ID)
	if err != nil {
		return PrerequisiteIntegrationResult{}, err
	}
	if claim.Workspace == nil {
		if len(prerequisiteChangeSets(handoffs)) == 0 {
			return PrerequisiteIntegrationResult{Applied: []IntegratedPrerequisite{}, AlreadyPresent: []IntegratedPrerequisite{}}, nil
		}
		workspace := model.RunWorkspace{RunID: claim.Run.ID, TaskID: claim.Task.Task.ID}
		return PrerequisiteIntegrationResult{}, integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, nil,
			"run has prerequisite change sets but no prepared workspace", nil)
	}
	initialHead := ""
	if claim.Workspace.Kind == model.WorkspaceWorktree {
		initialHead, _ = gitTextWithEnv(ctx, claim.Workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
			"rev-parse", "--verify", "HEAD^{commit}")
	}
	result, err := m.integratePrerequisiteHandoffs(ctx, *claim.Workspace, handoffs)
	if err != nil || result.EffectiveBaseCommit == "" {
		return result, err
	}
	if claim.Workspace.Kind != model.WorkspaceWorktree {
		return result, nil
	}
	updated, err := opened.UpdateRunWorkspaceBase(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, result.EffectiveBaseCommit)
	if err != nil {
		if initialHead != "" {
			return PrerequisiteIntegrationResult{}, errors.Join(err, rollbackIntegration(ctx, claim.Workspace.Path, initialHead))
		}
		return PrerequisiteIntegrationResult{}, err
	}
	claim.Workspace = &updated
	return result, nil
}

// VerifyPrerequisiteChangeSets ensures a worker did not rewrite its final
// history so that a pinned prerequisite disappeared after integration.
func (m *Manager) VerifyPrerequisiteChangeSets(ctx context.Context, opened *store.Store, taskID string, workspace model.RunWorkspace, descendant string) error {
	handoffs, err := opened.ListPrerequisiteHandoffs(ctx, taskID)
	if err != nil {
		return err
	}
	changeSets := prerequisiteChangeSets(handoffs)
	if len(changeSets) == 0 {
		return nil
	}
	if workspace.RepositoryPath == nil || workspace.Kind != model.WorkspaceWorktree {
		return integrationError(IntegrationFailureUnsupportedWorkspace, model.BlockKindCapability, workspace, nil,
			"cannot verify prerequisite history outside a prepared Git worktree", nil)
	}
	if !validObjectID(descendant) {
		return integrationError(IntegrationFailureHistoryRewrite, model.BlockKindNeedsInput, workspace, nil,
			"worker produced an invalid final Git commit", nil)
	}
	childCommon, err := gitCommonDirectory(ctx, workspace.Path)
	if err != nil {
		return err
	}
	for _, handoff := range changeSets {
		if integrationErr := verifyChangeSetReference(ctx, childCommon, workspace, handoff); integrationErr != nil {
			return integrationErr
		}
		ancestor, err := gitIsAncestor(ctx, workspace.Path, handoff.ChangeSet.HeadCommit, descendant)
		if err != nil {
			return err
		}
		if !ancestor {
			return integrationError(IntegrationFailureHistoryRewrite, model.BlockKindNeedsInput, workspace, &handoff,
				fmt.Sprintf("worker history dropped prerequisite change set %s from task %s", handoff.ChangeSet.ID, handoff.PrerequisiteID), nil)
		}
	}
	return nil
}
