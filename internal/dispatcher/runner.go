package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nn1a/autogora/internal/model"
)

type ToolApproval struct {
	Directory     string
	CommandPrefix string
}

type PolicyFile struct {
	Path    string
	Content string
}

type RunnerCommand struct {
	Command      string
	Args         []string
	CWD          string
	Env          map[string]string
	ToolApproval *ToolApproval
	PolicyFile   *PolicyFile
}

type RunnerOptions struct {
	DBPath           string
	CLIPath          string
	AllowWrites      bool
	WorkspaceRoot    string
	AttachmentsRoot  string
	LogsRoot         string
	ClineApprovalDir string
	Getenv           func(string) string
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func workerPrompt(claim model.ClaimedTask, cliPath string) string {
	task := claim.Task.Task
	instructions := []string{fmt.Sprintf("You are the assigned Autogora worker for %s.", task.ID)}
	if task.Runtime == model.RuntimeCline || task.Runtime == model.RuntimeGemini {
		bridge := shellQuote(cliPath)
		if task.Runtime == model.RuntimeCline {
			instructions = append(instructions, "MCP is unavailable in this Cline build. Use only the scoped Autogora CLI bridge for task lifecycle communication.")
		} else {
			instructions = append(instructions, "Use only the scoped Autogora CLI bridge for task lifecycle communication; do not change Gemini user or project MCP settings.")
		}
		instructions = append(instructions,
			fmt.Sprintf(`First run %s show "$AUTOGORA_TASK_ID". For long work run %s heartbeat "$AUTOGORA_TASK_ID" --note "progress".`, bridge, bridge),
			"Read relationshipGraph and workerContext from show. Work only on the current node; Autogora has already enforced every prerequisite, and your completion will unlock listed dependents.",
			fmt.Sprintf(`Record handoffs with %s comment "$AUTOGORA_TASK_ID" "message".`, bridge),
			fmt.Sprintf(`Finish exactly once with %s complete "$AUTOGORA_TASK_ID" --summary "summary" or %s block "$AUTOGORA_TASK_ID" "reason" --kind needs_input.`, bridge, bridge),
			"The dispatcher scopes these commands to the active task and claim. Do not claim, create, reassign, unblock, or modify unrelated tasks.",
		)
	} else {
		instructions = append(instructions,
			"Call autogora_show first without a task_id. Work only on that task in the current workspace.",
			"Read relationshipGraph and workerContext from autogora_show. Follow the recorded dependency phase; do not implement sibling or downstream tasks.",
			"Use autogora_heartbeat for long-running work. Record durable intermediate handoffs with autogora_comment.",
			"Do not claim, create, reassign, unblock, or modify unrelated tasks.",
		)
	}
	if task.GoalMode {
		instructions = append(instructions,
			"This card is in goal mode. Call autogora_complete only when every acceptance criterion is demonstrably satisfied, or autogora_block for a real blocker.",
		)
		if task.Runtime == model.RuntimeCline {
			instructions = append(instructions, "If meaningful work remains after this turn, leave the task running and end with a concise progress handoff; an independent judge may continue the goal in a fresh Cline turn.")
		} else {
			instructions = append(instructions, "If meaningful work remains after this turn, leave the task running and end your response with a concise progress handoff; an independent judge will continue this same session.")
		}
	} else {
		instructions = append(instructions, "You must end exactly once by calling autogora_complete with verification evidence, or autogora_block with the concrete reason.")
	}
	if len(task.Skills) > 0 {
		instructions = append(instructions, "Load and follow these task-specific skills before working: "+strings.Join(task.Skills, ", ")+".")
	}
	return strings.Join(instructions, " ")
}

func resolvedPath(value string) (string, error) {
	if value == "" {
		return "", errors.New("path cannot be empty")
	}
	return filepath.Abs(value)
}

func workerBinary(options RunnerOptions, runtime model.Runtime) string {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	name := "AUTOGORA_" + strings.ToUpper(string(runtime)) + "_BIN"
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return string(runtime)
}

func commandEnvironment(claim model.ClaimedTask, options RunnerOptions, cwd, dbPath string) map[string]string {
	task, run := claim.Task.Task, claim.Run
	tenant := ""
	if task.Tenant != nil {
		tenant = *task.Tenant
	}
	return map[string]string{
		"AUTOGORA_DB": dbPath, "AUTOGORA_BOARD": task.Board, "AUTOGORA_TASK_ID": task.ID,
		"AUTOGORA_RUN_ID": run.ID, "AUTOGORA_CLAIM_TOKEN": claim.ClaimToken, "AUTOGORA_WORKER_ID": run.WorkerID,
		"AUTOGORA_TENANT": tenant, "AUTOGORA_WORKSPACE": cwd, "AUTOGORA_WORKSPACES_ROOT": options.WorkspaceRoot,
		"AUTOGORA_ATTACHMENTS_ROOT": options.AttachmentsRoot, "AUTOGORA_LOGS_ROOT": options.LogsRoot,
		"AUTOGORA_CLI": options.CLIPath,
	}
}

func BuildRunnerCommand(claim model.ClaimedTask, options RunnerOptions, sessionID string) (RunnerCommand, error) {
	task := claim.Task.Task
	cliPath, err := resolvedPath(options.CLIPath)
	if err != nil {
		return RunnerCommand{}, err
	}
	dbPath, err := resolvedPath(options.DBPath)
	if err != nil {
		return RunnerCommand{}, err
	}
	options.CLIPath = cliPath
	cwd, err := os.Getwd()
	if err != nil {
		return RunnerCommand{}, err
	}
	if claim.Workspace != nil && strings.TrimSpace(claim.Workspace.Path) != "" {
		cwd, err = filepath.Abs(claim.Workspace.Path)
	} else if task.Workspace != nil && strings.TrimSpace(*task.Workspace) != "" && task.WorkspaceKind == model.WorkspaceDir {
		cwd, err = filepath.Abs(strings.TrimPrefix(*task.Workspace, "dir:"))
		if err != nil {
			return RunnerCommand{}, err
		}
	}
	env := commandEnvironment(claim, options, cwd, dbPath)
	prompt := workerPrompt(claim, cliPath)
	serverArgs := []string{"serve", "--db", dbPath}

	switch task.Runtime {
	case model.RuntimeCodex:
		commandJSON, _ := json.Marshal(cliPath)
		argsJSON, _ := json.Marshal(serverArgs)
		sandbox := "read-only"
		if options.AllowWrites {
			sandbox = "workspace-write"
		}
		return RunnerCommand{Command: workerBinary(options, task.Runtime), CWD: cwd, Env: env, Args: []string{
			"exec", "--json", "--color", "never", "--skip-git-repo-check", "--sandbox", sandbox, "-C", cwd,
			"-c", "mcp_servers.autogora.command=" + string(commandJSON),
			"-c", "mcp_servers.autogora.args=" + string(argsJSON),
			"-c", "mcp_servers.autogora.required=true", prompt,
		}}, nil
	case model.RuntimeClaude:
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"autogora": map[string]any{"type": "stdio", "command": cliPath, "args": serverArgs}}})
		lifecycle := []string{"mcp__autogora__autogora_show", "mcp__autogora__autogora_comment", "mcp__autogora__autogora_heartbeat", "mcp__autogora__autogora_complete", "mcp__autogora__autogora_block"}
		builtins := []string{"Read", "Glob", "Grep", "WebSearch", "WebFetch", "Skill"}
		permission := "dontAsk"
		if options.AllowWrites {
			builtins, permission = []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash", "Skill"}, "acceptEdits"
		}
		args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--strict-mcp-config", "--mcp-config", string(config), "--permission-mode", permission, "--allowedTools", strings.Join(append(builtins, lifecycle...), ",")}
		if sessionID != "" {
			args = append(args, "--session-id", sessionID)
		}
		return RunnerCommand{Command: workerBinary(options, task.Runtime), CWD: cwd, Env: env, Args: args}, nil
	case model.RuntimeCline:
		if !options.AllowWrites && options.ClineApprovalDir == "" {
			return RunnerCommand{}, errors.New("read-only Cline execution requires a scoped tool approval directory")
		}
		commandPrefix := shellQuote(cliPath)
		autoApprove := "false"
		var approval *ToolApproval
		if options.AllowWrites {
			autoApprove = "true"
		} else {
			env["CLINE_TOOL_APPROVAL_MODE"] = "desktop"
			env["CLINE_TOOL_APPROVAL_DIR"] = options.ClineApprovalDir
			approval = &ToolApproval{Directory: options.ClineApprovalDir, CommandPrefix: commandPrefix}
		}
		return RunnerCommand{Command: workerBinary(options, task.Runtime), CWD: cwd, Env: env,
			Args: []string{"--json", "--auto-approve", autoApprove, "--cwd", cwd, prompt}, ToolApproval: approval}, nil
	case model.RuntimeGemini:
		approval := "default"
		if options.AllowWrites {
			approval = "yolo"
		}
		args := []string{"--output-format", "stream-json", "--approval-mode", approval, "--skip-trust", "-e", "none"}
		var policy *PolicyFile
		if !options.AllowWrites {
			logsRoot := options.LogsRoot
			if logsRoot == "" {
				logsRoot = os.TempDir()
			}
			path := filepath.Join(logsRoot, "gemini-"+claim.Run.ID+".policy.toml")
			prefixJSON, _ := json.Marshal(shellQuote(cliPath))
			content := strings.Join([]string{
				"[[rule]]", `toolName = "run_shell_command"`, `decision = "deny"`, "priority = 998", "",
				"[[rule]]", `toolName = "run_shell_command"`, "commandPrefix = " + string(prefixJSON), `decision = "allow"`, "priority = 999", "",
				"[[rule]]", `toolName = "mcp_*"`, `decision = "deny"`, "priority = 999", "",
			}, "\n")
			policy = &PolicyFile{Path: path, Content: content}
			args = append(args, "--policy", path)
		}
		args = append(args, "-p", prompt)
		return RunnerCommand{Command: workerBinary(options, task.Runtime), CWD: cwd, Env: env, Args: args, PolicyFile: policy}, nil
	default:
		return RunnerCommand{}, fmt.Errorf("dispatcher cannot launch runtime: %s", task.Runtime)
	}
}

func BuildGoalContinuationCommand(claim model.ClaimedTask, options RunnerOptions, sessionID, prompt string) (RunnerCommand, error) {
	initial, err := BuildRunnerCommand(claim, options, "")
	if err != nil {
		return RunnerCommand{}, err
	}
	switch claim.Task.Task.Runtime {
	case model.RuntimeCodex:
		if sessionID == "" {
			return RunnerCommand{}, errors.New("Codex goal continuation requires a session id")
		}
		initial.Args = []string{"exec", "resume", "--json", "--skip-git-repo-check", sessionID, prompt}
	case model.RuntimeClaude:
		if sessionID == "" {
			return RunnerCommand{}, errors.New("Claude goal continuation requires a session id")
		}
		for index := range initial.Args {
			if index > 0 && initial.Args[index-1] == "-p" {
				initial.Args[index] = prompt
				break
			}
		}
		initial.Args = append(initial.Args, "--resume", sessionID)
	case model.RuntimeCline:
		if len(initial.Args) > 0 {
			index := len(initial.Args) - 1
			initial.Args[index] += "\nContinuation focus: " + prompt
		}
	case model.RuntimeGemini:
		if sessionID == "" {
			return RunnerCommand{}, errors.New("Gemini goal continuation requires a session id")
		}
		for index := range initial.Args {
			if index > 0 && initial.Args[index-1] == "-p" {
				initial.Args[index] = prompt
				break
			}
		}
		initial.Args = append(initial.Args, "--resume", sessionID)
	default:
		return RunnerCommand{}, fmt.Errorf("goal continuation cannot launch runtime: %s", claim.Task.Task.Runtime)
	}
	return initial, nil
}

func SessionIDFromOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		candidates := []any{event["thread_id"], event["session_id"], event["sessionId"]}
		if nested, ok := event["event"].(map[string]any); ok {
			candidates = append(candidates, nested["session_id"], nested["sessionId"])
		}
		for _, candidate := range candidates {
			if value, ok := candidate.(string); ok && value != "" {
				return value
			}
		}
	}
	return ""
}
