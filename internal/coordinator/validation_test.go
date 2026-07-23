package coordinator

import (
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func validationSnapshot() IncidentSnapshot {
	worker, fallback := "codex", "claude"
	blockKind, blockReason := model.BlockKindCapability, "primary unavailable"
	return IncidentSnapshot{
		IncidentID: "ci1", GraphRevision: 4, FocusTaskID: "blocked",
		Nodes: []NodeSnapshot{
			{ID: "base", Title: "Base", Status: model.TaskStatusDone, WorkflowRole: model.WorkflowRoleWorker, Assignee: &worker, Runtime: model.RuntimeCodex, UpdatedAt: "v1"},
			{ID: "blocked", Title: "Blocked", Status: model.TaskStatusBlocked, WorkflowRole: model.WorkflowRoleWorker, Assignee: &worker, Runtime: model.RuntimeCodex, UpdatedAt: "v2", BlockKind: &blockKind, BlockReason: &blockReason},
			{ID: "next", Title: "Next", Status: model.TaskStatusTodo, WorkflowRole: model.WorkflowRoleReviewer, Assignee: &fallback, Runtime: model.RuntimeClaude, UpdatedAt: "v3"},
		},
		Dependencies: []DependencySnapshot{{PrerequisiteID: "base", DependentID: "next", Satisfied: true}},
		AvailableAgents: []AgentSnapshot{
			{ID: "codex", Runtime: model.RuntimeCodex, Health: string(model.AgentHealthRateLimited)},
			{ID: "claude", Runtime: model.RuntimeClaude, Health: string(model.AgentHealthReady)},
		},
	}
}

func TestValidateAgainstSnapshotAcceptsVersionedHealthyReroute(t *testing.T) {
	proposal := Proposal{
		Summary: "Reroute blocked work", Rationale: "A healthy fallback is available.",
		Actions: []Action{{
			Kind: ActionSetRoute, TaskID: "blocked", ExpectedUpdatedAt: "v2",
			Assignee: "claude", Runtime: model.RuntimeClaude, Reason: "use healthy fallback",
		}},
	}
	result := ValidateAgainstSnapshot(proposal, validationSnapshot(), 3)
	if !result.Valid || len(result.Issues) != 0 || result.Actions[0].Risk != ActionRiskConditional {
		t.Fatalf("validation result = %#v", result)
	}
}

func TestValidateAgainstSnapshotRejectsStaleUnknownAndCyclicChanges(t *testing.T) {
	snapshot := validationSnapshot()
	snapshot.Dependencies = append(snapshot.Dependencies,
		DependencySnapshot{PrerequisiteID: "blocked", DependentID: "next"})
	priority := 5
	proposal := Proposal{
		Summary: "Unsafe proposal", Rationale: "Exercise guards.",
		Actions: []Action{
			{Kind: ActionUpdatePriority, TaskID: "blocked", ExpectedUpdatedAt: "old", Priority: &priority, Reason: "raise"},
			{Kind: ActionSetRoute, TaskID: "next", ExpectedUpdatedAt: "v3", Assignee: "missing", Runtime: model.RuntimeCodex, Reason: "route"},
			{Kind: ActionAddDependency, PrerequisiteID: "next", DependentID: "blocked", Reason: "reverse order"},
		},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	if result.Valid {
		t.Fatalf("unsafe proposal validated: %#v", result)
	}
	encoded := ""
	for _, issue := range result.Issues {
		encoded += issue.Code + " "
	}
	for _, expected := range []string{"stale_task", "unavailable_agent", "dependency_cycle"} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("issues %q omit %s", encoded, expected)
		}
	}
}

func TestValidateAgainstSnapshotRequiresCreatedTaskRelationshipsInScope(t *testing.T) {
	snapshot := validationSnapshot()
	proposal := Proposal{
		Summary: "Add recovery review", Rationale: "An independent review can verify recovery.",
		Actions: []Action{{Kind: ActionCreateTask, Reason: "verify", Task: &TaskDraft{
			Key: "recovery-review", Title: "Review recovery", Body: "Verify the blocked result.",
			Assignee: "claude", Runtime: model.RuntimeClaude, WorkflowRole: model.WorkflowRoleReviewer,
			Prerequisites: []string{"blocked"}, Dependents: []string{"outside"},
		}}},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	if result.Valid || len(result.Issues) != 1 || result.Issues[0].Code != "unknown_relationship_task" {
		t.Fatalf("out-of-scope relationship validation = %#v", result)
	}
}
