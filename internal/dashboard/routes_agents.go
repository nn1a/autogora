package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/agenthealth"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

type agentConfigResponse struct {
	Path     string               `json:"path"`
	Exists   bool                 `json:"exists"`
	Revision agentconfig.Revision `json:"revision"`
	Config   agentconfig.Config   `json:"config"`
}

type agentDetectionResponse struct {
	Path   string                  `json:"path"`
	Exists bool                    `json:"exists"`
	Agents []agentconfig.Detection `json:"agents"`
}

type agentPresetPreviewResponse struct {
	Preset     agentconfig.Preset      `json:"preset"`
	Detections []agentconfig.Detection `json:"detections"`
	Config     agentconfig.Config      `json:"config"`
}

type effectiveAgentProfile struct {
	orchestration.ProfileRoute
	Health     model.AgentHealth `json:"health"`
	ActiveRuns int               `json:"activeRuns"`
}

type effectiveAgentsResponse struct {
	Config   agentconfig.Config      `json:"config"`
	Metadata boards.Metadata         `json:"metadata"`
	Profiles []effectiveAgentProfile `json:"profiles"`
}

func loadAgentConfigResponse() (agentConfigResponse, error) {
	snapshot, err := agentconfig.LoadSnapshot(agentconfig.Options{})
	if err != nil {
		return agentConfigResponse{}, err
	}
	return agentConfigResponse{
		Path: snapshot.Path, Exists: snapshot.Exists,
		Revision: snapshot.Revision, Config: snapshot.Config,
	}, nil
}

func decodeAgentConfig(request *http.Request) (agentconfig.Config, error) {
	body, err := readBody(request, 1024*1024)
	if err != nil {
		return agentconfig.Config{}, err
	}
	return decodeAgentConfigBytes(body)
}

func decodeAgentConfigValue(value any) (agentconfig.Config, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return agentconfig.Config{}, fmt.Errorf("invalid agent configuration: %w", err)
	}
	return decodeAgentConfigBytes(body)
}

func decodeAgentConfigBytes(body []byte) (agentconfig.Config, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return agentconfig.Config{}, errors.New("invalid agent configuration: expected a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var config agentconfig.Config
	if err := decoder.Decode(&config); err != nil {
		return agentconfig.Config{}, fmt.Errorf("invalid agent configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return agentconfig.Config{}, fmt.Errorf("invalid agent configuration: %w", err)
	}
	config = agentconfig.Normalize(config)
	if err := agentconfig.Validate(config); err != nil {
		return agentconfig.Config{}, fmt.Errorf("invalid agent configuration: %w", err)
	}
	return config, nil
}

func (s *Server) handleAgentConfig(response http.ResponseWriter, request *http.Request, segments []string) error {
	if len(segments) != 2 {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	switch request.Method {
	case http.MethodGet:
		value, err := loadAgentConfigResponse()
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, value)
		return nil
	case http.MethodPut:
		expected := agentconfig.Revision(strings.TrimSpace(request.Header.Get("If-Match")))
		if expected == "" {
			return errors.New("agent configuration update requires an If-Match revision")
		}
		config, err := decodeAgentConfig(request)
		if err != nil {
			return err
		}
		desired := s.supervisor.Status().Desired
		switch strings.ToLower(strings.TrimSpace(request.Header.Get("X-Autogora-Supervisor-Desired"))) {
		case "":
		case "start":
			desired = true
		default:
			return errors.New("invalid X-Autogora-Supervisor-Desired value")
		}
		snapshot, err := agentconfig.CompareAndSwap(agentconfig.Options{}, expected, config)
		if err != nil {
			return err
		}
		reconcile, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		defer cancel()
		if err := s.supervisor.Reconcile(reconcile, s.ctx, snapshot.Config, desired); err != nil {
			return fmt.Errorf("apply supervisor configuration: %w", err)
		}
		sendJSON(response, http.StatusOK, agentConfigResponse{
			Path: snapshot.Path, Exists: snapshot.Exists,
			Revision: snapshot.Revision, Config: snapshot.Config,
		})
		return nil
	default:
		response.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "config endpoint requires GET or PUT"})
		return nil
	}
}

func (s *Server) handleDetectAgents(response http.ResponseWriter, request *http.Request) error {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "agent detection endpoint requires POST"})
		return nil
	}
	configResponse, err := loadAgentConfigResponse()
	if err != nil {
		return err
	}
	detections, err := agentconfig.DetectSupportedAgents(request.Context(), configResponse.Config, s.options.AgentDetection)
	if err != nil {
		return err
	}
	sendJSON(response, http.StatusOK, agentDetectionResponse{
		Path: configResponse.Path, Exists: configResponse.Exists, Agents: detections,
	})
	return nil
}

func (s *Server) handleAgentPresets(response http.ResponseWriter, request *http.Request) error {
	switch request.Method {
	case http.MethodGet:
		sendJSON(response, http.StatusOK, map[string]any{"presets": agentconfig.BuiltinPresets()})
		return nil
	case http.MethodPost:
		body, err := readJSON(request)
		if err != nil {
			return err
		}
		id := stringValue(body["id"])
		preset, found := agentconfig.FindPreset(id)
		if !found {
			return fmt.Errorf("invalid agent preset %q", id)
		}
		current, err := loadAgentConfigResponse()
		if err != nil {
			return err
		}
		if value, supplied := body["config"]; supplied {
			current.Config, err = decodeAgentConfigValue(value)
			if err != nil {
				return err
			}
		}
		detections, err := agentconfig.DetectSupportedAgents(request.Context(), current.Config, s.options.AgentDetection)
		if err != nil {
			return err
		}
		config, err := agentconfig.ApplyPreset(current.Config, preset.ID, agentconfig.PresetApplyOptions{
			Detections: detections, ReplaceExisting: boolValue(body["replace"], false),
		})
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, agentPresetPreviewResponse{
			Preset: preset, Detections: detections, Config: config,
		})
		return nil
	default:
		response.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "agent presets endpoint requires GET or POST"})
		return nil
	}
}

func (s *Server) handleSupervisor(response http.ResponseWriter, request *http.Request, segments []string) error {
	if len(segments) == 2 && request.Method == http.MethodGet {
		sendJSON(response, http.StatusOK, s.supervisor.Status())
		return nil
	}
	if len(segments) != 3 || request.Method != http.MethodPost {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	switch segments[2] {
	case "start":
		config, err := agentconfig.Load(agentconfig.Options{})
		if err != nil {
			return err
		}
		s.supervisor.Start(s.ctx, config)
	case "stop":
		stop, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		defer cancel()
		if err := s.supervisor.Stop(stop); err != nil {
			return err
		}
	default:
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return nil
	}
	sendJSON(response, http.StatusOK, s.supervisor.Status())
	return nil
}

func (s *Server) handleEffectiveAgents(response http.ResponseWriter, request *http.Request) error {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "effective agents endpoint requires GET"})
		return nil
	}
	board, err := s.boardFrom(request)
	if err != nil {
		return err
	}
	config, err := agentconfig.Load(agentconfig.Options{})
	if err != nil {
		return err
	}
	value, err := usingStore(request.Context(), s, board, func(opened *store.Store) (effectiveAgentsResponse, error) {
		boardContext, err := taskservice.New(opened, s.manager, board).BoardContext(request.Context())
		if err != nil {
			return effectiveAgentsResponse{}, err
		}
		profiles := make([]effectiveAgentProfile, 0, len(boardContext.Profiles))
		healthRouter := agenthealth.New(s.manager, opened)
		if opened.Board() != "default" {
			coordinationStore, err := s.manager.OpenCoordinationStore(request.Context())
			if err != nil {
				return effectiveAgentsResponse{}, err
			}
			defer coordinationStore.Close()
			healthRouter = agenthealth.NewWithGlobal(s.manager, opened, coordinationStore)
		}
		for _, profile := range boardContext.Profiles {
			health, err := healthRouter.Get(
				request.Context(), profile.Name,
				configuredAgentSupportsRole(config, profile.Name, agentconfig.RoleWorker),
			)
			if err != nil {
				return effectiveAgentsResponse{}, err
			}
			activeRuns, err := opened.CountActiveAgentRuns(request.Context(), profile.Name)
			if err != nil {
				return effectiveAgentsResponse{}, err
			}
			profiles = append(profiles, effectiveAgentProfile{ProfileRoute: profile, Health: health, ActiveRuns: activeRuns})
		}
		return effectiveAgentsResponse{Config: config, Metadata: boardContext.Metadata, Profiles: profiles}, nil
	})
	if err != nil {
		return err
	}
	sendJSON(response, http.StatusOK, value)
	return nil
}

func configuredAgentSupportsRole(config agentconfig.Config, name string, role agentconfig.Role) bool {
	agent, found := config.Find(name)
	if !found {
		return false
	}
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}
