package dispatcher

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

func createRolePlanner(manager *boards.Manager, opened *store.Store, metadata boards.Metadata, configured configuredProfileSet, options Options, role agentconfig.Role, cwd string) (orchestration.Planner, error) {
	candidates := dispatcherPlannerCandidates(metadata, configured, options, role)
	plannerOptions := orchestration.FallbackPlannerOptions{
		Candidates: candidates, CWD: cwd, Timeout: options.PlannerTimeout, Getenv: options.Getenv,
		Available: func(ctx context.Context, candidate orchestration.PlannerCandidate) (bool, error) {
			if !strings.HasPrefix(candidate.Source, "global_") {
				return true, nil
			}
			health, err := opened.GetAgentHealth(ctx, candidate.Profile)
			if err != nil {
				return false, err
			}
			return !store.IsAgentUnavailable(health, time.Now()), nil
		},
		OnFailure: func(ctx context.Context, attempt orchestration.PlannerAttempt) error {
			if !strings.HasPrefix(attempt.Candidate.Source, "global_") {
				return nil
			}
			status := dispatcherPlannerFailureHealth(attempt.FailureKind)
			rateLimit, retry := time.Minute, 5*time.Minute
			if options.RateLimitCooldown != nil {
				rateLimit = *options.RateLimitCooldown
			}
			if options.AgentRetryCooldown != nil {
				retry = *options.AgentRetryCooldown
			}
			until := agentCooldown(status, rateLimit, retry)
			message := attempt.Err.Error()
			_, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
				AgentID: attempt.Candidate.Profile, Status: status, CooldownUntil: until, LastError: &message,
			})
			return err
		},
		OnSelected: func(ctx context.Context, selection orchestration.PlannerSelection) error {
			if strings.HasPrefix(selection.Candidate.Source, "global_") {
				if _, err := opened.SetAgentHealth(ctx, store.SetAgentHealthInput{
					AgentID: selection.Candidate.Profile, Status: model.AgentHealthReady,
				}); err != nil {
					return err
				}
			}
			if strings.TrimSpace(selection.Request.TaskID) == "" {
				return nil
			}
			return opened.RecordOrchestrationAgentSelection(ctx, selection.Request.TaskID, store.RecordOrchestrationAgentSelectionInput{
				Kind: string(selection.Request.Kind), Role: string(role), Profile: selection.Candidate.Profile,
				Runtime: selection.Candidate.Runtime, Model: selection.Candidate.Model, Provider: selection.Candidate.Provider,
				Source: selection.Candidate.Source, FallbackFrom: selection.FallbackFrom, Attempt: selection.Attempt,
			})
		},
	}
	if manager != nil {
		capacity := agentcapacity.New(manager)
		ownerKind := store.AgentSlotOwnerPlanner
		if role == agentconfig.RoleJudge {
			ownerKind = store.AgentSlotOwnerJudge
		}
		plannerOptions.AcquireAttempt = func(ctx context.Context, _ orchestration.PlannerRequest, candidate orchestration.PlannerCandidate) (orchestration.PlannerAttemptHandle, bool, error) {
			if !strings.HasPrefix(candidate.Source, "global_") {
				return nil, true, nil
			}
			return capacity.AcquireEphemeral(ctx, candidate.Profile, candidate.MaxConcurrent, ownerKind, metadata.Slug, options.PlannerTimeout+agentcapacity.EphemeralSlotCleanupGrace)
		}
		plannerOptions.ReleaseAttempt = func(ctx context.Context, handle orchestration.PlannerAttemptHandle) error {
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
	return orchestration.CreateFallbackPlanner(plannerOptions)
}

func dispatcherPlannerCandidates(metadata boards.Metadata, configured configuredProfileSet, options Options, role agentconfig.Role) []orchestration.PlannerCandidate {
	if role == agentconfig.RoleJudge {
		if candidates := orchestration.GlobalPlannerCandidates(configured.Config, agentconfig.RoleJudge); len(candidates) > 0 {
			return candidates
		}
	}
	if options.PlannerRuntime == "" && strings.TrimSpace(metadata.Orchestration.PlannerModel) == "" && strings.TrimSpace(metadata.Orchestration.PlannerProvider) == "" {
		if candidates := orchestration.GlobalPlannerCandidates(configured.Config, agentconfig.RolePlanner); len(candidates) > 0 {
			if modelName := strings.TrimSpace(options.PlannerModel); modelName != "" {
				candidates[0].Model = modelName
			}
			if provider := strings.TrimSpace(options.PlannerProvider); provider != "" {
				candidates[0].Provider = provider
			}
			return candidates
		}
	}
	runtime, modelName, provider, command := plannerConfiguration(metadata, configured, options)
	profile, source := "board-planner", "board"
	if role == agentconfig.RoleJudge {
		profile = "board-judge"
	}
	if options.PlannerRuntime != "" {
		profile, source = "cli-planner", "cli_override"
		if role == agentconfig.RoleJudge {
			profile = "cli-judge"
		}
	}
	return []orchestration.PlannerCandidate{{
		Profile: profile, Runtime: runtime, Command: command, Model: modelName,
		Provider: provider, Source: source,
	}}
}

func dispatcherPlannerFailureHealth(kind orchestration.PlannerFailureKind) model.AgentHealthStatus {
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
