package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	setupcfg "github.com/nn1a/autogora/internal/setup"
	"github.com/nn1a/autogora/internal/store"
)

const issueJSONFields = "id,number,title,body,url,state,labels,assignees,author,createdAt,updatedAt"

type CommandOutput = setupcfg.CommandOutput
type CommandRunner = setupcfg.CommandRunner
type ExecRunner = setupcfg.ExecRunner

type Account struct {
	Login string `json:"login"`
}

type Label struct {
	Name string `json:"name"`
}

type Issue struct {
	ID        string    `json:"id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	State     string    `json:"state"`
	Labels    []Label   `json:"labels"`
	Assignees []Account `json:"assignees"`
	Author    *Account  `json:"author"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
}

type ImportOptions struct {
	Repository string
	Host       string
	State      string
	Labels     []string
	Search     string
	Limit      int
	Numbers    []int
	Tenant     *string
	Priority   int
	DryRun     bool
}

type ImportedIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	TaskID  string `json:"taskId,omitempty"`
	Created bool   `json:"created"`
	Action  string `json:"action"`
}

type ImportFailure struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Error  string `json:"error"`
}

type ImportResult struct {
	Board      string          `json:"board,omitempty"`
	Repository string          `json:"repository,omitempty"`
	DryRun     bool            `json:"dryRun"`
	Fetched    int             `json:"fetched"`
	Created    int             `json:"created"`
	Existing   int             `json:"existing"`
	Failed     int             `json:"failed"`
	Planned    int             `json:"planned,omitempty"`
	Issues     []ImportedIssue `json:"issues"`
	Errors     []ImportFailure `json:"errors"`
}

type Importer struct {
	Store     *store.Store
	Runner    CommandRunner
	Directory string
}

func normalizeHost(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid GitHub host %q: use a hostname such as github.example.com", value)
	}
	return strings.ToLower(parsed.Host), nil
}

func normalizeRepository(repository, host string) (string, error) {
	repository = strings.TrimSpace(strings.TrimSuffix(repository, ".git"))
	normalizedHost, err := normalizeHost(host)
	if err != nil {
		return "", err
	}
	if strings.Contains(repository, "://") {
		parsed, parseErr := url.Parse(repository)
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if parseErr != nil || parsed.Host == "" || len(parts) != 2 {
			return "", fmt.Errorf("invalid GitHub repository %q", repository)
		}
		repository = strings.ToLower(parsed.Host) + "/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	}
	if repository == "" {
		if normalizedHost != "" {
			return "", errors.New("--host requires --repo owner/repository")
		}
		return "", nil
	}
	parts := strings.Split(strings.Trim(repository, "/"), "/")
	if len(parts) != 2 && len(parts) != 3 {
		return "", fmt.Errorf("invalid GitHub repository %q: use OWNER/REPO or HOST/OWNER/REPO", repository)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || strings.ContainsAny(part, " \t\r\n") {
			return "", fmt.Errorf("invalid GitHub repository %q", repository)
		}
	}
	if normalizedHost != "" {
		if len(parts) == 2 {
			parts = append([]string{normalizedHost}, parts...)
		} else if !strings.EqualFold(parts[0], normalizedHost) {
			return "", fmt.Errorf("repository host %q conflicts with --host %q", parts[0], normalizedHost)
		}
	}
	return strings.Join(parts, "/"), nil
}

func normalizeOptions(options ImportOptions) (ImportOptions, error) {
	repository, err := normalizeRepository(options.Repository, options.Host)
	if err != nil {
		return ImportOptions{}, err
	}
	options.Repository = repository
	options.State = strings.ToLower(strings.TrimSpace(options.State))
	if options.State == "" {
		options.State = "open"
	}
	if options.State != "open" && options.State != "closed" && options.State != "all" {
		return ImportOptions{}, fmt.Errorf("invalid issue state %q: use open, closed, or all", options.State)
	}
	if options.Limit == 0 {
		options.Limit = 30
	}
	if options.Limit < 1 || options.Limit > 1000 {
		return ImportOptions{}, errors.New("issue import limit must be between 1 and 1000")
	}
	seenNumbers := map[int]bool{}
	numbers := make([]int, 0, len(options.Numbers))
	for _, number := range options.Numbers {
		if number < 1 {
			return ImportOptions{}, errors.New("issue numbers must be positive")
		}
		if !seenNumbers[number] {
			seenNumbers[number] = true
			numbers = append(numbers, number)
		}
	}
	options.Numbers = numbers
	if len(numbers) > 0 && (len(options.Labels) > 0 || strings.TrimSpace(options.Search) != "" || options.State != "open") {
		return ImportOptions{}, errors.New("--issue cannot be combined with --label, --search, or --state")
	}
	return options, nil
}

func ghError(output CommandOutput, err error) error {
	detail := strings.TrimSpace(output.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(output.Stdout)
	}
	if detail == "" {
		return fmt.Errorf("GitHub CLI failed: %w", err)
	}
	return fmt.Errorf("GitHub CLI failed: %s", detail)
}

func (i Importer) fetch(ctx context.Context, options ImportOptions) ([]Issue, error) {
	runner := i.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	gh, err := runner.LookPath("gh")
	if err != nil {
		return nil, errors.New("GitHub CLI (gh) was not found on PATH")
	}
	directory := i.Directory
	if directory == "" {
		directory = "."
	}
	repositoryArgs := []string{}
	if options.Repository != "" {
		repositoryArgs = []string{"--repo", options.Repository}
	}
	issues := []Issue{}
	if len(options.Numbers) > 0 {
		for _, number := range options.Numbers {
			args := []string{"issue", "view", strconv.Itoa(number), "--json", issueJSONFields}
			args = append(args, repositoryArgs...)
			output, runErr := runner.Run(ctx, directory, gh, args...)
			if runErr != nil {
				return nil, ghError(output, runErr)
			}
			var issue Issue
			if err := json.Unmarshal([]byte(output.Stdout), &issue); err != nil {
				return nil, fmt.Errorf("decode gh issue view output: %w", err)
			}
			issues = append(issues, issue)
		}
	} else {
		args := []string{"issue", "list", "--json", issueJSONFields, "--limit", strconv.Itoa(options.Limit), "--state", options.State}
		for _, label := range options.Labels {
			if label = strings.TrimSpace(label); label != "" {
				args = append(args, "--label", label)
			}
		}
		if search := strings.TrimSpace(options.Search); search != "" {
			args = append(args, "--search", search)
		}
		args = append(args, repositoryArgs...)
		output, runErr := runner.Run(ctx, directory, gh, args...)
		if runErr != nil {
			return nil, ghError(output, runErr)
		}
		if err := json.Unmarshal([]byte(output.Stdout), &issues); err != nil {
			return nil, fmt.Errorf("decode gh issue list output: %w", err)
		}
	}
	sort.SliceStable(issues, func(left, right int) bool { return issues[left].Number < issues[right].Number })
	return issues, nil
}

func sourceRepository(issue Issue) string {
	parsed, err := url.Parse(issue.URL)
	if err != nil || parsed.Host == "" {
		return "GitHub"
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return parsed.Host
	}
	return parsed.Host + "/" + parts[0] + "/" + parts[1]
}

func issueKey(issue Issue) (string, error) {
	if id := strings.TrimSpace(issue.ID); id != "" {
		return "github-issue:" + id, nil
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(issue.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("issue #%d has an invalid URL", issue.Number)
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery, parsed.Fragment = "", ""
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return "github-issue:" + parsed.String(), nil
}

func issueBody(issue Issue) string {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if value := strings.TrimSpace(label.Name); value != "" {
			labels = append(labels, value)
		}
	}
	assignees := make([]string, 0, len(issue.Assignees))
	for _, assignee := range issue.Assignees {
		if value := strings.TrimSpace(assignee.Login); value != "" {
			assignees = append(assignees, "@"+value)
		}
	}
	metadata := []string{
		fmt.Sprintf("- Source: [%s#%d](%s)", sourceRepository(issue), issue.Number, issue.URL),
		"- State: " + strings.ToLower(issue.State),
	}
	if issue.Author != nil && strings.TrimSpace(issue.Author.Login) != "" {
		metadata = append(metadata, "- Author: @"+issue.Author.Login)
	}
	if len(labels) > 0 {
		metadata = append(metadata, "- Labels: "+strings.Join(labels, ", "))
	}
	if len(assignees) > 0 {
		metadata = append(metadata, "- GitHub assignees: "+strings.Join(assignees, ", "))
	}
	if issue.UpdatedAt != "" {
		metadata = append(metadata, "- GitHub updated: "+issue.UpdatedAt)
	}
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(No issue description was provided.)"
	}
	return body + "\n\n---\n\n## Imported issue\n\n" + strings.Join(metadata, "\n")
}

func hasSourceAttachment(detail model.TaskDetail, rawURL string) bool {
	for _, attachment := range detail.Attachments {
		if attachment.URL != nil && *attachment.URL == rawURL {
			return true
		}
	}
	return false
}

func (i Importer) Import(ctx context.Context, options ImportOptions) (ImportResult, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return ImportResult{}, err
	}
	issues, err := i.fetch(ctx, options)
	if err != nil {
		return ImportResult{}, err
	}
	board := ""
	if i.Store != nil {
		board = i.Store.Board()
	}
	result := ImportResult{Board: board, Repository: options.Repository, DryRun: options.DryRun, Fetched: len(issues), Issues: []ImportedIssue{}, Errors: []ImportFailure{}}
	if i.Store == nil {
		return ImportResult{}, errors.New("GitHub issue importer requires a task store")
	}
	for _, issue := range issues {
		entry := ImportedIssue{Number: issue.Number, Title: issue.Title, URL: issue.URL}
		key, keyErr := issueKey(issue)
		if keyErr != nil {
			result.Errors = append(result.Errors, ImportFailure{Number: issue.Number, URL: issue.URL, Error: keyErr.Error()})
			continue
		}
		if options.DryRun {
			existing, findErr := i.Store.FindTaskByIdempotencyKey(ctx, key)
			if findErr != nil {
				result.Errors = append(result.Errors, ImportFailure{Number: issue.Number, URL: issue.URL, Error: findErr.Error()})
				continue
			}
			if existing == nil {
				entry.Action = "create"
				result.Planned++
			} else {
				entry.Action, entry.TaskID = "existing", existing.Task.ID
				result.Existing++
			}
			result.Issues = append(result.Issues, entry)
			continue
		}
		title := strings.TrimSpace(issue.Title)
		if title == "" {
			title = fmt.Sprintf("GitHub issue #%d", issue.Number)
		}
		detail, created, createErr := i.Store.CreateTaskWithDisposition(ctx, store.CreateTaskInput{
			Title: title, Body: issueBody(issue), Tenant: options.Tenant, IdempotencyKey: &key,
			Status: model.TaskStatusTriage, Runtime: model.RuntimeManual, Priority: options.Priority,
		})
		err = createErr
		if err == nil && !hasSourceAttachment(detail, issue.URL) {
			_, err = i.Store.AttachURL(ctx, detail.Task.ID, issue.URL, fmt.Sprintf("GitHub issue #%d", issue.Number))
		}
		if err != nil {
			result.Errors = append(result.Errors, ImportFailure{Number: issue.Number, URL: issue.URL, Error: err.Error()})
			continue
		}
		entry.TaskID, entry.Created = detail.Task.ID, created
		result.Issues = append(result.Issues, entry)
		if created {
			entry.Action = "created"
			result.Created++
		} else {
			entry.Action = "existing"
			result.Existing++
		}
		result.Issues[len(result.Issues)-1] = entry
	}
	result.Failed = len(result.Errors)
	return result, nil
}

func WorkingDirectory(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return filepath.Abs(".")
	}
	return filepath.Abs(value)
}
