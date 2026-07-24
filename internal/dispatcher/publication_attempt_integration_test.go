package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/store"
)

func publicationAttemptExecutionFixture(
	t *testing.T,
	mode boards.PublicationMode,
) (*boards.Manager, *store.Store, model.Task, model.Publication) {
	t.Helper()
	manager, _ := testManager(t)
	configurePublicationBoard(t, manager, "default", mode, false, true)
	task, _ := createCompletedFinalizerChangeSet(
		t,
		manager,
		"default",
		"attempt-integration",
		"ready",
	)
	opened, err := manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := manager.Read("default")
	if err != nil {
		opened.Close()
		t.Fatal(err)
	}
	values, err := ensureBoardPublications(
		context.Background(),
		opened,
		"default",
		metadata.Orchestration.Autopilot.Publication,
	)
	if err != nil || len(values) != 1 {
		opened.Close()
		t.Fatalf("publication fixture=%+v err=%v", values, err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return manager, opened, task, values[0]
}

func publicationAttemptRecordForTask(
	t *testing.T,
	opened *store.Store,
	taskID string,
) store.PublicationAttemptRecord {
	t.Helper()
	events, err := opened.ListEvents(
		context.Background(),
		store.EventFilter{
			TaskID: taskID,
			Kinds:  []string{"publication_attempt_started"},
		},
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("attempt events=%+v err=%v", events, err)
	}
	var payload struct {
		AttemptID string `json:"attemptId"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	record, err := opened.GetPublicationAttempt(
		context.Background(),
		payload.AttemptID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPublicationAttemptClosesBasePermitAndGatesEverySuccessfulCommand(
	t *testing.T,
) {
	manager, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	releaseCalls := 0
	var session *automationDispatcherSession
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			ctx context.Context,
			value model.Publication,
			publicationOptions publisher.Options,
		) (publisher.Result, error) {
			probeContext, cancel := context.WithTimeout(ctx, time.Second)
			_, _, probeErr := session.authority.ActivateAutomationQuarantine(
				probeContext,
				store.AutomationQuarantineSourceInput{
					Board:             globalAutomationSessionBoard,
					Kind:              "publication_base_permit_probe",
					SourceID:          value.ID,
					ObservedUpdatedAt: value.UpdatedAt,
					DiagnosticCode:    "base_permit_must_be_closed",
					ValidateCurrent: func(
						context.Context,
						store.AutomationQuarantineSourceInput,
					) (bool, error) {
						return false, nil
					},
				},
			)
			cancel()
			if !errors.Is(probeErr, store.ErrAutomationSourceStale) {
				return publisher.Result{}, fmt.Errorf(
					"exclusive base-permit probe: %w",
					probeErr,
				)
			}
			for range 2 {
				released, err := publicationOptions.ReleaseGate(
					ctx,
					func() (bool, error) {
						releaseCalls++
						return true, nil
					},
				)
				if err != nil || !released {
					return publisher.Result{}, fmt.Errorf(
						"release test publication command: released=%t err=%w",
						released,
						err,
					)
				}
			}
			return publishedResult(value), nil
		},
	)
	session = attachPublicationTestAutomation(t, manager, &options)
	acquired, err := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if err != nil || !acquired || releaseCalls != 2 {
		t.Fatalf(
			"execution acquired=%t releases=%d err=%v",
			acquired,
			releaseCalls,
			err,
		)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil || current.Status != model.PublicationPublished {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	record := publicationAttemptRecordForTask(t, opened, task.ID)
	if record.Result == nil ||
		record.Result.Outcome != store.PublicationAttemptPublished ||
		record.Result.ExecutorStatus != store.PublicationExecutorPublished ||
		record.Result.PublicationUpdatedAt != current.UpdatedAt {
		t.Fatalf("attempt=%+v publication=%+v", record, current)
	}
	exact := store.PublicationAttemptResultInput{
		Outcome:        store.PublicationAttemptPublished,
		ExecutorStatus: store.PublicationExecutorPublished,
	}
	matched, err := reconcilePublicationAttemptResult(
		context.Background(),
		opened,
		record.Intent.ID,
		exact,
	)
	if err != nil || !matched {
		t.Fatalf("reconcile exact receipt: matched=%t err=%v", matched, err)
	}
	different := exact
	different.ExecutorStatus = store.PublicationExecutorAlreadyPublished
	matched, err = reconcilePublicationAttemptResult(
		context.Background(),
		opened,
		record.Intent.ID,
		different,
	)
	if err != nil || matched {
		t.Fatalf("reconcile different receipt: matched=%t err=%v", matched, err)
	}
}

func TestPublicationAttemptFirstCommandStartBlockIsKnownFailure(t *testing.T) {
	manager, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			ctx context.Context,
			_ model.Publication,
			publicationOptions publisher.Options,
		) (publisher.Result, error) {
			released, err := publicationOptions.ReleaseGate(
				ctx,
				func() (bool, error) {
					return false, errors.New("target stayed fenced")
				},
			)
			return publisher.Result{}, &publisher.CommandStartError{
				Released: released,
				Err:      err,
			}
		},
	)
	session := attachPublicationTestAutomation(t, manager, &options)
	acquired, executeErr := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if !acquired || !errors.Is(executeErr, publisher.ErrCommandStartBlocked) {
		t.Fatalf("acquired=%t err=%v", acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil || current.Status != model.PublicationFailed {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	record := publicationAttemptRecordForTask(t, opened, task.ID)
	if record.Result == nil ||
		record.Result.Outcome != store.PublicationAttemptFailed ||
		record.Result.ErrorKind == nil ||
		*record.Result.ErrorKind != store.PublicationErrorCommandStartBlocked ||
		session.TeardownUnconfirmed() {
		t.Fatalf("attempt=%+v session=%+v", record, session.Err())
	}
}

func TestPublicationAttemptLaterStartBlockPreservesPriorReleaseAsUnknown(
	t *testing.T,
) {
	manager, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	priorEffect := false
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			ctx context.Context,
			_ model.Publication,
			publicationOptions publisher.Options,
		) (publisher.Result, error) {
			released, err := publicationOptions.ReleaseGate(
				ctx,
				func() (bool, error) {
					priorEffect = true
					return true, nil
				},
			)
			if err != nil || !released {
				return publisher.Result{}, fmt.Errorf(
					"first command release: released=%t err=%w",
					released,
					err,
				)
			}
			released, err = publicationOptions.ReleaseGate(
				ctx,
				func() (bool, error) {
					return false, errors.New("later target stayed fenced")
				},
			)
			return publisher.Result{}, &publisher.CommandStartError{
				Released: released,
				Err:      err,
			}
		},
	)
	session := attachPublicationTestAutomation(t, manager, &options)
	acquired, executeErr := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if !acquired ||
		!priorEffect ||
		!errors.Is(executeErr, publisher.ErrCommandStartBlocked) ||
		!errors.Is(executeErr, store.ErrAutomationQuarantined) {
		t.Fatalf("acquired=%t err=%v", acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil || current.Status != model.PublicationPublishing {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	record := publicationAttemptRecordForTask(t, opened, task.ID)
	if record.Result == nil ||
		record.Result.Outcome != store.PublicationAttemptUnknown ||
		record.Result.ErrorKind == nil ||
		*record.Result.ErrorKind != store.PublicationErrorCommandStartBlocked ||
		!session.TeardownUnconfirmed() {
		t.Fatalf("attempt=%+v sessionErr=%v", record, session.Err())
	}
	sources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board:      "default",
			Kind:       publicationQuarantineKind,
			SourceID:   pending.ID,
			ActiveOnly: true,
		},
	)
	if len(sources) != 1 {
		t.Fatalf("publication quarantine sources=%+v", sources)
	}
}

func TestPublicationAttemptKnownPreCommandFailureHasReceipt(t *testing.T) {
	manager, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			return publisher.Result{}, &publisher.Error{
				Kind: publisher.ErrorInvalidInput,
				Err:  errors.New("invalid captured publication input"),
			}
		},
	)
	attachPublicationTestAutomation(t, manager, &options)
	acquired, executeErr := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if !acquired || executeErr == nil {
		t.Fatalf("acquired=%t err=%v", acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil || current.Status != model.PublicationFailed {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	record := publicationAttemptRecordForTask(t, opened, task.ID)
	if record.Result == nil ||
		record.Result.Outcome != store.PublicationAttemptFailed ||
		record.Result.ErrorKind == nil ||
		*record.Result.ErrorKind != store.PublicationErrorInvalidInput {
		t.Fatalf("attempt=%+v", record)
	}
}

func TestPublicationAttemptRequiresAutomationSessionBeforeClaim(t *testing.T) {
	_, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	executorCalls := 0
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			context.Context,
			model.Publication,
			publisher.Options,
		) (publisher.Result, error) {
			executorCalls++
			return publisher.Result{}, nil
		},
	)
	acquired, executeErr := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if acquired || executeErr == nil || executorCalls != 0 {
		t.Fatalf(
			"acquired=%t executorCalls=%d err=%v",
			acquired,
			executorCalls,
			executeErr,
		)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil ||
		current.Status != model.PublicationPending ||
		current.ClaimEpoch != pending.ClaimEpoch {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	events, err := opened.ListEvents(
		context.Background(),
		store.EventFilter{
			TaskID: task.ID,
			Kinds:  []string{"publication_attempt_started"},
		},
	)
	if err != nil || len(events) != 0 {
		t.Fatalf("attempt events=%+v err=%v", events, err)
	}
}

func TestPublicationAttemptSuccessWithoutCommandGateIsUnknown(t *testing.T) {
	manager, opened, task, pending := publicationAttemptExecutionFixture(
		t,
		boards.PublicationModeLocalFF,
	)
	options := publicationTestOptions(
		time.Now().UTC(),
		func(
			_ context.Context,
			value model.Publication,
			_ publisher.Options,
		) (publisher.Result, error) {
			return publishedResult(value), nil
		},
	)
	session := attachPublicationTestAutomation(t, manager, &options)
	acquired, executeErr := executePublicationWithCapability(
		context.Background(),
		opened,
		pending,
		options,
		automaticMutationCapability{Available: true},
	)
	if !acquired || executeErr == nil ||
		!errors.Is(executeErr, store.ErrAutomationQuarantined) {
		t.Fatalf("acquired=%t err=%v", acquired, executeErr)
	}
	current, err := opened.GetPublication(context.Background(), pending.ID)
	if err != nil || current.Status != model.PublicationPublishing {
		t.Fatalf("publication=%+v err=%v", current, err)
	}
	record := publicationAttemptRecordForTask(t, opened, task.ID)
	if record.Result == nil ||
		record.Result.Outcome != store.PublicationAttemptUnknown ||
		record.Result.ErrorKind == nil ||
		*record.Result.ErrorKind != store.PublicationErrorInternal ||
		!session.TeardownUnconfirmed() {
		t.Fatalf("attempt=%+v sessionErr=%v", record, session.Err())
	}
	sources := publicationSources(
		t,
		manager,
		store.AutomationQuarantineSourceFilter{
			Board:      "default",
			Kind:       publicationQuarantineKind,
			SourceID:   pending.ID,
			ActiveOnly: true,
		},
	)
	if len(sources) != 1 {
		t.Fatalf("publication quarantine sources=%+v", sources)
	}
}

func TestPublicationAttemptResultValidatesSuccessIdentityAndURL(t *testing.T) {
	url := "https://example.test/pull/1"
	tests := []struct {
		name    string
		claimed model.Publication
		result  publisher.Result
		want    store.PublicationAttemptOutcome
	}{
		{
			name: "matching local",
			claimed: model.Publication{
				Mode: model.PublicationModeLocalFF, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			result: publisher.Result{
				Status: publisher.ResultPublished,
				Mode:   model.PublicationModeLocalFF, HeadCommit: "ABCDEF",
				TargetBranch: "main",
			},
			want: store.PublicationAttemptPublished,
		},
		{
			name: "target mismatch",
			claimed: model.Publication{
				Mode: model.PublicationModeLocalFF, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			result: publisher.Result{
				Status: publisher.ResultPublished,
				Mode:   model.PublicationModeLocalFF, HeadCommit: "abcdef",
				TargetBranch: "other",
			},
			want: store.PublicationAttemptUnknown,
		},
		{
			name: "local URL forbidden",
			claimed: model.Publication{
				Mode: model.PublicationModeLocalFF, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			result: publisher.Result{
				Status: publisher.ResultPublished,
				Mode:   model.PublicationModeLocalFF, HeadCommit: "abcdef",
				TargetBranch: "main", URL: &url,
			},
			want: store.PublicationAttemptUnknown,
		},
		{
			name: "published pull request requires URL",
			claimed: model.Publication{
				Mode: model.PublicationModePullRequest, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			result: publisher.Result{
				Status: publisher.ResultPublished,
				Mode:   model.PublicationModePullRequest, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			want: store.PublicationAttemptUnknown,
		},
		{
			name: "matching pull request",
			claimed: model.Publication{
				Mode: model.PublicationModePullRequest, HeadCommit: "abcdef",
				TargetBranch: "main",
			},
			result: publisher.Result{
				Status: publisher.ResultPublished,
				Mode:   model.PublicationModePullRequest, HeadCommit: "abcdef",
				TargetBranch: "main", URL: &url,
			},
			want: store.PublicationAttemptPublished,
		},
	}
	observation := &publicationCommandObservation{
		gateCalls:       1,
		commandReleased: true,
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := publicationAttemptExecutionResult(
				test.claimed,
				test.result,
				nil,
				observation,
			)
			if result.Outcome != test.want {
				t.Fatalf("result=%+v", result)
			}
			if result.Outcome == store.PublicationAttemptUnknown &&
				result.ErrorKind != store.PublicationErrorInternal {
				t.Fatalf("unknown result=%+v", result)
			}
		})
	}
}
