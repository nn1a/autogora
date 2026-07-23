package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestManagedRunWritePolicyDistinguishesKnownAndLegacyRuns(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "managed-policy.db"), "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "worker"

	knownTask, err := opened.CreateTask(ctx, CreateTaskInput{Title: "known policy", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	known, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: knownTask.Task.ID})
	if err != nil || known == nil {
		t.Fatalf("known claim: %+v, %v", known, err)
	}
	if err := opened.MarkRunManagedWithPolicy(ctx, RunScope{RunID: known.Run.ID, ClaimToken: known.ClaimToken}, false); err != nil {
		t.Fatal(err)
	}
	policy, err := opened.GetManagedRunWritePolicy(ctx, known.Run.ID)
	if err != nil || policy == nil || *policy {
		t.Fatalf("known read-only policy = %v, err=%v", policy, err)
	}
	if _, err := opened.RecoverAbandonedRun(ctx, known.Run.ID, model.RunStatusReclaimed, "next fixture", false); err != nil {
		t.Fatal(err)
	}

	legacyTask, err := opened.CreateTask(ctx, CreateTaskInput{Title: "unknown policy", Assignee: &assignee, Runtime: model.RuntimeCodex})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: legacyTask.Task.ID})
	if err != nil || legacy == nil {
		t.Fatalf("legacy claim: %+v, %v", legacy, err)
	}
	if err := opened.MarkRunManaged(ctx, RunScope{RunID: legacy.Run.ID, ClaimToken: legacy.ClaimToken}); err != nil {
		t.Fatal(err)
	}
	policy, err = opened.GetManagedRunWritePolicy(ctx, legacy.Run.ID)
	if err != nil || policy != nil {
		t.Fatalf("legacy policy = %v, err=%v; want unknown", policy, err)
	}
}
