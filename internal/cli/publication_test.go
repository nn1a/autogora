package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func publicationCLIApp(t *testing.T) (string, *App) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)
	return dbPath, app
}

func createCLIPublication(
	t *testing.T,
	dbPath, board, suffix string,
	mode model.PublicationMode,
	requireApproval bool,
) model.Publication {
	t.Helper()
	ctx := context.Background()
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "CLI publication " + suffix, Board: board,
		Runtime: model.RuntimeManual, Status: model.TaskStatusReady,
		WorkflowRole: model.WorkflowRoleFinalizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID, Board: board, WorkerID: "cli-publication-test",
		ClaimTTLSeconds: 300,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim publication source: value=%+v err=%v", claimed, err)
	}
	scope := store.RunScope{
		RunID: claimed.Run.ID, ClaimToken: claimed.ClaimToken,
	}
	if _, err := opened.RequestRunCompletion(ctx, scope, store.CompletionInput{
		Summary: "publication source ready",
	}); err != nil {
		t.Fatal(err)
	}
	changeSet, err := opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
		RunID: claimed.Run.ID, RepositoryPath: "/repo/" + suffix,
		WorktreePath: "/worktree/" + suffix, BaseCommit: "base-" + suffix,
		HeadCommit: "head-" + suffix,
		DurableRef: "refs/autogora/runs/" + claimed.Run.ID,
		State:      "ready", ChangedFiles: []string{suffix + ".go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinalizeRunTerminal(ctx, scope, 0); err != nil {
		t.Fatal(err)
	}
	value, created, err := opened.EnsurePublication(ctx, store.EnsurePublicationInput{
		Board: board, ChangeSetID: changeSet.ID, Mode: mode,
		TargetBranch: "main", Remote: "origin", RequireApproval: requireApproval,
	})
	if err != nil || !created {
		t.Fatalf("ensure publication: value=%+v created=%v err=%v", value, created, err)
	}
	return value
}

func failCLIPublication(
	t *testing.T,
	dbPath, board string,
	value model.Publication,
) model.Publication {
	t.Helper()
	ctx := context.Background()
	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	current := time.Now().UTC()
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		value.ID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: value.UpdatedAt, TTL: time.Minute, Current: current,
		},
	)
	if err != nil || !acquired {
		t.Fatalf("claim publication: value=%+v acquired=%v err=%v", claimed, acquired, err)
	}
	failed, err := opened.FailPublication(ctx, value.ID, store.FailPublicationInput{
		ExpectedUpdatedAt: claimed.UpdatedAt, ClaimToken: claimed.ClaimToken,
		Current: current.Add(time.Second), Error: "temporary CLI publication failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	return failed
}

func decodePublicationMutationOutput(
	t *testing.T,
	output string,
) publicationMutationOutput {
	t.Helper()
	var value publicationMutationOutput
	if err := json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatalf("decode publication mutation output %q: %v", output, err)
	}
	return value
}

func TestPublicationCLIInspectsAndFiltersHandoffs(t *testing.T) {
	dbPath, app := publicationCLIApp(t)
	first := createCLIPublication(
		t, dbPath, "default", "inspect-first", model.PublicationModePullRequest, false,
	)
	createCLIPublication(
		t, dbPath, "default", "inspect-second", model.PublicationModeManual, true,
	)

	status := runApp(t, app, "publication", "status", "--db", dbPath, "--limit", "10")
	if !strings.Contains(status, `"policy"`) ||
		!strings.Contains(status, `"statusCounts"`) ||
		!strings.Contains(status, first.ID) ||
		strings.Contains(status, "claimToken") {
		t.Fatalf("unexpected publication status: %s", status)
	}
	listed := runApp(
		t,
		app,
		"publication",
		"list",
		"--db",
		dbPath,
		"--status",
		"pending",
		"--task",
		first.TaskID,
		"--limit",
		"1",
	)
	var values []model.Publication
	if err := json.Unmarshal([]byte(listed), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != first.ID {
		t.Fatalf("filtered publication list = %+v", values)
	}
	shown := runApp(t, app, "publication", "show", first.ID, "--db", dbPath)
	if !strings.Contains(shown, first.ChangeSetID) ||
		strings.Contains(shown, "claimToken") {
		t.Fatalf("unexpected publication show output: %s", shown)
	}

	if err := app.Run(context.Background(), []string{
		"publication", "list", "--db", dbPath, "--status", "unknown",
	}); err == nil || !strings.Contains(err.Error(), "invalid publication status") {
		t.Fatalf("invalid status error = %v", err)
	}
	if err := app.Run(context.Background(), []string{
		"publication", "list", "--db", dbPath, "--limit", "501",
	}); err == nil || !strings.Contains(err.Error(), "between 1 and 500") {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestPublicationCLIMutationsUseUpdatedAtCAS(t *testing.T) {
	dbPath, app := publicationCLIApp(t)

	awaiting := createCLIPublication(
		t, dbPath, "default", "approve", model.PublicationModePullRequest, true,
	)
	approvedOutput := runApp(
		t,
		app,
		"publication",
		"approve",
		awaiting.ID,
		"--db",
		dbPath,
		"--updated-at",
		awaiting.UpdatedAt,
	)
	approved := decodePublicationMutationOutput(t, approvedOutput)
	if approved.Action != "approve" || approved.Outcome != "approved" ||
		approved.Publication.Status != model.PublicationPending ||
		approved.Publication.ApprovedAt == nil ||
		strings.Contains(approvedOutput, "claimToken") {
		t.Fatalf("approved publication = %+v", approved)
	}
	if err := app.Run(context.Background(), []string{
		"publication", "approve", awaiting.ID, "--db", dbPath,
		"--updated-at", awaiting.UpdatedAt,
	}); err == nil || !strings.Contains(err.Error(), "publication approve conflict") {
		t.Fatalf("stale approval error = %v", err)
	}

	rejected := createCLIPublication(
		t, dbPath, "default", "reject", model.PublicationModeLocalFF, true,
	)
	rejectedOutput := runApp(
		t,
		app,
		"publication",
		"reject",
		rejected.ID,
		"--db",
		dbPath,
		"--updated-at",
		rejected.UpdatedAt,
		"--reason",
		"operator rejected target",
	)
	rejectedValue := decodePublicationMutationOutput(t, rejectedOutput)
	if rejectedValue.Outcome != "superseded" ||
		rejectedValue.Publication.Status != model.PublicationSuperseded ||
		rejectedValue.Publication.Error == nil ||
		*rejectedValue.Publication.Error != "operator rejected target" {
		t.Fatalf("rejected publication = %+v", rejectedValue)
	}

	failed := failCLIPublication(t, dbPath, "default", createCLIPublication(
		t, dbPath, "default", "retry", model.PublicationModePullRequest, false,
	))
	retriedOutput := runApp(
		t,
		app,
		"publication",
		"retry",
		failed.ID,
		"--db",
		dbPath,
		"--updated-at",
		failed.UpdatedAt,
	)
	retried := decodePublicationMutationOutput(t, retriedOutput)
	if retried.Outcome != "pending" ||
		retried.Publication.Status != model.PublicationPending ||
		retried.Publication.Error != nil {
		t.Fatalf("retried publication = %+v", retried)
	}

	manual := createCLIPublication(
		t, dbPath, "default", "complete", model.PublicationModeManual, false,
	)
	completedOutput := runApp(
		t,
		app,
		"publication",
		"complete",
		manual.ID,
		"--db",
		dbPath,
		"--updated-at",
		manual.UpdatedAt,
		"--url",
		"https://example.test/releases/manual",
	)
	completed := decodePublicationMutationOutput(t, completedOutput)
	if completed.Outcome != "published" ||
		completed.Publication.Status != model.PublicationPublished ||
		completed.Publication.URL == nil ||
		*completed.Publication.URL != "https://example.test/releases/manual" {
		t.Fatalf("completed publication = %+v", completed)
	}
}

func TestPublicationCLIRejectsClaimTokensMissingVersionsAndWrongBoard(t *testing.T) {
	dbPath, app := publicationCLIApp(t)
	value := createCLIPublication(
		t, dbPath, "default", "strict", model.PublicationModeManual, true,
	)
	if err := app.Run(context.Background(), []string{
		"publication", "approve", value.ID, "--db", dbPath,
		"--claim-token", "internal",
		"--updated-at", value.UpdatedAt,
	}); err == nil || !strings.Contains(err.Error(), "do not accept claim tokens") {
		t.Fatalf("claim token error = %v", err)
	}
	if err := app.Run(context.Background(), []string{
		"publication", "approve", value.ID, "--db", dbPath,
	}); err == nil || !strings.Contains(err.Error(), "requires --updated-at") {
		t.Fatalf("missing version error = %v", err)
	}

	manager, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "other", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), []string{
		"publication", "show", value.ID, "--db", dbPath, "--board", "other",
	}); err == nil || !strings.Contains(err.Error(), "publication not found") {
		t.Fatalf("cross-board show error = %v", err)
	}
}

func TestPublicationCLIHelpDescribesHumanControls(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	for _, args := range [][]string{
		{"publication", "--help"},
		{"help", "publication"},
	} {
		output := runApp(t, app, args...)
		if !strings.HasPrefix(output, "autogora publication") ||
			!strings.Contains(output, "approve <id>") ||
			!strings.Contains(output, "--updated-at") ||
			!strings.Contains(output, "claim tokens") {
			t.Fatalf("publication help output = %q", output)
		}
	}
}
