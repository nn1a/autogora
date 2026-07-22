package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nn1a/kanban/internal/model"
	_ "modernc.org/sqlite"
)

func stringValue(value string) *string { return &value }

func TestCreateListAndReadTaskUsingExistingJSONContract(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parent, err := store.CreateTask(ctx, CreateTaskInput{Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateTask(ctx, CreateTaskInput{
		Title: "Child", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Parents: []string{parent.Task.ID}, Skills: []string{"review", "review", " testing "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Status != model.TaskStatusTodo {
		t.Fatalf("dependent task status = %q, want todo", child.Task.Status)
	}
	if len(child.Parents) != 1 || child.Parents[0].ID != parent.Task.ID {
		t.Fatalf("dependency was not persisted: %+v", child.Parents)
	}
	if len(child.Task.Skills) != 2 || child.Task.Skills[1] != "testing" {
		t.Fatalf("skills were not normalized: %#v", child.Task.Skills)
	}
	if _, err := store.AddComment(ctx, child.Task.ID, "human", "preserve this handoff"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetTask(ctx, child.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Comments) != 1 || loaded.Comments[0].Body != "preserve this handoff" {
		t.Fatalf("comment was not persisted: %+v", loaded.Comments)
	}
	listed, err := store.ListTasks(ctx, ListTaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d tasks, want 2", len(listed))
	}
	stats, err := store.Stats(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.ByStatus[model.TaskStatusTodo] != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAttachmentsAndCompletionArtifactsRemainDurable(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	store, err := Open(filepath.Join(directory, "kanban.db"), "default", filepath.Join(directory, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace := filepath.Join(directory, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(workspace, "report.md")
	if err := os.WriteFile(artifactPath, []byte("verified report"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Document", Assignee: stringValue("writer"), Runtime: model.RuntimeCodex, Workspace: &workspace, WorkspaceKind: model.WorkspaceDir})
	if err != nil {
		t.Fatal(err)
	}
	urlAttachment, err := store.AttachURL(ctx, task.Task.ID, "https://example.com/spec", "spec")
	if err != nil || urlAttachment.URL == nil {
		t.Fatalf("attach URL: %+v %v", urlAttachment, err)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	completed, err := store.CompleteRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, CompletionInput{Summary: "documented", Artifacts: []string{"report.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Task.Status != model.TaskStatusDone || len(completed.Attachments) != 2 {
		t.Fatalf("completion artifacts missing: %+v", completed)
	}
	fileAttachment := completed.Attachments[1]
	if fileAttachment.Path == nil || !fileExistsForTest(*fileAttachment.Path) || fileAttachment.SHA256 == nil {
		t.Fatalf("file attachment is not durable: %+v", fileAttachment)
	}
}

func TestMissingCompletionArtifactLeavesRunActive(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	store, err := Open(":memory:", "default", filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Evidence", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex, Workspace: &workspace, WorkspaceKind: model.WorkspaceDir})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := store.CompleteRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, CompletionInput{Summary: "done", Artifacts: []string{"missing.txt"}}); err == nil {
		t.Fatal("missing artifact was accepted")
	}
	loaded, err := store.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Task.Status != model.TaskStatusRunning || loaded.Task.CurrentRunID == nil {
		t.Fatalf("failed completion lost active run: %+v", loaded.Task)
	}
}

func fileExistsForTest(path string) bool { _, err := os.Stat(path); return err == nil }

func TestDependencyLifecycleAndRealWorldGraphEdits(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.CreateTask(ctx, CreateTaskInput{Title: "Parent", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateTask(ctx, CreateTaskInput{Title: "Child", Assignee: stringValue("reviewer"), Runtime: model.RuntimeClaude, Parents: []string{parent.Task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Status != model.TaskStatusTodo {
		t.Fatalf("child status = %s", child.Task.Status)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: parent.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim parent: claim=%v err=%v", claim, err)
	}
	if _, err := store.CompleteRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, CompletionInput{Summary: "verified"}); err != nil {
		t.Fatal(err)
	}
	child, err = store.GetTask(ctx, child.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Task.Status != model.TaskStatusReady {
		t.Fatalf("completed dependency left child in %s", child.Task.Status)
	}
	if _, err := store.LinkTasks(ctx, child.Task.ID, parent.Task.ID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Fatalf("dependency cycle error = %v", err)
	}

	late, err := store.CreateTask(ctx, CreateTaskInput{Title: "Late prerequisite", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	childClaim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: child.Task.ID})
	if err != nil || childClaim == nil {
		t.Fatalf("claim child: claim=%v err=%v", childClaim, err)
	}
	if _, err := store.CompleteRun(ctx, RunScope{RunID: childClaim.Run.ID, ClaimToken: childClaim.ClaimToken}, CompletionInput{Summary: "initial completion"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.LinkTasks(ctx, late.Task.ID, child.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Task.Status != model.TaskStatusTodo {
		t.Fatalf("late prerequisite did not reopen done task: %s", reopened.Task.Status)
	}
}

func TestConcurrentClaimHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "claims.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Exactly once", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	var wait sync.WaitGroup
	winners := make(chan *model.ClaimedTask, contenders)
	errors := make(chan error, contenders)
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
			if err != nil {
				errors <- err
				return
			}
			if claim != nil {
				winners <- claim
			}
		}()
	}
	wait.Wait()
	close(winners)
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent claim: %v", err)
	}
	if got := len(winners); got != 1 {
		t.Fatalf("claim winners = %d, want 1", got)
	}
}

func TestBlockLoopAndActiveMutationGuards(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Needs policy", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := store.ArchiveTask(ctx, task.Task.ID); err == nil || !strings.Contains(err.Error(), "terminate") {
		t.Fatalf("archive active run error = %v", err)
	}
	blocked, err := store.BlockRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, BlockInput{Reason: "choose retention", Kind: model.BlockKindNeedsInput})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Task.Status != model.TaskStatusBlocked || blocked.Task.BlockRecurrences != 1 {
		t.Fatalf("first block = %+v", blocked.Task)
	}
	if _, err := store.UnblockTask(ctx, task.Task.ID); err != nil {
		t.Fatal(err)
	}
	claim, err = store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("second claim: %v %v", claim, err)
	}
	blocked, err = store.BlockRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, BlockInput{Reason: "choose retention", Kind: model.BlockKindNeedsInput})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Task.Status != model.TaskStatusTriage || blocked.Task.BlockRecurrences != 2 {
		t.Fatalf("repeated block did not escalate: %+v", blocked.Task)
	}
}

func TestRelationshipGraphSeparatesHierarchyFromDependencies(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, err := store.CreateTask(ctx, CreateTaskInput{Title: "Root"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, CreateTaskInput{Title: "Analyze", Assignee: stringValue("analyst"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, CreateTaskInput{Title: "Implement", Assignee: stringValue("builder"), Runtime: model.RuntimeClaude})
	if err != nil {
		t.Fatal(err)
	}
	position0, position1 := 0, 1
	if _, err := store.SetSubtaskParent(ctx, root.Task.ID, first.Task.ID, &position0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSubtaskParent(ctx, root.Task.ID, second.Task.ID, &position1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LinkTasks(ctx, first.Task.ID, second.Task.ID); err != nil {
		t.Fatal(err)
	}
	graph, err := store.RelationshipGraph(ctx, second.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if graph.RootTaskID != root.Task.ID || graph.TotalConnectedNodes != 3 || graph.TotalPhases != 2 {
		t.Fatalf("unexpected graph summary: %+v", graph)
	}
	if len(graph.Hierarchy) != 2 || len(graph.Dependencies) != 1 {
		t.Fatalf("graph conflated hierarchy and dependencies: %+v", graph)
	}
	var implementationNode *model.RelationshipNode
	for index := range graph.Nodes {
		if graph.Nodes[index].Task.ID == second.Task.ID {
			implementationNode = &graph.Nodes[index]
		}
	}
	if implementationNode == nil || len(implementationNode.BlockedBy) != 1 || implementationNode.BlockedBy[0] != first.Task.ID {
		t.Fatalf("worker-safe dependency context missing: %+v", implementationNode)
	}
}

func TestNotificationDeliveryLeasesHideSecretsAndRemoveTerminalSubscription(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.CreateTask(ctx, CreateTaskInput{Title: "Notify", Assignee: stringValue("worker"), Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	secret := "delivery-secret"
	subscription, err := store.SubscribeTask(ctx, SubscriptionInput{TaskID: task.Task.ID, Platform: "webhook", ChatID: "https://example.com/hook", Secret: OptionalString{Set: true, Value: &secret}})
	if err != nil {
		t.Fatal(err)
	}
	if !subscription.HasSecret {
		t.Fatal("subscription did not record the secret presence")
	}
	listed, err := store.ListNotificationSubscriptions(ctx, task.Task.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list subscriptions: %+v %v", listed, err)
	}
	claim, err := store.ClaimTask(ctx, ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, err := store.CompleteRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, CompletionInput{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.ClaimNotificationDeliveries(ctx, 25, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Secret == nil || *deliveries[0].Secret != secret || deliveries[0].Event.Kind != "completed" {
		t.Fatalf("unexpected delivery: %+v", deliveries)
	}
	if err := store.ResolveNotificationDelivery(ctx, deliveries[0].ID, deliveries[0].LeaseToken, nil); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListNotificationSubscriptions(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("terminal subscription remains: %+v", listed)
	}
}

func TestIdempotencyKeyReturnsExistingTask(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := "nightly-review"
	first, err := store.CreateTask(ctx, CreateTaskInput{Title: "First", IdempotencyKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, CreateTaskInput{Title: "Second", IdempotencyKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	if first.Task.ID != second.Task.ID || second.Task.Title != "First" {
		t.Fatalf("idempotency returned different task: first=%+v second=%+v", first.Task, second.Task)
	}
}

func TestLegacyMVPDatabaseMigratesWithoutDataLoss(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	legacy, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(ctx, `
		CREATE TABLE tasks (
		  id TEXT PRIMARY KEY, board TEXT NOT NULL DEFAULT 'default', title TEXT NOT NULL,
		  body TEXT NOT NULL DEFAULT '', assignee TEXT, runtime TEXT NOT NULL,
		  status TEXT NOT NULL CHECK (status IN ('triage','todo','ready','running','blocked','done','archived')),
		  priority INTEGER NOT NULL DEFAULT 0, workspace TEXT, current_run_id TEXT,
		  failure_count INTEGER NOT NULL DEFAULT 0, max_retries INTEGER NOT NULL DEFAULT 2,
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE task_links (
		  parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  PRIMARY KEY(parent_id, child_id)
		);
		CREATE TABLE task_comments (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  author TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL
		);
		CREATE TABLE task_runs (
		  id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  worker_id TEXT NOT NULL, runtime TEXT NOT NULL, status TEXT NOT NULL,
		  claim_token TEXT NOT NULL, claimed_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL,
		  ended_at TEXT, summary TEXT, metadata_json TEXT, error TEXT
		);
		CREATE TABLE task_events (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		  run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
		  kind TEXT NOT NULL, payload_json TEXT, created_at TEXT NOT NULL
		);
		INSERT INTO tasks VALUES (
		  't_parent','default','legacy parent','body','worker','codex','done',1,'/tmp/work',NULL,0,2,
		  '2026-07-20T00:00:00.000Z','2026-07-20T00:01:00.000Z'
		);
		INSERT INTO tasks VALUES (
		  't_child','default','legacy child','','worker','claude','todo',0,NULL,NULL,0,2,
		  '2026-07-20T00:02:00.000Z','2026-07-20T00:02:00.000Z'
		);
		INSERT INTO task_links VALUES ('t_parent','t_child');
		INSERT INTO task_comments(task_id,author,body,created_at) VALUES ('t_child','human','keep this','2026-07-20T00:03:00.000Z');
		INSERT INTO task_runs VALUES (
		  'r_old','t_parent','worker','codex','completed','secret','2026-07-20T00:00:00.000Z',
		  '2026-07-20T00:01:00.000Z','2026-07-20T00:01:00.000Z','done','{}',NULL
		);
		INSERT INTO task_events(task_id,run_id,kind,payload_json,created_at) VALUES (
		  't_parent','r_old','completed','{}','2026-07-20T00:01:00.000Z'
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath, "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	parent, err := migrated.GetTask(ctx, "t_parent")
	if err != nil {
		t.Fatal(err)
	}
	child, err := migrated.GetTask(ctx, "t_child")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Task.WorkspaceKind != model.WorkspaceDir || len(parent.Runs) != 1 || parent.Runs[0].ID != "r_old" {
		t.Fatalf("parent migration lost data: %+v", parent)
	}
	if len(child.Parents) != 1 || len(child.Comments) != 1 || child.Comments[0].Body != "keep this" {
		t.Fatalf("child migration lost data: %+v", child)
	}
	var version int
	if err := migrated.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
}
