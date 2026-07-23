package agentconfig

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

const (
	DefaultDetectionTimeout = 3 * time.Second
	MaxDetectionOutputBytes = 16 * 1024
)

type VersionRunner func(context.Context, string) (stdout string, stderr string, err error)

type DetectOptions struct {
	LookPath   func(string) (string, error)
	RunVersion VersionRunner
	Timeout    time.Duration
}

type Detection struct {
	ID         string        `json:"id"`
	Runtime    model.Runtime `json:"runtime"`
	Executable string        `json:"executable,omitempty"`
	Version    string        `json:"version,omitempty"`
	State      string        `json:"state"`
	Configured bool          `json:"configured"`
	Message    string        `json:"message,omitempty"`
}

type boundedBuffer struct {
	value bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := b.limit - b.value.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.value.Write(value)
	}
	return written, nil
}

func defaultVersionRunner(ctx context.Context, executable string) (string, string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	stdout := boundedBuffer{limit: MaxDetectionOutputBytes}
	stderr := boundedBuffer{limit: MaxDetectionOutputBytes}
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.value.String(), stderr.value.String(), err
}

func detectedVersion(stdout, stderr string) string {
	value := strings.TrimSpace(stdout)
	if value == "" {
		value = strings.TrimSpace(stderr)
	}
	if newline := strings.IndexByte(value, '\n'); newline >= 0 {
		value = value[:newline]
	}
	runes := []rune(value)
	if len(runes) > 500 {
		value = string(runes[:500])
	}
	return value
}

// DetectSupportedAgents only resolves executables and invokes --version. It
// never sends a prompt, checks authentication, or calls a paid model API.
func DetectSupportedAgents(ctx context.Context, config Config, options DetectOptions) ([]Detection, error) {
	lookPath := options.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	runVersion := options.RunVersion
	if runVersion == nil {
		runVersion = defaultVersionRunner
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultDetectionTimeout
	}
	configured := make(map[string]bool, len(config.Agents))
	for _, agent := range config.Agents {
		configured[agent.ID] = true
	}
	result := make([]Detection, 0, len(model.WorkerRuntimes))
	for _, runtime := range model.WorkerRuntimes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := string(runtime)
		detection := Detection{ID: id, Runtime: runtime, State: "missing", Configured: configured[id]}
		executable, err := lookPath(id)
		if err != nil {
			detection.Message = "CLI was not found on PATH"
			result = append(result, detection)
			continue
		}
		detection.Executable, detection.State = executable, "installed"
		versionContext, cancel := context.WithTimeout(ctx, timeout)
		stdout, stderr, versionErr := runVersion(versionContext, executable)
		timedOut := errors.Is(versionContext.Err(), context.DeadlineExceeded)
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		detection.Version = detectedVersion(stdout, stderr)
		if versionErr != nil {
			detection.State = "version_unavailable"
			if timedOut {
				detection.Message = "version check timed out"
			} else {
				detection.Message = "CLI was found, but --version failed"
			}
		}
		result = append(result, detection)
	}
	return result, nil
}
