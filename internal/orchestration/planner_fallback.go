package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processguard"
)

// PlannerFailureKind identifies failures that make another configured agent a
// useful retry target. Invalid structured output is intentionally excluded:
// changing agents must not hide a prompt or schema defect.
type PlannerFailureKind string

const (
	PlannerFailureSpawn       PlannerFailureKind = "spawn_failure"
	PlannerFailureAuth        PlannerFailureKind = "auth_required"
	PlannerFailureRateLimited PlannerFailureKind = "rate_limited"
	PlannerFailureTimeout     PlannerFailureKind = "timeout"
)

// PlannerFailure preserves a machine-readable availability failure while
// retaining the original process error for errors.Is/errors.As callers.
type PlannerFailure struct {
	Kind PlannerFailureKind
	Err  error
}

func (e *PlannerFailure) Error() string {
	if e == nil || e.Err == nil {
		return "planner availability failure"
	}
	return e.Err.Error()
}

func (e *PlannerFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ClassifyPlannerFailure reports whether an error is safe to retry with the
// next planner candidate. It accepts typed failures and common messages from
// supported coding-agent CLIs because those tools do not share exit codes.
func ClassifyPlannerFailure(err error) (PlannerFailureKind, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, processguard.ErrTeardownUnconfirmed) {
		return "", false
	}
	var failure *PlannerFailure
	if errors.As(err, &failure) && failure.Kind != "" {
		return failure.Kind, true
	}
	var executableError *exec.Error
	if errors.As(err, &executableError) {
		return PlannerFailureSpawn, true
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) && (pathError.Op == "fork/exec" || pathError.Op == "exec") {
		return PlannerFailureSpawn, true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"rate limit", "rate-limit", "rate_limit", "too many requests", "quota exceeded", "usage limit", "usage_limit", "resource exhausted", "resource_exhausted", "credit balance", "http 429", "status 429"} {
		if strings.Contains(message, marker) {
			return PlannerFailureRateLimited, true
		}
	}
	for _, marker := range []string{"authentication required", "authentication failed", "auth required", "not authenticated", "not logged in", "please log in", "please login", "login required", "unauthorized", "invalid api key", "invalid api-key", "invalid_api_key", "api key not found", "missing api key", "http 401", "status 401"} {
		if strings.Contains(message, marker) {
			return PlannerFailureAuth, true
		}
	}
	for _, marker := range []string{"timed out", "timeout", "deadline exceeded"} {
		if strings.Contains(message, marker) {
			return PlannerFailureTimeout, true
		}
	}
	for _, marker := range []string{"executable file not found", "command not found", "cannot find the file"} {
		if strings.Contains(message, marker) {
			return PlannerFailureSpawn, true
		}
	}
	if (strings.Contains(message, "fork/exec") || strings.Contains(message, "exec:")) &&
		(strings.Contains(message, "no such file or directory") || strings.Contains(message, "permission denied")) {
		return PlannerFailureSpawn, true
	}
	return "", false
}

type PlannerCandidate struct {
	Profile       string
	Runtime       model.Runtime
	Command       string
	Model         string
	Provider      string
	MaxConcurrent int
	Source        string
	FallbackFrom  *string
}

type PlannerAttempt struct {
	Request     PlannerRequest
	Candidate   PlannerCandidate
	Attempt     int
	Observation PlannerAttemptObservation
	FailureKind PlannerFailureKind
	Err         error
}

type PlannerSelection struct {
	Request      PlannerRequest
	Candidate    PlannerCandidate
	Attempt      int
	Observation  PlannerAttemptObservation
	FallbackFrom *string
}

type PlannerFactory func(CLIPlannerOptions) (Planner, error)

// PlannerAttemptHandle is an opaque capacity reservation returned immediately
// before a planner candidate is invoked.
type PlannerAttemptHandle any

// PlannerAttemptObservation is opaque caller-owned state reserved immediately
// before a planner candidate is invoked and returned to its result callback.
type PlannerAttemptObservation any

// PlannerAttemptAcquire reserves capacity for one planner invocation. A false
// acquired result without an error means the candidate is currently full and
// should be skipped without recording a health failure.
type PlannerAttemptAcquire func(context.Context, PlannerRequest, PlannerCandidate) (handle PlannerAttemptHandle, acquired bool, err error)

// PlannerAttemptBegin reserves caller-owned causal state after capacity has
// been acquired and immediately before the planner process is invoked.
type PlannerAttemptBegin func(context.Context, PlannerRequest, PlannerCandidate) (PlannerAttemptObservation, error)

// PlannerAttemptRelease releases a reservation returned by
// PlannerAttemptAcquire. It receives a bounded context detached from request
// cancellation so cleanup can finish after the planner's context is canceled.
type PlannerAttemptRelease func(context.Context, PlannerAttemptHandle) error

type FallbackPlannerOptions struct {
	Candidates               []PlannerCandidate
	CWD                      string
	Timeout                  time.Duration
	MaxInvocationsPerRequest int
	Getenv                   func(string) string
	Factory                  PlannerFactory
	Available                func(context.Context, PlannerCandidate) (bool, error)
	AcquireAttempt           PlannerAttemptAcquire
	BeginAttempt             PlannerAttemptBegin
	ReleaseAttempt           PlannerAttemptRelease
	OnFailure                func(context.Context, PlannerAttempt) error
	OnSelected               func(context.Context, PlannerSelection) error
}

type preparedPlannerCandidate struct {
	candidate PlannerCandidate
	planner   Planner
}

const plannerAttemptReleaseTimeout = 5 * time.Second

// CreateFallbackPlanner builds a planner that follows the supplied stable
// candidate order. Health checks run for every request so a long-lived
// supervisor observes cooldown changes without being restarted.
func CreateFallbackPlanner(options FallbackPlannerOptions) (Planner, error) {
	if len(options.Candidates) == 0 {
		return nil, errors.New("at least one planner candidate is required")
	}
	if (options.AcquireAttempt == nil) != (options.ReleaseAttempt == nil) {
		return nil, errors.New("planner attempt acquire and release hooks must be configured together")
	}
	factory := options.Factory
	if factory == nil {
		factory = CreateCLIPlanner
	}
	prepared := make([]preparedPlannerCandidate, 0, len(options.Candidates))
	seen := make(map[string]bool, len(options.Candidates))
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, raw := range options.Candidates {
		candidate := normalizePlannerCandidate(raw)
		if candidate.Profile == "" {
			return nil, errors.New("planner candidate profile cannot be empty")
		}
		if seen[candidate.Profile] {
			continue
		}
		seen[candidate.Profile] = true
		prefix := "AUTOGORA_" + strings.ToUpper(string(candidate.Runtime))
		if candidate.Model == "" {
			candidate.Model = strings.TrimSpace(getenv(prefix + "_MODEL"))
		}
		if candidate.Provider == "" {
			candidate.Provider = strings.TrimSpace(getenv(prefix + "_PROVIDER"))
		}
		planner, err := factory(CLIPlannerOptions{
			Runtime: candidate.Runtime, Command: candidate.Command, Model: candidate.Model,
			Provider: candidate.Provider, CWD: options.CWD, Timeout: options.Timeout, Getenv: getenv,
		})
		if err != nil {
			return nil, fmt.Errorf("configure planner candidate %s: %w", candidate.Profile, err)
		}
		prepared = append(prepared, preparedPlannerCandidate{candidate: candidate, planner: planner})
	}
	if len(prepared) == 0 {
		return nil, errors.New("at least one distinct planner candidate is required")
	}

	return func(ctx context.Context, request PlannerRequest) (any, error) {
		attempt := 0
		failures := make([]error, 0, len(prepared))
		primary := prepared[0].candidate.Profile
		for _, configured := range prepared {
			if options.MaxInvocationsPerRequest > 0 &&
				attempt >= options.MaxInvocationsPerRequest {
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if options.Available != nil {
				available, err := options.Available(ctx, configured.candidate)
				if err != nil {
					return nil, fmt.Errorf("check planner candidate %s health: %w", configured.candidate.Profile, err)
				}
				if !available {
					continue
				}
			}
			var handle PlannerAttemptHandle
			if options.AcquireAttempt != nil {
				var acquired bool
				var err error
				handle, acquired, err = options.AcquireAttempt(ctx, request, configured.candidate)
				if err != nil {
					return nil, fmt.Errorf("acquire planner candidate %s capacity: %w", configured.candidate.Profile, err)
				}
				if !acquired {
					continue
				}
			}
			var observation PlannerAttemptObservation
			if options.BeginAttempt != nil {
				var err error
				observation, err = options.BeginAttempt(ctx, request, configured.candidate)
				if err != nil {
					var releaseErr error
					if options.ReleaseAttempt != nil {
						releaseErr = releasePlannerAttempt(ctx, handle, options.ReleaseAttempt)
					}
					return nil, errors.Join(
						fmt.Errorf("begin planner candidate %s attempt: %w", configured.candidate.Profile, err),
						releaseErr,
					)
				}
			}
			attempt++
			value, err, releaseErr := invokePlannerAttempt(ctx, request, configured, handle, options.ReleaseAttempt)
			if releaseErr != nil {
				wrapped := fmt.Errorf("release planner candidate %s capacity: %w", configured.candidate.Profile, releaseErr)
				if err != nil {
					return nil, errors.Join(err, wrapped)
				}
				return nil, wrapped
			}
			if err == nil {
				selection := PlannerSelection{
					Request: request, Candidate: configured.candidate, Attempt: attempt, Observation: observation,
				}
				if configured.candidate.Profile != primary {
					selection.FallbackFrom = stringReference(primary)
				} else if configured.candidate.FallbackFrom != nil {
					selection.FallbackFrom = stringReference(*configured.candidate.FallbackFrom)
				}
				if options.OnSelected != nil {
					if recordErr := options.OnSelected(ctx, selection); recordErr != nil {
						return nil, fmt.Errorf("record selected planner candidate %s: %w", configured.candidate.Profile, recordErr)
					}
				}
				return value, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			kind, retry := ClassifyPlannerFailure(err)
			if !retry || errors.Is(err, context.Canceled) {
				return nil, err
			}
			failure := PlannerAttempt{
				Request: request, Candidate: configured.candidate, Attempt: attempt,
				Observation: observation, FailureKind: kind, Err: err,
			}
			if options.OnFailure != nil {
				if recordErr := options.OnFailure(ctx, failure); recordErr != nil {
					return nil, errors.Join(err, fmt.Errorf("record planner candidate %s failure: %w", configured.candidate.Profile, recordErr))
				}
			}
			failures = append(failures, fmt.Errorf("%s (%s): %w", configured.candidate.Profile, kind, err))
		}
		if len(failures) == 0 {
			return nil, errors.New("no healthy planner candidate is available")
		}
		return nil, fmt.Errorf("all available planner candidates failed: %w", errors.Join(failures...))
	}, nil
}

func invokePlannerAttempt(ctx context.Context, request PlannerRequest, configured preparedPlannerCandidate, handle PlannerAttemptHandle, release PlannerAttemptRelease) (value any, plannerErr, releaseErr error) {
	if release != nil {
		defer func() {
			releaseErr = releasePlannerAttempt(ctx, handle, release)
		}()
	}
	value, plannerErr = configured.planner(ctx, request)
	return value, plannerErr, nil
}

func releasePlannerAttempt(ctx context.Context, handle PlannerAttemptHandle, release PlannerAttemptRelease) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("planner attempt release panicked: %v", recovered)
		}
	}()
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plannerAttemptReleaseTimeout)
	defer cancel()
	return release(releaseCtx, handle)
}

// GlobalPlannerCandidates expands each role default and then that default's
// configured fallback graph. The first occurrence wins, keeping the order
// deterministic even when fallback graphs converge.
func GlobalPlannerCandidates(config agentconfig.Config, role agentconfig.Role) []PlannerCandidate {
	config = agentconfig.Normalize(config)
	var roots []string
	switch role {
	case agentconfig.RolePlanner:
		roots = config.Defaults.PlannerAgents
	case agentconfig.RoleCoordinator:
		roots = config.Defaults.CoordinatorAgents
	case agentconfig.RoleJudge:
		roots = config.Defaults.JudgeAgents
	default:
		return nil
	}
	result := make([]PlannerCandidate, 0, len(roots))
	seen := make(map[string]bool, len(config.Agents))
	for _, root := range roots {
		queue := []string{root}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			agent, found := config.Find(id)
			if !found {
				continue
			}
			queue = append(queue, agent.Fallbacks...)
			if !agent.Enabled || !agentHasRole(agent, role) {
				continue
			}
			source := "global_default"
			var fallbackFrom *string
			if id != root {
				source = "global_fallback"
				fallbackFrom = stringReference(root)
			}
			result = append(result, PlannerCandidate{
				Profile: agent.ID, Runtime: agent.Runtime, Command: agent.Command, Model: agent.Model,
				Provider: agent.Provider, MaxConcurrent: agent.MaxConcurrent, Source: source, FallbackFrom: fallbackFrom,
			})
		}
	}
	return result
}

func normalizePlannerCandidate(candidate PlannerCandidate) PlannerCandidate {
	candidate.Profile = strings.TrimSpace(candidate.Profile)
	candidate.Command = strings.TrimSpace(candidate.Command)
	candidate.Model = strings.TrimSpace(candidate.Model)
	candidate.Provider = strings.TrimSpace(candidate.Provider)
	candidate.Source = strings.TrimSpace(candidate.Source)
	if candidate.FallbackFrom != nil {
		candidate.FallbackFrom = stringReference(*candidate.FallbackFrom)
	}
	return candidate
}

func stringReference(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func agentHasRole(agent agentconfig.Agent, role agentconfig.Role) bool {
	for _, candidate := range agent.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}
