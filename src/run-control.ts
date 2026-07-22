import type { KanbanStore } from "./store.js";
import type { TaskDetail } from "./types.js";

export interface RunTermination {
  runId: string;
  pid: number | null;
  signaled: boolean;
  pending: boolean;
  task: TaskDetail;
}

export function signalRunProcess(pid: number | null): boolean {
  if (pid === null || pid === process.pid || !Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, "SIGTERM");
    return true;
  } catch {
    return false;
  }
}

export function terminateRun(store: KanbanStore, runId: string, reason = "Run terminated administratively"): RunTermination {
  const inspection = store.getRun(runId);
  if (inspection.run.status !== "running" || inspection.task.currentRunId !== runId || inspection.task.status !== "running") {
    throw new Error(`Run is already terminal: ${inspection.run.status}`);
  }
  const signaled = signalRunProcess(inspection.run.pid);
  const cleanReason = reason.trim() || "Run terminated administratively";
  if (signaled) {
    store.deferReclaim(runId, 15, cleanReason);
    return { runId, pid: inspection.run.pid, signaled, pending: true, task: store.getTask(inspection.task.id) };
  }
  const task = store.recoverAbandonedRun(runId, "reclaimed", cleanReason, false);
  return { runId, pid: inspection.run.pid, signaled, pending: false, task };
}

export function terminateTaskRun(store: KanbanStore, taskId: string, reason?: string): RunTermination {
  const detail = store.getTask(taskId);
  if (!detail.task.currentRunId || detail.task.status !== "running") {
    throw new Error(`Task has no active run: ${taskId}`);
  }
  return terminateRun(store, detail.task.currentRunId, reason);
}
