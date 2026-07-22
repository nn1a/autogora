package runcontrol

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/store"
)

type Termination struct {
	RunID    string           `json:"runId"`
	PID      *int             `json:"pid"`
	Signaled bool             `json:"signaled"`
	Pending  bool             `json:"pending"`
	Task     model.TaskDetail `json:"task"`
}

func SignalRunProcess(pid *int) bool {
	if pid == nil || *pid <= 0 || *pid == os.Getpid() {
		return false
	}
	return signalProcessTree(*pid)
}

func TerminateRun(ctx context.Context, opened *store.Store, runID, reason string) (Termination, error) {
	inspection, err := opened.GetRun(ctx, runID)
	if err != nil {
		return Termination{}, err
	}
	if inspection.Run.Status != model.RunStatusRunning || inspection.Task.CurrentRunID == nil ||
		*inspection.Task.CurrentRunID != runID || inspection.Task.Status != model.TaskStatusRunning {
		return Termination{}, fmt.Errorf("run is already terminal: %s", inspection.Run.Status)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Run terminated administratively"
	}
	if _, err := opened.DeferReclaim(ctx, runID, 15, reason); err != nil {
		return Termination{}, err
	}
	signaled := SignalRunProcess(inspection.Run.PID)
	if signaled {
		detail, err := opened.GetTask(ctx, inspection.Task.ID)
		return Termination{RunID: runID, PID: inspection.Run.PID, Signaled: true, Pending: true, Task: detail}, err
	}
	detail, err := opened.RecoverAbandonedRun(ctx, runID, model.RunStatusReclaimed, reason, false)
	return Termination{RunID: runID, PID: inspection.Run.PID, Pending: false, Task: detail}, err
}

func TerminateTaskRun(ctx context.Context, opened *store.Store, taskID, reason string) (Termination, error) {
	detail, err := opened.GetTask(ctx, taskID)
	if err != nil {
		return Termination{}, err
	}
	if detail.Task.CurrentRunID == nil || detail.Task.Status != model.TaskStatusRunning {
		return Termination{}, fmt.Errorf("task has no active run: %s", taskID)
	}
	return TerminateRun(ctx, opened, *detail.Task.CurrentRunID, reason)
}
