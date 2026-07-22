package githubissues

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	setupcfg "github.com/nn1a/autogora/internal/setup"
	"github.com/nn1a/autogora/internal/store"
)

type runnerCall struct {
	Directory string
	File      string
	Args      []string
}

type fakeRunner struct {
	path    string
	outputs []setupcfg.CommandOutput
	err     error
	calls   []runnerCall
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if f.path == "" {
		return "", errors.New("missing")
	}
	return f.path, nil
}

func (f *fakeRunner) Run(_ context.Context, directory, file string, args ...string) (setupcfg.CommandOutput, error) {
	f.calls = append(f.calls, runnerCall{Directory: directory, File: file, Args: append([]string{}, args...)})
	if len(f.outputs) == 0 {
		return setupcfg.CommandOutput{}, f.err
	}
	output := f.outputs[0]
	f.outputs = f.outputs[1:]
	return output, f.err
}

const issueList = `[{"id":"I_kwDOE_transfer_safe","number":42,"title":"Retry failed uploads","body":"Uploads should retry twice.","url":"https://ghe.example.com/acme/platform/issues/42","state":"OPEN","labels":[{"name":"bug"}],"assignees":[{"login":"octo"}],"author":{"login":"reporter"},"createdAt":"2026-07-01T00:00:00Z","updatedAt":"2026-07-02T00:00:00Z"}]`

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	root := t.TempDir()
	opened, err := store.Open(filepath.Join(root, "autogora.db"), "default", filepath.Join(root, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened
}

func TestEnterpriseImportIsIdempotentAndKeepsSource(t *testing.T) {
	ctx := context.Background()
	opened := openTestStore(t)
	runner := &fakeRunner{path: "/usr/bin/gh", outputs: []setupcfg.CommandOutput{{Stdout: issueList}, {Stdout: issueList}}}
	importer := Importer{Store: opened, Runner: runner, Directory: "/work/project"}
	options := ImportOptions{Repository: "acme/platform", Host: "ghe.example.com", Labels: []string{"bug"}, Search: "no:assignee", Limit: 50, Tenant: stringPointer("platform"), Priority: 7}

	first, err := importer.Import(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := importer.Import(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || first.Existing != 0 || first.Failed != 0 || second.Created != 0 || second.Existing != 1 || second.Failed != 0 {
		t.Fatalf("unexpected import results: first=%#v second=%#v", first, second)
	}
	if len(runner.calls) != 2 || runner.calls[0].Directory != "/work/project" || !slices.Contains(runner.calls[0].Args, "ghe.example.com/acme/platform") {
		t.Fatalf("enterprise repository was not passed to gh: %#v", runner.calls)
	}
	if !slices.Contains(runner.calls[0].Args, "--label") || !slices.Contains(runner.calls[0].Args, "--search") {
		t.Fatalf("issue filters were not passed to gh: %#v", runner.calls[0].Args)
	}
	tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("imported tasks: %#v, %v", tasks, err)
	}
	detail, err := opened.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != "triage" || detail.Task.Priority != 7 || detail.Task.Tenant == nil || *detail.Task.Tenant != "platform" {
		t.Fatalf("triage mapping mismatch: %#v", detail.Task)
	}
	if !strings.Contains(detail.Task.Body, "Uploads should retry twice") || !strings.Contains(detail.Task.Body, "ghe.example.com/acme/platform#42") {
		t.Fatalf("source context missing from body: %s", detail.Task.Body)
	}
	if len(detail.Attachments) != 1 || detail.Attachments[0].URL == nil || *detail.Attachments[0].URL != "https://ghe.example.com/acme/platform/issues/42" {
		t.Fatalf("source attachment mismatch: %#v", detail.Attachments)
	}
}

func TestIssueNumberFetchUsesViewAndDryRunDoesNotWrite(t *testing.T) {
	opened := openTestStore(t)
	runner := &fakeRunner{path: "gh", outputs: []setupcfg.CommandOutput{{Stdout: strings.Trim(issueList, "[]")}}}
	result, err := (Importer{Store: opened, Runner: runner}).Import(context.Background(), ImportOptions{
		Repository: "https://github.example.net/team/repo.git", Numbers: []int{42}, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fetched != 1 || len(result.Issues) != 1 || result.Issues[0].TaskID != "" {
		t.Fatalf("dry-run result: %#v", result)
	}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0].Args[:3], []string{"issue", "view", "42"}) || !slices.Contains(runner.calls[0].Args, "github.example.net/team/repo") {
		t.Fatalf("issue view command mismatch: %#v", runner.calls)
	}
	tasks, _ := opened.ListTasks(context.Background(), store.ListTaskFilter{IncludeArchived: true})
	if len(tasks) != 0 {
		t.Fatalf("dry run created tasks: %#v", tasks)
	}
}

func TestImportValidationAndMissingCLI(t *testing.T) {
	importer := Importer{Runner: &fakeRunner{}}
	if _, err := importer.Import(context.Background(), ImportOptions{Repository: "owner/repo", Host: "https://ghe.example.com/path"}); err == nil || !strings.Contains(err.Error(), "invalid GitHub host") {
		t.Fatalf("invalid host error = %v", err)
	}
	if _, err := importer.Import(context.Background(), ImportOptions{Repository: "owner/repo", Numbers: []int{1}, Labels: []string{"bug"}}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed filters error = %v", err)
	}
	if _, err := importer.Import(context.Background(), ImportOptions{Repository: "owner/repo"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing gh error = %v", err)
	}
}

func stringPointer(value string) *string { return &value }
