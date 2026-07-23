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
			{
				ID: "codex", Runtime: model.RuntimeCodex, Enabled: true, Roles: []string{"worker"},
				Health: string(model.AgentHealthRateLimited), MaxConcurrent: 2,
			},
			{
				ID: "claude", Runtime: model.RuntimeClaude, Enabled: true, Roles: []string{"worker"},
				Health: string(model.AgentHealthReady), MaxConcurrent: 2,
			},
		},
	}
}

func TestValidateAgainstSnapshotAcceptsVersionedHealthyReroute(t *testing.T) {
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
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
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Unsafe proposal", Rationale: "Exercise guards.",
		Actions: []Action{
			{Kind: ActionUpdatePriority, TaskID: "blocked", ExpectedUpdatedAt: "old", Priority: &priority, Reason: "raise"},
			{Kind: ActionSetRoute, TaskID: "next", ExpectedUpdatedAt: "v3", Assignee: "missing", Runtime: model.RuntimeCodex, Reason: "route"},
			{
				Kind: ActionAddDependency, PrerequisiteID: "next", ExpectedPrerequisiteUpdatedAt: "v3",
				DependentID: "blocked", ExpectedDependentUpdatedAt: "v2", Reason: "reverse order",
			},
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
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Add recovery review", Rationale: "An independent review can verify recovery.",
		Actions: []Action{{
			Kind: ActionCreateTask, Reason: "verify",
			ExpectedTaskVersions: map[string]string{"blocked": "v2", "outside": "v9"},
			Task: &TaskDraft{
				Key: "recovery-review", Title: "Review recovery", Body: "Verify the blocked result.",
				Assignee: "claude", Runtime: model.RuntimeClaude, WorkflowRole: model.WorkflowRoleReviewer,
				Prerequisites: []string{"blocked"}, Dependents: []string{"outside"},
			},
		}},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	if result.Valid || len(result.Issues) != 1 || result.Issues[0].Code != "unknown_relationship_task" {
		t.Fatalf("out-of-scope relationship validation = %#v", result)
	}
}

func TestValidateAgainstSnapshotSimulatesCompoundDependencyActions(t *testing.T) {
	snapshot := validationSnapshot()
	snapshot.Dependencies = nil
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Rewire work", Rationale: "Exercise compound graph validation.",
		Actions: []Action{
			{
				Kind: ActionAddDependency, PrerequisiteID: "blocked", ExpectedPrerequisiteUpdatedAt: "v2",
				DependentID: "next", ExpectedDependentUpdatedAt: "v3", Reason: "first edge",
			},
			{
				Kind: ActionAddDependency, PrerequisiteID: "next", ExpectedPrerequisiteUpdatedAt: "v3",
				DependentID: "blocked", ExpectedDependentUpdatedAt: "v2", Reason: "reverse edge",
			},
		},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	if result.Valid {
		t.Fatalf("compound cycle validated: %#v", result)
	}
	found := false
	for _, issue := range result.Issues {
		found = found || issue.Code == "dependency_cycle"
	}
	if !found {
		t.Fatalf("compound cycle issue missing: %#v", result.Issues)
	}
}

func TestValidateAgainstSnapshotClassifiesAtomicRouteAndUnblock(t *testing.T) {
	snapshot := validationSnapshot()
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Switch and retry", Rationale: "A ready fallback can retry untouched work.",
		Actions: []Action{
			{
				Kind: ActionSetRoute, TaskID: "blocked", ExpectedUpdatedAt: "v2",
				Assignee: "claude", Runtime: model.RuntimeClaude, Reason: "use ready fallback",
			},
			{
				Kind: ActionUnblockTask, TaskID: "blocked", ExpectedUpdatedAt: "v2",
				Reason: "retry with the selected fallback",
			},
		},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	if !result.Valid || len(result.Actions) != 2 {
		t.Fatalf("route and unblock validation = %#v", result)
	}
	for _, action := range result.Actions {
		if action.Risk != ActionRiskConditional {
			t.Fatalf("safe compound action risk = %s, want conditional", action.Risk)
		}
	}

	dirty := snapshot
	dirty.Nodes = append([]NodeSnapshot(nil), snapshot.Nodes...)
	dirty.Nodes[1].PreservedWork = true
	result = ValidateAgainstSnapshot(proposal, dirty, 3)
	if !result.Valid {
		t.Fatalf("preserved work should remain approvable: %#v", result)
	}
	for _, action := range result.Actions {
		if action.Risk != ActionRiskApproval {
			t.Fatalf("preserved-work action risk = %s, want approval", action.Risk)
		}
	}
}

func TestValidateAgainstSnapshotRequiresReadyEnabledWorkerCapacity(t *testing.T) {
	base := validationSnapshot()
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Reroute", Rationale: "Use a fallback.",
		Actions: []Action{{
			Kind: ActionSetRoute, TaskID: "blocked", ExpectedUpdatedAt: "v2",
			Assignee: "claude", Runtime: model.RuntimeClaude, Reason: "fallback",
		}},
	}
	tests := []struct {
		name string
		edit func(*AgentSnapshot)
		code string
	}{
		{name: "disabled", edit: func(agent *AgentSnapshot) { agent.Enabled = false }, code: "disabled_agent"},
		{name: "unknown health", edit: func(agent *AgentSnapshot) { agent.Health = "" }, code: "unhealthy_agent"},
		{name: "wrong role", edit: func(agent *AgentSnapshot) { agent.Roles = []string{"planner"} }, code: "wrong_agent_role"},
		{name: "full", edit: func(agent *AgentSnapshot) { agent.ActiveSlots = agent.MaxConcurrent }, code: "agent_capacity"},
		{name: "cooldown", edit: func(agent *AgentSnapshot) {
			value := "2099-01-01T00:00:00Z"
			agent.CooldownUntil = &value
		}, code: "agent_cooldown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := base
			snapshot.AvailableAgents = append([]AgentSnapshot(nil), base.AvailableAgents...)
			test.edit(&snapshot.AvailableAgents[1])
			result := ValidateAgainstSnapshot(proposal, snapshot, 3)
			found := false
			for _, issue := range result.Issues {
				found = found || issue.Code == test.code
			}
			if result.Valid || !found {
				t.Fatalf("%s validation = %#v", test.name, result)
			}
		})
	}
}

func TestValidateAgainstSnapshotRejectsStaleProposalEnvelope(t *testing.T) {
	snapshot := validationSnapshot()
	priority := 1
	proposal := Proposal{
		IncidentID: "other", ExpectedGraphRevision: 3,
		Summary: "Stale", Rationale: "Exercise envelope guards.",
		Actions: []Action{{
			Kind: ActionUpdatePriority, TaskID: "next", ExpectedUpdatedAt: "v3",
			Priority: &priority, Reason: "reprioritize",
		}},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	codes := map[string]bool{}
	for _, issue := range result.Issues {
		codes[issue.Code] = true
	}
	if result.Valid || !codes["incident_mismatch"] || !codes["stale_graph"] {
		t.Fatalf("stale envelope validation = %#v", result)
	}
}

func TestValidateAgainstSnapshotTreatsMoveToTriageAsFreshReplanning(t *testing.T) {
	snapshot := validationSnapshot()
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Replan blocked work", Rationale: "The old plan cannot proceed.",
		Actions: []Action{
			{
				Kind: ActionMoveToTriage, TaskID: "blocked", ExpectedUpdatedAt: "v2",
				Reason: "replace the failed plan",
			},
			{
				Kind: ActionUnblockTask, TaskID: "blocked", ExpectedUpdatedAt: "v2",
				Reason: "redundant after fresh triage",
			},
		},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	found := false
	for _, issue := range result.Issues {
		found = found || (issue.ActionIndex == 1 && issue.Code == "not_blocked")
	}
	if result.Valid || !found {
		t.Fatalf("move_to_triage retained stale block state: %#v", result)
	}
}

func TestValidateAgainstSnapshotRejectsRecoveryTaskUnderControlParent(t *testing.T) {
	snapshot := validationSnapshot()
	snapshot.Nodes = append(snapshot.Nodes, NodeSnapshot{
		ID: "control", Title: "Approval gate", Status: model.TaskStatusTodo,
		WorkflowRole: model.WorkflowRoleControl, Runtime: model.RuntimeManual, UpdatedAt: "v4",
	})
	proposal := Proposal{
		IncidentID: "ci1", ExpectedGraphRevision: 4,
		Summary: "Add recovery task", Rationale: "Exercise control-task isolation.",
		Actions: []Action{{
			Kind: ActionCreateTask, Reason: "recover",
			ExpectedTaskVersions: map[string]string{"control": "v4"},
			Task: &TaskDraft{
				Key: "recovery", Title: "Recovery", Body: "Repair the blocked work.",
				Assignee: "claude", Runtime: model.RuntimeClaude, WorkflowRole: model.WorkflowRoleWorker,
				ParentTaskID: "control",
			},
		}},
	}
	result := ValidateAgainstSnapshot(proposal, snapshot, 3)
	found := false
	for _, issue := range result.Issues {
		found = found || issue.Code == "control_task"
	}
	if result.Valid || !found {
		t.Fatalf("control parent accepted: %#v", result)
	}
}
