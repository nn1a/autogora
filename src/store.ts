import { createHash, randomBytes, randomUUID } from "node:crypto";
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, statSync, unlinkSync } from "node:fs";
import { basename, dirname, isAbsolute, join, resolve } from "node:path";
import { DatabaseSync, type SQLInputValue } from "node:sqlite";

import {
  BLOCK_KINDS,
  RUNTIMES,
  TASK_STATUSES,
  type ClaimedTask,
  type Attachment,
  type BlockKind,
  type Comment,
  type CreateTaskInput,
  type ListTaskFilter,
  type Run,
  type Runtime,
  type RunStatus,
  type Task,
  type TaskDetail,
  type TaskEvent,
  type TaskStatus,
} from "./types.js";

type TaskRow = {
  id: string;
  board: string;
  tenant: string | null;
  idempotency_key: string | null;
  title: string;
  body: string;
  assignee: string | null;
  runtime: Runtime;
  status: TaskStatus;
  priority: number;
  workspace: string | null;
  workspace_kind: "scratch" | "dir" | "worktree";
  branch: string | null;
  current_run_id: string | null;
  result: string | null;
  scheduled_at: string | null;
  max_runtime_seconds: number | null;
  skills_json: string;
  goal_mode: number;
  goal_max_turns: number;
  workflow_template_id: string | null;
  current_step_key: string | null;
  block_kind: BlockKind | null;
  block_reason: string | null;
  block_recurrences: number;
  failure_count: number;
  max_retries: number;
  created_at: string;
  updated_at: string;
};

type RunRow = {
  id: string;
  task_id: string;
  worker_id: string;
  runtime: Runtime;
  status: Run["status"];
  claim_token: string;
  claimed_at: string;
  claim_expires_at: string;
  heartbeat_at: string;
  ended_at: string | null;
  pid: number | null;
  log_path: string | null;
  exit_code: number | null;
  summary: string | null;
  metadata_json: string | null;
  error: string | null;
};

type CommentRow = {
  id: number;
  task_id: string;
  author: string;
  body: string;
  created_at: string;
};

type EventRow = {
  id: number;
  task_id: string;
  run_id: string | null;
  kind: string;
  payload_json: string | null;
  created_at: string;
};

type AttachmentRow = {
  id: string;
  task_id: string;
  kind: "file" | "url";
  name: string;
  media_type: string | null;
  size: number | null;
  sha256: string | null;
  path: string | null;
  url: string | null;
  created_at: string;
};

export interface UpdateTaskInput {
  title?: string | undefined;
  body?: string | undefined;
  assignee?: string | null | undefined;
  tenant?: string | null | undefined;
  runtime?: Runtime | undefined;
  priority?: number | undefined;
  workspace?: string | null | undefined;
  workspaceKind?: "scratch" | "dir" | "worktree" | undefined;
  branch?: string | null | undefined;
  scheduledAt?: string | null | undefined;
  maxRuntimeSeconds?: number | null | undefined;
  skills?: string[] | undefined;
  goalMode?: boolean | undefined;
  goalMaxTurns?: number | undefined;
  workflowTemplateId?: string | null | undefined;
  currentStepKey?: string | null | undefined;
  status?: TaskStatus | undefined;
}

export interface RunScope {
  runId: string;
  claimToken: string;
}

export interface CompletionInput {
  summary?: string | undefined;
  result?: string | undefined;
  metadata?: Record<string, unknown> | undefined;
  artifacts?: string[] | undefined;
}

export interface BlockInput {
  reason: string;
  kind?: BlockKind | undefined;
}

export interface FailRunOptions {
  outcome?: Exclude<RunStatus, "running" | "completed" | "blocked" | "reclaimed"> | undefined;
  countFailure?: boolean | undefined;
  cooldownSeconds?: number | undefined;
}

export interface ActiveRun {
  task: Task;
  run: Run;
}

const BLOCK_RECURRENCE_LIMIT = 2;
export const ATTACHMENT_MAX_BYTES = 25 * 1024 * 1024;

function now(): string {
  return new Date().toISOString();
}

function futureIso(seconds: number): string {
  return new Date(Date.now() + seconds * 1_000).toISOString();
}

function normalizeIso(value: string | null | undefined, field: string): string | null {
  if (value === null || value === undefined || value.trim() === "") return null;
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) throw new Error(`${field} must be a valid ISO-8601 date`);
  return new Date(timestamp).toISOString();
}

function normalizeSkills(skills: string[] | undefined): string[] {
  return [...new Set((skills ?? []).map((skill) => skill.trim()).filter(Boolean))];
}

function workspaceKind(workspace: string | null | undefined, explicit?: "scratch" | "dir" | "worktree") {
  if (explicit) return explicit;
  if (!workspace || workspace === "scratch") return "scratch" as const;
  if (workspace === "worktree" || workspace.startsWith("worktree:")) return "worktree" as const;
  return "dir" as const;
}

function mediaTypeFor(name: string): string | null {
  const extension = name.toLowerCase().split(".").pop();
  const known: Record<string, string> = {
    txt: "text/plain",
    md: "text/markdown",
    json: "application/json",
    pdf: "application/pdf",
    png: "image/png",
    jpg: "image/jpeg",
    jpeg: "image/jpeg",
    gif: "image/gif",
    webp: "image/webp",
    csv: "text/csv",
    html: "text/html",
    xml: "application/xml",
    zip: "application/zip",
  };
  return extension ? known[extension] ?? null : null;
}

function cleanAttachmentName(value: string): string {
  const name = basename(value).replaceAll("\0", "").trim();
  if (!name || name === "." || name === "..") throw new Error("Attachment name cannot be empty");
  return name.slice(0, 255);
}

function newId(prefix: string): string {
  return `${prefix}_${randomUUID().replaceAll("-", "").slice(0, 12)}`;
}

function parseJson(value: string | null): Record<string, unknown> | null {
  if (value === null) return null;
  return JSON.parse(value) as Record<string, unknown>;
}

function taskFromRow(row: TaskRow): Task {
  return {
    id: row.id,
    board: row.board,
    tenant: row.tenant,
    idempotencyKey: row.idempotency_key,
    title: row.title,
    body: row.body,
    assignee: row.assignee,
    runtime: row.runtime,
    status: row.status,
    priority: row.priority,
    workspace: row.workspace,
    workspaceKind: row.workspace_kind,
    branch: row.branch,
    currentRunId: row.current_run_id,
    result: row.result,
    scheduledAt: row.scheduled_at,
    maxRuntimeSeconds: row.max_runtime_seconds,
    skills: JSON.parse(row.skills_json) as string[],
    goalMode: row.goal_mode === 1,
    goalMaxTurns: row.goal_max_turns,
    workflowTemplateId: row.workflow_template_id,
    currentStepKey: row.current_step_key,
    blockKind: row.block_kind,
    blockReason: row.block_reason,
    blockRecurrences: row.block_recurrences,
    failureCount: row.failure_count,
    maxRetries: row.max_retries,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

function runFromRow(row: RunRow): Run {
  return {
    id: row.id,
    taskId: row.task_id,
    workerId: row.worker_id,
    runtime: row.runtime,
    status: row.status,
    claimedAt: row.claimed_at,
    claimExpiresAt: row.claim_expires_at,
    heartbeatAt: row.heartbeat_at,
    endedAt: row.ended_at,
    pid: row.pid,
    logPath: row.log_path,
    exitCode: row.exit_code,
    summary: row.summary,
    metadata: parseJson(row.metadata_json),
    error: row.error,
  };
}

function commentFromRow(row: CommentRow): Comment {
  return {
    id: row.id,
    taskId: row.task_id,
    author: row.author,
    body: row.body,
    createdAt: row.created_at,
  };
}

function eventFromRow(row: EventRow): TaskEvent {
  return {
    id: row.id,
    taskId: row.task_id,
    runId: row.run_id,
    kind: row.kind,
    payload: parseJson(row.payload_json),
    createdAt: row.created_at,
  };
}

function attachmentFromRow(row: AttachmentRow): Attachment {
  return {
    id: row.id,
    taskId: row.task_id,
    kind: row.kind,
    name: row.name,
    mediaType: row.media_type,
    size: row.size,
    sha256: row.sha256,
    path: row.path,
    url: row.url,
    createdAt: row.created_at,
  };
}

export class KanbanStore {
  readonly dbPath: string;
  readonly board: string;
  readonly attachmentsRoot: string;
  private readonly db: DatabaseSync;

  constructor(dbPath: string, board = "default", attachmentsRoot?: string) {
    this.dbPath = dbPath === ":memory:" ? dbPath : resolve(dbPath);
    this.board = board.trim() || "default";
    this.attachmentsRoot = resolve(attachmentsRoot ?? join(dirname(this.dbPath), "attachments"));
    if (this.dbPath !== ":memory:") mkdirSync(dirname(this.dbPath), { recursive: true });
    this.db = new DatabaseSync(this.dbPath);
    this.db.exec("PRAGMA foreign_keys = ON");
    this.db.exec("PRAGMA busy_timeout = 5000");
    if (this.dbPath !== ":memory:") this.db.exec("PRAGMA journal_mode = WAL");
    this.initialize();
  }

  close(): void {
    this.db.close();
  }

  private initialize(): void {
    const existing = this.db
      .prepare("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'tasks'")
      .get() as { sql: string } | undefined;
    if (existing && (!existing.sql.includes("'scheduled'") || !existing.sql.includes("idempotency_key"))) {
      this.migrateLegacySchema();
    } else {
      this.createLatestSchema();
    }
    this.db.exec("PRAGMA user_version = 3");
  }

  private createLatestSchema(): void {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS tasks (
        id TEXT PRIMARY KEY,
        board TEXT NOT NULL DEFAULT 'default',
        tenant TEXT,
        idempotency_key TEXT,
        title TEXT NOT NULL,
        body TEXT NOT NULL DEFAULT '',
        assignee TEXT,
        runtime TEXT NOT NULL DEFAULT 'manual' CHECK (runtime IN ('claude', 'codex', 'manual')),
        status TEXT NOT NULL CHECK (status IN ('triage', 'todo', 'scheduled', 'ready', 'running', 'blocked', 'review', 'done', 'archived')),
        priority INTEGER NOT NULL DEFAULT 0,
        workspace TEXT,
        workspace_kind TEXT NOT NULL DEFAULT 'scratch' CHECK (workspace_kind IN ('scratch', 'dir', 'worktree')),
        branch TEXT,
        current_run_id TEXT,
        result TEXT,
        scheduled_at TEXT,
        max_runtime_seconds INTEGER CHECK (max_runtime_seconds IS NULL OR max_runtime_seconds >= 1),
        skills_json TEXT NOT NULL DEFAULT '[]',
        goal_mode INTEGER NOT NULL DEFAULT 0 CHECK (goal_mode IN (0, 1)),
        goal_max_turns INTEGER NOT NULL DEFAULT 20 CHECK (goal_max_turns >= 1),
        workflow_template_id TEXT,
        current_step_key TEXT,
        block_kind TEXT CHECK (block_kind IS NULL OR block_kind IN ('dependency', 'needs_input', 'capability', 'transient')),
        block_reason TEXT,
        block_recurrences INTEGER NOT NULL DEFAULT 0,
        failure_count INTEGER NOT NULL DEFAULT 0,
        max_retries INTEGER NOT NULL DEFAULT 2 CHECK (max_retries >= 1),
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS task_links (
        parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        PRIMARY KEY (parent_id, child_id),
        CHECK (parent_id <> child_id)
      );

      CREATE TABLE IF NOT EXISTS task_comments (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        author TEXT NOT NULL,
        body TEXT NOT NULL,
        created_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS task_runs (
        id TEXT PRIMARY KEY,
        task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        worker_id TEXT NOT NULL,
        runtime TEXT NOT NULL CHECK (runtime IN ('claude', 'codex', 'manual')),
        status TEXT NOT NULL,
        claim_token TEXT NOT NULL,
        claimed_at TEXT NOT NULL,
        claim_expires_at TEXT NOT NULL,
        heartbeat_at TEXT NOT NULL,
        ended_at TEXT,
        pid INTEGER,
        log_path TEXT,
        exit_code INTEGER,
        summary TEXT,
        metadata_json TEXT,
        error TEXT
      );

      CREATE TABLE IF NOT EXISTS task_attachments (
        id TEXT PRIMARY KEY,
        task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        kind TEXT NOT NULL CHECK (kind IN ('file', 'url')),
        name TEXT NOT NULL,
        media_type TEXT,
        size INTEGER,
        sha256 TEXT,
        path TEXT,
        url TEXT,
        created_at TEXT NOT NULL,
        CHECK ((kind = 'file' AND path IS NOT NULL AND url IS NULL) OR
               (kind = 'url' AND url IS NOT NULL AND path IS NULL))
      );

      CREATE TABLE IF NOT EXISTS task_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
        kind TEXT NOT NULL,
        payload_json TEXT,
        created_at TEXT NOT NULL
      );

      CREATE INDEX IF NOT EXISTS idx_tasks_queue
        ON tasks(board, status, scheduled_at, runtime, priority DESC, created_at);
      CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_idempotency
        ON tasks(board, idempotency_key) WHERE idempotency_key IS NOT NULL AND status <> 'archived';
      CREATE INDEX IF NOT EXISTS idx_runs_task ON task_runs(task_id, claimed_at DESC);
      CREATE INDEX IF NOT EXISTS idx_attachments_task ON task_attachments(task_id, created_at);
      CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id DESC);
    `);
  }

  private migrateLegacySchema(): void {
    this.db.exec("PRAGMA foreign_keys = OFF");
    try {
      this.db.exec(`
        BEGIN IMMEDIATE;
        ALTER TABLE task_events RENAME TO task_events_legacy;
        ALTER TABLE task_runs RENAME TO task_runs_legacy;
        ALTER TABLE task_comments RENAME TO task_comments_legacy;
        ALTER TABLE task_links RENAME TO task_links_legacy;
        ALTER TABLE tasks RENAME TO tasks_legacy;
        DROP INDEX IF EXISTS idx_tasks_queue;
        DROP INDEX IF EXISTS idx_runs_task;
        DROP INDEX IF EXISTS idx_events_task;
      `);
      this.createLatestSchema();
      this.db.exec(`
        INSERT INTO tasks(
          id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
          priority, workspace, workspace_kind, branch, current_run_id, result,
          scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
          workflow_template_id, current_step_key, block_kind, block_reason,
          block_recurrences, failure_count, max_retries, created_at, updated_at
        )
        SELECT
          id, board, NULL, NULL, title, body, assignee, runtime, status,
          priority, workspace,
          CASE WHEN workspace IS NULL OR workspace = 'scratch' THEN 'scratch' ELSE 'dir' END,
          NULL, current_run_id, NULL, NULL, NULL, '[]', 0, 20,
          NULL, NULL, NULL, NULL, 0, failure_count, max_retries, created_at, updated_at
        FROM tasks_legacy;

        INSERT INTO task_links SELECT * FROM task_links_legacy;
        INSERT INTO task_comments SELECT * FROM task_comments_legacy;
        INSERT INTO task_runs(
          id, task_id, worker_id, runtime, status, claim_token, claimed_at,
          claim_expires_at, heartbeat_at, ended_at, pid, log_path, exit_code,
          summary, metadata_json, error
        )
        SELECT
          id, task_id, worker_id, runtime, status, claim_token, claimed_at,
          strftime('%Y-%m-%dT%H:%M:%fZ', claimed_at, '+15 minutes'),
          heartbeat_at, ended_at, NULL, NULL, NULL, summary, metadata_json, error
        FROM task_runs_legacy;
        INSERT INTO task_events SELECT * FROM task_events_legacy;

        DROP TABLE task_events_legacy;
        DROP TABLE task_runs_legacy;
        DROP TABLE task_comments_legacy;
        DROP TABLE task_links_legacy;
        DROP TABLE tasks_legacy;
        COMMIT;
      `);
    } catch (error) {
      if (this.db.isTransaction) this.db.exec("ROLLBACK");
      throw error;
    } finally {
      this.db.exec("PRAGMA foreign_keys = ON");
    }
  }

  private write<T>(fn: () => T): T {
    this.db.exec("BEGIN IMMEDIATE");
    try {
      const result = fn();
      this.db.exec("COMMIT");
      return result;
    } catch (error) {
      this.db.exec("ROLLBACK");
      throw error;
    }
  }

  private requireTaskRow(taskId: string): TaskRow {
    const row = this.db.prepare("SELECT * FROM tasks WHERE id = ?").get(taskId) as TaskRow | undefined;
    if (!row) throw new Error(`Task not found: ${taskId}`);
    return row;
  }

  private appendEvent(
    taskId: string,
    kind: string,
    payload: Record<string, unknown> | null = null,
    runId: string | null = null,
  ): void {
    this.db
      .prepare(
        "INSERT INTO task_events(task_id, run_id, kind, payload_json, created_at) VALUES (?, ?, ?, ?, ?)",
      )
      .run(taskId, runId, kind, payload === null ? null : JSON.stringify(payload), now());
  }

  private closeRunNoTransaction(
    task: TaskRow,
    status: Run["status"],
    input: { summary?: string | null; metadata?: Record<string, unknown> | null; error?: string | null; exitCode?: number | null } = {},
  ): string | null {
    if (!task.current_run_id) return null;
    const timestamp = now();
    this.db
      .prepare(`
        UPDATE task_runs
        SET status = ?, ended_at = ?, heartbeat_at = ?, summary = COALESCE(?, summary),
            metadata_json = COALESCE(?, metadata_json), error = COALESCE(?, error),
            exit_code = COALESCE(?, exit_code)
        WHERE id = ? AND status = 'running'
      `)
      .run(
        status,
        timestamp,
        timestamp,
        input.summary ?? null,
        input.metadata ? JSON.stringify(input.metadata) : null,
        input.error ?? null,
        input.exitCode ?? null,
        task.current_run_id,
      );
    return task.current_run_id;
  }

  private syntheticRunNoTransaction(
    task: TaskRow,
    status: Run["status"],
    input: { summary?: string | null; metadata?: Record<string, unknown> | null; error?: string | null } = {},
  ): string {
    const runId = newId("r");
    const timestamp = now();
    this.db
      .prepare(`
        INSERT INTO task_runs(
          id, task_id, worker_id, runtime, status, claim_token, claimed_at,
          claim_expires_at, heartbeat_at, ended_at, summary, metadata_json, error
        ) VALUES (?, ?, 'human', ?, ?, 'synthetic', ?, ?, ?, ?, ?, ?, ?)
      `)
      .run(
        runId,
        task.id,
        task.runtime,
        status,
        timestamp,
        timestamp,
        timestamp,
        timestamp,
        input.summary ?? null,
        input.metadata ? JSON.stringify(input.metadata) : null,
        input.error ?? null,
      );
    return runId;
  }

  private hasOpenParents(taskId: string): boolean {
    const row = this.db
      .prepare(`
        SELECT COUNT(*) AS count
        FROM task_links l
        JOIN tasks p ON p.id = l.parent_id
        WHERE l.child_id = ? AND p.status <> 'done'
      `)
      .get(taskId) as { count: number };
    return row.count > 0;
  }

  private recomputeReady(taskId: string, at = now()): void {
    const task = this.requireTaskRow(taskId);
    if (["triage", "running", "blocked", "review", "done", "archived"].includes(task.status)) return;
    if (task.status === "scheduled" && task.scheduled_at === null) return;
    if (task.scheduled_at && Date.parse(task.scheduled_at) > Date.parse(at)) {
      if (task.status !== "scheduled") {
        this.db.prepare("UPDATE tasks SET status = 'scheduled', updated_at = ? WHERE id = ?").run(now(), taskId);
        this.appendEvent(taskId, "scheduled", { scheduledAt: task.scheduled_at });
      }
      return;
    }
    const status: TaskStatus =
      this.hasOpenParents(taskId) || task.assignee === null || task.runtime === "manual" ? "todo" : "ready";
    if (status !== task.status) {
      this.db.prepare("UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?").run(status, now(), taskId);
      this.appendEvent(taskId, status === "ready" ? "promoted" : "dependency_wait");
    }
  }

  private assertLinkDoesNotCycle(parentId: string, childId: string): void {
    if (parentId === childId) throw new Error("A task cannot depend on itself");
    const cycle = this.db
      .prepare(`
        WITH RECURSIVE descendants(id) AS (
          SELECT child_id FROM task_links WHERE parent_id = ?
          UNION
          SELECT l.child_id FROM task_links l JOIN descendants d ON l.parent_id = d.id
        )
        SELECT 1 AS found FROM descendants WHERE id = ? LIMIT 1
      `)
      .get(childId, parentId) as { found: number } | undefined;
    if (cycle) throw new Error(`Dependency cycle rejected: ${parentId} -> ${childId}`);
  }

  private linkNoTransaction(parentId: string, childId: string): void {
    const parent = this.requireTaskRow(parentId);
    const child = this.requireTaskRow(childId);
    if (parent.board !== child.board) throw new Error("Cross-board dependencies are not allowed");
    this.assertLinkDoesNotCycle(parentId, childId);
    this.db.prepare("INSERT OR IGNORE INTO task_links(parent_id, child_id) VALUES (?, ?)").run(parentId, childId);
    this.appendEvent(childId, "linked", { parentId });
    this.recomputeReady(childId);
  }

  createTask(input: CreateTaskInput): TaskDetail {
    const title = input.title.trim();
    if (!title) throw new Error("Task title cannot be empty");
    const runtime = input.runtime ?? "manual";
    if (!RUNTIMES.includes(runtime)) throw new Error(`Invalid runtime: ${runtime}`);
    const requestedStatus = input.status;
    if (requestedStatus && !TASK_STATUSES.includes(requestedStatus)) {
      throw new Error(`Invalid status: ${requestedStatus}`);
    }

    const tenant = input.tenant?.trim() || null;
    const idempotencyKey = input.idempotencyKey?.trim() || null;
    const scheduledAt = normalizeIso(input.scheduledAt, "scheduledAt");
    const maxRuntimeSeconds = input.maxRuntimeSeconds ?? null;
    if (maxRuntimeSeconds !== null && (!Number.isInteger(maxRuntimeSeconds) || maxRuntimeSeconds < 1)) {
      throw new Error("maxRuntimeSeconds must be a positive integer");
    }
    const maxRetries = input.maxRetries ?? 2;
    if (!Number.isInteger(maxRetries) || maxRetries < 1) throw new Error("maxRetries must be a positive integer");
    const goalMaxTurns = input.goalMaxTurns ?? 20;
    if (!Number.isInteger(goalMaxTurns) || goalMaxTurns < 1) throw new Error("goalMaxTurns must be a positive integer");
    const skills = normalizeSkills(input.skills);
    let taskId = newId("t");
    this.write(() => {
      if (idempotencyKey) {
        const existing = this.db
          .prepare("SELECT id FROM tasks WHERE board = ? AND idempotency_key = ? AND status <> 'archived'")
          .get(input.board ?? this.board, idempotencyKey) as { id: string } | undefined;
        if (existing) {
          taskId = existing.id;
          return;
        }
      }
      const timestamp = now();
      const automaticStatus: TaskStatus = scheduledAt && Date.parse(scheduledAt) > Date.now()
        ? "scheduled"
        : input.assignee && runtime !== "manual" && (input.parents?.length ?? 0) === 0
          ? "ready"
          : "todo";
      this.db
        .prepare(`
          INSERT INTO tasks(
            id, board, tenant, idempotency_key, title, body, assignee, runtime, status,
            priority, workspace, workspace_kind, branch, current_run_id, result,
            scheduled_at, max_runtime_seconds, skills_json, goal_mode, goal_max_turns,
            workflow_template_id, current_step_key, block_kind, block_reason,
            block_recurrences, failure_count, max_retries, created_at, updated_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, 0, ?, ?, ?)
        `)
        .run(
          taskId,
          input.board ?? this.board,
          tenant,
          idempotencyKey,
          title,
          input.body ?? "",
          input.assignee ?? null,
          runtime,
          requestedStatus ?? automaticStatus,
          input.priority ?? 0,
          input.workspace ?? null,
          workspaceKind(input.workspace, input.workspaceKind),
          input.branch ?? null,
          scheduledAt,
          maxRuntimeSeconds,
          JSON.stringify(skills),
          input.goalMode ? 1 : 0,
          goalMaxTurns,
          input.workflowTemplateId ?? null,
          input.currentStepKey ?? null,
          maxRetries,
          timestamp,
          timestamp,
        );
      this.appendEvent(taskId, "created", {
        runtime,
        assignee: input.assignee ?? null,
        tenant,
        status: requestedStatus ?? automaticStatus,
        parents: input.parents ?? [],
      });
      for (const parentId of input.parents ?? []) this.linkNoTransaction(parentId, taskId);
      if (requestedStatus === "ready" && this.hasOpenParents(taskId)) {
        this.db.prepare("UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?").run(now(), taskId);
      }
    });
    return this.getTask(taskId);
  }

  updateTask(taskId: string, input: UpdateTaskInput): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (input.status === "running" && task.status !== "running") {
        throw new Error("Tasks enter running only through an atomic claim");
      }
      if (
        task.current_run_id &&
        (input.assignee !== undefined || input.runtime !== undefined || input.workspace !== undefined || input.workspaceKind !== undefined)
      ) {
        throw new Error("Cannot change task ownership or workspace while a run is active");
      }
      const updates: string[] = [];
      const values: SQLInputValue[] = [];
      const add = (column: string, value: SQLInputValue): void => {
        updates.push(`${column} = ?`);
        values.push(value);
      };
      if (input.title !== undefined) {
        if (!input.title.trim()) throw new Error("Task title cannot be empty");
        add("title", input.title.trim());
      }
      if (input.body !== undefined) add("body", input.body);
      if (input.assignee !== undefined) add("assignee", input.assignee);
      if (input.tenant !== undefined) add("tenant", input.tenant?.trim() || null);
      if (input.runtime !== undefined) add("runtime", input.runtime);
      if (input.priority !== undefined) add("priority", input.priority);
      if (input.workspace !== undefined) {
        add("workspace", input.workspace);
        if (input.workspaceKind === undefined) add("workspace_kind", workspaceKind(input.workspace));
      }
      if (input.workspaceKind !== undefined) add("workspace_kind", input.workspaceKind);
      if (input.branch !== undefined) add("branch", input.branch);
      if (input.scheduledAt !== undefined) add("scheduled_at", normalizeIso(input.scheduledAt, "scheduledAt"));
      if (input.maxRuntimeSeconds !== undefined) {
        if (input.maxRuntimeSeconds !== null && (!Number.isInteger(input.maxRuntimeSeconds) || input.maxRuntimeSeconds < 1)) {
          throw new Error("maxRuntimeSeconds must be a positive integer");
        }
        add("max_runtime_seconds", input.maxRuntimeSeconds);
      }
      if (input.skills !== undefined) add("skills_json", JSON.stringify(normalizeSkills(input.skills)));
      if (input.goalMode !== undefined) add("goal_mode", input.goalMode ? 1 : 0);
      if (input.goalMaxTurns !== undefined) {
        if (!Number.isInteger(input.goalMaxTurns) || input.goalMaxTurns < 1) {
          throw new Error("goalMaxTurns must be a positive integer");
        }
        add("goal_max_turns", input.goalMaxTurns);
      }
      if (input.workflowTemplateId !== undefined) add("workflow_template_id", input.workflowTemplateId);
      if (input.currentStepKey !== undefined) add("current_step_key", input.currentStepKey);
      if (input.status !== undefined) add("status", input.status);
      let reclaimedRunId: string | null = null;
      if (task.current_run_id && input.status && input.status !== "running") {
        const runStatus: Run["status"] = input.status === "done"
          ? "completed"
          : input.status === "blocked"
            ? "blocked"
            : "reclaimed";
        reclaimedRunId = this.closeRunNoTransaction(task, runStatus, {
          error: runStatus === "reclaimed" ? `Administrative status transition to ${input.status}` : null,
        });
        add("current_run_id", null);
      }
      if (input.status === "done") {
        add("failure_count", 0);
        add("block_kind", null);
        add("block_reason", null);
        add("block_recurrences", 0);
      }
      if (updates.length === 0) return;
      updates.push("updated_at = ?");
      values.push(now(), taskId);
      this.db.prepare(`UPDATE tasks SET ${updates.join(", ")} WHERE id = ?`).run(...values);
      this.appendEvent(taskId, "updated", input as Record<string, unknown>);
      if (reclaimedRunId && input.status && !["done", "blocked"].includes(input.status)) {
        this.appendEvent(taskId, "reclaimed", { status: input.status }, reclaimedRunId);
      }
      if (input.status === undefined || ["ready", "todo", "scheduled"].includes(input.status)) {
        this.recomputeReady(taskId);
      }
    });
    return this.getTask(taskId);
  }

  listTasks(filter: ListTaskFilter = {}): Task[] {
    const clauses = ["board = ?"];
    const values: SQLInputValue[] = [filter.board ?? this.board];
    if (filter.status) {
      clauses.push("status = ?");
      values.push(filter.status);
    } else if (!filter.includeArchived) {
      clauses.push("status <> 'archived'");
    }
    if (filter.tenant) {
      clauses.push("tenant = ?");
      values.push(filter.tenant);
    }
    if (filter.assignee) {
      clauses.push("assignee = ?");
      values.push(filter.assignee);
    }
    if (filter.runtime) {
      clauses.push("runtime = ?");
      values.push(filter.runtime);
    }
    if (filter.search?.trim()) {
      clauses.push("(title LIKE ? OR body LIKE ?)");
      const pattern = `%${filter.search.trim()}%`;
      values.push(pattern, pattern);
    }
    const orderBy: Record<NonNullable<ListTaskFilter["sort"]>, string> = {
      created: "created_at ASC",
      "created-desc": "created_at DESC",
      priority: "priority ASC, created_at ASC",
      "priority-desc": "priority DESC, created_at ASC",
      status: "status ASC, priority DESC, created_at ASC",
      assignee: "assignee ASC, priority DESC, created_at ASC",
      title: "title COLLATE NOCASE ASC",
      updated: "updated_at DESC",
    };
    const limit = Math.max(1, Math.min(filter.limit ?? 100, 500));
    values.push(limit);
    const rows = this.db
      .prepare(`SELECT * FROM tasks WHERE ${clauses.join(" AND ")} ORDER BY ${orderBy[filter.sort ?? "priority-desc"]} LIMIT ?`)
      .all(...values) as unknown as TaskRow[];
    return rows.map(taskFromRow);
  }

  countTasksByStatus(board = this.board): Record<TaskStatus, number> {
    const counts = Object.fromEntries(TASK_STATUSES.map((status) => [status, 0])) as Record<TaskStatus, number>;
    const rows = this.db
      .prepare("SELECT status, COUNT(*) AS count FROM tasks WHERE board = ? GROUP BY status")
      .all(board) as unknown as { status: TaskStatus; count: number }[];
    for (const row of rows) counts[row.status] = row.count;
    return counts;
  }

  getTask(taskId: string): TaskDetail {
    const task = taskFromRow(this.requireTaskRow(taskId));
    const parents = this.db
      .prepare("SELECT t.* FROM tasks t JOIN task_links l ON l.parent_id = t.id WHERE l.child_id = ? ORDER BY t.created_at")
      .all(taskId) as unknown as TaskRow[];
    const children = this.db
      .prepare("SELECT t.* FROM tasks t JOIN task_links l ON l.child_id = t.id WHERE l.parent_id = ? ORDER BY t.created_at")
      .all(taskId) as unknown as TaskRow[];
    const comments = this.db
      .prepare("SELECT * FROM task_comments WHERE task_id = ? ORDER BY id")
      .all(taskId) as unknown as CommentRow[];
    const attachments = this.db
      .prepare("SELECT * FROM task_attachments WHERE task_id = ? ORDER BY created_at, id")
      .all(taskId) as unknown as AttachmentRow[];
    const runs = this.db
      .prepare("SELECT * FROM task_runs WHERE task_id = ? ORDER BY claimed_at")
      .all(taskId) as unknown as RunRow[];
    const events = this.db
      .prepare("SELECT * FROM task_events WHERE task_id = ? ORDER BY id DESC LIMIT 100")
      .all(taskId) as unknown as EventRow[];
    return {
      task,
      parents: parents.map(taskFromRow),
      children: children.map(taskFromRow),
      comments: comments.map(commentFromRow),
      attachments: attachments.map(attachmentFromRow),
      runs: runs.map(runFromRow),
      events: events.map(eventFromRow).reverse(),
    };
  }

  linkTasks(parentId: string, childId: string): TaskDetail {
    this.write(() => this.linkNoTransaction(parentId, childId));
    return this.getTask(childId);
  }

  unlinkTasks(parentId: string, childId: string): TaskDetail {
    this.write(() => {
      this.requireTaskRow(parentId);
      this.requireTaskRow(childId);
      const deleted = this.db
        .prepare("DELETE FROM task_links WHERE parent_id = ? AND child_id = ?")
        .run(parentId, childId);
      if (deleted.changes > 0) this.appendEvent(childId, "unlinked", { parentId });
      this.recomputeReady(childId);
    });
    return this.getTask(childId);
  }

  archiveTask(taskId: string): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (task.status === "archived") return;
      const runId = this.closeRunNoTransaction(task, "reclaimed", { error: "Task archived while running" });
      this.db
        .prepare("UPDATE tasks SET status = 'archived', current_run_id = NULL, updated_at = ? WHERE id = ?")
        .run(now(), taskId);
      this.appendEvent(taskId, "archived", null, runId);
    });
    return this.getTask(taskId);
  }

  deleteTask(taskId: string): { id: string; deleted: true } {
    const result = this.write(() => {
      this.requireTaskRow(taskId);
      this.db.prepare("DELETE FROM tasks WHERE id = ?").run(taskId);
      return { id: taskId, deleted: true as const };
    });
    const directory = join(this.attachmentsRoot, taskId);
    if (existsSync(directory)) rmSync(directory, { recursive: true, force: true });
    return result;
  }

  promoteTask(taskId: string): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (!["todo", "scheduled", "blocked", "triage", "review"].includes(task.status)) {
        throw new Error(`Task cannot be promoted from ${task.status}`);
      }
      if (task.current_run_id) throw new Error("Cannot promote a running task");
      this.db
        .prepare("UPDATE tasks SET status = 'todo', scheduled_at = NULL, failure_count = 0, updated_at = ? WHERE id = ?")
        .run(now(), taskId);
      this.appendEvent(taskId, "promote_requested");
      this.recomputeReady(taskId);
    });
    return this.getTask(taskId);
  }

  scheduleTask(taskId: string, scheduledAt: string | null, reason?: string): TaskDetail {
    const normalized = normalizeIso(scheduledAt, "scheduledAt");
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (task.current_run_id) throw new Error("Cannot schedule a running task");
      this.db
        .prepare("UPDATE tasks SET status = 'scheduled', scheduled_at = ?, updated_at = ? WHERE id = ?")
        .run(normalized, now(), taskId);
      this.appendEvent(taskId, "scheduled", { scheduledAt: normalized, reason: reason?.trim() || null });
    });
    return this.getTask(taskId);
  }

  addComment(taskId: string, author: string, body: string): Comment {
    const cleanBody = body.trim();
    if (!cleanBody) throw new Error("Comment cannot be empty");
    const id = this.write(() => {
      this.requireTaskRow(taskId);
      const result = this.db
        .prepare("INSERT INTO task_comments(task_id, author, body, created_at) VALUES (?, ?, ?, ?)")
        .run(taskId, author.trim() || "agent", cleanBody, now());
      this.appendEvent(taskId, "commented", { author: author.trim() || "agent" });
      return Number(result.lastInsertRowid);
    });
    const row = this.db.prepare("SELECT * FROM task_comments WHERE id = ?").get(id) as CommentRow;
    return commentFromRow(row);
  }

  listAttachments(taskId: string): Attachment[] {
    this.requireTaskRow(taskId);
    const rows = this.db
      .prepare("SELECT * FROM task_attachments WHERE task_id = ? ORDER BY created_at, id")
      .all(taskId) as unknown as AttachmentRow[];
    return rows.map(attachmentFromRow);
  }

  attachFile(taskId: string, sourcePath: string, displayName?: string): Attachment {
    this.requireTaskRow(taskId);
    const source = resolve(sourcePath);
    if (!existsSync(source)) throw new Error(`Attachment file not found: ${source}`);
    const stat = statSync(source);
    if (!stat.isFile()) throw new Error(`Attachment source is not a file: ${source}`);
    if (stat.size > ATTACHMENT_MAX_BYTES) {
      throw new Error(`Attachment exceeds the ${ATTACHMENT_MAX_BYTES} byte limit`);
    }
    const id = newId("a");
    const name = cleanAttachmentName(displayName ?? source);
    const directory = join(this.attachmentsRoot, taskId);
    mkdirSync(directory, { recursive: true });
    const target = join(directory, `${id}-${name}`);
    copyFileSync(source, target);
    const sha256 = createHash("sha256").update(readFileSync(target)).digest("hex");
    try {
      this.write(() => {
        this.requireTaskRow(taskId);
        this.db
          .prepare(`
            INSERT INTO task_attachments(id, task_id, kind, name, media_type, size, sha256, path, url, created_at)
            VALUES (?, ?, 'file', ?, ?, ?, ?, ?, NULL, ?)
          `)
          .run(id, taskId, name, mediaTypeFor(name), stat.size, sha256, target, now());
        this.appendEvent(taskId, "attached", { attachmentId: id, kind: "file", name, size: stat.size });
      });
    } catch (error) {
      if (existsSync(target)) unlinkSync(target);
      throw error;
    }
    return attachmentFromRow(
      this.db.prepare("SELECT * FROM task_attachments WHERE id = ?").get(id) as AttachmentRow,
    );
  }

  attachUrl(taskId: string, rawUrl: string, displayName?: string): Attachment {
    this.requireTaskRow(taskId);
    let url: URL;
    try {
      url = new URL(rawUrl);
    } catch {
      throw new Error("Attachment URL must be valid");
    }
    if (!["http:", "https:"].includes(url.protocol)) throw new Error("Attachment URL must use http or https");
    const id = newId("a");
    const fallbackName = basename(url.pathname) || url.hostname;
    const name = cleanAttachmentName(displayName ?? fallbackName);
    this.write(() => {
      this.requireTaskRow(taskId);
      this.db
        .prepare(`
          INSERT INTO task_attachments(id, task_id, kind, name, media_type, size, sha256, path, url, created_at)
          VALUES (?, ?, 'url', ?, NULL, NULL, NULL, NULL, ?, ?)
        `)
        .run(id, taskId, name, url.toString(), now());
      this.appendEvent(taskId, "attached", { attachmentId: id, kind: "url", name, url: url.toString() });
    });
    return attachmentFromRow(
      this.db.prepare("SELECT * FROM task_attachments WHERE id = ?").get(id) as AttachmentRow,
    );
  }

  removeAttachment(taskId: string, attachmentId: string): { id: string; removed: true } {
    const row = this.db
      .prepare("SELECT * FROM task_attachments WHERE id = ? AND task_id = ?")
      .get(attachmentId, taskId) as AttachmentRow | undefined;
    if (!row) throw new Error(`Attachment not found: ${attachmentId}`);
    this.write(() => {
      this.db.prepare("DELETE FROM task_attachments WHERE id = ? AND task_id = ?").run(attachmentId, taskId);
      this.appendEvent(taskId, "attachment_removed", { attachmentId, name: row.name });
    });
    if (row.path && existsSync(row.path)) unlinkSync(row.path);
    return { id: attachmentId, removed: true };
  }

  private captureArtifacts(task: TaskRow, artifacts: string[] | undefined): Attachment[] {
    if (!artifacts || artifacts.length === 0) return [];
    const workspace = task.workspace?.replace(/^(?:dir|worktree):/, "") || process.cwd();
    return normalizeSkills(artifacts).map((artifact) => {
      const path = isAbsolute(artifact) ? artifact : resolve(workspace, artifact);
      return this.attachFile(task.id, path);
    });
  }

  promoteDueTasks(board = this.board, at = now()): number {
    return this.write(() => {
      const rows = this.db
        .prepare("SELECT id FROM tasks WHERE board = ? AND status = 'scheduled' AND scheduled_at IS NOT NULL AND scheduled_at <= ?")
        .all(board, at) as unknown as { id: string }[];
      for (const row of rows) {
        this.db.prepare("UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?").run(now(), row.id);
        this.appendEvent(row.id, "schedule_due", { scheduledAt: at });
        this.recomputeReady(row.id, at);
      }
      return rows.length;
    });
  }

  private respawnGuardReason(taskId: string): "blocker_auth" | "recent_success" | "active_pr" | null {
    const oneHourAgo = new Date(Date.now() - 60 * 60 * 1_000).toISOString();
    const recent = this.db
      .prepare("SELECT status, error FROM task_runs WHERE task_id = ? AND ended_at >= ? ORDER BY ended_at DESC LIMIT 1")
      .get(taskId, oneHourAgo) as { status: RunStatus; error: string | null } | undefined;
    if (recent?.status === "completed") return "recent_success";
    if (recent?.error && /(?:429|rate.?limit|quota|unauthorized|authentication|invalid api key)/i.test(recent.error)) {
      return "blocker_auth";
    }
    const pullRequest = this.db
      .prepare(`
        SELECT 1 AS found FROM task_comments
        WHERE task_id = ? AND body LIKE '%github.com/%/pull/%'
        ORDER BY id DESC LIMIT 1
      `)
      .get(taskId) as { found: number } | undefined;
    return pullRequest ? "active_pr" : null;
  }

  private appendRespawnGuard(taskId: string, reason: string): void {
    const recent = this.db
      .prepare(`
        SELECT payload_json FROM task_events
        WHERE task_id = ? AND kind = 'respawn_guarded' AND created_at >= ?
        ORDER BY id DESC LIMIT 1
      `)
      .get(taskId, new Date(Date.now() - 60_000).toISOString()) as { payload_json: string | null } | undefined;
    if (recent && parseJson(recent.payload_json)?.reason === reason) return;
    this.appendEvent(taskId, "respawn_guarded", { reason });
  }

  claimTask(
    input: {
      taskId?: string | undefined;
      board?: string | undefined;
      runtime?: Runtime | undefined;
      workerId?: string | undefined;
      excludeManual?: boolean | undefined;
      claimTtlSeconds?: number | undefined;
      maxInProgress?: number | undefined;
      maxInProgressPerAssignee?: number | undefined;
    } = {},
  ): ClaimedTask | null {
    let runId = "";
    let claimToken = "";
    let taskId = "";
    const claimed = this.write(() => {
      const board = input.board ?? this.board;
      if (input.maxInProgress !== undefined) {
        const running = this.db
          .prepare("SELECT COUNT(*) AS count FROM tasks WHERE board = ? AND status = 'running'")
          .get(board) as { count: number };
        if (running.count >= Math.max(1, input.maxInProgress)) return false;
      }
      const clauses = ["board = ?", "status = 'ready'", "current_run_id IS NULL", "(scheduled_at IS NULL OR scheduled_at <= ?)"];
      const values: SQLInputValue[] = [board, now()];
      if (input.taskId) {
        clauses.push("id = ?");
        values.push(input.taskId);
      }
      if (input.runtime) {
        clauses.push("runtime = ?");
        values.push(input.runtime);
      }
      if (input.excludeManual) clauses.push("runtime <> 'manual'");
      const candidates = this.db
        .prepare(`SELECT * FROM tasks WHERE ${clauses.join(" AND ")} ORDER BY priority DESC, created_at ASC LIMIT 50`)
        .all(...values) as unknown as TaskRow[];
      let row: TaskRow | undefined;
      for (const candidate of candidates) {
        if (this.hasOpenParents(candidate.id)) {
          this.db.prepare("UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?").run(now(), candidate.id);
          continue;
        }
        if (input.maxInProgressPerAssignee !== undefined && candidate.assignee) {
          const running = this.db
            .prepare("SELECT COUNT(*) AS count FROM tasks WHERE board = ? AND status = 'running' AND assignee = ?")
            .get(board, candidate.assignee) as { count: number };
          if (running.count >= Math.max(1, input.maxInProgressPerAssignee)) continue;
        }
        const guard = this.respawnGuardReason(candidate.id);
        if (guard) {
          this.appendRespawnGuard(candidate.id, guard);
          continue;
        }
        row = candidate;
        break;
      }
      if (!row) return false;
      runId = newId("r");
      claimToken = randomBytes(24).toString("base64url");
      taskId = row.id;
      const timestamp = now();
      const claimTtlSeconds = Math.max(1, input.claimTtlSeconds ?? 15 * 60);
      const claimExpiresAt = futureIso(claimTtlSeconds);
      const changed = this.db
        .prepare("UPDATE tasks SET status = 'running', current_run_id = ?, updated_at = ? WHERE id = ? AND status = 'ready' AND current_run_id IS NULL")
        .run(runId, timestamp, row.id);
      if (changed.changes !== 1) return false;
      this.db
        .prepare(`
          INSERT INTO task_runs(
            id, task_id, worker_id, runtime, status, claim_token, claimed_at,
            claim_expires_at, heartbeat_at
          ) VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?)
        `)
        .run(
          runId,
          row.id,
          input.workerId ?? `${row.runtime}-worker`,
          row.runtime,
          claimToken,
          timestamp,
          claimExpiresAt,
          timestamp,
        );
      this.appendEvent(
        row.id,
        "claimed",
        { workerId: input.workerId ?? `${row.runtime}-worker`, expires: claimExpiresAt },
        runId,
      );
      return true;
    });
    if (!claimed) return null;
    const runRow = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(runId) as RunRow;
    return { task: this.getTask(taskId), run: runFromRow(runRow), claimToken };
  }

  private requireActiveRun(scope: RunScope): { task: TaskRow; run: RunRow } {
    const run = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(scope.runId) as RunRow | undefined;
    if (!run) throw new Error(`Run not found: ${scope.runId}`);
    const token = this.db.prepare("SELECT claim_token FROM task_runs WHERE id = ?").get(scope.runId) as
      | { claim_token: string }
      | undefined;
    if (!token || token.claim_token !== scope.claimToken) throw new Error("Invalid claim token");
    if (run.status !== "running") throw new Error(`Run is already terminal: ${run.status}`);
    const task = this.requireTaskRow(run.task_id);
    if (task.current_run_id !== run.id || task.status !== "running") throw new Error("Run no longer owns this task");
    return { task, run };
  }

  heartbeat(scope: RunScope, note?: string): Run {
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      const timestamp = now();
      const originalTtl = Math.max(1_000, Date.parse(run.claim_expires_at) - Date.parse(run.claimed_at));
      const claimExpiresAt = new Date(Date.now() + originalTtl).toISOString();
      this.db
        .prepare("UPDATE task_runs SET heartbeat_at = ?, claim_expires_at = ? WHERE id = ?")
        .run(timestamp, claimExpiresAt, run.id);
      this.db.prepare("UPDATE tasks SET updated_at = ? WHERE id = ?").run(timestamp, task.id);
      this.appendEvent(task.id, "heartbeat", note ? { note } : null, run.id);
    });
    const row = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(scope.runId) as RunRow;
    return runFromRow(row);
  }

  bindRunWorkspace(
    scope: RunScope,
    workspace: string,
    kind: "scratch" | "dir" | "worktree",
  ): TaskDetail {
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const path = resolve(workspace);
      this.db
        .prepare("UPDATE tasks SET workspace = ?, workspace_kind = ?, updated_at = ? WHERE id = ?")
        .run(path, kind, now(), task.id);
      this.appendEvent(task.id, "workspace_prepared", { path, kind }, run.id);
    });
    return this.getTask(taskId);
  }

  recordSpawn(scope: RunScope, pid: number, logPath: string): Run {
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      this.db.prepare("UPDATE task_runs SET pid = ?, log_path = ? WHERE id = ?").run(pid, resolve(logPath), run.id);
      this.appendEvent(task.id, "spawned", { pid, logPath: resolve(logPath) }, run.id);
    });
    const row = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(scope.runId) as RunRow;
    return runFromRow(row);
  }

  completeRun(
    scope: RunScope,
    summaryOrInput: string | CompletionInput,
    legacyMetadata?: Record<string, unknown>,
  ): TaskDetail {
    const completion: CompletionInput = typeof summaryOrInput === "string"
      ? { summary: summaryOrInput, metadata: legacyMetadata }
      : summaryOrInput;
    const cleanSummary = completion.summary?.trim() || completion.result?.trim() || "";
    const cleanResult = completion.result?.trim() || null;
    if (!cleanSummary) throw new Error("Completion requires a summary or result");
    const preflight = this.requireActiveRun(scope);
    const captured = this.captureArtifacts(preflight.task, completion.artifacts);
    const metadata = captured.length > 0
      ? { ...(completion.metadata ?? {}), artifacts: captured.map((attachment) => ({ id: attachment.id, name: attachment.name, path: attachment.path })) }
      : completion.metadata;
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const timestamp = now();
      this.db
        .prepare("UPDATE task_runs SET status = 'completed', ended_at = ?, heartbeat_at = ?, summary = ?, metadata_json = ? WHERE id = ?")
        .run(timestamp, timestamp, cleanSummary, metadata ? JSON.stringify(metadata) : null, run.id);
      this.db
        .prepare(`
          UPDATE tasks
          SET status = 'done', current_run_id = NULL, result = ?, failure_count = 0,
              block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ?
          WHERE id = ?
        `)
        .run(cleanResult, timestamp, task.id);
      this.appendEvent(
        task.id,
        "completed",
        { summary: cleanSummary.slice(0, 400), resultLength: cleanResult?.length ?? 0 },
        run.id,
      );
      const children = this.db.prepare("SELECT child_id FROM task_links WHERE parent_id = ?").all(task.id) as unknown as {
        child_id: string;
      }[];
      for (const child of children) this.recomputeReady(child.child_id);
    });
    return this.getTask(taskId);
  }

  completeTask(taskId: string, completion: CompletionInput = {}): TaskDetail {
    const cleanSummary = completion.summary?.trim() || completion.result?.trim() || "";
    const cleanResult = completion.result?.trim() || null;
    const preflight = this.requireTaskRow(taskId);
    const captured = preflight.status === "done" ? [] : this.captureArtifacts(preflight, completion.artifacts);
    const metadata = captured.length > 0
      ? { ...(completion.metadata ?? {}), artifacts: captured.map((attachment) => ({ id: attachment.id, name: attachment.name, path: attachment.path })) }
      : completion.metadata;
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (task.status === "archived") throw new Error("Cannot complete an archived task");
      if (task.status === "done") return;
      const runId = task.current_run_id
        ? this.closeRunNoTransaction(task, "completed", {
            summary: cleanSummary || null,
            metadata: metadata ?? null,
          })
        : cleanSummary || metadata
          ? this.syntheticRunNoTransaction(task, "completed", {
              summary: cleanSummary || null,
              metadata: metadata ?? null,
            })
          : null;
      this.db
        .prepare(`
          UPDATE tasks
          SET status = 'done', current_run_id = NULL, result = ?, failure_count = 0,
              block_kind = NULL, block_reason = NULL, block_recurrences = 0, updated_at = ?
          WHERE id = ?
        `)
        .run(cleanResult, now(), taskId);
      this.appendEvent(taskId, "completed", { summary: cleanSummary.slice(0, 400), resultLength: cleanResult?.length ?? 0 }, runId);
      const children = this.db.prepare("SELECT child_id FROM task_links WHERE parent_id = ?").all(taskId) as unknown as {
        child_id: string;
      }[];
      for (const child of children) this.recomputeReady(child.child_id);
    });
    return this.getTask(taskId);
  }

  private blockNoTransaction(task: TaskRow, input: BlockInput, runId: string | null): void {
    const cleanReason = input.reason.trim();
    if (!cleanReason) throw new Error("Block reason cannot be empty");
    if (input.kind && !BLOCK_KINDS.includes(input.kind)) throw new Error(`Invalid block kind: ${input.kind}`);
    const timestamp = now();
    if (input.kind === "dependency") {
      this.db
        .prepare(`
          UPDATE tasks
          SET status = 'todo', current_run_id = NULL, block_kind = ?, block_reason = ?, updated_at = ?
          WHERE id = ?
      `)
        .run(input.kind, cleanReason, timestamp, task.id);
      this.appendEvent(task.id, "dependency_wait", { reason: cleanReason, kind: input.kind }, runId);
      return;
    }

    const sameBlock = task.block_kind === (input.kind ?? null) && task.block_reason === cleanReason;
    const recurrences = sameBlock ? task.block_recurrences + 1 : 1;
    const loopDetected = recurrences >= BLOCK_RECURRENCE_LIMIT && task.block_recurrences > 0;
    const status: TaskStatus = loopDetected ? "triage" : "blocked";
    this.db
      .prepare(`
        UPDATE tasks
        SET status = ?, current_run_id = NULL, block_kind = ?, block_reason = ?,
            block_recurrences = ?, updated_at = ?
        WHERE id = ?
      `)
      .run(status, input.kind ?? null, cleanReason, recurrences, timestamp, task.id);
    this.appendEvent(
      task.id,
      loopDetected ? "block_loop_detected" : "blocked",
      loopDetected
        ? { reason: cleanReason, kind: input.kind ?? null, recurrences, limit: BLOCK_RECURRENCE_LIMIT }
        : { reason: cleanReason, kind: input.kind ?? null, recurrences },
      runId,
    );
  }

  blockRun(scope: RunScope, reason: string, kind?: BlockKind): TaskDetail {
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const timestamp = now();
      this.db
        .prepare("UPDATE task_runs SET status = 'blocked', ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?")
        .run(timestamp, timestamp, reason.trim(), run.id);
      this.blockNoTransaction(task, { reason, kind }, run.id);
    });
    return this.getTask(taskId);
  }

  blockTask(taskId: string, input: BlockInput): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (["done", "archived"].includes(task.status)) throw new Error(`Cannot block a ${task.status} task`);
      if (task.status === "blocked") throw new Error("Task is already blocked; unblock it before blocking again");
      const runId = task.current_run_id
        ? this.closeRunNoTransaction(task, "blocked", { error: input.reason.trim() })
        : this.syntheticRunNoTransaction(task, "blocked", { error: input.reason.trim() });
      this.blockNoTransaction(task, input, runId);
    });
    return this.getTask(taskId);
  }

  private finishUnsuccessfulNoTransaction(
    task: TaskRow,
    run: RunRow,
    error: string,
    outcome: Exclude<RunStatus, "running" | "completed" | "blocked">,
    countFailure: boolean,
    cooldownSeconds: number,
  ): void {
    const timestamp = now();
    const failures = task.failure_count + (countFailure ? 1 : 0);
    const exhausted = countFailure && failures >= task.max_retries;
    const scheduledAt = !exhausted && cooldownSeconds > 0 ? futureIso(cooldownSeconds) : null;
    const nextStatus: TaskStatus = exhausted
      ? "blocked"
      : scheduledAt
        ? "scheduled"
        : this.hasOpenParents(task.id) || task.assignee === null || task.runtime === "manual"
          ? "todo"
          : "ready";
    this.db
      .prepare("UPDATE task_runs SET status = ?, ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?")
      .run(outcome, timestamp, timestamp, error, run.id);
    this.db
      .prepare(`
        UPDATE tasks
        SET status = ?, current_run_id = NULL, failure_count = ?, scheduled_at = ?,
            block_reason = CASE WHEN ? THEN ? ELSE block_reason END, updated_at = ?
        WHERE id = ?
      `)
      .run(nextStatus, failures, scheduledAt, exhausted ? 1 : 0, exhausted ? error : null, timestamp, task.id);

    const payload = { error, failures, outcome, countFailure, scheduledAt };
    if (outcome === "failed") {
      this.appendEvent(task.id, exhausted ? "gave_up" : "requeued", payload, run.id);
    } else {
      this.appendEvent(task.id, outcome, payload, run.id);
      if (exhausted) this.appendEvent(task.id, "gave_up", payload, run.id);
    }
  }

  failRun(scope: RunScope, error: string, options: FailRunOptions = {}): TaskDetail {
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      this.finishUnsuccessfulNoTransaction(
        task,
        run,
        error,
        options.outcome ?? "failed",
        options.countFailure ?? true,
        Math.max(0, options.cooldownSeconds ?? 0),
      );
    });
    return this.getTask(taskId);
  }

  listActiveRuns(board = this.board): ActiveRun[] {
    const rows = this.db
      .prepare(`
        SELECT r.id AS run_id, r.task_id
        FROM task_runs r JOIN tasks t ON t.id = r.task_id
        WHERE t.board = ? AND t.status = 'running' AND t.current_run_id = r.id AND r.status = 'running'
        ORDER BY r.claimed_at
      `)
      .all(board) as unknown as { run_id: string; task_id: string }[];
    return rows.map((row) => {
      const task = taskFromRow(this.requireTaskRow(row.task_id));
      const run = runFromRow(this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(row.run_id) as RunRow);
      return { task, run };
    });
  }

  recoverAbandonedRun(
    runId: string,
    outcome: "reclaimed" | "crashed" | "timed_out",
    error: string,
    countFailure = outcome !== "reclaimed",
  ): TaskDetail {
    let taskId = "";
    this.write(() => {
      const run = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(runId) as RunRow | undefined;
      if (!run) throw new Error(`Run not found: ${runId}`);
      const task = this.requireTaskRow(run.task_id);
      taskId = task.id;
      if (run.status !== "running" || task.current_run_id !== run.id || task.status !== "running") return;
      this.finishUnsuccessfulNoTransaction(task, run, error, outcome, countFailure, 0);
    });
    return this.getTask(taskId);
  }

  deferReclaim(runId: string, seconds = 120): Run {
    this.write(() => {
      const run = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(runId) as RunRow | undefined;
      if (!run || run.status !== "running") throw new Error(`Active run not found: ${runId}`);
      const expires = futureIso(Math.max(1, seconds));
      this.db.prepare("UPDATE task_runs SET claim_expires_at = ? WHERE id = ?").run(expires, runId);
      this.appendEvent(run.task_id, "reclaim_deferred", { pid: run.pid, expires }, run.id);
    });
    return runFromRow(this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(runId) as RunRow);
  }

  unblockTask(taskId: string): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (task.status !== "blocked") throw new Error(`Task is not blocked: ${task.status}`);
      this.db
        .prepare("UPDATE tasks SET status = 'todo', failure_count = 0, updated_at = ? WHERE id = ?")
        .run(now(), taskId);
      this.appendEvent(taskId, "unblocked");
      this.recomputeReady(taskId);
    });
    return this.getTask(taskId);
  }
}
