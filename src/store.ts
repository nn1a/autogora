import { randomBytes, randomUUID } from "node:crypto";
import { mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { DatabaseSync, type SQLInputValue } from "node:sqlite";

import {
  RUNTIMES,
  TASK_STATUSES,
  type ClaimedTask,
  type Comment,
  type CreateTaskInput,
  type ListTaskFilter,
  type Run,
  type Runtime,
  type Task,
  type TaskDetail,
  type TaskEvent,
  type TaskStatus,
} from "./types.js";

type TaskRow = {
  id: string;
  board: string;
  title: string;
  body: string;
  assignee: string | null;
  runtime: Runtime;
  status: TaskStatus;
  priority: number;
  workspace: string | null;
  current_run_id: string | null;
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
  claimed_at: string;
  heartbeat_at: string;
  ended_at: string | null;
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

export interface UpdateTaskInput {
  title?: string | undefined;
  body?: string | undefined;
  assignee?: string | null | undefined;
  runtime?: Runtime | undefined;
  priority?: number | undefined;
  workspace?: string | null | undefined;
  status?: TaskStatus | undefined;
}

export interface RunScope {
  runId: string;
  claimToken: string;
}

function now(): string {
  return new Date().toISOString();
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
    title: row.title,
    body: row.body,
    assignee: row.assignee,
    runtime: row.runtime,
    status: row.status,
    priority: row.priority,
    workspace: row.workspace,
    currentRunId: row.current_run_id,
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
    heartbeatAt: row.heartbeat_at,
    endedAt: row.ended_at,
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

export class KanbanStore {
  readonly dbPath: string;
  private readonly db: DatabaseSync;

  constructor(dbPath: string) {
    this.dbPath = dbPath === ":memory:" ? dbPath : resolve(dbPath);
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
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS tasks (
        id TEXT PRIMARY KEY,
        board TEXT NOT NULL DEFAULT 'default',
        title TEXT NOT NULL,
        body TEXT NOT NULL DEFAULT '',
        assignee TEXT,
        runtime TEXT NOT NULL DEFAULT 'manual' CHECK (runtime IN ('claude', 'codex', 'manual')),
        status TEXT NOT NULL CHECK (status IN ('triage', 'todo', 'ready', 'running', 'blocked', 'done', 'archived')),
        priority INTEGER NOT NULL DEFAULT 0,
        workspace TEXT,
        current_run_id TEXT,
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
        status TEXT NOT NULL CHECK (status IN ('running', 'completed', 'blocked', 'failed')),
        claim_token TEXT NOT NULL,
        claimed_at TEXT NOT NULL,
        heartbeat_at TEXT NOT NULL,
        ended_at TEXT,
        summary TEXT,
        metadata_json TEXT,
        error TEXT
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
        ON tasks(board, status, runtime, priority DESC, created_at);
      CREATE INDEX IF NOT EXISTS idx_runs_task ON task_runs(task_id, claimed_at DESC);
      CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id DESC);
    `);
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

  private recomputeReady(taskId: string): void {
    const task = this.requireTaskRow(taskId);
    if (["triage", "running", "blocked", "done", "archived"].includes(task.status)) return;
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

    const taskId = newId("t");
    this.write(() => {
      const timestamp = now();
      const automaticStatus: TaskStatus =
        input.assignee && runtime !== "manual" && (input.parents?.length ?? 0) === 0 ? "ready" : "todo";
      this.db
        .prepare(`
          INSERT INTO tasks(
            id, board, title, body, assignee, runtime, status, priority, workspace,
            current_run_id, failure_count, max_retries, created_at, updated_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, ?, ?, ?)
        `)
        .run(
          taskId,
          input.board ?? "default",
          title,
          input.body ?? "",
          input.assignee ?? null,
          runtime,
          requestedStatus ?? automaticStatus,
          input.priority ?? 0,
          input.workspace ?? null,
          input.maxRetries ?? 2,
          timestamp,
          timestamp,
        );
      this.appendEvent(taskId, "created", { runtime, assignee: input.assignee ?? null });
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
      if (task.current_run_id && input.status && input.status !== "running") {
        throw new Error("Use complete, block, or fail to terminate a running task");
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
      if (input.runtime !== undefined) add("runtime", input.runtime);
      if (input.priority !== undefined) add("priority", input.priority);
      if (input.workspace !== undefined) add("workspace", input.workspace);
      if (input.status !== undefined) add("status", input.status);
      if (updates.length === 0) return;
      updates.push("updated_at = ?");
      values.push(now(), taskId);
      this.db.prepare(`UPDATE tasks SET ${updates.join(", ")} WHERE id = ?`).run(...values);
      this.appendEvent(taskId, "updated", input as Record<string, unknown>);
      if (input.status === undefined || input.status === "ready" || input.status === "todo") {
        this.recomputeReady(taskId);
      }
    });
    return this.getTask(taskId);
  }

  listTasks(filter: ListTaskFilter = {}): Task[] {
    const clauses = ["board = ?"];
    const values: SQLInputValue[] = [filter.board ?? "default"];
    if (filter.status) {
      clauses.push("status = ?");
      values.push(filter.status);
    } else if (!filter.includeArchived) {
      clauses.push("status <> 'archived'");
    }
    if (filter.assignee) {
      clauses.push("assignee = ?");
      values.push(filter.assignee);
    }
    if (filter.runtime) {
      clauses.push("runtime = ?");
      values.push(filter.runtime);
    }
    const limit = Math.max(1, Math.min(filter.limit ?? 100, 500));
    values.push(limit);
    const rows = this.db
      .prepare(`SELECT * FROM tasks WHERE ${clauses.join(" AND ")} ORDER BY priority DESC, created_at ASC LIMIT ?`)
      .all(...values) as unknown as TaskRow[];
    return rows.map(taskFromRow);
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
      runs: runs.map(runFromRow),
      events: events.map(eventFromRow).reverse(),
    };
  }

  linkTasks(parentId: string, childId: string): TaskDetail {
    this.write(() => this.linkNoTransaction(parentId, childId));
    return this.getTask(childId);
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

  claimTask(
    input: {
      taskId?: string | undefined;
      board?: string | undefined;
      runtime?: Runtime | undefined;
      workerId?: string | undefined;
      excludeManual?: boolean | undefined;
    } = {},
  ): ClaimedTask | null {
    let runId = "";
    let claimToken = "";
    let taskId = "";
    const claimed = this.write(() => {
      const clauses = ["board = ?", "status = 'ready'", "current_run_id IS NULL"];
      const values: SQLInputValue[] = [input.board ?? "default"];
      if (input.taskId) {
        clauses.push("id = ?");
        values.push(input.taskId);
      }
      if (input.runtime) {
        clauses.push("runtime = ?");
        values.push(input.runtime);
      }
      if (input.excludeManual) clauses.push("runtime <> 'manual'");
      const row = this.db
        .prepare(`SELECT * FROM tasks WHERE ${clauses.join(" AND ")} ORDER BY priority DESC, created_at ASC LIMIT 1`)
        .get(...values) as TaskRow | undefined;
      if (!row) return false;
      if (this.hasOpenParents(row.id)) {
        this.db.prepare("UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?").run(now(), row.id);
        return false;
      }
      runId = newId("r");
      claimToken = randomBytes(24).toString("base64url");
      taskId = row.id;
      const timestamp = now();
      const changed = this.db
        .prepare("UPDATE tasks SET status = 'running', current_run_id = ?, updated_at = ? WHERE id = ? AND status = 'ready' AND current_run_id IS NULL")
        .run(runId, timestamp, row.id);
      if (changed.changes !== 1) return false;
      this.db
        .prepare(`
          INSERT INTO task_runs(
            id, task_id, worker_id, runtime, status, claim_token, claimed_at, heartbeat_at
          ) VALUES (?, ?, ?, ?, 'running', ?, ?, ?)
        `)
        .run(runId, row.id, input.workerId ?? `${row.runtime}-worker`, row.runtime, claimToken, timestamp, timestamp);
      this.appendEvent(row.id, "claimed", { workerId: input.workerId ?? `${row.runtime}-worker` }, runId);
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
      this.db.prepare("UPDATE task_runs SET heartbeat_at = ? WHERE id = ?").run(timestamp, run.id);
      this.db.prepare("UPDATE tasks SET updated_at = ? WHERE id = ?").run(timestamp, task.id);
      this.appendEvent(task.id, "heartbeat", note ? { note } : null, run.id);
    });
    const row = this.db.prepare("SELECT * FROM task_runs WHERE id = ?").get(scope.runId) as RunRow;
    return runFromRow(row);
  }

  completeRun(scope: RunScope, summary: string, metadata?: Record<string, unknown>): TaskDetail {
    const cleanSummary = summary.trim();
    if (!cleanSummary) throw new Error("Completion summary cannot be empty");
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const timestamp = now();
      this.db
        .prepare("UPDATE task_runs SET status = 'completed', ended_at = ?, heartbeat_at = ?, summary = ?, metadata_json = ? WHERE id = ?")
        .run(timestamp, timestamp, cleanSummary, metadata ? JSON.stringify(metadata) : null, run.id);
      this.db
        .prepare("UPDATE tasks SET status = 'done', current_run_id = NULL, failure_count = 0, updated_at = ? WHERE id = ?")
        .run(timestamp, task.id);
      this.appendEvent(task.id, "completed", { summary: cleanSummary, metadata: metadata ?? null }, run.id);
      const children = this.db.prepare("SELECT child_id FROM task_links WHERE parent_id = ?").all(task.id) as unknown as {
        child_id: string;
      }[];
      for (const child of children) this.recomputeReady(child.child_id);
    });
    return this.getTask(taskId);
  }

  blockRun(scope: RunScope, reason: string): TaskDetail {
    const cleanReason = reason.trim();
    if (!cleanReason) throw new Error("Block reason cannot be empty");
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const timestamp = now();
      this.db
        .prepare("UPDATE task_runs SET status = 'blocked', ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?")
        .run(timestamp, timestamp, cleanReason, run.id);
      this.db
        .prepare("UPDATE tasks SET status = 'blocked', current_run_id = NULL, updated_at = ? WHERE id = ?")
        .run(timestamp, task.id);
      this.appendEvent(task.id, "blocked", { reason: cleanReason }, run.id);
    });
    return this.getTask(taskId);
  }

  failRun(scope: RunScope, error: string): TaskDetail {
    let taskId = "";
    this.write(() => {
      const { task, run } = this.requireActiveRun(scope);
      taskId = task.id;
      const timestamp = now();
      const failures = task.failure_count + 1;
      const exhausted = failures >= task.max_retries;
      const nextStatus: TaskStatus = exhausted
        ? "blocked"
        : this.hasOpenParents(task.id) || task.assignee === null || task.runtime === "manual"
          ? "todo"
          : "ready";
      this.db
        .prepare("UPDATE task_runs SET status = 'failed', ended_at = ?, heartbeat_at = ?, error = ? WHERE id = ?")
        .run(timestamp, timestamp, error, run.id);
      this.db
        .prepare("UPDATE tasks SET status = ?, current_run_id = NULL, failure_count = ?, updated_at = ? WHERE id = ?")
        .run(nextStatus, failures, timestamp, task.id);
      this.appendEvent(task.id, exhausted ? "gave_up" : "requeued", { error, failures }, run.id);
    });
    return this.getTask(taskId);
  }

  unblockTask(taskId: string): TaskDetail {
    this.write(() => {
      const task = this.requireTaskRow(taskId);
      if (task.status !== "blocked") throw new Error(`Task is not blocked: ${task.status}`);
      this.db.prepare("UPDATE tasks SET status = 'todo', updated_at = ? WHERE id = ?").run(now(), taskId);
      this.appendEvent(taskId, "unblocked");
      this.recomputeReady(taskId);
    });
    return this.getTask(taskId);
  }
}
