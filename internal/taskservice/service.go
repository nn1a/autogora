package taskservice

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nn1a/autogora/internal/agentconfig"
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
	manager      *boards.Manager
	board        string
	dispatchTask func(context.Context, string) error
}

type BoardContext struct {
	Metadata boards.Metadata              `json:"metadata"`
	Profiles []orchestration.ProfileRoute `json:"profiles"`
}

func New(opened *store.Store, manager *boards.Manager, board string) *Service {
	return &Service{Store: opened, manager: manager, board: board}
}

func (s *Service) WithTaskDispatcher(dispatch func(context.Context, string) error) *Service {
	s.dispatchTask = dispatch
	return s
}

func (s *Service) DispatchTask(ctx context.Context, taskID string) error {
	if s.dispatchTask == nil {
		return errors.New("interactive task dispatch is not configured")
	}
	return s.dispatchTask(ctx, taskID)
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
// followed by global worker agents and board-local profiles. A board profile
// may specialize a global agent without changing its runtime or relaxing its
// availability and concurrency limits.
func (s *Service) ProfileRoutes(ctx context.Context, metadata boards.Metadata) ([]orchestration.ProfileRoute, error) {
	tasks, err := s.ListTasks(ctx, store.ListTaskFilter{Board: s.board, IncludeArchived: true, Limit: 500})
	if err != nil {
		return nil, err
	}
	config, err := agentconfig.Load(agentconfig.Options{})
	if err != nil {
		return nil, fmt.Errorf("load global agent configuration: %w", err)
	}
	configured := mergeProfileRoutes(globalWorkerProfileRoutes(config), profileRoutes(metadata.Orchestration.Profiles))
	return orchestration.ResolveProfileRoutes(tasks, configured), nil
}

func (s *Service) planner(metadata boards.Metadata) (orchestration.Planner, error) {
	return s.plannerForRole(metadata, agentconfig.RolePlanner)
}

func (s *Service) SpecifyTask(ctx context.Context, taskID string, explicit *orchestration.SpecificationPlan, author string) (model.TaskDetail, error) {
	return s.SpecifyTaskWithVersion(ctx, taskID, explicit, author, nil)
}

func (s *Service) SpecifyTaskWithVersion(ctx context.Context, taskID string, explicit *orchestration.SpecificationPlan, author string, expectedUpdatedAt *string) (model.TaskDetail, error) {
	metadata, err := s.manager.Read(s.board)
	if err != nil {
		return model.TaskDetail{}, err
	}
	planner, err := s.planner(metadata)
	if err != nil {
		return model.TaskDetail{}, err
	}
	return orchestration.SpecifyTriageTaskWithVersion(ctx, s.Store, taskID, planner, explicit, author, expectedUpdatedAt)
}

func profileRoutes(values []boards.Profile) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(values))
	for _, value := range values {
		result = append(result, orchestration.ProfileRoute{Name: value.Name, Runtime: value.Runtime, Model: value.Model, Provider: value.Provider,
			Description: value.Description, Disabled: value.Disabled, MaxConcurrent: value.MaxConcurrent, Priority: value.Priority,
			Fallbacks: append([]string{}, value.Fallbacks...)})
	}
	return result
}

func globalWorkerProfileRoutes(config agentconfig.Config) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, 0, len(config.Agents))
	seen := map[string]bool{}
	appendAgent := func(agent agentconfig.Agent) {
		if seen[agent.ID] || !agentSupportsRole(agent, agentconfig.RoleWorker) {
			return
		}
		seen[agent.ID] = true
		result = append(result, orchestration.ProfileRoute{
			Name: agent.ID, Runtime: agent.Runtime, Model: agent.Model, Provider: agent.Provider,
			Disabled: !agent.Enabled, MaxConcurrent: agent.MaxConcurrent, Fallbacks: append([]string{}, agent.Fallbacks...),
		})
	}
	for _, id := range config.Defaults.WorkerAgents {
		if agent, found := config.Find(id); found {
			appendAgent(agent)
		}
	}
	for _, agent := range config.Agents {
		if !agentSupportsRole(agent, agentconfig.RoleWorker) {
			continue
		}
		appendAgent(agent)
	}
	return result
}

func agentSupportsRole(agent agentconfig.Agent, role agentconfig.Role) bool {
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

func firstGlobalAgent(config agentconfig.Config, ids []string, role agentconfig.Role) (agentconfig.Agent, bool) {
	for _, id := range ids {
		agent, found := config.Find(id)
		if found && agent.Enabled && agentSupportsRole(agent, role) {
			return agent, true
		}
	}
	return agentconfig.Agent{}, false
}

func mergeProfileRoutes(global, board []orchestration.ProfileRoute) []orchestration.ProfileRoute {
	result := make([]orchestration.ProfileRoute, len(global))
	copy(result, global)
	index := make(map[string]int, len(global)+len(board))
	for position, profile := range result {
		index[profile.Name] = position
	}
	for _, profile := range board {
		position, found := index[profile.Name]
		if !found {
			index[profile.Name] = len(result)
			result = append(result, profile)
			continue
		}
		registered := result[position]
		if profile.Model != "" {
			registered.Model = profile.Model
		}
		if profile.Provider != "" {
			registered.Provider = profile.Provider
		}
		if profile.Description != "" {
			registered.Description = profile.Description
		}
		registered.Priority = profile.Priority
		if len(profile.Fallbacks) > 0 {
			registered.Fallbacks = append([]string{}, profile.Fallbacks...)
		}
		registered.Disabled = registered.Disabled || profile.Disabled
		registered.MaxConcurrent = concurrencyLimit(registered.MaxConcurrent, profile.MaxConcurrent)
		result[position] = registered
	}
	return result
}

func concurrencyLimit(global, board int) int {
	if global > 0 && (board <= 0 || global < board) {
		return global
	}
	if board > 0 {
		return board
	}
	return 0
}

func (s *Service) DecomposeTask(ctx context.Context, taskID string, plan *orchestration.DecompositionPlan) (orchestration.DecompositionResult, error) {
	return s.DecomposeTaskWithVersion(ctx, taskID, plan, nil)
}

func (s *Service) DecomposeTaskWithVersion(ctx context.Context, taskID string, plan *orchestration.DecompositionPlan, expectedUpdatedAt *string) (orchestration.DecompositionResult, error) {
	metadata, err := s.manager.Read(s.board)
	if err != nil {
		return orchestration.DecompositionResult{}, err
	}
	profiles, err := s.ProfileRoutes(ctx, metadata)
	if err != nil {
		return orchestration.DecompositionResult{}, err
	}
	defaultProfile := metadata.Orchestration.DefaultProfile
	if defaultProfile == nil {
		config, configErr := agentconfig.Load(agentconfig.Options{})
		if configErr != nil {
			return orchestration.DecompositionResult{}, fmt.Errorf("load global agent configuration: %w", configErr)
		}
		if agent, found := firstGlobalAgent(config, config.Defaults.WorkerAgents, agentconfig.RoleWorker); found {
			value := agent.ID
			defaultProfile = &value
		}
	}
	fallback, finalizer := orchestration.SelectProfileRoutes(profiles, defaultProfile, metadata.Orchestration.FinalizerProfile, metadata.Orchestration.PlannerRuntime)
	planner, err := s.planner(metadata)
	if err != nil {
		return orchestration.DecompositionResult{}, err
	}
	return orchestration.DecomposeTriageTask(ctx, s.Store, taskID, orchestration.DecomposeOptions{
		Profiles: profiles, DefaultProfile: fallback, FinalizerProfile: &finalizer,
		AutoPromoteChildren: &metadata.Orchestration.AutoPromoteChildren, Planner: planner, Plan: plan,
		ExpectedUpdatedAt: expectedUpdatedAt,
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
	workspaces := workspace.New(s.manager)
	prepared, err := workspaces.Prepare(ctx, s.Store, claim)
	if err != nil {
		_, _ = s.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "Workspace preparation failed: "+err.Error(), store.FailRunOptions{})
		return nil, err
	}
	if _, err := workspaces.IntegratePrerequisiteChangeSets(ctx, s.Store, prepared); err != nil {
		var integrationErr *workspace.PrerequisiteIntegrationError
		if errors.As(err, &integrationErr) {
			_, blockErr := s.BlockRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, store.BlockInput{
				Reason: integrationErr.Reason, Kind: integrationErr.BlockKind,
			})
			return nil, errors.Join(err, blockErr)
		}
		countFailure := false
		_, failErr := s.FailRun(ctx, store.RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "Prerequisite integration failed: "+err.Error(), store.FailRunOptions{
			Outcome: model.RunStatusReclaimed, CountFailure: &countFailure,
		})
		return nil, errors.Join(err, failErr)
	}
	return prepared, nil
}

func (s *Service) TerminateRun(ctx context.Context, runID, reason string) (runcontrol.Termination, error) {
	if reason == "" {
		reason = "Run terminated from interactive UI"
	}
	return runcontrol.TerminateRun(ctx, s.Store, runID, reason)
}
