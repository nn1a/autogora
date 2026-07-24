package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestRecoveryCLIShowsAndConfirmsExactFenceGeneration(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	manager, err := app.managerFor(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "external-worker"
	task, err := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "confirm an external recovery", Assignee: &assignee,
		Runtime: model.RuntimeCodex,
	})
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(
		context.Background(),
		store.ClaimOptions{TaskID: task.Task.ID},
	)
	if err != nil || claim == nil {
		opened.Close()
		t.Fatalf("claim run: claim=%+v err=%v", claim, err)
	}
	if _, err := opened.RequireObservedRunRecoveryIntervention(
		context.Background(),
		store.ObserveRunForRecovery(claim.Run, nil),
		30,
		"external worker quiescence is unknown",
		model.RunStatusReclaimed,
		false,
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	fence, err := opened.GetDeferredReclaim(context.Background(), claim.Run.ID)
	if err != nil || fence == nil {
		opened.Close()
		t.Fatalf("load fence: value=%+v err=%v", fence, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	shown := runApp(
		t,
		app,
		"recovery",
		"show",
		claim.Run.ID,
		"--db",
		dbPath,
	)
	if strings.Contains(shown, fence.FenceToken) ||
		strings.Contains(shown, "processIdentity") ||
		!strings.Contains(shown, `"fenceGeneration": 1`) {
		t.Fatalf("unsafe or incomplete recovery show output: %s", shown)
	}

	app.Stdout, app.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	err = app.Run(context.Background(), []string{
		"recovery", "confirm", claim.Run.ID,
		"--db", dbPath,
		"--fence-generation", "1",
		"--actor", "operator@example.test",
		"--reason", "verified the external session and host process stopped",
		"--confirm-worker-stopped",
	})
	if err == nil || !strings.Contains(
		err.Error(),
		"confirm that host and external writes stopped",
	) {
		t.Fatalf("one-sided recovery confirmation error = %v", err)
	}

	confirmedJSON := runApp(
		t,
		app,
		"recovery",
		"confirm",
		claim.Run.ID,
		"--db",
		dbPath,
		"--fence-generation",
		"1",
		"--actor",
		"operator@example.test",
		"--reason",
		"verified the external session and host process stopped",
		"--confirm-worker-stopped",
		"--confirm-host-writes-stopped",
	)
	var confirmed store.DeferredReclaim
	if err := json.Unmarshal([]byte(confirmedJSON), &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.RequiresOperator ||
		confirmed.FenceGeneration != fence.FenceGeneration+1 ||
		confirmed.OperatorQuiescedBy == nil ||
		*confirmed.OperatorQuiescedBy != "operator@example.test" ||
		!confirmed.OperatorConfirmedWorkerStopped ||
		!confirmed.OperatorConfirmedHostWritesStopped {
		t.Fatalf("confirmed recovery fence = %+v", confirmed)
	}
}

func TestRecoveryCLIHelpRequiresTwoExplicitConfirmations(t *testing.T) {
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	output := runApp(t, app, "recovery", "help")
	for _, expected := range []string{
		"--fence-generation",
		"--confirm-worker-stopped",
		"--confirm-host-writes-stopped",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("recovery help omitted %s: %s", expected, output)
		}
	}
}
