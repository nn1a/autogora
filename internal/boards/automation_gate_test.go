package boards

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func validateManagerPublishingSource(
	ctx context.Context,
	manager *Manager,
	input store.AutomationQuarantineSourceInput,
) (bool, error) {
	inventory, err := manager.ListMetadata(ctx, true)
	if err != nil {
		return false, err
	}
	matches := 0
	for _, metadata := range inventory {
		if metadata.Slug != input.Board {
			continue
		}
		reader, err := manager.OpenListedPublicationRecoveryReader(
			ctx,
			metadata,
		)
		if err != nil {
			return false, err
		}
		exact, validationErr := reader.ValidatePublishingAutomationSource(
			ctx,
			input,
		)
		closeErr := reader.Close()
		if validationErr != nil || closeErr != nil {
			return false, errors.Join(validationErr, closeErr)
		}
		if exact {
			matches++
		}
	}
	return matches == 1, nil
}

func TestManagerInjectsOneAutomationAuthorityAndLockIntoEveryBoardStore(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "default", Update{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "alpha", Update{}); err != nil {
		t.Fatal(err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer coordination.Close()
	alpha, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alpha.Close()

	lease, acquired, err := coordination.RegisterAutomationDispatcherSession(
		ctx,
		"*",
		"alpha-dispatcher",
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("register session acquired=%v, err=%v", acquired, err)
	}
	permit, err := alpha.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordination.ValidateAutomationPermit(ctx, permit); err != nil {
		t.Fatalf("coordination Store rejected board permit: %v", err)
	}
	globalPermitUsed := false
	if err := alpha.WithAutomationPermit(ctx, permit, func() error {
		globalPermitUsed = true
		return nil
	}); err != nil || !globalPermitUsed {
		t.Fatalf("global session did not cover alpha: used=%v, err=%v", globalPermitUsed, err)
	}

	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, _, err := coordination.ActivateAutomationQuarantine(
		blockedContext,
		store.AutomationQuarantineSourceInput{
			Board:             "alpha",
			Kind:              "automation_test_source",
			SourceID:          "pub-alpha",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("board and authority did not share an OS lock: %v", err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	if released, err := coordination.ReleaseAutomationDispatcherSession(
		ctx,
		lease,
	); err != nil || !released {
		t.Fatalf("release global session released=%v, err=%v", released, err)
	}
	scopedLease, acquired, err := coordination.RegisterAutomationDispatcherSession(
		ctx,
		"alpha",
		"scoped-alpha-dispatcher",
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("register scoped session acquired=%v, err=%v", acquired, err)
	}
	scopedPermit, err := alpha.AcquireAutomationPermitForSession(ctx, scopedLease)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coordination.AcquireGlobalWorkspaceLeaseAutomated(
		ctx,
		scopedPermit,
		"beta",
		"run-beta",
		filepath.Join(t.TempDir(), "beta-workspace"),
	); !errors.Is(err, store.ErrAutomationPermitScope) {
		t.Fatalf("board-scoped session covered another board: %v", err)
	}
	if err := scopedPermit.Close(); err != nil {
		t.Fatal(err)
	}
	activated, changed, err := coordination.ActivateAutomationQuarantine(
		ctx,
		store.AutomationQuarantineSourceInput{
			Board:             "alpha",
			Kind:              "automation_test_source",
			SourceID:          "pub-alpha",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	)
	if err != nil || !changed || !activated.Active {
		t.Fatalf("activation = %+v, changed=%v, err=%v", activated, changed, err)
	}
	observed, err := alpha.GetAutomationQuarantine(ctx)
	if err != nil || !observed.Active ||
		observed.Generation != activated.Generation {
		t.Fatalf("board authority view = %+v, err=%v", observed, err)
	}
	if _, err := alpha.AcquireAutomationPermitForSession(
		ctx,
		scopedLease,
	); !errors.Is(err, store.ErrAutomationQuarantined) {
		t.Fatalf("board permit behind global quarantine error = %v", err)
	}
}

func TestDirectAndManagerStoresDeriveOneCanonicalAutomationLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a directory symlink requires privileges on Windows")
	}
	ctx := context.Background()
	root := t.TempDir()
	realHome := filepath.Join(root, "real")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasHome := filepath.Join(root, "alias")
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(realHome, "autogora.db")
	direct, err := store.Open(
		databasePath,
		"default",
		filepath.Join(realHome, "attachments"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	manager, err := NewManager(filepath.Join(aliasHome, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	managed, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Close()

	lease, acquired, err := direct.RegisterAutomationDispatcherSession(
		ctx,
		"*",
		"canonical-lock-dispatcher",
		time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("register session acquired=%v, err=%v", acquired, err)
	}
	permit, err := direct.AcquireAutomationPermitForSession(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()

	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, _, err := managed.ActivateAutomationQuarantine(
		blockedContext,
		store.AutomationQuarantineSourceInput{
			Board:             "default",
			Kind:              "automation_test_source",
			SourceID:          "canonical-lock-publication",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same authority used different automation locks: %v", err)
	}
}

func TestBoardRemovalRejectsActiveGlobalAutomationQuarantine(t *testing.T) {
	for _, hardDelete := range []bool{false, true} {
		t.Run(map[bool]string{
			false: "archive",
			true:  "hard-delete",
		}[hardDelete], func(t *testing.T) {
			ctx := context.Background()
			manager, err := NewManager(
				filepath.Join(t.TempDir(), "autogora.db"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Create(ctx, "protected", Update{}); err != nil {
				t.Fatal(err)
			}
			coordination, err := manager.OpenCoordinationStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := coordination.ActivateAutomationQuarantine(
				ctx,
				store.AutomationQuarantineSourceInput{
					Board:             "protected",
					Kind:              "automation_test_source",
					SourceID:          "protected-publication",
					ObservedUpdatedAt: "epoch-one",
					DiagnosticCode:    "process_teardown_unconfirmed",
				},
			); err != nil {
				coordination.Close()
				t.Fatal(err)
			}
			if err := coordination.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Remove(
				"protected",
				hardDelete,
			); !errors.Is(err, store.ErrAutomationQuarantined) {
				t.Fatalf(
					"remove behind global quarantine error = %v",
					err,
				)
			}
			if !manager.Exists("protected") {
				t.Fatal("global quarantine allowed board removal")
			}
			metadata, err := manager.Read("protected")
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Archived {
				t.Fatal("rejected removal marked the board archived")
			}
		})
	}
}

func TestPublishingBoardRemovalFencePrecedesLaterQuarantineActivation(
	t *testing.T,
) {
	ctx := context.Background()
	manager, err := NewManager(filepath.Join(t.TempDir(), "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "publishing", Update{}); err != nil {
		t.Fatal(err)
	}
	claimed := seedPublishingPublication(t, manager, "publishing")
	authority, err := manager.openStoreUnlocked(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	activationStore, err := manager.openStoreUnlocked(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer activationStore.Close()

	entered := make(chan struct{})
	releaseRemoval := make(chan struct{})
	removalDone := make(chan error, 1)
	go func() {
		removalDone <- authority.RunWithAutomationGateOpen(
			ctx,
			func() error {
				close(entered)
				<-releaseRemoval
				_, err := manager.Remove("publishing", false)
				return err
			},
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("board removal fence did not acquire the automation lock")
	}

	activation := store.AutomationQuarantineSourceInput{
		Board:              "publishing",
		Kind:               "publication",
		SourceID:           claimed.ID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: strconv.FormatInt(claimed.ClaimEpoch, 10),
		DiagnosticCode:     "process_teardown_unconfirmed",
		ValidateCurrent: func(
			validateContext context.Context,
			input store.AutomationQuarantineSourceInput,
		) (bool, error) {
			return validateManagerPublishingSource(
				validateContext,
				manager,
				input,
			)
		},
	}
	blockedContext, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if _, _, err := activationStore.ActivateAutomationQuarantine(
		blockedContext,
		activation,
	); !errors.Is(err, context.DeadlineExceeded) {
		close(releaseRemoval)
		<-removalDone
		t.Fatalf("activation overlapped board removal fence: %v", err)
	}
	close(releaseRemoval)
	var busy *store.BoardBusyError
	if err := <-removalDone; !errors.As(err, &busy) ||
		busy.PublishingPublications != 1 {
		t.Fatalf("publishing board removal fence error = %#v", err)
	}
	gate, activated, err := activationStore.ActivateAutomationQuarantine(
		ctx,
		activation,
	)
	if err != nil || !activated || !gate.Active {
		t.Fatalf(
			"activation after rejected removal = %+v, activated=%v, err=%v",
			gate,
			activated,
			err,
		)
	}
	if !manager.Exists("publishing") {
		t.Fatal("rejected removal lost the board")
	}
	boardStore, err := manager.openStoreUnlocked(ctx, "publishing")
	if err != nil {
		t.Fatal(err)
	}
	preserved, readErr := boardStore.GetPublication(ctx, claimed.ID)
	closeErr := boardStore.Close()
	if readErr != nil || closeErr != nil ||
		preserved.Status != model.PublicationPublishing {
		t.Fatalf(
			"publishing evidence = %+v, readErr=%v, closeErr=%v",
			preserved,
			readErr,
			closeErr,
		)
	}
}
