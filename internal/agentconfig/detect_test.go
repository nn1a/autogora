package agentconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestDetectSupportedAgentsUsesOnlyVersionAndReportsAvailability(t *testing.T) {
	config := Default()
	config.Agents = []Agent{{ID: "codex", Runtime: model.RuntimeCodex, Enabled: true, Roles: []Role{RoleWorker}}}
	calls := []string{}
	detections, err := DetectSupportedAgents(context.Background(), config, DetectOptions{
		LookPath: func(name string) (string, error) {
			if name == "codex" || name == "cline" {
				return "/tools/" + name, nil
			}
			return "", errors.New("missing")
		},
		RunVersion: func(_ context.Context, executable string) (string, string, error) {
			calls = append(calls, executable+" --version")
			if executable == "/tools/cline" {
				return "", "login is not relevant here", errors.New("exit 1")
			}
			return "codex 1.2.3\nextra", "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != len(model.WorkerRuntimes) || len(calls) != 2 {
		t.Fatalf("detections=%#v calls=%#v", detections, calls)
	}
	byID := map[string]Detection{}
	for _, detection := range detections {
		byID[detection.ID] = detection
	}
	if byID["codex"].State != "installed" || byID["codex"].Version != "codex 1.2.3" || !byID["codex"].Configured {
		t.Fatalf("codex detection = %#v", byID["codex"])
	}
	if byID["cline"].State != "version_unavailable" || byID["cline"].Version != "login is not relevant here" {
		t.Fatalf("cline detection = %#v", byID["cline"])
	}
	if byID["claude"].State != "missing" || byID["gemini"].State != "missing" {
		t.Fatalf("missing detections = %#v", byID)
	}
}

func TestDetectSupportedAgentsBoundsVersionTimeout(t *testing.T) {
	_, err := DetectSupportedAgents(context.Background(), Default(), DetectOptions{
		Timeout: time.Millisecond,
		LookPath: func(name string) (string, error) {
			if name == "codex" {
				return name, nil
			}
			return "", errors.New("missing")
		},
		RunVersion: func(ctx context.Context, _ string) (string, string, error) {
			<-ctx.Done()
			return "", "", ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDetectionBuffersBoundEachOutputStream(t *testing.T) {
	for _, name := range []string{"stdout", "stderr"} {
		t.Run(name, func(t *testing.T) {
			buffer := boundedBuffer{limit: MaxDetectionOutputBytes}
			value := []byte(strings.Repeat("x", MaxDetectionOutputBytes+4096))
			written, err := buffer.Write(value)
			if err != nil || written != len(value) ||
				buffer.value.Len() != MaxDetectionOutputBytes {
				t.Fatalf("bounded buffer wrote=%d stored=%d err=%v",
					written, buffer.value.Len(), err)
			}
		})
	}
}
