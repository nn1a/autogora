package boards

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/store"
)

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
			Kind:              "publication",
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
			Kind:              "publication",
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
			Kind:              "publication",
			SourceID:          "canonical-lock-publication",
			ObservedUpdatedAt: "epoch-one",
			DiagnosticCode:    "process_teardown_unconfirmed",
		},
	); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same authority used different automation locks: %v", err)
	}
}
