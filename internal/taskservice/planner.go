package taskservice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/agentcapacity"
	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
	"github.com/nn1a/autogora/internal/store"
)

const (
	interactivePlannerTimeout = 120 * time.Second
	plannerRateLimitCooldown  = time.Minute
	plannerRetryCooldown      = 5 * time.Minute
)

func (s *Service) plannerForRole(metadata boards.Metadata, role agentconfig.Role) (orchestration.Planner, error) {
	config, err := agentconfig.Load(agentconfig.Options{})
	if err != nil {
		return nil, fmt.Errorf("load global agent configuration: %w", err)
	}
	candidates := plannerCandidates(metadata, config, role)
	options := orchestration.FallbackPlannerOptions{
		Candidates: candidates,
		Timeout:    interactivePlannerTimeout,
		Available: func(ctx context.Context, candidate orchestration.PlannerCandidate) (bool, error) {
			if s == nil || s.Store == nil || !strings.HasPrefix(candidate.Source, "global_") {
				return true, nil
			}
			health, err := s.GetAgentHealth(ctx, candidate.Profile)
			if err != nil {
				return false, err
			}
			return !store.IsAgentUnavailable(health, time.Now()), nil
		},
		OnFailure: func(ctx context.Context, attempt orchestration.PlannerAttempt) error {
			if s == nil || s.Store == nil || !strings.HasPrefix(attempt.Candidate.Source, "global_") {
				return nil
			}
			status := plannerFailureHealth(attempt.FailureKind)
			cooldown := plannerRetryCooldown
			if status == model.AgentHealthRateLimited {
				cooldown = plannerRateLimitCooldown
			}
			until := time.Now().Add(cooldown).UTC().Format(time.RFC3339Nano)
			message := attempt.Err.Error()
			_, err := s.SetAgentHealth(ctx, store.SetAgentHealthInput{
				AgentID: attempt.Candidate.Profile, Status: status, CooldownUntil: &until, LastError: &message,
			})
			return err
		},
		OnSelected: func(ctx context.Context, selection orchestration.PlannerSelection) error {
			if s == nil || s.Store == nil {
				return nil
			}
			if strings.HasPrefix(selection.Candidate.Source, "global_") {
				if _, err := s.SetAgentHealth(ctx, store.SetAgentHealthInput{
					AgentID: selection.Candidate.Profile, Status: model.AgentHealthReady,
				}); err != nil {
					return err
				}
			}
			if strings.TrimSpace(selection.Request.TaskID) == "" {
				return nil
			}
			return s.RecordOrchestrationAgentSelection(ctx, selection.Request.TaskID, store.RecordOrchestrationAgentSelectionInput{
				Kind: string(selection.Request.Kind), Role: string(role), Profile: selection.Candidate.Profile,
				Runtime: selection.Candidate.Runtime, Model: selection.Candidate.Model, Provider: selection.Candidate.Provider,
				Source: selection.Candidate.Source, FallbackFrom: selection.FallbackFrom, Attempt: selection.Attempt,
			})
		},
	}
	if s != nil && s.manager != nil {
		capacity := agentcapacity.New(s.manager)
		board := strings.TrimSpace(s.board)
		if board == "" {
			board = strings.TrimSpace(metadata.Slug)
		}
		if board == "" && s.Store != nil {
			board = s.Store.Board()
		}
		ownerKind := store.AgentSlotOwnerPlanner
		switch role {
		case agentconfig.RoleCoordinator:
			ownerKind = store.AgentSlotOwnerCoordinator
		case agentconfig.RoleJudge:
			ownerKind = store.AgentSlotOwnerJudge
		}
		options.AcquireAttempt = func(ctx context.Context, _ orchestration.PlannerRequest, candidate orchestration.PlannerCandidate) (orchestration.PlannerAttemptHandle, bool, error) {
			if !strings.HasPrefix(candidate.Source, "global_") {
				return nil, true, nil
			}
			return capacity.AcquireEphemeral(ctx, candidate.Profile, candidate.MaxConcurrent, ownerKind, board, interactivePlannerTimeout+agentcapacity.EphemeralSlotCleanupGrace)
		}
		options.ReleaseAttempt = func(ctx context.Context, handle orchestration.PlannerAttemptHandle) error {
			if handle == nil {
				return nil
			}
			lease, ok := handle.(*agentcapacity.Lease)
			if !ok {
				return fmt.Errorf("unexpected planner capacity handle %T", handle)
			}
			return lease.Release(ctx)
		}
	}
	return orchestration.CreateFallbackPlanner(options)
}

func plannerCandidates(metadata boards.Metadata, config agentconfig.Config, role agentconfig.Role) []orchestration.PlannerCandidate {
	if role == agentconfig.RoleCoordinator {
		config = configWithCoordinatorProfile(config, metadata.Orchestration.Autopilot.Coordination.Profile)
		return orchestration.GlobalPlannerCandidates(config, agentconfig.RoleCoordinator)
	}
	if role == agentconfig.RoleJudge {
		if candidates := orchestration.GlobalPlannerCandidates(config, agentconfig.RoleJudge); len(candidates) > 0 {
			return candidates
		}
	}
	modelName := strings.TrimSpace(metadata.Orchestration.PlannerModel)
	provider := strings.TrimSpace(metadata.Orchestration.PlannerProvider)
	if modelName == "" && provider == "" {
		if candidates := orchestration.GlobalPlannerCandidates(config, agentconfig.RolePlanner); len(candidates) > 0 {
			return candidates
		}
	}
	profile := "board-planner"
	if role == agentconfig.RoleJudge {
		profile = "board-judge"
	}
	return []orchestration.PlannerCandidate{{
		Profile: profile, Runtime: metadata.Orchestration.PlannerRuntime, Model: modelName,
		Provider: provider, Source: "board",
	}}
}

func configWithCoordinatorProfile(config agentconfig.Config, profile *string) agentconfig.Config {
	if profile == nil || strings.TrimSpace(*profile) == "" {
		return config
	}
	cloned := config
	cloned.Defaults = config.Defaults
	cloned.Defaults.CoordinatorAgents = []string{strings.TrimSpace(*profile)}
	return cloned
}

func plannerFailureHealth(kind orchestration.PlannerFailureKind) model.AgentHealthStatus {
	switch kind {
	case orchestration.PlannerFailureSpawn:
		return model.AgentHealthMissing
	case orchestration.PlannerFailureAuth:
		return model.AgentHealthAuthRequired
	case orchestration.PlannerFailureRateLimited:
		return model.AgentHealthRateLimited
	default:
		return model.AgentHealthUnhealthy
	}
}
