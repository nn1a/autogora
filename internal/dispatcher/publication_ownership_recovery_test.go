package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/store"
)

type cappedPublicationAutomationAuthority struct {
	*fakeAutomationSessionAuthority
	mu        sync.Mutex
	limit     int
	persisted []store.AutomationQuarantineSourceInput
}

type mutateBeforePublicationActivationAuthority struct {
	*store.Store
	once        sync.Once
	mutate      func(context.Context) error
	mutationErr error
}

func (a *mutateBeforePublicationActivationAuthority) ActivateAutomationQuarantine(
	ctx context.Context,
	input store.AutomationQuarantineSourceInput,
) (store.AutomationQuarantine, bool, error) {
	if input.Kind == publicationQuarantineKind {
		a.once.Do(func() {
			if a.mutate != nil {
				a.mutationErr = a.mutate(ctx)
			}
		})
		if a.mutationErr != nil {
			return store.AutomationQuarantine{}, false, a.mutationErr
		}
	}
	return a.Store.ActivateAutomationQuarantine(ctx, input)
}

func (a *cappedPublicationAutomationAuthority) ActivateAutomationQuarantine(
	ctx context.Context,
	input store.AutomationQuarantineSourceInput,
) (store.AutomationQuarantine, bool, error) {
	a.mu.Lock()
	if len(a.persisted) >= a.limit {
		a.mu.Unlock()
		a.addCall("activate")
		a.fakeAutomationSessionAuthority.mu.Lock()
		gate := a.fakeAutomationSessionAuthority.gate
		a.fakeAutomationSessionAuthority.mu.Unlock()
		return gate, false, errors.New(
			"automation quarantine has too many active sources",
		)
	}
	a.mu.Unlock()
	gate, activated, err := a.fakeAutomationSessionAuthority.
		ActivateAutomationQuarantine(ctx, input)
	if err == nil {
		a.mu.Lock()
		a.persisted = append(a.persisted, input)
		a.mu.Unlock()
	}
	return gate, activated, err
}

func (a *cappedPublicationAutomationAuthority) persistedSources() []store.AutomationQuarantineSourceInput {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]store.AutomationQuarantineSourceInput(nil), a.persisted...)
}

func seedPublishingPublications(
	t *testing.T,
	manager *boards.Manager,
	board string,
	count int,
) []model.Publication {
	t.Helper()
	configurePublicationBoard(
		t,
		manager,
		board,
		boards.PublicationModeLocalFF,
		false,
		true,
	)
	for index := 0; index < count; index++ {
		createCompletedFinalizerChangeSet(
			t,
			manager,
			board,
			fmt.Sprintf("ownership-%03d", index),
			"ready",
		)
	}
	metadata, err := manager.Read(board)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := manager.OpenStore(context.Background(), board)
	if err != nil {
		t.Fatal(err)
	}
	publications, err := ensureBoardPublications(
		context.Background(),
		opened,
		board,
		metadata.Orchestration.Autopilot.Publication,
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if len(publications) != count {
		opened.Close()
		t.Fatalf("publications=%d want=%d", len(publications), count)
	}
	claimed := make([]model.Publication, 0, count)
	for _, publication := range publications {
		value, acquired, err := opened.ClaimPublication(
			context.Background(),
			publication.ID,
			store.ClaimPublicationInput{
				ExpectedUpdatedAt: publication.UpdatedAt,
				TTL:               time.Minute,
			},
		)
		if err != nil || !acquired {
			opened.Close()
			t.Fatalf(
				"claim publication %s: acquired=%t err=%v",
				publication.ID,
				acquired,
				err,
			)
		}
		claimed = append(claimed, value)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func publicationSources(
	t *testing.T,
	manager *boards.Manager,
	filter store.AutomationQuarantineSourceFilter,
) []store.AutomationQuarantineSource {
	t.Helper()
	opened, err := manager.OpenCoordinationStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	values, err := opened.ListAutomationQuarantineSources(
		context.Background(),
		filter,
	)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestDispatcherStartupQuarantinesActiveAndArchivedPublishingOwnership(
	t *testing.T,
) {
	for _, test := range []struct {
		name     string
		board    string
		archived bool
		once     bool
	}{
		{name: "active once", board: "default", once: true},
		{name: "archived watch", board: "archived", archived: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, dbPath := testManager(t)
			if test.board != "default" {
				if _, err := manager.Create(
					context.Background(),
					test.board,
					boards.Update{},
				); err != nil {
					t.Fatal(err)
				}
			}
			claimed := seedPublishingPublications(
				t,
				manager,
				test.board,
				1,
			)[0]
			if test.archived {
				opened, err := manager.OpenStore(
					context.Background(),
					test.board,
				)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := opened.FailPublication(
					context.Background(),
					claimed.ID,
					store.FailPublicationInput{
						ExpectedUpdatedAt: claimed.UpdatedAt,
						ClaimToken:        claimed.ClaimToken,
						ClaimEpoch:        claimed.ClaimEpoch,
						Error:             "prepare archive fixture",
					},
				); err != nil {
					opened.Close()
					t.Fatal(err)
				}
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
				removed, err := manager.Remove(test.board, false)
				if err != nil {
					t.Fatal(err)
				}
				archivedDB, err := sql.Open(
					"sqlite",
					filepath.Join(removed.Path, "autogora.db"),
				)
				if err != nil {
					t.Fatal(err)
				}
				claimed.UpdatedAt = time.Now().UTC().Format(
					time.RFC3339Nano,
				)
				expiresAt := time.Now().UTC().Add(time.Minute).Format(
					time.RFC3339Nano,
				)
				if _, err := archivedDB.ExecContext(
					context.Background(),
					`UPDATE publications
					 SET status = 'publishing', error = NULL,
					     claim_token = 'archived-test-claim',
					     claim_expires_at = ?, updated_at = ?
					 WHERE id = ?`,
					expiresAt,
					claimed.UpdatedAt,
					claimed.ID,
				); err != nil {
					archivedDB.Close()
					t.Fatal(err)
				}
				if err := archivedDB.Close(); err != nil {
					t.Fatal(err)
				}
				claimed.Status = model.PublicationPublishing
			}
			config := agentconfig.Default()
			err := Run(context.Background(), Options{
				DBPath: dbPath, CLIPath: "/tmp/autogora",
				Board: "default", Once: test.once,
				AgentConfig: &config,
			})
			if !errors.Is(err, store.ErrAutomationQuarantined) {
				t.Fatalf("dispatcher startup error = %v", err)
			}
			sources := publicationSources(
				t,
				manager,
				store.AutomationQuarantineSourceFilter{
					Board: test.board, Kind: publicationQuarantineKind,
					SourceID: claimed.ID, ActiveOnly: true,
				},
			)
			if len(sources) != 1 {
				t.Fatalf("publication sources = %#v", sources)
			}
			source := sources[0]
			if source.ObservedUpdatedAt != claimed.UpdatedAt ||
				source.ObservedClaimEpoch != strconv.FormatInt(
					claimed.ClaimEpoch,
					10,
				) ||
				source.DiagnosticCode !=
					publicationOwnershipUnconfirmedDiagnostic {
				t.Fatalf("publication source = %#v", source)
			}
			encoded, encodeErr := json.Marshal(struct {
				Error   string                             `json:"error"`
				Sources []store.AutomationQuarantineSource `json:"sources"`
			}{Error: err.Error(), Sources: sources})
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if strings.Contains(string(encoded), claimed.ClaimToken) {
				t.Fatalf("startup quarantine leaked claim token: %s", encoded)
			}
		})
	}
}

func TestDispatcherStartupRevalidatesStalePublishingObservation(t *testing.T) {
	for _, test := range []struct {
		name             string
		reclaim          bool
		wantQuarantined  bool
		wantPublication  bool
		wantCurrentState model.PublicationStatus
	}{
		{
			name:             "terminal row is skipped",
			wantCurrentState: model.PublicationPublished,
		},
		{
			name:             "new publishing tuple is quarantined",
			reclaim:          true,
			wantQuarantined:  true,
			wantPublication:  true,
			wantCurrentState: model.PublicationPublishing,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := testManager(t)
			observed := seedPublishingPublications(
				t,
				manager,
				"default",
				1,
			)[0]
			current := observed
			coordination, err := manager.OpenCoordinationStore(
				context.Background(),
			)
			if err != nil {
				t.Fatal(err)
			}
			authority := &mutateBeforePublicationActivationAuthority{
				Store: coordination,
			}
			authority.mutate = func(mutationContext context.Context) error {
				opened, err := manager.OpenStore(
					mutationContext,
					"default",
				)
				if err != nil {
					return err
				}
				defer opened.Close()
				if !test.reclaim {
					resultURL := "https://example.test/pulls/stale-startup"
					current, err = opened.CompletePublication(
						mutationContext,
						observed.ID,
						store.CompletePublicationInput{
							ExpectedUpdatedAt: observed.UpdatedAt,
							ClaimToken:        observed.ClaimToken,
							ClaimEpoch:        observed.ClaimEpoch,
							URL:               &resultURL,
						},
					)
					return err
				}
				failed, err := opened.FailPublication(
					mutationContext,
					observed.ID,
					store.FailPublicationInput{
						ExpectedUpdatedAt: observed.UpdatedAt,
						ClaimToken:        observed.ClaimToken,
						ClaimEpoch:        observed.ClaimEpoch,
						Error:             "replace stale startup tuple",
					},
				)
				if err != nil {
					return err
				}
				pending, err := opened.RetryPublication(
					mutationContext,
					failed.ID,
					store.RetryPublicationInput{
						ExpectedUpdatedAt: failed.UpdatedAt,
					},
				)
				if err != nil {
					return err
				}
				var acquired bool
				current, acquired, err = opened.ClaimPublication(
					mutationContext,
					pending.ID,
					store.ClaimPublicationInput{
						ExpectedUpdatedAt: pending.UpdatedAt,
						TTL:               time.Minute,
					},
				)
				if err == nil && !acquired {
					return errors.New(
						"replacement publication claim was not acquired",
					)
				}
				return err
			}
			sessionContext, cancelSession := context.WithCancel(
				context.Background(),
			)
			session, err := startAutomationDispatcherSessionWithAuthority(
				sessionContext,
				authority,
				coordination.Close,
				cancelSession,
				"dispatcher-stale-publication-"+
					strings.ReplaceAll(test.name, " ", "-"),
				time.Minute,
				time.Hour,
				time.Hour,
			)
			if err != nil {
				coordination.Close()
				t.Fatal(err)
			}

			scanErr := quarantineUnconfirmedPublishingOwnership(
				manager,
				session,
			)
			if test.wantQuarantined {
				if !errors.Is(scanErr, store.ErrAutomationQuarantined) {
					t.Fatalf("stale startup scan error=%v", scanErr)
				}
			} else if scanErr != nil {
				t.Fatalf("terminal stale startup scan error=%v", scanErr)
			}
			if authority.mutationErr != nil {
				t.Fatalf("startup mutation error=%v", authority.mutationErr)
			}
			if current.Status != test.wantCurrentState {
				t.Fatalf("current publication=%+v", current)
			}
			sources := publicationSources(
				t,
				manager,
				store.AutomationQuarantineSourceFilter{
					Board:      "default",
					Kind:       publicationQuarantineKind,
					SourceID:   observed.ID,
					ActiveOnly: true,
				},
			)
			if (len(sources) == 1) != test.wantPublication {
				t.Fatalf("stale startup sources=%+v", sources)
			}
			if test.wantPublication &&
				(sources[0].ObservedUpdatedAt != current.UpdatedAt ||
					sources[0].ObservedClaimEpoch != strconv.FormatInt(
						current.ClaimEpoch,
						10,
					)) {
				t.Fatalf("replacement publication source=%+v current=%+v",
					sources[0], current)
			}
			shutdownErr := session.Shutdown(true)
			if test.wantQuarantined {
				if !errors.Is(shutdownErr, store.ErrAutomationQuarantined) {
					t.Fatalf("quarantined shutdown error=%v", shutdownErr)
				}
			} else if shutdownErr != nil {
				t.Fatalf("terminal stale shutdown error=%v", shutdownErr)
			}
		})
	}
}

func TestDispatcherStartupPublishingScanWalksPastOnePage(t *testing.T) {
	manager, _ := testManager(t)
	claimed := seedPublishingPublications(
		t,
		manager,
		"default",
		101,
	)
	sessionContext, cancelSession := context.WithCancel(context.Background())
	session, err := startAutomationDispatcherSession(
		sessionContext,
		manager,
		cancelSession,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = quarantineUnconfirmedPublishingOwnership(manager, session)
	if !errors.Is(err, store.ErrAutomationQuarantined) {
		t.Fatalf("publishing scan error = %v", err)
	}
	if shutdownErr := session.Shutdown(true); !errors.Is(
		shutdownErr,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("session shutdown error = %v", shutdownErr)
	}
	sources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board: "default", Kind: publicationQuarantineKind,
			ActiveOnly: true, Limit: 1000,
		},
	)
	if len(sources) != len(claimed) {
		t.Fatalf("publication sources=%d want=%d", len(sources), len(claimed))
	}
}

func TestDispatcherStartupPublishingScanFailsClosedAtSourceCapacity(
	t *testing.T,
) {
	manager, _ := testManager(t)
	claimed := seedPublishingPublications(t, manager, "default", 2)
	baseAuthority := &fakeAutomationSessionAuthority{registerOK: true}
	authority := &cappedPublicationAutomationAuthority{
		fakeAutomationSessionAuthority: baseAuthority,
		limit:                          1,
	}
	canceled := make(chan struct{})
	var cancelOnce sync.Once
	session, err := startAutomationDispatcherSessionWithAuthority(
		context.Background(),
		authority,
		func() error { return nil },
		func() { cancelOnce.Do(func() { close(canceled) }) },
		"dispatcher-publication-capacity",
		time.Minute,
		time.Hour,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = quarantineUnconfirmedPublishingOwnership(manager, session)
	if !errors.Is(err, store.ErrAutomationQuarantined) ||
		!strings.Contains(err.Error(), "too many active sources") {
		t.Fatalf("source-capacity scan error = %v", err)
	}
	waitAutomationSessionCancellation(t, canceled)
	persisted := authority.persistedSources()
	if len(persisted) != 1 ||
		persisted[0].Kind != publicationQuarantineKind {
		t.Fatalf("capacity persisted sources = %#v", persisted)
	}
	claimedByID := map[string]bool{
		claimed[0].ID: true,
		claimed[1].ID: true,
	}
	if !claimedByID[persisted[0].SourceID] {
		t.Fatalf("capacity source is not exact publication evidence: %#v", persisted[0])
	}
	baseAuthority.mu.Lock()
	gateActive := baseAuthority.gate.Active
	baseAuthority.mu.Unlock()
	if !gateActive {
		t.Fatal("capacity overflow did not leave the global gate active")
	}
	opened, openErr := manager.OpenStore(context.Background(), "default")
	if openErr != nil {
		t.Fatal(openErr)
	}
	publishing, listErr := opened.ListPublications(
		context.Background(),
		store.PublicationFilter{
			Status: model.PublicationPublishing,
			Limit:  10,
		},
	)
	closeErr := opened.Close()
	if listErr != nil || closeErr != nil || len(publishing) != len(claimed) {
		t.Fatalf(
			"capacity publishing rows=%d listErr=%v closeErr=%v",
			len(publishing),
			listErr,
			closeErr,
		)
	}
	if shutdownErr := session.Shutdown(true); shutdownErr == nil {
		t.Fatal("capacity-limited uncertain session unexpectedly shut down cleanly")
	}
}

func TestPublicationTeardownUnconfirmedKeepsPublishingAndExactSource(
	t *testing.T,
) {
	manager, _ := testManager(t)
	pending := seedPublishingPublications(t, manager, "default", 1)[0]
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RetryPublication(
		context.Background(),
		pending.ID,
		store.RetryPublicationInput{ExpectedUpdatedAt: pending.UpdatedAt},
	); !errors.Is(err, store.ErrPublicationStateConflict) {
		opened.Close()
		t.Fatalf("publishing retry unexpectedly succeeded: %v", err)
	}
	// Return the fixture to Pending so executePublication can own its first
	// attempt in this session.
	if _, err := opened.FailPublication(
		context.Background(),
		pending.ID,
		store.FailPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			ClaimToken:        pending.ClaimToken,
			ClaimEpoch:        pending.ClaimEpoch,
			Error:             "reset fixture",
		},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	failed, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	pending, err = opened.RetryPublication(
		context.Background(),
		failed.ID,
		store.RetryPublicationInput{ExpectedUpdatedAt: failed.UpdatedAt},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	sessionContext, cancelSession := context.WithCancel(context.Background())
	session, err := startAutomationDispatcherSession(
		sessionContext,
		manager,
		cancelSession,
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			return publisher.Result{}, processguard.ErrTeardownUnconfirmed
		},
	)
	options.automationSession = session
	acquired, executeErr := executePublicationWithCapability(
		sessionContext,
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if !acquired ||
		!errors.Is(executeErr, processguard.ErrTeardownUnconfirmed) ||
		!errors.Is(executeErr, store.ErrAutomationQuarantined) {
		t.Fatalf("teardown execution: acquired=%t err=%v", acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if current.Status != model.PublicationPublishing ||
		current.ClaimEpoch != pending.ClaimEpoch+1 {
		opened.Close()
		t.Fatalf("teardown publication = %+v", current)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	sources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board: "default", Kind: publicationQuarantineKind,
			SourceID: current.ID, ActiveOnly: true,
		},
	)
	if len(sources) != 1 ||
		sources[0].ObservedUpdatedAt != current.UpdatedAt ||
		sources[0].ObservedClaimEpoch != strconv.FormatInt(
			current.ClaimEpoch,
			10,
		) {
		t.Fatalf("teardown source = %#v", sources)
	}
	if !session.TeardownUnconfirmed() {
		t.Fatal("publication teardown did not latch session uncertainty")
	}
	if shutdownErr := session.Shutdown(true); !errors.Is(
		shutdownErr,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("session shutdown error = %v", shutdownErr)
	}
}

func TestPublicationTeardownStaleTupleFallsBackToSessionQuarantine(
	t *testing.T,
) {
	manager, _ := testManager(t)
	original := seedPublishingPublications(t, manager, "default", 1)[0]
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.FailPublication(
		context.Background(),
		original.ID,
		store.FailPublicationInput{
			ExpectedUpdatedAt: original.UpdatedAt,
			ClaimToken:        original.ClaimToken,
			ClaimEpoch:        original.ClaimEpoch,
			Error:             "reset fixture",
		},
	); err != nil {
		opened.Close()
		t.Fatal(err)
	}
	failed, err := opened.GetPublication(context.Background(), original.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	pending, err := opened.RetryPublication(
		context.Background(),
		failed.ID,
		store.RetryPublicationInput{ExpectedUpdatedAt: failed.UpdatedAt},
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	sessionContext, cancelSession := context.WithCancel(context.Background())
	session, err := startAutomationDispatcherSession(
		sessionContext,
		manager,
		cancelSession,
	)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	var terminalizationErr error
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			_ context.Context,
			claimed model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			_, terminalizationErr = opened.FailPublication(
				context.Background(),
				claimed.ID,
				store.FailPublicationInput{
					ExpectedUpdatedAt: claimed.UpdatedAt,
					ClaimToken:        claimed.ClaimToken,
					ClaimEpoch:        claimed.ClaimEpoch,
					Error:             "terminalized before quarantine activation",
				},
			)
			return publisher.Result{}, processguard.ErrTeardownUnconfirmed
		},
	)
	options.automationSession = session
	acquired, executeErr := executePublicationWithCapability(
		sessionContext,
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if terminalizationErr != nil {
		opened.Close()
		t.Fatalf("terminalize before activation: %v", terminalizationErr)
	}
	if !acquired ||
		!errors.Is(executeErr, processguard.ErrTeardownUnconfirmed) ||
		!errors.Is(executeErr, store.ErrAutomationSourceStale) ||
		!errors.Is(executeErr, store.ErrAutomationQuarantined) {
		opened.Close()
		t.Fatalf("stale teardown execution: acquired=%t err=%v",
			acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	if current.Status != model.PublicationFailed {
		opened.Close()
		t.Fatalf("stale teardown publication=%+v", current)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	exactSources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board:      "default",
			Kind:       publicationQuarantineKind,
			SourceID:   current.ID,
			ActiveOnly: true,
		},
	)
	if len(exactSources) != 0 {
		t.Fatalf("stale publication source persisted=%+v", exactSources)
	}
	sessionSources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board:      globalAutomationSessionBoard,
			Kind:       automationSessionSourceKind,
			ActiveOnly: true,
		},
	)
	if len(sessionSources) != 1 ||
		sessionSources[0].DiagnosticCode != automationTeardownDiagnostic {
		t.Fatalf("fallback session source=%+v", sessionSources)
	}
	if !session.TeardownUnconfirmed() ||
		!session.UncertaintySourcePersisted() {
		t.Fatalf("fallback session state: unconfirmed=%t saved=%t",
			session.TeardownUnconfirmed(),
			session.UncertaintySourcePersisted())
	}
	if shutdownErr := session.Shutdown(true); !errors.Is(
		shutdownErr,
		store.ErrAutomationQuarantined,
	) {
		t.Fatalf("fallback session shutdown error=%v", shutdownErr)
	}
}

func TestDispatcherStartupWithoutPublishingOwnershipProceeds(t *testing.T) {
	_, dbPath := testManager(t)
	config := agentconfig.Default()
	maintenanceCalls := 0
	err := Run(context.Background(), Options{
		DBPath: dbPath, CLIPath: "/tmp/autogora", Once: true,
		AgentConfig: &config,
		testHooks: &dispatcherTestHooks{
			maintainGlobal: func(
				context.Context,
				*boards.Manager,
				Options,
			) error {
				maintenanceCalls++
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if maintenanceCalls == 0 {
		t.Fatal("dispatcher did not proceed beyond an empty ownership scan")
	}
}

func TestPublicationQuarantineSourceCloneCollisionRequiresUniqueRecoveryMatch(
	t *testing.T,
) {
	publication := model.Publication{
		ID: "p-cloned", Board: "archived",
		Status:     model.PublicationPublishing,
		UpdatedAt:  "2026-07-24T00:00:00.000000000Z",
		ClaimEpoch: 3,
	}
	first := publicationQuarantineSource(publication, nil)
	second := publicationQuarantineSource(publication, nil)
	if first.Board != second.Board ||
		first.Kind != second.Kind ||
		first.SourceID != second.SourceID ||
		first.ObservedUpdatedAt != second.ObservedUpdatedAt ||
		first.ObservedClaimEpoch != second.ObservedClaimEpoch ||
		first.DiagnosticCode != second.DiagnosticCode ||
		first.Board != publication.Board ||
		first.SourceID != publication.ID {
		t.Fatalf("cloned publication source identity changed: %#v %#v", first, second)
	}
	// A byte-for-byte archive clone intentionally has the same conservative
	// source identity. Operator recovery must therefore locate this tuple
	// across every active and archived DB and require exactly one match.
}
