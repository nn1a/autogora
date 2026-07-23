package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestTaskWorkflowRoleDefaultsValidatesAndUpdates(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	ordinary, err := opened.CreateTask(ctx, CreateTaskInput{Title: "ordinary task"})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Task.WorkflowRole != model.WorkflowRoleWorker {
		t.Fatalf("ordinary workflow role = %q", ordinary.Task.WorkflowRole)
	}

	reviewer, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "review task", WorkflowRole: model.WorkflowRoleReviewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.Task.WorkflowRole != model.WorkflowRoleReviewer {
		t.Fatalf("explicit workflow role = %q", reviewer.Task.WorkflowRole)
	}

	finalizer := model.WorkflowRoleFinalizer
	updated, err := opened.UpdateTask(ctx, reviewer.Task.ID, UpdateTaskInput{WorkflowRole: &finalizer})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Task.WorkflowRole != model.WorkflowRoleFinalizer {
		t.Fatalf("updated workflow role = %q", updated.Task.WorkflowRole)
	}

	if _, err := opened.CreateTask(ctx, CreateTaskInput{
		Title: "invalid create", WorkflowRole: model.WorkflowRole("coordinator"),
	}); err == nil || !strings.Contains(err.Error(), "invalid workflow role") {
		t.Fatalf("invalid create error = %v", err)
	}
	invalid := model.WorkflowRole("operator")
	if _, err := opened.UpdateTask(ctx, ordinary.Task.ID, UpdateTaskInput{
		WorkflowRole: &invalid,
	}); err == nil || !strings.Contains(err.Error(), "invalid workflow role") {
		t.Fatalf("invalid update error = %v", err)
	}
	if _, err := opened.db.ExecContext(ctx,
		"UPDATE tasks SET workflow_role = 'coordinator' WHERE id = ?", ordinary.Task.ID,
	); err == nil {
		t.Fatal("database accepted an invalid workflow role")
	}
}

func TestSchema17MigrationAddsDefaultWorkflowRole(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "schema-17.db")
	legacy, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	v18Columns := `  updated_at TEXT NOT NULL,
  workflow_role TEXT NOT NULL DEFAULT 'worker' CHECK (workflow_role IN ('worker', 'reviewer', 'finalizer', 'control'))`
	schema17 := strings.Replace(latestSchema, v18Columns, "  updated_at TEXT NOT NULL", 1)
	if schema17 == latestSchema {
		legacy.Close()
		t.Fatal("test could not derive the v17 task schema")
	}
	if _, err := legacy.ExecContext(ctx, schema17); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `
		INSERT INTO tasks(id, title, runtime, status, created_at, updated_at)
		VALUES ('t_v17', 'preserve me', 'manual', 'todo',
			'2026-07-23T00:00:00.000Z', '2026-07-23T00:00:00.000Z');
		PRAGMA user_version = 17;
	`); err != nil {
		legacy.Close()
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
	task, err := migrated.GetTask(ctx, "t_v17")
	if err != nil {
		t.Fatal(err)
	}
	if task.Task.Title != "preserve me" || task.Task.WorkflowRole != model.WorkflowRoleWorker {
		t.Fatalf("v17 task migration = %#v", task.Task)
	}
	var version int
	if err := migrated.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 19 {
		t.Fatalf("schema version = %d, want 19", version)
	}
}
