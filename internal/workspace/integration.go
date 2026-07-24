package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/filesecurity"
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
	IntegrationFailureResolutionExhausted  = "resolution_exhausted"
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
	Resolution          *model.IntegrationResolution
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

func resolutionTarget(handoff model.PrerequisiteHandoff, mergeInProgress bool) model.IntegrationResolutionTarget {
	return model.IntegrationResolutionTarget{
		PrerequisiteID:  handoff.PrerequisiteID,
		ChangeSetID:     handoff.ChangeSet.ID,
		HeadCommit:      handoff.ChangeSet.HeadCommit,
		DurableRef:      handoff.ChangeSet.DurableRef,
		MergeInProgress: mergeInProgress,
	}
}

func integrationConflictFingerprint(unmergedIndex []byte, handoffs []model.PrerequisiteHandoff) string {
	heads := make([]string, 0, len(handoffs))
	for _, handoff := range handoffs {
		if handoff.ChangeSet != nil {
			heads = append(heads, handoff.ChangeSet.HeadCommit)
		}
	}
	return integrationConflictFingerprintFromHeads(unmergedIndex, heads)
}

func integrationConflictFingerprintFromHeads(unmergedIndex []byte, values []string) string {
	digest := sha256.New()
	writeFingerprintField := func(value []byte) {
		var size [binary.MaxVarintLen64]byte
		written := binary.PutUvarint(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:written])
		_, _ = digest.Write(value)
	}
	writeFingerprintField([]byte("autogora-integration-conflict-v1"))
	writeFingerprintField(unmergedIndex)
	uniqueHeads := make(map[string]struct{}, len(values))
	for _, head := range values {
		uniqueHeads[strings.ToLower(strings.TrimSpace(head))] = struct{}{}
	}
	heads := make([]string, 0, len(uniqueHeads))
	for head := range uniqueHeads {
		heads = append(heads, head)
	}
	sort.Strings(heads)
	for _, head := range heads {
		writeFingerprintField([]byte(head))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func rollbackIntegrationDurably(directory, initialHead string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return rollbackIntegration(ctx, directory, initialHead)
}

func boundedManifestConflicts(values []string) ([]string, int) {
	const (
		fileLimit = 200
		byteLimit = 1024
	)
	result := make([]string, 0, min(len(values), fileLimit))
	for _, value := range values {
		if len(result) == fileLimit {
			break
		}
		value = strings.ToValidUTF8(strings.TrimSpace(value), "\uFFFD")
		if value == "" {
			continue
		}
		if len(value) > byteLimit {
			value = value[:byteLimit]
			for !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
		}
		result = append(result, value)
	}
	return result, max(0, len(values)-len(result))
}

func safeManifestRunID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func writeIntegrationResolutionManifest(
	ctx context.Context,
	claim *model.ClaimedTask,
	resolution *model.IntegrationResolution,
) error {
	if claim == nil || claim.Workspace == nil || resolution == nil {
		return errors.New("integration resolution manifest requires a prepared claim")
	}
	if !safeManifestRunID(claim.Run.ID) {
		return errors.New("run id is unsafe for an integration resolution manifest")
	}
	common, err := gitCommonDirectory(ctx, claim.Workspace.Path)
	if err != nil {
		return fmt.Errorf("resolve integration resolution manifest directory: %w", err)
	}
	directory := filepath.Join(common, "autogora", "integration-resolutions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create integration resolution manifest directory: %w", err)
	}
	directory, err = canonicalPath(directory)
	if err != nil {
		return fmt.Errorf("resolve integration resolution manifest directory: %w", err)
	}
	relative, err := filepath.Rel(common, directory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("integration resolution manifest directory escaped Git metadata")
	}
	if err := filesecurity.RestrictDirectoryToCurrentUser(directory); err != nil {
		return fmt.Errorf("secure integration resolution manifest directory: %w", err)
	}
	conflicts, omitted := boundedManifestConflicts(resolution.ConflictingFiles)
	manifest := model.IntegrationResolutionManifest{
		Version: model.IntegrationResolutionManifestVersion,
		TaskID:  claim.Task.Task.ID, RunID: claim.Run.ID,
		ConflictFingerprint: resolution.ConflictFingerprint,
		WorkspacePath:       resolution.WorkspacePath,
		Targets:             append([]model.IntegrationResolutionTarget(nil), resolution.Targets...),
		ConflictingFiles:    conflicts, ConflictingFileCount: len(resolution.ConflictingFiles),
		ConflictingFilesOmitted: omitted,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode integration resolution manifest: %w", err)
	}
	if len(encoded) > model.IntegrationResolutionManifestMaxBytes {
		return fmt.Errorf(
			"integration resolution manifest exceeds %d bytes for %d target(s)",
			model.IntegrationResolutionManifestMaxBytes, len(resolution.Targets),
		)
	}
	temporary, err := os.CreateTemp(directory, "."+claim.Run.ID+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create integration resolution manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := filesecurity.RestrictToCurrentUser(temporaryPath); err != nil {
		cleanup()
		return fmt.Errorf("secure integration resolution manifest: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("write integration resolution manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync integration resolution manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close integration resolution manifest: %w", err)
	}
	finalPath := filepath.Join(directory, claim.Run.ID+".json")
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish integration resolution manifest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	resolution.ManifestPath = finalPath
	resolution.ManifestSHA256 = fmt.Sprintf("%x", digest[:])
	resolution.ConflictingFileCount = len(resolution.ConflictingFiles)
	resolution.ConflictingFilesOmitted = omitted
	resolution.TargetCount = len(resolution.Targets)
	return nil
}

// independentPrerequisiteHeads computes the same reachability relation as
// `git merge-base --independent`, but supplies revisions through stdin and
// streams one rev-list process. Topological output visits every child before
// its parents. An input is independent exactly when no already-visited
// descendant input reaches it.
func independentPrerequisiteHeads(
	ctx context.Context,
	directory string,
	heads []string,
) (map[string]bool, error) {
	unique := make([]string, 0, len(heads))
	targetHeads := make(map[string]bool, len(heads))
	for _, value := range heads {
		head := strings.ToLower(strings.TrimSpace(value))
		if targetHeads[head] {
			continue
		}
		targetHeads[head] = true
		unique = append(unique, head)
	}
	independent := make(map[string]bool, len(unique))
	if len(unique) == 0 {
		return independent, nil
	}
	commandCtx, cancelCommand := context.WithTimeout(ctx, hostGitCommandLimit)
	defer cancelCommand()
	command := workspaceCommand(commandCtx, "git", "-C", directory,
		"rev-list", "--topo-order", "--parents", "--stdin")
	command.Env = append([]string(nil), os.Environ()...)
	command.Env = append(command.Env, "GIT_TERMINAL_PROMPT=0")
	command.Stdin = strings.NewReader(strings.Join(unique, "\n") + "\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		command.Close()
		return nil, err
	}
	reachedByTarget := make(map[string]bool, len(unique))
	seenTargets := make(map[string]bool, len(unique))
	var readErr error
	readDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, lineErr := reader.ReadString('\n')
			if len(line) > 0 {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					commit := strings.ToLower(fields[0])
					reached := reachedByTarget[commit]
					delete(reachedByTarget, commit)
					if targetHeads[commit] {
						seenTargets[commit] = true
						independent[commit] = !reached
						reached = true
					}
					if reached {
						for _, parent := range fields[1:] {
							reachedByTarget[strings.ToLower(parent)] = true
						}
					}
				}
			}
			if lineErr != nil {
				if errors.Is(lineErr, io.EOF) {
					readDone <- nil
				} else {
					readDone <- lineErr
				}
				return
			}
		}
	}()
	select {
	case readErr = <-readDone:
	case <-commandCtx.Done():
		_ = stdout.Close()
		readErr = errors.Join(commandCtx.Err(), <-readDone)
	}
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil {
		return nil, fmt.Errorf("git rev-list prerequisite graph: %w: %s",
			errors.Join(readErr, waitErr), strings.TrimSpace(stderr.String()))
	}
	for _, head := range unique {
		if !seenTargets[head] {
			return nil, fmt.Errorf("git rev-list omitted prerequisite head %s", head)
		}
	}
	return independent, nil
}

// orderPrerequisiteChangeSets puts independent aggregate tips before commits
// already contained by them without placing the entire fan-in in argv.
func orderPrerequisiteChangeSets(
	ctx context.Context,
	directory string,
	handoffs []model.PrerequisiteHandoff,
) ([]model.PrerequisiteHandoff, error) {
	ordered := append([]model.PrerequisiteHandoff(nil), handoffs...)
	if len(ordered) < 2 {
		return ordered, nil
	}
	heads := make([]string, 0, len(ordered))
	for _, handoff := range ordered {
		heads = append(heads, handoff.ChangeSet.HeadCommit)
	}
	independent, err := independentPrerequisiteHeads(ctx, directory, heads)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left := independent[strings.ToLower(ordered[i].ChangeSet.HeadCommit)]
		right := independent[strings.ToLower(ordered[j].ChangeSet.HeadCommit)]
		if left != right {
			return left
		}
		if ordered[i].PrerequisiteID != ordered[j].PrerequisiteID {
			return ordered[i].PrerequisiteID < ordered[j].PrerequisiteID
		}
		return ordered[i].ChangeSet.ID < ordered[j].ChangeSet.ID
	})
	return ordered, nil
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
	var abortErr error
	if _, err := gitOutputWithEnv(ctx, directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "-q", "MERGE_HEAD"); err == nil {
		if _, err := gitOutputWithEnv(ctx, directory, integrationGitEnvironment(), "merge", "--abort"); err != nil {
			abortErr = fmt.Errorf("abort prerequisite merge: %w", err)
		}
	}
	_, resetErr := gitOutputWithEnv(ctx, directory, integrationGitEnvironment(), "reset", "--hard", initialHead)
	if resetErr != nil {
		resetErr = fmt.Errorf("reset prerequisite integration to %s: %w", initialHead, resetErr)
	}
	return errors.Join(abortErr, resetErr)
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

// ValidateIntegrationResolutionStart rechecks the live Git conflict at the
// final process-start boundary. The manifest and database reservation describe
// an earlier snapshot; neither is sufficient if another process has since
// resolved, aborted, or replaced one of the pinned durable refs.
func ValidateIntegrationResolutionStart(ctx context.Context, resolution model.IntegrationResolution) error {
	if strings.TrimSpace(resolution.WorkspacePath) == "" {
		return errors.New("integration resolution workspace is empty")
	}
	if len(resolution.Targets) == 0 {
		return errors.New("integration resolution has no target heads")
	}
	first := resolution.Targets[0]
	mergeHead, err := gitTextWithEnv(ctx, resolution.WorkspacePath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "MERGE_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("conflicting merge is no longer in progress: %w", err)
	}
	if !strings.EqualFold(mergeHead, strings.TrimSpace(first.HeadCommit)) {
		return fmt.Errorf("MERGE_HEAD changed from %s to %s", first.HeadCommit, mergeHead)
	}
	unmergedIndex, err := gitOutputWithEnv(ctx, resolution.WorkspacePath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"ls-files", "-u", "-z")
	if err != nil {
		return fmt.Errorf("read unresolved Git index: %w", err)
	}
	if len(unmergedIndex) == 0 {
		return errors.New("unresolved Git index is empty")
	}
	heads := make([]string, 0, len(resolution.Targets))
	expectedRefs := make(map[string]string, len(resolution.Targets))
	for _, target := range resolution.Targets {
		head := strings.ToLower(strings.TrimSpace(target.HeadCommit))
		ref := strings.TrimSpace(target.DurableRef)
		if !validObjectID(head) || !strings.HasPrefix(ref, "refs/autogora/runs/") {
			return errors.New("integration resolution contains an invalid target ref")
		}
		if previous, exists := expectedRefs[ref]; exists && previous != head {
			return fmt.Errorf("durable ref %s names multiple target heads", ref)
		}
		expectedRefs[ref] = head
		heads = append(heads, head)
	}
	fingerprint := integrationConflictFingerprintFromHeads(unmergedIndex, heads)
	if !strings.EqualFold(fingerprint, strings.TrimSpace(resolution.ConflictFingerprint)) {
		return fmt.Errorf(
			"integration conflict fingerprint changed from %s to %s",
			resolution.ConflictFingerprint,
			fingerprint,
		)
	}
	refOutput, err := gitTextWithEnv(ctx, resolution.WorkspacePath,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"for-each-ref", "--format=%(refname) %(objectname)", "refs/autogora/runs/")
	if err != nil {
		return fmt.Errorf("read durable integration refs: %w", err)
	}
	actualRefs := make(map[string]string, len(expectedRefs))
	for _, line := range strings.Split(refOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			actualRefs[fields[0]] = strings.ToLower(fields[1])
		}
	}
	for ref, expected := range expectedRefs {
		actual, exists := actualRefs[ref]
		if !exists {
			return fmt.Errorf("durable ref %s disappeared before finalizer launch", ref)
		}
		if actual != expected {
			return fmt.Errorf("durable ref %s changed from %s to %s", ref, expected, actual)
		}
	}
	return nil
}

func (m *Manager) integratePrerequisiteHandoffs(ctx context.Context, workspace model.RunWorkspace, handoffs []model.PrerequisiteHandoff) (PrerequisiteIntegrationResult, error) {
	return m.integratePrerequisiteHandoffsWithResolution(ctx, workspace, handoffs, false)
}

func (m *Manager) integratePrerequisiteHandoffsWithResolution(
	ctx context.Context,
	workspace model.RunWorkspace,
	handoffs []model.PrerequisiteHandoff,
	allowResolution bool,
) (PrerequisiteIntegrationResult, error) {
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
	for _, handoff := range changeSets {
		if integrationErr := verifyChangeSetReference(ctx, childCommon, workspace, handoff); integrationErr != nil {
			return result, integrationErr
		}
	}
	changeSets, err = orderPrerequisiteChangeSets(ctx, workspace.Path, changeSets)
	if err != nil {
		return result, integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, nil,
			"cannot order prerequisite change sets by Git ancestry", err)
	}
	if workspace.Kind == model.WorkspaceDir {
		for _, handoff := range changeSets {
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
	for index, handoff := range changeSets {
		item := integrationItem(handoff)
		ancestor, err := gitIsAncestor(ctx, workspace.Path, item.HeadCommit, "HEAD")
		if err != nil {
			integrationErr := integrationError(IntegrationFailureInvalidReference, model.BlockKindCapability, workspace, &handoff,
				fmt.Sprintf("cannot inspect prerequisite change set %s from task %s", item.ChangeSetID, item.PrerequisiteID), err)
			return result, addRollbackFailure(integrationErr, rollbackIntegrationDurably(workspace.Path, initialHead))
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
			if code == IntegrationFailureConflict && allowResolution {
				unmergedIndex, indexErr := gitOutputWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
					"ls-files", "-u", "-z")
				if indexErr != nil {
					integrationErr.Reason = "cannot capture the unresolved Git index for bounded finalizer recovery"
					integrationErr.Cause = errors.Join(mergeErr, indexErr)
					return result, addRollbackFailure(integrationErr, rollbackIntegrationDurably(workspace.Path, initialHead))
				}
				targets := make([]model.IntegrationResolutionTarget, 0, len(changeSets)-index)
				for pending := index; pending < len(changeSets); pending++ {
					targets = append(targets, resolutionTarget(changeSets[pending], pending == index))
				}
				result.Resolution = &model.IntegrationResolution{
					ConflictFingerprint: integrationConflictFingerprint(unmergedIndex, changeSets[index:]),
					WorkspacePath:       workspace.Path,
					ConflictingFiles:    conflicts, Targets: targets,
				}
				return result, nil
			}
			return result, addRollbackFailure(integrationErr, rollbackIntegrationDurably(workspace.Path, initialHead))
		}
		result.Applied = append(result.Applied, item)
	}
	result.EffectiveBaseCommit, err = gitTextWithEnv(ctx, workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		_ = rollbackIntegrationDurably(workspace.Path, initialHead)
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
		initialHead, err = gitTextWithEnv(ctx, claim.Workspace.Path, map[string]string{"GIT_TERMINAL_PROMPT": "0"},
			"rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return PrerequisiteIntegrationResult{}, fmt.Errorf(
				"capture initial finalizer worktree HEAD before prerequisite integration: %w",
				err,
			)
		}
	}
	allowResolution := m.allowWrites &&
		claim.Task.Task.WorkflowRole == model.WorkflowRoleFinalizer &&
		claim.Workspace.Kind == model.WorkspaceWorktree &&
		claim.Workspace.Generated
	result, err := m.integratePrerequisiteHandoffsWithResolution(ctx, *claim.Workspace, handoffs, allowResolution)
	if err != nil {
		persistExceptionalIntegrationIncident(opened, claim.Task.Task.ID, err)
		return result, err
	}
	if result.Resolution != nil {
		target := result.Resolution.Targets[0]
		reservation, reserveErr := opened.ReserveIntegrationResolution(ctx, store.RunScope{
			RunID: claim.Run.ID, ClaimToken: claim.ClaimToken,
		}, store.ReserveIntegrationResolutionInput{
			WorkspacePath: result.Resolution.WorkspacePath, PrerequisiteID: target.PrerequisiteID,
			ChangeSetID: target.ChangeSetID, ConflictFingerprint: result.Resolution.ConflictFingerprint,
			ConflictingFiles: result.Resolution.ConflictingFiles,
		})
		if reserveErr != nil {
			rollbackErr := rollbackIntegrationDurably(claim.Workspace.Path, initialHead)
			if errors.Is(reserveErr, store.ErrIntegrationResolutionExhausted) {
				integrationErr := &PrerequisiteIntegrationError{
					Code: IntegrationFailureResolutionExhausted, BlockKind: model.BlockKindNeedsInput,
					Reason: fmt.Sprintf(
						"finalizer integration resolution exhausted its %d attempt(s); inspect preserved attempts or revise the conflicting work before retrying",
						reservation.MaxAttempts,
					),
					WorkspacePath: claim.Workspace.Path, PrerequisiteID: target.PrerequisiteID,
					ChangeSetID: target.ChangeSetID, DurableRef: target.DurableRef,
					ConflictingFiles: result.Resolution.ConflictingFiles, Cause: reserveErr,
				}
				integrationErr = addRollbackFailure(integrationErr, rollbackErr)
				persistExceptionalIntegrationIncident(opened, claim.Task.Task.ID, integrationErr)
				return PrerequisiteIntegrationResult{}, integrationErr
			}
			return PrerequisiteIntegrationResult{}, errors.Join(
				fmt.Errorf("reserve finalizer integration resolution: %w", reserveErr),
				rollbackErr,
			)
		}
		result.Resolution.Attempt = reservation.Attempt
		result.Resolution.MaxAttempts = reservation.MaxAttempts
		if manifestErr := writeIntegrationResolutionManifest(ctx, claim, result.Resolution); manifestErr != nil {
			return PrerequisiteIntegrationResult{}, errors.Join(
				fmt.Errorf("prepare finalizer integration resolution manifest: %w", manifestErr),
				rollbackIntegrationDurably(claim.Workspace.Path, initialHead),
			)
		}
		claim.IntegrationResolution = result.Resolution
		return result, nil
	}
	if result.EffectiveBaseCommit == "" {
		return result, err
	}
	if claim.Workspace.Kind != model.WorkspaceWorktree {
		return result, nil
	}
	updated, err := opened.UpdateRunWorkspaceBase(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, result.EffectiveBaseCommit)
	if err != nil {
		if initialHead != "" {
			return PrerequisiteIntegrationResult{}, errors.Join(err, rollbackIntegrationDurably(claim.Workspace.Path, initialHead))
		}
		return PrerequisiteIntegrationResult{}, err
	}
	claim.Workspace = &updated
	return result, nil
}

// VerifyPrerequisiteChangeSets ensures a worker did not rewrite its final
// history so that a pinned prerequisite disappeared after integration.
func (m *Manager) VerifyPrerequisiteChangeSets(ctx context.Context, opened *store.Store, taskID string, workspace model.RunWorkspace, descendant string) error {
	err := m.verifyPrerequisiteChangeSets(ctx, opened, taskID, workspace, descendant)
	persistExceptionalIntegrationIncident(opened, taskID, err)
	return err
}

// ValidateIntegrationResolutionResult returns the clean resolver commit only
// after every pinned handoff is present. The caller can compose recovery work
// on that commit before atomically promoting it to the persisted output base.
func (m *Manager) ValidateIntegrationResolutionResult(
	ctx context.Context,
	opened *store.Store,
	claim *model.ClaimedTask,
) (string, error) {
	if opened == nil || claim == nil || claim.Workspace == nil ||
		claim.IntegrationResolution == nil {
		return "", errors.New(
			"integration resolution base requires a prepared resolver claim",
		)
	}
	workspace := *claim.Workspace
	if workspace.Kind != model.WorkspaceWorktree || !workspace.Generated {
		return "", errors.New(
			"integration resolution base requires a generated worktree",
		)
	}
	unmerged, err := gitOutputWithEnv(
		ctx,
		workspace.Path,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"ls-files",
		"-u",
		"-z",
	)
	if err != nil {
		return "", fmt.Errorf(
			"inspect resolved integration index: %w",
			err,
		)
	}
	if len(unmerged) != 0 {
		return "", errors.New(
			"integration resolver left unresolved index entries",
		)
	}
	status, err := gitOutputWithEnv(
		ctx,
		workspace.Path,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"status",
		"--porcelain=v1",
		"-z",
		"--untracked-files=all",
	)
	if err != nil {
		return "", fmt.Errorf(
			"inspect resolved integration workspace: %w",
			err,
		)
	}
	if len(status) != 0 {
		return "", errors.New(
			"integration resolver must commit its resolution before completion",
		)
	}
	head, err := gitTextWithEnv(
		ctx,
		workspace.Path,
		map[string]string{"GIT_TERMINAL_PROMPT": "0"},
		"rev-parse",
		"--verify",
		"HEAD^{commit}",
	)
	if err != nil {
		return "", fmt.Errorf(
			"resolve integration commit: %w",
			err,
		)
	}
	if err := m.VerifyPrerequisiteChangeSets(
		ctx,
		opened,
		claim.Task.Task.ID,
		workspace,
		head,
	); err != nil {
		return "", fmt.Errorf(
			"resolved integration omitted a prerequisite: %w",
			err,
		)
	}
	return head, nil
}

func (m *Manager) verifyPrerequisiteChangeSets(ctx context.Context, opened *store.Store, taskID string, workspace model.RunWorkspace, descendant string) error {
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
