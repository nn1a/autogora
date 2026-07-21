export const TASK_STATUSES = [
  "triage",
  "todo",
  "scheduled",
  "ready",
  "running",
  "blocked",
  "review",
  "done",
  "archived",
] as const;

export type TaskStatus = (typeof TASK_STATUSES)[number];

export const WORKER_RUNTIMES = ["claude", "codex", "cline"] as const;

export type WorkerRuntime = (typeof WORKER_RUNTIMES)[number];

export const PLANNER_RUNTIMES = ["claude", "codex", "cline"] as const;

export type PlannerRuntime = (typeof PLANNER_RUNTIMES)[number];

export const RUNTIMES = [...WORKER_RUNTIMES, "manual"] as const;

export type Runtime = (typeof RUNTIMES)[number];

export const BLOCK_KINDS = ["dependency", "needs_input", "capability", "transient"] as const;

export type BlockKind = (typeof BLOCK_KINDS)[number];

export const RUN_STATUSES = [
  "running",
  "completed",
  "blocked",
  "failed",
  "reclaimed",
  "crashed",
  "timed_out",
  "rate_limited",
  "spawn_failed",
  "protocol_violation",
] as const;

export type RunStatus = (typeof RUN_STATUSES)[number];

export interface Task {
  id: string;
  board: string;
  tenant: string | null;
  idempotencyKey: string | null;
  title: string;
  body: string;
  assignee: string | null;
  runtime: Runtime;
  status: TaskStatus;
  priority: number;
  workspace: string | null;
  workspaceKind: "scratch" | "dir" | "worktree";
  branch: string | null;
  currentRunId: string | null;
  result: string | null;
  scheduledAt: string | null;
  maxRuntimeSeconds: number | null;
  skills: string[];
  goalMode: boolean;
  goalMaxTurns: number;
  workflowTemplateId: string | null;
  currentStepKey: string | null;
  blockKind: BlockKind | null;
  blockReason: string | null;
  blockRecurrences: number;
  failureCount: number;
  maxRetries: number;
  createdAt: string;
  updatedAt: string;
}

export interface Run {
  id: string;
  taskId: string;
  workerId: string;
  runtime: Runtime;
  status: RunStatus;
  claimedAt: string;
  claimExpiresAt: string;
  heartbeatAt: string;
  endedAt: string | null;
  pid: number | null;
  logPath: string | null;
  exitCode: number | null;
  summary: string | null;
  metadata: Record<string, unknown> | null;
  error: string | null;
}

export interface Comment {
  id: number;
  taskId: string;
  author: string;
  body: string;
  createdAt: string;
}

export interface Attachment {
  id: string;
  taskId: string;
  kind: "file" | "url";
  name: string;
  mediaType: string | null;
  size: number | null;
  sha256: string | null;
  path: string | null;
  url: string | null;
  createdAt: string;
}

export interface TaskEvent {
  id: number;
  taskId: string;
  runId: string | null;
  kind: string;
  payload: Record<string, unknown> | null;
  createdAt: string;
}

export interface TaskDetail {
  task: Task;
  parents: Task[];
  children: Task[];
  comments: Comment[];
  attachments: Attachment[];
  runs: Run[];
  events: TaskEvent[];
}

export interface CreateTaskInput {
  title: string;
  body?: string | undefined;
  board?: string | undefined;
  tenant?: string | null | undefined;
  idempotencyKey?: string | null | undefined;
  assignee?: string | null | undefined;
  runtime?: Runtime | undefined;
  priority?: number | undefined;
  workspace?: string | null | undefined;
  workspaceKind?: "scratch" | "dir" | "worktree" | undefined;
  branch?: string | null | undefined;
  status?: TaskStatus | undefined;
  scheduledAt?: string | null | undefined;
  maxRuntimeSeconds?: number | null | undefined;
  skills?: string[] | undefined;
  goalMode?: boolean | undefined;
  goalMaxTurns?: number | undefined;
  workflowTemplateId?: string | null | undefined;
  currentStepKey?: string | null | undefined;
  maxRetries?: number | undefined;
  parents?: string[] | undefined;
}

export interface ClaimedTask {
  task: TaskDetail;
  run: Run;
  claimToken: string;
}

export interface ListTaskFilter {
  board?: string | undefined;
  status?: TaskStatus | undefined;
  tenant?: string | undefined;
  assignee?: string | undefined;
  runtime?: Runtime | undefined;
  workflowTemplateId?: string | undefined;
  currentStepKey?: string | undefined;
  includeArchived?: boolean | undefined;
  search?: string | undefined;
  sort?: "created" | "created-desc" | "priority" | "priority-desc" | "status" | "assignee" | "title" | "updated" | undefined;
  limit?: number | undefined;
}
