package operatorrecovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const recoveryTestTimestamp = "2026-07-24T01:02:03.000000000Z"

type recoveryServiceFixture struct {
	manager   *boards.Manager
	service   *Service
	defaultDB string
}

func newRecoveryServiceFixture(t *testing.T) recoveryServiceFixture {
	t.Helper()
	defaultDB := filepath.Join(t.TempDir(), "autogora.db")
	manager, err := boards.NewManager(defaultDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(
		context.Background(),
		"default",
		boards.Update{},
	); err != nil {
		t.Fatal(err)
	}
	service, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	return recoveryServiceFixture{
		manager: manager, service: service, defaultDB: defaultDB,
	}
}

func (f recoveryServiceFixture) addPublishingBoard(
	t *testing.T,
	board string,
	publicationID string,
	claimEpoch int64,
	archived bool,
) (string, store.AutomationQuarantineSourceInput) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.manager.Create(ctx, board, boards.Update{}); err != nil {
		t.Fatal(err)
	}
	dbPath, err := f.manager.DBPath(board)
	if err != nil {
		t.Fatal(err)
	}
	insertPendingPublicationFixture(
		t,
		dbPath,
		board,
		publicationID,
		claimEpoch-1,
		recoveryTestTimestamp,
	)
	var publicationStore *store.Store
	if archived {
		removed, err := f.manager.Remove(board, false)
		if err != nil {
			t.Fatal(err)
		}
		dbPath = filepath.Join(removed.Path, "autogora.db")
		publicationStore, err = store.Open(
			dbPath,
			board,
			filepath.Join(removed.Path, "attachments"),
		)
		if err != nil {
			t.Fatal(err)
		}
		// A normally archived database retains its local removal guard. Clear
		// it only in this fixture before directly claiming the archived
		// publication to model an older or non-cooperating writer that outlived
		// the manager-owned archive operation.
		if err := publicationStore.ClearLocalBoardRemovalGuard(
			ctx,
			board,
		); err != nil {
			publicationStore.Close()
			t.Fatal(err)
		}
	} else {
		publicationStore, err = f.manager.OpenStore(ctx, board)
		if err != nil {
			t.Fatal(err)
		}
	}
	pending, err := publicationStore.GetPublication(ctx, publicationID)
	if err != nil {
		publicationStore.Close()
		t.Fatal(err)
	}
	claimed, acquired, claimErr := publicationStore.ClaimPublication(
		ctx,
		publicationID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: pending.UpdatedAt,
			TTL:               store.MaxPublicationClaimTTL,
		},
	)
	closeErr := publicationStore.Close()
	if claimErr != nil || closeErr != nil || !acquired {
		t.Fatal(errors.Join(claimErr, closeErr))
	}
	if claimed.ClaimEpoch != claimEpoch {
		t.Fatalf(
			"publication claim epoch = %d, want %d",
			claimed.ClaimEpoch,
			claimEpoch,
		)
	}
	return dbPath, store.AutomationQuarantineSourceInput{
		Board:              board,
		Kind:               PublicationSourceKind,
		SourceID:           publicationID,
		ObservedUpdatedAt:  claimed.UpdatedAt,
		ObservedClaimEpoch: fmt.Sprintf("%d", claimed.ClaimEpoch),
		DiagnosticCode:     "publishing_ownership_unconfirmed",
	}
}

func insertPendingPublicationFixture(
	t *testing.T,
	dbPath string,
	board string,
	publicationID string,
	previousClaimEpoch int64,
	updatedAt string,
) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	taskID := "task_" + publicationID
	runID := "run_" + publicationID
	changeSetID := "change_" + publicationID
	if _, err := db.Exec(`
		INSERT INTO tasks(
			id, board, title, status, created_at, updated_at, workflow_role
		) VALUES (?, ?, ?, 'done', ?, ?, 'finalizer')
	`, taskID, board, "Recovery "+publicationID, updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_runs(
			id, task_id, worker_id, runtime, status, claim_token,
			claimed_at, claim_expires_at, heartbeat_at, ended_at,
			metadata_json
		) VALUES (?, ?, 'recovery-test', 'manual', 'completed',
			'retired-run-token', ?, ?, ?, ?, '{}')
	`, runID, taskID, updatedAt, updatedAt, updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_change_sets(
			id, run_id, task_id, repository_path, worktree_path,
			base_commit, head_commit, durable_ref, state,
			changed_files_json, created_at
		) VALUES (?, ?, ?, '/secret/repository', '/secret/worktree',
			'base', 'head', ?, 'ready', '[]', ?)
	`, changeSetID, runID, taskID,
		"refs/autogora/runs/"+runID, updatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO publications(
			id, board, task_id, run_id, change_set_id, status, mode,
			target_branch, remote, require_approval, repository_path,
			worktree_path, base_commit, head_commit, durable_ref,
			policy_snapshot_json, source_snapshot_json, claim_epoch,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'pending', 'pull_request',
			'main', 'origin', 0, '/secret/repository', '/secret/worktree',
			'base', 'head', ?, '{}', '{}', ?, ?, ?)
	`, publicationID, board, taskID, runID, changeSetID,
		"refs/autogora/runs/"+runID, previousClaimEpoch,
		updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryServiceFixture) activateSources(
	t *testing.T,
	inputs ...store.AutomationQuarantineSourceInput,
) {
	t.Helper()
	authority, err := f.manager.OpenCoordinationStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		if input.Kind == PublicationSourceKind {
			input.ValidateCurrent = f.validatePublishingSource
		}
		if _, _, err := authority.ActivateAutomationQuarantine(
			context.Background(),
			input,
		); err != nil {
			authority.Close()
			t.Fatal(err)
		}
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryServiceFixture) validatePublishingSource(
	ctx context.Context,
	input store.AutomationQuarantineSourceInput,
) (bool, error) {
	inventory, err := f.manager.ListMetadata(ctx, true)
	if err != nil {
		return false, err
	}
	matches := 0
	for _, metadata := range inventory {
		if metadata.Slug != input.Board {
			continue
		}
		reader, err := f.manager.OpenListedPublicationRecoveryReader(
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

func confirmationForStatus(
	t *testing.T,
	status Status,
	actor string,
	reason string,
	outcomes map[string]PublicationOutcome,
	nonPublication store.AutomationSourceDisposition,
) Confirmation {
	t.Helper()
	value := Confirmation{
		Generation:            status.Gate.Generation,
		Actor:                 actor,
		Reason:                reason,
		HelpersStopped:        true,
		ExternalWritesStopped: true,
		Sources:               make([]ConfirmationSource, 0, len(status.Sources)),
	}
	for _, source := range status.Sources {
		resolution := ConfirmationSource{
			SourceKey:          source.SourceKey,
			Board:              source.Board,
			Kind:               source.Kind,
			SourceID:           source.SourceID,
			ObservedUpdatedAt:  source.ObservedUpdatedAt,
			ObservedClaimEpoch: source.ObservedClaimEpoch,
			DiagnosticCode:     source.DiagnosticCode,
		}
		if source.Kind == PublicationSourceKind {
			outcome, ok := outcomes[source.SourceID]
			if !ok {
				t.Fatalf("missing outcome for %s", source.SourceID)
			}
			resolution.Outcome = outcome
			resolution.Disposition, _ = publicationDisposition(outcome)
			if outcome == PublicationOutcomePublished {
				url := "https://example.test/publications/" + source.SourceID
				resolution.ResultURL = &url
			}
		} else {
			resolution.Disposition = nonPublication
		}
		value.Sources = append(value.Sources, resolution)
	}
	return value
}

func readPublicationRecoveryState(
	t *testing.T,
	dbPath string,
	publicationID string,
) (
	model.PublicationStatus,
	string,
	int64,
	*string,
	*string,
	string,
) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status model.PublicationStatus
	var updatedAt string
	var epoch int64
	var url, publicationError sql.NullString
	var claimToken sql.NullString
	if err := db.QueryRow(`
		SELECT status, updated_at, claim_epoch, url, error, claim_token
		FROM publications WHERE id = ?
	`, publicationID).Scan(
		&status,
		&updatedAt,
		&epoch,
		&url,
		&publicationError,
		&claimToken,
	); err != nil {
		t.Fatal(err)
	}
	var urlPointer, errorPointer *string
	if url.Valid {
		value := url.String
		urlPointer = &value
	}
	if publicationError.Valid {
		value := publicationError.String
		errorPointer = &value
	}
	return status, updatedAt, epoch, urlPointer, errorPointer, claimToken.String
}

func TestServiceConfirmsActiveArchivedAndMixedSources(t *testing.T) {
	fixture := newRecoveryServiceFixture(t)
	activeDB, activeSource := fixture.addPublishingBoard(
		t, "active", "pub_active", 3, false,
	)
	archivedDB, archivedSource := fixture.addPublishingBoard(
		t, "archived", "pub_archived", 5, true,
	)
	mixedSource := store.AutomationQuarantineSourceInput{
		Board:             "*",
		Kind:              "dispatcher_session",
		SourceID:          "session-shutdown",
		ObservedUpdatedAt: recoveryTestTimestamp,
		DiagnosticCode:    "process_teardown_unconfirmed",
	}
	fixture.activateSources(
		t,
		activeSource,
		archivedSource,
		mixedSource,
	)

	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Gate.Active || len(status.Sources) != 3 ||
		status.UnacknowledgedSessionCount != 0 {
		t.Fatalf("status = %+v", status)
	}
	serializedStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"secret-publication-token",
		"/secret/repository",
		"/secret/worktree",
		filepath.Dir(fixture.defaultDB),
	} {
		if strings.Contains(string(serializedStatus), forbidden) {
			t.Fatalf(
				"status leaked %q: %s",
				forbidden,
				serializedStatus,
			)
		}
	}
	archivedSummaryFound := false
	for _, source := range status.Sources {
		if source.Kind != PublicationSourceKind {
			continue
		}
		if source.Publication == nil ||
			source.Publication.MatchCount != 1 ||
			source.Publication.Archived == nil ||
			source.Publication.HasReceipt {
			t.Fatalf("publication source status = %+v", source)
		}
		if source.SourceID == "pub_archived" {
			archivedSummaryFound = *source.Publication.Archived
		}
	}
	if !archivedSummaryFound {
		t.Fatal("archived publication storage was not summarized")
	}

	const reason = "operator verified all helpers and external writes stopped"
	confirmation := confirmationForStatus(
		t,
		status,
		"operator@example.test",
		reason,
		map[string]PublicationOutcome{
			"pub_active":   PublicationOutcomePublished,
			"pub_archived": PublicationOutcomeFailed,
		},
		store.AutomationSourceAbandoned,
	)
	result, err := fixture.service.Confirm(
		context.Background(),
		confirmation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || result.Gate.Active ||
		len(result.Publications) != 2 {
		t.Fatalf("confirmation result = %+v", result)
	}
	for _, publication := range result.Publications {
		serialized, err := json.Marshal(publication)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(serialized), "secret") ||
			strings.Contains(string(serialized), "worktree") ||
			strings.Contains(string(serialized), "repository") {
			t.Fatalf("publication result leaked secret/path data: %s", serialized)
		}
	}

	activeStatus, _, activeEpoch, activeURL, _, activeToken :=
		readPublicationRecoveryState(t, activeDB, "pub_active")
	if activeStatus != model.PublicationPublished ||
		activeEpoch != 3 || activeURL == nil ||
		activeToken != "" {
		t.Fatalf(
			"active publication = status %s epoch %d url %v token %q",
			activeStatus,
			activeEpoch,
			activeURL,
			activeToken,
		)
	}
	archivedStatus, _, archivedEpoch, _, archivedError, archivedToken :=
		readPublicationRecoveryState(t, archivedDB, "pub_archived")
	if archivedStatus != model.PublicationFailed ||
		archivedEpoch != 5 || archivedError == nil ||
		*archivedError != reason || archivedToken != "" {
		t.Fatalf(
			"archived publication = status %s epoch %d error %v token %q",
			archivedStatus,
			archivedEpoch,
			archivedError,
			archivedToken,
		)
	}

	replayed, err := fixture.service.Confirm(
		context.Background(),
		confirmation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Cleared || replayed.Gate.Active {
		t.Fatalf("idempotent replay result = %+v", replayed)
	}
	if len(replayed.Publications) != 2 {
		t.Fatalf("idempotent replay omitted publication results: %+v", replayed)
	}
	for _, publication := range replayed.Publications {
		if publication.Changed {
			t.Fatalf("idempotent replay changed publication: %+v", publication)
		}
	}
	tampered := confirmation
	tampered.Sources = append(
		[]ConfirmationSource(nil),
		confirmation.Sources...,
	)
	for index := range tampered.Sources {
		if tampered.Sources[index].SourceID == "pub_active" {
			tampered.Sources[index].Outcome = PublicationOutcomeSuperseded
			tampered.Sources[index].ResultURL = nil
		}
	}
	if _, err := fixture.service.Confirm(
		context.Background(),
		tampered,
	); !errors.Is(err, store.ErrAutomationGateConflict) {
		t.Fatalf("tampered inactive outcome replay error = %v", err)
	}
}

func TestServiceRejectsMissingAndDuplicatePublicationStorage(
	t *testing.T,
) {
	t.Run("missing", func(t *testing.T) {
		fixture := newRecoveryServiceFixture(t)
		dbPath, source := fixture.addPublishingBoard(
			t,
			"missing",
			"pub_missing",
			1,
			false,
		)
		fixture.activateSources(t, source)
		// The source was exact when quarantine activated. Move its board
		// directory out of inventory afterward to model storage loss while
		// preserving a valid durable authority observation.
		boardDirectory := filepath.Dir(dbPath)
		if err := os.Rename(
			boardDirectory,
			boardDirectory+".missing",
		); err != nil {
			t.Fatal(err)
		}
		status, err := fixture.service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		confirmation := confirmationForStatus(
			t,
			status,
			"operator",
			"verified missing storage",
			map[string]PublicationOutcome{
				"pub_missing": PublicationOutcomeFailed,
			},
			store.AutomationSourceAbandoned,
		)
		if _, err := fixture.service.Confirm(
			context.Background(),
			confirmation,
		); !errors.Is(err, ErrPublicationStorageConflict) {
			t.Fatalf("missing storage error = %v", err)
		}
	})

	t.Run("duplicate archived clone", func(t *testing.T) {
		fixture := newRecoveryServiceFixture(t)
		_, source := fixture.addPublishingBoard(
			t, "clone", "pub_clone", 2, true,
		)
		listed, err := fixture.manager.ListMetadata(
			context.Background(),
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		var originalDirectory string
		for _, metadata := range listed {
			if metadata.Archived && metadata.Slug == "clone" {
				originalDirectory = filepath.Dir(metadata.DBPath)
				break
			}
		}
		if originalDirectory == "" {
			t.Fatal("archived clone board not found")
		}
		fixture.activateSources(t, source)
		copyDirectory(
			t,
			originalDirectory,
			filepath.Join(
				filepath.Dir(originalDirectory),
				"clone-duplicate",
			),
		)
		status, err := fixture.service.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(status.Sources) != 1 ||
			status.Sources[0].Publication == nil ||
			status.Sources[0].Publication.MatchCount != 2 {
			t.Fatalf("duplicate status = %+v", status)
		}
		confirmation := confirmationForStatus(
			t,
			status,
			"operator",
			"verified duplicate clone",
			map[string]PublicationOutcome{
				"pub_clone": PublicationOutcomeSuperseded,
			},
			store.AutomationSourceAbandoned,
		)
		if _, err := fixture.service.Confirm(
			context.Background(),
			confirmation,
		); !errors.Is(err, ErrPublicationStorageConflict) {
			t.Fatalf("duplicate storage error = %v", err)
		}
	})
}

func TestServiceRejectsWrongGenerationAndSourceSet(t *testing.T) {
	fixture := newRecoveryServiceFixture(t)
	_, source := fixture.addPublishingBoard(
		t, "wrong", "pub_wrong", 1, false,
	)
	fixture.activateSources(t, source)
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	confirmation := confirmationForStatus(
		t,
		status,
		"operator",
		"verified exact source set",
		map[string]PublicationOutcome{
			"pub_wrong": PublicationOutcomeFailed,
		},
		store.AutomationSourceAbandoned,
	)
	wrongGeneration := confirmation
	wrongGeneration.Generation++
	if _, err := fixture.service.Confirm(
		context.Background(),
		wrongGeneration,
	); !errors.Is(err, store.ErrAutomationGateConflict) {
		t.Fatalf("wrong generation error = %v", err)
	}
	missingSource := confirmation
	missingSource.Sources = nil
	if _, err := fixture.service.Confirm(
		context.Background(),
		missingSource,
	); !errors.Is(err, store.ErrAutomationSourceConflict) {
		t.Fatalf("wrong source set error = %v", err)
	}
	wrongIdentity := confirmation
	wrongIdentity.Sources = append(
		[]ConfirmationSource(nil),
		confirmation.Sources...,
	)
	wrongIdentity.Sources[0].Board = "other"
	if _, err := fixture.service.Confirm(
		context.Background(),
		wrongIdentity,
	); !errors.Is(err, store.ErrAutomationSourceConflict) {
		t.Fatalf("wrong source identity error = %v", err)
	}
}

func TestServiceSkipsUnrelatedCorruptBoardInventory(t *testing.T) {
	fixture := newRecoveryServiceFixture(t)
	_, source := fixture.addPublishingBoard(
		t, "target", "pub_target", 1, false,
	)
	if _, err := fixture.manager.Create(
		context.Background(),
		"unrelated",
		boards.Update{},
	); err != nil {
		t.Fatal(err)
	}
	unrelatedDB, err := fixture.manager.DBPath("unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelatedDB, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.activateSources(t, source)
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatalf("unrelated corrupt board blocked status: %v", err)
	}
	if len(status.Sources) != 1 ||
		status.Sources[0].Publication == nil ||
		status.Sources[0].Publication.MatchCount != 1 {
		t.Fatalf("targeted status = %+v", status)
	}
}

func TestServiceResumesPhaseOneConfirmationFromReceipt(t *testing.T) {
	fixture := newRecoveryServiceFixture(t)
	archivedDB, sourceInput := fixture.addPublishingBoard(
		t, "resume", "pub_resume", 4, true,
	)
	fixture.activateSources(t, sourceInput)
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const actor = "resume-operator"
	const reason = "verified phase-one recovery"
	confirmation := confirmationForStatus(
		t,
		status,
		actor,
		reason,
		map[string]PublicationOutcome{
			"pub_resume": PublicationOutcomeSuperseded,
		},
		store.AutomationSourceAbandoned,
	)
	source := confirmation.Sources[0]
	simulatePublicationRecoveryReceipt(
		t,
		archivedDB,
		status.Sources[0].FirstGeneration,
		source,
		actor,
		reason,
	)
	simulateConfirmationPhaseOne(
		t,
		fixture.defaultDB,
		confirmation,
	)

	pending, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending.Pending == nil ||
		pending.Pending.ResolvedGeneration != confirmation.Generation ||
		pending.Pending.Actor != actor ||
		pending.Pending.Reason != reason ||
		len(pending.Sources) != 1 ||
		pending.Sources[0].Outcome != PublicationOutcomeSuperseded ||
		pending.Sources[0].Publication == nil ||
		!pending.Sources[0].Publication.HasReceipt {
		t.Fatalf("pending recovery status = %+v", pending)
	}
	resumed := confirmationForStatus(
		t,
		pending,
		pending.Pending.Actor,
		pending.Pending.Reason,
		map[string]PublicationOutcome{
			"pub_resume": pending.Sources[0].Outcome,
		},
		store.AutomationSourceAbandoned,
	)
	result, err := fixture.service.Confirm(
		context.Background(),
		resumed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || result.Gate.Active ||
		len(result.Publications) != 1 ||
		result.Publications[0].Changed {
		t.Fatalf("resumed recovery = %+v", result)
	}
}

func TestServiceResumesPreparedReceiptBeforePhaseOne(t *testing.T) {
	fixture := newRecoveryServiceFixture(t)
	activeDB, sourceInput := fixture.addPublishingBoard(
		t, "prepared", "pub_prepared", 6, false,
	)
	fixture.activateSources(t, sourceInput)
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const actor = "prepared-operator"
	const reason = "verified prepared receipt recovery"
	confirmation := confirmationForStatus(
		t,
		status,
		actor,
		reason,
		map[string]PublicationOutcome{
			"pub_prepared": PublicationOutcomePublished,
		},
		store.AutomationSourceAbandoned,
	)
	source := confirmation.Sources[0]
	simulatePublicationRecoveryReceipt(
		t,
		activeDB,
		status.Sources[0].FirstGeneration,
		source,
		actor,
		reason,
	)
	deletePublicationTask(t, activeDB, "task_pub_prepared")

	prepared, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Pending != nil ||
		prepared.Prepared == nil ||
		prepared.Prepared.Actor != actor ||
		prepared.Prepared.Reason != reason ||
		prepared.Prepared.RecoveredPublicationSources != 1 ||
		len(prepared.Sources) != 1 ||
		prepared.Sources[0].Outcome != PublicationOutcomePublished ||
		prepared.Sources[0].ReceiptDisposition == nil ||
		*prepared.Sources[0].ReceiptDisposition !=
			store.AutomationSourceSuperseded {
		t.Fatalf("prepared recovery status = %+v", prepared)
	}
	resumed := confirmationForStatus(
		t,
		prepared,
		prepared.Prepared.Actor,
		prepared.Prepared.Reason,
		map[string]PublicationOutcome{
			"pub_prepared": prepared.Sources[0].Outcome,
		},
		store.AutomationSourceAbandoned,
	)
	result, err := fixture.service.Confirm(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared || result.Gate.Active ||
		len(result.Publications) != 1 ||
		result.Publications[0].Changed ||
		result.Publications[0].Present {
		t.Fatalf("prepared recovery replay = %+v", result)
	}
}

func deletePublicationTask(t *testing.T, dbPath, taskID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM tasks WHERE id = ?", taskID); err != nil {
		t.Fatal(err)
	}
}

func simulatePublicationRecoveryReceipt(
	t *testing.T,
	dbPath string,
	firstGeneration int64,
	source ConfirmationSource,
	actor string,
	reason string,
) {
	t.Helper()
	epoch, err := strconv.ParseInt(source.ObservedClaimEpoch, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resultUpdatedAt := "2026-07-24T01:02:04.000000000Z"
	var status model.PublicationStatus
	var resultURL, publicationError, publishedAt any
	switch source.Outcome {
	case PublicationOutcomePublished:
		status = model.PublicationPublished
		if source.ResultURL != nil {
			resultURL = *source.ResultURL
		}
		publishedAt = resultUpdatedAt
	case PublicationOutcomeFailed:
		status = model.PublicationFailed
		publicationError = reason
	case PublicationOutcomeSuperseded:
		status = model.PublicationSuperseded
		publicationError = reason
	default:
		t.Fatalf("unsupported simulated outcome %s", source.Outcome)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
		UPDATE publications
		SET status = ?, url = ?, error = ?, claim_token = NULL,
			claim_expires_at = NULL, published_at = ?, updated_at = ?
		WHERE id = ? AND board = ? AND status = 'publishing'
			AND updated_at = ? AND claim_epoch = ?
	`, status, resultURL, publicationError, publishedAt, resultUpdatedAt,
		source.SourceID, source.Board, source.ObservedUpdatedAt, epoch); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
		INSERT INTO publication_recovery_receipts(
			source_key, first_generation, publication_id,
			observed_updated_at, observed_claim_epoch, outcome,
			disposition, result_url, actor, reason, recovered_at,
			result_updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, source.SourceKey, firstGeneration, source.SourceID,
		source.ObservedUpdatedAt, epoch, source.Outcome,
		source.Disposition, resultURL, actor, reason,
		resultUpdatedAt, resultUpdatedAt); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func simulateConfirmationPhaseOne(
	t *testing.T,
	dbPath string,
	confirmation Confirmation,
) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	confirmationDigest := testConfirmationEvidenceDigest(t, confirmation)
	for _, source := range confirmation.Sources {
		if _, err := tx.Exec(`
			UPDATE automation_quarantine_sources
			SET disposition = ?, resolved_at = ?, resolved_by = ?,
				resolution_reason = ?, resolved_generation = ?
			WHERE source_key = ? AND disposition = 'active'
		`, source.Disposition, recoveryTestTimestamp,
			confirmation.Actor, confirmation.Reason,
			confirmation.Generation, source.SourceKey); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO automation_quarantine_confirmation_evidence(
			generation, confirmation_digest, recorded_at
		) VALUES (?, ?, ?)
	`, confirmation.Generation, confirmationDigest, recoveryTestTimestamp); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
		UPDATE automation_quarantine_gate
		SET confirmation_started_at = ?, confirmation_actor = ?,
			confirmation_reason = ?, confirmation_helpers_stopped = 1,
			confirmation_external_writes_stopped = 1
		WHERE singleton = 1 AND active = 1 AND generation = ?
	`, recoveryTestTimestamp, confirmation.Actor, confirmation.Reason,
		confirmation.Generation); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func testConfirmationEvidenceDigest(
	t *testing.T,
	confirmation Confirmation,
) string {
	t.Helper()
	_, _, normalized, err := validateConfirmation(confirmation)
	if err != nil {
		t.Fatal(err)
	}
	evidence := struct {
		Version               int                                          `json:"version"`
		Generation            int64                                        `json:"generation"`
		Actor                 string                                       `json:"actor"`
		Reason                string                                       `json:"reason"`
		HelpersStopped        bool                                         `json:"helpersStopped"`
		ExternalWritesStopped bool                                         `json:"externalWritesStopped"`
		Sources               []store.AutomationQuarantineSourceResolution `json:"sources"`
	}{
		Version:               1,
		Generation:            normalized.Generation,
		Actor:                 normalized.Actor,
		Reason:                normalized.Reason,
		HelpersStopped:        normalized.HelpersStopped,
		ExternalWritesStopped: normalized.ExternalWritesStopped,
		Sources:               normalized.Sources,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	digestInput := append(
		[]byte("autogora/automation-confirmation/v1\x00"),
		encoded...,
	)
	return fmt.Sprintf("%x", sha256.Sum256(digestInput))
}

func copyDirectory(t *testing.T, source, target string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			copyDirectory(t, sourcePath, targetPath)
			continue
		}
		contents, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			targetPath,
			contents,
			info.Mode().Perm(),
		); err != nil {
			t.Fatal(err)
		}
	}
}
