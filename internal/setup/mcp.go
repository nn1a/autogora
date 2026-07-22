package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const mcpServerName = "autogora"

type CommandOutput struct {
	Stdout string
	Stderr string
}

type CommandRunner interface {
	LookPath(file string) (string, error)
	Run(ctx context.Context, directory, file string, args ...string) (CommandOutput, error)
}

type ExecRunner struct{}

func (ExecRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (ExecRunner) Run(ctx context.Context, directory, file string, args ...string) (CommandOutput, error) {
	command := exec.CommandContext(ctx, file, args...)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return CommandOutput{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

type MCPOptions struct {
	Clients     []string
	Scope       string
	BinaryPath  string
	DBPath      string
	ProjectRoot string
	Replace     bool
	DryRun      bool
	Runner      CommandRunner
}

type MCPResult struct {
	Client  string   `json:"client"`
	Scope   string   `json:"scope"`
	State   string   `json:"state"`
	Changed bool     `json:"changed"`
	Command []string `json:"command"`
	Message string   `json:"message,omitempty"`
}

type mcpInspection struct {
	Exists bool
	Exact  bool
	Scope  string
	Detail string
}

type codexMCPConfig struct {
	Transport struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"transport"`
}

func MCPStatus(ctx context.Context, options MCPOptions) ([]MCPResult, error) {
	return visitMCP(ctx, options, "status")
}

func RegisterMCP(ctx context.Context, options MCPOptions) ([]MCPResult, error) {
	return visitMCP(ctx, options, "register")
}

func UnregisterMCP(ctx context.Context, options MCPOptions) ([]MCPResult, error) {
	return visitMCP(ctx, options, "unregister")
}

func visitMCP(ctx context.Context, options MCPOptions, action string) ([]MCPResult, error) {
	clients, err := mcpClients(options.Clients)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.BinaryPath) == "" {
		return nil, errors.New("Autogora binary path is empty")
	}
	if strings.TrimSpace(options.DBPath) == "" {
		return nil, errors.New("Autogora database path is empty")
	}
	options.BinaryPath, err = filepath.Abs(options.BinaryPath)
	if err != nil {
		return nil, err
	}
	options.DBPath, err = filepath.Abs(options.DBPath)
	if err != nil {
		return nil, err
	}
	if options.ProjectRoot == "" {
		options.ProjectRoot = "."
	}
	options.ProjectRoot, err = filepath.Abs(options.ProjectRoot)
	if err != nil {
		return nil, err
	}
	if options.Runner == nil {
		options.Runner = ExecRunner{}
	}

	if action != "status" && !options.DryRun {
		preflight := options
		preflight.DryRun = true
		for _, client := range clients {
			if _, visitErr := visitMCPClient(ctx, preflight, client, action); visitErr != nil {
				return nil, visitErr
			}
		}
	}

	results := make([]MCPResult, 0, len(clients))
	for _, client := range clients {
		result, visitErr := visitMCPClient(ctx, options, client, action)
		if visitErr != nil {
			return results, visitErr
		}
		results = append(results, result)
	}
	return results, nil
}

func visitMCPClient(ctx context.Context, options MCPOptions, client, action string) (MCPResult, error) {
	scope, err := resolveMCPScope(client, options.Scope)
	if err != nil {
		return MCPResult{}, err
	}
	result := MCPResult{Client: client, Scope: scope, State: "missing", Command: expectedMCPCommand(options)}
	clientPath, err := options.Runner.LookPath(client)
	if err != nil {
		result.State = "client_missing"
		result.Message = fmt.Sprintf("%s CLI was not found on PATH", client)
		if action == "status" {
			return result, nil
		}
		return result, errors.New(result.Message)
	}
	inspection, err := inspectMCP(ctx, options, clientPath, client)
	if err != nil {
		return result, err
	}
	if inspection.Scope != "" {
		result.Scope = inspection.Scope
	}
	if inspection.Exact && client == "claude" && options.Scope != "" && options.Scope != "auto" && inspection.Scope != "" && inspection.Scope != scope {
		inspection.Exact = false
		inspection.Detail = fmt.Sprintf("existing Claude MCP registration uses %s scope instead of %s", inspection.Scope, scope)
	}
	if inspection.Exists {
		result.State = "conflict"
		result.Message = inspection.Detail
		if inspection.Exact {
			result.State = "registered"
			result.Message = "registration matches this Autogora binary and database"
		}
	}

	switch action {
	case "status":
		return result, nil
	case "register":
		if inspection.Exact {
			return result, nil
		}
		if inspection.Exists && !options.Replace {
			return result, fmt.Errorf("%s already has a different %s MCP registration; inspect it or retry with --replace", client, mcpServerName)
		}
		result.Scope = scope
		result.Changed = true
		result.State = "registered"
		if options.DryRun {
			result.Message = "would register"
			return result, nil
		}
		if inspection.Exists {
			removeScope := scope
			if inspection.Scope != "" {
				removeScope = inspection.Scope
			}
			if err := removeMCP(ctx, options, clientPath, client, removeScope, false); err != nil {
				return result, err
			}
		}
		if err := addMCP(ctx, options, clientPath, client, scope); err != nil {
			return result, err
		}
		result.Scope = scope
		result.Message = "registered"
		return result, nil
	case "unregister":
		if !inspection.Exists {
			result.Message = "not registered"
			return result, nil
		}
		result.Changed = true
		result.State = "missing"
		if options.DryRun {
			result.Message = "would unregister"
			return result, nil
		}
		removeScope := scope
		autoScope := options.Scope == "" || options.Scope == "auto"
		if inspection.Scope != "" {
			removeScope = inspection.Scope
			if client == "claude" {
				autoScope = false
			}
		}
		if err := removeMCP(ctx, options, clientPath, client, removeScope, autoScope); err != nil {
			return result, err
		}
		result.Message = "unregistered"
		return result, nil
	default:
		return result, fmt.Errorf("unsupported MCP action %q", action)
	}
}

func mcpClients(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("at least one --client is required")
	}
	clients := normalizeClients(values)
	if slices.Contains(clients, "all") {
		clients = []string{"claude", "codex", "gemini"}
	}
	if len(clients) == 0 {
		return nil, errors.New("at least one --client is required")
	}
	for _, client := range clients {
		if client == "cline" {
			return nil, errors.New("the configured Cline runtime has MCP disabled; use Autogora's scoped CLI bridge")
		}
		if client != "claude" && client != "codex" && client != "gemini" {
			return nil, fmt.Errorf("unsupported MCP client %q", client)
		}
	}
	sort.Strings(clients)
	return clients, nil
}

func resolveMCPScope(client, requested string) (string, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" || requested == "auto" {
		switch client {
		case "codex":
			return "user", nil
		case "claude":
			return "local", nil
		case "gemini":
			return "project", nil
		}
	}
	switch client {
	case "codex":
		if requested != "user" {
			return "", errors.New("Codex CLI registration currently supports user scope; use --scope user or omit --scope")
		}
	case "claude":
		if requested != "local" && requested != "project" && requested != "user" {
			return "", errors.New("Claude MCP scope must be local, project, or user")
		}
	case "gemini":
		if requested != "project" && requested != "user" {
			return "", errors.New("Gemini MCP scope must be project or user")
		}
	}
	return requested, nil
}

func expectedMCPCommand(options MCPOptions) []string {
	return []string{options.BinaryPath, "serve", "--db", options.DBPath}
}

func inspectMCP(ctx context.Context, options MCPOptions, clientPath, client string) (mcpInspection, error) {
	expected := expectedMCPCommand(options)
	switch client {
	case "codex":
		output, err := options.Runner.Run(ctx, options.ProjectRoot, clientPath, "mcp", "get", mcpServerName, "--json")
		if err != nil {
			if reportsMissing(output) {
				return mcpInspection{}, nil
			}
			return mcpInspection{}, commandError(client, output, err)
		}
		var config codexMCPConfig
		if err := json.Unmarshal([]byte(output.Stdout), &config); err != nil {
			return mcpInspection{}, fmt.Errorf("parse Codex MCP configuration: %w", err)
		}
		exact := config.Transport.Type == "stdio" && config.Transport.Command == expected[0] && slices.Equal(config.Transport.Args, expected[1:])
		return mcpInspection{Exists: true, Exact: exact, Scope: "user", Detail: "existing Codex MCP registration differs"}, nil
	case "claude":
		output, err := options.Runner.Run(ctx, options.ProjectRoot, clientPath, "mcp", "get", mcpServerName)
		if err != nil {
			if reportsMissing(output) {
				return mcpInspection{}, nil
			}
			return mcpInspection{}, commandError(client, output, err)
		}
		combined := output.Stdout + "\n" + output.Stderr
		scope := parseClaudeScope(combined)
		exact := strings.Contains(combined, "Command: "+expected[0]) && strings.Contains(combined, "Args: "+strings.Join(expected[1:], " "))
		return mcpInspection{Exists: true, Exact: exact, Scope: scope, Detail: "existing Claude MCP registration differs"}, nil
	case "gemini":
		output, err := options.Runner.Run(ctx, options.ProjectRoot, clientPath, "mcp", "list")
		if err != nil {
			return mcpInspection{}, commandError(client, output, err)
		}
		combined := output.Stdout + "\n" + output.Stderr
		line := findServerLine(combined, mcpServerName)
		if line == "" {
			return mcpInspection{}, nil
		}
		exact := strings.Contains(line, expected[0]) && strings.Contains(line, strings.Join(expected[1:], " "))
		return mcpInspection{Exists: true, Exact: exact, Detail: "existing Gemini MCP registration differs"}, nil
	}
	return mcpInspection{}, fmt.Errorf("unsupported MCP client %q", client)
}

func addMCP(ctx context.Context, options MCPOptions, clientPath, client, scope string) error {
	expected := expectedMCPCommand(options)
	var args []string
	switch client {
	case "codex":
		args = []string{"mcp", "add", mcpServerName, "--"}
		args = append(args, expected...)
	case "claude":
		args = []string{"mcp", "add", "--scope", scope, mcpServerName, "--"}
		args = append(args, expected...)
	case "gemini":
		args = []string{"mcp", "add", "--scope", scope, mcpServerName, expected[0], "serve", "--", "--db", expected[3]}
	}
	output, err := options.Runner.Run(ctx, options.ProjectRoot, clientPath, args...)
	if err != nil {
		return commandError(client, output, err)
	}
	return nil
}

func removeMCP(ctx context.Context, options MCPOptions, clientPath, client, scope string, autoScope bool) error {
	args := []string{"mcp", "remove", mcpServerName}
	if client == "claude" && !autoScope {
		args = append(args, "--scope", scope)
	}
	if client == "gemini" {
		args = append(args, "--scope", scope)
	}
	output, err := options.Runner.Run(ctx, options.ProjectRoot, clientPath, args...)
	if err != nil {
		return commandError(client, output, err)
	}
	return nil
}

func reportsMissing(output CommandOutput) bool {
	combined := strings.ToLower(output.Stdout + "\n" + output.Stderr)
	return strings.Contains(combined, "no mcp server") || strings.Contains(combined, "not found") || strings.Contains(combined, "no server named")
}

func commandError(client string, output CommandOutput, err error) error {
	detail := strings.TrimSpace(output.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(output.Stdout)
	}
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("%s MCP command failed: %s", client, detail)
}

func parseClaudeScope(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Scope:") {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "local"):
			return "local"
		case strings.Contains(lower, "project"):
			return "project"
		case strings.Contains(lower, "user"):
			return "user"
		}
	}
	return ""
}

func findServerLine(output, name string) string {
	name = strings.ToLower(name)
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(lower, name+":") || strings.Contains(lower, name+" ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
