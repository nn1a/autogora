package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/taskservice"
)

type agentConfigResponse struct {
	Path   string             `json:"path"`
	Exists bool               `json:"exists"`
	Config agentconfig.Config `json:"config"`
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
	options := agentconfig.Options{}
	path, err := agentconfig.Path(options)
	if err != nil {
		return agentConfigResponse{}, err
	}
	exists, err := agentconfig.Exists(options)
	if err != nil {
		return agentConfigResponse{}, err
	}
	config, err := agentconfig.Load(options)
	if err != nil {
		return agentConfigResponse{}, err
	}
	return agentConfigResponse{Path: path, Exists: exists, Config: config}, nil
}

func decodeAgentConfig(request *http.Request) (agentconfig.Config, error) {
	body, err := readBody(request, 1024*1024)
	if err != nil {
		return agentconfig.Config{}, err
	}
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
		config, err := decodeAgentConfig(request)
		if err != nil {
			return err
		}
		if err := agentconfig.Save(agentconfig.Options{}, config); err != nil {
			return err
		}
		value, err := loadAgentConfigResponse()
		if err != nil {
			return err
		}
		sendJSON(response, http.StatusOK, value)
		return nil
	default:
		response.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "config endpoint requires GET or PUT"})
		return nil
	}
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
		for _, profile := range boardContext.Profiles {
			health, err := opened.GetAgentHealth(request.Context(), profile.Name)
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
