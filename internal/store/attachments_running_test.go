package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func runningAttachmentTask(
	t *testing.T,
) (*Store, model.TaskDetail, *model.ClaimedTask, string) {
	t.Helper()
	root := t.TempDir()
	attachmentsRoot := filepath.Join(root, "attachments")
	opened, err := Open(
		filepath.Join(root, "autogora.db"),
		"default",
		attachmentsRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	assignee := "attachment-worker"
	task, err := opened.CreateTask(context.Background(), CreateTaskInput{
		Title:    "stable running attachments",
		Assignee: &assignee,
		Runtime:  model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%v err=%v", claim, err)
	}
	return opened, task, claim, attachmentsRoot
}

func TestRunningTaskRejectsPublicAttachmentAddsAndCleansCopiedFile(t *testing.T) {
	opened, task, _, attachmentsRoot := runningAttachmentTask(t)
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "evidence.txt")
	if err := os.WriteFile(source, []byte("must be cleaned"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := opened.AttachFile(ctx, task.Task.ID, source, "evidence.txt"); err == nil ||
		!strings.Contains(err.Error(), "while running") {
		t.Fatalf("running file attachment error = %v", err)
	}
	taskDirectory := filepath.Join(attachmentsRoot, task.Task.ID)
	entries, err := os.ReadDir(taskDirectory)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected file attachment left files: %+v", entries)
	}

	if _, err := opened.AttachURL(
		ctx,
		task.Task.ID,
		"https://example.com/new-requirement",
		"new requirement",
	); err == nil || !strings.Contains(err.Error(), "while running") {
		t.Fatalf("running URL attachment error = %v", err)
	}
	var attachments, events int
	if err := opened.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_attachments WHERE task_id = ?",
		task.Task.ID,
	).Scan(&attachments); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND kind = 'attached'`, task.Task.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if attachments != 0 || events != 0 {
		t.Fatalf("rejected attachment mutated store: attachments=%d events=%d", attachments, events)
	}
}

func TestRunningTaskRejectsAttachmentRemovalAndExternalSourceMutation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opened, err := Open(
		filepath.Join(root, "autogora.db"),
		"default",
		filepath.Join(root, "attachments"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "attachment-worker"
	key := "external:running-attachment-test"
	task, created, err := opened.CreateTaskWithURLSource(ctx, CreateTaskInput{
		Title:          "stable external source",
		Assignee:       &assignee,
		Runtime:        model.RuntimeCodex,
		IdempotencyKey: &key,
	}, "https://example.com/original", "original")
	if err != nil || !created || len(task.Attachments) != 1 {
		t.Fatalf("create sourced task: task=%+v created=%t err=%v", task, created, err)
	}
	attachment := task.Attachments[0]
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%v err=%v", claim, err)
	}

	if err := opened.RemoveAttachment(ctx, task.Task.ID, attachment.ID); err == nil ||
		!strings.Contains(err.Error(), "while running") {
		t.Fatalf("running attachment removal error = %v", err)
	}
	if _, created, err := opened.CreateTaskWithURLSource(ctx, CreateTaskInput{
		Title:          "ignored duplicate import",
		IdempotencyKey: &key,
	}, "https://example.com/new-source", "new source"); err == nil ||
		created || !strings.Contains(err.Error(), "while running") {
		t.Fatalf("running external-source mutation: created=%t err=%v", created, err)
	}
	// Repeating the already-recorded source is read-only and remains
	// idempotent even while the worker owns the task.
	existing, created, err := opened.CreateTaskWithURLSource(ctx, CreateTaskInput{
		Title:          "ignored duplicate import",
		IdempotencyKey: &key,
	}, "https://example.com/original", "original")
	if err != nil || created || len(existing.Attachments) != 1 {
		t.Fatalf("idempotent existing source: task=%+v created=%t err=%v", existing, created, err)
	}
	after, err := opened.ListAttachments(ctx, task.Task.ID)
	if err != nil || len(after) != 1 || after[0].ID != attachment.ID {
		t.Fatalf("running attachments changed: %+v err=%v", after, err)
	}
}

func TestCompletionArtifactCaptureRemainsAllowedForRunningTask(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspacePath := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "result.txt"), []byte("verified result"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(
		filepath.Join(root, "autogora.db"),
		"default",
		filepath.Join(root, "attachments"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "artifact-worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:         "capture completion artifact",
		Assignee:      &assignee,
		Runtime:       model.RuntimeCodex,
		Workspace:     &workspacePath,
		WorkspaceKind: model.WorkspaceDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%v err=%v", claim, err)
	}
	completed, err := opened.CompleteRun(
		ctx,
		RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		CompletionInput{Summary: "complete", Artifacts: []string{"result.txt"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone ||
		len(completed.Attachments) != 1 ||
		completed.Attachments[0].Name != "result.txt" {
		t.Fatalf("completion artifact was not captured: %+v", completed)
	}
}
