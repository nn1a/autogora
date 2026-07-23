package store

import (
	"context"
	"strings"
	"testing"

	"github.com/nn1a/autogora/internal/model"
)

func claimAgentConfigTask(t *testing.T, store *Store, runtime model.Runtime) *model.ClaimedTask {
	t.Helper()
	assignee := "implementer"
	task, err := store.CreateTask(context.Background(), CreateTaskInput{
		Title: "Capture execution config", Assignee: &assignee, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimTask(context.Background(), ClaimOptions{TaskID: task.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim task: claim=%+v err=%v", claim, err)
	}
	return claim
}

func equalAgentConfig(left, right model.RunAgentConfig) bool {
	if left.RunID != right.RunID || left.TaskID != right.TaskID || left.Profile != right.Profile ||
		left.Runtime != right.Runtime || left.Model != right.Model || left.Provider != right.Provider ||
		left.Source != right.Source || left.ConfiguredAt != right.ConfiguredAt {
		return false
	}
	if left.FallbackFrom == nil || right.FallbackFrom == nil {
		return left.FallbackFrom == nil && right.FallbackFrom == nil
	}
	return *left.FallbackFrom == *right.FallbackFrom
}

func TestRecordRunAgentConfigIsImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claim := claimAgentConfigTask(t, store, model.RuntimeCodex)
	fallback := " claude-backup "
	input := RecordRunAgentConfigInput{
		Profile: " implementer ", Runtime: model.RuntimeCodex, Model: " gpt-coding ",
		Provider: " openai ", Source: " board_profile ", FallbackFrom: &fallback,
	}

	first, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !equalAgentConfig(first, second) || first.TaskID != claim.Task.Task.ID || first.Profile != "implementer" ||
		first.Model != "gpt-coding" || first.Provider != "openai" || first.FallbackFrom == nil ||
		*first.FallbackFrom != "claude-backup" {
		t.Fatalf("unexpected immutable snapshot: first=%+v second=%+v", first, second)
	}
	loaded, err := store.GetRunAgentConfig(ctx, claim.Run.ID)
	if err != nil || loaded == nil || !equalAgentConfig(*loaded, first) {
		t.Fatalf("get snapshot: loaded=%+v err=%v", loaded, err)
	}
	listed, err := store.ListTaskRunAgentConfigs(ctx, claim.Task.Task.ID)
	if err != nil || len(listed) != 1 || !equalAgentConfig(listed[0], first) {
		t.Fatalf("list snapshots: listed=%+v err=%v", listed, err)
	}

	changed := input
	changed.Model = "another-model"
	if _, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, changed); err == nil ||
		!strings.Contains(err.Error(), "different agent config") {
		t.Fatalf("changed snapshot error = %v", err)
	}
	var events int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'run_agent_configured'", claim.Task.Task.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("configured events = %d, want 1", events)
	}
}

func TestRecordRunAgentConfigAllowsExplicitUnpinnedCLIModel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claim := claimAgentConfigTask(t, store, model.RuntimeClaude)

	value, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, RecordRunAgentConfigInput{
		Profile: "reviewer", Runtime: model.RuntimeClaude, Source: "cli_default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.Model != "" || value.Provider != "" || value.Source != "cli_default" || value.FallbackFrom != nil {
		t.Fatalf("unexpected unpinned snapshot: %+v", value)
	}
}

func TestRecordRunAgentConfigRequiresActiveMatchingRun(t *testing.T) {
	ctx := context.Background()
	store, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claim := claimAgentConfigTask(t, store, model.RuntimeCodex)
	input := RecordRunAgentConfigInput{Profile: "implementer", Runtime: model.RuntimeCodex, Source: "global_default"}

	if _, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: "wrong"}, input); err == nil ||
		!strings.Contains(err.Error(), "invalid claim token") {
		t.Fatalf("invalid token error = %v", err)
	}
	wrongRuntime := input
	wrongRuntime.Runtime = model.RuntimeGemini
	if _, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, wrongRuntime); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("runtime mismatch error = %v", err)
	}
	if value, err := store.GetRunAgentConfig(ctx, claim.Run.ID); err != nil || value != nil {
		t.Fatalf("invalid attempts persisted config: value=%+v err=%v", value, err)
	}

	if _, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FailRun(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, "stopped", FailRunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, input); err == nil ||
		!strings.Contains(err.Error(), "already terminal") {
		t.Fatalf("terminal run error = %v", err)
	}
}

func TestClaimRespectsAgentAvailabilityAndConcurrency(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	worker, paused := "worker", "paused"
	first, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "first", Assignee: &worker, Runtime: model.RuntimeCodex, Priority: 30})
	second, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "second", Assignee: &worker, Runtime: model.RuntimeCodex, Priority: 20})
	pausedTask, _ := opened.CreateTask(ctx, CreateTaskInput{Title: "paused", Assignee: &paused, Runtime: model.RuntimeClaude, Priority: 10})
	claim, err := opened.ClaimTask(ctx, ClaimOptions{TaskID: first.Task.ID})
	if err != nil || claim == nil {
		t.Fatalf("claim first: claim=%#v err=%v", claim, err)
	}
	next, err := opened.ClaimTask(ctx, ClaimOptions{MaxInProgressByAssignee: map[string]int{"worker": 1}, ExcludedAssignees: []string{"paused"}})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("agent concurrency or exclusion was bypassed: %#v", next)
	}
	secondDetail, _ := opened.GetTask(ctx, second.Task.ID)
	pausedDetail, _ := opened.GetTask(ctx, pausedTask.Task.ID)
	if secondDetail.Task.Status != model.TaskStatusReady || pausedDetail.Task.Status != model.TaskStatusReady {
		t.Fatalf("skipped tasks changed state: second=%s paused=%s", secondDetail.Task.Status, pausedDetail.Task.Status)
	}
}

func TestRecordRunAgentConfigCanPinDispatcherFallbackRuntime(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(":memory:", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	claim := claimAgentConfigTask(t, opened, model.RuntimeCodex)
	primary := "codex-primary"
	config, err := opened.RecordRunAgentConfig(ctx, RunScope{RunID: claim.Run.ID, ClaimToken: claim.ClaimToken}, RecordRunAgentConfigInput{
		Profile: "claude-backup", Runtime: model.RuntimeClaude, Model: "claude-model", Source: "fallback",
		FallbackFrom: &primary, AllowRuntimeOverride: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := opened.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if config.Runtime != model.RuntimeClaude || run.Run.Runtime != model.RuntimeClaude || config.FallbackFrom == nil || *config.FallbackFrom != primary {
		t.Fatalf("fallback runtime was not pinned: config=%#v run=%#v", config, run.Run)
	}
}
