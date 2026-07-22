package taskservice

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/runcontrol"
	"github.com/nn1a/autogora/internal/store"
	"github.com/nn1a/autogora/internal/workspace"
)

// Service is the in-process task API shared by interactive frontends. It keeps
// the Web UI and TUI on the same board metadata, profile, orchestration, and
// lifecycle rules without requiring either frontend to call the other over HTTP.
type Service struct {
	*store.Store
	manager *boards.Manager
	board   string
}

type BoardContext struct {
	Metadata boards.Metadata              `json:"metadata"`
	Profiles []orchestration.ProfileRoute `json:"profiles"`
}

func New(opened *store.Store, manager *boards.Manager, board string) *Service {
	return &Service{Store: opened, manager: manager, board: board}
}

func (s *Service) BoardContext(ctx context.Context) (BoardContext, error) {
	metadata, err := s.manager.Read(s.board)
	if err != nil {
		return BoardContext{}, err
	}
	profiles, err := s.ProfileRoutes(ctx, metadata)
	if err != nil {
		return BoardContext{}, err
	}
	return BoardContext{Metadata: metadata, Profiles: profiles}, nil
}

// ProfileRoutes matches the Web API profile list: task-derived routes first,
// with configured board profiles overriding routes that have the same name.
func (s *Service) ProfileRoutes(ctx context.Context, metadata boards.Metadata) ([]orchestration.ProfileRoute, error) {
	tasks, err := s.ListTasks(ctx, store.ListTaskFilter{Board: s.board, IncludeArchived: true, Limit: 500})
	if err != nil {
		return nil, err
	}
	profiles := []orchestration.ProfileRoute{}
	index := map[string]int{}
	for _, task := range tasks {
		if task.Assignee == nil || task.Runtime == model.RuntimeManual {
			continue
		}
		if _, exists := index[*task.Assignee]; exists {
			continue
		}
		index[*task.Assignee] = len(profiles)
		profiles = append(profiles, orchestration.ProfileRoute{Name: *task.Assignee, Runtime: task.Runtime})
	}
	for _, profile := range metadata.Orchestration.Profiles {
		route := orchestration.ProfileRoute{Name: profile.Name, Runtime: profile.Runtime, Description: profile.Description}
		if old, exists := index[route.Name]; exists {
			profiles[old] = route
		} else {
			index[route.Name] = len(profiles)
			profiles = append(profiles, route)
		}
	}
	return profiles, nil
}

func (s *Service) planner(metadata boards.Metadata) (orchestration.Planner, error) {
	return orchestration.CreateCLIPlanner(orchestration.CLIPlannerOptions{
		Runtime: metadata.Orchestration.PlannerRuntime,
		Timeout: 120 * time.Second,
	})
}

func (s *Service) SpecifyTask(ctx context.Context, taskID string, explicit *orchestration.SpecificationPlan, author string) (model.TaskDetail, error) {
	metadata, err := s.manager.Read(s.board)
	if err != nil {
		return model.TaskDetail{}, err
	}
	planner, err := s.planner(metadata)
	if err != nil {
		return model.TaskDetail{}, err
	}
	return orchestration.SpecifyTriageTask(ctx, s.Store, taskID, planner, explicit, author)
}

func profileRoutes(values []boards.Profile) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(values))
	for _, value := range values {
		result = append(result, orchestration.ProfileRoute{Name: value.Name, Runtime: value.Runtime, Description: value.Description})
	}
	return result
}

func selectProfile(metadata boards.Metadata, profiles []orchestration.ProfileRoute) (orchestration.ProfileRoute, orchestration.ProfileRoute) {
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
	orchestrator := fallback
	for _, profile := range profiles {
		if metadata.Orchestration.OrchestratorProfile != nil && profile.Name == *metadata.Orchestration.OrchestratorProfile {
			orchestrator = profile
		}
	}
	return fallback, orchestrator
}

func (s *Service) DecomposeTask(ctx context.Context, taskID string, plan *orchestration.DecompositionPlan) (orchestration.DecompositionResult, error) {
	metadata, err := s.manager.Read(s.board)
	if err != nil {
		return orchestration.DecompositionResult{}, err
	}
	profiles := profileRoutes(metadata.Orchestration.Profiles)
	fallback, orchestrator := selectProfile(metadata, profiles)
	planner, err := s.planner(metadata)
	if err != nil {
		return orchestration.DecompositionResult{}, err
	}
	return orchestration.DecomposeTriageTask(ctx, s.Store, taskID, orchestration.DecomposeOptions{
		Profiles: profiles, DefaultProfile: fallback, OrchestratorProfile: &orchestrator,
		AutoPromoteChildren: &metadata.Orchestration.AutoPromoteChildren, Planner: planner, Plan: plan,
	})
}

func (s *Service) ClaimTaskForUser(ctx context.Context, taskID string, ttlSeconds int, workerID string) (*model.ClaimedTask, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 900
	}
	if workerID == "" {
		workerID = fmt.Sprintf("interactive-%d", os.Getpid())
	}
	claim, err := s.ClaimTask(ctx, store.ClaimOptions{TaskID: taskID, ClaimTTLSeconds: ttlSeconds, WorkerID: workerID})
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("task is not claimable: %s", taskID)
	}
	prepared, err := workspace.New(s.manager).Prepare(ctx, s.Store, claim)
	if err != nil {
		_, _ = s.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "Workspace preparation failed: "+err.Error(), store.FailRunOptions{})
		return nil, err
	}
	return prepared, nil
}

func (s *Service) TerminateRun(ctx context.Context, runID, reason string) (runcontrol.Termination, error) {
	if reason == "" {
		reason = "Run terminated from interactive UI"
	}
	return runcontrol.TerminateRun(ctx, s.Store, runID, reason)
}
