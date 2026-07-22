package dispatcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/kanban/internal/model"
	"github.com/nn1a/kanban/internal/store"
)

func claimedTask(t *testing.T, runtime model.Runtime) model.ClaimedTask {
	t.Helper()
	opened, err := store.Open(":memory:", "default", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	workspace := t.TempDir()
	assignee := "worker"
	detail, err := opened.CreateTask(context.Background(), store.CreateTaskInput{Title: string(runtime) + " task", Assignee: &assignee, Runtime: runtime, Workspace: &workspace})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := opened.ClaimTask(context.Background(), store.ClaimOptions{TaskID: detail.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v, %v", claim, err)
	}
	return *claim
}

func TestBuildRunnerCommandsAreScopedAndDoNotLeakToken(t *testing.T) {
	for _, runtime := range []model.Runtime{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeCline, model.RuntimeGemini} {
		t.Run(string(runtime), func(t *testing.T) {
			claim := claimedTask(t, runtime)
			options := RunnerOptions{DBPath: filepath.Join(t.TempDir(), "taskcircuit.db"), CLIPath: filepath.Join(t.TempDir(), "taskcircuit"), ClineApprovalDir: t.TempDir()}
			command, err := BuildRunnerCommand(claim, options, "")
			if err != nil {
				t.Fatal(err)
			}
			if command.Env["TASKCIRCUIT_TASK_ID"] != claim.Task.Task.ID || command.Env["TASKCIRCUIT_CLAIM_TOKEN"] != claim.ClaimToken {
				t.Fatalf("scope missing: %#v", command.Env)
			}
			if strings.Contains(strings.Join(command.Args, " "), claim.ClaimToken) {
				t.Fatal("claim token leaked into argv")
			}
			joined := strings.Join(command.Args, " ")
			switch runtime {
			case model.RuntimeCline:
				if command.ToolApproval == nil || !strings.Contains(joined, "--auto-approve false") || strings.Contains(joined, "mcpServers") || !strings.Contains(joined, "scoped TaskCircuit CLI bridge") || !strings.Contains(joined, "$TASKCIRCUIT_TASK_ID") {
					t.Fatalf("invalid Cline command: %#v", command)
				}
			case model.RuntimeGemini:
				if command.PolicyFile == nil || !strings.Contains(joined, "--approval-mode default") || !strings.Contains(command.PolicyFile.Content, `toolName = "mcp_*"`) || !strings.Contains(joined, "$TASKCIRCUIT_TASK_ID") {
					t.Fatalf("invalid Gemini command: %#v", command)
				}
			case model.RuntimeClaude:
				if !strings.Contains(joined, "dontAsk") || !strings.Contains(joined, "mcp__taskcircuit__taskcircuit_complete") {
					t.Fatalf("invalid Claude command: %#v", command)
				}
			case model.RuntimeCodex:
				if !strings.Contains(joined, "read-only") || !strings.Contains(joined, "mcp_servers.taskcircuit.required=true") {
					t.Fatalf("invalid Codex command: %#v", command)
				}
			}
		})
	}
}

func TestWriteOptInAndGoalContinuations(t *testing.T) {
	gemini := claimedTask(t, model.RuntimeGemini)
	options := RunnerOptions{DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "taskcircuit"), AllowWrites: true}
	command, err := BuildRunnerCommand(gemini, options, "")
	if err != nil || command.PolicyFile != nil || !strings.Contains(strings.Join(command.Args, " "), "--approval-mode yolo") {
		t.Fatalf("write opt-in failed: %#v, %v", command, err)
	}
	codex := claimedTask(t, model.RuntimeCodex)
	continued, err := BuildGoalContinuationCommand(codex, options, "session-1", "continue")
	if err != nil || strings.Join(continued.Args[:2], " ") != "exec resume" || !strings.Contains(strings.Join(continued.Args, " "), "session-1") {
		t.Fatalf("Codex continuation failed: %#v, %v", continued, err)
	}
	cline := claimedTask(t, model.RuntimeCline)
	continued, err = BuildGoalContinuationCommand(cline, options, "", "finish the gap")
	if err != nil || !strings.Contains(strings.Join(continued.Args, " "), "Continuation focus: finish the gap") {
		t.Fatalf("Cline continuation failed: %#v, %v", continued, err)
	}
}

func TestSessionIDExtraction(t *testing.T) {
	output := "noise\n{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n"
	if SessionIDFromOutput(output) != "thread-1" {
		t.Fatal("session id was not extracted")
	}
}
