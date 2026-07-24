package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestRecoveryCheckpointContextReturnsActiveAndBoundedRecentWithoutCredentials(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	ctx := context.Background()
	original := registerFailedRecoverySource(t, fixture)
	claim, scope := claimRecoveryRun(t, fixture.store, fixture.task.Task.ID)
	reserved := reserveRecoveryCheckpoint(t, fixture.store, scope, original)
	adopted, err := fixture.store.ConfirmRecoveryCheckpointAdoption(
		ctx,
		scope,
		original.ID,
		reserved.ReservationToken,
		original.OutputBaseCommit,
		recoveryCommit('e'),
	)
	if err != nil {
		t.Fatal(err)
	}
	replacementInput := recoveryCheckpointInput(
		claim.Run.ID,
		"/private/recovery/worktree",
		'e',
		'f',
	)
	replacementInput.RepositoryPath = original.RepositoryPath
	replacement, _, err := fixture.store.SupersedeRecoveryCheckpointAndFailRun(
		ctx,
		scope,
		adopted.ID,
		adopted.ReservationToken,
		replacementInput,
		"recovery worker failed",
		FailRunOptions{Outcome: model.RunStatusCrashed},
	)
	if err != nil {
		t.Fatal(err)
	}

	values, err := fixture.store.ListRecoveryCheckpointContext(
		ctx,
		fixture.task.Task.ID,
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 ||
		values[0].ID != replacement.ID ||
		values[0].State != model.RecoveryCheckpointPending ||
		values[1].ID != original.ID ||
		values[1].State != model.RecoveryCheckpointSuperseded ||
		values[1].AdoptedAt == nil ||
		values[1].SupersedeReason == nil ||
		len(values[0].ChangedFiles) != len(replacementInput.ChangedFiles) {
		t.Fatalf("recovery checkpoint context = %+v", values)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(encoded)
	for _, secret := range []string{
		replacementInput.RepositoryPath,
		replacementInput.WorktreePath,
		replacementInput.DurableRef,
		adopted.ReservationToken,
	} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("recovery checkpoint context leaked %q: %s", secret, serialized)
		}
	}
}

func TestRecoveryCheckpointContextRejectsUnboundedHistory(t *testing.T) {
	fixture := newRecoveryCheckpointFixture(t, 5)
	if _, err := fixture.store.ListRecoveryCheckpointContext(
		context.Background(),
		fixture.task.Task.ID,
		11,
	); err == nil || !strings.Contains(err.Error(), "between 0 and 10") {
		t.Fatalf("unbounded recovery checkpoint context error = %v", err)
	}
}
