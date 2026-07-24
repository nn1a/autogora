package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/orchestration"
)

func TestAnalyzerSanitizesEveryIncidentTriggerAtExternalBoundary(t *testing.T) {
	const (
		repositoryPath   = "/home/operator/private/autogora-repository"
		worktreePath     = "/tmp/autogora/worktrees/run-private"
		durableRef       = "refs/autogora/checkpoints/run_private"
		claimToken       = "claim_boundary_secret_38d8c6"
		reservationToken = "reservation_boundary_secret_57ad31"
		safeChangedPath  = "src/game/engine.go"
	)
	for _, trigger := range model.CoordinationTriggers {
		t.Run(string(trigger), func(t *testing.T) {
			var prompt string
			analyzer := Analyzer{
				Planner: func(
					_ context.Context,
					request orchestration.PlannerRequest,
				) (any, error) {
					prompt = request.Prompt
					return Proposal{
						IncidentID:            "ci_boundary",
						ExpectedGraphRevision: 7,
						Summary:               "Escalate safely",
						Rationale:             "No automatic action is safe.",
						Actions:               []Action{},
					}, nil
				},
			}
			blockKind := model.BlockKindCapability
			blockReason := strings.Join([]string{
				repositoryPath,
				worktreePath,
				durableRef,
				"claim token " + claimToken,
				"reservation token " + reservationToken,
			}, " ")
			supersedeReason := "failed in " + worktreePath + " using " + reservationToken
			snapshot := IncidentSnapshot{
				IncidentID: "ci_boundary",
				Trigger:    string(trigger),
				Severity:   string(model.CoordinationSeverityError),
				Summary:    "raw incident " + repositoryPath + " " + durableRef,
				Details: boundaryCanaryDetails(
					trigger,
					repositoryPath,
					worktreePath,
					durableRef,
					claimToken,
					reservationToken,
				),
				GraphRevision: 7,
				FocusTaskID:   "t_focus",
				Nodes: []NodeSnapshot{{
					ID:           "t_focus",
					Title:        "Recover the game engine",
					Status:       model.TaskStatusBlocked,
					WorkflowRole: model.WorkflowRoleWorker,
					Runtime:      model.RuntimeCodex,
					UpdatedAt:    "2026-07-24T04:05:06Z",
					BlockKind:    &blockKind,
					BlockReason:  &blockReason,
				}},
				RecoveryCheckpoints: []RecoveryCheckpointSnapshot{{
					ID:               "rcp_boundary",
					State:            model.RecoveryCheckpointSuperseded,
					SourceRunID:      "run_boundary",
					ChangedFileCount: 4,
					BoundedChangedFiles: []string{
						safeChangedPath,
						repositoryPath + "/secret.go",
						"../worktree/escape.go",
						durableRef,
					},
					CreatedAt:       "2026-07-24T04:05:06Z",
					SupersedeReason: &supersedeReason,
				}},
				Diagnostics: []IssueSnapshot{{
					Kind: "workspace_inspection_failed",
					Detail: "inspect " + worktreePath + ": claim token " +
						claimToken,
				}},
				AvailableAgents: []AgentSnapshot{{
					ID:            "codex-worker",
					Runtime:       model.RuntimeCodex,
					Model:         "claim token " + claimToken,
					Provider:      "openai",
					Enabled:       true,
					Roles:         []string{"worker"},
					Health:        string(model.AgentHealthReady),
					MaxConcurrent: 1,
					Fallbacks:     []string{"codex-worker"},
				}},
			}
			if _, err := analyzer.Analyze(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				repositoryPath,
				worktreePath,
				durableRef,
				claimToken,
				reservationToken,
				"raw run error",
				"raw run summary",
			} {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("external Coordinator prompt leaked %q: %s", forbidden, prompt)
				}
			}
			if !strings.Contains(prompt, safeChangedPath) ||
				!strings.Contains(prompt, `"diagnosticCode":"`+string(trigger)+`"`) ||
				!strings.Contains(prompt, `"blockReason":"capability_block"`) ||
				!strings.Contains(prompt, `"detail":"Workspace state could not be inspected."`) {
				t.Fatalf("external boundary removed safe machine context: %s", prompt)
			}
			if snapshot.Nodes[0].BlockReason == nil ||
				*snapshot.Nodes[0].BlockReason != blockReason {
				t.Fatal("sanitization mutated the caller's snapshot")
			}
		})
	}
}

func TestSafeRelativeChangedPathRejectsAmbiguousAndSensitiveValues(t *testing.T) {
	tests := []struct {
		value string
		safe  bool
	}{
		{value: "src/game/engine.go", safe: true},
		{value: "assets/player idle.png", safe: true},
		{value: "/home/operator/repository/main.go"},
		{value: `C:\Users\operator\repository\main.go`},
		{value: "../worktree/main.go"},
		{value: "src/../worktree/main.go"},
		{value: "refs/autogora/checkpoints/run-private"},
		{value: ".git/config"},
		{value: "src/control\ninjection.go"},
		{value: strings.Repeat("a", coordinatorChangedPathLimit+1)},
	}
	for _, test := range tests {
		value, safe := SafeRelativeChangedPath(test.value)
		if safe != test.safe {
			t.Errorf("SafeRelativeChangedPath(%q) safe=%t, want %t", test.value, safe, test.safe)
		}
		if safe && value != test.value {
			t.Errorf("SafeRelativeChangedPath(%q)=%q, want unchanged", test.value, value)
		}
	}
}

func TestSanitizeOperatorRecoveryIncidentKeepsPublicFenceIdentity(t *testing.T) {
	for _, diagnosticCode := range []string{
		"unverifiable_process_ownership",
		"process_teardown_unconfirmed",
		"process_teardown_proof_unavailable",
	} {
		t.Run(diagnosticCode, func(t *testing.T) {
			snapshot := SanitizeIncidentSnapshot(IncidentSnapshot{
				IncidentID:    "ci_operator_recovery",
				Trigger:       string(model.CoordinationTriggerRunInvariant),
				Severity:      string(model.CoordinationSeverityCritical),
				GraphRevision: 7,
				FocusTaskID:   "t_focus",
				Details: map[string]any{
					"reason":          "operator_recovery_required",
					"currentRunId":    "run_boundary",
					"diagnosticCode":  diagnosticCode,
					"fenceGeneration": 4,
					"fenceToken":      "secret-fence-token",
					"processIdentity": "linux:/private/identity",
					"worktreePath":    "/home/operator/private/worktree",
				},
			})
			if snapshot.Details["reason"] != "operator_recovery_required" ||
				snapshot.Details["diagnosticCode"] != diagnosticCode ||
				snapshot.Details["fenceGeneration"] != 4 ||
				snapshot.Details["currentRunId"] != "run_boundary" {
				t.Fatalf("public operator recovery context was removed: %#v", snapshot.Details)
			}
			encoded, err := json.Marshal(snapshot.Details)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"secret-fence-token",
				"linux:/private/identity",
				"/home/operator/private/worktree",
			} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("operator recovery boundary leaked %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestSanitizeOperatorRecoveryIncidentReplacesUnknownDiagnosticCode(t *testing.T) {
	snapshot := SanitizeIncidentSnapshot(IncidentSnapshot{
		IncidentID:    "ci_operator_recovery",
		Trigger:       string(model.CoordinationTriggerRunInvariant),
		Severity:      string(model.CoordinationSeverityCritical),
		GraphRevision: 7,
		FocusTaskID:   "t_focus",
		Details: map[string]any{
			"reason":          "operator_recovery_required",
			"currentRunId":    "run_boundary",
			"diagnosticCode":  "/home/operator/private/process-diagnostic",
			"fenceGeneration": 4,
		},
	})
	if snapshot.Details["diagnosticCode"] != string(model.CoordinationTriggerRunInvariant) {
		t.Fatalf("unknown operator diagnostic crossed boundary: %#v", snapshot.Details)
	}
}

func TestAnalyzerRejectsSensitiveIdentityBeforePlanner(t *testing.T) {
	called := false
	analyzer := Analyzer{
		Planner: func(context.Context, orchestration.PlannerRequest) (any, error) {
			called = true
			return nil, nil
		},
	}
	if _, err := analyzer.Analyze(context.Background(), IncidentSnapshot{
		IncidentID: "/home/operator/private/incident",
		Trigger:    string(model.CoordinationTriggerRunInvariant),
	}); err == nil || !strings.Contains(err.Error(), "incident ID is unsafe") {
		t.Fatalf("unsafe incident ID error = %v", err)
	}
	if called {
		t.Fatal("Planner received a snapshot with an unsafe identity")
	}
}

func TestSanitizeIncidentSnapshotRejectsInvalidBlockKindAndPrefixedPaths(
	t *testing.T,
) {
	invalidKind := model.BlockKind("/home/operator/secret-kind")
	blockReason := "do not expose this reason"
	snapshot := SanitizeIncidentSnapshot(IncidentSnapshot{
		IncidentID:    "ci_boundary",
		Trigger:       string(model.CoordinationTriggerRunInvariant),
		Severity:      string(model.CoordinationSeverityError),
		GraphRevision: 1,
		FocusTaskID:   "t_focus",
		Nodes: []NodeSnapshot{{
			ID:           "t_focus",
			Title:        "Open file:///home/operator/private/spec.md",
			Status:       model.TaskStatusBlocked,
			WorkflowRole: model.WorkflowRoleWorker,
			Runtime:      model.RuntimeCodex,
			UpdatedAt:    "2026-07-24T04:05:06Z",
			BlockKind:    &invalidKind,
			BlockReason:  &blockReason,
		}},
		AvailableAgents: []AgentSnapshot{{
			ID: "codex-worker", Runtime: model.RuntimeCodex,
			Model:    "path:/home/operator/private/model",
			Provider: "(/home/operator/private/provider)",
			Roles:    []string{"worker"}, Health: string(model.AgentHealthReady),
		}},
	})
	if len(snapshot.Nodes) != 1 {
		t.Fatalf("sanitized nodes = %+v", snapshot.Nodes)
	}
	node := snapshot.Nodes[0]
	if node.BlockKind != nil || node.BlockReason == nil ||
		*node.BlockReason != "task_blocked" {
		t.Fatalf("invalid block state crossed boundary: %+v", node)
	}
	if node.Title != "Task title withheld at the Coordinator boundary" {
		t.Fatalf("file URI title = %q", node.Title)
	}
	if len(snapshot.AvailableAgents) != 1 ||
		snapshot.AvailableAgents[0].Model != "" ||
		snapshot.AvailableAgents[0].Provider != "" {
		t.Fatalf("prefixed paths crossed agent boundary: %+v", snapshot.AvailableAgents)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/home/operator") ||
		strings.Contains(string(encoded), "file://") {
		t.Fatalf("filesystem location crossed Coordinator boundary: %s", encoded)
	}
}

func boundaryCanaryDetails(
	trigger model.CoordinationTrigger,
	repositoryPath string,
	worktreePath string,
	durableRef string,
	claimToken string,
	reservationToken string,
) map[string]any {
	details := map[string]any{
		"repositoryPath":   repositoryPath,
		"worktreePath":     worktreePath,
		"durableRef":       durableRef,
		"claimToken":       claimToken,
		"reservationToken": reservationToken,
		"blockReason":      "raw block reason " + repositoryPath,
		"error":            "raw run error " + claimToken,
		"summary":          "raw run summary " + reservationToken,
		"taskStatus":       string(model.TaskStatusBlocked),
		"blockKind":        string(model.BlockKindCapability),
		"taskUpdatedAt":    "2026-07-24T04:05:06Z",
	}
	switch trigger {
	case model.CoordinationTriggerRepeatedBlock:
		details["blockRecurrences"] = 3
		details["failureCount"] = 1
		details["maxRetries"] = 4
	case model.CoordinationTriggerRetryExhausted:
		details["failureCount"] = 2
		details["maxRetries"] = 2
		details["lastRun"] = map[string]any{
			"id":       "run_boundary",
			"status":   string(model.RunStatusFailed),
			"exitCode": 1,
			"error":    "raw run error " + repositoryPath,
			"summary":  "raw run summary " + durableRef,
			"endedAt":  "2026-07-24T04:05:06Z",
		}
	case model.CoordinationTriggerGraphStalled:
		details["unfinishedTasks"] = 2
		details["actionableTasks"] = 1
		details["byStatus"] = map[string]any{"blocked": 1}
	case model.CoordinationTriggerIntegrationConflict:
		details["code"] = "conflict"
		details["reason"] = "merge failed in " + worktreePath
		details["conflictingFiles"] = []any{
			"src/game/engine.go",
			repositoryPath + "/secret.go",
		}
	case model.CoordinationTriggerAgentExhausted:
		details["runtime"] = string(model.RuntimeCodex)
		details["assignee"] = "codex-worker"
		details["capacityIgnored"] = true
		details["routes"] = []any{map[string]any{
			"id":         "codex-worker",
			"runtime":    string(model.RuntimeCodex),
			"enabled":    true,
			"workerRole": true,
			"health":     string(model.AgentHealthReady),
		}}
	case model.CoordinationTriggerRunInvariant:
		details["currentRunId"] = "run_boundary"
		details["runStatus"] = string(model.RunStatusCrashed)
		details["reason"] = "recovery_checkpoint_adoption_exhausted"
		details["checkpointId"] = "rcp_boundary"
		details["checkpointState"] = string(model.RecoveryCheckpointPending)
		details["sourceRunId"] = "run_source"
	}
	// Prove the input really contains all canaries before exercising the
	// boundary, so a malformed fixture cannot create a false-positive test.
	encoded, _ := json.Marshal(details)
	for _, expected := range []string{
		repositoryPath,
		worktreePath,
		durableRef,
		claimToken,
		reservationToken,
	} {
		if !strings.Contains(string(encoded), expected) {
			panic("Coordinator boundary canary fixture omitted " + expected)
		}
	}
	return details
}
