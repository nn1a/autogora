package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type terminalArtifactFixture struct {
	store           *Store
	task            model.TaskDetail
	claim           *model.ClaimedTask
	workspace       string
	attachmentsRoot string
}

func newTerminalArtifactFixture(
	t *testing.T,
	artifacts []string,
) terminalArtifactFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
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
	assignee := "artifact-worker"
	task, err := opened.CreateTask(ctx, CreateTaskInput{
		Title:         "idempotent terminal artifacts",
		Assignee:      &assignee,
		Runtime:       model.RuntimeCodex,
		Workspace:     &workspace,
		WorkspaceKind: model.WorkspaceDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%v err=%v", claim, err)
	}
	if _, err := opened.RequestRunCompletion(
		ctx,
		RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		CompletionInput{Summary: "captured", Artifacts: artifacts},
	); err != nil {
		t.Fatal(err)
	}
	return terminalArtifactFixture{
		store:           opened,
		task:            task,
		claim:           claim,
		workspace:       workspace,
		attachmentsRoot: attachmentsRoot,
	}
}

func attachmentMutationCounts(
	t *testing.T,
	fixture terminalArtifactFixture,
) (attachments, events int) {
	t.Helper()
	ctx := context.Background()
	if err := fixture.store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM task_attachments WHERE task_id = ?",
		fixture.task.Task.ID,
	).Scan(&attachments); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM task_events
		WHERE task_id = ? AND kind = 'attached'`,
		fixture.task.Task.ID,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return attachments, events
}

func terminalArtifactFiles(
	t *testing.T,
	fixture terminalArtifactFixture,
) []string {
	t.Helper()
	directory := filepath.Join(
		fixture.attachmentsRoot,
		fixture.task.Task.ID,
	)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func terminalArtifactScope(fixture terminalArtifactFixture) RunScope {
	return RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
}

func TestTerminalArtifactIdentityIsRequestAndPositionScoped(t *testing.T) {
	first := terminalArtifactID(
		"run-one",
		"2026-07-24T10:00:00Z",
		0,
		"reports/../result.txt",
	)
	if repeated := terminalArtifactID(
		"run-one",
		"2026-07-24T10:00:00Z",
		0,
		"result.txt",
	); repeated != first {
		t.Fatalf("cleaned artifact identity changed: %s != %s", repeated, first)
	}
	for label, other := range map[string]string{
		"run": terminalArtifactID(
			"run-two",
			"2026-07-24T10:00:00Z",
			0,
			"result.txt",
		),
		"request": terminalArtifactID(
			"run-one",
			"2026-07-24T10:00:01Z",
			0,
			"result.txt",
		),
		"position": terminalArtifactID(
			"run-one",
			"2026-07-24T10:00:00Z",
			1,
			"result.txt",
		),
	} {
		if other == first {
			t.Fatalf("%s did not scope terminal artifact identity", label)
		}
	}
}

func TestFinalizeRunTerminalArtifactRollbackRetryAndResponseLossAreIdempotent(
	t *testing.T,
) {
	ctx := context.Background()
	fixture := newTerminalArtifactFixture(
		t,
		[]string{"first.txt", "second.txt"},
	)
	firstPath := filepath.Join(fixture.workspace, "first.txt")
	secondPath := filepath.Join(fixture.workspace, "second.txt")
	if err := os.WriteFile(firstPath, []byte("first attempt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second artifact"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fail after both files and attachment rows have been prepared, but before
	// the run can become terminal. SQLite rolls back the rows and events while
	// the deterministic files remain bounded and reusable.
	if _, err := fixture.store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_terminal_after_artifacts
		BEFORE UPDATE OF status ON task_runs
		WHEN NEW.status = 'completed'
		BEGIN
			SELECT RAISE(ABORT, 'forced late terminal failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinalizeRunTerminal(
		ctx,
		terminalArtifactScope(fixture),
		0,
	); err == nil || !strings.Contains(err.Error(), "forced late terminal failure") {
		t.Fatalf("late finalization error = %v", err)
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 0 || events != 0 {
		t.Fatalf(
			"rolled-back finalization exposed artifacts: attachments=%d events=%d",
			attachments,
			events,
		)
	}
	filesAfterFailure := terminalArtifactFiles(t, fixture)
	if len(filesAfterFailure) != 2 {
		t.Fatalf(
			"rolled-back capture files = %v, want one bounded file per request item",
			filesAfterFailure,
		)
	}

	// A retry captures the current post-process snapshot into the same
	// destination instead of preserving stale bytes from the failed attempt.
	if err := os.WriteFile(firstPath, []byte("retry snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.db.ExecContext(
		ctx,
		"DROP TRIGGER fail_terminal_after_artifacts",
	); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.FinalizeRunTerminal(
		ctx,
		terminalArtifactScope(fixture),
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone ||
		len(completed.Attachments) != 2 {
		t.Fatalf("completed task = %+v", completed)
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 2 || events != 2 {
		t.Fatalf(
			"successful finalization artifacts=%d events=%d, want 2 and 2",
			attachments,
			events,
		)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 2 {
		t.Fatalf("successful capture files = %v, want 2", files)
	}
	firstContents, err := os.ReadFile(*completed.Attachments[0].Path)
	if err != nil || string(firstContents) != "retry snapshot" {
		t.Fatalf(
			"retry artifact snapshot = %q err=%v",
			firstContents,
			err,
		)
	}

	var metadataJSON string
	if err := fixture.store.db.QueryRowContext(
		ctx,
		"SELECT metadata_json FROM task_runs WHERE id = ?",
		fixture.claim.Run.ID,
	).Scan(&metadataJSON); err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Artifacts []struct {
			ID string `json:"id"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Artifacts) != 2 ||
		metadata.Artifacts[0].ID != completed.Attachments[0].ID ||
		metadata.Artifacts[1].ID != completed.Attachments[1].ID {
		t.Fatalf(
			"run metadata does not reference captured attachment IDs: %+v vs %+v",
			metadata.Artifacts,
			completed.Attachments,
		)
	}

	// A caller that lost the successful response can retry safely.
	reconciled, err := fixture.store.FinalizeRunTerminal(
		ctx,
		terminalArtifactScope(fixture),
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Task.Status != model.TaskStatusDone {
		t.Fatalf("response-loss retry = %+v", reconciled.Task)
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 2 || events != 2 {
		t.Fatalf(
			"response-loss retry duplicated artifacts=%d events=%d",
			attachments,
			events,
		)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 2 {
		t.Fatalf("response-loss retry files = %v, want 2", files)
	}
}

func TestFinalizeRunTerminalPartialArtifactRetryDoesNotAccumulateFiles(
	t *testing.T,
) {
	ctx := context.Background()
	fixture := newTerminalArtifactFixture(
		t,
		[]string{"present.txt", "later.txt"},
	)
	if err := os.WriteFile(
		filepath.Join(fixture.workspace, "present.txt"),
		[]byte("present"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinalizeRunTerminal(
		ctx,
		terminalArtifactScope(fixture),
		0,
	); err == nil || !strings.Contains(err.Error(), "later.txt") {
		t.Fatalf("partial capture error = %v", err)
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 0 || events != 0 {
		t.Fatalf(
			"partial capture exposed attachments=%d events=%d",
			attachments,
			events,
		)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 1 {
		t.Fatalf("partial capture files = %v, want 1 bounded remnant", files)
	}
	if err := os.WriteFile(
		filepath.Join(fixture.workspace, "later.txt"),
		[]byte("now present"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.FinalizeRunTerminal(
		ctx,
		terminalArtifactScope(fixture),
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Attachments) != 2 {
		t.Fatalf("partial retry attachments = %+v", completed.Attachments)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 2 {
		t.Fatalf("partial retry files = %v, want 2", files)
	}
}

func TestConcurrentTerminalArtifactFinalizersConverge(t *testing.T) {
	ctx := context.Background()
	fixture := newTerminalArtifactFixture(t, []string{"result.txt"})
	if err := os.WriteFile(
		filepath.Join(fixture.workspace, "result.txt"),
		[]byte("one durable snapshot"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsByCall := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := fixture.store.FinalizeRunTerminal(
				ctx,
				terminalArtifactScope(fixture),
				0,
			)
			errorsByCall <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatal(err)
		}
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 1 || events != 1 {
		t.Fatalf(
			"concurrent finalizers produced attachments=%d events=%d",
			attachments,
			events,
		)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 1 {
		t.Fatalf("concurrent finalizer files = %v, want 1", files)
	}
}

func TestObservedTerminalCASLossDoesNotCaptureArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := newTerminalArtifactFixture(t, []string{"result.txt"})
	if err := os.WriteFile(
		filepath.Join(fixture.workspace, "result.txt"),
		[]byte("must remain private until ownership is revalidated"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	scope := RunScope{
		RunID:      fixture.claim.Run.ID,
		ClaimToken: fixture.claim.ClaimToken,
	}
	if err := fixture.store.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	run, err := getRun(ctx, fixture.store.db, fixture.claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialObservation := ObserveRunForRecovery(run, nil, nil)
	if _, err := fixture.store.FenceObservedRunRecovery(
		ctx,
		initialObservation,
		30,
		"test process stopped",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.store.GetDeferredReclaim(ctx, fixture.claim.Run.ID)
	if err != nil || fence == nil {
		t.Fatalf("load recovery fence: %#v, %v", fence, err)
	}
	reclaim, err := fixture.store.AcknowledgeRunRecoveryFence(
		ctx,
		scope,
		fence.FenceToken,
		fence.FenceGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	run, err = getRun(ctx, fixture.store.db, fixture.claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	observation := ObserveRunForRecovery(run, nil, &reclaim)
	observation, acquired, err := fixture.store.ClaimObservedRunRecovery(
		ctx,
		observation,
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("claim recovery ownership: acquired=%v, err=%v", acquired, err)
	}
	// Advance the durable recovery decision after the snapshot. The stale
	// observer must lose before it copies any workspace bytes.
	if _, err := fixture.store.FenceObservedRunRecovery(
		ctx,
		observation,
		60,
		"new recovery decision",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinalizeObservedRunTerminal(
		ctx,
		observation,
		0,
	); !errors.Is(err, ErrRunRecoveryObservationChanged) {
		t.Fatalf("stale observed finalization error = %v", err)
	}
	if attachments, events := attachmentMutationCounts(t, fixture); attachments != 0 || events != 0 {
		t.Fatalf(
			"CAS loss exposed attachments=%d events=%d",
			attachments,
			events,
		)
	}
	if files := terminalArtifactFiles(t, fixture); len(files) != 0 {
		t.Fatalf("CAS loss captured files before validation: %v", files)
	}
}
