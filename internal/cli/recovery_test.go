package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/operatorrecovery"
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
		"quarantine status",
		"quarantine confirm --file",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("recovery help omitted %s: %s", expected, output)
		}
	}
}

func TestRecoveryCLIShowsAndConfirmsGlobalQuarantine(t *testing.T) {
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
	authority, err := manager.OpenCoordinationStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gate, activated, err := authority.ActivateAutomationQuarantine(
		context.Background(),
		store.AutomationQuarantineSourceInput{
			Board:             "*",
			Kind:              "dispatcher_session_expired",
			SourceID:          "dispatcher-cli-recovery",
			ObservedUpdatedAt: "2026-07-24T00:00:00.000000000Z",
			DiagnosticCode:    "session_expired_without_release",
		},
	)
	if err != nil || !activated {
		authority.Close()
		t.Fatalf("activate quarantine: gate=%+v activated=%t err=%v",
			gate, activated, err)
	}
	sources, err := authority.ListAutomationQuarantineSources(
		context.Background(),
		store.AutomationQuarantineSourceFilter{
			ActiveOnly: true,
			Limit:      1000,
		},
	)
	if err != nil || len(sources) != 1 {
		authority.Close()
		t.Fatalf("active sources=%+v err=%v", sources, err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	shown := runApp(
		t,
		app,
		"recovery",
		"quarantine",
		"status",
		"--db",
		dbPath,
	)
	var status operatorrecovery.Status
	if err := json.Unmarshal([]byte(shown), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Gate.Active ||
		status.Gate.Generation != gate.Generation ||
		len(status.Sources) != 1 ||
		status.Sources[0].SourceKey != sources[0].SourceKey ||
		strings.Contains(shown, "permitToken") ||
		strings.Contains(shown, "claimToken") ||
		strings.Contains(shown, dbPath) {
		t.Fatalf("unsafe or incomplete quarantine status: %s", shown)
	}

	confirmation := operatorrecovery.Confirmation{
		Generation:            gate.Generation,
		Actor:                 "operator@example.test",
		Reason:                "verified all helper processes and external writers stopped",
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources: []operatorrecovery.ConfirmationSource{{
			SourceKey:          sources[0].SourceKey,
			Board:              sources[0].Board,
			Kind:               sources[0].Kind,
			SourceID:           sources[0].SourceID,
			ObservedUpdatedAt:  sources[0].ObservedUpdatedAt,
			ObservedClaimEpoch: sources[0].ObservedClaimEpoch,
			DiagnosticCode:     sources[0].DiagnosticCode,
			Disposition:        store.AutomationSourceAbandoned,
		}},
	}
	raw, err := json.Marshal(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	confirmationPath := filepath.Join(directory, "recovery.json")
	if err := os.WriteFile(confirmationPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	app.Stdout, app.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	confirmed := runApp(
		t,
		app,
		"recovery",
		"quarantine",
		"confirm",
		"--file",
		confirmationPath,
		"--db",
		dbPath,
	)
	var result operatorrecovery.ConfirmationResult
	if err := json.Unmarshal([]byte(confirmed), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || result.Gate.Active {
		t.Fatalf("quarantine confirmation = %+v", result)
	}

	app.Stdout, app.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	replayed := runApp(
		t,
		app,
		"recovery",
		"quarantine",
		"confirm",
		"--file",
		confirmationPath,
		"--db",
		dbPath,
	)
	if err := json.Unmarshal([]byte(replayed), &result); err != nil {
		t.Fatal(err)
	}
	if result.Cleared || result.Gate.Active {
		t.Fatalf("idempotent quarantine confirmation = %+v", result)
	}
}

func TestRecoveryCLIQuarantineRejectsUnsafeInputsAndWorkerScope(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "autogora.db")
	app := New(&bytes.Buffer{}, &bytes.Buffer{})
	app.Cwd = directory
	app.Getenv = func(string) string { return "" }
	runApp(t, app, "init", "--db", dbPath)

	invalidPath := filepath.Join(directory, "invalid-recovery.json")
	if err := os.WriteFile(
		invalidPath,
		[]byte(`{"generation":1,"claimToken":"secret"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{
			"recovery", "quarantine", "confirm",
			"--file", invalidPath, "--db", dbPath,
		},
		{
			"recovery", "quarantine", "status",
			"--board", "default", "--db", dbPath,
		},
		{
			"recovery", "quarantine", "status",
			"--claim-token", "secret", "--db", dbPath,
		},
	} {
		app.Stdout, app.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
		if err := app.Run(context.Background(), args); err == nil {
			t.Fatalf("unsafe quarantine command succeeded: %v", args)
		}
	}

	app.Getenv = func(name string) string {
		if name == "AUTOGORA_TASK_ID" {
			return "task_worker"
		}
		return ""
	}
	if err := app.Run(context.Background(), []string{
		"recovery", "quarantine", "status", "--db", dbPath,
	}); err == nil || !strings.Contains(err.Error(), "scoped workers") {
		t.Fatalf("worker-scoped quarantine error = %v", err)
	}
}
