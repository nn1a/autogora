package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

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
