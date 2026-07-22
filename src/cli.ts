#!/usr/bin/env node

import { parseArgs } from "node:util";
import { join, resolve } from "node:path";

import { BoardManager } from "./boards.js";
import { runDispatcher } from "./dispatcher.js";
import { startDashboardServer } from "./http.js";
import { garbageCollect } from "./maintenance.js";
import { deliverNotifications } from "./notifications.js";
import {
  createCliPlanner,
  decomposeTriageTask,
  specifyTriageTask,
  type DecompositionPlan,
  type ProfileRoute,
} from "./orchestration.js";
import { runStdioServer } from "./server.js";
import { WorkspaceManager } from "./workspaces.js";
import {
  BLOCK_KINDS,
  PLANNER_RUNTIMES,
  RUNTIMES,
  TASK_STATUSES,
  WORKER_RUNTIMES,
  type BlockKind,
  type ListTaskFilter,
  type PlannerRuntime,
  type Runtime,
  type TaskStatus,
  type WorkerRuntime,
} from "./types.js";

const HELP = `taskcircuit <command> [options]

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
  specify <id>          Expand a triage idea into an executable specification
  decompose <id>        Expand a triage idea into an atomic task graph
  swarm <goal>          Create a blackboard/worker/verifier/synthesizer graph
  edit <task-id>        Edit task metadata
  assign <id> <worker>  Assign or unassign a task
  reassign <id>... <worker> Bulk assign or unassign tasks
  link <parent> <child> Add a dependency
  unlink <parent> <child> Remove a dependency
  claim <task-id>       Atomically claim and prepare a ready task
  heartbeat <task-id>   Refresh the active run lease
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
  dispatch              Run the Claude/Codex/Cline/Gemini worker dispatcher
  daemon --force        Run the deprecated standalone dispatcher alias
  dashboard             Run the authenticated local web dashboard

Common options:
  --db <path>           SQLite path (default: ./data/kanban.db)
  --board <slug>        Override the current board for this command

Dispatch options:
  --once                Run at most one ready task, then exit
  --dry-run             Preview claimable tasks without mutating the board
  --max <n>             Alias for --max-workers
  --failure-limit <n>   Override the run failure circuit breaker
  --watch               Keep polling for work (default)
  --max-workers <n>     Parallel workers (default: 2)
  --max-in-progress <n> Board-wide running task cap
  --max-per-assignee <n> Running task cap per assignee
  --claim-ttl-seconds <n> Claim lease duration (default: 900)
  --interval-ms <n>     Idle poll interval (default: 2000)
  --allow-writes        Allow workspace edits and shell commands
  --auto-decompose      Use the auxiliary planner for triage cards
  --auto-decompose-per-tick <n> Limit planner calls per dispatcher tick
  --profile <route>     Repeat name:runtime:description routes for decomposition
`;

function defaultDbPath(): string {
  return resolve(process.env.KANBAN_DB ?? join(process.cwd(), "data", "kanban.db"));
}

function managerFor(dbPath?: string): BoardManager {
  const resolved = resolve(dbPath ?? defaultDbPath());
  const pinned = process.env.KANBAN_TASK_ID ? process.env.KANBAN_DB?.trim() : undefined;
  if (pinned && resolved !== resolve(pinned)) throw new Error(`This worker is scoped to database ${resolve(pinned)}`);
  return new BoardManager(resolved);
}

function openTaskStore(dbPath: string | undefined, board?: string) {
  const manager = managerFor(dbPath);
  return manager.openStore(manager.resolve(board));
}

function scopedCliTaskId(requested: string | undefined, command: string): string {
  const pinned = process.env.KANBAN_TASK_ID?.trim();
  if (pinned && requested && pinned !== requested) throw new Error(`${command} is scoped to task ${pinned}`);
  const taskId = pinned ?? requested;
  if (!taskId) throw new Error(`${command} requires a task id`);
  return taskId;
}

function scopedCliRun(): { runId: string; claimToken: string } | null {
  const runId = process.env.KANBAN_RUN_ID?.trim();
  const claimToken = process.env.KANBAN_CLAIM_TOKEN?.trim();
  if (!runId && !claimToken) return null;
  if (!runId || !claimToken) throw new Error("Scoped worker commands require KANBAN_RUN_ID and KANBAN_CLAIM_TOKEN");
  return { runId, claimToken };
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

function durationSeconds(value: string): number {
  const match = /^(\d+)(s|m|h|d)?$/i.exec(value.trim());
  if (!match) throw new Error(`Invalid duration: ${value}`);
  const amount = Number.parseInt(match[1]!, 10);
  const multiplier = { s: 1, m: 60, h: 60 * 60, d: 24 * 60 * 60 }[(match[2] ?? "s").toLowerCase() as "s" | "m" | "h" | "d"];
  if (!Number.isSafeInteger(amount) || amount < 1) throw new Error(`Invalid duration: ${value}`);
  const seconds = amount * multiplier;
  if (!Number.isSafeInteger(seconds)) throw new Error(`Duration is too large: ${value}`);
  return seconds;
}

function requireRuntime(value: string | undefined): Runtime {
  const runtime = value ?? "manual";
  if (!RUNTIMES.includes(runtime as Runtime)) throw new Error(`Invalid runtime: ${runtime}`);
  return runtime as Runtime;
}

function requirePlannerRuntime(value: string | undefined): PlannerRuntime {
  const runtime = (value ?? "codex") as PlannerRuntime;
  if (!PLANNER_RUNTIMES.includes(runtime)) throw new Error(`Invalid planner runtime: ${runtime}`);
  return runtime;
}

function parseProfileRoute(value: string, fallbackRuntime: WorkerRuntime = "codex"): ProfileRoute {
  const [rawName, rawRuntime, ...descriptionParts] = value.split(":");
  const name = rawName?.trim();
  if (!name) throw new Error(`Invalid profile route: ${value}`);
  const runtime = rawRuntime?.trim() || fallbackRuntime;
  if (!WORKER_RUNTIMES.includes(runtime as WorkerRuntime)) {
    throw new Error(`Invalid profile runtime in ${value}`);
  }
  return { name, runtime: runtime as WorkerRuntime, description: descriptionParts.join(":").trim() || undefined };
}

function requireStatus(value: string | undefined): TaskStatus | undefined {
  if (value === undefined) return undefined;
  if (!TASK_STATUSES.includes(value as TaskStatus)) throw new Error(`Invalid status: ${value}`);
  return value as TaskStatus;
}

function requireSort(value: string | undefined): ListTaskFilter["sort"] {
  if (value === undefined) return undefined;
  const sorts: NonNullable<ListTaskFilter["sort"]>[] = [
    "created", "created-desc", "priority", "priority-desc", "status", "assignee", "title", "updated",
  ];
  if (!sorts.includes(value as NonNullable<ListTaskFilter["sort"]>)) throw new Error(`Invalid sort: ${value}`);
  return value as ListTaskFilter["sort"];
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
  const { args, board: requestedBoard } = extractBoardOption(rawArgs);
  const pinnedBoard = process.env.KANBAN_BOARD?.trim();
  if (pinnedBoard && requestedBoard && pinnedBoard !== requestedBoard.trim().toLowerCase()) {
    throw new Error(`This worker is scoped to board ${pinnedBoard}`);
  }
  const globalBoard = pinnedBoard ?? requestedBoard;
  if (!command || command === "help" || command === "--help" || command === "-h") {
    process.stdout.write(HELP);
    return;
  }
  if (
    process.env.KANBAN_TASK_ID &&
    !["serve", "show", "context", "runs", "log", "heartbeat", "comment", "complete", "block"].includes(command)
  ) {
    throw new Error("Dispatcher-scoped workers may only use Kanban CLI context and lifecycle commands");
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
        triage: { type: "boolean" },
        "scheduled-at": { type: "string" },
        "max-runtime": { type: "string" },
        "max-runtime-seconds": { type: "string" },
        skill: { type: "string", multiple: true },
        goal: { type: "boolean" },
        "goal-max-turns": { type: "string" },
        "workflow-template-id": { type: "string" },
        "current-step-key": { type: "string" },
        parent: { type: "string", multiple: true },
        "max-retries": { type: "string" },
      },
    });
    const title = parsed.positionals.join(" ").trim();
    if (!title) throw new Error("create requires a title");
    if (parsed.values.triage && parsed.values.status && parsed.values.status !== "triage") {
      throw new Error("--triage cannot be combined with a different --status");
    }
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
        status: parsed.values.triage ? "triage" : requireStatus(parsed.values.status),
        scheduledAt: parsed.values["scheduled-at"],
        maxRuntimeSeconds: parsed.values["max-runtime"] !== undefined
          ? durationSeconds(parsed.values["max-runtime"])
          : parsed.values["max-runtime-seconds"] === undefined
            ? undefined
            : durationSeconds(parsed.values["max-runtime-seconds"]),
        skills: parsed.values.skill,
        goalMode: parsed.values.goal,
        goalMaxTurns: numberOption(parsed.values["goal-max-turns"], 20),
        workflowTemplateId: parsed.values["workflow-template-id"],
        currentStepKey: parsed.values["current-step-key"],
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
        mine: { type: "boolean" },
        runtime: { type: "string" },
        "workflow-template-id": { type: "string" },
        "current-step-key": { type: "string" },
        archived: { type: "boolean" },
        search: { type: "string" },
        sort: { type: "string" },
        limit: { type: "string" },
      },
    });
    const manager = managerFor(parsed.values.db);
    const board = manager.resolve(globalBoard);
    const store = manager.openStore(board);
    try {
      if (parsed.values.mine && parsed.values.assignee) throw new Error("--mine and --assignee cannot be combined");
      const mine = parsed.values.mine
        ? process.env.KANBAN_PROFILE ?? process.env.HERMES_PROFILE ?? process.env.KANBAN_WORKER_ID
        : undefined;
      if (parsed.values.mine && !mine) throw new Error("--mine requires KANBAN_PROFILE, HERMES_PROFILE, or KANBAN_WORKER_ID");
      const tasks = store.listTasks({
        board,
        status: requireStatus(parsed.values.status),
        tenant: parsed.values.tenant,
        assignee: mine ?? parsed.values.assignee,
        runtime: parsed.values.runtime ? requireRuntime(parsed.values.runtime) : undefined,
        workflowTemplateId: parsed.values["workflow-template-id"],
        currentStepKey: parsed.values["current-step-key"],
        includeArchived: parsed.values.archived,
        search: parsed.values.search,
        sort: requireSort(parsed.values.sort),
        limit: numberOption(parsed.values.limit, 100),
      });
      process.stdout.write(`${JSON.stringify(tasks, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "show") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    const taskId = scopedCliTaskId(parsed.positionals[0], "show");
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
    const taskId = scopedCliTaskId(parsed.positionals[0], command);
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

  if (command === "specify" || command === "decompose") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        all: { type: "boolean" },
        tenant: { type: "string" },
        title: { type: "string" },
        body: { type: "string" },
        author: { type: "string" },
        "planner-runtime": { type: "string" },
        "planner-timeout-ms": { type: "string" },
        "plan-json": { type: "string" },
        profile: { type: "string", multiple: true },
        "default-profile": { type: "string" },
        "orchestrator-profile": { type: "string" },
      },
    });
    const requestedId = parsed.positionals[0];
    if (!requestedId && !parsed.values.all) throw new Error(`${command} requires a task id or --all`);
    if (requestedId && parsed.values.all) throw new Error(`${command} accepts a task id or --all, not both`);
    if ((parsed.values.title === undefined) !== (parsed.values.body === undefined)) {
      throw new Error("--title and --body must be provided together");
    }
    if (parsed.values.all && parsed.values.title !== undefined) {
      throw new Error("An explicit specification cannot be reused with --all");
    }
    const plannerRuntime = requirePlannerRuntime(parsed.values["planner-runtime"]);
    const orchestrationManager = managerFor(parsed.values.db);
    const orchestrationBoard = orchestrationManager.resolve(globalBoard);
    const orchestrationSettings = orchestrationManager.read(orchestrationBoard).orchestration;
    const store = orchestrationManager.openStore(orchestrationBoard);
    try {
      const taskIds = requestedId
        ? [requestedId]
        : store.listTasks({ status: "triage", tenant: parsed.values.tenant, limit: 500 }).map((task) => task.id);
      const planner = createCliPlanner({
        runtime: plannerRuntime,
        cwd: process.cwd(),
        timeoutMs: numberOption(parsed.values["planner-timeout-ms"], 120_000),
      });
      const explicitProfiles = (parsed.values.profile ?? []).map((profile) => parseProfileRoute(profile, plannerRuntime));
      const discoveredProfiles = store.listTasks({ includeArchived: true, limit: 500 })
        .filter((task) => task.assignee && task.runtime !== "manual")
        .map((task) => ({
          name: task.assignee!,
          runtime: task.runtime as WorkerRuntime,
        } satisfies ProfileRoute));
      const profiles = [...new Map([...discoveredProfiles, ...explicitProfiles].map((profile) => [profile.name, profile])).values()];
      const explicitPlan = parsed.values["plan-json"]
        ? JSON.parse(parsed.values["plan-json"]) as DecompositionPlan
        : undefined;
      const results: Array<{ taskId: string; ok: boolean; value?: unknown; error?: string }> = [];
      for (const taskId of taskIds) {
        try {
          if (command === "specify") {
            const value = await specifyTriageTask(store, taskId, {
              planner,
              specification: parsed.values.title && parsed.values.body
                ? { title: parsed.values.title, body: parsed.values.body }
                : undefined,
              author: parsed.values.author,
            });
            results.push({ taskId, ok: true, value });
          } else {
            const root = store.getTask(taskId).task;
            const fallback = parsed.values["default-profile"]
              ? parseProfileRoute(parsed.values["default-profile"], plannerRuntime)
              : root.assignee && root.runtime !== "manual"
                ? { name: root.assignee, runtime: root.runtime as WorkerRuntime }
                : profiles[0] ?? { name: `${plannerRuntime}-worker`, runtime: plannerRuntime };
            const value = await decomposeTriageTask(store, taskId, {
              profiles,
              defaultProfile: fallback,
              orchestratorProfile: parsed.values["orchestrator-profile"]
                ? parseProfileRoute(parsed.values["orchestrator-profile"], plannerRuntime)
                : fallback,
              autoPromoteChildren: orchestrationSettings.autoPromoteChildren,
              planner,
              plan: explicitPlan,
            });
            results.push({ taskId, ok: true, value });
          }
        } catch (error) {
          results.push({ taskId, ok: false, error: error instanceof Error ? error.message : String(error) });
        }
      }
      process.stdout.write(`${JSON.stringify(results, null, 2)}\n`);
    } finally {
      store.close();
    }
    return;
  }

  if (command === "swarm") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: {
        db: { type: "string" },
        workers: { type: "string" },
        verifier: { type: "string" },
        synthesizer: { type: "string" },
        tenant: { type: "string" },
        workspace: { type: "string" },
        "workspace-kind": { type: "string" },
        blackboard: { type: "string" },
      },
    });
    const goal = parsed.positionals.join(" ").trim();
    if (!goal || !parsed.values.workers || !parsed.values.verifier || !parsed.values.synthesizer) {
      throw new Error("swarm requires a goal, --workers, --verifier, and --synthesizer");
    }
    const workers = parsed.values.workers.split(",").map((worker) => parseProfileRoute(worker.trim()));
    const verifier = parseProfileRoute(parsed.values.verifier);
    const synthesizer = parseProfileRoute(parsed.values.synthesizer);
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const output = store.createSwarm({
        goal,
        workers: workers.map((profile) => ({ assignee: profile.name, runtime: profile.runtime })),
        verifier: { assignee: verifier.name, runtime: verifier.runtime },
        synthesizer: { assignee: synthesizer.name, runtime: synthesizer.runtime },
        tenant: parsed.values.tenant,
        workspace: parsed.values.workspace,
        workspaceKind: parsed.values["workspace-kind"] as "scratch" | "dir" | "worktree" | undefined,
        blackboard: parsed.values.blackboard ? parseMetadata(parsed.values.blackboard) : undefined,
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

  if (command === "reassign") {
    const parsed = parseArgs({ args, allowPositionals: true, options: { db: { type: "string" } } });
    if (parsed.positionals.length < 2) throw new Error("reassign requires at least one task id and an assignee");
    const assignee = parsed.positionals.at(-1)!;
    const taskIds = parsed.positionals.slice(0, -1);
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      process.stdout.write(`${JSON.stringify(store.bulkMutate(taskIds, {
        assignee: assignee === "none" ? null : assignee,
      }), null, 2)}\n`);
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

  if (command === "claim") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, ttl: { type: "string" }, worker: { type: "string" } },
    });
    const taskId = parsed.positionals[0];
    if (!taskId) throw new Error("claim requires a task id");
    const manager = managerFor(parsed.values.db);
    const board = manager.resolve(globalBoard);
    const store = manager.openStore(board);
    try {
      const claim = store.claimTask({
        taskId,
        claimTtlSeconds: numberOption(parsed.values.ttl, 900),
        workerId: parsed.values.worker ?? `cli-${process.pid}`,
      });
      if (!claim) throw new Error(`Task is not claimable: ${taskId}`);
      try {
        const prepared = new WorkspaceManager(manager).prepare(store, claim);
        process.stdout.write(`${JSON.stringify(prepared, null, 2)}\n`);
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        store.failRun(
          { runId: claim.run.id, claimToken: claim.claimToken },
          `Workspace preparation failed: ${message}`,
        );
        throw error;
      }
    } finally {
      store.close();
    }
    return;
  }

  if (command === "heartbeat") {
    const parsed = parseArgs({
      args,
      allowPositionals: true,
      options: { db: { type: "string" }, note: { type: "string" } },
    });
    const taskId = scopedCliTaskId(parsed.positionals[0], "heartbeat");
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const scope = scopedCliRun();
      process.stdout.write(`${JSON.stringify(
        scope ? store.heartbeat(scope, parsed.values.note) : store.heartbeatTask(taskId, parsed.values.note),
        null,
        2,
      )}\n`);
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
    const [requestedTaskId, ...bodyParts] = parsed.positionals;
    const taskId = scopedCliTaskId(requestedTaskId, "comment");
    const body = bodyParts.join(" ").trim();
    if (!body) throw new Error("comment requires text");
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
    const taskIds = parsed.positionals.length === 0 && process.env.KANBAN_TASK_ID
      ? [process.env.KANBAN_TASK_ID]
      : parsed.positionals;
    if (taskIds.length === 0) throw new Error("complete requires at least one task id");
    if (
      parsed.positionals.length > 1 &&
      (parsed.values.summary || parsed.values.result || parsed.values.metadata || parsed.values.artifact)
    ) {
      throw new Error("Structured completion handoff is only allowed for one task at a time");
    }
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const completion = {
        summary: parsed.values.summary,
        result: parsed.values.result,
        metadata: parseMetadata(parsed.values.metadata),
        artifacts: parsed.values.artifact,
      };
      const resolvedTaskIds = taskIds.map((taskId) => scopedCliTaskId(taskId, "complete"));
      const scope = scopedCliRun();
      const completed = scope
        ? [store.completeRun(scope, completion)]
        : resolvedTaskIds.map((taskId) => store.completeTask(taskId, completion));
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
      options: { db: { type: "string" }, kind: { type: "string" }, ids: { type: "string", multiple: true } },
    });
    const [requestedTaskId, reasonValue, ...positionalIds] = parsed.positionals;
    const taskId = scopedCliTaskId(requestedTaskId, "block");
    const reason = reasonValue?.trim() ?? "";
    if (!reason) throw new Error("block requires a reason");
    const store = openTaskStore(parsed.values.db, globalBoard);
    try {
      const scope = scopedCliRun();
      const kind = requireBlockKind(parsed.values.kind);
      const taskIds = [taskId, ...positionalIds, ...(parsed.values.ids ?? [])]
        .map((id) => scopedCliTaskId(id, "block"));
      const blocked = scope
        ? [store.blockRun(scope, reason, kind)]
        : taskIds.map((id) => store.blockTask(id, { reason, kind }));
      process.stdout.write(`${JSON.stringify(blocked, null, 2)}\n`);
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

  if (command === "dispatch" || command === "daemon") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        board: { type: "string" },
        once: { type: "boolean" },
        watch: { type: "boolean" },
        force: { type: "boolean" },
        "dry-run": { type: "boolean" },
        max: { type: "string" },
        "max-workers": { type: "string" },
        "failure-limit": { type: "string" },
        "max-in-progress": { type: "string" },
        "max-per-assignee": { type: "string" },
        "claim-ttl-seconds": { type: "string" },
        "stale-timeout-seconds": { type: "string" },
        "heartbeat-max-stale-seconds": { type: "string" },
        "crash-grace-seconds": { type: "string" },
        "rate-limit-cooldown-seconds": { type: "string" },
        "interval-ms": { type: "string" },
        "allow-writes": { type: "boolean" },
        "auto-decompose": { type: "boolean" },
        "auto-decompose-per-tick": { type: "string" },
        profile: { type: "string", multiple: true },
        "default-profile": { type: "string" },
        "orchestrator-profile": { type: "string" },
        "planner-runtime": { type: "string" },
        "planner-timeout-ms": { type: "string" },
      },
    });
    if (command === "daemon" && !parsed.values.force) throw new Error("daemon is deprecated and requires --force; prefer dispatch --watch");
    if (parsed.values["dry-run"]) {
      const manager = managerFor(parsed.values.db);
      const boards = globalBoard
        ? [manager.resolve(globalBoard)]
        : manager.list().filter((item) => !item.archived).map((item) => item.slug);
      const limit = numberOption(parsed.values.max ?? parsed.values["max-workers"], 2);
      const candidates: unknown[] = [];
      for (const board of boards) {
        const store = manager.openStore(board);
        try {
          const ready = store.listTasks({ status: "ready", sort: "priority-desc", limit: 500 });
          for (const task of ready) {
            if (!task.assignee || task.runtime === "manual") continue;
            if (task.scheduledAt && Date.parse(task.scheduledAt) > Date.now()) continue;
            if (store.getTask(task.id).parents.some((parent) => parent.status !== "done")) continue;
            candidates.push(task);
            if (candidates.length >= limit) break;
          }
        } finally {
          store.close();
        }
        if (candidates.length >= limit) break;
      }
      process.stdout.write(`${JSON.stringify({ dryRun: true, candidates }, null, 2)}\n`);
      return;
    }
    const controller = new AbortController();
    process.once("SIGINT", () => controller.abort());
    process.once("SIGTERM", () => controller.abort());
    await runDispatcher({
      dbPath: resolve(parsed.values.db ?? defaultDbPath()),
      cliEntry: resolve(process.argv[1] ?? "dist/cli.js"),
      board: globalBoard,
      once: command === "daemon" ? false : parsed.values.once ?? false,
      intervalMs: numberOption(parsed.values["interval-ms"], 2_000),
      maxWorkers: numberOption(parsed.values.max ?? parsed.values["max-workers"], 2),
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
      failureLimit: parsed.values["failure-limit"] === undefined
        ? undefined
        : numberOption(parsed.values["failure-limit"], 2),
      autoDecompose: parsed.values["auto-decompose"],
      autoDecomposePerTick: numberOption(parsed.values["auto-decompose-per-tick"], 3),
      decompositionProfiles: (parsed.values.profile ?? []).map((profile) =>
        parseProfileRoute(profile, requirePlannerRuntime(parsed.values["planner-runtime"])),
      ),
      defaultDecompositionProfile: parsed.values["default-profile"]
        ? parseProfileRoute(parsed.values["default-profile"], requirePlannerRuntime(parsed.values["planner-runtime"]))
        : undefined,
      orchestratorProfile: parsed.values["orchestrator-profile"]
        ? parseProfileRoute(parsed.values["orchestrator-profile"], requirePlannerRuntime(parsed.values["planner-runtime"]))
        : undefined,
      plannerRuntime: requirePlannerRuntime(parsed.values["planner-runtime"]),
      plannerTimeoutMs: numberOption(parsed.values["planner-timeout-ms"], 120_000),
      allowWrites: parsed.values["allow-writes"] ?? false,
      signal: controller.signal,
      onLog: (message) => process.stderr.write(`[kanban] ${message}\n`),
    });
    return;
  }

  if (command === "dashboard") {
    const parsed = parseArgs({
      args,
      options: {
        db: { type: "string" },
        host: { type: "string" },
        port: { type: "string" },
        token: { type: "string" },
      },
    });
    const dashboard = await startDashboardServer({
      dbPath: resolve(parsed.values.db ?? defaultDbPath()),
      cliEntry: resolve(process.argv[1] ?? "dist/cli.js"),
      host: parsed.values.host ?? "127.0.0.1",
      port: numberOption(parsed.values.port, 8420),
      token: parsed.values.token,
      onLog: (message) => process.stderr.write(`[kanban] ${message}\n`),
    });
    process.stdout.write(`${dashboard.url}/?token=${encodeURIComponent(dashboard.token)}\n`);
    const controller = new AbortController();
    process.once("SIGINT", () => controller.abort());
    process.once("SIGTERM", () => controller.abort());
    await new Promise<void>((resolveStop) => controller.signal.addEventListener("abort", () => resolveStop(), { once: true }));
    await dashboard.close();
    return;
  }

  throw new Error(`Unknown command: ${command}`);
}

main().catch((error: unknown) => {
  const message = error instanceof Error ? error.message : String(error);
  process.stderr.write(`taskcircuit: ${message}\n`);
  process.exitCode = 1;
});
