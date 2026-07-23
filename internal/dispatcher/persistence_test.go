package dispatcher

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	_ "modernc.org/sqlite"
)

func TestRetryStoreOperationRetriesTransientSQLiteFailure(t *testing.T) {
	attempts := 0
	value, err := retryStoreOperation(context.Background(), func() (string, error) {
		attempts++
		if attempts < statePersistenceAttempts {
			return "", errors.New("database is locked (SQLITE_BUSY)")
		}
		return "persisted", nil
	})
	if err != nil || value != "persisted" || attempts != statePersistenceAttempts {
		t.Fatalf("retry result value=%q attempts=%d err=%v", value, attempts, err)
	}
}

func TestOnceReturnsTerminalPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "terminal write must be durable", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_terminal_run_update
		BEFORE UPDATE OF status ON task_runs
		WHEN OLD.status = 'running' AND NEW.status = 'completed'
		BEGIN SELECT RAISE(ABORT, 'forced terminal persistence failure'); END`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cliPath := buildAutogora(t)
	fixture := executableFixture(t, `
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "worker finished" >/dev/null
printf '%s\n' '{"type":"run_result","text":"done"}'`)
	runErr := Run(ctx, Options{DBPath: dbPath, CLIPath: cliPath, Board: "default", TaskID: task.Task.ID, Once: true,
		AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}})
	if runErr == nil || !strings.Contains(runErr.Error(), "forced terminal persistence failure") {
		t.Fatalf("one-shot dispatcher error = %v, want terminal persistence failure", runErr)
	}

	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusBlocked || detail.Task.CurrentRunID != nil || detail.Task.BlockKind == nil ||
		*detail.Task.BlockKind != model.BlockKindNeedsInput || len(detail.Runs) != 1 || detail.Runs[0].Status != model.RunStatusBlocked ||
		len(detail.TerminalRequests) != 0 {
		t.Fatalf("failed persistence was reported but unexpected state was exposed: %#v", detail)
	}
}

func TestOnceReturnsFailedRunPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{Title: "failed exit must be durable", Assignee: &assignee, Runtime: model.RuntimeCline})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_failed_run_update
		BEFORE UPDATE OF status ON task_runs
		WHEN OLD.status = 'running' AND NEW.status = 'failed'
		BEGIN SELECT RAISE(ABORT, 'forced failed-run persistence failure'); END`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	fixture := executableFixture(t, "exit 1")
	runErr := Run(ctx, Options{DBPath: dbPath, CLIPath: "/tmp/autogora", Board: "default", TaskID: task.Task.ID, Once: true,
		AutoDecompose: boolValue(false), Getenv: func(name string) string {
			if name == "AUTOGORA_CLINE_BIN" {
				return fixture
			}
			return ""
		}})
	if runErr == nil || !strings.Contains(runErr.Error(), "forced failed-run persistence failure") {
		t.Fatalf("one-shot dispatcher error = %v, want failed-run persistence failure", runErr)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != model.TaskStatusRunning || detail.Task.CurrentRunID == nil || len(detail.Runs) != 1 ||
		detail.Runs[0].Status != model.RunStatusRunning {
		t.Fatalf("failed-run persistence error was not left recoverable: %#v", detail)
	}
}
