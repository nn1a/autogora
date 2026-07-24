package runcontrol

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nn1a/autogora/internal/model"
	"github.com/nn1a/autogora/internal/processidentity"
	"github.com/nn1a/autogora/internal/store"
)

type Termination struct {
	RunID    string           `json:"runId"`
	PID      *int             `json:"pid"`
	Signaled bool             `json:"signaled"`
	Pending  bool             `json:"pending"`
	Task     model.TaskDetail `json:"task"`
}

// ProcessMayStillBeRunning is the conservative ownership check used before a
// caller releases run-scoped leases. A live PID retains ownership when its
// identity matches or cannot be verified. A verified mismatch does not prove
// that the original worker stopped: on Unix, its dedicated process group can
// outlive the leader while another process reuses the numeric PID. Probe that
// group before releasing ownership whenever the PID itself is not the worker.
func ProcessMayStillBeRunning(pid *int, expectedIdentity *string) bool {
	if pid == nil || *pid <= 0 {
		return false
	}
	state := processidentity.Inspect(*pid, expectedIdentity)
	if state.Alive && (!state.Verified || state.Matches) {
		return true
	}
	return ProcessTreeAlive(pid)
}

func SignalRunProcess(pid *int, expectedIdentity *string) bool {
	if pid == nil || *pid <= 0 || *pid == os.Getpid() {
		return false
	}
	return signalVerifiedProcess(*pid, expectedIdentity, false)
}

// ForceKillRunProcess escalates a prior graceful termination request. Callers
// must still wait until the process is observed dead before releasing a run.
func ForceKillRunProcess(pid *int, expectedIdentity *string) bool {
	if pid == nil || *pid <= 0 || *pid == os.Getpid() {
		return false
	}
	return signalVerifiedProcess(*pid, expectedIdentity, true)
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
	// Re-read after persisting the intent. A worker can record its PID while
	// the reclaim request is being written; signaling the earlier snapshot
	// would miss that process and could release the run underneath it.
	inspection, err = opened.GetRun(ctx, runID)
	if err != nil {
		return Termination{}, err
	}
	if inspection.Run.Status != model.RunStatusRunning || inspection.Task.CurrentRunID == nil ||
		*inspection.Task.CurrentRunID != runID || inspection.Task.Status != model.TaskStatusRunning {
		return Termination{}, fmt.Errorf("run is already terminal: %s", inspection.Run.Status)
	}
	processIdentity, err := opened.GetRunProcessIdentity(ctx, runID)
	if err != nil {
		return Termination{}, err
	}
	managed, err := opened.IsRunManaged(ctx, runID)
	if err != nil {
		return Termination{}, err
	}
	if !managed {
		reclaim, reclaimErr := opened.GetDeferredReclaim(ctx, runID)
		if reclaimErr != nil {
			return Termination{}, reclaimErr
		}
		if reclaim == nil {
			return Termination{}, fmt.Errorf(
				"run recovery fence disappeared: %s",
				runID,
			)
		}
		observation := store.ObserveRunForRecovery(
			inspection.Run,
			processIdentity,
			reclaim,
		)
		if _, err := opened.RequireObservedRunRecoveryIntervention(
			ctx,
			observation,
			15,
			"External run termination requires explicit confirmation that the worker and host writes stopped",
			model.RunStatusReclaimed,
			false,
		); err != nil {
			return Termination{}, err
		}
	}
	signaled := SignalRunProcess(inspection.Run.PID, processIdentity)
	if signaled {
		detail, err := opened.GetTask(ctx, inspection.Task.ID)
		return Termination{RunID: runID, PID: inspection.Run.PID, Signaled: true, Pending: true, Task: detail}, err
	}
	if managed || ProcessMayStillBeRunning(inspection.Run.PID, processIdentity) {
		// A dispatcher-managed run can be between process turns or can have a
		// workspace whose final state has not been inspected yet. Keep the
		// durable termination intent pending so the dispatcher can preserve
		// partial work before it releases the task for another worker. An
		// unmanaged process also stays pending when its PID cannot be proven
		// stale or descendants still own its process group; releasing it could
		// overlap two writers.
		detail, err := opened.GetTask(ctx, inspection.Task.ID)
		return Termination{RunID: runID, PID: inspection.Run.PID, Pending: true, Task: detail}, err
	}
	// An external PID disappearing is not proof that its host, plugin, or
	// network-disconnected agent stopped writing. Keep the durable operator
	// fence until ConfirmRunRecoveryQuiescence records both attestations.
	detail, err := opened.GetTask(ctx, inspection.Task.ID)
	return Termination{
		RunID: runID,
		PID:   inspection.Run.PID,
		// Unmanaged runs always remain pending for audited confirmation.
		Pending: true,
		Task:    detail,
	}, err
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
