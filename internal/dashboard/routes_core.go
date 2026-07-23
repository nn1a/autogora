package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

func usingStore[T any](ctx context.Context, s *Server, board string, handler func(*store.Store) (T, error)) (T, error) {
	var zero T
	opened, err := s.manager.OpenStore(ctx, board)
	if err != nil {
		return zero, err
	}
	defer opened.Close()
	return handler(opened)
}

func (s *Server) boardFrom(request *http.Request) (string, error) {
	return s.manager.Resolve(request.URL.Query().Get("board"))
}

func (s *Server) handleAPI(response http.ResponseWriter, request *http.Request, segments []string) error {
	if len(segments) < 2 || segments[0] != "api" {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	if segments[1] == "events" && len(segments) > 2 && segments[2] == "stream" && request.Method == http.MethodGet {
		return s.streamEvents(response, request)
	}
	if segments[1] == "boards" {
		return s.handleBoards(response, request, segments)
	}
	if segments[1] == "config" {
		return s.handleAgentConfig(response, request, segments)
	}
	if segments[1] == "agents" && len(segments) == 3 {
		switch segments[2] {
		case "effective":
			return s.handleEffectiveAgents(response, request)
		case "detect":
			return s.handleDetectAgents(response, request)
		case "presets":
			return s.handleAgentPresets(response, request)
		}
	}
	if segments[1] == "supervisor" {
		return s.handleSupervisor(response, request, segments)
	}
	if segments[1] == "operations" {
		return s.handleOperations(response, request, segments)
	}
	board, err := s.boardFrom(request)
	if err != nil {
		return err
	}
	switch segments[1] {
	case "board":
		return s.handleBoardSnapshot(response, request, board)
	case "tasks":
		return s.handleTasks(response, request, segments, board)
	case "links":
		return s.handleLinks(response, request, board)
	case "hierarchy":
		return s.handleHierarchy(response, request, board)
	case "events":
		return s.handleEvents(response, request, board)
	case "coordination":
		return s.handleCoordination(response, request, segments, board)
	case "publications":
		return s.handlePublications(response, request, segments, board)
	case "stats":
		value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) { return opened.Stats(request.Context(), board) })
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	case "diagnostics":
		value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) { return opened.Diagnose(request.Context(), board) })
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	case "workers":
		if len(segments) == 3 && segments[2] == "active" && request.Method == http.MethodGet {
			value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) { return opened.ListActiveRuns(request.Context(), board) })
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
	case "inspect":
		if request.Method == http.MethodGet {
			value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
				diagnostics, err := opened.Diagnose(request.Context(), board)
				if err != nil {
					return nil, err
				}
				events, err := opened.ListEvents(request.Context(), store.EventFilter{Limit: 100, NewestFirst: true})
				return map[string]any{"diagnostics": diagnostics, "recentEvents": events}, err
			})
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
	case "github":
		return s.handleGitHubImport(response, request, segments, board)
	}
	return s.handleExtendedAPI(response, request, segments, board)
}

func (s *Server) handleBoards(response http.ResponseWriter, request *http.Request, segments []string) error {
	ctx := request.Context()
	if len(segments) == 2 && request.Method == http.MethodGet {
		values, err := s.manager.List(ctx, request.URL.Query().Get("archived") == "true")
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, map[string]any{"current": s.manager.Current(), "boards": values})
		return nil
	}
	if len(segments) == 2 && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		update, err := boardUpdate(body)
		if err != nil {
			return err
		}
		metadata, err := s.manager.Create(ctx, stringValue(body["slug"]), update)
		if err != nil {
			return err
		}
		if boolValue(body["switch"], false) {
			metadata, err = s.manager.Switch(metadata.Slug)
			if err != nil {
				return err
			}
		}
		sendJSON(response, http.StatusCreated, metadata)
		return nil
	}
	if len(segments) >= 3 {
		slug := segments[2]
		if len(segments) == 4 && segments[3] == "switch" && request.Method == http.MethodPost {
			metadata, err := s.manager.Switch(slug)
			if err == nil {
				sendJSON(response, http.StatusOK, metadata)
			}
			return err
		}
		if request.Method == http.MethodPatch {
			body, err := readJSON(request)
			if err != nil {
				return err
			}
			update, err := boardUpdate(body)
			if err != nil {
				return err
			}
			metadata, err := s.manager.Update(slug, update)
			if err == nil {
				sendJSON(response, http.StatusOK, metadata)
			}
			return err
		}
		if request.Method == http.MethodDelete {
			value, err := s.manager.Remove(slug, request.URL.Query().Get("hard") == "true")
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}

type dashboardTask struct {
	model.Task
	SubtasksDone       int `json:"subtasksDone"`
	SubtasksTotal      int `json:"subtasksTotal"`
	CommentsCount      int `json:"commentsCount"`
	RelationshipsCount int `json:"relationshipsCount"`
}

const dashboardTaskLimit = 500

type dashboardTaskWindow struct {
	Returned  int  `json:"returned"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit"`
}

func (s *Server) handleBoardSnapshot(response http.ResponseWriter, request *http.Request, board string) error {
	if request.Method != http.MethodGet {
		return errors.New("board endpoint requires GET")
	}
	value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
		includeArchived := request.URL.Query().Get("includeArchived") == "true"
		tasks, err := opened.ListTasks(request.Context(), store.ListTaskFilter{IncludeArchived: includeArchived, Limit: dashboardTaskLimit})
		if err != nil {
			return nil, err
		}
		result := make([]dashboardTask, 0, len(tasks))
		for _, task := range tasks {
			detail, err := opened.GetTask(request.Context(), task.ID)
			if err != nil {
				return nil, err
			}
			done := 0
			for _, subtask := range detail.Subtasks {
				if subtask.Status == model.TaskStatusDone {
					done++
				}
			}
			relationships := len(detail.Parents) + len(detail.Children) + len(detail.Subtasks)
			if detail.ParentTask != nil {
				relationships++
			}
			result = append(result, dashboardTask{Task: task, SubtasksDone: done, SubtasksTotal: len(detail.Subtasks), CommentsCount: len(detail.Comments), RelationshipsCount: relationships})
		}
		boardContext, err := taskservice.New(opened, s.manager, board).BoardContext(request.Context())
		if err != nil {
			return nil, err
		}
		stats, err := opened.Stats(request.Context(), board)
		if err != nil {
			return nil, err
		}
		visibleTotal := stats.Total
		if !includeArchived {
			visibleTotal -= stats.ByStatus[model.TaskStatusArchived]
		}
		taskWindow := dashboardTaskWindow{
			Returned: len(result), Total: visibleTotal,
			Truncated: len(result) < visibleTotal, Limit: dashboardTaskLimit,
		}
		diagnostics, err := opened.Diagnose(request.Context(), board)
		return map[string]any{"board": boardContext.Metadata, "profiles": boardContext.Profiles, "tasks": result, "taskWindow": taskWindow, "stats": stats, "diagnostics": diagnostics}, err
	})
	if err == nil {
		sendJSON(response, http.StatusOK, value)
	}
	return err
}

func createTaskInput(body map[string]any, board string) store.CreateTaskInput {
	input := store.CreateTaskInput{Title: stringValue(body["title"]), Body: stringValue(body["body"]), Board: board,
		Tenant: optionalString(body, "tenant").Value, IdempotencyKey: optionalString(body, "idempotencyKey").Value,
		Assignee: optionalString(body, "assignee").Value, Runtime: runtimeValue(body["runtime"]), Priority: intValue(body["priority"], 0),
		Workspace: optionalString(body, "workspace").Value, WorkspaceKind: model.WorkspaceKind(stringValue(body["workspaceKind"])),
		Branch: optionalString(body, "branch").Value, Status: statusValue(body["status"]), ScheduledAt: optionalString(body, "scheduledAt").Value,
		MaxRuntimeSeconds: optionalInt(body, "maxRuntimeSeconds").Value, Skills: stringArray(body["skills"]), GoalMode: boolValue(body["goalMode"], false),
		GoalMaxTurns: intValue(body["goalMaxTurns"], 0), MaxRetries: intValue(body["maxRetries"], 0), Parents: stringArray(body["parents"])}
	return input
}

func requireFutureSchedule(value *string) error {
	if value == nil || strings.TrimSpace(*value) == "" {
		return errors.New("scheduled task requires scheduledAt; choose a future time")
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*value))
	if err != nil || !parsed.After(time.Now()) {
		return errors.New("scheduledAt must be a valid future ISO-8601 date")
	}
	return nil
}

func (s *Server) handleTasks(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	ctx := request.Context()
	if len(segments) == 2 {
		if request.Method == http.MethodGet {
			sort, err := requireSort(request.URL.Query().Get("sort"))
			if err != nil {
				return err
			}
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			if limit == 0 {
				limit = 500
			}
			value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
				return opened.ListTasks(ctx, store.ListTaskFilter{
					Status: statusValue(request.URL.Query().Get("status")), Tenant: request.URL.Query().Get("tenant"), Assignee: request.URL.Query().Get("assignee"),
					Runtime: runtimeValue(request.URL.Query().Get("runtime")), IncludeArchived: request.URL.Query().Get("includeArchived") == "true",
					Search: request.URL.Query().Get("search"), Sort: sort, Limit: limit,
				})
			})
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
		if request.Method == http.MethodPost {
			body, err := readJSON(request)
			if err != nil {
				return err
			}
			input := createTaskInput(body, board)
			if input.Status == model.TaskStatusScheduled {
				if err := requireFutureSchedule(input.ScheduledAt); err != nil {
					return err
				}
			}
			value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) { return opened.CreateTask(ctx, input) })
			if err == nil {
				sendJSON(response, http.StatusCreated, value)
			}
			return err
		}
	}
	if len(segments) >= 3 && segments[2] == "bulk" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		ids := stringArray(body["ids"])
		if len(ids) == 0 {
			return errors.New("bulk mutation requires ids")
		}
		mutationBody := body
		if nested, ok := body["mutation"].(map[string]any); ok {
			mutationBody = nested
		}
		expectedValue := mutationBody["expectedUpdatedAt"]
		if expectedValue == nil {
			expectedValue = body["expectedUpdatedAt"]
		}
		expectedUpdatedAt, err := stringMap(expectedValue)
		if err != nil {
			return err
		}
		mutation := store.BulkMutation{
			Archive: boolValue(mutationBody["archive"], false), Delete: boolValue(mutationBody["delete"], false),
			Assignee: optionalString(mutationBody, "assignee"), Priority: intPointerFrom(mutationBody, "priority"),
			ExpectedUpdatedAt: expectedUpdatedAt,
		}
		if status := statusValue(mutationBody["status"]); status != "" {
			mutation.Status = &status
			if status == model.TaskStatusScheduled {
				mutation.ScheduledAt = optionalString(mutationBody, "scheduledAt")
				if err := requireFutureSchedule(mutation.ScheduledAt.Value); err != nil {
					return err
				}
			} else {
				// A Web status move away from Scheduled also removes the
				// reservation. Keeping a future value would make the store's
				// queue reconciliation move the task back to Scheduled.
				mutation.ScheduledAt = store.OptionalString{Set: true}
			}
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) { return opened.BulkMutate(ctx, ids, mutation), nil })
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) < 3 {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	taskID := segments[2]
	if len(segments) == 3 && request.Method == http.MethodGet {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			detail, err := opened.GetTask(ctx, taskID)
			if err != nil {
				return nil, err
			}
			graph, err := opened.RelationshipGraph(ctx, taskID)
			if err != nil {
				return nil, err
			}
			workerContext, err := opened.BuildWorkerContext(ctx, taskID)
			return struct {
				model.TaskDetail
				RelationshipGraph model.RelationshipGraph `json:"relationshipGraph"`
				WorkerContext     string                  `json:"workerContext"`
			}{detail, graph, workerContext}, err
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 3 && request.Method == http.MethodPatch {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		status := statusValue(body["status"])
		if status != "" && status != model.TaskStatusScheduled {
			// Status and reservation are written by the same UpdateTask
			// transaction for ordinary Web board moves.
			body["scheduledAt"] = nil
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			scheduled := optionalString(body, "scheduledAt")
			if status == model.TaskStatusScheduled || scheduled.Set {
				current, err := opened.GetTask(ctx, taskID)
				if err != nil {
					return nil, err
				}
				effectiveStatus := current.Task.Status
				if status != "" {
					effectiveStatus = status
				}
				effectiveSchedule := current.Task.ScheduledAt
				if scheduled.Set {
					effectiveSchedule = scheduled.Value
				}
				if effectiveStatus == model.TaskStatusScheduled || effectiveSchedule != nil {
					if err := requireFutureSchedule(effectiveSchedule); err != nil {
						return nil, err
					}
				}
			}
			switch status {
			case model.TaskStatusDone:
				metadata, _ := body["metadata"].(map[string]any)
				return opened.CompleteTask(ctx, taskID, store.CompletionInput{
					Summary: stringValue(body["summary"]), Result: stringValue(body["result"]),
					Metadata: metadata, ExpectedUpdatedAt: stringPointerFrom(body, "expectedUpdatedAt"),
				})
			case model.TaskStatusBlocked:
				return opened.BlockTask(ctx, taskID, store.BlockInput{
					Reason: firstText(stringValue(body["reason"]), "Blocked from dashboard"), Kind: model.BlockKind(stringValue(body["kind"])),
					ExpectedUpdatedAt: stringPointerFrom(body, "expectedUpdatedAt"),
				})
			case model.TaskStatusArchived:
				return opened.ArchiveTaskWithVersion(ctx, taskID, stringPointerFrom(body, "expectedUpdatedAt"))
			default:
				return opened.UpdateTask(ctx, taskID, taskUpdate(body))
			}
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 3 && request.Method == http.MethodDelete {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		err = usingStoreError(ctx, s, board, func(opened *store.Store) error {
			return opened.DeleteTaskWithVersion(ctx, taskID, stringPointerFrom(body, "expectedUpdatedAt"))
		})
		if err == nil {
			sendJSON(response, http.StatusOK, map[string]any{"id": taskID, "deleted": true})
		}
		return err
	}
	if len(segments) == 4 && segments[3] == "graph" && request.Method == http.MethodGet {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) { return opened.RelationshipGraph(ctx, taskID) })
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	return s.handleTaskAction(response, request, segments, board, taskID)
}

func usingStoreError(ctx context.Context, s *Server, board string, handler func(*store.Store) error) error {
	_, err := usingStore(ctx, s, board, func(opened *store.Store) (bool, error) { return true, handler(opened) })
	return err
}

func firstText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *Server) handleLinks(response http.ResponseWriter, request *http.Request, board string) error {
	if request.Method != http.MethodPost && request.Method != http.MethodDelete {
		return errors.New("links require POST or DELETE")
	}
	body := map[string]any{}
	var err error
	if request.Method == http.MethodPost {
		body, err = readJSON(request)
		if err != nil {
			return err
		}
	}
	parentID := firstText(stringValue(body["parentId"]), request.URL.Query().Get("parentId"))
	childID := firstText(stringValue(body["childId"]), request.URL.Query().Get("childId"))
	value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
		if request.Method == http.MethodPost {
			return opened.LinkTasks(request.Context(), parentID, childID)
		}
		return opened.UnlinkTasks(request.Context(), parentID, childID)
	})
	if err == nil {
		sendJSON(response, http.StatusOK, value)
	}
	return err
}

func (s *Server) handleHierarchy(response http.ResponseWriter, request *http.Request, board string) error {
	if request.Method != http.MethodPost && request.Method != http.MethodDelete {
		return errors.New("hierarchy requires POST or DELETE")
	}
	body := map[string]any{}
	var err error
	if request.Method == http.MethodPost {
		body, err = readJSON(request)
		if err != nil {
			return err
		}
	}
	parentID := firstText(stringValue(body["parentTaskId"]), request.URL.Query().Get("parentTaskId"))
	subtaskID := firstText(stringValue(body["subtaskId"]), request.URL.Query().Get("subtaskId"))
	value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
		var detail model.TaskDetail
		var err error
		if request.Method == http.MethodPost {
			detail, err = opened.SetSubtaskParent(request.Context(), parentID, subtaskID, intPointerFrom(body, "position"))
		} else {
			detail, err = opened.RemoveSubtask(request.Context(), parentID, subtaskID)
		}
		if err != nil {
			return nil, err
		}
		graph, err := opened.RelationshipGraph(request.Context(), subtaskID)
		return map[string]any{"detail": detail, "graph": graph}, err
	})
	if err == nil {
		sendJSON(response, http.StatusOK, value)
	}
	return err
}

func (s *Server) handleEvents(response http.ResponseWriter, request *http.Request, board string) error {
	if request.Method != http.MethodGet {
		return errors.New("events endpoint requires GET")
	}
	since, _ := strconv.ParseInt(request.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 500
	}
	kinds := []string{}
	for _, kind := range strings.Split(request.URL.Query().Get("kinds"), ",") {
		if kind != "" {
			kinds = append(kinds, kind)
		}
	}
	value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
		return opened.ListEvents(request.Context(), store.EventFilter{TaskID: request.URL.Query().Get("taskId"), SinceID: &since, Kinds: kinds, Limit: limit})
	})
	if err == nil {
		sendJSON(response, http.StatusOK, value)
	}
	return err
}

func (s *Server) streamEvents(response http.ResponseWriter, request *http.Request) error {
	board, err := s.boardFrom(request)
	if err != nil {
		return err
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		return errors.New("streaming is unavailable")
	}
	cursor, _ := strconv.ParseInt(request.URL.Query().Get("since"), 10, 64)
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Connection", "keep-alive")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		value, err := usingStore(request.Context(), s, board, func(opened *store.Store) ([]model.TaskEvent, error) {
			return opened.ListEvents(request.Context(), store.EventFilter{SinceID: &cursor, Limit: 500})
		})
		if err != nil {
			return err
		}
		if len(value) > 0 {
			cursor = value[len(value)-1].ID
			payload, _ := json.Marshal(map[string]any{"type": "events", "board": board, "cursor": cursor, "events": value})
			if _, err := fmt.Fprintf(response, "data: %s\n\n", payload); err != nil {
				return nil
			}
			flusher.Flush()
		}
		select {
		case <-request.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}
