package dispatcher

import (
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func recoveryStringPointer(value string) *string { return &value }

func recoveryFingerprintFixture() model.TaskDetail {
	tenant, key := "tenant-a", "source:42"
	workspace, branch := "worktree", "main"
	maxRuntime := 900
	template, step := "delivery", "implement"
	media, checksum := "text/markdown", strings.Repeat("a", 64)
	size, path := int64(42), "/attachments/spec.md"
	runID := "run-parent"
	return model.TaskDetail{
		Task: model.Task{
			ID: "task-child", Board: "board-a", Tenant: &tenant,
			IdempotencyKey: &key, Title: "Implement campaign",
			Body: "Acceptance: tests pass.", Assignee: recoveryStringPointer("claude"),
			Runtime: model.RuntimeClaude, Status: model.TaskStatusRunning,
			WorkflowRole: model.WorkflowRoleWorker, Priority: 9,
			Workspace: &workspace, WorkspaceKind: model.WorkspaceWorktree,
			Branch: &branch, MaxRuntimeSeconds: &maxRuntime,
			Skills: []string{"go", "testing"}, GoalMode: true, GoalMaxTurns: 4,
			WorkflowTemplateID: &template, CurrentStepKey: &step,
			FailureCount: 2, UpdatedAt: "2026-07-24T00:00:00Z",
		},
		Attachments: []model.Attachment{{
			ID: "attachment-a", Kind: "file", Name: "spec.md",
			MediaType: &media, Size: &size, SHA256: &checksum, Path: &path,
		}},
		PrerequisiteHandoffs: []model.PrerequisiteHandoff{{
			PrerequisiteID: "task-parent", DependentID: "task-child",
			SatisfiedAt: "2026-07-23T00:00:00Z", SatisfiedRunID: &runID,
			ChangeSet: &model.ChangeSet{
				ID: "changes-parent", RunID: runID, TaskID: "task-parent",
				RepositoryPath: "/repo", BaseCommit: strings.Repeat("b", 40),
				HeadCommit: strings.Repeat("c", 40),
				DurableRef: "refs/autogora/runs/run-parent", State: "ready",
				ChangedFiles: []string{"engine.go", "engine_test.go"},
			},
		}},
	}
}

func TestRecoveryTaskFingerprintIgnoresRoutingAndLifecycleChanges(t *testing.T) {
	original := recoveryFingerprintFixture()
	changed := recoveryFingerprintFixture()
	changed.Task.Assignee = recoveryStringPointer("codex")
	changed.Task.Runtime = model.RuntimeCodex
	changed.Task.Status = model.TaskStatusReady
	changed.Task.Priority = -5
	changed.Task.CurrentRunID = recoveryStringPointer("run-new")
	changed.Task.ScheduledAt = recoveryStringPointer("2026-08-01T00:00:00Z")
	changed.Task.BlockReason = recoveryStringPointer("old transient failure")
	changed.Task.FailureCount = 99
	changed.Task.UpdatedAt = "2099-01-01T00:00:00Z"

	if got, want := recoveryTaskSpecFingerprint(changed), recoveryTaskSpecFingerprint(original); got != want {
		t.Fatalf("routing/lifecycle mutation changed task fingerprint:\n got %s\nwant %s", got, want)
	}
}

func TestRecoveryTaskFingerprintRejectsSemanticChanges(t *testing.T) {
	base := recoveryFingerprintFixture()
	want := recoveryTaskSpecFingerprint(base)
	tests := map[string]func(*model.TaskDetail){
		"body": func(value *model.TaskDetail) {
			value.Task.Body = "Different acceptance criteria."
		},
		"branch": func(value *model.TaskDetail) {
			value.Task.Branch = recoveryStringPointer("release")
		},
		"goal": func(value *model.TaskDetail) {
			value.Task.GoalMaxTurns++
		},
		"skills": func(value *model.TaskDetail) {
			value.Task.Skills = append(value.Task.Skills, "security")
		},
		"attachment": func(value *model.TaskDetail) {
			value.Attachments[0].SHA256 = recoveryStringPointer(strings.Repeat("d", 64))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := recoveryFingerprintFixture()
			mutate(&changed)
			if got := recoveryTaskSpecFingerprint(changed); got == want {
				t.Fatalf("semantic %s mutation did not change fingerprint", name)
			}
		})
	}
}

func TestRecoveryFingerprintsAreOrderIndependentForSets(t *testing.T) {
	left := recoveryFingerprintFixture()
	right := recoveryFingerprintFixture()
	left.Task.Skills = []string{"testing", "go", "go"}
	right.Task.Skills = []string{"go", "testing"}
	left.Attachments = append(left.Attachments, model.Attachment{ID: "attachment-b", Kind: "url", Name: "design"})
	right.Attachments = []model.Attachment{left.Attachments[1], left.Attachments[0]}
	if got, want := recoveryTaskSpecFingerprint(left), recoveryTaskSpecFingerprint(right); got != want {
		t.Fatalf("set ordering changed task fingerprint:\n got %s\nwant %s", got, want)
	}

	secondRun := "run-second"
	second := model.PrerequisiteHandoff{
		PrerequisiteID: "task-second", DependentID: "task-child",
		SatisfiedAt: "2026-07-23T01:00:00Z", SatisfiedRunID: &secondRun,
	}
	left.PrerequisiteHandoffs = append(left.PrerequisiteHandoffs, second)
	right.PrerequisiteHandoffs = []model.PrerequisiteHandoff{second, left.PrerequisiteHandoffs[0]}
	right.PrerequisiteHandoffs[1].ChangeSet.ChangedFiles =
		[]string{"engine_test.go", "engine.go", "engine.go"}
	if got, want := recoveryPrerequisiteFingerprint(left.PrerequisiteHandoffs),
		recoveryPrerequisiteFingerprint(right.PrerequisiteHandoffs); got != want {
		t.Fatalf("handoff ordering changed prerequisite fingerprint:\n got %s\nwant %s", got, want)
	}
}

func TestRecoveryPrerequisiteFingerprintChangesWithDeliveredHead(t *testing.T) {
	original := recoveryFingerprintFixture()
	changed := recoveryFingerprintFixture()
	changed.PrerequisiteHandoffs[0].ChangeSet.HeadCommit = strings.Repeat("e", 40)
	if recoveryPrerequisiteFingerprint(original.PrerequisiteHandoffs) ==
		recoveryPrerequisiteFingerprint(changed.PrerequisiteHandoffs) {
		t.Fatal("changed prerequisite head did not invalidate fingerprint")
	}
}
