package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func TestRecordOrchestrationAgentSelection(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	task, err := opened.CreateTask(ctx, CreateTaskInput{Title: "Judge this"})
	if err != nil {
		t.Fatal(err)
	}
	primary := "codex-primary"
	err = opened.RecordOrchestrationAgentSelection(ctx, task.Task.ID, RecordOrchestrationAgentSelectionInput{
		Kind: "goal_judge", Role: "judge", Profile: "claude-backup", Runtime: model.RuntimeClaude,
		Model: "claude-opus", Provider: "anthropic", Source: "global_fallback", FallbackFrom: &primary, Attempt: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := opened.ListEvents(ctx, EventFilter{TaskID: task.Task.ID, Kinds: []string{"orchestration_agent_selected"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("selection events = %#v", events)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["profile"] != "claude-backup" || payload["model"] != "claude-opus" || payload["provider"] != "anthropic" || payload["fallbackFrom"] != primary || payload["attempt"] != float64(2) {
		t.Fatalf("selection payload = %#v", payload)
	}
}
