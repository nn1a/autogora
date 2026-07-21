#!/usr/bin/env node

import { parseArgs } from "node:util";
import { join, resolve } from "node:path";

import { BoardManager } from "./boards.js";
import { runDispatcher } from "./dispatcher.js";
import { garbageCollect } from "./maintenance.js";
import { deliverNotifications } from "./notifications.js";
import { runStdioServer } from "./server.js";
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
  boards <action>       List, create, switch, edit, archive, or delete boards
  create <title>        Create a task from the shell
  list                  List tasks
  show <task-id>        Show task details
  context <task-id>     Print the bounded worker context
  runs <task-id>        Show attempt history
  log <task-id>         Read the latest worker log tail
  stats                 Show board counts
  diagnostics           Inspect board health and active workers
  tail <task-id>        Read or follow one task's events
  watch                 Read or follow the board event stream
  bulk <id>...          Apply a mutation with per-task results
  gc                     Collect old events, logs, and terminal scratch dirs
  notify-subscribe <id> Subscribe a destination to task terminal events
  notify-list [id]      List board or task notification subscriptions
  notify-unsubscribe <id> Remove a task notification destination
  notify-deliver        Deliver pending notifications once
  edit <task-id>        Edit task metadata
  assign <id> <worker>  Assign or unassign a task
  link <parent> <child> Add a dependency
  unlink <parent> <child> Remove a dependency
  comment <id> <text>   Append a durable comment
  attach <id> <path>    Copy a file into durable attachment storage
  attach-url <id> <url> Attach an HTTP(S) reference
  attachments <id>      List task attachments
  attach-rm <id> <aid>  Remove an attachment
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
  --board <slug>        Override the current board for this command

Dispatch options:
  --once                Run at most one ready task, then exit
  --watch               Keep polling for work (default)
  --max-workers <n>     Parallel workers (default: 2)
  --max-in-progress <n> Board-wide running task cap
  --max-per-assignee <n> Running task cap per assignee
  --claim-ttl-seconds <n> Claim lease duration (default: 900)
  --interval-ms <n>     Idle poll interval (default: 2000)
  --allow-writes        Allow workspace edits and shell commands
`;

function defaultDbPath(): string {
  return resolve(process.env.KANBAN_DB ?? join(process.cwd(), "data", "kanban.db"));
}

function managerFor(dbPath?: string): BoardManager {
  return new BoardManager(resolve(dbPath ?? defaultDbPath()));
}

function openTaskStore(dbPath: string | undefined, board?: string) {
  const manager = managerFor(dbPath);
  return manager.openStore(manager.resolve(board));
}

function extractBoardOption(values: string[]): { args: string[]; board: string | undefined } {
  const args: string[] = [];
  let board: string | undefined;
  for (let index = 0; index < values.length; index += 1) {
    const value = values[index];
    if (value === "--board") {
      const next = values[index + 1];
      if (!next) throw new Error("--board requires a slug");
      board = next;
      index += 1;
    } else if (value?.startsWith("--board=")) {
      board = value.slice("--board=".length);
    } else if (value !== undefined) {
      args.push(value);
    }
  }
  return { args, board };
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

function pause(milliseconds: number): Promise<void> {
  return new Promise((resolvePause) => setTimeout(resolvePause, milliseconds));
}

async function main(): Promise<void> {
  const [command, ...rawArgs] = process.argv.slice(2);
  const { args, board: globalBoard } = extractBoardOption(rawArgs);
  if (!command || command === "help" || command === "--help" || command === "-h") {
    process.stdout.write(HELP);
    return;
  }

  if (command === "boards") {
    const [action, ...boardArgs] = args;
    if (!action) throw new Error("boards requires list, create, switch, show, rename, or rm");
    if (action === "list" || action === "ls") {
      const parsed = parseArgs({
        args: boardArgs,
        options: { db: { type: "string" }, all: { type: "boolean" } },
      });
      process.stdout.write(`${JSON.stringify(managerFor(parsed.values.db).list(parsed.values.all), null, 2)}\n`);
      return;
    }
    if (action === "create") {
      const parsed = parseArgs({
        args: boardArgs,
        allowPositionals: true,
        options: {
          db: { type: "string" },
          name: { type: "string" },
          description: { type: "string" },
          icon: { type: "string" },
          color: { type: "string" },
          "default-workdir": { type: "string" },
          switch: { type: "boolean" },
        },
      });
      const slug = parsed.positionals[0];
      if (!slug) throw new Error("boards create requires a slug");
      const manager = managerFor(parsed.values.db);
      const metadata = manager.create(slug, {
        name: parsed.values.name,
        description: parsed.values.description,
        icon: parsed.values.icon,
        color: parsed.values.color,
        defaultWorkdir: parsed.values["default-workdir"],
      });
      if (parsed.values.switch) manager.switch(metadata.slug);
      process.stdout.write(`${JSON.stringify(metadata, null, 2)}\n`);
      return;
    }
    if (["switch", "use"].includes(action)) {
      const parsed = parseArgs({ args: boardArgs, allowPositionals: true, options: { db: { type: "string" } } });
      const slug = parsed.positionals[0];
      if (!slug) throw new Error("boards switch requires a slug");
      process.stdout.write(`${JSON.stringify(managerFor(parsed.values.db).switch(slug), null, 2)}\n`);
      return;
    }
    if (action === "show" || action === "current") {
      const parsed = parseArgs({ args: boardArgs, allowPositionals: true, options: { db: { type: "string" } } });
      const manager = managerFor(parsed.values.db);
      const slug = parsed.positionals[0] ?? manager.getCurrent();
      const metadata = manager.read(manager.resolve(slug));
      const store = manager.openStore(metadata.slug);
      try {
        process.stdout.write(`${JSON.stringify({ ...metadata, counts: store.countTasksByStatus() }, null, 2)}\n`);
      } finally {
        store.close();
      }
      return;
    }
    if (action === "rename") {
      const parsed = parseArgs({ args: boardArgs, allowPositionals: true, options: { db: { type: "string" } } });
      const [slug, ...nameParts] = parsed.positionals;
      const name = nameParts.join(" ").trim();
      if (!slug || !name) throw new Error("boards rename requires a slug and display name");
      process.stdout.write(`${JSON.stringify(managerFor(parsed.values.db).update(slug, { name }), null, 2)}\n`);
      return;
    }
    if (action === "rm" || action === "remove") {
      const parsed = parseArgs({
        args: boardArgs,
        allowPositionals: true,
        options: { db: { type: "string" }, delete: { type: "boolean" } },
      });
      const slug = parsed.positionals[0];
      if (!slug) throw new Error("boards rm requires a slug");
      process.stdout.write(`${JSON.stringify(managerFor(parsed.values.db).remove(slug, parsed.values.delete), null, 2)}\n`);
      return;
    }
    throw new Error(`Unknown boards action: ${action}`);
  }

  if (command === "serve" || command === "init") {
    const parsed = parseArgs({ args, options: { db: { type: "string" } } });
    const dbPath = resolve(parsed.values.db ?? defaultDbPath());
    if (command === "serve") {
      await runStdioServer(dbPath);
      return;
    }
    const manager = new BoardManager(dbPath);
    manager.create("default");
    const store = manager.openStore("default");
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
    const manager = managerFor(parsed.values.db);
    const board = manager.resolve(globalBoard);
    const store = manager.openStore(board);
    try {
      const task = store.createTask({
        title,
        body: parsed.values.body,
        board,
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
    const manager = managerFor(parsed.values.db);
    const board = manager.resolve(globalBoard);
    const store = manager.openStore(board);
    try {
      const tasks = store.listTasks({
        board,
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
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      process.stdout.write(`${JSON.stringify(store.getTask(taskId), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (["context", "runs", "log"].includes(command)) {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        run: { type: "string" },
        "tail-bytes": { type: "string" },
      },
    });
    const taskId = parsed.positionals[0];
    if (!taskId) throw new Error(`${command} requires a task id`);
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      if (command === "context") process.stdout.write(`${store.buildWorkerContext(taskId)}\n`);
      else if (command === "runs") process.stdout.write(`${JSON.stringify(store.getTask(taskId).runs, null, 2)}\n`);
      else {
        const log = store.readRunLog(taskId, numberOption(parsed.values["tail-bytes"], 64 * 1_024), parsed.values.run);
        process.stdout.write(log.text.endsWith("\n") ? log.text : `${log.text}\n`);
      }
    } finally {
      store.close();
    }
    return;
  }

  if (["stats", "diagnostics", "diag", "assignees"].includes(command)) {
    const parsed = parseArgs({ args, options: { db: { type: "string" } } });
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const output = command === "stats"
        ? store.getStats()
        : command === "assignees"
          ? store.getStats().byAssignee
          : store.diagnose();
      process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "tail" || command === "watch") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        since: { type: "string" },
        kinds: { type: "string" },
        limit: { type: "string" },
        follow: { type: "boolean" },
        "interval-ms": { type: "string" },
      },
    });
    const taskId = command === "tail" ? parsed.positionals[0] : undefined;
    if (command === "tail" && !taskId) throw new Error("tail requires a task id");
    const store = openTaskStore(parsed.values.db, globalBoard);
    let stopped = false;
    const stop = (): void => {
      stopped = true;
    };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
    try {
      let cursor = numberOption(parsed.values.since, 0);
      const kinds = parsed.values.kinds?.split(",").map((kind) => kind.trim()).filter(Boolean);
      do {
        const events = store.listEvents({
          taskId,
          sinceId: cursor,
          kinds,
          limit: numberOption(parsed.values.limit, 500),
        });
        for (const event of events) {
          process.stdout.write(`${JSON.stringify(event)}\n`);
          cursor = Math.max(cursor, event.id);
        }
        if (!parsed.values.follow || stopped) break;
        await pause(Math.max(100, numberOption(parsed.values["interval-ms"], 1_000)));
      } while (!stopped);
    } finally {
      process.removeListener("SIGINT", stop);
      process.removeListener("SIGTERM", stop);
      store.close();
    }
    return;
  }

  if (command === "bulk") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        status: { type: "string" },
        assignee: { type: "string" },
        priority: { type: "string" },
        archive: { type: "boolean" },
        delete: { type: "boolean" },
      },
    });
    if (parsed.positionals.length === 0) throw new Error("bulk requires at least one task id");
    if (
      parsed.values.status === undefined && parsed.values.assignee === undefined &&
      parsed.values.priority === undefined && !parsed.values.archive && !parsed.values.delete
    ) {
      throw new Error("bulk requires --status, --assignee, --priority, --archive, or --delete");
    }
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const output = store.bulkMutate(parsed.positionals, {
        status: requireStatus(parsed.values.status),
        assignee: parsed.values.assignee === undefined
          ? undefined
          : parsed.values.assignee === "none" ? null : parsed.values.assignee,
        priority: parsed.values.priority === undefined ? undefined : numberOption(parsed.values.priority, 0),
        archive: parsed.values.archive,
        delete: parsed.values.delete,
      });
      process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "gc") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        "event-retention-days": { type: "string" },
        "log-retention-days": { type: "string" },
        "workspace-retention-days": { type: "string" },
      },
    });
    const manager = managerFor(parsed.values.db);
    const board = manager.resolve(globalBoard);
    const output = garbageCollect(manager, board, {
      eventRetentionDays: numberOption(parsed.values["event-retention-days"], 30),
      logRetentionDays: numberOption(parsed.values["log-retention-days"], 30),
      workspaceRetentionDays: numberOption(parsed.values["workspace-retention-days"], 7),
    });
    process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
    return;
  }

  if (["notify-subscribe", "notify-unsubscribe"].includes(command)) {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        platform: { type: "string" },
        "chat-id": { type: "string" },
        "thread-id": { type: "string" },
        "user-id": { type: "string" },
        kinds: { type: "string" },
        secret: { type: "string" },
        "clear-secret": { type: "boolean" },
      },
    });
    const taskId = parsed.positionals[0];
    const platform = parsed.values.platform;
    const chatId = parsed.values["chat-id"];
    if (!taskId || !platform || !chatId) {
      throw new Error(`${command} requires a task id, --platform, and --chat-id`);
    }
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      if (command === "notify-subscribe") {
        const output = store.subscribeTask({
          taskId,
          platform,
          chatId,
          threadId: parsed.values["thread-id"],
          userId: parsed.values["user-id"],
          eventKinds: parsed.values.kinds?.split(",").map((kind) => kind.trim()).filter(Boolean),
          secret: parsed.values["clear-secret"] ? null : parsed.values.secret,
        });
        process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
      } else {
        const unsubscribed = store.unsubscribeTask({
          taskId,
          platform,
          chatId,
          threadId: parsed.values["thread-id"],
        });
        process.stdout.write(`${JSON.stringify({ taskId, unsubscribed }, null, 2)}\n`);
      }
    } finally {
      store.close();
    }
    return;
  }

  if (command === "notify-list") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      process.stdout.write(`${JSON.stringify(store.listNotificationSubscriptions(parsed.positionals[0]), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "notify-deliver") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        limit: { type: "string" },
        "timeout-ms": { type: "string" },
      },
    });
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const output = await deliverNotifications(store, {
        limit: numberOption(parsed.values.limit, 25),
        timeoutMs: numberOption(parsed.values["timeout-ms"], 10_000),
      });
      process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      process.stdout.write(`${JSON.stringify(store.addComment(taskId, parsed.values.author ?? "human", body), null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (["attach", "attach-url", "attachments", "attach-rm"].includes(command)) {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, name: { type: "string" } },
    });
    const [taskId, value] = parsed.positionals;
    if (!taskId) throw new Error(`${command} requires a task id`);
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      let output: unknown;
      if (command === "attachments") output = store.listAttachments(taskId);
      else if (!value) throw new Error(`${command} requires a path, URL, or attachment id`);
      else if (command === "attach") output = store.attachFile(taskId, value, parsed.values.name);
      else if (command === "attach-url") output = store.attachUrl(taskId, value, parsed.values.name);
      else output = store.removeAttachment(taskId, value);
      process.stdout.write(`${JSON.stringify(output, null, 2)}\n`);
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
        artifact: { type: "string", multiple: true },
      },
    });
    if (parsed.positionals.length === 0) throw new Error("complete requires at least one task id");
    if (
      parsed.positionals.length > 1 &&
      (parsed.values.summary || parsed.values.result || parsed.values.metadata || parsed.values.artifact)
    ) {
      throw new Error("Structured completion handoff is only allowed for one task at a time");
    }
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const completed = parsed.positionals.map((taskId) =>
        store.completeTask(taskId, {
          summary: parsed.values.summary,
          result: parsed.values.result,
          metadata: parseMetadata(parsed.values.metadata),
          artifacts: parsed.values.artifact,
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
    const store = openTaskStore(parsed.values.db, globalBoard);
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
        "max-in-progress": { type: "string" },
        "max-per-assignee": { type: "string" },
        "claim-ttl-seconds": { type: "string" },
        "stale-timeout-seconds": { type: "string" },
        "heartbeat-max-stale-seconds": { type: "string" },
        "crash-grace-seconds": { type: "string" },
        "rate-limit-cooldown-seconds": { type: "string" },
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
      board: globalBoard,
      once: parsed.values.once ?? false,
      intervalMs: numberOption(parsed.values["interval-ms"], 2_000),
      maxWorkers: numberOption(parsed.values["max-workers"], 2),
      maxInProgress: parsed.values["max-in-progress"] === undefined
        ? undefined
        : numberOption(parsed.values["max-in-progress"], 1),
      maxInProgressPerAssignee: parsed.values["max-per-assignee"] === undefined
        ? undefined
        : numberOption(parsed.values["max-per-assignee"], 1),
      claimTtlSeconds: numberOption(parsed.values["claim-ttl-seconds"], 900),
      staleTimeoutSeconds: numberOption(parsed.values["stale-timeout-seconds"], 4 * 60 * 60),
      heartbeatMaxStaleSeconds: numberOption(parsed.values["heartbeat-max-stale-seconds"], 60 * 60),
      crashGraceSeconds: numberOption(parsed.values["crash-grace-seconds"], 30),
      rateLimitCooldownSeconds: numberOption(parsed.values["rate-limit-cooldown-seconds"], 60),
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
