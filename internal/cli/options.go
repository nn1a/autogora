package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

type options struct {
	positionals []string
	values      map[string][]string
	flags       map[string]bool
	flagSet     map[string]bool
}

var booleanOptions = map[string]bool{
	"all": true, "archived": true, "archive": true, "delete": true,
	"switch": true, "triage": true, "goal": true, "mine": true,
	"follow": true, "clear-secret": true, "once": true, "watch": true,
	"force": true, "dry-run": true, "allow-writes": true, "auto-decompose": true,
	"replace": true, "reset-data-dir": true, "save": true, "auto-start": true,
	"apply": true, "autopilot": true,
}

func parseOptions(args []string) (options, error) {
	result := options{positionals: []string{}, values: map[string][]string{}, flags: map[string]bool{}, flagSet: map[string]bool{}}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			result.positionals = append(result.positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "--") {
			result.positionals = append(result.positionals, argument)
			continue
		}
		nameValue := strings.TrimPrefix(argument, "--")
		if nameValue == "" {
			return options{}, errors.New("invalid empty option")
		}
		name, value, hasValue := nameValue, "", false
		if split := strings.IndexByte(nameValue, '='); split >= 0 {
			name, value, hasValue = nameValue[:split], nameValue[split+1:], true
		}
		if booleanOptions[name] {
			result.flagSet[name] = true
			if hasValue {
				parsed, err := strconv.ParseBool(value)
				if err != nil {
					return options{}, fmt.Errorf("--%s expects true or false", name)
				}
				result.flags[name] = parsed
			} else {
				result.flags[name] = true
			}
			continue
		}
		if !hasValue {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return options{}, fmt.Errorf("--%s requires a value", name)
			}
			index++
			value = args[index]
		}
		result.values[name] = append(result.values[name], value)
	}
	return result, nil
}

func (o options) value(name string) string {
	values := o.values[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func (o options) present(name string) bool {
	return len(o.values[name]) > 0 || o.flagSet[name]
}

func (o options) many(name string) []string {
	return append([]string{}, o.values[name]...)
}

func numberOption(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", value)
	}
	return parsed, nil
}

func durationSeconds(value string) (int, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, errors.New("duration cannot be empty")
	}
	multiplier := 1
	last := value[len(value)-1]
	switch last {
	case 's':
		value = value[:len(value)-1]
	case 'm':
		value, multiplier = value[:len(value)-1], 60
	case 'h':
		value, multiplier = value[:len(value)-1], 60*60
	case 'd':
		value, multiplier = value[:len(value)-1], 24*60*60
	}
	amount, err := strconv.Atoi(value)
	if err != nil || amount < 1 {
		return 0, fmt.Errorf("invalid duration: %s", value)
	}
	return amount * multiplier, nil
}

func requireRuntime(value string, fallback model.Runtime) (model.Runtime, error) {
	if value == "" {
		value = string(fallback)
	}
	runtime := model.Runtime(value)
	if !model.ValidRuntime(runtime) {
		return "", fmt.Errorf("invalid runtime: %s", value)
	}
	return runtime, nil
}

func requireStatus(value string) (*model.TaskStatus, error) {
	if value == "" {
		return nil, nil
	}
	status := model.TaskStatus(value)
	if !model.ValidTaskStatus(status) {
		return nil, fmt.Errorf("invalid status: %s", value)
	}
	return &status, nil
}

func requireWorkspaceKind(value string) (model.WorkspaceKind, error) {
	kind := model.WorkspaceKind(value)
	if kind == "" || kind == model.WorkspaceScratch || kind == model.WorkspaceDir || kind == model.WorkspaceWorktree {
		return kind, nil
	}
	return "", fmt.Errorf("invalid workspace kind: %s", value)
}

func parseMetadata(value string) (map[string]any, error) {
	if value == "" {
		return nil, nil
	}
	result := map[string]any{}
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, fmt.Errorf("metadata must be a JSON object: %w", err)
	}
	return result, nil
}

func parseKinds(value string) []string {
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func parseSince(value string) (*int64, error) {
	if value == "" {
		zero := int64(0)
		return &zero, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid number: %s", value)
	}
	return &parsed, nil
}

func futureOrNil(value string) (*string, error) {
	if value == "" {
		return nil, nil
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return nil, fmt.Errorf("scheduled time must be ISO-8601: %w", err)
	}
	return &value, nil
}
