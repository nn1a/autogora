package dispatcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func hasArgPair(args []string, name, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			return true
		}
	}
	return false
}

func countArg(args []string, value string) int {
	count := 0
	for _, argument := range args {
		if argument == value {
			count++
		}
	}
	return count
}

func argPairValues(args []string, name string) []string {
	values := []string{}
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			values = append(values, args[index+1])
			index++
		}
	}
	return values
}

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
			options := RunnerOptions{DBPath: filepath.Join(t.TempDir(), "autogora.db"), CLIPath: filepath.Join(t.TempDir(), "autogora"), ClineApprovalDir: t.TempDir()}
			command, err := BuildRunnerCommand(claim, options, "")
			if err != nil {
				t.Fatal(err)
			}
			if command.Env["AUTOGORA_TASK_ID"] != claim.Task.Task.ID || command.Env["AUTOGORA_CLAIM_TOKEN"] != claim.ClaimToken {
				t.Fatalf("scope missing: %#v", command.Env)
			}
			if strings.Contains(strings.Join(command.Args, " "), claim.ClaimToken) {
				t.Fatal("claim token leaked into argv")
			}
			joined := strings.Join(command.Args, " ")
			if countArg(command.Args, "--model") != 0 {
				t.Fatalf("unpinned model unexpectedly added a model flag: %#v", command.Args)
			}
			switch runtime {
			case model.RuntimeCline:
				if command.ToolApproval == nil || !strings.Contains(joined, "--auto-approve false") || strings.Contains(joined, "mcpServers") || !strings.Contains(joined, "scoped Autogora CLI bridge") || !strings.Contains(joined, "$AUTOGORA_TASK_ID") {
					t.Fatalf("invalid Cline command: %#v", command)
				}
			case model.RuntimeGemini:
				if command.PolicyFile == nil || !strings.Contains(joined, "--approval-mode default") || !strings.Contains(command.PolicyFile.Content, `toolName = "mcp_*"`) || !strings.Contains(joined, "$AUTOGORA_TASK_ID") {
					t.Fatalf("invalid Gemini command: %#v", command)
				}
			case model.RuntimeClaude:
				if !strings.Contains(joined, "dontAsk") || !strings.Contains(joined, "mcp__autogora__autogora_complete") {
					t.Fatalf("invalid Claude command: %#v", command)
				}
			case model.RuntimeCodex:
				if !strings.Contains(joined, "read-only") || !strings.Contains(joined, "mcp_servers.autogora.required=true") {
					t.Fatalf("invalid Codex command: %#v", command)
				}
			}
		})
	}
}

func TestWriteOptInAndGoalContinuations(t *testing.T) {
	gemini := claimedTask(t, model.RuntimeGemini)
	options := RunnerOptions{DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "autogora"), AllowWrites: true}
	command, err := BuildRunnerCommand(gemini, options, "")
	if err != nil || command.PolicyFile != nil || !strings.Contains(strings.Join(command.Args, " "), "--approval-mode yolo") {
		t.Fatalf("write opt-in failed: %#v, %v", command, err)
	}
	codex := claimedTask(t, model.RuntimeCodex)
	options.Model = "gpt-test"
	continued, err := BuildGoalContinuationCommand(codex, options, "session-1", "continue")
	if err != nil || strings.Join(continued.Args[:4], " ") != "exec --sandbox workspace-write resume" || !strings.Contains(strings.Join(continued.Args, " "), "session-1") || !hasArgPair(continued.Args, "--model", "gpt-test") || countArg(continued.Args, "-c") != 3 {
		t.Fatalf("Codex continuation failed: %#v, %v", continued, err)
	}
	cline := claimedTask(t, model.RuntimeCline)
	continued, err = BuildGoalContinuationCommand(cline, options, "", "finish the gap")
	if err != nil || !strings.Contains(strings.Join(continued.Args, " "), "Continuation focus: finish the gap") {
		t.Fatalf("Cline continuation failed: %#v, %v", continued, err)
	}
}

func TestCodexGoalContinuationPreservesExecutionPolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		allowWrites bool
		sandbox     string
	}{
		{name: "read only", sandbox: "read-only"},
		{name: "workspace writes", allowWrites: true, sandbox: "workspace-write"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim := claimedTask(t, model.RuntimeCodex)
			options := RunnerOptions{
				DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "autogora"),
				AllowWrites: test.allowWrites, Model: "gpt-test",
			}
			initial, err := BuildRunnerCommand(claim, options, "")
			if err != nil {
				t.Fatal(err)
			}
			continued, err := BuildGoalContinuationCommand(claim, options, "session-1", "continue")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(continued.Args[:4], " "); got != "exec --sandbox "+test.sandbox+" resume" {
				t.Fatalf("continuation sandbox = %q, args = %#v", got, continued.Args)
			}
			if got, want := argPairValues(continued.Args, "-c"), argPairValues(initial.Args, "-c"); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("continuation MCP config = %#v, want %#v", got, want)
			}
			if !hasArgPair(continued.Args, "--model", "gpt-test") {
				t.Fatalf("continuation model missing from %#v", continued.Args)
			}
		})
	}
}

func TestRunnerCommandsPassConfiguredModelAndClineProvider(t *testing.T) {
	tests := []struct {
		runtime  model.Runtime
		model    string
		provider string
	}{
		{runtime: model.RuntimeCodex, model: "gpt-test"},
		{runtime: model.RuntimeClaude, model: "claude-test"},
		{runtime: model.RuntimeCline, model: "cline-test", provider: "openrouter"},
		{runtime: model.RuntimeGemini, model: "gemini-test"},
	}
	for _, test := range tests {
		t.Run(string(test.runtime), func(t *testing.T) {
			claim := claimedTask(t, test.runtime)
			command, err := BuildRunnerCommand(claim, RunnerOptions{
				DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "autogora"),
				AllowWrites: true, Model: test.model, Provider: test.provider,
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			if !hasArgPair(command.Args, "--model", test.model) {
				t.Fatalf("model flag missing from %#v", command.Args)
			}
			if test.runtime == model.RuntimeCline && !hasArgPair(command.Args, "--provider", test.provider) {
				t.Fatalf("provider flag missing from %#v", command.Args)
			}
		})
	}
}

func TestSessionIDExtraction(t *testing.T) {
	output := "noise\n{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n"
	if SessionIDFromOutput(output) != "thread-1" {
		t.Fatal("session id was not extracted")
	}
}
