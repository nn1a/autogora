package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const publicationListDefaultLimit = 100

const publicationHelp = `autogora publication <action> [options]

Actions:
  status                Show board policy and recent publication status counts
  list                  List publication handoffs with optional filters
  show <id>             Show one publication handoff
  approve <id>          Approve a publication awaiting approval
  reject <id>           Reject and supersede a publication
  retry <id>            Retry a failed publication
  complete <id>         Record completion of a manual publication

Options:
  --board <slug>        Select a board
  --db <path>           Override the database path
  --status <status>     Filter list by publication status
  --task <task-id>      Filter list by finalizer task
  --limit <number>      Return 1-500 publications (default 100)
  --updated-at <time>   Required current version for mutations
  --reason <text>       Required rejection reason
  --url <url>           Optional URL for manual completion

Publisher claim tokens are internal and are never accepted by these commands.
`

type publicationMutationOutput struct {
	Action      string            `json:"action"`
	Outcome     string            `json:"outcome"`
	Publication model.Publication `json:"publication"`
}

func publicationMutationID(opts options, action string) (string, error) {
	if len(opts.positionals) != 2 {
		return "", fmt.Errorf(
			"publication %s requires exactly one publication id",
			action,
		)
	}
	id := strings.TrimSpace(opts.positionals[1])
	if id == "" {
		return "", fmt.Errorf(
			"publication %s requires exactly one publication id",
			action,
		)
	}
	return id, nil
}

func publicationUpdatedAtOption(opts options, action string) (string, error) {
	value := strings.TrimSpace(opts.value("updated-at"))
	if value == "" {
		return "", fmt.Errorf("publication %s requires --updated-at", action)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return "", fmt.Errorf(
			"publication %s --updated-at must be RFC3339: %w",
			action,
			err,
		)
	}
	return value, nil
}

func publicationMutationError(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrPublicationStateConflict) ||
		errors.Is(err, store.ErrPublicationUpdateConflict) {
		return fmt.Errorf(
			"publication %s conflict; refresh the publication and retry with its current --updated-at: %w",
			action,
			err,
		)
	}
	return fmt.Errorf("publication %s failed: %w", action, err)
}

func publicationListFilter(
	board string,
	opts options,
) (store.PublicationFilter, error) {
	limit, err := numberOption(opts.value("limit"), publicationListDefaultLimit)
	if err != nil {
		return store.PublicationFilter{}, err
	}
	if limit < 1 || limit > 500 {
		return store.PublicationFilter{},
			errors.New("publication limit must be between 1 and 500")
	}
	filter := store.PublicationFilter{
		Board:  board,
		TaskID: strings.TrimSpace(opts.value("task")),
		Limit:  limit,
	}
	if raw := strings.TrimSpace(opts.value("status")); raw != "" {
		filter.Status = model.PublicationStatus(raw)
		if !model.ValidPublicationStatus(filter.Status) {
			return store.PublicationFilter{}, fmt.Errorf(
				"invalid publication status: %s",
				raw,
			)
		}
	}
	return filter, nil
}

func (a *App) mutatePublication(
	ctx context.Context,
	opened *store.Store,
	opts options,
	action string,
) error {
	id, err := publicationMutationID(opts, action)
	if err != nil {
		return err
	}
	updatedAt, err := publicationUpdatedAtOption(opts, action)
	if err != nil {
		return err
	}
	if _, err := opened.GetPublication(ctx, id); err != nil {
		return publicationMutationError(action, err)
	}

	var value model.Publication
	switch action {
	case "approve":
		value, err = opened.ApprovePublication(
			ctx,
			id,
			store.ApprovePublicationInput{ExpectedUpdatedAt: updatedAt},
		)
	case "reject":
		reason := strings.TrimSpace(opts.value("reason"))
		if reason == "" {
			return errors.New("publication reject requires --reason")
		}
		value, err = opened.SupersedePublication(
			ctx,
			id,
			store.SupersedePublicationInput{
				ExpectedUpdatedAt: updatedAt,
				Reason:            reason,
			},
		)
	case "retry":
		value, err = opened.RetryPublication(
			ctx,
			id,
			store.RetryPublicationInput{ExpectedUpdatedAt: updatedAt},
		)
	case "complete":
		var url *string
		if opts.present("url") {
			value := opts.value("url")
			url = &value
		}
		value, err = opened.CompleteManualPublication(
			ctx,
			id,
			store.CompleteManualPublicationInput{
				ExpectedUpdatedAt: updatedAt,
				URL:               url,
			},
		)
	default:
		return fmt.Errorf("invalid publication mutation action: %s", action)
	}
	if err != nil {
		return publicationMutationError(action, err)
	}
	outcome := map[string]string{
		"approve":  "approved",
		"reject":   "superseded",
		"retry":    "pending",
		"complete": "published",
	}[action]
	return writeJSON(a.Stdout, publicationMutationOutput{
		Action: action, Outcome: outcome, Publication: value,
	})
}

func (a *App) runPublication(ctx context.Context, opts options) error {
	if opts.present("claim-token") || opts.present("claimToken") {
		return errors.New("publication commands do not accept claim tokens")
	}
	action := "status"
	if len(opts.positionals) > 0 {
		action = strings.ToLower(strings.TrimSpace(opts.positionals[0]))
	}
	opened, manager, board, err := a.openStore(ctx, opts)
	if err != nil {
		return err
	}
	defer opened.Close()

	switch action {
	case "status":
		if len(opts.positionals) > 1 {
			return errors.New("publication status does not accept arguments")
		}
		limit, err := numberOption(opts.value("limit"), publicationListDefaultLimit)
		if err != nil {
			return err
		}
		if limit < 1 || limit > 500 {
			return errors.New("publication limit must be between 1 and 500")
		}
		metadata, err := manager.Read(board)
		if err != nil {
			return err
		}
		values, err := opened.ListPublications(
			ctx,
			store.PublicationFilter{Board: board, Limit: limit},
		)
		if err != nil {
			return err
		}
		counts := make(map[model.PublicationStatus]int, len(model.PublicationStatuses))
		for _, status := range model.PublicationStatuses {
			counts[status] = 0
		}
		for _, value := range values {
			counts[value.Status]++
		}
		return writeJSON(a.Stdout, map[string]any{
			"board":        board,
			"policy":       metadata.Orchestration.Autopilot.Publication,
			"statusCounts": counts,
			"publications": values,
		})
	case "list":
		if len(opts.positionals) > 1 {
			return errors.New("publication list does not accept arguments")
		}
		filter, err := publicationListFilter(board, opts)
		if err != nil {
			return err
		}
		values, err := opened.ListPublications(ctx, filter)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, values)
	case "show":
		id, err := publicationMutationID(opts, "show")
		if err != nil {
			return err
		}
		value, err := opened.GetPublication(ctx, id)
		if err != nil {
			return err
		}
		return writeJSON(a.Stdout, value)
	case "approve", "reject", "retry", "complete":
		return a.mutatePublication(ctx, opened, opts, action)
	default:
		return errors.New(
			"publication requires status, list, show, approve, reject, retry, or complete",
		)
	}
}
