package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var safeRemotePattern = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]*\z`)
var safeRemoteHostPattern = regexp.MustCompile(
	`\A[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?\z`,
)
var safeRepositoryComponentPattern = regexp.MustCompile(
	`\A[A-Za-z0-9][A-Za-z0-9._-]*\z`,
)

type publicationRemote struct {
	name               string
	repositorySelector string
	repositoryIdentity string
}

type remoteRepositoryIdentity struct {
	selector string
	identity string
}

type pullRequestRecord struct {
	URL        string `json:"url"`
	HeadRefOID string `json:"headRefOid"`
}

func validateRemote(raw string) (string, error) {
	remote := strings.TrimSpace(raw)
	if remote == "" || remote != raw || len(remote) > 128 ||
		!safeRemotePattern.MatchString(remote) ||
		strings.Contains(remote, "..") || strings.HasSuffix(remote, ".lock") {
		return "", semanticError(
			ErrorInvalidInput, "validate publication remote", ErrInvalidInput,
			"remote must be a safe configured Git remote name",
		)
	}
	return remote, nil
}

func parseRemoteRepositoryIdentity(
	raw string,
) (remoteRepositoryIdentity, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > 8*1024 ||
		strings.IndexByte(raw, 0) >= 0 {
		return remoteRepositoryIdentity{}, semanticError(
			ErrorInvalidInput,
			"resolve publication remote",
			ErrInvalidInput,
			"publication remote URL must be non-empty, bounded, and unmodified",
		)
	}
	for _, value := range raw {
		if unicode.IsControl(value) {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote URL contains control characters",
			)
		}
	}

	host := ""
	path := ""
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
			parsed.Hostname() == "" || parsed.Port() != "" {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote URL is not an unambiguous repository URL",
			)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "https":
			if parsed.User != nil {
				return remoteRepositoryIdentity{}, semanticError(
					ErrorInvalidInput,
					"resolve publication remote",
					ErrInvalidInput,
					"publication remote URL must not contain embedded credentials",
				)
			}
		case "ssh":
			password, hasPassword := "", false
			if parsed.User != nil {
				password, hasPassword = parsed.User.Password()
			}
			if parsed.User == nil || parsed.User.Username() != "git" ||
				hasPassword || password != "" {
				return remoteRepositoryIdentity{}, semanticError(
					ErrorInvalidInput,
					"resolve publication remote",
					ErrInvalidInput,
					"SSH publication remotes must use the credential-free git user",
				)
			}
		default:
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote URL must use HTTPS or SSH",
			)
		}
		if parsed.EscapedPath() != parsed.Path {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote URL must not contain escaped path components",
			)
		}
		host = parsed.Hostname()
		path = strings.TrimPrefix(parsed.Path, "/")
	} else {
		left, right, found := strings.Cut(raw, ":")
		if !found || strings.Count(raw, ":") != 1 {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote must identify one hosted repository",
			)
		}
		user, remoteHost, found := strings.Cut(left, "@")
		if !found || user != "git" || remoteHost == "" {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"SCP publication remotes must use the credential-free git user",
			)
		}
		host, path = remoteHost, right
	}

	host = strings.ToLower(host)
	if !safeRemoteHostPattern.MatchString(host) ||
		strings.Contains(host, "..") {
		return remoteRepositoryIdentity{}, semanticError(
			ErrorInvalidInput,
			"resolve publication remote",
			ErrInvalidInput,
			"publication remote host is not safe",
		)
	}
	path = strings.TrimSuffix(path, ".git")
	components := strings.Split(path, "/")
	if len(components) != 2 {
		return remoteRepositoryIdentity{}, semanticError(
			ErrorInvalidInput,
			"resolve publication remote",
			ErrInvalidInput,
			"publication remote must identify exactly one owner and repository",
		)
	}
	for _, component := range components {
		if component == "." || component == ".." ||
			!safeRepositoryComponentPattern.MatchString(component) {
			return remoteRepositoryIdentity{}, semanticError(
				ErrorInvalidInput,
				"resolve publication remote",
				ErrInvalidInput,
				"publication remote owner or repository is not safe",
			)
		}
	}
	selector := host + "/" + components[0] + "/" + components[1]
	return remoteRepositoryIdentity{
		selector: selector,
		identity: strings.ToLower(selector),
	}, nil
}

func sanitizedTaskID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", semanticError(
			ErrorInvalidInput, "build pull-request branch", ErrInvalidInput,
			"task ID cannot be empty",
		)
	}
	var result strings.Builder
	result.Grow(min(len(raw), 80))
	lastSeparator := false
	for _, value := range raw {
		if result.Len() >= 80 {
			break
		}
		if value <= unicode.MaxASCII &&
			((value >= 'a' && value <= 'z') ||
				(value >= 'A' && value <= 'Z') ||
				(value >= '0' && value <= '9')) {
			result.WriteRune(unicode.ToLower(value))
			lastSeparator = false
			continue
		}
		if value == '.' || value == '_' || value == '-' {
			if result.Len() > 0 && !lastSeparator {
				result.WriteByte('-')
				lastSeparator = true
			}
			continue
		}
		if result.Len() > 0 && !lastSeparator {
			result.WriteByte('-')
			lastSeparator = true
		}
	}
	value := strings.Trim(result.String(), "._-")
	if value == "" {
		value = "task"
	}
	return value, nil
}

func (e *Engine) pullRequestBranch(
	ctx context.Context,
	publication validatedPublication,
) (string, error) {
	taskID, err := sanitizedTaskID(publication.publication.TaskID)
	if err != nil {
		return "", err
	}
	branch := fmt.Sprintf("autogora/%s-%s", taskID, publication.head[:8])
	if _, err := e.command(ctx, publication.repository, "validate pull-request branch",
		"git", "check-ref-format", "--branch", branch); err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return "", controlErr
		}
		return "", semanticError(
			ErrorInvalidInput, "validate pull-request branch", ErrInvalidInput,
			"generated pull-request branch is not a valid Git branch",
		)
	}
	return branch, nil
}

func (e *Engine) configuredRemote(
	ctx context.Context,
	publication validatedPublication,
) (publicationRemote, error) {
	remote, err := validateRemote(publication.publication.Remote)
	if err != nil {
		return publicationRemote{}, err
	}
	fetchOutput, err := e.gitText(
		ctx,
		publication.repository,
		"resolve publication remote",
		"remote", "get-url", "--all", remote)
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return publicationRemote{}, controlErr
		}
		return publicationRemote{}, semanticError(
			ErrorInvalidInput, "resolve publication remote", ErrInvalidInput,
			fmt.Sprintf("configured remote %s does not exist", remote),
		)
	}
	pushOutput, err := e.gitText(
		ctx,
		publication.repository,
		"resolve publication push remote",
		"remote", "get-url", "--push", "--all", remote,
	)
	if err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return publicationRemote{}, controlErr
		}
		return publicationRemote{}, semanticError(
			ErrorInvalidInput,
			"resolve publication push remote",
			ErrInvalidInput,
			fmt.Sprintf("configured remote %s has no push URL", remote),
		)
	}
	fetchURLs := nonEmptyLines(fetchOutput)
	pushURLs := nonEmptyLines(pushOutput)
	if len(fetchURLs) != 1 || len(pushURLs) != 1 {
		return publicationRemote{}, semanticError(
			ErrorInvalidInput, "resolve publication remote", ErrInvalidInput,
			"publication remote must have exactly one fetch URL and one push URL",
		)
	}
	fetchIdentity, err := parseRemoteRepositoryIdentity(fetchURLs[0])
	if err != nil {
		return publicationRemote{}, err
	}
	pushIdentity, err := parseRemoteRepositoryIdentity(pushURLs[0])
	if err != nil {
		return publicationRemote{}, err
	}
	if fetchIdentity.identity != pushIdentity.identity {
		return publicationRemote{}, semanticError(
			ErrorInvalidInput,
			"resolve publication remote",
			ErrInvalidInput,
			"publication fetch and push URLs identify different repositories",
		)
	}
	return publicationRemote{
		name:               remote,
		repositorySelector: fetchIdentity.selector,
		repositoryIdentity: fetchIdentity.identity,
	}, nil
}

func (e *Engine) remoteBranchHead(
	ctx context.Context,
	publication validatedPublication,
	remote publicationRemote,
	branch string,
) (string, error) {
	ref := "refs/heads/" + branch
	output, err := e.gitText(ctx, publication.repository, "inspect remote publication branch",
		"ls-remote", "--heads", remote.name, ref)
	if err != nil {
		return "", err
	}
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		return "", nil
	}
	if len(lines) != 1 {
		return "", semanticError(
			ErrorRemoteConflict, "inspect remote publication branch", ErrRemoteConflict,
			"remote returned more than one exact publication branch",
		)
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != ref || !validObjectID(fields[0]) {
		return "", semanticError(
			ErrorRemoteConflict, "inspect remote publication branch", ErrRemoteConflict,
			"remote returned an invalid publication branch record",
		)
	}
	return strings.ToLower(fields[0]), nil
}

func (e *Engine) ensurePullRequestBranch(
	ctx context.Context,
	publication validatedPublication,
	remote publicationRemote,
	branch string,
) (bool, error) {
	ref := "refs/heads/" + branch
	localHead, err := e.exactRef(ctx, publication.repository, ref)
	if err != nil {
		return false, err
	}
	if localHead != "" && !sameObjectID(localHead, publication.head) {
		return false, semanticError(
			ErrorRemoteConflict, "reuse pull-request branch", ErrRemoteConflict,
			fmt.Sprintf("local branch %s points to a different commit", branch),
		)
	}
	remoteHead, err := e.remoteBranchHead(ctx, publication, remote, branch)
	if err != nil {
		return false, err
	}
	if remoteHead != "" {
		if !sameObjectID(remoteHead, publication.head) {
			return false, semanticError(
				ErrorRemoteConflict, "reuse pull-request branch", ErrRemoteConflict,
				fmt.Sprintf("remote branch %s points to a different commit", branch),
			)
		}
		return true, nil
	}
	refspec := publication.head + ":" + ref
	if _, err := e.command(ctx, publication.repository, "push pull-request branch",
		"git", "push", "--porcelain",
		"--force-with-lease="+ref+":",
		remote.name,
		refspec,
	); err != nil {
		if controlErr := commandControlError(err); controlErr != nil {
			return false, controlErr
		}
		observed, observeErr := e.remoteBranchHead(
			ctx,
			publication,
			remote,
			branch,
		)
		if observeErr != nil {
			return false, err
		}
		if sameObjectID(observed, publication.head) {
			return true, nil
		}
		if observed != "" {
			return false, semanticError(
				ErrorRemoteConflict,
				"push pull-request branch",
				ErrRemoteConflict,
				"remote publication branch changed while it was being created",
			)
		}
		return false, err
	}
	remoteHead, err = e.remoteBranchHead(ctx, publication, remote, branch)
	if err != nil {
		return false, err
	}
	if !sameObjectID(remoteHead, publication.head) {
		return false, semanticError(
			ErrorSourceChanged, "verify remote publication branch", ErrSourceChanged,
			"remote branch does not point to the captured head after push",
		)
	}
	return false, nil
}

func (e *Engine) listPullRequests(
	ctx context.Context,
	publication validatedPublication,
	remote publicationRemote,
	branch string,
) ([]pullRequestRecord, error) {
	output, err := e.command(ctx, publication.repository, "list pull requests", "gh",
		"pr", "list",
		"--repo", remote.repositorySelector,
		"--head", branch,
		"--base", publication.target,
		"--state", "open",
		"--limit", "100",
		"--json", "url,headRefOid",
	)
	if err != nil {
		return nil, err
	}
	var records []pullRequestRecord
	if err := json.Unmarshal([]byte(output.stdout), &records); err != nil {
		return nil, semanticError(
			ErrorCommandFailed, "decode pull-request list", ErrCommandFailed,
			"gh returned invalid JSON",
		)
	}
	if len(records) > 100 {
		return nil, semanticError(
			ErrorCommandFailed, "decode pull-request list", ErrCommandFailed,
			"gh returned more pull requests than requested",
		)
	}
	return records, nil
}

func pullRequestURL(
	records []pullRequestRecord,
	head string,
	remote publicationRemote,
) (*string, error) {
	var found *string
	for _, record := range records {
		if !sameObjectID(record.HeadRefOID, head) {
			return nil, semanticError(
				ErrorRemoteConflict, "reuse pull request", ErrRemoteConflict,
				"an existing pull request for the publication branch has a different head",
			)
		}
		value, err := validPullRequestURL(record.URL, remote)
		if err != nil {
			return nil, err
		}
		if found == nil {
			found = &value
		}
	}
	return found, nil
}

func validPullRequestURL(
	raw string,
	remote publicationRemote,
) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 8*1024 {
		return "", semanticError(
			ErrorCommandFailed, "validate pull-request URL", ErrCommandFailed,
			"gh returned an empty or oversized pull-request URL",
		)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", semanticError(
			ErrorCommandFailed, "validate pull-request URL", ErrCommandFailed,
			"gh returned a pull-request URL for a different or invalid repository",
		)
	}
	selector := strings.Split(remote.repositoryIdentity, "/")
	path := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	number := uint64(0)
	if len(path) == 4 {
		number, _ = strconv.ParseUint(path[3], 10, 64)
	}
	if parsed.Scheme != "https" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.Port() != "" ||
		parsed.EscapedPath() != parsed.Path ||
		len(selector) != 3 ||
		len(path) != 4 ||
		!strings.EqualFold(parsed.Hostname(), selector[0]) ||
		!strings.EqualFold(path[0], selector[1]) ||
		!strings.EqualFold(path[1], selector[2]) ||
		path[2] != "pull" ||
		number == 0 {
		return "", semanticError(
			ErrorCommandFailed, "validate pull-request URL", ErrCommandFailed,
			"gh returned a pull-request URL for a different or invalid repository",
		)
	}
	return value, nil
}

func pullRequestTitle(publication validatedPublication) string {
	task := strings.Join(strings.Fields(publication.publication.TaskID), " ")
	task = boundedText(task, 120, false)
	if task == "" {
		task = "task"
	}
	return "Autogora: publish " + task
}

func pullRequestBody(publication validatedPublication) string {
	return fmt.Sprintf(
		"Autogora publication `%s`\n\nTask: `%s`\nChange set: `%s`\nCommit: `%s`\n",
		boundedText(publication.publication.ID, 160, false),
		boundedText(publication.publication.TaskID, 160, false),
		boundedText(publication.publication.ChangeSetID, 160, false),
		publication.head,
	)
}

func createdPullRequestURL(
	stdout string,
	remote publicationRemote,
) (string, error) {
	fields := strings.Fields(stdout)
	for index := len(fields) - 1; index >= 0; index-- {
		if value, err := validPullRequestURL(
			fields[index],
			remote,
		); err == nil {
			return value, nil
		}
	}
	return "", semanticError(
		ErrorCommandFailed, "read created pull-request URL", ErrCommandFailed,
		"gh did not return a pull-request URL",
	)
}

func (e *Engine) createPullRequest(
	ctx context.Context,
	publication validatedPublication,
	remote publicationRemote,
	branch string,
) (string, error) {
	output, err := e.command(ctx, publication.repository, "create pull request", "gh",
		"pr", "create",
		"--repo", remote.repositorySelector,
		"--base", publication.target,
		"--head", branch,
		"--title", pullRequestTitle(publication),
		"--body", pullRequestBody(publication),
	)
	if err == nil {
		return createdPullRequestURL(output.stdout, remote)
	}
	if controlErr := commandControlError(err); controlErr != nil {
		return "", controlErr
	}
	records, listErr := e.listPullRequests(ctx, publication, remote, branch)
	if listErr != nil {
		return "", err
	}
	existing, reuseErr := pullRequestURL(records, publication.head, remote)
	if reuseErr != nil {
		return "", reuseErr
	}
	if existing != nil {
		return *existing, nil
	}
	return "", err
}

func (e *Engine) publishPullRequest(
	ctx context.Context,
	publication validatedPublication,
	result Result,
) (Result, error) {
	if publication.targetContainsHead {
		result.Status = ResultAlreadyPublished
		result.Message = "target branch already contains the captured head commit"
		return result, nil
	}
	remote, err := e.configuredRemote(ctx, publication)
	if err != nil {
		return result, err
	}
	branch, err := e.pullRequestBranch(ctx, publication)
	if err != nil {
		return result, err
	}
	result.Branch = branch
	branchExisted, err := e.ensurePullRequestBranch(
		ctx, publication, remote, branch,
	)
	if err != nil {
		return result, err
	}
	records, err := e.listPullRequests(ctx, publication, remote, branch)
	if err != nil {
		return result, err
	}
	existing, err := pullRequestURL(records, publication.head, remote)
	if err != nil {
		return result, err
	}
	if existing != nil {
		result.Status = ResultAlreadyPublished
		result.URL = existing
		result.Message = "pull request already exists for the captured head commit"
		return result, nil
	}
	url, err := e.createPullRequest(ctx, publication, remote, branch)
	if err != nil {
		return result, err
	}
	result.Status = ResultPublished
	result.URL = &url
	if branchExisted {
		result.Message = "existing publication branch reused and pull request created"
	} else {
		result.Message = "publication branch pushed and pull request created"
	}
	return result, nil
}
