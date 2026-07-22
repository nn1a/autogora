package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/dispatcher"
	"github.com/nn1a/autogora/internal/maintenance"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/notifications"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

func (s *Server) handleTaskAction(response http.ResponseWriter, request *http.Request, segments []string, board, taskID string) error {
	if len(segments) < 4 {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	action, ctx := segments[3], request.Context()
	if action == "claim" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			claim, err := opened.ClaimTask(ctx, store.ClaimOptions{TaskID: taskID, ClaimTTLSeconds: intValue(body["ttlSeconds"], 900), WorkerID: firstText(stringValue(body["workerId"]), fmt.Sprintf("dashboard-%d", os.Getpid()))})
			if err != nil {
				return nil, err
			}
			if claim == nil {
				return nil, fmt.Errorf("task is not claimable: %s", taskID)
			}
			prepared, err := workspace.New(s.manager).Prepare(ctx, opened, claim)
			if err != nil {
				_, _ = opened.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "Workspace preparation failed: "+err.Error(), store.FailRunOptions{})
			}
			return prepared, err
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if action == "comments" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return opened.AddComment(ctx, taskID, firstText(stringValue(body["author"]), "human"), stringValue(body["body"]))
		})
		if err == nil {
			sendJSON(response, http.StatusCreated, value)
		}
		return err
	}
	if request.Method == http.MethodPost {
		switch action {
		case "complete", "block", "unblock", "promote", "schedule", "archive":
			body, err := readJSON(request)
			if err != nil {
				return err
			}
			value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
				switch action {
				case "complete":
					metadata, _ := body["metadata"].(map[string]any)
					return opened.CompleteTask(ctx, taskID, store.CompletionInput{Summary: stringValue(body["summary"]), Result: stringValue(body["result"]), Metadata: metadata})
				case "block":
					return opened.BlockTask(ctx, taskID, store.BlockInput{Reason: firstText(stringValue(body["reason"]), "Blocked from dashboard"), Kind: model.BlockKind(stringValue(body["kind"]))})
				case "unblock":
					return opened.UnblockTask(ctx, taskID)
				case "promote":
					return opened.PromoteTask(ctx, taskID)
				case "schedule":
					return opened.ScheduleTask(ctx, taskID, optionalString(body, "at").Value, stringValue(body["reason"]))
				default:
					return opened.ArchiveTask(ctx, taskID)
				}
			})
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
	}
	if action == "attachments" {
		return s.handleTaskAttachments(response, request, segments, board, taskID)
	}
	if action == "log" && request.Method == http.MethodGet {
		tail, _ := strconv.Atoi(request.URL.Query().Get("tailBytes"))
		if tail == 0 {
			tail = 65536
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return opened.ReadRunLog(ctx, taskID, tail, request.URL.Query().Get("runId"))
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if action == "specify" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		metadata, err := s.manager.Read(board)
		if err != nil {
			return err
		}
		planner, err := orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: metadata.Orchestration.PlannerRuntime, Timeout: 120 * time.Second})
		if err != nil {
			return err
		}
		var explicit *orchestration.SpecificationPlan
		if stringValue(body["title"]) != "" && stringValue(body["body"]) != "" {
			explicit = &orchestration.SpecificationPlan{Title: stringValue(body["title"]), Body: stringValue(body["body"])}
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return orchestration.SpecifyTriageTask(ctx, opened, taskID, planner, explicit, stringValue(body["author"]))
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if action == "decompose" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		metadata, err := s.manager.Read(board)
		if err != nil {
			return err
		}
		profiles := boardProfiles(metadata.Orchestration.Profiles)
		fallback := orchestration.ProfileRoute{}
		for _, profile := range profiles {
			if metadata.Orchestration.DefaultProfile != nil && profile.Name == *metadata.Orchestration.DefaultProfile {
				fallback = profile
			}
		}
		if fallback.Name == "" && len(profiles) > 0 {
			fallback = profiles[0]
		}
		if fallback.Name == "" {
			fallback = orchestration.ProfileRoute{Name: string(metadata.Orchestration.PlannerRuntime) + "-worker", Runtime: metadata.Orchestration.PlannerRuntime}
		}
		orchestratorProfile := fallback
		for _, profile := range profiles {
			if metadata.Orchestration.OrchestratorProfile != nil && profile.Name == *metadata.Orchestration.OrchestratorProfile {
				orchestratorProfile = profile
			}
		}
		planner, err := orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: metadata.Orchestration.PlannerRuntime, Timeout: 120 * time.Second})
		if err != nil {
			return err
		}
		var plan *orchestration.DecompositionPlan
		if raw, exists := body["plan"]; exists {
			plan = &orchestration.DecompositionPlan{}
			if err := decodeInto(raw, plan); err != nil {
				return err
			}
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return orchestration.DecomposeTriageTask(ctx, opened, taskID, orchestration.DecomposeOptions{Profiles: profiles, DefaultProfile: fallback, OrchestratorProfile: &orchestratorProfile, AutoPromoteChildren: &metadata.Orchestration.AutoPromoteChildren, Planner: planner, Plan: plan})
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}

func decodeInto(value any, destination any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}

func (s *Server) handleTaskAttachments(response http.ResponseWriter, request *http.Request, segments []string, board, taskID string) error {
	ctx := request.Context()
	if len(segments) == 5 && request.Method == http.MethodDelete {
		err := usingStoreError(ctx, s, board, func(opened *store.Store) error { return opened.RemoveAttachment(ctx, taskID, segments[4]) })
		if err == nil {
			sendJSON(response, http.StatusOK, map[string]any{"id": segments[4], "removed": true})
		}
		return err
	}
	if request.Method != http.MethodPost {
		return errors.New("attachments endpoint requires POST or DELETE")
	}
	if strings.Contains(request.Header.Get("Content-Type"), "application/json") {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			if rawURL := stringValue(body["url"]); rawURL != "" {
				return opened.AttachURL(ctx, taskID, rawURL, stringValue(body["name"]))
			}
			if path := stringValue(body["path"]); path != "" {
				return opened.AttachFile(ctx, taskID, path, stringValue(body["name"]))
			}
			return nil, errors.New("attachment JSON requires url or path")
		})
		if err == nil {
			sendJSON(response, http.StatusCreated, value)
		}
		return err
	}
	body, err := readBody(request, store.AttachmentMaxBytes)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp("", "autogora-upload-")
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer os.Remove(path)
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	name := firstText(request.URL.Query().Get("name"), "upload.bin")
	value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) { return opened.AttachFile(ctx, taskID, path, name) })
	if err == nil {
		sendJSON(response, http.StatusCreated, value)
	}
	return err
}

func (s *Server) handleExtendedAPI(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	ctx := request.Context()
	switch segments[1] {
	case "attachments":
		if len(segments) == 4 && segments[3] == "download" && request.Method == http.MethodGet {
			taskID := request.URL.Query().Get("taskId")
			if taskID == "" {
				return errors.New("attachment download requires taskId")
			}
			attachment, err := usingStore(ctx, s, board, func(opened *store.Store) (*model.Attachment, error) {
				detail, err := opened.GetTask(ctx, taskID)
				if err != nil {
					return nil, err
				}
				for index := range detail.Attachments {
					if detail.Attachments[index].ID == segments[2] {
						value := detail.Attachments[index]
						return &value, nil
					}
				}
				return nil, errors.New("attachment file not found")
			})
			if err != nil {
				return err
			}
			if attachment.Path == nil {
				return errors.New("attachment file not found")
			}
			file, err := os.Open(*attachment.Path)
			if err != nil {
				return errors.New("attachment file not found")
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			contentType := "application/octet-stream"
			if attachment.MediaType != nil {
				contentType = *attachment.MediaType
			} else if detected := mime.TypeByExtension(filepath.Ext(attachment.Name)); detected != "" {
				contentType = detected
			}
			response.Header().Set("Content-Type", contentType)
			response.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.QueryEscape(attachment.Name))
			http.ServeContent(response, request, attachment.Name, info.ModTime(), file)
			return nil
		}
	case "runs":
		if len(segments) >= 3 {
			if len(segments) == 3 && request.Method == http.MethodGet {
				value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) { return opened.GetRun(ctx, segments[2]) })
				if err == nil {
					sendJSON(response, http.StatusOK, value)
				}
				return err
			}
			if len(segments) == 4 && segments[3] == "terminate" && request.Method == http.MethodPost {
				body, err := readJSON(request)
				if err != nil {
					return err
				}
				value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
					return runcontrol.TerminateRun(ctx, opened, segments[2], firstText(stringValue(body["reason"]), "Run terminated from dashboard"))
				})
				if err == nil {
					sendJSON(response, http.StatusOK, value)
				}
				return err
			}
		}
	case "gc":
		if request.Method == http.MethodPost {
			body, err := readJSON(request)
			if err != nil {
				return err
			}
			value, err := maintenance.Collect(ctx, s.manager, board, maintenance.Options{EventRetentionDays: intValue(body["eventRetentionDays"], 30), LogRetentionDays: intValue(body["logRetentionDays"], 30), WorkspaceRetentionDays: intValue(body["workspaceRetentionDays"], 7)})
			if err == nil {
				sendJSON(response, http.StatusOK, value)
			}
			return err
		}
	case "notifications":
		return s.handleNotifications(response, request, segments, board)
	case "profiles":
		return s.handleProfiles(response, request, segments, board)
	case "orchestration":
		return s.handleOrchestration(response, request, segments, board)
	case "dispatch":
		if request.Method == http.MethodPost {
			body, err := readJSON(request)
			if err != nil {
				return err
			}
			go func() {
				if err := dispatcher.Run(s.ctx, dispatcher.Options{DBPath: s.options.DBPath, CLIPath: s.options.CLIPath, Board: board, Once: true, MaxWorkers: intValue(body["maxWorkers"], 2), AllowWrites: boolValue(body["allowWrites"], false)}); err != nil && s.options.OnLog != nil {
					s.options.OnLog("dashboard dispatch failed: " + err.Error())
				}
			}()
			sendJSON(response, http.StatusAccepted, map[string]any{"accepted": true, "board": board})
			return nil
		}
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}

func (s *Server) handleNotifications(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	ctx := request.Context()
	if request.Method == http.MethodGet {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return opened.ListNotificationSubscriptions(ctx, request.URL.Query().Get("taskId"))
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	if len(segments) == 3 && segments[2] == "deliver" && request.Method == http.MethodPost {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return notifications.Deliver(ctx, opened, notifications.Options{})
		})
		if err == nil {
			sendJSON(response, http.StatusOK, value)
		}
		return err
	}
	body, err := readJSON(request)
	if err != nil {
		return err
	}
	if request.Method == http.MethodPost {
		value, err := usingStore(ctx, s, board, func(opened *store.Store) (any, error) {
			return opened.SubscribeTask(ctx, store.SubscriptionInput{TaskID: stringValue(body["taskId"]), Platform: stringValue(body["platform"]), ChatID: stringValue(body["chatId"]), ThreadID: optionalString(body, "threadId").Value, UserID: optionalString(body, "userId").Value, EventKinds: stringArray(body["eventKinds"]), Secret: optionalString(body, "secret")})
		})
		if err == nil {
			sendJSON(response, http.StatusCreated, value)
		}
		return err
	}
	if request.Method == http.MethodDelete {
		removed, err := usingStore(ctx, s, board, func(opened *store.Store) (bool, error) {
			return opened.UnsubscribeTask(ctx, stringValue(body["taskId"]), stringValue(body["platform"]), stringValue(body["chatId"]), optionalString(body, "threadId").Value)
		})
		if err == nil {
			sendJSON(response, http.StatusOK, map[string]any{"unsubscribed": removed})
		}
		return err
	}
	return errors.New("notifications endpoint requires GET, POST, or DELETE")
}

func boardProfiles(values []boards.Profile) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(values))
	for _, value := range values {
		result = append(result, orchestration.ProfileRoute{Name: value.Name, Runtime: value.Runtime, Description: value.Description})
	}
	return result
}

func (s *Server) handleProfiles(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	ctx := request.Context()
	metadata, err := s.manager.Read(board)
	if err != nil {
		return err
	}
	if len(segments) == 2 && request.Method == http.MethodGet {
		tasks, err := usingStore(ctx, s, board, func(opened *store.Store) ([]model.Task, error) {
			return opened.ListTasks(ctx, store.ListTaskFilter{IncludeArchived: true, Limit: 500})
		})
		if err != nil {
			return err
		}
		profiles := []orchestration.ProfileRoute{}
		index := map[string]int{}
		for _, task := range tasks {
			if task.Assignee != nil && task.Runtime != model.RuntimeManual {
				if _, ok := index[*task.Assignee]; !ok {
					index[*task.Assignee] = len(profiles)
					profiles = append(profiles, orchestration.ProfileRoute{Name: *task.Assignee, Runtime: task.Runtime})
				}
			}
		}
		for _, profile := range boardProfiles(metadata.Orchestration.Profiles) {
			if old, ok := index[profile.Name]; ok {
				profiles[old] = profile
			} else {
				index[profile.Name] = len(profiles)
				profiles = append(profiles, profile)
			}
		}
		sendJSON(response, http.StatusOK, profiles)
		return nil
	}
	if len(segments) == 4 && segments[3] == "describe-auto" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		name := segments[2]
		runtime := runtimeValue(body["runtime"])
		existingDescription := ""
		for _, profile := range metadata.Orchestration.Profiles {
			if profile.Name == name {
				runtime, existingDescription = profile.Runtime, profile.Description
			}
		}
		if runtime == "" || runtime == model.RuntimeManual {
			return errors.New("profile auto-description requires a worker runtime")
		}
		tasks, err := usingStore(ctx, s, board, func(opened *store.Store) ([]model.Task, error) {
			return opened.ListTasks(ctx, store.ListTaskFilter{Assignee: name, IncludeArchived: true, Limit: 50})
		})
		if err != nil {
			return err
		}
		evidence := make([]orchestration.ProfileEvidence, 0, len(tasks))
		for _, task := range tasks {
			evidence = append(evidence, orchestration.ProfileEvidence{Title: task.Title, Body: task.Body, Skills: task.Skills})
		}
		planner, err := orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{Runtime: metadata.Orchestration.PlannerRuntime, Timeout: 120 * time.Second})
		if err != nil {
			return err
		}
		described, err := orchestration.DescribeProfileRoute(ctx, orchestration.ProfileRoute{Name: name, Runtime: runtime, Description: existingDescription}, evidence, planner)
		if err != nil {
			return err
		}
		profiles := make([]boards.Profile, 0, len(metadata.Orchestration.Profiles)+1)
		for _, profile := range metadata.Orchestration.Profiles {
			if profile.Name != name {
				profiles = append(profiles, profile)
			}
		}
		profiles = append(profiles, boards.Profile{Name: described.Name, Runtime: described.Runtime, Description: described.Description})
		if _, err := s.manager.Update(board, boards.Update{Orchestration: &boards.OrchestrationUpdate{Profiles: &profiles}}); err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, described)
		return nil
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}

func profileRoute(value any) (orchestration.ProfileRoute, error) {
	record, ok := value.(map[string]any)
	if !ok {
		return orchestration.ProfileRoute{}, errors.New("profile route must be an object")
	}
	route := orchestration.ProfileRoute{Name: strings.TrimSpace(stringValue(record["name"])), Runtime: runtimeValue(record["runtime"]), Description: stringValue(record["description"])}
	if route.Name == "" || route.Runtime == "" || route.Runtime == model.RuntimeManual {
		return orchestration.ProfileRoute{}, errors.New("profile route requires name and a worker runtime")
	}
	return route, nil
}

func (s *Server) handleOrchestration(response http.ResponseWriter, request *http.Request, segments []string, board string) error {
	if len(segments) == 2 && request.Method == http.MethodGet {
		metadata, err := s.manager.Read(board)
		if err == nil {
			sendJSON(response, http.StatusOK, metadata.Orchestration)
		}
		return err
	}
	if len(segments) == 2 && request.Method == http.MethodPut {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		update, err := orchestrationUpdate(body)
		if err != nil {
			return err
		}
		metadata, err := s.manager.Update(board, boards.Update{Orchestration: update})
		if err == nil {
			sendJSON(response, http.StatusOK, metadata.Orchestration)
		}
		return err
	}
	if len(segments) == 3 && segments[2] == "swarm" && request.Method == http.MethodPost {
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		rawWorkers, _ := body["workers"].([]any)
		workers := make([]store.SwarmRoute, 0, len(rawWorkers))
		for _, raw := range rawWorkers {
			route, err := profileRoute(raw)
			if err != nil {
				return err
			}
			workers = append(workers, store.SwarmRoute{Assignee: route.Name, Runtime: route.Runtime})
		}
		verifier, err := profileRoute(body["verifier"])
		if err != nil {
			return err
		}
		synthesizer, err := profileRoute(body["synthesizer"])
		if err != nil {
			return err
		}
		blackboard, _ := body["blackboard"].(map[string]any)
		value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (any, error) {
			return opened.CreateSwarm(request.Context(), store.SwarmInput{Goal: stringValue(body["goal"]), Workers: workers, Verifier: store.SwarmRoute{Assignee: verifier.Name, Runtime: verifier.Runtime}, Synthesizer: store.SwarmRoute{Assignee: synthesizer.Name, Runtime: synthesizer.Runtime}, Tenant: optionalString(body, "tenant").Value, Workspace: optionalString(body, "workspace").Value, WorkspaceKind: model.WorkspaceKind(stringValue(body["workspaceKind"])), Blackboard: blackboard})
		})
		if err == nil {
			sendJSON(response, http.StatusCreated, value)
		}
		return err
	}
	sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
	return nil
}
