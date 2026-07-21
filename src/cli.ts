#!/usr/bin/env node

import { parseArgs } from "node:util";
import { join, resolve } from "node:path";

import { runDispatcher } from "./dispatcher.js";
import { runStdioServer } from "./server.js";
import { KanbanStore } from "./store.js";
import {
  BLOCK_KINDS,
  RUNTIMES,
  TASK_STATUSES,
  type BlockKind,
  type ListTaskFilter,
  type Runtime,
  type TaskStatus,
} from "./types.js";

const HELP = `kanban-mcp <command> [options]

Commands:
  serve                 Run the stdio MCP server
  init                  Initialize the SQLite database
  create <title>        Create a task from the shell
  list                  List tasks
  show <task-id>        Show task details
  edit <task-id>        Edit task metadata
  assign <id> <worker>  Assign or unassign a task
  link <parent> <child> Add a dependency
  unlink <parent> <child> Remove a dependency
  comment <id> <text>   Append a durable comment
  complete <id>...      Complete one or more tasks
  block <id> <reason>   Block a task with an optional typed reason
  unblock <id>...       Return blocked tasks to the work queue
  promote <id>...       Promote parked tasks into the work queue
  schedule <id>         Park a task until a start time
  archive <id>...       Archive tasks
  delete <id>...        Permanently delete tasks
  dispatch              Run the Claude/Codex worker dispatcher

Common options:
  --db <path>           SQLite path (default: ./data/kanban.db)

Dispatch options:
  --once                Run at most one ready task, then exit
  --watch               Keep polling for work (default)
  --max-workers <n>     Parallel workers (default: 2)
  --interval-ms <n>     Idle poll interval (default: 2000)
  --allow-writes        Allow workspace edits and shell commands
`;

function defaultDbPath(): string {
  return resolve(process.env.KANBAN_DB ?? join(process.cwd(), "data", "kanban.db"));
}

function numberOption(value: string | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed)) throw new Error(`Invalid number: ${value}`);
  return parsed;
}

function requireRuntime(value: string | undefined): Runtime {
  const runtime = value ?? "manual";
  if (!RUNTIMES.includes(runtime as Runtime)) throw new Error(`Invalid runtime: ${runtime}`);
  return runtime as Runtime;
}

function requireStatus(value: string | undefined): TaskStatus | undefined {
  if (value === undefined) return undefined;
  if (!TASK_STATUSES.includes(value as TaskStatus)) throw new Error(`Invalid status: ${value}`);
  return value as TaskStatus;
}

function requireBlockKind(value: string | undefined): BlockKind | undefined {
  if (value === undefined) return undefined;
  if (!BLOCK_KINDS.includes(value as BlockKind)) throw new Error(`Invalid block kind: ${value}`);
  return value as BlockKind;
}

function parseMetadata(value: string | undefined): Record<string, unknown> | undefined {
  if (value === undefined) return undefined;
  const parsed = JSON.parse(value) as unknown;
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
    throw new Error("metadata must be a JSON object");
  }
  return parsed as Record<string, unknown>;
}

async function main(): Promise<void> {
  const [command, ...args] = process.argv.slice(2);
  if (!command || command === "help" || command === "--help" || command === "-h") {
    process.stdout.write(HELP);
    return;
  }

  if (command === "serve" || command === "init") {
    const parsed = parseArgs({ args, options: { db: { type: "string" } } });
    const dbPath = resolve(parsed.values.db ?? defaultDbPath());
    if (command === "serve") {
      await runStdioServer(dbPath);
      return;
    }
    const store = new KanbanStore(dbPath);
    store.close();
    process.stdout.write(`${dbPath}\n`);
    return;
  }

  if (command === "create") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        body: { type: "string" },
        board: { type: "string" },
        tenant: { type: "string" },
        "idempotency-key": { type: "string" },
        assignee: { type: "string" },
        runtime: { type: "string" },
        priority: { type: "string" },
        workspace: { type: "string" },
        "workspace-kind": { type: "string" },
        branch: { type: "string" },
        status: { type: "string" },
        "scheduled-at": { type: "string" },
        "max-runtime-seconds": { type: "string" },
        skill: { type: "string", multiple: true },
        goal: { type: "boolean" },
        "goal-max-turns": { type: "string" },
        parent: { type: "string", multiple: true },
        "max-retries": { type: "string" },
      },
    });
    const title = parsed.positionals.join(" ").trim();
    if (!title) throw new Error("create requires a title");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const task = store.createTask({
        title,
        body: parsed.values.body,
        board: parsed.values.board,
        tenant: parsed.values.tenant,
        idempotencyKey: parsed.values["idempotency-key"],
        assignee: parsed.values.assignee,
        runtime: requireRuntime(parsed.values.runtime),
        priority: numberOption(parsed.values.priority, 0),
        workspace: parsed.values.workspace,
        workspaceKind: parsed.values["workspace-kind"] as "scratch" | "dir" | "worktree" | undefined,
        branch: parsed.values.branch,
        status: requireStatus(parsed.values.status),
        scheduledAt: parsed.values["scheduled-at"],
        maxRuntimeSeconds: parsed.values["max-runtime-seconds"] === undefined
          ? undefined
          : numberOption(parsed.values["max-runtime-seconds"], 0),
        skills: parsed.values.skill,
        goalMode: parsed.values.goal,
        goalMaxTurns: numberOption(parsed.values["goal-max-turns"], 20),
        parents: parsed.values.parent,
        maxRetries: numberOption(parsed.values["max-retries"], 2),
      });
      process.stdout.write(`${JSON.stringify(task, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "list") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        board: { type: "string" },
        status: { type: "string" },
        tenant: { type: "string" },
        assignee: { type: "string" },
        runtime: { type: "string" },
        archived: { type: "boolean" },
        search: { type: "string" },
        sort: { type: "string" },
      },
    });
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const tasks = store.listTasks({
        board: parsed.values.board,
        status: requireStatus(parsed.values.status),
        tenant: parsed.values.tenant,
        assignee: parsed.values.assignee,
        runtime: parsed.values.runtime ? requireRuntime(parsed.values.runtime) : undefined,
        includeArchived: parsed.values.archived,
        search: parsed.values.search,
        sort: parsed.values.sort as ListTaskFilter["sort"],
      });
      process.stdout.write(`${JSON.stringify(tasks, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "show") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    const taskId = parsed.positionals[0];
    if (!taskId) throw new Error("show requires a task id");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      process.stdout.write(`${JSON.stringify(store.getTask(taskId), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "edit") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        title: { type: "string" },
        body: { type: "string" },
        tenant: { type: "string" },
        assignee: { type: "string" },
        runtime: { type: "string" },
        priority: { type: "string" },
        workspace: { type: "string" },
        "workspace-kind": { type: "string" },
        branch: { type: "string" },
        status: { type: "string" },
        "scheduled-at": { type: "string" },
        "max-runtime-seconds": { type: "string" },
        skill: { type: "string", multiple: true },
        goal: { type: "boolean" },
        "goal-max-turns": { type: "string" },
      },
    });
    const taskId = parsed.positionals[0];
    if (!taskId) throw new Error("edit requires a task id");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const detail = store.updateTask(taskId, {
        title: parsed.values.title,
        body: parsed.values.body,
        tenant: parsed.values.tenant,
        assignee: parsed.values.assignee,
        runtime: parsed.values.runtime ? requireRuntime(parsed.values.runtime) : undefined,
        priority: parsed.values.priority === undefined ? undefined : numberOption(parsed.values.priority, 0),
        workspace: parsed.values.workspace,
        workspaceKind: parsed.values["workspace-kind"] as "scratch" | "dir" | "worktree" | undefined,
        branch: parsed.values.branch,
        status: requireStatus(parsed.values.status),
        scheduledAt: parsed.values["scheduled-at"],
        maxRuntimeSeconds: parsed.values["max-runtime-seconds"] === undefined
          ? undefined
          : numberOption(parsed.values["max-runtime-seconds"], 0),
        skills: parsed.values.skill,
        goalMode: parsed.values.goal,
        goalMaxTurns: parsed.values["goal-max-turns"] === undefined
          ? undefined
          : numberOption(parsed.values["goal-max-turns"], 20),
      });
      process.stdout.write(`${JSON.stringify(detail, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "assign") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    const [taskId, assignee] = parsed.positionals;
    if (!taskId || !assignee) throw new Error("assign requires a task id and assignee (or 'none')");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      process.stdout.write(`${JSON.stringify(store.updateTask(taskId, { assignee: assignee === "none" ? null : assignee }), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "link" || command === "unlink") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    const [parentId, childId] = parsed.positionals;
    if (!parentId || !childId) throw new Error(`${command} requires parent and child task ids`);
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const detail = command === "link" ? store.linkTasks(parentId, childId) : store.unlinkTasks(parentId, childId);
      process.stdout.write(`${JSON.stringify(detail, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "comment") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, author: { type: "string" } },
    });
    const [taskId, ...bodyParts] = parsed.positionals;
    const body = bodyParts.join(" ").trim();
    if (!taskId || !body) throw new Error("comment requires a task id and text");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      process.stdout.write(`${JSON.stringify(store.addComment(taskId, parsed.values.author ?? "human", body), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "complete") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        summary: { type: "string" },
        result: { type: "string" },
        metadata: { type: "string" },
      },
    });
    if (parsed.positionals.length === 0) throw new Error("complete requires at least one task id");
    if (parsed.positionals.length > 1 && (parsed.values.summary || parsed.values.result || parsed.values.metadata)) {
      throw new Error("Structured completion handoff is only allowed for one task at a time");
    }
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const completed = parsed.positionals.map((taskId) =>
        store.completeTask(taskId, {
          summary: parsed.values.summary,
          result: parsed.values.result,
          metadata: parseMetadata(parsed.values.metadata),
        })
      );
      process.stdout.write(`${JSON.stringify(completed, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "block") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, kind: { type: "string" } },
    });
    const [taskId, ...reasonParts] = parsed.positionals;
    const reason = reasonParts.join(" ").trim();
    if (!taskId || !reason) throw new Error("block requires a task id and reason");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      process.stdout.write(`${JSON.stringify(store.blockTask(taskId, { reason, kind: requireBlockKind(parsed.values.kind) }), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (["unblock", "promote", "archive", "delete"].includes(command)) {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    if (parsed.positionals.length === 0) throw new Error(`${command} requires at least one task id`);
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      const results = parsed.positionals.map((taskId) => {
        if (command === "unblock") return store.unblockTask(taskId);
        if (command === "promote") return store.promoteTask(taskId);
        if (command === "archive") return store.archiveTask(taskId);
        return store.deleteTask(taskId);
      });
      process.stdout.write(`${JSON.stringify(results, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "schedule") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, at: { type: "string" }, reason: { type: "string" } },
    });
    const taskId = parsed.positionals[0];
    if (!taskId) throw new Error("schedule requires a task id");
    const store = new KanbanStore(resolve(parsed.values.db ?? defaultDbPath()));
    try {
      process.stdout.write(`${JSON.stringify(store.scheduleTask(taskId, parsed.values.at ?? null, parsed.values.reason), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "dispatch") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        board: { type: "string" },
        once: { type: "boolean" },
        watch: { type: "boolean" },
        "max-workers": { type: "string" },
        "interval-ms": { type: "string" },
        "allow-writes": { type: "boolean" },
      },
    });
    const controller = new AbortController();
    process.once("SIGINT", () => controller.abort());
    process.once("SIGTERM", () => controller.abort());
    await runDispatcher({
      dbPath: resolve(parsed.values.db ?? defaultDbPath()),
      cliEntry: resolve(process.argv[1] ?? "dist/cli.js"),
      board: parsed.values.board,
      once: parsed.values.once ?? false,
      intervalMs: numberOption(parsed.values["interval-ms"], 2_000),
      maxWorkers: numberOption(parsed.values["max-workers"], 2),
      allowWrites: parsed.values["allow-writes"] ?? false,
      signal: controller.signal,
      onLog: (message) => process.stderr.write(`[kanban] ${message}\n`),
    });
    return;
  }

  throw new Error(`Unknown command: ${command}`);
}

main().catch((error: unknown) => {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`kanban-mcp: ${message}\n`);
  process.exitCode = 1;
});
