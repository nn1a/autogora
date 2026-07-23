package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type runnerCall struct {
	Directory string
	File      string
	Args      []string
}

type fakeRunner struct {
	missing map[string]bool
	run     func(file string, args []string) (CommandOutput, error)
	calls   []runnerCall
}

func TestExecRunnerBoundsEachOutputStreamWhileProcessRuns(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("shell fixture is Unix-only")
	}
	output, err := (ExecRunner{}).RunBounded(
		context.Background(), t.TempDir(), "sh", 17, 23,
		"-c", "printf '1234567890123456789012345'; printf 'abcdefghijklmnopqrstuvwxyz' >&2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Stdout) != 17 || len(output.Stderr) != 23 {
		t.Fatalf("bounded output lengths = stdout:%d stderr:%d", len(output.Stdout), len(output.Stderr))
	}
}

func (runner *fakeRunner) LookPath(file string) (string, error) {
	if runner.missing[file] {
		return "", errors.New("not found")
	}
	return "/clients/" + file, nil
}

func (runner *fakeRunner) Run(_ context.Context, directory, file string, args ...string) (CommandOutput, error) {
	runner.calls = append(runner.calls, runnerCall{Directory: directory, File: file, Args: append([]string{}, args...)})
	if runner.run == nil {
		return CommandOutput{}, nil
	}
	return runner.run(file, args)
}

func TestMCPRegisterDryRunUsesClientSafeDefaultScopes(t *testing.T) {
	runner := &fakeRunner{run: missingMCP}
	options := MCPOptions{
		Clients: []string{"all"}, BinaryPath: "/opt/autogora", DBPath: "/work/data/autogora.db",
		ProjectRoot: t.TempDir(), DryRun: true, Runner: runner,
	}
	results, err := RegisterMCP(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %#v", results)
	}
	scopes := map[string]string{}
	for _, result := range results {
		scopes[result.Client] = result.Scope
		if !result.Changed || result.State != "registered" || result.Message != "would register" {
			t.Fatalf("dry-run result = %#v", result)
		}
		if !slices.Equal(result.Command, []string{"/opt/autogora", "serve", "--db", "/work/data/autogora.db"}) {
			t.Fatalf("expected command = %#v", result.Command)
		}
	}
	if scopes["codex"] != "user" || scopes["claude"] != "local" || scopes["gemini"] != "project" {
		t.Fatalf("default scopes = %#v", scopes)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("dry-run should only inspect clients: %#v", runner.calls)
	}
}

func TestMCPRegisterInvokesNativeClientCommands(t *testing.T) {
	runner := &fakeRunner{run: missingMCP}
	root := t.TempDir()
	options := MCPOptions{Clients: []string{"all"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: root, Runner: runner}
	if _, err := RegisterMCP(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	assertCall(t, runner.calls, "/clients/claude", []string{"mcp", "add", "--scope", "local", "autogora", "--", "/opt/autogora", "serve", "--db", "/work/autogora.db"})
	assertCall(t, runner.calls, "/clients/codex", []string{"mcp", "add", "autogora", "--", "/opt/autogora", "serve", "--db", "/work/autogora.db"})
	assertCall(t, runner.calls, "/clients/gemini", []string{"mcp", "add", "--scope", "project", "autogora", "/opt/autogora", "serve", "--", "--db", "/work/autogora.db"})
	for _, call := range runner.calls {
		if call.Directory != root {
			t.Fatalf("command directory = %q, want %q", call.Directory, root)
		}
	}
}

func TestMCPStatusRecognizesMatchingRegistrations(t *testing.T) {
	runner := &fakeRunner{run: func(file string, args []string) (CommandOutput, error) {
		switch filepath.Base(file) {
		case "codex":
			return CommandOutput{Stdout: `{"transport":{"type":"stdio","command":"/opt/autogora","args":["serve","--db","/work/autogora.db"]}}`}, nil
		case "claude":
			return CommandOutput{Stdout: "autogora:\n  Scope: User config\n  Command: /opt/autogora\n  Args: serve --db /work/autogora.db\n"}, nil
		case "gemini":
			return CommandOutput{Stdout: "✓ autogora: /opt/autogora serve --db /work/autogora.db (stdio) - Connected\n"}, nil
		}
		return CommandOutput{}, nil
	}}
	results, err := MCPStatus(context.Background(), MCPOptions{Clients: []string{"all"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.State != "registered" || result.Changed {
			t.Fatalf("status result = %#v", result)
		}
	}
}

func TestMCPRegisterProtectsConflictsAndReplaceRemovesFirst(t *testing.T) {
	runner := &fakeRunner{run: func(_ string, args []string) (CommandOutput, error) {
		if slices.Equal(args, []string{"mcp", "get", "autogora", "--json"}) {
			return CommandOutput{Stdout: `{"transport":{"type":"stdio","command":"/old/autogora","args":["serve","--db","/old.db"]}}`}, nil
		}
		return CommandOutput{}, nil
	}}
	options := MCPOptions{Clients: []string{"codex"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner}
	if _, err := RegisterMCP(context.Background(), options); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("conflict error = %v", err)
	}
	options.Replace = true
	runner.calls = nil
	if _, err := RegisterMCP(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 || !slices.Equal(runner.calls[2].Args, []string{"mcp", "remove", "autogora"}) || runner.calls[3].Args[1] != "add" {
		t.Fatalf("replace calls = %#v", runner.calls)
	}
}

func TestMCPUnregisterAndMissingClientStatus(t *testing.T) {
	runner := &fakeRunner{missing: map[string]bool{"gemini": true}, run: func(_ string, args []string) (CommandOutput, error) {
		if len(args) > 1 && args[1] == "get" {
			return CommandOutput{Stdout: "autogora:\n  Scope: Local config\n  Command: /opt/autogora\n  Args: serve --db /work/autogora.db\n"}, nil
		}
		return CommandOutput{}, nil
	}}
	options := MCPOptions{Clients: []string{"claude"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner}
	removed, err := UnregisterMCP(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !removed[0].Changed || removed[0].State != "missing" {
		t.Fatalf("unregister result = %#v", removed[0])
	}
	assertCall(t, runner.calls, "/clients/claude", []string{"mcp", "remove", "autogora", "--scope", "local"})

	status, err := MCPStatus(context.Background(), MCPOptions{Clients: []string{"gemini"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if status[0].State != "client_missing" {
		t.Fatalf("missing client status = %#v", status[0])
	}
}

func TestMCPRejectsDisabledClineAndUnsupportedScope(t *testing.T) {
	options := MCPOptions{Clients: []string{"cline"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: &fakeRunner{}}
	if _, err := RegisterMCP(context.Background(), options); err == nil || !strings.Contains(err.Error(), "MCP disabled") {
		t.Fatalf("Cline error = %v", err)
	}
	options.Clients = []string{"codex"}
	options.Scope = "project"
	if _, err := RegisterMCP(context.Background(), options); err == nil || !strings.Contains(err.Error(), "user scope") {
		t.Fatalf("Codex scope error = %v", err)
	}
}

func TestMCPMultiClientPreflightPreventsPartialRegistration(t *testing.T) {
	runner := &fakeRunner{missing: map[string]bool{"gemini": true}, run: missingMCP}
	options := MCPOptions{Clients: []string{"all"}, BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner}
	if _, err := RegisterMCP(context.Background(), options); err == nil || !strings.Contains(err.Error(), "gemini CLI") {
		t.Fatalf("missing client error = %v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 1 && (call.Args[1] == "add" || call.Args[1] == "remove") {
			t.Fatalf("mutation occurred before preflight completed: %#v", runner.calls)
		}
	}
}

func TestClaudeExplicitScopeIsPartOfRegistrationIdentity(t *testing.T) {
	runner := &fakeRunner{run: func(_ string, args []string) (CommandOutput, error) {
		if len(args) > 1 && args[1] == "get" {
			return CommandOutput{Stdout: "autogora:\n  Scope: User config\n  Command: /opt/autogora\n  Args: serve --db /work/autogora.db\n"}, nil
		}
		return CommandOutput{}, nil
	}}
	options := MCPOptions{Clients: []string{"claude"}, Scope: "project", BinaryPath: "/opt/autogora", DBPath: "/work/autogora.db", ProjectRoot: t.TempDir(), Runner: runner}
	if _, err := RegisterMCP(context.Background(), options); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("scope conflict error = %v", err)
	}
	options.Replace = true
	runner.calls = nil
	results, err := RegisterMCP(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Scope != "project" {
		t.Fatalf("replacement scope = %#v", results[0])
	}
	assertCall(t, runner.calls, "/clients/claude", []string{"mcp", "remove", "autogora", "--scope", "user"})
}

func missingMCP(file string, args []string) (CommandOutput, error) {
	if len(args) > 1 && args[1] == "list" && filepath.Base(file) == "gemini" {
		return CommandOutput{Stdout: "No MCP servers configured.\n"}, nil
	}
	if len(args) > 1 && args[1] == "get" {
		return CommandOutput{Stderr: "No MCP server named 'autogora' found.\n"}, errors.New("exit 1")
	}
	return CommandOutput{}, nil
}

func assertCall(t *testing.T, calls []runnerCall, file string, args []string) {
	t.Helper()
	for _, call := range calls {
		if call.File == file && slices.Equal(call.Args, args) {
			return
		}
	}
	t.Fatalf("missing call %s %#v in %#v", file, args, calls)
}
