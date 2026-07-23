package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

const (
	RecoveryAdoptionFastForward    = "fast_forward"
	RecoveryAdoptionMerge          = "merge"
	RecoveryAdoptionAlreadyPresent = "already_present"
)

// RecoveryCheckpoint is an immutable Git snapshot of a stopped run. The
// output base remains the prerequisite-integrated commit used for the final
// full task diff. SourceStartCommit instead records the commit at which this
// particular process started, so callers can distinguish inherited recovery
// work from changes made by the stopped attempt.
type RecoveryCheckpoint struct {
	RunID             string
	RepositoryPath    string
	WorktreePath      string
	OutputBaseCommit  string
	SourceStartCommit string
	SourceHeadCommit  string
	HeadCommit        string
	DurableRef        string
	ChangedFiles      []string
}

// RecoveryCheckpointAdoption describes the prepared target after checkpoint
// recovery. AdoptedHeadCommit is the exact HEAD from which the next process
// must start and therefore the correct baseline for InspectChangesSince.
type RecoveryCheckpointAdoption struct {
	RepositoryPath       string
	WorktreePath         string
	WorktreeGitDirectory string
	OutputBaseCommit     string
	InitialHeadCommit    string
	CheckpointHeadCommit string
	AdoptedHeadCommit    string
	Mode                 string
	ChangedFiles         []string
}

type recoveryWorktreeIdentity struct {
	TopLevel        string
	GitDirectory    string
	CommonDirectory string
}

func gitPathWithEnv(
	ctx context.Context,
	directory string,
	environment map[string]string,
	args ...string,
) (string, error) {
	output, err := gitOutputWithEnv(ctx, directory, environment, args...)
	if err != nil {
		return "", err
	}
	output = bytes.TrimSuffix(output, []byte{'\n'})
	output = bytes.TrimSuffix(output, []byte{'\r'})
	if len(output) == 0 {
		return "", errors.New("git returned an empty path")
	}
	return string(output), nil
}

func exactRecoveryWorktreeIdentity(
	ctx context.Context,
	path, label string,
) (recoveryWorktreeIdentity, error) {
	var identity recoveryWorktreeIdentity
	if strings.TrimSpace(path) == "" {
		return identity, fmt.Errorf("%s is empty", label)
	}
	requested, err := canonicalPath(path)
	if err != nil {
		return identity, fmt.Errorf("resolve %s: %w", label, err)
	}
	topLevel, err := gitPathWithEnv(ctx, path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--show-toplevel")
	if err != nil {
		return identity, fmt.Errorf("%s is not an available Git worktree: %w", label, err)
	}
	topLevel, err = canonicalPath(topLevel)
	if err != nil {
		return identity, fmt.Errorf("resolve %s top-level: %w", label, err)
	}
	if requested != topLevel {
		return identity, fmt.Errorf(
			"%s must name the exact Git worktree top-level %s, not %s",
			label,
			topLevel,
			requested,
		)
	}
	gitDirectory, err := gitPathWithEnv(ctx, topLevel, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return identity, fmt.Errorf("resolve %s Git directory: %w", label, err)
	}
	gitDirectory, err = canonicalPath(gitDirectory)
	if err != nil {
		return identity, fmt.Errorf("canonicalize %s Git directory: %w", label, err)
	}
	commonDirectory, err := gitPathWithEnv(
		ctx,
		topLevel,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse",
		"--path-format=absolute",
		"--git-common-dir",
	)
	if err != nil {
		return identity, fmt.Errorf("resolve %s common Git directory: %w", label, err)
	}
	commonDirectory, err = canonicalPath(commonDirectory)
	if err != nil {
		return identity, fmt.Errorf("canonicalize %s common Git directory: %w", label, err)
	}
	return recoveryWorktreeIdentity{
		TopLevel: topLevel, GitDirectory: gitDirectory, CommonDirectory: commonDirectory,
	}, nil
}

func optionalRecoverySourceIdentity(
	ctx context.Context,
	path string,
) (*recoveryWorktreeIdentity, error) {
	// The old path is audit data after capture. Use an identity only when it is
	// still an intact worktree; disappearance, reuse, or inaccessibility cannot
	// invalidate an otherwise exact durable checkpoint.
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}
	if !info.IsDir() {
		return nil, nil
	}
	identity, err := exactRecoveryWorktreeIdentity(ctx, path, "recovery checkpoint source worktree")
	if err != nil {
		return nil, nil
	}
	return &identity, nil
}

func recoveryCheckpointRef(runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" ||
		strings.HasPrefix(runID, ".") ||
		strings.HasSuffix(runID, ".") ||
		strings.HasSuffix(strings.ToLower(runID), ".lock") ||
		strings.Contains(runID, "..") {
		return "", errors.New("invalid run id for recovery checkpoint")
	}
	for _, character := range runID {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			character != '-' && character != '_' && character != '.' {
			return "", errors.New("invalid run id for recovery checkpoint")
		}
	}
	return "refs/autogora/checkpoints/" + runID, nil
}

func exactRefHead(ctx context.Context, repository, ref string) (string, bool, error) {
	symbolic, symbolicErr := gitTextWithEnv(
		ctx,
		repository,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"symbolic-ref", "-q", ref,
	)
	if symbolicErr == nil {
		return "", false, fmt.Errorf("recovery checkpoint ref %s is symbolic to %s", ref, symbolic)
	}
	var symbolicExit *exec.ExitError
	if !errors.As(symbolicErr, &symbolicExit) || symbolicExit.ExitCode() != 1 {
		return "", false, symbolicErr
	}
	value, err := gitTextWithEnv(ctx, repository, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "-q", ref)
	if err == nil {
		return value, true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return "", false, nil
	}
	return "", false, err
}

func resolveExactCommit(ctx context.Context, directory, revision, label string) (string, error) {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	value, err := gitTextWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return value, nil
}

func resolvePinnedCommit(ctx context.Context, directory, revision, label string) (string, error) {
	revision = strings.TrimSpace(revision)
	if !validObjectID(revision) {
		return "", fmt.Errorf("%s must be an exact full Git object ID", label)
	}
	resolved, err := resolveExactCommit(ctx, directory, revision, label)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(resolved, revision) {
		return "", fmt.Errorf("%s does not resolve to its exact commit", label)
	}
	return resolved, nil
}

func requireAncestor(ctx context.Context, directory, ancestor, descendant, reason string) error {
	ok, err := gitIsAncestor(ctx, directory, ancestor, descendant)
	if err != nil {
		return fmt.Errorf("%s: %w", reason, err)
	}
	if !ok {
		return errors.New(reason)
	}
	return nil
}

func createTemporaryIndex() (string, func(), error) {
	temporary, err := os.CreateTemp("", "autogora-checkpoint-index-*")
	if err != nil {
		return "", nil, err
	}
	indexPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", nil, err
	}
	if err := os.Remove(indexPath); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(indexPath)
		_ = os.Remove(indexPath + ".lock")
	}
	return indexPath, cleanup, nil
}

func recoveryGitEnvironment(indexPath string) map[string]string {
	environment := integrationGitEnvironment()
	environment["GIT_INDEX_FILE"] = indexPath
	return environment
}

func checkpointHeadMatchesTree(
	ctx context.Context,
	directory, checkpointHead, sourceHead, tree string,
) error {
	resolved, err := resolveExactCommit(ctx, directory, checkpointHead, "existing checkpoint head")
	if err != nil {
		return err
	}
	if !strings.EqualFold(resolved, checkpointHead) {
		return errors.New("existing recovery checkpoint does not resolve to its exact commit")
	}
	existingTree, err := gitTextWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", checkpointHead+"^{tree}")
	if err != nil {
		return err
	}
	if !strings.EqualFold(existingTree, tree) {
		return errors.New("immutable recovery checkpoint already captures different work")
	}
	if strings.EqualFold(checkpointHead, sourceHead) {
		return nil
	}
	line, err := gitTextWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-list", "--parents", "-n", "1", checkpointHead)
	if err != nil {
		return err
	}
	fields := strings.Fields(line)
	if len(fields) != 2 ||
		!strings.EqualFold(fields[0], checkpointHead) ||
		!strings.EqualFold(fields[1], sourceHead) {
		return errors.New("immutable recovery checkpoint is not a direct snapshot of the source HEAD")
	}
	return nil
}

// CaptureRecoveryCheckpoint snapshots all committed, staged, unstaged, and
// untracked work without changing the source HEAD, index, or working tree. It
// writes objects through a temporary index and publishes one immutable CAS ref
// scoped to the stopped run.
func (m *Manager) CaptureRecoveryCheckpoint(
	ctx context.Context,
	workspace model.RunWorkspace,
	sourceStartCommit, taskID, title string,
) (RecoveryCheckpoint, error) {
	if workspace.Kind != model.WorkspaceWorktree ||
		workspace.RepositoryPath == nil ||
		strings.TrimSpace(*workspace.RepositoryPath) == "" ||
		workspace.BaseCommit == nil ||
		strings.TrimSpace(*workspace.BaseCommit) == "" {
		return RecoveryCheckpoint{}, errors.New("recovery checkpoint requires a prepared git worktree")
	}
	repositoryIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		*workspace.RepositoryPath,
		"recovery checkpoint repository",
	)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	sourceIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		workspace.Path,
		"recovery checkpoint source worktree",
	)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	if repositoryIdentity.CommonDirectory != sourceIdentity.CommonDirectory {
		return RecoveryCheckpoint{}, errors.New(
			"recovery checkpoint source belongs to a different repository",
		)
	}
	repositoryPath := repositoryIdentity.TopLevel
	worktreePath := sourceIdentity.TopLevel
	unmerged, err := gitOutputWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"ls-files", "-u", "-z")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	if len(unmerged) != 0 {
		return RecoveryCheckpoint{}, errors.New("cannot checkpoint a worktree with unresolved conflicts")
	}
	outputBase, err := resolvePinnedCommit(ctx, workspace.Path, *workspace.BaseCommit, "prepared output base")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	sourceStart, err := resolvePinnedCommit(ctx, workspace.Path, sourceStartCommit, "source start commit")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	sourceHead, err := resolveExactCommit(ctx, workspace.Path, "HEAD", "source HEAD")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	if err := requireAncestor(
		ctx,
		workspace.Path,
		outputBase,
		sourceStart,
		"source start commit does not descend from the prepared output base",
	); err != nil {
		return RecoveryCheckpoint{}, err
	}
	if err := requireAncestor(
		ctx,
		workspace.Path,
		sourceStart,
		sourceHead,
		"source HEAD no longer contains the run start commit",
	); err != nil {
		return RecoveryCheckpoint{}, err
	}

	ref, err := recoveryCheckpointRef(workspace.RunID)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	indexPath, cleanup, err := createTemporaryIndex()
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	defer cleanup()
	environment := recoveryGitEnvironment(indexPath)
	if _, err := gitOutputWithEnv(ctx, workspace.Path, environment, "read-tree", sourceHead); err != nil {
		return RecoveryCheckpoint{}, err
	}
	if _, err := gitOutputWithEnv(ctx, workspace.Path, environment, "add", "-A", "--", ":/"); err != nil {
		return RecoveryCheckpoint{}, err
	}
	tree, err := gitTextWithEnv(ctx, workspace.Path, environment, "write-tree")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	sourceTree, err := gitTextWithEnv(ctx, workspace.Path, environment,
		"rev-parse", "--verify", sourceHead+"^{tree}")
	if err != nil {
		return RecoveryCheckpoint{}, err
	}

	head := sourceHead
	existingHead, exists, err := exactRefHead(ctx, repositoryPath, ref)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	if exists {
		if err := checkpointHeadMatchesTree(ctx, repositoryPath, existingHead, sourceHead, tree); err != nil {
			return RecoveryCheckpoint{}, err
		}
		head = existingHead
	} else if !strings.EqualFold(tree, sourceTree) {
		head, err = gitTextWithEnv(
			ctx,
			workspace.Path,
			environment,
			"-c", "user.name=Autogora",
			"-c", "user.email=autogora@localhost",
			"-c", "commit.gpgSign=false",
			"commit-tree", tree, "-p", sourceHead,
			"-m", boundedCommitSubject(taskID, "recovery checkpoint: "+title),
		)
		if err != nil {
			return RecoveryCheckpoint{}, err
		}
	}
	if !exists {
		if _, updateErr := gitOutputWithEnv(ctx, repositoryPath, environment,
			"update-ref", ref, head, ""); updateErr != nil {
			// Another observer may have captured the same stopped process. Only
			// adopt its winner when the immutable content and ancestry match.
			winner, winnerExists, readErr := exactRefHead(ctx, repositoryPath, ref)
			if readErr != nil || !winnerExists {
				return RecoveryCheckpoint{}, errors.Join(updateErr, readErr)
			}
			if matchErr := checkpointHeadMatchesTree(ctx, repositoryPath, winner, sourceHead, tree); matchErr != nil {
				return RecoveryCheckpoint{}, errors.Join(updateErr, matchErr)
			}
			head = winner
		}
	}
	exactHead, exists, err := exactRefHead(ctx, repositoryPath, ref)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	if !exists || !strings.EqualFold(exactHead, head) {
		return RecoveryCheckpoint{}, errors.New("recovery checkpoint ref does not point to the captured head")
	}
	if err := requireAncestor(
		ctx,
		repositoryPath,
		outputBase,
		head,
		"recovery checkpoint head does not descend from the prepared output base",
	); err != nil {
		return RecoveryCheckpoint{}, err
	}
	changedOutput, err := gitOutputWithEnv(ctx, repositoryPath, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"diff", "--name-only", "-z", outputBase, head)
	if err != nil {
		return RecoveryCheckpoint{}, err
	}
	return RecoveryCheckpoint{
		RunID: workspace.RunID, RepositoryPath: repositoryPath, WorktreePath: worktreePath,
		OutputBaseCommit: outputBase, SourceStartCommit: sourceStart,
		SourceHeadCommit: sourceHead, HeadCommit: head, DurableRef: ref,
		ChangedFiles: splitNullTerminated(changedOutput),
	}, nil
}

// ValidateRecoveryCheckpoint verifies the complete immutable Git provenance
// before any target worktree is changed.
func ValidateRecoveryCheckpoint(ctx context.Context, checkpoint RecoveryCheckpoint) error {
	expectedRef, err := recoveryCheckpointRef(checkpoint.RunID)
	if err != nil {
		return err
	}
	if checkpoint.DurableRef != expectedRef {
		return fmt.Errorf("recovery checkpoint ref %q does not match expected ref %q", checkpoint.DurableRef, expectedRef)
	}
	if strings.TrimSpace(checkpoint.RepositoryPath) == "" ||
		strings.TrimSpace(checkpoint.WorktreePath) == "" {
		return errors.New("recovery checkpoint is missing its repository or source worktree")
	}
	_, err = exactRecoveryWorktreeIdentity(
		ctx,
		checkpoint.RepositoryPath,
		"recovery checkpoint repository",
	)
	if err != nil {
		return fmt.Errorf("resolve recovery checkpoint repository: %w", err)
	}
	// WorktreePath is immutable audit provenance after capture. Recovery is
	// anchored by the canonical repository, exact commits, and durable ref, so
	// validation deliberately does not require the old worktree to remain.
	resolvePinned := func(value, label string) (string, error) {
		return resolvePinnedCommit(ctx, checkpoint.RepositoryPath, value, label)
	}
	outputBase, err := resolvePinned(checkpoint.OutputBaseCommit, "checkpoint output base")
	if err != nil {
		return err
	}
	sourceStart, err := resolvePinned(checkpoint.SourceStartCommit, "checkpoint source start")
	if err != nil {
		return err
	}
	head, err := resolvePinned(checkpoint.HeadCommit, "checkpoint head")
	if err != nil {
		return err
	}
	refHead, exists, err := exactRefHead(ctx, checkpoint.RepositoryPath, checkpoint.DurableRef)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("recovery checkpoint ref is missing")
	}
	if !strings.EqualFold(refHead, head) {
		return fmt.Errorf(
			"recovery checkpoint ref resolves to %s, expected %s",
			refHead,
			head,
		)
	}
	relations := []struct {
		ancestor   string
		descendant string
		reason     string
	}{
		{outputBase, sourceStart, "checkpoint source start does not descend from its output base"},
		{sourceStart, head, "checkpoint head does not descend from its run start"},
	}
	// SourceHeadCommit is useful capture-time evidence but is optional in a
	// persisted recovery reservation. The immutable start-to-checkpoint
	// ancestry remains sufficient to validate adoption after restart.
	if strings.TrimSpace(checkpoint.SourceHeadCommit) != "" {
		sourceHead, err := resolvePinned(checkpoint.SourceHeadCommit, "checkpoint source HEAD")
		if err != nil {
			return err
		}
		relations[1] = struct {
			ancestor   string
			descendant string
			reason     string
		}{
			sourceStart,
			sourceHead,
			"checkpoint source HEAD does not descend from its run start",
		}
		relations = append(relations, struct {
			ancestor   string
			descendant string
			reason     string
		}{
			sourceHead,
			head,
			"checkpoint head does not descend from its source HEAD",
		})
	}
	for _, relation := range relations {
		if err := requireAncestor(
			ctx,
			checkpoint.RepositoryPath,
			relation.ancestor,
			relation.descendant,
			relation.reason,
		); err != nil {
			return err
		}
	}
	changedOutput, err := gitOutputWithEnv(ctx, checkpoint.RepositoryPath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"diff", "--name-only", "-z", outputBase, head)
	if err != nil {
		return err
	}
	if actual := splitNullTerminated(changedOutput); !slices.Equal(actual, checkpoint.ChangedFiles) {
		return errors.New("recovery checkpoint changed-file manifest does not match its commits")
	}
	return nil
}

func rollbackRecoveryAdoption(ctx context.Context, path, initialHead string) error {
	rollbackErr := rollbackIntegration(ctx, path, initialHead)
	status, statusErr := gitOutputWithEnv(ctx, path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if statusErr == nil && len(status) != 0 {
		statusErr = errors.New("recovery adoption rollback left the target worktree dirty")
	}
	return errors.Join(rollbackErr, statusErr)
}

func recoveryAdoptionFailure(
	path, initialHead string,
	cause error,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return errors.Join(cause, rollbackRecoveryAdoption(ctx, path, initialHead))
}

func validExistingRecoveryAdoption(
	ctx context.Context,
	directory, initialHead, targetBase, checkpointHead string,
) (bool, error) {
	if strings.EqualFold(initialHead, targetBase) ||
		strings.EqualFold(initialHead, checkpointHead) {
		return true, nil
	}
	line, err := gitTextWithEnv(
		ctx,
		directory,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-list",
		"--parents",
		"-n",
		"1",
		initialHead,
	)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(line)
	return len(fields) == 3 &&
		strings.EqualFold(fields[0], initialHead) &&
		strings.EqualFold(fields[1], targetBase) &&
		strings.EqualFold(fields[2], checkpointHead), nil
}

// AdoptRecoveryCheckpoint restores an immutable checkpoint into a distinct,
// clean prepared worktree after prerequisite integration. It keeps the
// target's persisted output base unchanged and either fast-forwards or merges
// histories so the checkpoint head remains an ancestor of the process start.
func (m *Manager) AdoptRecoveryCheckpoint(
	ctx context.Context,
	target model.RunWorkspace,
	checkpoint RecoveryCheckpoint,
) (RecoveryCheckpointAdoption, error) {
	if target.Kind != model.WorkspaceWorktree ||
		target.RepositoryPath == nil ||
		strings.TrimSpace(*target.RepositoryPath) == "" ||
		target.BaseCommit == nil ||
		strings.TrimSpace(*target.BaseCommit) == "" {
		return RecoveryCheckpointAdoption{}, errors.New("recovery adoption requires a prepared git worktree")
	}
	if err := ValidateRecoveryCheckpoint(ctx, checkpoint); err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	targetRepositoryIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		*target.RepositoryPath,
		"recovery adoption repository",
	)
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if targetRepositoryIdentity.TopLevel != checkpoint.RepositoryPath {
		return RecoveryCheckpointAdoption{}, errors.New(
			"recovery checkpoint names a different repository top-level",
		)
	}
	targetIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		target.Path,
		"recovery adoption target worktree",
	)
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if targetRepositoryIdentity.CommonDirectory != targetIdentity.CommonDirectory {
		return RecoveryCheckpointAdoption{}, errors.New(
			"recovery adoption target belongs to a different repository",
		)
	}
	sourcePath, err := canonicalPath(checkpoint.WorktreePath)
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if targetIdentity.TopLevel == sourcePath {
		return RecoveryCheckpointAdoption{}, errors.New("recovery checkpoint must be adopted into a distinct worktree")
	}
	sourceIdentity, err := optionalRecoverySourceIdentity(ctx, checkpoint.WorktreePath)
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if sourceIdentity != nil && targetIdentity.GitDirectory == sourceIdentity.GitDirectory {
		return RecoveryCheckpointAdoption{}, errors.New(
			"recovery checkpoint source and target resolve to the same physical worktree",
		)
	}
	if targetIdentity.CommonDirectory != targetRepositoryIdentity.CommonDirectory {
		return RecoveryCheckpointAdoption{}, errors.New("recovery checkpoint belongs to a different repository")
	}
	unmerged, err := gitOutputWithEnv(ctx, target.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"ls-files", "-u", "-z")
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if len(unmerged) != 0 {
		return RecoveryCheckpointAdoption{}, errors.New("target worktree contains unresolved conflicts")
	}
	status, err := gitOutputWithEnv(ctx, target.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if len(status) != 0 {
		return RecoveryCheckpointAdoption{}, errors.New("target worktree must be clean before recovery adoption")
	}
	targetBase, err := resolvePinnedCommit(ctx, target.Path, *target.BaseCommit, "target output base")
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	initialHead, err := resolveExactCommit(ctx, target.Path, "HEAD", "target HEAD")
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if err := requireAncestor(
		ctx,
		target.Path,
		targetBase,
		initialHead,
		"target HEAD does not descend from its output base",
	); err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if !strings.EqualFold(checkpoint.OutputBaseCommit, targetBase) {
		checkpointBaseAncestor, err := gitIsAncestor(
			ctx, target.Path, checkpoint.OutputBaseCommit, targetBase,
		)
		if err != nil {
			return RecoveryCheckpointAdoption{}, err
		}
		if !checkpointBaseAncestor {
			targetBaseAncestor, err := gitIsAncestor(
				ctx, target.Path, targetBase, checkpoint.OutputBaseCommit,
			)
			if err != nil {
				return RecoveryCheckpointAdoption{}, err
			}
			if targetBaseAncestor {
				return RecoveryCheckpointAdoption{}, errors.New(
					"target output base is older than the checkpoint output base",
				)
			}
			if _, err := gitTextWithEnv(
				ctx,
				target.Path,
				map[string]string{"GIT_TERMINAL_PROMPT": "0"},
				"merge-base",
				checkpoint.OutputBaseCommit,
				targetBase,
			); err != nil {
				return RecoveryCheckpointAdoption{}, errors.New(
					"checkpoint and target output bases do not share ancestry",
				)
			}
			checkpointBaseTree, err := gitTextWithEnv(
				ctx,
				target.Path,
				map[string]string{"GIT_TERMINAL_PROMPT": "0"},
				"rev-parse",
				"--verify",
				checkpoint.OutputBaseCommit+"^{tree}",
			)
			if err != nil {
				return RecoveryCheckpointAdoption{}, err
			}
			targetBaseTree, err := gitTextWithEnv(
				ctx,
				target.Path,
				map[string]string{"GIT_TERMINAL_PROMPT": "0"},
				"rev-parse",
				"--verify",
				targetBase+"^{tree}",
			)
			if err != nil {
				return RecoveryCheckpointAdoption{}, err
			}
			if !strings.EqualFold(checkpointBaseTree, targetBaseTree) {
				return RecoveryCheckpointAdoption{}, errors.New(
					"independent checkpoint and target output bases have different trees",
				)
			}
		}
	}

	result := RecoveryCheckpointAdoption{
		RepositoryPath: targetRepositoryIdentity.TopLevel, WorktreePath: targetIdentity.TopLevel,
		WorktreeGitDirectory: targetIdentity.GitDirectory,
		OutputBaseCommit:     targetBase, InitialHeadCommit: initialHead,
		CheckpointHeadCommit: checkpoint.HeadCommit,
	}
	alreadyPresent, err := gitIsAncestor(ctx, target.Path, checkpoint.HeadCommit, initialHead)
	if err != nil {
		return RecoveryCheckpointAdoption{}, err
	}
	if alreadyPresent {
		valid, err := validExistingRecoveryAdoption(
			ctx,
			target.Path,
			initialHead,
			targetBase,
			checkpoint.HeadCommit,
		)
		if err != nil {
			return RecoveryCheckpointAdoption{}, err
		}
		if !valid {
			return RecoveryCheckpointAdoption{}, errors.New(
				"target contains commits beyond a verifiable checkpoint adoption",
			)
		}
		result.AdoptedHeadCommit = initialHead
		result.Mode = RecoveryAdoptionAlreadyPresent
	} else {
		if !strings.EqualFold(initialHead, targetBase) {
			return RecoveryCheckpointAdoption{}, errors.New(
				"target contains work beyond its output base before recovery adoption",
			)
		}
		canFastForward, err := gitIsAncestor(ctx, target.Path, initialHead, checkpoint.HeadCommit)
		if err != nil {
			return RecoveryCheckpointAdoption{}, err
		}
		if canFastForward {
			_, err = gitOutputWithEnv(ctx, target.Path, integrationGitEnvironment(),
				"-c", "commit.gpgSign=false", "merge", "--ff-only", "--no-stat", checkpoint.HeadCommit)
			result.Mode = RecoveryAdoptionFastForward
		} else {
			_, err = gitOutputWithEnv(ctx, target.Path, integrationGitEnvironment(),
				"-c", "commit.gpgSign=false", "merge", "--no-ff", "--no-edit", "--no-stat",
				"-m", fmt.Sprintf("autogora: adopt recovery checkpoint %s", checkpoint.RunID),
				checkpoint.HeadCommit)
			result.Mode = RecoveryAdoptionMerge
		}
		if err != nil {
			conflicts, _ := gitOutputWithEnv(ctx, target.Path,
				map[string]string{"GIT_TERMINAL_PROMPT": "0"},
				"diff", "--name-only", "--diff-filter=U", "-z")
			cause := fmt.Errorf("adopt recovery checkpoint")
			if names := splitNullTerminated(conflicts); len(names) != 0 {
				cause = fmt.Errorf("recovery checkpoint conflicts in %s", strings.Join(names, ", "))
			}
			return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(
				target.Path,
				initialHead,
				errors.Join(cause, err),
			)
		}
		result.AdoptedHeadCommit, err = resolveExactCommit(ctx, target.Path, "HEAD", "adopted target HEAD")
		if err != nil {
			return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(target.Path, initialHead, err)
		}
	}

	if err := requireAncestor(
		ctx,
		target.Path,
		checkpoint.HeadCommit,
		result.AdoptedHeadCommit,
		"adopted target HEAD does not preserve checkpoint ancestry",
	); err != nil {
		return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(target.Path, initialHead, err)
	}
	refHead, exists, err := exactRefHead(ctx, target.Path, checkpoint.DurableRef)
	if err != nil || !exists || !strings.EqualFold(refHead, checkpoint.HeadCommit) {
		if err == nil {
			err = errors.New("recovery checkpoint ref changed during adoption")
		}
		return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(target.Path, initialHead, err)
	}
	status, err = gitOutputWithEnv(ctx, target.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		if err == nil {
			err = errors.New("adopted target worktree is not clean")
		}
		return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(target.Path, initialHead, err)
	}
	changedOutput, err := gitOutputWithEnv(ctx, target.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"diff", "--name-only", "-z", targetBase, result.AdoptedHeadCommit)
	if err != nil {
		return RecoveryCheckpointAdoption{}, recoveryAdoptionFailure(target.Path, initialHead, err)
	}
	result.ChangedFiles = splitNullTerminated(changedOutput)
	return result, nil
}

// RollbackRecoveryCheckpointAdoption compensates for a database confirmation
// failure after Git adoption succeeded. It refuses to touch a target that has
// moved or acquired any new index/worktree state since adoption.
func (m *Manager) RollbackRecoveryCheckpointAdoption(
	ctx context.Context,
	adoption RecoveryCheckpointAdoption,
) error {
	if strings.TrimSpace(adoption.RepositoryPath) == "" ||
		strings.TrimSpace(adoption.WorktreePath) == "" ||
		strings.TrimSpace(adoption.WorktreeGitDirectory) == "" {
		return errors.New("recovery adoption rollback is missing its repository or worktree")
	}
	repositoryIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		adoption.RepositoryPath,
		"recovery adoption rollback repository",
	)
	if err != nil {
		return err
	}
	worktreeIdentity, err := exactRecoveryWorktreeIdentity(
		ctx,
		adoption.WorktreePath,
		"recovery adoption rollback worktree",
	)
	if err != nil {
		return err
	}
	if repositoryIdentity.CommonDirectory != worktreeIdentity.CommonDirectory {
		return errors.New("recovery adoption rollback worktree belongs to a different repository")
	}
	expectedGitDirectory, err := canonicalPath(adoption.WorktreeGitDirectory)
	if err != nil {
		return fmt.Errorf("resolve recovery adoption rollback Git directory: %w", err)
	}
	if worktreeIdentity.GitDirectory != expectedGitDirectory {
		return errors.New("recovery adoption rollback worktree identity changed")
	}
	initialHead, err := resolvePinnedCommit(
		ctx,
		adoption.WorktreePath,
		adoption.InitialHeadCommit,
		"recovery adoption initial HEAD",
	)
	if err != nil {
		return err
	}
	outputBase, err := resolvePinnedCommit(
		ctx,
		adoption.WorktreePath,
		adoption.OutputBaseCommit,
		"recovery adoption output base",
	)
	if err != nil {
		return err
	}
	checkpointHead, err := resolvePinnedCommit(
		ctx,
		adoption.WorktreePath,
		adoption.CheckpointHeadCommit,
		"recovery adoption checkpoint head",
	)
	if err != nil {
		return err
	}
	adoptedHead, err := resolvePinnedCommit(
		ctx,
		adoption.WorktreePath,
		adoption.AdoptedHeadCommit,
		"recovery adoption adopted HEAD",
	)
	if err != nil {
		return err
	}
	for _, relation := range []struct {
		ancestor   string
		descendant string
		reason     string
	}{
		{outputBase, initialHead, "recovery adoption initial HEAD does not contain its output base"},
		{initialHead, adoptedHead, "recovery adoption adopted HEAD does not contain its initial HEAD"},
		{checkpointHead, adoptedHead, "recovery adoption adopted HEAD does not contain its checkpoint"},
	} {
		if err := requireAncestor(
			ctx,
			adoption.WorktreePath,
			relation.ancestor,
			relation.descendant,
			relation.reason,
		); err != nil {
			return err
		}
	}
	currentHead, err := resolveExactCommit(
		ctx,
		adoption.WorktreePath,
		"HEAD",
		"recovery adoption rollback current HEAD",
	)
	if err != nil {
		return err
	}
	if !strings.EqualFold(currentHead, adoptedHead) {
		return fmt.Errorf(
			"recovery adoption target HEAD changed from %s to %s; refusing rollback",
			adoptedHead,
			currentHead,
		)
	}
	status, err := gitOutputWithEnv(
		ctx,
		adoption.WorktreePath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=all",
	)
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("recovery adoption target changed after adoption; refusing rollback")
	}
	if _, err := gitOutputWithEnv(
		ctx,
		adoption.WorktreePath,
		integrationGitEnvironment(),
		"reset",
		"--hard",
		initialHead,
	); err != nil {
		return fmt.Errorf("rollback recovery adoption to %s: %w", initialHead, err)
	}
	rolledBackHead, err := resolveExactCommit(
		ctx,
		adoption.WorktreePath,
		"HEAD",
		"rolled-back recovery adoption HEAD",
	)
	if err != nil {
		return err
	}
	if !strings.EqualFold(rolledBackHead, initialHead) {
		return fmt.Errorf(
			"recovery adoption rollback left HEAD at %s, expected %s",
			rolledBackHead,
			initialHead,
		)
	}
	status, err = gitOutputWithEnv(
		ctx,
		adoption.WorktreePath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=all",
	)
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("recovery adoption rollback left the target worktree dirty")
	}
	return nil
}
