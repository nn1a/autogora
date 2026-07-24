package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	claimed, acquired, err := opened.ClaimPublication(
		ctx,
		value.ID,
		store.ClaimPublicationInput{
			ExpectedUpdatedAt: value.UpdatedAt,
			TTL:               options.PublicationClaimTTL,
		},
	)
	if err != nil {
		if errors.Is(err, store.ErrPublicationStateConflict) ||
			errors.Is(err, store.ErrPublicationUpdateConflict) {
			return false, nil
		}
		return false, fmt.Errorf("claim publication %s: %w", value.ID, err)
	}
	if !acquired {
		return false, nil
	}
	if capabilityErr := automaticPublicationCapabilityFailure(
		claimed,
		capability,
	); capabilityErr != nil {
		persistenceContext, cancelPersistence := context.WithTimeout(
			context.WithoutCancel(ctx),
			publicationPersistenceTimeout,
		)
		defer cancelPersistence()
		_, failErr := opened.FailPublication(
			persistenceContext,
			claimed.ID,
			store.FailPublicationInput{
				ExpectedUpdatedAt: claimed.UpdatedAt,
				ClaimToken:        claimed.ClaimToken,
				ClaimEpoch:        claimed.ClaimEpoch,
				Error:             boundedPublicationFailure(capabilityErr),
			},
		)
		if failErr != nil {
			return true, errors.Join(
				fmt.Errorf(
					"block unsupported automatic publication %s: %w",
					claimed.ID,
					capabilityErr,
				),
				fmt.Errorf(
					"persist publication %s capability failure: %w",
					claimed.ID,
					failErr,
				),
			)
		}
		return true, fmt.Errorf(
			"block unsupported automatic publication %s: %w",
			claimed.ID,
			capabilityErr,
		)
	}
	executionContext, cancel := context.WithTimeout(ctx, options.PublicationTimeout)
	result, executionErr := options.PublicationExecutor(
		executionContext,
		claimed,
		publisher.Options{CommandTimeout: options.PublicationTimeout},
	)
	cancel()
	if errors.Is(executionErr, processguard.ErrTeardownUnconfirmed) {
		quarantineErr := options.automationSession.quarantinePublicationTeardown(
			opened,
			claimed,
		)
		return true, errors.Join(
			fmt.Errorf(
				"execute publication %s with unconfirmed teardown: %w",
				claimed.ID,
				executionErr,
			),
			quarantineErr,
		)
	}
	persistenceContext, cancelPersistence := context.WithTimeout(
		context.WithoutCancel(ctx), publicationPersistenceTimeout,
	)
	defer cancelPersistence()
	if executionErr != nil {
		_, failErr := opened.FailPublication(
			persistenceContext,
			claimed.ID,
			store.FailPublicationInput{
				ExpectedUpdatedAt: claimed.UpdatedAt,
				ClaimToken:        claimed.ClaimToken,
				ClaimEpoch:        claimed.ClaimEpoch,
				Error:             boundedPublicationFailure(executionErr),
			},
		)
		if failErr != nil {
			return true, errors.Join(
				fmt.Errorf("execute publication %s: %w", claimed.ID, executionErr),
				fmt.Errorf("persist publication %s failure: %w", claimed.ID, failErr),
			)
		}
		return true, fmt.Errorf("execute publication %s: %w", claimed.ID, executionErr)
	}
	if _, err := opened.CompletePublication(
		persistenceContext,
		claimed.ID,
		store.CompletePublicationInput{
			ExpectedUpdatedAt: claimed.UpdatedAt,
			ClaimToken:        claimed.ClaimToken,
			ClaimEpoch:        claimed.ClaimEpoch,
			URL:               result.URL,
		},
	); err != nil {
		return true, fmt.Errorf("complete publication %s: %w", claimed.ID, err)
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
