package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nn1a/autogora/internal/githubissues"
)

const githubHelp = `autogora github import [options]

Fetches issues through the authenticated GitHub CLI and creates idempotent
Triage tasks. The source issue URL is kept in the task body and attachments.

Options:
  --repo <repository>   OWNER/REPO or HOST/OWNER/REPO; defaults to the Git repo
  --host <hostname>     GitHub Enterprise host used with OWNER/REPO
  --issue <number>      Import one issue; repeatable
  --state <state>       open (default), closed, or all
  --label <label>       Filter issue list by label; repeatable
  --search <query>      GitHub issue search query
  --limit <number>      Maximum list size, 1-1000 (default: 30)
  --tenant <tenant>     Tenant assigned to imported tasks
  --priority <number>   Priority assigned to imported tasks
  --board <slug>        Destination board
  --db <path>           Override the project-specific SQLite path
  --dry-run             Fetch and show issues without creating tasks

GitHub Enterprise examples:
  autogora github import --host github.corp.example --repo team/service
  autogora github import --repo github.corp.example/team/service --issue 42

Authentication and TLS settings come from gh. Configure each host first with
"gh auth login --hostname HOST" or the corresponding GH_* token variables.
`

func rejectGitHubOptions(opts options) error {
	allowedValues := map[string]bool{
		"repo": true, "host": true, "issue": true, "state": true, "label": true,
		"search": true, "limit": true, "tenant": true, "priority": true,
		"board": true, "db": true,
	}
	for name := range opts.values {
		if !allowedValues[name] {
			return fmt.Errorf("unknown github import option --%s", name)
		}
	}
	for name := range opts.flagSet {
		if name != "dry-run" {
			return fmt.Errorf("unknown github import option --%s", name)
		}
	}
	for name, values := range opts.values {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("--%s cannot be empty", name)
			}
		}
	}
	return nil
}

func (a *App) runGitHub(ctx context.Context, opts options) error {
	if len(opts.positionals) == 0 {
		return errors.New("github requires the import action")
	}
	if strings.ToLower(strings.TrimSpace(opts.positionals[0])) != "import" || len(opts.positionals) > 1 {
		return errors.New("github supports: autogora github import [options]")
	}
	if err := rejectGitHubOptions(opts); err != nil {
		return err
	}
	if len(opts.many("issue")) > 0 && (opts.present("state") || opts.present("label") || opts.present("search")) {
		return errors.New("--issue cannot be combined with --state, --label, or --search")
	}
	numbers := make([]int, 0, len(opts.many("issue")))
	for _, raw := range opts.many("issue") {
		number, err := strconv.Atoi(raw)
		if err != nil || number < 1 {
			return fmt.Errorf("invalid issue number: %s", raw)
		}
		numbers = append(numbers, number)
	}
	limit, err := numberOption(opts.value("limit"), 30)
	if err != nil {
		return err
	}
	priority, err := numberOption(opts.value("priority"), 0)
	if err != nil {
		return err
	}
	directory, err := a.workingDirectory()
	if err != nil {
		return err
	}
	opened, _, _, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()
	runner := a.CommandRunner
	if runner == nil {
		runner = githubissues.ExecRunner{}
	}
	result, err := (githubissues.Importer{Store: opened, Runner: runner, Directory: directory}).Import(ctx, githubissues.ImportOptions{
		Repository: opts.value("repo"), Host: opts.value("host"), State: opts.value("state"),
		Labels: opts.many("label"), Search: opts.value("search"), Limit: limit, Numbers: numbers,
		Tenant: stringPointer(opts.value("tenant")), Priority: priority, DryRun: opts.flags["dry-run"],
	})
	if err != nil && !githubissues.IsPartialImportError(err) {
		return err
	}
	if writeErr := writeJSON(a.Stdout, result); writeErr != nil {
		if err != nil {
			return errors.Join(err, writeErr)
		}
		return writeErr
	}
	return err
}
