package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type PlannerKind string

const (
	PlannerSpecify         PlannerKind = "specify"
	PlannerDecompose       PlannerKind = "decompose"
	PlannerGoalJudge       PlannerKind = "goal_judge"
	PlannerProfileDescribe PlannerKind = "profile_describe"
)

type PlannerRequest struct {
	TaskID string
	Kind   PlannerKind
	Prompt string
	Schema map[string]any
}

type Planner func(context.Context, PlannerRequest) (any, error)

type CLIPlannerOptions struct {
	Runtime  model.Runtime
	Command  string
	Model    string
	Provider string
	CWD      string
	Timeout  time.Duration
	Getenv   func(string) string
}

const plannerOutputLimit = 2 * 1024 * 1024

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = w.buffer.Write(value)
	}
	return written, nil
}

func (w *limitedBuffer) String() string { return w.buffer.String() }

func plannerBinary(getenv func(string) string, runtime model.Runtime, configured string) string {
	name := "AUTOGORA_" + strings.ToUpper(string(runtime)) + "_BIN"
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return string(runtime)
}

func runPlannerProcess(ctx context.Context, command string, args []string, cwd string, timeout time.Duration) (string, string, error) {
	if timeout < time.Second {
		timeout = time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = plannerOutputLimit, plannerOutputLimit
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if parentErr := ctx.Err(); parentErr != nil {
			return stdout.String(), stderr.String(), parentErr
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return stdout.String(), stderr.String(), &PlannerFailure{Kind: PlannerFailureTimeout, Err: fmt.Errorf("planner timed out after %s", timeout)}
		}
		message := stderr.String()
		if len(message) > 2000 {
			message = message[len(message)-2000:]
		}
		processErr := fmt.Errorf("planner failed: %w: %s", err, strings.TrimSpace(message))
		if kind, available := ClassifyPlannerFailure(processErr); available {
			return stdout.String(), stderr.String(), &PlannerFailure{Kind: kind, Err: processErr}
		}
		return stdout.String(), stderr.String(), processErr
	}
	return stdout.String(), stderr.String(), nil
}

func decodeJSONObject(text string) (any, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "json"))
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "```"))
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err == nil {
		return value, nil
	}
	start, end := strings.IndexByte(trimmed, '{'), strings.LastIndexByte(trimmed, '}')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(trimmed[start:end+1]), &value); err == nil {
			return value, nil
		}
	}
	return nil, errors.New("planner did not return a JSON object")
}

func unwrapPlannerOutput(value any) any {
	record, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if nested, exists := record["structured_output"]; exists {
		return nested
	}
	if nested, exists := record["structuredOutput"]; exists {
		return nested
	}
	if nested, exists := record["result"]; exists {
		if text, ok := nested.(string); ok {
			if parsed, err := decodeJSONObject(text); err == nil {
				return parsed
			}
		}
		return nested
	}
	return value
}

func clineOutput(stdout string) (any, error) {
	lines := strings.Split(stdout, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event["type"] == "run_result" {
			if text, ok := event["text"].(string); ok {
				if value, err := decodeJSONObject(text); err == nil {
					return value, nil
				}
			}
		}
		if event["type"] == "agent_event" {
			if nested, ok := event["event"].(map[string]any); ok && nested["type"] == "done" {
				if text, ok := nested["text"].(string); ok {
					if value, err := decodeJSONObject(text); err == nil {
						return value, nil
					}
				}
			}
		}
	}
	return decodeJSONObject(stdout)
}

func CreateCLIPlanner(options CLIPlannerOptions) (Planner, error) {
	if options.Runtime == model.RuntimeManual || !model.ValidRuntime(options.Runtime) {
		return nil, fmt.Errorf("invalid planner runtime: %s", options.Runtime)
	}
	cwd := options.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	resolved, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("inspect planner working directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("planner working directory is not a directory: %s", resolved)
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	prefix := "AUTOGORA_" + strings.ToUpper(string(options.Runtime))
	if strings.TrimSpace(options.Model) == "" {
		options.Model = strings.TrimSpace(getenv(prefix + "_MODEL"))
	}
	if strings.TrimSpace(options.Provider) == "" {
		options.Provider = strings.TrimSpace(getenv(prefix + "_PROVIDER"))
	}

	return func(ctx context.Context, request PlannerRequest) (any, error) {
		directory, err := os.MkdirTemp("", "autogora-planner-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(directory)
		schema, err := json.Marshal(request.Schema)
		if err != nil {
			return nil, err
		}
		schemaPath := filepath.Join(directory, "schema.json")
		outputPath := filepath.Join(directory, "output.json")
		if err := os.WriteFile(schemaPath, schema, 0o600); err != nil {
			return nil, err
		}
		binary := plannerBinary(getenv, options.Runtime, options.Command)
		var value any
		switch options.Runtime {
		case model.RuntimeCodex:
			args := []string{
				"exec", "--ephemeral", "--color", "never", "--sandbox", "read-only",
				"--skip-git-repo-check", "-C", resolved, "--output-schema", schemaPath,
				"--output-last-message", outputPath,
			}
			if selected := strings.TrimSpace(options.Model); selected != "" {
				args = append(args, "--model", selected)
			}
			args = append(args, request.Prompt)
			_, _, err = runPlannerProcess(ctx, binary, args, resolved, timeout)
			if err == nil {
				var output []byte
				output, err = os.ReadFile(outputPath)
				if err == nil {
					err = json.Unmarshal(output, &value)
				}
			}
		case model.RuntimeCline:
			prompt := request.Prompt + "\n\nDo not call tools. Return exactly one JSON object and no prose or Markdown.\nThe JSON object must conform to this schema: " + string(schema)
			args := []string{"--json", "--auto-approve", "false", "--cwd", resolved}
			if selected := strings.TrimSpace(options.Provider); selected != "" {
				args = append(args, "--provider", selected)
			}
			if selected := strings.TrimSpace(options.Model); selected != "" {
				args = append(args, "--model", selected)
			}
			args = append(args, prompt)
			var stdout string
			stdout, _, err = runPlannerProcess(ctx, binary, args, resolved, timeout)
			if err == nil {
				value, err = clineOutput(stdout)
			}
		case model.RuntimeGemini:
			policyPath := filepath.Join(directory, "gemini-planner-policy.toml")
			err = os.WriteFile(policyPath, []byte("[[rule]]\ntoolName = \"*\"\ndecision = \"deny\"\npriority = 999\n"), 0o600)
			if err == nil {
				prompt := request.Prompt + "\n\nDo not call tools. Return exactly one JSON object and no prose or Markdown.\nThe JSON object must conform to this schema: " + string(schema)
				var stdout string
				args := []string{"--output-format", "json", "--approval-mode", "default", "--policy", policyPath, "--skip-trust", "-e", "none"}
				if selected := strings.TrimSpace(options.Model); selected != "" {
					args = append(args, "--model", selected)
				}
				args = append(args, "-p", prompt)
				stdout, _, err = runPlannerProcess(ctx, binary, args, resolved, timeout)
				if err == nil {
					var envelope map[string]any
					if err = json.Unmarshal([]byte(stdout), &envelope); err == nil {
						response, ok := envelope["response"].(string)
						if !ok {
							err = errors.New("Gemini planner response is missing JSON text")
						} else {
							value, err = decodeJSONObject(response)
						}
					}
				}
			}
		case model.RuntimeClaude:
			args := []string{"-p", request.Prompt, "--output-format", "json", "--json-schema", string(schema), "--permission-mode", "dontAsk", "--tools", "", "--no-session-persistence"}
			if selected := strings.TrimSpace(options.Model); selected != "" {
				args = append(args, "--model", selected)
			}
			var stdout string
			stdout, _, err = runPlannerProcess(ctx, binary, args, resolved, timeout)
			if err == nil {
				err = json.Unmarshal([]byte(stdout), &value)
			}
		}
		if err != nil {
			return nil, err
		}
		return unwrapPlannerOutput(value), nil
	}, nil
}
