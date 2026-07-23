package orchestration

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/model"
)

func TestGlobalPlannerCandidatesFollowDefaultsAndFallbackGraphs(t *testing.T) {
	config := agentconfig.Default()
	config.Defaults.PlannerAgents = []string{"primary", "second"}
	config.Agents = []agentconfig.Agent{
		{ID: "primary", Runtime: model.RuntimeCodex, Command: "codex", Model: "primary-model", Enabled: true, MaxConcurrent: 3, Roles: []agentconfig.Role{agentconfig.RolePlanner}, Fallbacks: []string{"disabled", "shared"}},
		{ID: "disabled", Runtime: model.RuntimeClaude, Command: "claude", Enabled: false, MaxConcurrent: 6, Roles: []agentconfig.Role{agentconfig.RolePlanner}, Fallbacks: []string{"nested"}},
		{ID: "nested", Runtime: model.RuntimeGemini, Command: "gemini", Enabled: true, MaxConcurrent: 5, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		{ID: "shared", Runtime: model.RuntimeCline, Command: "cline", Enabled: true, MaxConcurrent: 4, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
		{ID: "second", Runtime: model.RuntimeClaude, Command: "claude", Enabled: true, MaxConcurrent: 2, Roles: []agentconfig.Role{agentconfig.RolePlanner}, Fallbacks: []string{"shared"}},
		{ID: "not-default", Runtime: model.RuntimeCodex, Command: "codex", Enabled: true, MaxConcurrent: 9, Roles: []agentconfig.Role{agentconfig.RolePlanner}},
	}

	candidates := GlobalPlannerCandidates(config, agentconfig.RolePlanner)
	profiles := make([]string, 0, len(candidates))
	limits := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		profiles = append(profiles, candidate.Profile)
		limits = append(limits, candidate.MaxConcurrent)
	}
	if want := []string{"primary", "shared", "nested", "second"}; !reflect.DeepEqual(profiles, want) {
		t.Fatalf("candidate order = %v, want %v", profiles, want)
	}
	if candidates[1].Source != "global_fallback" || candidates[1].FallbackFrom == nil || *candidates[1].FallbackFrom != "primary" {
		t.Fatalf("configured fallback metadata = %#v", candidates[1])
	}
	if candidates[3].Source != "global_default" || candidates[3].FallbackFrom != nil {
		t.Fatalf("second default metadata = %#v", candidates[3])
	}
	if want := []int{3, 4, 5, 2}; !reflect.DeepEqual(limits, want) {
		t.Fatalf("candidate max concurrency = %v, want %v", limits, want)
	}
}

func TestFallbackPlannerSkipsCapacityFullCandidateWithoutFailure(t *testing.T) {
	called := make([]string, 0, 1)
	acquired := make([]string, 0, 2)
	released := make([]PlannerAttemptHandle, 0, 1)
	failures := make([]PlannerAttempt, 0, 1)
	var selected PlannerSelection
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{
			{Profile: "primary", Runtime: model.RuntimeCodex, Model: "primary", MaxConcurrent: 1},
			{Profile: "backup", Runtime: model.RuntimeClaude, Model: "backup", MaxConcurrent: 2},
		},
		Factory: func(options CLIPlannerOptions) (Planner, error) {
			name := options.Model
			return func(context.Context, PlannerRequest) (any, error) {
				called = append(called, name)
				return name, nil
			}, nil
		},
		AcquireAttempt: func(_ context.Context, _ PlannerRequest, candidate PlannerCandidate) (PlannerAttemptHandle, bool, error) {
			acquired = append(acquired, candidate.Profile)
			if candidate.Profile == "primary" {
				return nil, false, nil
			}
			return candidate.Profile + "-lease", true, nil
		},
		ReleaseAttempt: func(_ context.Context, handle PlannerAttemptHandle) error {
			released = append(released, handle)
			return nil
		},
		OnFailure: func(_ context.Context, attempt PlannerAttempt) error {
			failures = append(failures, attempt)
			return nil
		},
		OnSelected: func(_ context.Context, selection PlannerSelection) error {
			selected = selection
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := planner(context.Background(), PlannerRequest{TaskID: "task-capacity"})
	if err != nil {
		t.Fatal(err)
	}
	if value != "backup" {
		t.Fatalf("planner result = %v, want backup", value)
	}
	if want := []string{"primary", "backup"}; !reflect.DeepEqual(acquired, want) {
		t.Fatalf("capacity acquisitions = %v, want %v", acquired, want)
	}
	if want := []string{"backup"}; !reflect.DeepEqual(called, want) {
		t.Fatalf("planner calls = %v, want %v", called, want)
	}
	if len(failures) != 0 {
		t.Fatalf("capacity skip recorded failures = %#v", failures)
	}
	if want := []PlannerAttemptHandle{"backup-lease"}; !reflect.DeepEqual(released, want) {
		t.Fatalf("released handles = %#v, want %#v", released, want)
	}
	if selected.Candidate.Profile != "backup" || selected.Attempt != 1 || selected.FallbackFrom == nil || *selected.FallbackFrom != "primary" {
		t.Fatalf("selection callback = %#v", selected)
	}
}

func TestFallbackPlannerReleasesCapacityOnSuccess(t *testing.T) {
	var released PlannerAttemptHandle
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{{Profile: "primary", Runtime: model.RuntimeCodex}},
		Factory: func(CLIPlannerOptions) (Planner, error) {
			return func(context.Context, PlannerRequest) (any, error) { return "ok", nil }, nil
		},
		AcquireAttempt: func(context.Context, PlannerRequest, PlannerCandidate) (PlannerAttemptHandle, bool, error) {
			return "success-lease", true, nil
		},
		ReleaseAttempt: func(ctx context.Context, handle PlannerAttemptHandle) error {
			assertDetachedBoundedReleaseContext(t, ctx)
			released = handle
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(context.Background(), PlannerRequest{}); err != nil {
		t.Fatal(err)
	}
	if released != "success-lease" {
		t.Fatalf("released handle = %#v, want success-lease", released)
	}
}

func TestFallbackPlannerReleasesCapacityOnRetryableFailure(t *testing.T) {
	released := make([]PlannerAttemptHandle, 0, 2)
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{
			{Profile: "primary", Runtime: model.RuntimeCodex, Model: "primary"},
			{Profile: "backup", Runtime: model.RuntimeClaude, Model: "backup"},
		},
		Factory: func(options CLIPlannerOptions) (Planner, error) {
			name := options.Model
			return func(context.Context, PlannerRequest) (any, error) {
				if name == "primary" {
					return nil, errors.New("HTTP 429: rate limit exceeded")
				}
				return "ok", nil
			}, nil
		},
		AcquireAttempt: func(_ context.Context, _ PlannerRequest, candidate PlannerCandidate) (PlannerAttemptHandle, bool, error) {
			return candidate.Profile + "-lease", true, nil
		},
		ReleaseAttempt: func(ctx context.Context, handle PlannerAttemptHandle) error {
			assertDetachedBoundedReleaseContext(t, ctx)
			released = append(released, handle)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(context.Background(), PlannerRequest{}); err != nil {
		t.Fatal(err)
	}
	if want := []PlannerAttemptHandle{"primary-lease", "backup-lease"}; !reflect.DeepEqual(released, want) {
		t.Fatalf("released handles = %#v, want %#v", released, want)
	}
}

func TestFallbackPlannerReleasesCapacityAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	released := false
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{{Profile: "primary", Runtime: model.RuntimeCodex}},
		Factory: func(CLIPlannerOptions) (Planner, error) {
			return func(context.Context, PlannerRequest) (any, error) {
				cancel()
				return nil, context.Canceled
			}, nil
		},
		AcquireAttempt: func(context.Context, PlannerRequest, PlannerCandidate) (PlannerAttemptHandle, bool, error) {
			return "canceled-lease", true, nil
		},
		ReleaseAttempt: func(releaseCtx context.Context, handle PlannerAttemptHandle) error {
			assertDetachedBoundedReleaseContext(t, releaseCtx)
			if handle != "canceled-lease" {
				t.Fatalf("released handle = %#v, want canceled-lease", handle)
			}
			released = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(ctx, PlannerRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("planner error = %v, want context cancellation", err)
	}
	if !released {
		t.Fatal("capacity handle was not released after cancellation")
	}
}

func TestFallbackPlannerReleasesCapacityAfterPanic(t *testing.T) {
	panicValue := errors.New("planner panic")
	released := false
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{{Profile: "primary", Runtime: model.RuntimeCodex}},
		Factory: func(CLIPlannerOptions) (Planner, error) {
			return func(context.Context, PlannerRequest) (any, error) { panic(panicValue) }, nil
		},
		AcquireAttempt: func(context.Context, PlannerRequest, PlannerCandidate) (PlannerAttemptHandle, bool, error) {
			return "panic-lease", true, nil
		},
		ReleaseAttempt: func(context.Context, PlannerAttemptHandle) error {
			released = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = planner(context.Background(), PlannerRequest{})
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want %#v", recovered, panicValue)
	}
	if !released {
		t.Fatal("capacity handle was not released after panic")
	}
}

func assertDetachedBoundedReleaseContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Fatalf("release context is already canceled: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("release context has no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > plannerAttemptReleaseTimeout+time.Second {
		t.Fatalf("release deadline remaining = %s, want within (0, %s]", remaining, plannerAttemptReleaseTimeout+time.Second)
	}
}

func TestFallbackPlannerSkipsUnhealthyAndRetriesAvailabilityFailures(t *testing.T) {
	candidates := []PlannerCandidate{
		{Profile: "cooldown", Runtime: model.RuntimeCodex, Model: "cooldown"},
		{Profile: "primary", Runtime: model.RuntimeClaude, Model: "primary"},
		{Profile: "backup", Runtime: model.RuntimeGemini, Model: "backup", Source: "global_fallback"},
	}
	called := make([]string, 0, 2)
	failures := make([]PlannerAttempt, 0, 1)
	var selected PlannerSelection
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: candidates,
		Factory: func(options CLIPlannerOptions) (Planner, error) {
			name := options.Model
			return func(context.Context, PlannerRequest) (any, error) {
				called = append(called, name)
				if name == "primary" {
					return nil, errors.New("HTTP 429: rate limit exceeded")
				}
				return map[string]any{"agent": name}, nil
			}, nil
		},
		Available: func(_ context.Context, candidate PlannerCandidate) (bool, error) {
			return candidate.Profile != "cooldown", nil
		},
		OnFailure: func(_ context.Context, attempt PlannerAttempt) error {
			failures = append(failures, attempt)
			return nil
		},
		OnSelected: func(_ context.Context, selection PlannerSelection) error {
			selected = selection
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := PlannerRequest{TaskID: "task-1", Kind: PlannerGoalJudge}
	value, err := planner(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"primary", "backup"}; !reflect.DeepEqual(called, want) {
		t.Fatalf("planner calls = %v, want %v", called, want)
	}
	if len(failures) != 1 || failures[0].FailureKind != PlannerFailureRateLimited || failures[0].Candidate.Profile != "primary" {
		t.Fatalf("failure callback = %#v", failures)
	}
	if selected.Candidate.Profile != "backup" || selected.Attempt != 2 || selected.FallbackFrom == nil || *selected.FallbackFrom != "cooldown" || selected.Request.TaskID != "task-1" {
		t.Fatalf("selection callback = %#v", selected)
	}
	if result := value.(map[string]any); result["agent"] != "backup" {
		t.Fatalf("planner result = %#v", value)
	}
}

func TestFallbackPlannerDoesNotHideStructuredOutputFailure(t *testing.T) {
	called := make([]string, 0, 2)
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{
			{Profile: "primary", Runtime: model.RuntimeCodex, Model: "primary"},
			{Profile: "backup", Runtime: model.RuntimeClaude, Model: "backup"},
		},
		Factory: func(options CLIPlannerOptions) (Planner, error) {
			name := options.Model
			return func(context.Context, PlannerRequest) (any, error) {
				called = append(called, name)
				return nil, errors.New("planner did not return a JSON object")
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(context.Background(), PlannerRequest{}); err == nil {
		t.Fatal("expected structured output failure")
	}
	if want := []string{"primary"}; !reflect.DeepEqual(called, want) {
		t.Fatalf("planner calls = %v, want %v", called, want)
	}
}

func TestFallbackPlannerDoesNotRetryCanceledParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make([]string, 0, 2)
	planner, err := CreateFallbackPlanner(FallbackPlannerOptions{
		Candidates: []PlannerCandidate{
			{Profile: "primary", Runtime: model.RuntimeCodex, Model: "primary"},
			{Profile: "backup", Runtime: model.RuntimeClaude, Model: "backup"},
		},
		Factory: func(options CLIPlannerOptions) (Planner, error) {
			name := options.Model
			return func(context.Context, PlannerRequest) (any, error) {
				called = append(called, name)
				cancel()
				return nil, errors.New("planner timed out")
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner(ctx, PlannerRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("planner error = %v, want context cancellation", err)
	}
	if want := []string{"primary"}; !reflect.DeepEqual(called, want) {
		t.Fatalf("planner calls = %v, want %v", called, want)
	}
}

func TestClassifyPlannerFailure(t *testing.T) {
	tests := []struct {
		err  error
		want PlannerFailureKind
		ok   bool
	}{
		{err: &PlannerFailure{Kind: PlannerFailureTimeout, Err: context.DeadlineExceeded}, want: PlannerFailureTimeout, ok: true},
		{err: errors.New("provider says authentication required"), want: PlannerFailureAuth, ok: true},
		{err: errors.New("RESOURCE EXHAUSTED"), want: PlannerFailureRateLimited, ok: true},
		{err: errors.New("planner did not return a JSON object")},
	}
	for _, test := range tests {
		got, ok := ClassifyPlannerFailure(test.err)
		if got != test.want || ok != test.ok {
			t.Fatalf("ClassifyPlannerFailure(%v) = %q, %v; want %q, %v", test.err, got, ok, test.want, test.ok)
		}
	}
}
