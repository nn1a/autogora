#!/usr/bin/env node

import { parseArgs } from "node:util";
import { join, resolve } from "node:path";

import { runDispatcher } from "./dispatcher.js";
import { runStdioServer } from "./server.js";
import { KanbanStore } from "./store.js";
import { RUNTIMES, TASK_STATUSES, type ListTaskFilter, type Runtime, type TaskStatus } from "./types.js";

const HELP = `kanban-mcp <command> [options]

Commands:
  serve                 Run the stdio MCP server
  init                  Initialize the SQLite database
  create <title>        Create a task from the shell
  list                  List tasks
  show <task-id>        Show task details
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
