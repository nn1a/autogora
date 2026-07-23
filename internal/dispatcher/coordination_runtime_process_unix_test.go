//go:build !windows

package dispatcher

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

func TestCoordinationRuntimeTerminatesTimedOutPlannerTreeAndCleansOwnership(t *testing.T) {
	fixture := seedCoordinationRuntimeFixture(t, boards.CoordinationModeAssist)
	markers := t.TempDir()
	childPIDPath := markers + "/child.pid"
	callPath := markers + "/calls"
	t.Setenv("AUTOGORA_TEST_COORDINATOR_CHILD_PID", childPIDPath)
	t.Setenv("AUTOGORA_TEST_COORDINATOR_CALLS", callPath)
	command := executableFixture(t, `
printf 'call\n' >> "$AUTOGORA_TEST_COORDINATOR_CALLS"
(
	trap '' TERM
	while :; do sleep 1; done
) &
printf '%s' "$!" > "$AUTOGORA_TEST_COORDINATOR_CHILD_PID"
while :; do sleep 1; done`)
	config := *fixture.options.AgentConfig
	for index := range config.Agents {
		if config.Agents[index].ID == "coordinator" {
			config.Agents[index].Command = command
		}
	}
	config = agentconfig.Normalize(config)
	fixture.options.AgentConfig = &config
	fixture.options.CoordinatorPlanner = nil
	fixture.options.PlannerTimeout = 500 * time.Millisecond

	started := time.Now()
	err := runCoordinationPass(
		context.Background(),
		fixture.manager,
		[]string{"default"},
		fixture.options,
		&coordinationRuntimeState{},
		fixture.current,
	)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Coordinator error = %v", err)
	}
	if elapsed >= coordinationClaimTTL(fixture.options) {
		t.Fatalf(
			"planner cleanup took %s and crossed claim TTL %s",
			elapsed,
			coordinationClaimTTL(fixture.options),
		)
	}
	childPID := readPlannerChildPID(t, childPIDPath)
	childExited := false
	defer func() {
		if !childExited {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()
	childExited = waitForCoordinatorPlannerExit(childPID, 5*time.Second)
	if !childExited {
		t.Fatalf("timed-out Coordinator descendant %d is still running", childPID)
	}
	calls, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "call\n") != 1 {
		t.Fatalf("Coordinator paid calls = %q, want exactly one", calls)
	}

	opened, err := fixture.manager.OpenStore(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	incident, err := opened.GetCoordinationIncident(
		context.Background(),
		fixture.incident.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := opened.ListCoordinationAttempts(
		context.Background(),
		store.CoordinationAttemptFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := opened.ListCoordinationProposals(
		context.Background(),
		store.CoordinationProposalFilter{IncidentID: fixture.incident.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status != model.CoordinationIncidentOpen ||
		len(attempts) != 1 ||
		attempts[0].Status != model.CoordinationAttemptFailed ||
		len(proposals) != 0 {
		t.Fatalf(
			"timed-out ownership cleanup: incident=%+v attempts=%+v proposals=%+v",
			incident,
			attempts,
			proposals,
		)
	}
	coordination, err := fixture.manager.OpenCoordinationStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer coordination.Close()
	slots, err := coordination.ListGlobalAgentSlots(
		context.Background(),
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatalf("timed-out Coordinator left agent slots: %+v", slots)
	}
}

func readPlannerChildPID(t *testing.T, path string) int {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(value))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForCoordinatorPlannerExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
