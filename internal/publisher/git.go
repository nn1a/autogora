package publisher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

var objectIDPattern = regexp.MustCompile(`\A[0-9a-fA-F]+\z`)

type validatedPublication struct {
	publication        model.Publication
	repository         string
	worktree           string
	head               string
	base               string
	target             string
	targetRef          string
	targetHead         string
	targetContainsHead bool
}

func (e *Engine) Execute(
	ctx context.Context,
	publication model.Publication,
) (Result, error) {
	result := Result{
		Mode: publication.Mode, HeadCommit: strings.TrimSpace(publication.HeadCommit),
		TargetBranch: strings.TrimSpace(publication.TargetBranch),
	}
	if publication.Mode == model.PublicationModeManual {
		result.Status = ResultManualRequired
		result.Message = "manual publication policy does not permit host-side execution"
		return result, semanticError(
			ErrorManualMode, "execute publication", ErrManualMode, result.Message,
		)
	}
	if publication.Mode != model.PublicationModeLocalFF &&
		publication.Mode != model.PublicationModePullRequest {
		return result, semanticError(
			ErrorInvalidInput, "execute publication", ErrInvalidInput,
			fmt.Sprintf("unsupported publication mode %q", publication.Mode),
		)
	}
	validated, err := e.validate(ctx, publication)
	if err != nil {
		return result, err
	}
	result.HeadCommit = validated.head
	result.TargetBranch = validated.target
	switch publication.Mode {
	case model.PublicationModeLocalFF:
		return e.publishLocalFF(ctx, validated, result)
	case model.PublicationModePullRequest:
		return e.publishPullRequest(ctx, validated, result)
	default:
		panic("validated publication mode was not handled")
	}
}

func (e *Engine) validate(
	ctx context.Context,
	publication model.Publication,
) (validatedPublication, error) {
	repository, err := existingDirectory(publication.RepositoryPath, "repository")
	if err != nil {
		return validatedPublication{}, err
	}
	worktree, err := existingDirectory(publication.WorktreePath, "source worktree")
	if err != nil {
		return validatedPublication{}, err
	}
	if err := e.validateRepositoryPair(ctx, repository, worktree); err != nil {
		return validatedPublication{}, err
	}

	head, err := normalizeObjectID(publication.HeadCommit, "head commit")
	if err != nil {
		return validatedPublication{}, err
	}
	base, err := normalizeObjectID(publication.BaseCommit, "base commit")
	if err != nil {
		return validatedPublication{}, err
	}
	if err := e.requireCommit(ctx, repository, head, "head commit"); err != nil {
		return validatedPublication{}, err
	}
	if err := e.requireCommit(ctx, repository, base, "base commit"); err != nil {
		return validatedPublication{}, err
	}

	durableRef := strings.TrimSpace(publication.DurableRef)
	if durableRef != publication.DurableRef || durableRef == "" ||
		!strings.HasPrefix(durableRef, "refs/autogora/runs/") {
		return validatedPublication{}, semanticError(
			ErrorInvalidInput, "validate durable ref", ErrInvalidInput,
			"durable ref must be an unmodified refs/autogora/runs ref",
		)
	}
	if _, err := e.command(ctx, repository, "validate durable ref",
		"git", "check-ref-format", durableRef); err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return validatedPublication{}, controlErr
		}
		return validatedPublication{}, semanticError(
			ErrorInvalidInput, "validate durable ref", ErrInvalidInput,
			"durable ref is not a valid Git ref",
		)
	}
	resolvedRef, err := e.gitText(ctx, repository, "resolve durable ref",
		"rev-parse", "--verify", "--end-of-options", durableRef+"^{commit}")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return validatedPublication{}, controlErr
		}
		return validatedPublication{}, semanticError(
			ErrorSourceChanged, "resolve durable ref", ErrSourceChanged,
			"durable ref is missing or does not identify a commit",
		)
	}
	if !sameObjectID(resolvedRef, head) {
		return validatedPublication{}, semanticError(
			ErrorSourceChanged, "validate durable ref", ErrSourceChanged,
			fmt.Sprintf("durable ref resolves to %s, expected %s", resolvedRef, head),
		)
	}

	target := strings.TrimSpace(publication.TargetBranch)
	if target == "" || target != publication.TargetBranch {
		return validatedPublication{}, semanticError(
			ErrorInvalidInput, "validate target branch", ErrInvalidInput,
			"target branch must be non-empty and contain no surrounding whitespace",
		)
	}
	checkedTarget, err := e.gitText(ctx, repository, "validate target branch",
		"check-ref-format", "--branch", target)
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return validatedPublication{}, controlErr
		}
		return validatedPublication{}, semanticError(
			ErrorInvalidInput, "validate target branch", ErrInvalidInput,
			"target branch is not safe",
		)
	}
	if checkedTarget != target {
		return validatedPublication{}, semanticError(
			ErrorInvalidInput, "validate target branch", ErrInvalidInput,
			"target branch uses a context-dependent Git shorthand",
		)
	}
	targetRef := "refs/heads/" + target
	targetHead, err := e.exactRef(ctx, repository, targetRef)
	if err != nil {
		return validatedPublication{}, err
	}
	if targetHead == "" {
		return validatedPublication{}, semanticError(
			ErrorRepository, "resolve target branch", ErrRepository,
			fmt.Sprintf("target branch %s does not exist", target),
		)
	}
	if !validObjectID(targetHead) {
		return validatedPublication{}, semanticError(
			ErrorRepository, "resolve target branch", ErrRepository,
			"target branch did not resolve to a full object ID",
		)
	}
	if err := e.requireAncestor(ctx, repository, base, head,
		"validate captured history", "base commit is not an ancestor of head commit"); err != nil {
		return validatedPublication{}, err
	}
	targetContainsHead, err := e.isAncestor(ctx, repository, head, targetHead)
	if err != nil {
		return validatedPublication{}, err
	}
	if publication.Mode == model.PublicationModeLocalFF && !targetContainsHead {
		targetCanFastForward, err := e.isAncestor(ctx, repository, targetHead, head)
		if err != nil {
			return validatedPublication{}, err
		}
		if !targetCanFastForward {
			return validatedPublication{}, semanticError(
				ErrorNonFastForward, "validate fast-forward", ErrNonFastForward,
				"target branch is not an ancestor of head commit",
			)
		}
	}
	return validatedPublication{
		publication: publication, repository: repository, worktree: worktree,
		head: head, base: base, target: target, targetRef: targetRef,
		targetHead: strings.ToLower(targetHead), targetContainsHead: targetContainsHead,
	}, nil
}

func existingDirectory(raw, label string) (string, error) {
	if raw == "" {
		return "", semanticError(
			ErrorInvalidInput, "validate "+label, ErrInvalidInput,
			label+" path must be non-empty",
		)
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", semanticError(ErrorRepository, "resolve "+label, ErrRepository, err.Error())
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", semanticError(
			ErrorRepository, "validate "+label, ErrRepository,
			fmt.Sprintf("%s does not exist: %v", label, err),
		)
	}
	if !info.IsDir() {
		return "", semanticError(
			ErrorRepository, "validate "+label, ErrRepository, label+" is not a directory",
		)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", semanticError(ErrorRepository, "resolve "+label, ErrRepository, err.Error())
	}
	return filepath.Clean(canonical), nil
}

func normalizeObjectID(raw, label string) (string, error) {
	value := strings.TrimSpace(raw)
	if value != raw || !validObjectID(value) {
		return "", semanticError(
			ErrorInvalidInput, "validate "+label, ErrInvalidInput,
			label+" must be a full 40- or 64-character hexadecimal object ID",
		)
	}
	return strings.ToLower(value), nil
}

func validObjectID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && objectIDPattern.MatchString(value)
}

func sameObjectID(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func (e *Engine) command(
	ctx context.Context,
	directory string,
	operation string,
	file string,
	args ...string,
) (commandResult, error) {
	result, err := e.run(ctx, directory, file, args...)
	if err == nil {
		return result, nil
	}
	var execution *Error
	if errors.As(err, &execution) {
		return result, &Error{
			Kind: execution.Kind, Operation: operation, Err: execution.Err,
			exitCode: execution.exitCode, hasExitCode: execution.hasExitCode,
		}
	}
	return result, &Error{Kind: ErrorCommandFailed, Operation: operation, Err: err}
}

func (e *Engine) gitText(
	ctx context.Context,
	directory string,
	operation string,
	args ...string,
) (string, error) {
	output, err := e.command(ctx, directory, operation, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output.stdout), nil
}

func (e *Engine) validateRepositoryPair(
	ctx context.Context,
	repository string,
	worktree string,
) error {
	repositoryRoot, err := e.gitText(ctx, repository, "resolve repository root",
		"rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return controlErr
		}
		return semanticError(
			ErrorRepository, "resolve repository root", ErrRepository,
			"repository is not a Git worktree",
		)
	}
	worktreeRoot, err := e.gitText(ctx, worktree, "resolve source worktree root",
		"rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return controlErr
		}
		return semanticError(
			ErrorRepository, "resolve source worktree root", ErrRepository,
			"source worktree is not a Git worktree",
		)
	}
	repositoryRoot, err = canonicalGitPath(repository, repositoryRoot)
	if err != nil || repositoryRoot != repository {
		return semanticError(
			ErrorRepository, "validate repository root", ErrRepository,
			"repository path is not the Git worktree root",
		)
	}
	worktreeRoot, err = canonicalGitPath(worktree, worktreeRoot)
	if err != nil || worktreeRoot != worktree {
		return semanticError(
			ErrorRepository, "validate source worktree root", ErrRepository,
			"source worktree path is not the Git worktree root",
		)
	}
	repositoryCommon, err := e.gitText(ctx, repository, "resolve repository common directory",
		"rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return controlErr
		}
		return semanticError(
			ErrorRepository, "resolve repository common directory", ErrRepository, err.Error(),
		)
	}
	worktreeCommon, err := e.gitText(ctx, worktree, "resolve source common directory",
		"rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return controlErr
		}
		return semanticError(
			ErrorRepository, "resolve source common directory", ErrRepository, err.Error(),
		)
	}
	repositoryCommon, err = canonicalGitPath(repository, repositoryCommon)
	if err != nil {
		return semanticError(
			ErrorRepository, "resolve repository common directory", ErrRepository, err.Error(),
		)
	}
	worktreeCommon, err = canonicalGitPath(worktree, worktreeCommon)
	if err != nil {
		return semanticError(
			ErrorRepository, "resolve source common directory", ErrRepository, err.Error(),
		)
	}
	if repositoryCommon != worktreeCommon {
		return semanticError(
			ErrorRepository, "validate source repository", ErrRepository,
			"source worktree belongs to a different Git common repository",
		)
	}
	return nil
}

func canonicalGitPath(base, raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(base, raw)
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func (e *Engine) requireCommit(
	ctx context.Context,
	repository string,
	objectID string,
	label string,
) error {
	resolved, err := e.gitText(ctx, repository, "resolve "+label,
		"rev-parse", "--verify", "--end-of-options", objectID+"^{commit}")
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return controlErr
		}
		return semanticError(
			ErrorSourceChanged, "resolve "+label, ErrSourceChanged,
			label+" does not exist as a commit",
		)
	}
	if !sameObjectID(resolved, objectID) {
		return semanticError(
			ErrorSourceChanged, "validate "+label, ErrSourceChanged,
			fmt.Sprintf("%s resolves to %s, expected %s", label, resolved, objectID),
		)
	}
	return nil
}

func (e *Engine) exactRef(
	ctx context.Context,
	repository string,
	ref string,
) (string, error) {
	output, err := e.gitText(ctx, repository, "resolve Git ref",
		"for-each-ref", "--format=%(objectname)%00%(refname)", "--", ref)
	if err != nil {
		return "", err
	}
	matches := make([]string, 0, 1)
	for _, line := range nonEmptyLines(output) {
		fields := strings.Split(line, "\x00")
		if len(fields) != 2 || !validObjectID(fields[0]) {
			return "", semanticError(
				ErrorRepository, "resolve Git ref", ErrRepository,
				"Git returned an invalid exact ref record",
			)
		}
		if fields[1] == ref {
			matches = append(matches, fields[0])
		}
	}
	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return strings.ToLower(matches[0]), nil
	default:
		return "", semanticError(
			ErrorRepository, "resolve Git ref", ErrRepository,
			"an exact Git ref query returned more than one object",
		)
	}
}

func nonEmptyLines(value string) []string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func (e *Engine) requireAncestor(
	ctx context.Context,
	repository string,
	ancestor string,
	descendant string,
	operation string,
	detail string,
) error {
	contains, err := e.isAncestor(ctx, repository, ancestor, descendant)
	if err != nil {
		return err
	}
	if !contains {
		return semanticError(
			ErrorNonFastForward, operation, ErrNonFastForward, detail,
		)
	}
	return nil
}

func (e *Engine) isAncestor(
	ctx context.Context,
	repository string,
	ancestor string,
	descendant string,
) (bool, error) {
	if sameObjectID(ancestor, descendant) {
		return true, nil
	}
	_, err := e.command(ctx, repository, "inspect commit ancestry", "git",
		"merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var execution *Error
	if errors.As(err, &execution) && execution.Kind == ErrorCommandFailed &&
		execution.hasExitCode && execution.exitCode == 1 {
		return false, nil
	}
	return false, err
}

type worktreeRecord struct {
	path   string
	head   string
	branch string
}

func parseWorktreePorcelain(value string) ([]worktreeRecord, error) {
	fields := strings.Split(value, "\x00")
	result := make([]worktreeRecord, 0)
	current := worktreeRecord{}
	flush := func() error {
		if current.path == "" && current.head == "" && current.branch == "" {
			return nil
		}
		if current.path == "" {
			return errors.New("Git worktree record is missing its path")
		}
		result = append(result, current)
		current = worktreeRecord{}
		return nil
	}
	for _, field := range fields {
		if field == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		switch {
		case strings.HasPrefix(field, "worktree "):
			if current.path != "" {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			current.path = strings.TrimPrefix(field, "worktree ")
		case strings.HasPrefix(field, "HEAD "):
			current.head = strings.TrimPrefix(field, "HEAD ")
		case strings.HasPrefix(field, "branch "):
			current.branch = strings.TrimPrefix(field, "branch ")
		case field == "detached", field == "bare", field == "locked",
			field == "prunable":
			// These flags do not affect branch ownership.
		case strings.HasPrefix(field, "locked "), strings.HasPrefix(field, "prunable "):
			// Optional human-readable reason.
		default:
			return nil, fmt.Errorf("unknown Git worktree field %q",
				boundedText(field, 256, false))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Engine) checkedOutTarget(
	ctx context.Context,
	publication validatedPublication,
) (string, error) {
	output, err := e.command(ctx, publication.repository, "list Git worktrees",
		"git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	records, err := parseWorktreePorcelain(output.stdout)
	if err != nil {
		return "", semanticError(
			ErrorRepository, "parse Git worktrees", ErrRepository, err.Error(),
		)
	}
	found := ""
	for _, record := range records {
		if record.branch != publication.targetRef {
			continue
		}
		if found != "" {
			return "", semanticError(
				ErrorRepository, "locate target worktree", ErrRepository,
				"target branch is checked out in more than one worktree",
			)
		}
		path, err := existingDirectory(record.path, "target worktree")
		if err != nil {
			return "", err
		}
		found = path
	}
	return found, nil
}

func (e *Engine) publishLocalFF(
	ctx context.Context,
	publication validatedPublication,
	result Result,
) (Result, error) {
	if publication.targetContainsHead {
		result.Status = ResultAlreadyPublished
		result.Message = "target branch already contains the captured head commit"
		return result, nil
	}
	targetWorktree, err := e.checkedOutTarget(ctx, publication)
	if err != nil {
		return result, err
	}
	if targetWorktree == "" {
		if _, err := e.command(ctx, publication.repository,
			"fast-forward target branch", "git", "update-ref",
			publication.targetRef, publication.head, publication.targetHead); err != nil {
			if controlErr := commandControlError(err); controlErr != nil {
				return result, controlErr
			}
			return result, semanticError(
				ErrorSourceChanged, "fast-forward target branch", ErrSourceChanged,
				"target branch changed before the compare-and-swap update",
			)
		}
	} else {
		status, err := e.command(ctx, targetWorktree, "inspect target worktree",
			"git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
		if err != nil {
			return result, err
		}
		if status.stdout != "" {
			return result, semanticError(
				ErrorDirtyWorktree, "inspect target worktree", ErrDirtyWorktree,
				"checked-out target branch has tracked or untracked changes",
			)
		}
		currentHead, err := e.gitText(ctx, targetWorktree, "verify target worktree head",
			"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
		if err != nil {
			return result, err
		}
		if !sameObjectID(currentHead, publication.targetHead) {
			return result, semanticError(
				ErrorSourceChanged, "verify target worktree head", ErrSourceChanged,
				"checked-out target branch changed before fast-forward",
			)
		}
		if _, err := e.command(ctx, targetWorktree, "fast-forward checked-out target",
			"git", "merge", "--ff-only", "--no-edit", publication.head); err != nil {
			if controlErr := commandControlError(err); controlErr != nil {
				return result, controlErr
			}
			return result, semanticError(
				ErrorNonFastForward, "fast-forward checked-out target",
				ErrNonFastForward, "Git refused the fast-forward merge",
			)
		}
	}
	current, err := e.exactRef(ctx, publication.repository, publication.targetRef)
	if err != nil {
		return result, err
	}
	if !sameObjectID(current, publication.head) {
		return result, semanticError(
			ErrorSourceChanged, "verify published target", ErrSourceChanged,
			"target branch does not point to the captured head after publication",
		)
	}
	result.Status = ResultPublished
	result.Message = "target branch fast-forwarded to the captured head commit"
	return result, nil
}
