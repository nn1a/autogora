package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

func TestProposalSchemaMeetsStrictObjectRequirements(t *testing.T) {
	assertStrictCoordinatorObjectSchema(t, "$", proposalSchema(3))
}

func TestAnalyzerDecodesStrictCreateTaskOptionalsAndVersions(t *testing.T) {
	analyzer := Analyzer{
		MaxActions: 2,
		Planner: func(context.Context, orchestration.PlannerRequest) (any, error) {
			return map[string]any{
				"incidentId":            "ci1",
				"expectedGraphRevision": 7,
				"summary":               "Create bounded recovery work",
				"rationale":             "One worker task can recover the incident.",
				"actions": []any{map[string]any{
					"kind": "create_task",
					"task": map[string]any{
						"key": "recovery", "title": "Recover", "body": "Implement recovery",
						"assignee": "codex", "runtime": "codex", "workflowRole": "worker",
						"priority": 1, "prerequisites": []any{"t1"},
						"dependents": []any{}, "parentTaskId": nil,
					},
					"expectedTaskVersions": []any{
						map[string]any{"taskId": "t1", "updatedAt": "v1"},
					},
					"reason": "isolate recovery",
				}},
			}, nil
		},
	}
	proposal, err := analyzer.Analyze(context.Background(), IncidentSnapshot{
		IncidentID: "ci1", GraphRevision: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Actions) != 1 ||
		proposal.Actions[0].Task == nil ||
		proposal.Actions[0].Task.ParentTaskID != "" ||
		proposal.Actions[0].ExpectedTaskVersions["t1"] != "v1" {
		t.Fatalf("strict create_task decode = %+v", proposal.Actions)
	}

	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Proposal
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Actions[0].ExpectedTaskVersions["t1"] != "v1" {
		t.Fatalf("legacy map round trip = %+v", roundTrip.Actions[0])
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	actions := wire["actions"].([]any)
	versions := actions[0].(map[string]any)["expectedTaskVersions"]
	if _, ok := versions.(map[string]any); !ok {
		t.Fatalf("stored expectedTaskVersions = %#v, want legacy object map", versions)
	}
}

func TestTaskVersionMapRejectsInvalidStrictEntries(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "empty task ID",
			value: `[{"taskId":" ","updatedAt":"v1"}]`,
			want:  "require taskId and updatedAt",
		},
		{
			name:  "empty version",
			value: `[{"taskId":"t1","updatedAt":" "}]`,
			want:  "require taskId and updatedAt",
		},
		{
			name:  "duplicate task ID",
			value: `[{"taskId":"t1","updatedAt":"v1"},{"taskId":"t1","updatedAt":"v2"}]`,
			want:  "duplicate taskId t1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var versions TaskVersionMap
			err := json.Unmarshal([]byte(test.value), &versions)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAnalyzerRejectsUnboundedRecoveryCheckpointContextBeforePlanner(t *testing.T) {
	called := false
	analyzer := Analyzer{
		Planner: func(context.Context, orchestration.PlannerRequest) (any, error) {
			called = true
			return nil, nil
		},
	}
	snapshot := IncidentSnapshot{
		IncidentID: "ci-recovery-bound",
		RecoveryCheckpoints: []RecoveryCheckpointSnapshot{
			{ID: "rcp-1"},
			{ID: "rcp-2"},
			{ID: "rcp-3"},
			{ID: "rcp-4"},
		},
	}
	err := func() error {
		_, err := analyzer.Analyze(context.Background(), snapshot)
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "bounded analysis limit") {
		t.Fatalf("unbounded recovery context error = %v", err)
	}
	if called {
		t.Fatal("Planner received an unbounded recovery checkpoint snapshot")
	}
}

func assertStrictCoordinatorObjectSchema(t *testing.T, path string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "object" {
			if additional, exists := typed["additionalProperties"]; !exists || additional != false {
				t.Errorf("%s object additionalProperties = %#v, want false", path, additional)
			}
			properties, ok := typed["properties"].(map[string]any)
			if !ok {
				t.Errorf("%s object properties = %#v, want map", path, typed["properties"])
			} else {
				required, ok := typed["required"].([]string)
				if !ok {
					t.Errorf("%s object required = %#v, want []string", path, typed["required"])
				} else {
					requiredSet := make(map[string]bool, len(required))
					for _, name := range required {
						if requiredSet[name] {
							t.Errorf("%s required contains duplicate %q", path, name)
						}
						requiredSet[name] = true
						if _, exists := properties[name]; !exists {
							t.Errorf("%s requires unknown property %q", path, name)
						}
					}
					for name := range properties {
						if !requiredSet[name] {
							t.Errorf("%s property %q is not required", path, name)
						}
					}
					if len(required) != len(properties) {
						t.Errorf(
							"%s required/property count = %d/%d",
							path,
							len(required),
							len(properties),
						)
					}
				}
			}
		}
		for name, nested := range typed {
			assertStrictCoordinatorObjectSchema(
				t,
				fmt.Sprintf("%s.%s", path, name),
				nested,
			)
		}
	case []any:
		for index, nested := range typed {
			assertStrictCoordinatorObjectSchema(
				t,
				fmt.Sprintf("%s[%d]", path, index),
				nested,
			)
		}
	}
}

func TestAnalyzerRequestsToolFreeStructuredProposal(t *testing.T) {
	var captured orchestration.PlannerRequest
	analyzer := Analyzer{
		MaxActions: 2,
		Planner: func(_ context.Context, request orchestration.PlannerRequest) (any, error) {
			captured = request
			return map[string]any{
				"incidentId":            "ci1",
				"expectedGraphRevision": 7,
				"summary":               "Use the configured fallback",
				"rationale":             "The primary agent is unavailable and the task has not started.",
				"actions": []any{map[string]any{
					"kind": "set_route", "taskId": "t1", "expectedUpdatedAt": "v1",
					"assignee": "claude", "runtime": "claude", "reason": "healthy fallback",
				}},
			}, nil
		},
	}
	proposal, err := analyzer.Analyze(context.Background(), IncidentSnapshot{
		IncidentID: "ci1", Trigger: "agent_exhausted", GraphRevision: 7, FocusTaskID: "t1",
		Nodes: []NodeSnapshot{{
			ID: "t1", Title: "Implement", Status: model.TaskStatusReady,
			WorkflowRole: model.WorkflowRoleWorker, Runtime: model.RuntimeCodex, UpdatedAt: "v1",
		}},
		AvailableAgents: []AgentSnapshot{{ID: "claude", Runtime: model.RuntimeClaude, Health: "ready"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Kind != orchestration.PlannerCoordinator || captured.TaskID != "t1" {
		t.Fatalf("unexpected coordinator request: %#v", captured)
	}
	if !strings.Contains(captured.Prompt, "Do not call tools") ||
		!strings.Contains(captured.Prompt, `"graphRevision":7`) {
		t.Fatalf("coordinator prompt omitted safety/context: %s", captured.Prompt)
	}
	actions := captured.Schema["properties"].(map[string]any)["actions"].(map[string]any)
	if actions["maxItems"] != 2 {
		t.Fatalf("schema maxItems = %#v", actions["maxItems"])
	}
	if variants, ok := actions["items"].(map[string]any)["anyOf"].([]any); !ok || len(variants) != 7 {
		t.Fatalf("schema action variants = %#v", actions["items"])
	}
	if len(proposal.Actions) != 1 || proposal.Actions[0].Risk() != ActionRiskConditional {
		t.Fatalf("unexpected proposal: %#v", proposal)
	}
}

func TestValidateProposalRejectsForbiddenAndAmbiguousActions(t *testing.T) {
	base := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 1,
		Summary: "summary", Rationale: "rationale",
	}
	tests := []struct {
		name   string
		action Action
		want   string
	}{
		{
			name:   "unknown action",
			action: Action{Kind: "complete_task", TaskID: "t1", ExpectedUpdatedAt: "v1", Reason: "done"},
			want:   "unsupported action",
		},
		{
			name:   "stale unsafe task mutation",
			action: Action{Kind: ActionUnblockTask, TaskID: "t1", Reason: "retry"},
			want:   "expectedUpdatedAt",
		},
		{
			name: "manual route",
			action: Action{
				Kind: ActionSetRoute, TaskID: "t1", ExpectedUpdatedAt: "v1",
				Assignee: "human", Runtime: model.RuntimeManual, Reason: "delegate",
			},
			want: "coding-agent runtime",
		},
		{
			name: "planner creates finalizer",
			action: Action{Kind: ActionCreateTask, Reason: "replace root", Task: &TaskDraft{
				Key: "new", Title: "Root", Body: "Take over", Assignee: "worker",
				Runtime: model.RuntimeCodex, WorkflowRole: model.WorkflowRoleFinalizer,
			}},
			want: "worker or reviewer",
		},
		{
			name: "mixed fields",
			action: Action{
				Kind: ActionUpdatePriority, TaskID: "t1", ExpectedUpdatedAt: "v1",
				Priority: intPointer(5), Assignee: "claude", Reason: "ambiguous",
			},
			want: "route fields",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal := base
			proposal.Actions = []Action{test.action}
			err := ValidateProposal(proposal, 3)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestValidateProposalBoundsActionsAndClassifiesGraphChanges(t *testing.T) {
	priority := 10
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 1,
		Summary: "rebalance", Rationale: "two changes",
		Actions: []Action{
			{Kind: ActionUpdatePriority, TaskID: "t1", ExpectedUpdatedAt: "v1", Priority: &priority, Reason: "unblock capacity"},
			{Kind: ActionRemoveDependency, PrerequisiteID: "t1", DependentID: "t2", Reason: "edge is obsolete"},
		},
	}
	if err := ValidateProposal(proposal, 1); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("action bound error = %v", err)
	}
	if proposal.Actions[1].Risk() != ActionRiskApproval {
		t.Fatalf("graph mutation risk = %q", proposal.Actions[1].Risk())
	}
}
