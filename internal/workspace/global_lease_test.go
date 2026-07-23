package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

func createClaimedDirTask(t *testing.T, ctx context.Context, opened *store.Store, title, path string) *model.ClaimedTask {
	t.Helper()
	task, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: title, Assignee: stringValue("worker"), Runtime: model.RuntimeCodex,
		Workspace: &path, WorkspaceKind: model.WorkspaceDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim %s: %+v, %v", title, claim, err)
	}
	return claim
}

func globalLeaseTestRepository(t *testing.T, root string) (string, string) {
	t.Helper()
	repository := filepath.Join(root, "repository")
	for _, directory := range []string{repository, filepath.Join(repository, "one"), filepath.Join(repository, "two")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("global lease fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Autogora", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Autogora", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init")
	git("add", "README.md")
	git("commit", "-m", "fixture")
	alias := filepath.Join(root, "repository-alias")
	if err := os.Symlink(repository, alias); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	return repository, alias
}

func TestGlobalWritableDirLeaseCoordinatesDefaultBoardGitRootAndSymlink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	defaultStore, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer defaultStore.Close()
	alphaStore, err := manager.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alphaStore.Close()

	repository, alias := globalLeaseTestRepository(t, root)
	defaultClaim := createClaimedDirTask(t, ctx, defaultStore, "default board writer", filepath.Join(repository, "one"))
	alphaClaim := createClaimedDirTask(t, ctx, alphaStore, "alpha board writer", filepath.Join(alias, "two"))
	defaultWorkspaces, alphaWorkspaces := New(manager), New(manager)
	defaultWorkspaces.SetAllowWrites(true)
	alphaWorkspaces.SetAllowWrites(true)
	prepared, err := defaultWorkspaces.Prepare(ctx, defaultStore, defaultClaim)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Workspace == nil || prepared.Workspace.RepositoryPath == nil {
		t.Fatalf("git repository root was not recorded: %+v", prepared.Workspace)
	}
	if _, err := alphaWorkspaces.Prepare(ctx, alphaStore, alphaClaim); !errors.Is(err, store.ErrResourceBusy) {
		t.Fatalf("symlink alias on another board was not globally blocked: %v", err)
	} else {
		var busy *store.ResourceBusyError
		if !errors.As(err, &busy) || busy.OwnerBoard != "default" || busy.OwnerRunID != defaultClaim.Run.ID {
			t.Fatalf("global owner was not reported: %+v", busy)
		}
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := coordination.ListGlobalWorkspaceLeases(ctx)
	coordination.Close()
	canonicalRepository, _ := canonicalPath(repository)
	if err != nil || len(leases) != 1 || leases[0].Board != "default" || leases[0].Path != canonicalRepository {
		t.Fatalf("global repository lease = %+v, err=%v", leases, err)
	}

	process := exec.Command(os.Args[0], "-test.run=TestWorkspaceProcessHelper")
	process.Env = append(os.Environ(), "AUTOGORA_WORKSPACE_HELPER=1")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = process.Process.Kill()
		_, _ = process.Process.Wait()
	}()
	identity, err := processidentity.Capture(process.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.RunScope{RunID: defaultClaim.Run.ID, ClaimToken: defaultClaim.ClaimToken}
	if _, err := defaultStore.RecordSpawnWithIdentity(ctx, scope, process.Process.Pid, filepath.Join(t.TempDir(), "worker.log"), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultStore.FailRun(ctx, scope, "finished", store.FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := alphaWorkspaces.Prepare(ctx, alphaStore, alphaClaim); !errors.Is(err, store.ErrResourceBusy) {
		t.Fatalf("terminal run with a live process released the global lease: %v", err)
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := alphaWorkspaces.Prepare(ctx, alphaStore, alphaClaim); err != nil {
		t.Fatalf("terminal global owner was not safely replaced: %v", err)
	}
	coordination, err = manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	leases, err = coordination.ListGlobalWorkspaceLeases(ctx)
	coordination.Close()
	if err != nil || len(leases) != 1 || leases[0].Board != "alpha" || leases[0].RunID != alphaClaim.Run.ID {
		t.Fatalf("replacement global lease = %+v, err=%v", leases, err)
	}
}

func TestWorkspaceProcessHelper(t *testing.T) {
	if os.Getenv("AUTOGORA_WORKSPACE_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestReadOnlyDirWorkspaceCanRunAlongsideGlobalWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(ctx, "reader", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	defaultStore, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer defaultStore.Close()
	readerStore, err := manager.OpenStore(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	defer readerStore.Close()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	writerClaim := createClaimedDirTask(t, ctx, defaultStore, "writer", shared)
	readerClaim := createClaimedDirTask(t, ctx, readerStore, "reader", shared)
	writer := New(manager)
	writer.SetAllowWrites(true)
	if _, err := writer.Prepare(ctx, defaultStore, writerClaim); err != nil {
		t.Fatal(err)
	}
	if _, err := New(manager).Prepare(ctx, readerStore, readerClaim); err != nil {
		t.Fatalf("read-only workspace was blocked by writer: %v", err)
	}
	local, err := readerStore.ListResourceLeases(ctx)
	if err != nil || len(local) != 0 {
		t.Fatalf("read-only run acquired a local write lease: %+v, %v", local, err)
	}
	coordination, err := manager.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	global, err := coordination.ListGlobalWorkspaceLeases(ctx)
	coordination.Close()
	if err != nil || len(global) != 1 || global[0].RunID != writerClaim.Run.ID {
		t.Fatalf("read-only run changed global ownership: %+v, %v", global, err)
	}
}

func TestGlobalWritableDirLeaseKeepsUnverifiableOwnerBusy(t *testing.T) {
	for _, test := range []struct {
		name        string
		ownerBoard  string
		createOwner bool
	}{
		{name: "unknown board", ownerBoard: "missing-board"},
		{name: "missing run", ownerBoard: "owner", createOwner: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			manager, err := boards.NewManager(filepath.Join(root, "autogora.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Create(ctx, "claimant", boards.Update{}); err != nil {
				t.Fatal(err)
			}
			if test.createOwner {
				if _, err := manager.Create(ctx, test.ownerBoard, boards.Update{}); err != nil {
					t.Fatal(err)
				}
			}
			shared := filepath.Join(root, "shared")
			if err := os.MkdirAll(shared, 0o755); err != nil {
				t.Fatal(err)
			}
			coordination, err := manager.OpenCoordinationStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ownerLease, acquired, err := coordination.AcquireGlobalWorkspaceLease(ctx, test.ownerBoard, "missing-run", shared)
			coordination.Close()
			if err != nil || !acquired {
				t.Fatalf("seed unverifiable owner: %+v, %v, %v", ownerLease, acquired, err)
			}
			claimant, err := manager.OpenStore(ctx, "claimant")
			if err != nil {
				t.Fatal(err)
			}
			defer claimant.Close()
			claim := createClaimedDirTask(t, ctx, claimant, "claimant writer", shared)
			workspaces := New(manager)
			workspaces.SetAllowWrites(true)
			if _, err := workspaces.Prepare(ctx, claimant, claim); !errors.Is(err, store.ErrResourceBusy) {
				t.Fatalf("unverifiable owner was not kept busy: %v", err)
			}
			coordination, err = manager.OpenCoordinationStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			leases, err := coordination.ListGlobalWorkspaceLeases(ctx)
			coordination.Close()
			if err != nil || len(leases) != 1 || leases[0].LeaseToken != ownerLease.LeaseToken {
				t.Fatalf("unverifiable owner was changed: %+v, %v", leases, err)
			}
		})
	}
}

func TestGlobalWritableDirLeaseIsAtomicAcrossManagersAndConnections(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "autogora.db")
	seed, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, board := range []string{"alpha", "beta"} {
		if _, err := seed.Create(ctx, board, boards.Update{}); err != nil {
			t.Fatal(err)
		}
	}
	coordination, err := seed.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coordination.Close()
	managerA, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := boards.NewManager(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	alphaStore, err := managerA.OpenStore(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer alphaStore.Close()
	betaStore, err := managerB.OpenStore(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	defer betaStore.Close()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	claims := map[string]*model.ClaimedTask{
		"alpha": createClaimedDirTask(t, ctx, alphaStore, "alpha writer", shared),
		"beta":  createClaimedDirTask(t, ctx, betaStore, "beta writer", shared),
	}
	workspaces := map[string]*Manager{"alpha": New(managerA), "beta": New(managerB)}
	for _, manager := range workspaces {
		manager.SetAllowWrites(true)
	}
	type result struct {
		board string
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for board, opened := range map[string]*store.Store{"alpha": alphaStore, "beta": betaStore} {
		board, opened := board, opened
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := workspaces[board].Prepare(ctx, opened, claims[board])
			results <- result{board: board, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	winner := ""
	busyCount := 0
	for result := range results {
		switch {
		case result.err == nil:
			if winner != "" {
				t.Fatalf("both managers acquired the global lease: %s and %s", winner, result.board)
			}
			winner = result.board
		case errors.Is(result.err, store.ErrResourceBusy):
			busyCount++
		default:
			t.Fatalf("prepare %s: %v", result.board, result.err)
		}
	}
	if winner == "" || busyCount != 1 {
		t.Fatalf("global race winner=%q busy=%d", winner, busyCount)
	}
	coordination, err = seed.OpenCoordinationStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	global, err := coordination.ListGlobalWorkspaceLeases(ctx)
	coordination.Close()
	if err != nil || len(global) != 1 || global[0].Board != winner || global[0].RunID != claims[winner].Run.ID {
		t.Fatalf("global race lease = %+v, winner=%s, err=%v", global, winner, err)
	}
}
