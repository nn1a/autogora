export const TASK_STATUSES = [
  "triage",
  "todo",
  "ready",
  "running",
  "blocked",
  "done",
  "archived",
] as const;

export type TaskStatus = (typeof TASK_STATUSES)[number];

export const RUNTIMES = ["claude", "codex", "manual"] as const;

export type Runtime = (typeof RUNTIMES)[number];

export interface Task {
  id: string;
  board: string;
  title: string;
  body: string;
  assignee: string | null;
  runtime: Runtime;
  status: TaskStatus;
  priority: number;
  workspace: string | null;
  currentRunId: string | null;
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
  status: "running" | "completed" | "blocked" | "failed";
  claimedAt: string;
  heartbeatAt: string;
  endedAt: string | null;
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
  runs: Run[];
  events: TaskEvent[];
}

export interface CreateTaskInput {
  title: string;
  body?: string | undefined;
  board?: string | undefined;
  assignee?: string | null | undefined;
  runtime?: Runtime | undefined;
  priority?: number | undefined;
  workspace?: string | null | undefined;
  status?: TaskStatus | undefined;
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
  assignee?: string | undefined;
  runtime?: Runtime | undefined;
  includeArchived?: boolean | undefined;
  limit?: number | undefined;
}
