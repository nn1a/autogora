package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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

func TestWorkerPromptDescribesDistinctWorkflowRoleBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		role      model.WorkflowRole
		required  []string
		forbidden []string
	}{
		{
			name: "worker",
			role: model.WorkflowRoleWorker,
			required: []string{
				"assigned Autogora worker",
				"Work only on that task in the current workspace",
			},
			forbidden: []string{
				"assigned Autogora reviewer",
				"independent review task",
				"explicit final integration task",
			},
		},
		{
			name: "reviewer",
			role: model.WorkflowRoleReviewer,
			required: []string{
				"assigned Autogora reviewer",
				"independent review task",
				"Do not implement fixes, modify product source, or take over prerequisite work",
				"prerequisite handoff",
				"acceptance criteria",
				"relevant tests",
				"clear pass or fail decision with concrete evidence",
				"instead of fixing the implementation yourself",
			},
			forbidden: []string{
				"assigned Autogora worker",
				"explicit final integration task",
			},
		},
		{
			name: "finalizer",
			role: model.WorkflowRoleFinalizer,
			required: []string{
				"assigned Autogora finalizer",
				"explicit final integration task",
				"Preserve every prerequisite change set in Git history",
			},
			forbidden: []string{
				"assigned Autogora worker",
				"assigned Autogora reviewer",
				"independent review task",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := claimedTask(t, model.RuntimeCodex)
			claim.Task.Task.WorkflowRole = test.role
			prompt := workerPrompt(claim, filepath.Join(t.TempDir(), "autogora"))
			for _, required := range test.required {
				if !strings.Contains(prompt, required) {
					t.Fatalf("%s prompt missing %q: %s", test.role, required, prompt)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(prompt, forbidden) {
					t.Fatalf("%s prompt contains forbidden boundary %q: %s", test.role, forbidden, prompt)
				}
			}
		})
	}
}

func TestWorkerPromptMakesHostOwnedGitLifecycleExplicit(t *testing.T) {
	for _, role := range []model.WorkflowRole{model.WorkflowRoleWorker, model.WorkflowRoleFinalizer} {
		t.Run(string(role), func(t *testing.T) {
			claim := claimedTask(t, model.RuntimeCodex)
			claim.Task.Task.WorkflowRole = role
			claim.Task.Task.Body = "Commit your work and push the branch."
			claim.Workspace = &model.RunWorkspace{Kind: model.WorkspaceWorktree}

			prompt := workerPrompt(claim, filepath.Join(t.TempDir(), "autogora"))
			for _, required := range []string{
				"Autogora owns the Git lifecycle",
				"Do not create, amend, rebase, reset, or push commits",
				"tracked and untracked deliverable files",
				"captures them into an immutable change set",
				"task-body request to commit or push does not override",
			} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("%s prompt missing %q:\n%s", role, required, prompt)
				}
			}
		})
	}

	directoryClaim := claimedTask(t, model.RuntimeCodex)
	directoryClaim.Workspace = &model.RunWorkspace{Kind: model.WorkspaceDir}
	if prompt := workerPrompt(directoryClaim, filepath.Join(t.TempDir(), "autogora")); strings.Contains(prompt, "Autogora owns the Git lifecycle") {
		t.Fatalf("plain directory workspace received managed-worktree contract:\n%s", prompt)
	}
}

func TestIntegrationResolverKeepsItsBoundedGitException(t *testing.T) {
	claim := claimedTask(t, model.RuntimeCodex)
	claim.Task.Task.WorkflowRole = model.WorkflowRoleFinalizer
	claim.IntegrationResolution = &model.IntegrationResolution{
		Attempt: 1, MaxAttempts: 2, ManifestPath: "/tmp/manifest.json",
		ManifestSHA256: strings.Repeat("a", 64), TargetCount: 2,
		ConflictingFileCount: 1,
	}

	prompt := workerPrompt(claim, filepath.Join(t.TempDir(), "autogora"))
	if !strings.Contains(prompt, "finish its in-progress merge") {
		t.Fatalf("integration resolver prompt lost merge contract:\n%s", prompt)
	}
	if strings.Contains(prompt, "Autogora owns the Git lifecycle") {
		t.Fatalf("integration resolver received ordinary managed-worktree restriction:\n%s", prompt)
	}
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
				if !strings.Contains(joined, "read-only") ||
					!strings.Contains(joined, "mcp_servers.autogora.required=true") ||
					!strings.Contains(joined, `mcp_servers.autogora.env_vars=["AUTOGORA_BOARD","AUTOGORA_TASK_ID","AUTOGORA_RUN_ID","AUTOGORA_CLAIM_TOKEN"]`) ||
					!strings.Contains(joined, `mcp_servers.autogora.enabled_tools=["autogora_show","autogora_comment","autogora_heartbeat","autogora_complete","autogora_block"]`) ||
					!strings.Contains(joined, `mcp_servers.autogora.tools.autogora_complete.approval_mode="approve"`) ||
					!strings.Contains(joined, `mcp_servers.autogora.tools.autogora_block.approval_mode="approve"`) {
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
	if err != nil || strings.Join(continued.Args[:4], " ") != "exec --sandbox workspace-write resume" || !strings.Contains(strings.Join(continued.Args, " "), "session-1") || !hasArgPair(continued.Args, "--model", "gpt-test") || countArg(continued.Args, "-c") != 7 {
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

func TestRunnerCommandUsesRegisteredExecutableWithEnvironmentOverride(t *testing.T) {
	claim := claimedTask(t, model.RuntimeCodex)
	options := RunnerOptions{
		DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "autogora"),
		Command: "/opt/autogora/codex-primary", Getenv: func(string) string { return "" },
	}
	command, err := BuildRunnerCommand(claim, options, "")
	if err != nil {
		t.Fatal(err)
	}
	if command.Command != options.Command {
		t.Fatalf("registered command = %q, want %q", command.Command, options.Command)
	}
	options.Getenv = func(name string) string {
		if name == "AUTOGORA_CODEX_BIN" {
			return "/tmp/test-codex"
		}
		return ""
	}
	command, err = BuildRunnerCommand(claim, options, "")
	if err != nil || command.Command != "/tmp/test-codex" {
		t.Fatalf("environment command override = %#v, %v", command, err)
	}
}

func finalizerResolutionRunnerFixture(t *testing.T, targetCount int) (model.ClaimedTask, RunnerOptions) {
	t.Helper()
	claim := claimedTask(t, model.RuntimeCline)
	claim.Task.Task.WorkflowRole = model.WorkflowRoleFinalizer
	repository := gitRepositoryFixture(t)
	worktree := filepath.Join(t.TempDir(), "finalizer")
	finalizerGit(t, repository, "worktree", "add", "-q", "--detach", worktree, "HEAD")
	base := finalizerGit(t, worktree, "rev-parse", "HEAD^{commit}")
	repositoryPath := repository
	claim.Workspace = &model.RunWorkspace{
		RunID: claim.Run.ID, TaskID: claim.Task.Task.ID, Path: worktree,
		Kind: model.WorkspaceWorktree, RepositoryPath: &repositoryPath,
		BaseCommit: &base, Generated: true,
	}
	targets := make([]model.IntegrationResolutionTarget, 0, targetCount)
	for index := range targetCount {
		targets = append(targets, model.IntegrationResolutionTarget{
			PrerequisiteID: "parent-" + strings.Repeat("x", 4) + fmt.Sprint(index),
			ChangeSetID:    "changeset-" + fmt.Sprint(index), HeadCommit: base,
			DurableRef:      "refs/autogora/runs/parent-" + fmt.Sprint(index),
			MergeInProgress: index == 0,
		})
	}
	fingerprint := strings.Repeat("f", 64)
	common := finalizerGit(t, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	manifestPath := filepath.Join(common, "autogora", "integration-resolutions", claim.Run.ID+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := model.IntegrationResolutionManifest{
		Version: model.IntegrationResolutionManifestVersion,
		TaskID:  claim.Task.Task.ID, RunID: claim.Run.ID,
		ConflictFingerprint: fingerprint, WorkspacePath: worktree,
		Targets: targets, ConflictingFiles: []string{"README.md"},
		ConflictingFileCount: 1,
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	claim.IntegrationResolution = &model.IntegrationResolution{
		Attempt: 2, MaxAttempts: 3, ConflictFingerprint: fingerprint,
		WorkspacePath: worktree, ManifestPath: manifestPath,
		ManifestSHA256:       hex.EncodeToString(digest[:]),
		ConflictingFileCount: 1, TargetCount: len(targets), Targets: targets,
	}
	options := RunnerOptions{
		DBPath: filepath.Join(t.TempDir(), "db"), CLIPath: filepath.Join(t.TempDir(), "autogora"),
		AllowWrites: true,
	}
	return claim, options
}

func TestFinalizerResolutionPromptCarriesBoundedHostValidatedHandoff(t *testing.T) {
	claim, options := finalizerResolutionRunnerFixture(t, 1)
	command, err := BuildRunnerCommand(claim, options, "")
	if err != nil {
		t.Fatal(err)
	}
	prompt := strings.Join(command.Args, " ")
	for _, required := range []string{
		"assigned Autogora finalizer", "attempt 2 of 3", "real Git merge conflict",
		"immutable host-authored handoff manifest",
		"finish its in-progress merge", "final history must retain every target head",
		"host will reject an unresolved index",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("finalizer prompt missing %q: %s", required, prompt)
		}
	}
	if command.Env["AUTOGORA_INTEGRATION_RESOLUTION_MANIFEST"] != claim.IntegrationResolution.ManifestPath ||
		command.Env["AUTOGORA_INTEGRATION_RESOLUTION_SHA256"] != claim.IntegrationResolution.ManifestSHA256 ||
		command.Env["AUTOGORA_INTEGRATION_RESOLUTION"] != "" {
		t.Fatalf("bounded resolution environment = %#v", command.Env)
	}
}

func TestFinalizerResolutionRunnerRejectsForgedTransportState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.ClaimedTask, *RunnerOptions)
		want   string
	}{
		{name: "ordinary role", mutate: func(claim *model.ClaimedTask, _ *RunnerOptions) {
			claim.Task.Task.WorkflowRole = model.WorkflowRoleWorker
		}, want: "requires a finalizer"},
		{name: "read only", mutate: func(_ *model.ClaimedTask, options *RunnerOptions) {
			options.AllowWrites = false
		}, want: "requires workspace writes"},
		{name: "shared workspace", mutate: func(claim *model.ClaimedTask, _ *RunnerOptions) {
			claim.Workspace.Generated = false
		}, want: "generated isolated worktree"},
		{name: "workspace mismatch", mutate: func(claim *model.ClaimedTask, _ *RunnerOptions) {
			claim.IntegrationResolution.WorkspacePath = t.TempDir()
		}, want: "does not match"},
		{name: "digest mismatch", mutate: func(claim *model.ClaimedTask, _ *RunnerOptions) {
			claim.IntegrationResolution.ManifestSHA256 = strings.Repeat("0", 64)
		}, want: "digest mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim, options := finalizerResolutionRunnerFixture(t, 1)
			test.mutate(&claim, &options)
			if _, err := BuildRunnerCommand(claim, options, ""); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildRunnerCommand error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFinalizerResolutionLargeFanInDoesNotExpandArgvOrEnvironment(t *testing.T) {
	claim, options := finalizerResolutionRunnerFixture(t, 5000)
	command, err := BuildRunnerCommand(claim, options, "")
	if err != nil {
		t.Fatal(err)
	}
	transportSize := len(strings.Join(command.Args, "\x00"))
	for key, value := range command.Env {
		transportSize += len(key) + len(value) + 1
	}
	if transportSize > 16*1024 {
		t.Fatalf("large fan-in transport grew to %d bytes", transportSize)
	}
	if strings.Contains(strings.Join(command.Args, " "), "changeset-4999") {
		t.Fatal("target details leaked into argv")
	}
}

func TestSessionIDExtraction(t *testing.T) {
	output := "noise\n{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}\n"
	if SessionIDFromOutput(output) != "thread-1" {
		t.Fatal("session id was not extracted")
	}
}
