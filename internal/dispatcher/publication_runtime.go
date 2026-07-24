package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
	"github.com/nn1a/autogora/internal/publisher"
	"github.com/nn1a/autogora/internal/store"
)

const (
	publicationCandidatePageSize  = 100
	publicationClaimGrace         = 15 * time.Second
	publicationPersistenceTimeout = 15 * time.Second

	publicationAttemptUnknownDiagnostic    = "publication_attempt_result_unknown"
	publicationPermitBoundaryDiagnostic    = "publication_attempt_permit_boundary_unconfirmed"
	publicationResultPersistenceDiagnostic = "publication_attempt_result_persistence_unconfirmed"
)

type PublicationExecutor func(
	context.Context,
	model.Publication,
	publisher.Options,
) (publisher.Result, error)

type publicationRuntimeState struct {
	nextBoard string
}

func (s *publicationRuntimeState) orderedBoards(boardSlugs []string) []string {
	next := ""
	if s != nil {
		next = s.nextBoard
	}
	ordered := rotatedBoardSlugs(boardSlugs, next)
	if s != nil && len(ordered) > 0 {
		// Advance even when no publication is executable so an idle or
		// misconfigured board cannot permanently own the first probe.
		s.nextBoard = boardAfter(ordered, ordered[0])
	}
	return ordered
}

func (s *publicationRuntimeState) advanceAfter(boardSlugs []string, board string) {
	if s != nil {
		s.nextBoard = boardAfter(boardSlugs, board)
	}
}

func publicationMode(value boards.PublicationMode) (model.PublicationMode, error) {
	mode := model.PublicationMode(value)
	if !model.ValidPublicationMode(mode) {
		return "", fmt.Errorf("invalid board publication mode: %s", value)
	}
	return mode, nil
}

func ensureBoardPublications(
	ctx context.Context,
	opened *store.Store,
	board string,
	policy boards.PublicationSettings,
) ([]model.Publication, error) {
	mode, err := publicationMode(policy.Mode)
	if err != nil {
		return nil, err
	}
	policySnapshot, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("encode publication policy: %w", err)
	}
	var cursor *store.TaskListCursor
	candidates := make([]model.Publication, 0)
	var resultErr error
	for {
		tasks, err := opened.ListTasks(ctx, store.ListTaskFilter{
			Board: board, Status: model.TaskStatusDone,
			Sort: "priority-desc", Limit: publicationCandidatePageSize,
			After: cursor,
		})
		if err != nil {
			return candidates, errors.Join(resultErr, err)
		}
		if len(tasks) == 0 {
			return candidates, resultErr
		}
		for _, task := range tasks {
			if task.WorkflowRole != model.WorkflowRoleFinalizer {
				continue
			}
			detail, err := opened.GetTask(ctx, task.ID)
			if err != nil {
				resultErr = errors.Join(
					resultErr,
					fmt.Errorf("inspect finalizer %s: %w", task.ID, err),
				)
				continue
			}
			if len(detail.ChangeSets) == 0 {
				continue
			}
			// Change sets are returned in creation order. A reopened finalizer
			// can complete again, but only its latest immutable result is a new
			// publication handoff.
			changeSet := detail.ChangeSets[len(detail.ChangeSets)-1]
			publication, _, err := opened.EnsurePublication(
				ctx,
				store.EnsurePublicationInput{
					Board: board, ChangeSetID: changeSet.ID,
					Mode: mode, TargetBranch: policy.TargetBranch,
					Remote: policy.Remote, RequireApproval: policy.RequireApproval,
					PolicySnapshot: policySnapshot,
				},
			)
			if err != nil {
				resultErr = errors.Join(
					resultErr,
					fmt.Errorf("ensure publication for finalizer %s: %w", task.ID, err),
				)
				continue
			}
			candidates = append(candidates, publication)
		}
		if len(tasks) < publicationCandidatePageSize {
			return candidates, resultErr
		}
		last := tasks[len(tasks)-1]
		cursor = &store.TaskListCursor{
			Priority: last.Priority, CreatedAt: last.CreatedAt, ID: last.ID,
		}
	}
}

func automaticPublicationCandidate(value model.Publication) bool {
	if value.Mode == model.PublicationModeManual {
		return false
	}
	if value.Mode != model.PublicationModeLocalFF &&
		value.Mode != model.PublicationModePullRequest {
		return false
	}
	return value.Status == model.PublicationPending ||
		value.Status == model.PublicationPublishing
}

func boundedPublicationFailure(err error) string {
	value := "publication execution failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		value = strings.TrimSpace(err.Error())
	}
	value = strings.ReplaceAll(value, "\x00", "\uFFFD")
	if len(value) <= store.MaxPublicationErrorBytes {
		return value
	}
	value = value[:store.MaxPublicationErrorBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

type publicationCommandObservation struct {
	mu              sync.Mutex
	gateCalls       int
	commandReleased bool
}

func (o *publicationCommandObservation) beginGateCall() {
	o.mu.Lock()
	o.gateCalls++
	o.mu.Unlock()
}

func (o *publicationCommandObservation) observeRelease(released bool) {
	if !released {
		return
	}
	o.mu.Lock()
	o.commandReleased = true
	o.mu.Unlock()
}

func (o *publicationCommandObservation) snapshot() (int, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gateCalls, o.commandReleased
}

// publicationReleaseGate authorizes exactly one command start with a fresh
// session permit. Released is scoped to this command; callers must retain
// attempt-wide release evidence because a later blocked command does not undo
// an earlier command start.
func (s *automationDispatcherSession) publicationReleaseGate(
	opened *store.Store,
	attempt *store.PublicationAttemptPermit,
	observation *publicationCommandObservation,
) publisher.CommandReleaseGate {
	return func(
		ctx context.Context,
		release publisher.CommandRelease,
	) (released bool, resultErr error) {
		if s == nil {
			return false, errors.New(
				"automation dispatcher session is required for publication command start",
			)
		}
		if opened == nil || attempt == nil || observation == nil {
			return false, errors.New(
				"publication command start capability is incomplete",
			)
		}
		if release == nil {
			return false, errors.New(
				"publication command release callback is required",
			)
		}
		observation.beginGateCall()
		permit, err := opened.AcquireAutomationPermitForSession(
			ctx,
			s.currentLease(),
		)
		if err != nil {
			if !isCallerContextFailure(ctx, err) {
				s.recordPermitBoundaryFailure(err)
			}
			return false, err
		}
		released, startErr := opened.WithPublicationAttemptCommandStart(
			ctx,
			permit,
			attempt,
			func() (bool, error) {
				released, releaseErr := release()
				observation.observeRelease(released)
				return released, releaseErr
			},
		)
		observation.observeRelease(released)
		closeErr := permit.Close()
		if closeErr != nil {
			quarantineErr := s.activateUnconfirmedQuarantine(
				publicationPermitBoundaryDiagnostic,
			)
			return released, errors.Join(startErr, closeErr, quarantineErr)
		}
		if startErr != nil &&
			!isCallerContextFailure(ctx, startErr) &&
			(isAutomationPermitAuthorityFailure(startErr) ||
				errors.Is(startErr, store.ErrPublicationAttemptNotFound) ||
				errors.Is(startErr, store.ErrPublicationAttemptPermitClosed) ||
				errors.Is(startErr, store.ErrPublicationAttemptScope)) {
			s.recordPermitBoundaryFailure(startErr)
		}
		return released, startErr
	}
}

func publisherAttemptErrorKind(err error) store.PublicationAttemptErrorKind {
	var execution *publisher.Error
	if errors.As(err, &execution) {
		switch execution.Kind {
		case publisher.ErrorInvalidInput:
			return store.PublicationErrorInvalidInput
		case publisher.ErrorManualMode:
			return store.PublicationErrorManualMode
		case publisher.ErrorRepository:
			return store.PublicationErrorRepository
		case publisher.ErrorSourceChanged:
			return store.PublicationErrorSourceChanged
		case publisher.ErrorNonFastForward:
			return store.PublicationErrorNonFastForward
		case publisher.ErrorDirtyWorktree:
			return store.PublicationErrorDirtyWorktree
		case publisher.ErrorRemoteConflict:
			return store.PublicationErrorRemoteConflict
		case publisher.ErrorCommandTimeout:
			return store.PublicationErrorCommandTimeout
		case publisher.ErrorTeardownUnconfirmed:
			return store.PublicationErrorTeardownUnconfirmed
		case publisher.ErrorCommandFailed:
			return store.PublicationErrorCommandFailed
		case publisher.ErrorCanceled:
			return store.PublicationErrorCanceled
		case publisher.ErrorCommandStartBlocked:
			return store.PublicationErrorCommandStartBlocked
		case publisher.ErrorCommandStartUncertain:
			return store.PublicationErrorCommandStartUncertain
		default:
			return store.PublicationErrorUnknown
		}
	}
	switch {
	case errors.Is(err, processguard.ErrTeardownUnconfirmed):
		return store.PublicationErrorTeardownUnconfirmed
	case errors.Is(err, publisher.ErrCommandStartUncertain):
		return store.PublicationErrorCommandStartUncertain
	case errors.Is(err, publisher.ErrCommandStartBlocked):
		return store.PublicationErrorCommandStartBlocked
	case errors.Is(err, publisher.ErrCommandTimeout),
		errors.Is(err, context.DeadlineExceeded):
		return store.PublicationErrorCommandTimeout
	case errors.Is(err, context.Canceled):
		return store.PublicationErrorCanceled
	case errors.Is(err, publisher.ErrCommandFailed):
		return store.PublicationErrorCommandFailed
	default:
		return store.PublicationErrorInternal
	}
}

func commandStartDefinitelyBlocked(err error) bool {
	var execution *publisher.Error
	if errors.As(err, &execution) {
		return execution.Kind == publisher.ErrorCommandStartBlocked
	}
	return errors.Is(err, publisher.ErrCommandStartBlocked)
}

func deterministicPreCommandPublicationFailure(err error) bool {
	var execution *publisher.Error
	if !errors.As(err, &execution) {
		return false
	}
	switch execution.Kind {
	case publisher.ErrorInvalidInput,
		publisher.ErrorManualMode,
		publisher.ErrorRepository:
		return true
	default:
		return false
	}
}

func verifiedPublicationSuccess(
	claimed model.Publication,
	result publisher.Result,
) bool {
	if result.Status != publisher.ResultPublished &&
		result.Status != publisher.ResultAlreadyPublished {
		return false
	}
	if result.Mode != claimed.Mode ||
		!strings.EqualFold(result.HeadCommit, claimed.HeadCommit) ||
		result.TargetBranch != claimed.TargetBranch {
		return false
	}
	switch claimed.Mode {
	case model.PublicationModeLocalFF:
		return result.URL == nil
	case model.PublicationModePullRequest:
		if result.Status == publisher.ResultPublished {
			return result.URL != nil &&
				strings.TrimSpace(*result.URL) != ""
		}
		return result.URL == nil ||
			strings.TrimSpace(*result.URL) != ""
	default:
		return false
	}
}

func publicationAttemptExecutionResult(
	claimed model.Publication,
	result publisher.Result,
	executionErr error,
	observation *publicationCommandObservation,
) store.PublicationAttemptResultInput {
	gateCalls, commandReleased := observation.snapshot()
	if executionErr == nil && gateCalls > 0 && commandReleased &&
		verifiedPublicationSuccess(claimed, result) {
		switch result.Status {
		case publisher.ResultPublished:
			return store.PublicationAttemptResultInput{
				Outcome:        store.PublicationAttemptPublished,
				ExecutorStatus: store.PublicationExecutorPublished,
				URL:            result.URL,
			}
		case publisher.ResultAlreadyPublished:
			return store.PublicationAttemptResultInput{
				Outcome:        store.PublicationAttemptPublished,
				ExecutorStatus: store.PublicationExecutorAlreadyPublished,
				URL:            result.URL,
			}
		}
	}
	if executionErr != nil && gateCalls == 0 && !commandReleased &&
		deterministicPreCommandPublicationFailure(executionErr) {
		status := store.PublicationExecutorFailed
		var execution *publisher.Error
		if errors.As(executionErr, &execution) &&
			execution.Kind == publisher.ErrorManualMode {
			status = store.PublicationExecutorManualRequired
		}
		return store.PublicationAttemptResultInput{
			Outcome:        store.PublicationAttemptFailed,
			ExecutorStatus: status,
			ErrorKind:      publisherAttemptErrorKind(executionErr),
			Error:          boundedPublicationFailure(executionErr),
		}
	}
	if executionErr != nil && !commandReleased &&
		commandStartDefinitelyBlocked(executionErr) {
		return store.PublicationAttemptResultInput{
			Outcome:        store.PublicationAttemptFailed,
			ExecutorStatus: store.PublicationExecutorFailed,
			ErrorKind:      store.PublicationErrorCommandStartBlocked,
			Error:          boundedPublicationFailure(executionErr),
		}
	}
	unknownErr := executionErr
	if unknownErr == nil {
		unknownErr = errors.New(
			"publication executor returned no command-start evidence or an invalid success status",
		)
	}
	return store.PublicationAttemptResultInput{
		Outcome:        store.PublicationAttemptUnknown,
		ExecutorStatus: store.PublicationExecutorUnknown,
		ErrorKind:      publisherAttemptErrorKind(unknownErr),
		Error:          boundedPublicationFailure(unknownErr),
	}
}

func exactPublicationAttemptValidator(
	opened *store.Store,
	intent store.PublicationAttemptIntent,
) store.AutomationQuarantineSourceValidator {
	return func(
		ctx context.Context,
		input store.AutomationQuarantineSourceInput,
	) (bool, error) {
		exact, err := opened.ValidatePublishingAutomationSource(ctx, input)
		if err != nil || !exact {
			return exact, err
		}
		current, err := opened.GetPublication(ctx, intent.PublicationID)
		if err != nil {
			return false, err
		}
		return current.Status == model.PublicationPublishing &&
			current.ID == intent.PublicationID &&
			current.Board == intent.Board &&
			current.ChangeSetID == intent.ChangeSetID &&
			current.Mode == intent.Mode &&
			current.TargetBranch == intent.TargetBranch &&
			current.Remote == intent.Remote &&
			current.BaseCommit == intent.BaseCommit &&
			current.HeadCommit == intent.HeadCommit &&
			current.DurableRef == intent.DurableRef &&
			current.ClaimEpoch == intent.ClaimEpoch &&
			current.UpdatedAt == intent.PublicationUpdatedAt, nil
	}
}

func publicationAttemptIntentFromClaimed(
	value model.Publication,
) store.PublicationAttemptIntent {
	return store.PublicationAttemptIntent{
		Board:                value.Board,
		PublicationID:        value.ID,
		ChangeSetID:          value.ChangeSetID,
		Mode:                 value.Mode,
		TargetBranch:         value.TargetBranch,
		Remote:               value.Remote,
		BaseCommit:           value.BaseCommit,
		HeadCommit:           value.HeadCommit,
		DurableRef:           value.DurableRef,
		ClaimEpoch:           value.ClaimEpoch,
		PublicationUpdatedAt: value.UpdatedAt,
	}
}

func (s *automationDispatcherSession) quarantinePublicationAttempt(
	opened *store.Store,
	value model.Publication,
	intent store.PublicationAttemptIntent,
	diagnostic string,
) error {
	if s == nil {
		return errors.New(
			"automation dispatcher session is required for publication quarantine",
		)
	}
	s.unconfirmed.Store(true)
	operationContext, cancel := context.WithTimeout(
		context.Background(),
		automationSessionOperationLimit,
	)
	gate, exactErr := s.persistPublicationQuarantineWithDiagnostic(
		operationContext,
		value,
		diagnostic,
		exactPublicationAttemptValidator(opened, intent),
	)
	cancel()
	if exactErr == nil {
		s.sourceSaved.Store(true)
		return s.observeGate(gate)
	}
	return errors.Join(
		exactErr,
		s.activateUnconfirmedQuarantine(diagnostic),
	)
}

func optionalPublicationResultStringEqual(left, right *string) bool {
	switch {
	case left == nil || strings.TrimSpace(*left) == "":
		return right == nil || strings.TrimSpace(*right) == ""
	case right == nil || strings.TrimSpace(*right) == "":
		return false
	default:
		return strings.TrimSpace(*left) == strings.TrimSpace(*right)
	}
}

func publicationAttemptResultMatches(
	result *store.PublicationAttemptResult,
	input store.PublicationAttemptResultInput,
) bool {
	if result == nil ||
		result.Outcome != input.Outcome ||
		result.ExecutorStatus != input.ExecutorStatus ||
		!optionalPublicationResultStringEqual(result.URL, input.URL) {
		return false
	}
	switch {
	case result.ErrorKind == nil:
		if input.ErrorKind != "" {
			return false
		}
	case *result.ErrorKind != input.ErrorKind:
		return false
	}
	switch {
	case result.Error == nil:
		return strings.TrimSpace(input.Error) == ""
	default:
		return *result.Error == input.Error
	}
}

func reconcilePublicationAttemptResult(
	ctx context.Context,
	opened *store.Store,
	attemptID string,
	input store.PublicationAttemptResultInput,
) (bool, error) {
	record, err := opened.GetPublicationAttempt(ctx, attemptID)
	if err != nil {
		return false, err
	}
	return publicationAttemptResultMatches(record.Result, input), nil
}

func finishPublicationAttempt(
	ctx context.Context,
	opened *store.Store,
	session *automationDispatcherSession,
	claimed model.Publication,
	attempt *store.PublicationAttemptPermit,
	input store.PublicationAttemptResultInput,
) error {
	persistenceContext, cancelPersistence := context.WithTimeout(
		context.WithoutCancel(ctx),
		publicationPersistenceTimeout,
	)
	_, finishErr := opened.FinishAutomatedPublicationAttempt(
		persistenceContext,
		attempt,
		input,
	)
	cancelPersistence()
	reconciled := finishErr == nil
	var reconcileErr error
	if finishErr != nil {
		reconcileContext, cancelReconcile := context.WithTimeout(
			context.WithoutCancel(ctx),
			publicationPersistenceTimeout,
		)
		matched, err := reconcilePublicationAttemptResult(
			reconcileContext,
			opened,
			attempt.Intent().ID,
			input,
		)
		cancelReconcile()
		reconcileErr = err
		reconciled = err == nil && matched
	}
	var boundaryErr error
	if reconciled && finishErr != nil &&
		isAutomationPermitAuthorityFailure(finishErr) {
		boundaryErr = errors.Join(
			fmt.Errorf(
				"publication %s attempt result committed with an uncertain authority boundary: %w",
				claimed.ID,
				finishErr,
			),
			session.activateUnconfirmedQuarantine(
				publicationResultPersistenceDiagnostic,
			),
		)
		if input.Outcome != store.PublicationAttemptUnknown {
			return boundaryErr
		}
	}
	if reconciled && input.Outcome != store.PublicationAttemptUnknown {
		return nil
	}
	diagnostic := publicationAttemptUnknownDiagnostic
	if !reconciled {
		diagnostic = publicationResultPersistenceDiagnostic
	}
	quarantineErr := session.quarantinePublicationAttempt(
		opened,
		claimed,
		attempt.Intent(),
		diagnostic,
	)
	if finishErr != nil {
		return errors.Join(
			boundaryErr,
			fmt.Errorf(
				"persist publication %s attempt result: %w",
				claimed.ID,
				finishErr,
			),
			reconcileErr,
			quarantineErr,
		)
	}
	return errors.Join(boundaryErr, quarantineErr)
}

func executePublication(
	ctx context.Context,
	opened *store.Store,
	value model.Publication,
	options Options,
) (bool, error) {
	return executePublicationWithCapability(
		ctx,
		opened,
		value,
		options,
		currentAutomaticMutationCapability(),
	)
}

func executePublicationWithCapability(
	ctx context.Context,
	opened *store.Store,
	value model.Publication,
	options Options,
	capability automaticMutationCapability,
) (bool, error) {
	if options.automationSession == nil {
		return false, errors.New(
			"automation dispatcher session is required for automatic publication",
		)
	}
	basePermit, err := opened.AcquireAutomationPermitForSession(
		ctx,
		options.automationSession.currentLease(),
	)
	if err != nil {
		if !isCallerContextFailure(ctx, err) {
			options.automationSession.recordPermitBoundaryFailure(err)
		}
		return false, fmt.Errorf(
			"authorize publication %s attempt: %w",
			value.ID,
			err,
		)
	}
	claimed, attempt, acquired, beginErr := opened.BeginAutomatedPublicationAttempt(
		ctx,
		basePermit,
		value.ID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: value.UpdatedAt,
			TTL:               options.PublicationClaimTTL,
		},
	)
	closeErr := basePermit.Close()
	if closeErr != nil {
		closeErr = errors.Join(
			closeErr,
			options.automationSession.activateUnconfirmedQuarantine(
				publicationPermitBoundaryDiagnostic,
			),
		)
	}
	if beginErr != nil {
		boundaryErr := errors.Join(beginErr, closeErr)
		if !isCallerContextFailure(ctx, beginErr) &&
			isAutomationPermitAuthorityFailure(beginErr) {
			options.automationSession.recordPermitBoundaryFailure(boundaryErr)
		}
		if errors.Is(beginErr, store.ErrPublicationStateConflict) ||
			errors.Is(beginErr, store.ErrPublicationUpdateConflict) {
			if closeErr != nil {
				options.automationSession.recordPermitBoundaryFailure(closeErr)
				return false, fmt.Errorf(
					"close contended publication %s attempt permit: %w",
					value.ID,
					closeErr,
				)
			}
			return false, nil
		}
		return false, fmt.Errorf(
			"begin publication %s attempt: %w",
			value.ID,
			boundaryErr,
		)
	}
	if !acquired {
		if closeErr != nil {
			options.automationSession.recordPermitBoundaryFailure(closeErr)
			return false, fmt.Errorf(
				"close publication %s attempt permit: %w",
				value.ID,
				closeErr,
			)
		}
		return false, nil
	}
	if attempt == nil {
		capabilityErr := errors.New(
			"publication attempt was acquired without an attempt capability",
		)
		options.automationSession.recordPermitBoundaryFailure(capabilityErr)
		return true, errors.Join(
			capabilityErr,
			options.automationSession.quarantinePublicationAttempt(
				opened,
				claimed,
				publicationAttemptIntentFromClaimed(claimed),
				publicationPermitBoundaryDiagnostic,
			),
		)
	}
	if closeErr != nil {
		input := store.PublicationAttemptResultInput{
			Outcome:        store.PublicationAttemptUnknown,
			ExecutorStatus: store.PublicationExecutorUnknown,
			ErrorKind:      store.PublicationErrorInternal,
			Error: boundedPublicationFailure(
				fmt.Errorf("close publication attempt permit: %w", closeErr),
			),
		}
		finishErr := finishPublicationAttempt(
			ctx,
			opened,
			options.automationSession,
			claimed,
			attempt,
			input,
		)
		return true, errors.Join(closeErr, finishErr)
	}
	if capabilityErr := automaticPublicationCapabilityFailure(
		claimed,
		capability,
	); capabilityErr != nil {
		finishErr := finishPublicationAttempt(
			ctx,
			opened,
			options.automationSession,
			claimed,
			attempt,
			store.PublicationAttemptResultInput{
				Outcome:        store.PublicationAttemptFailed,
				ExecutorStatus: store.PublicationExecutorFailed,
				ErrorKind:      store.PublicationErrorInternal,
				Error:          boundedPublicationFailure(capabilityErr),
			},
		)
		if finishErr != nil {
			return true, errors.Join(
				fmt.Errorf(
					"block unsupported automatic publication %s: %w",
					claimed.ID,
					capabilityErr,
				),
				fmt.Errorf(
					"persist publication %s capability failure: %w",
					claimed.ID,
					finishErr,
				),
			)
		}
		return true, fmt.Errorf(
			"block unsupported automatic publication %s: %w",
			claimed.ID,
			capabilityErr,
		)
	}
	observation := &publicationCommandObservation{}
	executionContext, cancel := context.WithTimeout(ctx, options.PublicationTimeout)
	result, executionErr := options.PublicationExecutor(
		executionContext,
		claimed,
		publisher.Options{
			CommandTimeout: options.PublicationTimeout,
			ReleaseGate: options.automationSession.publicationReleaseGate(
				opened,
				attempt,
				observation,
			),
		},
	)
	cancel()
	attemptResult := publicationAttemptExecutionResult(
		claimed,
		result,
		executionErr,
		observation,
	)
	finishErr := finishPublicationAttempt(
		ctx,
		opened,
		options.automationSession,
		claimed,
		attempt,
		attemptResult,
	)
	if executionErr != nil {
		return true, errors.Join(
			fmt.Errorf("execute publication %s: %w", claimed.ID, executionErr),
			finishErr,
		)
	}
	if attemptResult.Outcome == store.PublicationAttemptUnknown {
		return true, errors.Join(
			errors.New(
				"publication executor returned an unverified success result",
			),
			finishErr,
		)
	}
	if finishErr != nil {
		return true, finishErr
	}
	return true, nil
}

// runPublicationPass discovers every completed finalizer while allowing at
// most one external Git/GitHub operation. Discovery is durable even when the
// process-wide or board-wide write gate blocks execution.
func runPublicationPass(
	ctx context.Context,
	manager *boards.Manager,
	boardSlugs []string,
	options Options,
	state *publicationRuntimeState,
	_ time.Time,
) error {
	if !options.Autopilot {
		return nil
	}
	ordered := state.orderedBoards(boardSlugs)
	executed := false
	var passErr error
	for _, board := range ordered {
		if ctx.Err() != nil {
			return errors.Join(passErr, ctx.Err())
		}
		metadata, err := manager.Read(board)
		if err != nil {
			passErr = errors.Join(
				passErr, fmt.Errorf("publication board %s: %w", board, err),
			)
			continue
		}
		autopilot := metadata.Orchestration.Autopilot
		if !autopilot.Enabled {
			continue
		}
		opened, err := manager.OpenStore(ctx, board)
		if err != nil {
			passErr = errors.Join(
				passErr, fmt.Errorf("publication board %s: %w", board, err),
			)
			continue
		}
		publications, discoverErr := ensureBoardPublications(
			ctx, opened, board, autopilot.Publication,
		)
		if discoverErr != nil {
			passErr = errors.Join(
				passErr,
				fmt.Errorf("publication board %s discovery: %w", board, discoverErr),
			)
		}
		canWrite := options.AllowWrites && autopilot.WorkspaceWrites
		if !executed && canWrite {
			for _, publication := range publications {
				if !automaticPublicationCandidate(publication) {
					continue
				}
				// A claim is the one external-attempt budget for this pass even
				// when execution fails. A live publishing lease is not acquired
				// and therefore does not consume the budget.
				acquired, executeErr := executePublication(
					ctx, opened, publication, options,
				)
				if acquired {
					executed = true
					state.advanceAfter(ordered, board)
				}
				if executeErr != nil {
					passErr = errors.Join(
						passErr,
						fmt.Errorf("publication board %s: %w", board, executeErr),
					)
				}
				if acquired {
					break
				}
			}
		}
		if closeErr := opened.Close(); closeErr != nil {
			passErr = errors.Join(
				passErr, fmt.Errorf("publication board %s close: %w", board, closeErr),
			)
		}
	}
	return passErr
}
