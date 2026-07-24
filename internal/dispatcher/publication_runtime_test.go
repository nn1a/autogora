package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/store"
)

func configurePublicationBoard(
	t *testing.T,
	manager *boards.Manager,
	board string,
	mode boards.PublicationMode,
	requireApproval bool,
	workspaceWrites bool,
) {
	t.Helper()
	enabled := true
	target, remote := "main", "origin"
	if _, err := manager.Update(board, boards.Update{
		Orchestration: &boards.OrchestrationUpdate{
			Autopilot: &boards.AutopilotUpdate{
				Enabled:         &enabled,
				WorkspaceWrites: &workspaceWrites,
				Publication: &boards.PublicationUpdate{
					Mode: &mode, TargetBranch: &target, Remote: &remote,
					RequireApproval: &requireApproval,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func createCompletedFinalizerChangeSet(
	t *testing.T,
	manager *boards.Manager,
	board string,
	suffix string,
	state string,
) (model.Task, model.ChangeSet) {
	t.Helper()
	ctx := context.Background()
	opened, err := manager.OpenStore(ctx, board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assignee := "publisher-test"
	detail, err := opened.CreateTask(ctx, store.CreateTaskInput{
		Title: "publication " + suffix, Board: board,
		Status: model.TaskStatusReady, Runtime: model.RuntimeCodex,
		WorkflowRole: model.WorkflowRoleFinalizer, Assignee: &assignee,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(ctx, store.ClaimOptions{
		TaskID: detail.Task.ID, Board: board, WorkerID: "publisher-test",
		ClaimTTLSeconds: 60,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim finalizer: claim=%v err=%v", claim, err)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RequestRunCompletion(
		ctx, scope, store.CompletionInput{Summary: "ready to publish"},
	); err != nil {
		t.Fatal(err)
	}
	changeSet, err := opened.RecordRunChangeSet(
		ctx,
		scope,
		store.RecordChangeSetInput{
			RunID: claim.Run.ID, RepositoryPath: filepath.Join("/repo", suffix),
			WorktreePath: filepath.Join("/worktree", suffix),
			BaseCommit:   "base-" + suffix, HeadCommit: "head-" + suffix,
			DurableRef: "refs/autogora/runs/" + claim.Run.ID,
			State:      state, ChangedFiles: []string{suffix + ".go"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := opened.FinalizeRunTerminal(ctx, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	return finalized.Task, changeSet
}

func publicationForChangeSet(
	t *testing.T,
	manager *boards.Manager,
	board string,
	changeSetID string,
) model.Publication {
	t.Helper()
	opened, err := manager.OpenStore(context.Background(), board)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	value, err := opened.GetPublicationByChangeSet(context.Background(), changeSetID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func publicationTestOptions(
	current time.Time,
	executor PublicationExecutor,
) Options {
	options := Options{
		Autopilot: true, AllowWrites: true, Now: func() time.Time { return current },
		PublicationTimeout: 5 * time.Second, PublicationExecutor: executor,
	}
	options.normalize()
	return options
}

func publishedResult(value model.Publication) publisher.Result {
	return publisher.Result{
		Status: publisher.ResultPublished, Mode: value.Mode,
		HeadCommit: value.HeadCommit, TargetBranch: value.TargetBranch,
	}
}

func TestPublicationOptionsKeepLeaseBeyondExecutionTimeout(t *testing.T) {
	options := Options{
		PublicationTimeout:  store.MaxPublicationClaimTTL,
		PublicationClaimTTL: store.MinPublicationClaimTTL,
	}
	options.normalize()
	if options.PublicationTimeout != store.MaxPublicationClaimTTL-publicationClaimGrace ||
		options.PublicationClaimTTL != store.MaxPublicationClaimTTL ||
		options.PublicationExecutor == nil {
		t.Fatalf(
			"timeout=%s claimTTL=%s executor=%v",
			options.PublicationTimeout,
			options.PublicationClaimTTL,
			options.PublicationExecutor != nil,
		)
	}
}

func TestPublicationPassRecordsManualApprovalAndNoChangeWithoutExecuting(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	for _, test := range []struct {
		name            string
		mode            boards.PublicationMode
		requireApproval bool
		changeState     string
		wantStatus      model.PublicationStatus
	}{
		{
			name: "manual", mode: boards.PublicationModeManual,
			changeState: "ready", wantStatus: model.PublicationPending,
		},
		{
			name: "approval", mode: boards.PublicationModeLocalFF,
			requireApproval: true, changeState: "ready",
			wantStatus: model.PublicationAwaitingApproval,
		},
		{
			name: "no change", mode: boards.PublicationModeLocalFF,
			changeState: "no_change", wantStatus: model.PublicationNoChange,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := testManager(t)
			configurePublicationBoard(
				t, manager, "default", test.mode, test.requireApproval, true,
			)
			_, changeSet := createCompletedFinalizerChangeSet(
				t, manager, "default", test.name, test.changeState,
			)
			calls := 0
			options := publicationTestOptions(
				current,
				func(
					context.Context,
					model.Publication,
					publisher.Options,
				) (publisher.Result, error) {
					calls++
					return publisher.Result{}, nil
				},
			)
			if err := runPublicationPass(
				ctx, manager, []string{"default"}, options,
				&publicationRuntimeState{}, current,
			); err != nil {
				t.Fatal(err)
			}
			value := publicationForChangeSet(
				t, manager, "default", changeSet.ID,
			)
			if value.Status != test.wantStatus || calls != 0 {
				t.Fatalf("publication=%+v calls=%d", value, calls)
			}
			var policy boards.PublicationSettings
			if err := json.Unmarshal(value.PolicySnapshot, &policy); err != nil {
				t.Fatal(err)
			}
			if policy.Mode != test.mode ||
				policy.RequireApproval != test.requireApproval ||
				policy.TargetBranch != "main" || policy.Remote != "origin" {
				t.Fatalf("policy snapshot=%+v", policy)
			}
		})
	}
}

func TestPublicationPassApprovalExecutesOnlyAfterExplicitApproval(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, true, true,
	)
	_, changeSet := createCompletedFinalizerChangeSet(
		t, manager, "default", "approved", "ready",
	)
	calls := 0
	options := publicationTestOptions(
		current,
		func(
			_ context.Context,
			value model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			calls++
			return publishedResult(value), nil
		},
	)
	state := &publicationRuntimeState{}
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options, state, current,
	); err != nil {
		t.Fatal(err)
	}
	awaiting := publicationForChangeSet(t, manager, "default", changeSet.ID)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ApprovePublication(
		ctx, awaiting.ID,
		store.ApprovePublicationInput{ExpectedUpdatedAt: awaiting.UpdatedAt},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options, state, current,
	); err != nil {
		t.Fatal(err)
	}
	value := publicationForChangeSet(t, manager, "default", changeSet.ID)
	if value.Status != model.PublicationPublished || calls != 1 {
		t.Fatalf("publication=%+v calls=%d", value, calls)
	}
}

func TestPublicationPassPersistsExecutionSuccessAndFailure(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	for _, test := range []struct {
		name        string
		executeErr  error
		wantStatus  model.PublicationStatus
		wantPassErr bool
	}{
		{name: "success", wantStatus: model.PublicationPublished},
		{
			name: "failure", executeErr: errors.New("remote rejected"),
			wantStatus: model.PublicationFailed, wantPassErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := testManager(t)
			configurePublicationBoard(
				t, manager, "default", boards.PublicationModePullRequest,
				false, true,
			)
			_, changeSet := createCompletedFinalizerChangeSet(
				t, manager, "default", test.name, "ready",
			)
			url := "https://example.invalid/pull/1"
			options := publicationTestOptions(
				current,
				func(
					_ context.Context,
					value model.Publication,
					_ publisher.Options,
				) (publisher.Result, error) {
					if test.executeErr != nil {
						return publisher.Result{}, test.executeErr
					}
					result := publishedResult(value)
					result.URL = &url
					return result, nil
				},
			)
			err := runPublicationPass(
				ctx, manager, []string{"default"}, options,
				&publicationRuntimeState{}, current,
			)
			if (err != nil) != test.wantPassErr {
				t.Fatalf("pass error=%v, want error=%v", err, test.wantPassErr)
			}
			value := publicationForChangeSet(
				t, manager, "default", changeSet.ID,
			)
			if value.Status != test.wantStatus {
				t.Fatalf("publication=%+v", value)
			}
			if test.wantStatus == model.PublicationPublished &&
				(value.URL == nil || *value.URL != url) {
				t.Fatalf("publication URL=%v", value.URL)
			}
			if test.wantStatus == model.PublicationFailed &&
				(value.Error == nil || *value.Error != test.executeErr.Error()) {
				t.Fatalf("publication error=%v", value.Error)
			}
		})
	}
}

func TestPublicationPassPersistsFailureAfterDispatcherCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, true,
	)
	_, changeSet := createCompletedFinalizerChangeSet(
		t, manager, "default", "canceled", "ready",
	)
	options := publicationTestOptions(
		current,
		func(
			executionContext context.Context,
			_ model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			cancel()
			<-executionContext.Done()
			return publisher.Result{}, executionContext.Err()
		},
	)
	err := runPublicationPass(
		ctx, manager, []string{"default"}, options,
		&publicationRuntimeState{}, current,
	)
	if err == nil {
		t.Fatal("canceled publication did not report an execution error")
	}
	value := publicationForChangeSet(t, manager, "default", changeSet.ID)
	if value.Status != model.PublicationFailed ||
		value.Error == nil ||
		!strings.Contains(*value.Error, "canceled") {
		t.Fatalf("publication=%+v", value)
	}
}

func TestPublicationPassWriteAndAutopilotGates(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	for _, test := range []struct {
		name           string
		processEnabled bool
		processWrites  bool
		boardEnabled   bool
		boardWrites    bool
		wantRecord     bool
	}{
		{
			name: "process autopilot off", processWrites: true,
			boardEnabled: true, boardWrites: true,
		},
		{
			name: "board autopilot off", processEnabled: true,
			processWrites: true, boardWrites: true,
		},
		{
			name: "process writes off", processEnabled: true,
			boardEnabled: true, boardWrites: true, wantRecord: true,
		},
		{
			name: "board writes off", processEnabled: true,
			processWrites: true, boardEnabled: true, wantRecord: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := testManager(t)
			configurePublicationBoard(
				t, manager, "default", boards.PublicationModeLocalFF,
				false, test.boardWrites,
			)
			if !test.boardEnabled {
				disabled := false
				if _, err := manager.Update(
					"default",
					boards.Update{
						Orchestration: &boards.OrchestrationUpdate{
							Autopilot: &boards.AutopilotUpdate{Enabled: &disabled},
						},
					},
				); err != nil {
					t.Fatal(err)
				}
			}
			_, changeSet := createCompletedFinalizerChangeSet(
				t, manager, "default", test.name, "ready",
			)
			calls := 0
			options := publicationTestOptions(
				current,
				func(
					_ context.Context,
					value model.Publication,
					_ publisher.Options,
				) (publisher.Result, error) {
					calls++
					return publishedResult(value), nil
				},
			)
			options.Autopilot = test.processEnabled
			options.AllowWrites = test.processWrites
			if err := runPublicationPass(
				ctx, manager, []string{"default"}, options,
				&publicationRuntimeState{}, current,
			); err != nil {
				t.Fatal(err)
			}
			opened, err := manager.OpenStore(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}
			value, getErr := opened.GetPublicationByChangeSet(ctx, changeSet.ID)
			closeErr := opened.Close()
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if test.wantRecord {
				if getErr != nil || value.Status != model.PublicationPending {
					t.Fatalf("publication=%+v err=%v", value, getErr)
				}
			} else if !errors.Is(getErr, store.ErrPublicationNotFound) {
				t.Fatalf("publication unexpectedly recorded: %+v err=%v", value, getErr)
			}
			if calls != 0 {
				t.Fatalf("executor calls=%d", calls)
			}
		})
	}
}

func TestPublicationPassDoesNotTakeOverExpiredLease(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, false,
	)
	_, changeSet := createCompletedFinalizerChangeSet(
		t, manager, "default", "takeover", "ready",
	)
	calls := 0
	options := publicationTestOptions(
		current,
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			calls++
			return publisher.Result{}, errors.New("unexpected execution")
		},
	)
	state := &publicationRuntimeState{}
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options, state, current,
	); err != nil {
		t.Fatal(err)
	}
	pending := publicationForChangeSet(t, manager, "default", changeSet.ID)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := opened.ClaimPublication(
		ctx, pending.ID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               store.MinPublicationClaimTTL,
		},
	); err != nil || !acquired {
		opened.Close()
		t.Fatalf("seed expired claim: acquired=%v err=%v", acquired, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, true,
	)
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options, state, current,
	); err != nil {
		t.Fatal(err)
	}
	live := publicationForChangeSet(t, manager, "default", changeSet.ID)
	if live.Status != model.PublicationPublishing || calls != 0 {
		t.Fatalf("live publication=%+v calls=%d", live, calls)
	}
	later := current.Add(store.MinPublicationClaimTTL)
	options.Now = func() time.Time { return later }
	err = runPublicationPass(
		ctx, manager, []string{"default"}, options, state, later,
	)
	if err != nil {
		t.Fatal(err)
	}
	stillPublishing := publicationForChangeSet(t, manager, "default", changeSet.ID)
	if stillPublishing.Status != model.PublicationPublishing {
		t.Fatalf("publication=%+v", stillPublishing)
	}
	if calls != 0 {
		t.Fatalf("expired lease executor calls=%d", calls)
	}
}

func TestPublicationPassExecutesOneBoardPerPassWithRoundRobinFairness(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	if _, err := manager.Create(ctx, "alpha", boards.Update{}); err != nil {
		t.Fatal(err)
	}
	for _, board := range []string{"default", "alpha"} {
		configurePublicationBoard(
			t, manager, board, boards.PublicationModeLocalFF, false, true,
		)
		createCompletedFinalizerChangeSet(
			t, manager, board, "fair-"+board, "ready",
		)
	}
	var executed []string
	options := publicationTestOptions(
		current,
		func(
			_ context.Context,
			value model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			executed = append(executed, value.Board)
			return publishedResult(value), nil
		},
	)
	state := &publicationRuntimeState{}
	for range 2 {
		if err := runPublicationPass(
			ctx, manager, []string{"default", "alpha"}, options, state, current,
		); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(executed) != "[default alpha]" {
		t.Fatalf("execution order=%v", executed)
	}
}

func TestPublicationPassPaginatesDoneFinalizers(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeManual, false, true,
	)
	for index := 0; index < publicationCandidatePageSize+1; index++ {
		createCompletedFinalizerChangeSet(
			t, manager, "default", fmt.Sprintf("page-%03d", index), "ready",
		)
	}
	options := publicationTestOptions(
		current,
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			t.Fatal("manual publication executed")
			return publisher.Result{}, nil
		},
	)
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options,
		&publicationRuntimeState{}, current,
	); err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	publications, err := opened.ListPublications(
		ctx, store.PublicationFilter{Limit: publicationCandidatePageSize + 1},
	)
	closeErr := opened.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if len(publications) != publicationCandidatePageSize+1 {
		t.Fatalf(
			"publications=%d, want %d",
			len(publications), publicationCandidatePageSize+1,
		)
	}
}

func TestPublicationPassUsesOnlyLatestFinalizerChangeSet(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeManual, false, true,
	)
	task, first := createCompletedFinalizerChangeSet(
		t, manager, "default", "first-completion", "ready",
	)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	ready := model.TaskStatusReady
	if _, err := opened.UpdateTask(
		ctx, task.ID, store.UpdateTaskInput{Status: &ready},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(
		ctx,
		store.ClaimOptions{
			TaskID: task.ID, Board: "default", WorkerID: "publisher-test",
			ClaimTTLSeconds: 60,
		},
	)
	if err != nil || claim == nil {
		reopened, _ := opened.GetTask(ctx, task.ID)
		opened.Close()
		t.Fatalf(
			"reclaim finalizer: claim=%v err=%v status=%s",
			claim, err, reopened.Task.Status,
		)
	}
	scope := store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}
	if err := opened.MarkRunManaged(ctx, scope); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := opened.RequestRunCompletion(
		ctx, scope, store.CompletionInput{Summary: "second completion"},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	second, err := opened.RecordRunChangeSet(
		ctx,
		scope,
		store.RecordChangeSetInput{
			RunID: claim.Run.ID, RepositoryPath: "/repo/latest",
			WorktreePath: "/worktree/latest", BaseCommit: "base-latest",
			HeadCommit: "head-latest",
			DurableRef: "refs/autogora/runs/" + claim.Run.ID,
			State:      "ready", ChangedFiles: []string{"latest.go"},
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if _, err := opened.FinalizeRunTerminal(ctx, scope, 0); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	options := publicationTestOptions(
		current,
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			t.Fatal("manual publication executed")
			return publisher.Result{}, nil
		},
	)
	if err := runPublicationPass(
		ctx, manager, []string{"default"}, options,
		&publicationRuntimeState{}, current,
	); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	_, firstErr := check.GetPublicationByChangeSet(ctx, first.ID)
	latest, latestErr := check.GetPublicationByChangeSet(ctx, second.ID)
	closeErr := check.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(firstErr, store.ErrPublicationNotFound) ||
		latestErr != nil || latest.ChangeSetID != second.ID {
		t.Fatalf(
			"first error=%v latest=%+v latest error=%v",
			firstErr, latest, latestErr,
		)
	}
}

func TestPublicationPassContinuesAfterBoardError(t *testing.T) {
	ctx := context.Background()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, true,
	)
	_, changeSet := createCompletedFinalizerChangeSet(
		t, manager, "default", "after-error", "ready",
	)
	calls := 0
	options := publicationTestOptions(
		current,
		func(
			_ context.Context,
			value model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			calls++
			return publishedResult(value), nil
		},
	)
	err := runPublicationPass(
		ctx, manager, []string{"missing", "default"}, options,
		&publicationRuntimeState{}, current,
	)
	if err == nil || !strings.Contains(err.Error(), "publication board missing") {
		t.Fatalf("pass error=%v", err)
	}
	value := publicationForChangeSet(t, manager, "default", changeSet.ID)
	if value.Status != model.PublicationPublished || calls != 1 {
		t.Fatalf("publication=%+v calls=%d", value, calls)
	}
}

func TestPublicationQueueCoalescesPendingPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := time.Now().UTC()
	manager, _ := testManager(t)
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, true,
	)
	createCompletedFinalizerChangeSet(
		t, manager, "default", "queue", "ready",
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	options := publicationTestOptions(
		current,
		func(
			_ context.Context,
			value model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			once.Do(func() { close(started) })
			<-release
			return publishedResult(value), nil
		},
	)
	queue := startPublicationQueue(ctx, manager, options)
	first := queue.Enqueue([]string{"default"})
	if first == nil {
		t.Fatal("first pass was not queued")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publication did not start")
	}
	second := queue.Enqueue([]string{"default"})
	third := queue.Enqueue([]string{"default"})
	if second == nil || third != nil {
		t.Fatalf("coalescing second=%v third=%v", second, third)
	}
	close(release)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first publication pass did not finish")
	}
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("pending publication pass did not finish")
	}
	cancel()
	if !queue.Wait(time.Second) {
		t.Fatal("publication queue did not stop")
	}
}

func TestOncePublishesFinalizerCompletedByItsWorker(t *testing.T) {
	ctx := context.Background()
	manager, dbPath := testManager(t)
	repository := gitRepositoryFixture(t)
	if _, err := manager.Update(
		"default",
		boards.Update{
			DefaultWorkdir: store.OptionalString{Set: true, Value: &repository},
		},
	); err != nil {
		t.Fatal(err)
	}
	configurePublicationBoard(
		t, manager, "default", boards.PublicationModeLocalFF, false, true,
	)
	opened, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "finalizer"
	task, err := opened.CreateTask(
		ctx,
		store.CreateTaskInput{
			Title: "finish and publish", Assignee: &assignee,
			Runtime: model.RuntimeCline, WorkflowRole: model.WorkflowRoleFinalizer,
		},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	cliPath := buildAutogora(t)
	worker := executableFixture(
		t,
		`printf '%s\n' 'published' > "$AUTOGORA_WORKSPACE/result.txt"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "finalized" >/dev/null`,
	)
	calls := 0
	if err := Run(
		ctx,
		Options{
			DBPath: dbPath, CLIPath: cliPath, Board: "default",
			TaskID: task.Task.ID, Once: true, Autopilot: true, AllowWrites: true,
			AutoDecompose: boolValue(false), Interval: 250 * time.Millisecond,
			Getenv: func(name string) string {
				if name == "AUTOGORA_CLINE_BIN" {
					return worker
				}
				return ""
			},
			PublicationExecutor: func(
				_ context.Context,
				value model.Publication,
				_ publisher.Options,
			) (publisher.Result, error) {
				calls++
				return publishedResult(value), nil
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	check, err := manager.OpenStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := check.GetTask(ctx, task.Task.ID)
	if err != nil {
		check.Close()
		t.Fatal(err)
	}
	publications, err := check.ListPublications(
		ctx, store.PublicationFilter{TaskID: task.Task.ID},
	)
	closeErr := check.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if detail.Task.Status != model.TaskStatusDone ||
		len(detail.ChangeSets) != 1 ||
		len(publications) != 1 ||
		publications[0].Status != model.PublicationPublished ||
		calls != 1 {
		t.Fatalf(
			"task=%s changes=%d publications=%+v calls=%d",
			detail.Task.Status, len(detail.ChangeSets), publications, calls,
		)
	}
}
