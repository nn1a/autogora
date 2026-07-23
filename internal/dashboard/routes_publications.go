package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

const publicationDefaultLimit = 100

type publicationMutationInput struct {
	ExpectedUpdatedAt string
	Reason            string
	URL               *string
}

func publicationLimit(request *http.Request) (int, error) {
	raw := strings.TrimSpace(request.URL.Query().Get("limit"))
	if raw == "" {
		return publicationDefaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		return 0, errors.New("publication limit must be between 1 and 500")
	}
	return limit, nil
}

func publicationFilter(
	request *http.Request,
	board string,
) (store.PublicationFilter, error) {
	limit, err := publicationLimit(request)
	if err != nil {
		return store.PublicationFilter{}, err
	}
	filter := store.PublicationFilter{
		Board:       board,
		TaskID:      strings.TrimSpace(request.URL.Query().Get("task")),
		RunID:       strings.TrimSpace(request.URL.Query().Get("run")),
		ChangeSetID: strings.TrimSpace(request.URL.Query().Get("changeSet")),
		Limit:       limit,
	}
	if raw := strings.TrimSpace(request.URL.Query().Get("status")); raw != "" {
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

func publicationForBoard(
	request *http.Request,
	opened *store.Store,
	id, board string,
) (model.Publication, error) {
	value, err := opened.GetPublication(request.Context(), strings.TrimSpace(id))
	if err != nil {
		return model.Publication{}, err
	}
	if value.Board != board {
		return model.Publication{}, store.ErrPublicationNotFound
	}
	return value, nil
}

func readPublicationMutation(
	request *http.Request,
	action string,
) (publicationMutationInput, error) {
	body, err := readJSON(request)
	if err != nil {
		return publicationMutationInput{}, err
	}
	allowed := map[string]bool{"expectedUpdatedAt": true}
	switch action {
	case "reject":
		allowed["reason"] = true
	case "complete":
		allowed["url"] = true
	case "approve", "retry":
	default:
		return publicationMutationInput{}, fmt.Errorf(
			"invalid publication mutation action: %s",
			action,
		)
	}
	for key := range body {
		if key == "claimToken" {
			return publicationMutationInput{},
				errors.New("invalid publication mutation field: claimToken; claim tokens are internal")
		}
		if !allowed[key] {
			return publicationMutationInput{}, fmt.Errorf(
				"invalid publication mutation field: %s",
				key,
			)
		}
	}
	rawUpdatedAt, exists := body["expectedUpdatedAt"]
	updatedAt, valid := rawUpdatedAt.(string)
	updatedAt = strings.TrimSpace(updatedAt)
	if !exists || !valid || updatedAt == "" {
		return publicationMutationInput{},
			fmt.Errorf("publication %s requires expectedUpdatedAt", action)
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return publicationMutationInput{}, fmt.Errorf(
			"publication %s expectedUpdatedAt must be RFC3339: %w",
			action,
			err,
		)
	}
	result := publicationMutationInput{ExpectedUpdatedAt: updatedAt}
	if action == "reject" {
		rawReason, exists := body["reason"]
		reason, valid := rawReason.(string)
		result.Reason = strings.TrimSpace(reason)
		if !exists || !valid || result.Reason == "" {
			return publicationMutationInput{},
				errors.New("publication reject requires reason")
		}
	}
	if action == "complete" {
		if rawURL, exists := body["url"]; exists {
			if rawURL == nil {
				result.URL = nil
			} else {
				url, valid := rawURL.(string)
				if !valid {
					return publicationMutationInput{},
						errors.New("publication complete url must be a string or null")
				}
				result.URL = &url
			}
		}
	}
	return result, nil
}

func (s *Server) mutatePublication(
	response http.ResponseWriter,
	request *http.Request,
	id, board, action string,
) error {
	input, err := readPublicationMutation(request, action)
	if err != nil {
		return err
	}
	value, err := usingStore(
		request.Context(),
		s,
		board,
		func(opened *store.Store) (model.Publication, error) {
			if _, err := publicationForBoard(request, opened, id, board); err != nil {
				return model.Publication{}, err
			}
			switch action {
			case "approve":
				return opened.ApprovePublication(
					request.Context(),
					id,
					store.ApprovePublicationInput{
						ExpectedUpdatedAt: input.ExpectedUpdatedAt,
					},
				)
			case "reject":
				return opened.SupersedePublication(
					request.Context(),
					id,
					store.SupersedePublicationInput{
						ExpectedUpdatedAt: input.ExpectedUpdatedAt,
						Reason:            input.Reason,
					},
				)
			case "retry":
				return opened.RetryPublication(
					request.Context(),
					id,
					store.RetryPublicationInput{
						ExpectedUpdatedAt: input.ExpectedUpdatedAt,
					},
				)
			case "complete":
				return opened.CompleteManualPublication(
					request.Context(),
					id,
					store.CompleteManualPublicationInput{
						ExpectedUpdatedAt: input.ExpectedUpdatedAt,
						URL:               input.URL,
					},
				)
			default:
				return model.Publication{}, fmt.Errorf(
					"invalid publication mutation action: %s",
					action,
				)
			}
		},
	)
	if err == nil {
		sendJSON(response, http.StatusOK, value)
	}
	return err
}

func (s *Server) handlePublications(
	response http.ResponseWriter,
	request *http.Request,
	segments []string,
	board string,
) error {
	if len(segments) == 4 {
		if request.Method != http.MethodPost {
			sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
				"error": "Publication actions require POST",
			})
			return nil
		}
		switch segments[3] {
		case "approve", "reject", "retry", "complete":
			return s.mutatePublication(
				response,
				request,
				segments[2],
				board,
				segments[3],
			)
		default:
			sendJSON(response, http.StatusNotFound, map[string]any{
				"error": "Not found",
			})
			return nil
		}
	}
	if request.Method != http.MethodGet {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{
			"error": "Publication endpoint requires GET",
		})
		return nil
	}
	switch len(segments) {
	case 2:
		filter, err := publicationFilter(request, board)
		if err != nil {
			return err
		}
		value, err := usingStore(
			request.Context(),
			s,
			board,
			func(opened *store.Store) ([]model.Publication, error) {
				return opened.ListPublications(request.Context(), filter)
			},
		)
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	case 3:
		value, err := usingStore(
			request.Context(),
			s,
			board,
			func(opened *store.Store) (model.Publication, error) {
				return publicationForBoard(request, opened, segments[2], board)
			},
		)
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	default:
		sendJSON(response, http.StatusNotFound, map[string]any{
			"error": "Not found",
		})
		return nil
	}
}
