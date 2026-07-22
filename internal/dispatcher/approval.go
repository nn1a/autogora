package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var clineReadOnlyTools = map[string]bool{
	"read_files": true, "read_file": true, "list_files": true, "list_code_definition_names": true,
	"search_codebase": true, "search_files": true, "fetch_web_content": true, "skills": true,
}

func approvalCommands(input any) []string {
	switch value := input.(type) {
	case string:
		return []string{value}
	case []any:
		result := []string{}
		for _, item := range value {
			result = append(result, approvalCommands(item)...)
		}
		return result
	case map[string]any:
		if commands, ok := value["commands"]; ok {
			return approvalCommands(commands)
		}
		if command, ok := value["command"].(string); ok {
			if arguments, ok := value["args"].([]any); ok {
				parts := []string{shellQuote(command)}
				valid := true
				for _, argument := range arguments {
					text, ok := argument.(string)
					if !ok {
						valid = false
						break
					}
					parts = append(parts, shellQuote(text))
				}
				if valid {
					return []string{strings.Join(parts, " ")}
				}
			}
			return []string{command}
		}
		if command, ok := value["cmd"].(string); ok {
			return []string{command}
		}
	}
	return nil
}

func isScopedBridgeCommand(command, prefix string) bool {
	normalized := strings.TrimSpace(command)
	if !strings.HasPrefix(normalized, prefix+" ") || strings.ContainsAny(normalized, "\n\r;|&<>`") || strings.Contains(normalized, "$(") {
		return false
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(normalized, prefix))
	fields := strings.Fields(remainder)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "show", "context", "heartbeat", "comment", "complete", "block":
		return true
	default:
		return false
	}
}

type approvalBroker struct {
	policy  ToolApproval
	handled map[string]bool
	mu      sync.Mutex
}

func (b *approvalBroker) sweep() {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, err := os.ReadDir(b.policy.Directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if b.handled[name] || !strings.Contains(name, ".request.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(b.policy.Directory, name))
		if err != nil {
			continue
		}
		request := map[string]any{}
		if json.Unmarshal(contents, &request) != nil {
			continue
		}
		toolName, _ := request["toolName"].(string)
		commands := approvalCommands(request["input"])
		approved := clineReadOnlyTools[toolName]
		if toolName == "run_commands" || toolName == "execute_command" {
			approved = len(commands) > 0
			for _, command := range commands {
				approved = approved && isScopedBridgeCommand(command, b.policy.CommandPrefix)
			}
		}
		reason := "Denied by the scoped Autogora read-only policy"
		if approved {
			reason = "Approved by the scoped Autogora read-only policy"
		}
		decision, _ := json.Marshal(map[string]any{"approved": approved, "reason": reason})
		decisionName := strings.Replace(name, ".request.", ".decision.", 1)
		if err := os.WriteFile(filepath.Join(b.policy.Directory, decisionName), append(decision, '\n'), 0o600); err == nil {
			b.handled[name] = true
		}
	}
}

func startToolApprovalBroker(ctx context.Context, policy ToolApproval) (func(), error) {
	if strings.TrimSpace(policy.Directory) == "" {
		return nil, fmt.Errorf("Cline approval directory cannot be empty")
	}
	if err := os.MkdirAll(policy.Directory, 0o700); err != nil {
		return nil, err
	}
	broker := &approvalBroker{policy: policy, handled: map[string]bool{}}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				broker.sweep()
			case <-ctx.Done():
				broker.sweep()
				return
			case <-stop:
				broker.sweep()
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop); <-done })
	}, nil
}
