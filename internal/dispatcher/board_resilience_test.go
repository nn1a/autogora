package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func completeFaultInjectionClaim(
	ctx context.Context,
	_ *boards.Manager,
	opened *store.Store,
	claim *model.ClaimedTask,
	_ Options,
	_ *ProcessSet,
	_ string,
) error {
	_, err := opened.CompleteRun(
		ctx,
		store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken},
		store.CompletionInput{Summary: "fault-injection worker completed"},
	)
	return err
}

func newResilientWatchFixture(t *testing.T) (*boards.Manager, string, string) {
	t.Helper()
	manager, dbPath := testManager(t)
	for _, board := range []string{"bad", "healthy"} {
		if _, err := manager.Create(context.Background(), board, boards.Update{}); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := manager.OpenStore(context.Background(), "healthy")
	if err != nil {
		t.Fatal(err)
	}
	assignee := "worker"
	task, createErr := opened.CreateTask(context.Background(), store.CreateTaskInput{
		Title: "healthy board keeps moving", Assignee: &assignee, Runtime: model.RuntimeCodex,
	})
	if closeErr := opened.Close(); createErr != nil || closeErr != nil {
		t.Fatal(errors.Join(createErr, closeErr))
	}
	return manager, dbPath, task.Task.ID
}

func watchFaultOptions(
	dbPath string,
	hooks *dispatcherTestHooks,
	logs *[]string,
	logMu *sync.Mutex,
) Options {
	config := agentconfig.Default()
	return Options{
		DBPath: dbPath, CLIPath: "/tmp/autogora",
		Interval: 250 * time.Millisecond, MaxWorkers: 1,
		AutoDecompose: boolValue(false), AgentConfig: &config,
		Getenv:    func(string) string { return "" },
		testHooks: hooks,
		OnLog: func(message string) {
			logMu.Lock()
			*logs = append(*logs, message)
			logMu.Unlock()
		},
	}
}

func waitForHealthyWatchResult(
	t *testing.T,
	manager *boards.Manager,
	taskID string,
	completed <-chan struct{},
	cancel context.CancelFunc,
	result <-chan error,
) {
	t.Helper()
	select {
	case <-completed:
	case <-time.After(4 * time.Second):
		cancel()
		t.Fatal("healthy board worker was not launched")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("resilient dispatcher stopped on a board-local failure: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("resilient dispatcher did not stop after cancellation")
	}
	opened, err := manager.OpenStore(context.Background(), "healthy")
	if err != nil {
		t.Fatal(err)
	}
	detail, getErr := opened.GetTask(context.Background(), taskID)
	closeErr := opened.Close()
	if getErr != nil || closeErr != nil {
		t.Fatal(errors.Join(getErr, closeErr))
	}
	if detail.Task.Status != model.TaskStatusDone {
		t.Fatalf("healthy task status = %s, want done", detail.Task.Status)
	}
}

func TestWatchDispatcherIsolatesBoardOperationFailures(t *testing.T) {
	tests := []struct {
		name      string
		autopilot bool
		install   func(*dispatcherTestHooks, *atomic.Int32, *[]string, *sync.Mutex)
	}{
		{
			name:      "metadata",
			autopilot: true,
			install: func(hooks *dispatcherTestHooks, failures *atomic.Int32, order *[]string, mu *sync.Mutex) {
				hooks.readMetadata = func(manager *boards.Manager, board string) (boards.Metadata, error) {
					mu.Lock()
					*order = append(*order, board)
					mu.Unlock()
					if board == "bad" {
						failures.Add(1)
						return boards.Metadata{}, errors.New("injected metadata failure")
					}
					return manager.Read(board)
				}
			},
		},
		{
			name: "open store",
			install: func(hooks *dispatcherTestHooks, failures *atomic.Int32, order *[]string, mu *sync.Mutex) {
				hooks.openStore = func(ctx context.Context, manager *boards.Manager, board string) (*store.Store, error) {
					mu.Lock()
					*order = append(*order, board)
					mu.Unlock()
					if board == "bad" {
						failures.Add(1)
						return nil, errors.New("injected open-store failure")
					}
					return manager.OpenStore(ctx, board)
				}
			},
		},
		{
			name: "maintenance",
			install: func(hooks *dispatcherTestHooks, failures *atomic.Int32, order *[]string, mu *sync.Mutex) {
				hooks.maintainBoard = func(ctx context.Context, manager *boards.Manager, board string, options Options) error {
					mu.Lock()
					*order = append(*order, board)
					mu.Unlock()
					if board == "bad" {
						failures.Add(1)
						return errors.New("injected maintenance failure")
					}
					options.testHooks = nil
					return maintainBoard(ctx, manager, board, options)
				}
			},
		},
		{
			name: "profile policy",
			install: func(hooks *dispatcherTestHooks, failures *atomic.Int32, order *[]string, mu *sync.Mutex) {
				hooks.claimProfile = func(
					ctx context.Context,
					manager *boards.Manager,
					opened *store.Store,
					board string,
					options Options,
				) ([]string, map[string]int, error) {
					mu.Lock()
					*order = append(*order, board)
					mu.Unlock()
					if board == "bad" {
						failures.Add(1)
						return nil, nil, errors.New("injected profile failure")
					}
					options.testHooks = nil
					return claimProfilePolicy(ctx, manager, opened, board, options)
				}
			},
		},
		{
			name: "claim",
			install: func(hooks *dispatcherTestHooks, failures *atomic.Int32, order *[]string, mu *sync.Mutex) {
				hooks.claimTask = func(ctx context.Context, opened *store.Store, input store.ClaimOptions) (*model.ClaimedTask, error) {
					mu.Lock()
					*order = append(*order, input.Board)
					mu.Unlock()
					if input.Board == "bad" {
						failures.Add(1)
						return nil, errors.New("injected claim failure")
					}
					return opened.ClaimTask(ctx, input)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, dbPath, taskID := newResilientWatchFixture(t)
			var failures atomic.Int32
			var order []string
			var orderMu sync.Mutex
			hooks := &dispatcherTestHooks{}
			test.install(hooks, &failures, &order, &orderMu)
			completed := make(chan struct{}, 1)
			hooks.runClaim = func(
				ctx context.Context,
				manager *boards.Manager,
				opened *store.Store,
				claim *model.ClaimedTask,
				options Options,
				processes *ProcessSet,
				approvalDir string,
			) error {
				err := completeFaultInjectionClaim(ctx, manager, opened, claim, options, processes, approvalDir)
				if err == nil && claim.Task.Task.Board == "healthy" {
					select {
					case completed <- struct{}{}:
					default:
					}
				}
				return err
			}
			var logs []string
			var logMu sync.Mutex
			options := watchFaultOptions(dbPath, hooks, &logs, &logMu)
			options.Autopilot = test.autopilot
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- Run(ctx, options) }()

			waitForHealthyWatchResult(t, manager, taskID, completed, cancel, result)
			if failures.Load() == 0 {
				t.Fatal("fault hook was not exercised")
			}
			orderMu.Lock()
			observedOrder := append([]string{}, order...)
			orderMu.Unlock()
			badIndex, healthyIndex := -1, -1
			for index, board := range observedOrder {
				if board == "bad" && badIndex < 0 {
					badIndex = index
				}
				if board == "healthy" && healthyIndex < 0 {
					healthyIndex = index
				}
			}
			if badIndex < 0 || healthyIndex < 0 || badIndex >= healthyIndex {
				t.Fatalf("failed board blocked the fairness cursor: operation order = %v", observedOrder)
			}
			logMu.Lock()
			logOutput := strings.Join(logs, "\n")
			logMu.Unlock()
			if !strings.Contains(logOutput, "paused board bad") {
				t.Fatalf("board circuit transition was not logged:\n%s", logOutput)
			}
		})
	}
}

func TestWatchDispatcherIsolatesWorkerFailure(t *testing.T) {
	manager, dbPath := testManager(t)
	for _, board := range []string{"alpha", "beta"} {
		if _, err := manager.Create(context.Background(), board, boards.Update{}); err != nil {
			t.Fatal(err)
		}
		opened, err := manager.OpenStore(context.Background(), board)
		if err != nil {
			t.Fatal(err)
		}
		assignee := "worker"
		_, createErr := opened.CreateTask(context.Background(), store.CreateTaskInput{
			Title: "work on " + board, Assignee: &assignee, Runtime: model.RuntimeCodex,
		})
		if closeErr := opened.Close(); createErr != nil || closeErr != nil {
			t.Fatal(errors.Join(createErr, closeErr))
		}
	}
	completed := make(chan struct{}, 1)
	hooks := &dispatcherTestHooks{
		runClaim: func(
			ctx context.Context,
			manager *boards.Manager,
			opened *store.Store,
			claim *model.ClaimedTask,
			options Options,
			processes *ProcessSet,
			approvalDir string,
		) error {
			if claim.Task.Task.Board == "alpha" {
				return errors.New("injected recoverable worker failure")
			}
			err := completeFaultInjectionClaim(ctx, manager, opened, claim, options, processes, approvalDir)
			if err == nil {
				select {
				case completed <- struct{}{}:
				default:
				}
			}
			return err
		},
	}
	var logs []string
	var logMu sync.Mutex
	options := watchFaultOptions(dbPath, hooks, &logs, &logMu)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, options) }()

	select {
	case <-completed:
	case <-time.After(4 * time.Second):
		cancel()
		t.Fatal("beta worker did not run after alpha worker failed")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("worker-local failure stopped all-board watch: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("dispatcher did not stop")
	}
	beta, err := manager.OpenStore(context.Background(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	tasks, listErr := beta.ListTasks(context.Background(), store.ListTaskFilter{Limit: 10})
	closeErr := beta.Close()
	if listErr != nil || closeErr != nil {
		t.Fatal(errors.Join(listErr, closeErr))
	}
	if len(tasks) != 1 || tasks[0].Status != model.TaskStatusDone {
		t.Fatalf("beta task did not complete: %#v", tasks)
	}
	logMu.Lock()
	logOutput := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(logOutput, "paused board alpha") ||
		!strings.Contains(logOutput, "injected recoverable worker failure") {
		t.Fatalf("worker circuit transition was not logged:\n%s", logOutput)
	}
}

func TestTargetedDispatcherKeepsStrictBoardAndWorkerErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *boards.Manager)
		hooks   func() *dispatcherTestHooks
	}{
		{
			name: "maintenance",
			hooks: func() *dispatcherTestHooks {
				return &dispatcherTestHooks{maintainBoard: func(context.Context, *boards.Manager, string, Options) error {
					return errors.New("strict maintenance failure")
				}}
			},
		},
		{
			name: "profile",
			hooks: func() *dispatcherTestHooks {
				return &dispatcherTestHooks{claimProfile: func(
					context.Context, *boards.Manager, *store.Store, string, Options,
				) ([]string, map[string]int, error) {
					return nil, nil, errors.New("strict profile failure")
				}}
			},
		},
		{
			name: "claim",
			hooks: func() *dispatcherTestHooks {
				return &dispatcherTestHooks{claimTask: func(
					context.Context, *store.Store, store.ClaimOptions,
				) (*model.ClaimedTask, error) {
					return nil, errors.New("strict claim failure")
				}}
			},
		},
		{
			name: "worker",
			prepare: func(t *testing.T, manager *boards.Manager) {
				opened, err := manager.OpenStore(context.Background(), "default")
				if err != nil {
					t.Fatal(err)
				}
				assignee := "worker"
				_, createErr := opened.CreateTask(context.Background(), store.CreateTaskInput{
					Title: "strict worker", Assignee: &assignee, Runtime: model.RuntimeCodex,
				})
				if closeErr := opened.Close(); createErr != nil || closeErr != nil {
					t.Fatal(errors.Join(createErr, closeErr))
				}
			},
			hooks: func() *dispatcherTestHooks {
				return &dispatcherTestHooks{runClaim: func(
					context.Context, *boards.Manager, *store.Store, *model.ClaimedTask, Options, *ProcessSet, string,
				) error {
					return errors.New("strict worker failure")
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, dbPath := testManager(t)
			if test.prepare != nil {
				test.prepare(t, manager)
			}
			config := agentconfig.Default()
			err := Run(context.Background(), Options{
				DBPath: dbPath, CLIPath: "/tmp/autogora", Board: "default",
				Once: true, MaxWorkers: 1, AutoDecompose: boolValue(false),
				AgentConfig: &config, Getenv: func(string) string { return "" },
				testHooks: test.hooks(),
			})
			if err == nil || !strings.Contains(err.Error(), "strict "+test.name+" failure") {
				t.Fatalf("strict %s error = %v", test.name, err)
			}
		})
	}
}

func TestBoardFailureCircuitUsesBoundedExponentialBackoff(t *testing.T) {
	current := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	circuit := newBoardFailureCircuit(time.Second, func() time.Time { return current })
	circuit.limit = 4 * time.Second

	first := circuit.failure("bad")
	if first.Delay != time.Second || circuit.ready("bad") {
		t.Fatalf("first failure = %#v ready=%t", first, circuit.ready("bad"))
	}
	if circuit.success("bad", 0) {
		t.Fatal("stale success cleared a newer board failure")
	}
	current = first.RetryAt
	if !circuit.ready("bad") {
		t.Fatal("board did not become probeable at retry time")
	}
	if !circuit.beginProbe("bad") || circuit.beginProbe("bad") || circuit.ready("bad") {
		t.Fatal("half-open circuit allowed more than one in-flight probe")
	}
	second := circuit.failure("bad")
	current = second.RetryAt
	third := circuit.failure("bad")
	current = third.RetryAt
	fourth := circuit.failure("bad")
	if second.Delay != 2*time.Second || third.Delay != 4*time.Second || fourth.Delay != 4*time.Second {
		t.Fatalf("backoff sequence = [%s %s %s %s]", first.Delay, second.Delay, third.Delay, fourth.Delay)
	}
	if circuit.success("bad", third.Generation) {
		t.Fatal("older probe generation cleared the latest failure")
	}
	if !circuit.success("bad", fourth.Generation) || !circuit.ready("bad") {
		t.Fatal("latest successful probe did not close the circuit")
	}
}

func TestGlobalCoordinationFailureBypassesBoardCircuit(t *testing.T) {
	_, dbPath, _ := newResilientWatchFixture(t)
	hooks := &dispatcherTestHooks{
		claimProfile: func(
			_ context.Context,
			_ *boards.Manager,
			_ *store.Store,
			board string,
			_ Options,
		) ([]string, map[string]int, error) {
			if board == "bad" {
				return nil, nil, markGlobalCoordinationError(
					"fault injection", errors.New("shared coordination unavailable"),
				)
			}
			return nil, nil, nil
		},
	}
	var logs []string
	var logMu sync.Mutex
	err := Run(context.Background(), watchFaultOptions(dbPath, hooks, &logs, &logMu))
	if err == nil ||
		!strings.Contains(err.Error(), "shared coordination unavailable") ||
		!strings.Contains(err.Error(), "resolve claim profiles for board bad") {
		t.Fatalf("global coordination error = %v", err)
	}
	if fmt.Sprint(logs) == "" {
		// The assertion keeps the log callback race detector-visible; a global
		// failure is intentionally returned rather than logged as a board pause.
		t.Log("dispatcher stopped before producing an informational log")
	}
}
