package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func completePrerequisiteWithChangeSet(
	t *testing.T,
	opened *store.Store,
	taskID, repository, worktree, baseCommit, headCommit string,
	changedFiles []string,
) model.ChangeSet {
	t.Helper()
	ctx := context.Background()
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: taskID})
	if err != nil || claim == nil {
		t.Fatalf("claim prerequisite: claim=%#v err=%v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if _, err := opened.RequestRunCompletion(ctx, scope, store.CompletionInput{Summary: "prerequisite ready"}); err != nil {
		t.Fatal(err)
	}
	durableRef, err := durableRunRef(claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	changeSet, err := opened.RecordRunChangeSet(ctx, scope, store.RecordChangeSetInput{
		RunID: claim.Run.ID, RepositoryPath: repository, WorktreePath: worktree,
		BaseCommit: baseCommit, HeadCommit: headCommit, DurableRef: durableRef,
		State: "ready", ChangedFiles: changedFiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FinalizeRunTerminal(ctx, scope, 0); err != nil {
		t.Fatal(err)
	}
	return changeSet
}

func TestPrerequisiteConflictPersistsRichDeduplicatedIncident(t *testing.T) {
	ctx := context.Background()
	_, opened := testManager(t)
	defer opened.Close()

	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "init", "-q")
	conflictPath := filepath.Join(repository, "conflict.txt")
	if err := os.WriteFile(conflictPath, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "add", "conflict.txt")
	runWorkspaceGit(t, repository, "commit", "-q", "-m", "base")
	baseCommit := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")
	if err := os.WriteFile(conflictPath, []byte("prerequisite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "commit", "-q", "-am", "prerequisite")
	prerequisiteHead := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")
	runWorkspaceGit(t, repository, "reset", "--hard", baseCommit)
	if err := os.WriteFile(conflictPath, []byte("dependent base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, repository, "commit", "-q", "-am", "dependent base")
	dependentBase := runWorkspaceGit(t, repository, "rev-parse", "HEAD^{commit}")

	root, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "integration root"})
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "prerequisite", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "dependent", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Parents: []string{prerequisite.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetSubtaskParent(ctx, root.Task.ID, dependent.Task.ID, nil); err != nil {
		t.Fatal(err)
	}

	prerequisiteChangeSet := completePrerequisiteWithChangeSet(
		t, opened, prerequisite.Task.ID, repository, repository, baseCommit, prerequisiteHead, []string{"conflict.txt"},
	)
	runWorkspaceGit(t, repository, "update-ref", prerequisiteChangeSet.DurableRef, prerequisiteHead)

	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: dependent.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim dependent: claim=%#v err=%v", claim, err)
	}
	worktree := filepath.Join(t.TempDir(), "dependent-worktree")
	runWorkspaceGit(t, repository, "worktree", "add", "-q", "--detach", worktree, dependentBase)
	claim.Workspace = &model.RunWorkspace{
		RunID: claim.Run.ID, TaskID: dependent.Task.ID, Kind: model.WorkspaceWorktree,
		Path: worktree, RepositoryPath: &repository, BaseCommit: &dependentBase,
	}

	workspaces := &Manager{}
	for attempt := 0; attempt < 2; attempt++ {
		_, detected := workspaces.IntegratePrerequisiteChangeSets(ctx, opened, claim)
		var integrationErr *PrerequisiteIntegrationError
		if !errors.As(detected, &integrationErr) || integrationErr.Code != IntegrationFailureConflict ||
			integrationErr.BlockKind != model.BlockKindNeedsInput {
			t.Fatalf("attempt %d returned %#v", attempt+1, detected)
		}
		if !slices.Equal(integrationErr.ConflictingFiles, []string{"conflict.txt"}) {
			t.Fatalf("conflicting files = %#v", integrationErr.ConflictingFiles)
		}
	}

	incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		TaskID: dependent.Task.ID, Trigger: model.CoordinationTriggerIntegrationConflict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("active incident dedupe produced %d incidents: %#v", len(incidents), incidents)
	}
	incident := incidents[0]
	state, err := opened.GetBoardGraphState(ctx, dependent.Task.Board)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Board != dependent.Task.Board ||
		incident.TaskID == nil || *incident.TaskID != dependent.Task.ID ||
		incident.RootTaskID == nil || *incident.RootTaskID != root.Task.ID ||
		incident.GraphRevision != state.Revision ||
		incident.Severity != model.CoordinationSeverityError {
		t.Fatalf("incident identity/revision = %#v, graph=%#v", incident, state)
	}
	var details integrationIncidentDetails
	if err := json.Unmarshal(incident.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details.Code != IntegrationFailureConflict || details.BlockKind != model.BlockKindNeedsInput ||
		details.WorkspacePath != worktree || details.PrerequisiteID != prerequisite.Task.ID ||
		details.ChangeSetID != prerequisiteChangeSet.ID || details.DurableRef != prerequisiteChangeSet.DurableRef ||
		!slices.Equal(details.ConflictingFiles, []string{"conflict.txt"}) {
		t.Fatalf("incident details = %#v", details)
	}

	current, err := opened.GetTask(ctx, dependent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Task.Status != model.TaskStatusRunning || current.Task.CurrentRunID == nil ||
		*current.Task.CurrentRunID != claim.Run.ID {
		t.Fatalf("incident persistence replaced the integration lifecycle: %#v", current.Task)
	}
}

func TestHistoryRewritePersistsIncidentAndOrdinaryFailureDoesNot(t *testing.T) {
	ctx := context.Background()
	_, opened := testManager(t)
	defer opened.Close()

	root, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "history root"})
	if err != nil {
		t.Fatal(err)
	}
	prerequisite, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "history prerequisite", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "history dependent", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Parents: []string{prerequisite.Task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.SetSubtaskParent(ctx, root.Task.ID, dependent.Task.ID, nil); err != nil {
		t.Fatal(err)
	}
	completePrerequisiteWithChangeSet(
		t, opened, prerequisite.Task.ID, "/repository", "/prerequisite-worktree",
		"base-commit", "head-commit", []string{"prerequisite.txt"},
	)

	repository, workspacePath := "/repository", "/preserved/dependent-worktree"
	detected := (&Manager{}).VerifyPrerequisiteChangeSets(ctx, opened, dependent.Task.ID, model.RunWorkspace{
		Kind: model.WorkspaceWorktree, Path: workspacePath, RepositoryPath: &repository,
	}, "invalid-final-commit")
	var integrationErr *PrerequisiteIntegrationError
	if !errors.As(detected, &integrationErr) || integrationErr.Code != IntegrationFailureHistoryRewrite ||
		integrationErr.BlockKind != model.BlockKindNeedsInput ||
		integrationErr.Reason != "worker produced an invalid final Git commit" {
		t.Fatalf("history rewrite error was replaced: %#v", detected)
	}

	persistExceptionalIntegrationIncident(opened, dependent.Task.ID, &PrerequisiteIntegrationError{
		Code: IntegrationFailureDirtyWorkspace, BlockKind: model.BlockKindNeedsInput,
		Reason: "ordinary dirty workspace", WorkspacePath: workspacePath,
	})
	incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		TaskID: dependent.Task.ID, Trigger: model.CoordinationTriggerIntegrationConflict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("history rewrite/ordinary failure incidents = %#v", incidents)
	}
	incident := incidents[0]
	if incident.RootTaskID == nil || *incident.RootTaskID != root.Task.ID {
		t.Fatalf("history incident root = %#v", incident.RootTaskID)
	}
	var details integrationIncidentDetails
	if err := json.Unmarshal(incident.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details.Code != IntegrationFailureHistoryRewrite ||
		details.BlockKind != model.BlockKindNeedsInput ||
		details.WorkspacePath != workspacePath ||
		details.Reason != integrationErr.Reason {
		t.Fatalf("history incident details = %#v", details)
	}
	current, err := opened.GetTask(ctx, dependent.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Task.Status != model.TaskStatusReady || current.Task.CurrentRunID != nil {
		t.Fatalf("history incident persistence mutated task lifecycle: %#v", current.Task)
	}
}

func TestIntegrationIncidentBoundsLargeConflictDetailsForCoordinatorSnapshot(t *testing.T) {
	ctx := context.Background()
	_, opened := testManager(t)
	defer opened.Close()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "large conflict"})
	if err != nil {
		t.Fatal(err)
	}

	conflictingFiles := make([]string, 0, 128)
	for index := 0; index < 64; index++ {
		path := fmt.Sprintf("%03d/", index) + strings.Repeat("\x00<&긴-path-segment/", 160)
		conflictingFiles = append(conflictingFiles, path, path)
	}
	reason := strings.Repeat("\x00<&very long integration reason🙂", 800)
	integrationErr := &PrerequisiteIntegrationError{
		Code: IntegrationFailureConflict, BlockKind: model.BlockKindNeedsInput,
		Reason: reason, WorkspacePath: strings.Repeat("/\x00<&workspace", 800),
		PrerequisiteID:   strings.Repeat("prerequisite-\x00", 100),
		ChangeSetID:      strings.Repeat("changeset-\x00", 100),
		DurableRef:       strings.Repeat("refs/autogora/\x00<&", 200),
		ConflictingFiles: conflictingFiles,
	}
	persistExceptionalIntegrationIncident(opened, task.Task.ID, integrationErr)

	incidents, err := opened.ListCoordinationIncidents(ctx, store.CoordinationIncidentFilter{
		TaskID: task.Task.ID, Trigger: model.CoordinationTriggerIntegrationConflict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("large conflict incidents = %#v", incidents)
	}
	incident := incidents[0]
	if len(incident.Details) > integrationIncidentDetailsLimit {
		t.Fatalf("incident details are %d bytes, limit %d", len(incident.Details), integrationIncidentDetailsLimit)
	}
	if len(incident.Summary) > integrationIncidentSummaryLimit || !utf8.ValidString(incident.Summary) {
		t.Fatalf("incident summary is not bounded valid UTF-8: bytes=%d valid=%v", len(incident.Summary), utf8.ValidString(incident.Summary))
	}
	var details integrationIncidentDetails
	if err := json.Unmarshal(incident.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details.ConflictingFilesUniqueCount != 64 ||
		details.ConflictingFilesOmittedCount != 64-len(details.ConflictingFiles) {
		t.Fatalf("conflicting file counts = unique:%d omitted:%d stored:%d",
			details.ConflictingFilesUniqueCount, details.ConflictingFilesOmittedCount, len(details.ConflictingFiles))
	}
	if len(details.ConflictingFiles) == 0 || len(details.ConflictingFiles) > integrationIncidentConflictFilesLimit ||
		!sort.StringsAreSorted(details.ConflictingFiles) {
		t.Fatalf("bounded conflicting files = %#v", details.ConflictingFiles)
	}
	seen := map[string]bool{}
	for _, path := range details.ConflictingFiles {
		if len(path) > integrationIncidentConflictFileLimit || !utf8.ValidString(path) || seen[path] {
			t.Fatalf("invalid bounded conflict path: bytes=%d valid=%v duplicate=%v", len(path), utf8.ValidString(path), seen[path])
		}
		seen[path] = true
	}
	if len(details.Reason) > integrationIncidentReasonLimit ||
		len(details.WorkspacePath) > integrationIncidentWorkspacePathLimit ||
		len(details.PrerequisiteID) > integrationIncidentIdentifierLimit ||
		len(details.ChangeSetID) > integrationIncidentIdentifierLimit ||
		len(details.DurableRef) > integrationIncidentDurableRefLimit {
		t.Fatalf("scalar detail bounds were exceeded: %#v", details)
	}
	if integrationErr.Reason != reason || len(integrationErr.ConflictingFiles) != len(conflictingFiles) {
		t.Fatal("incident bounding mutated the original integration error")
	}
	current, err := opened.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Task.Status != model.TaskStatusTodo || current.Task.CurrentRunID != nil {
		t.Fatalf("large incident persistence mutated task lifecycle: %#v", current.Task)
	}
}
