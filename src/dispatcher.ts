import { spawn, type ChildProcess } from "node:child_process";
import { createWriteStream, mkdirSync } from "node:fs";
import { isAbsolute, join, resolve } from "node:path";

import { BoardManager } from "./boards.js";
import { KanbanStore } from "./store.js";
import type { ClaimedTask, Runtime } from "./types.js";
import { WorkspaceManager } from "./workspaces.js";

export interface RunnerCommand {
  command: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
}

export interface DispatcherOptions {
  dbPath: string;
  cliEntry: string;
  board?: string | undefined;
  once?: boolean | undefined;
  intervalMs?: number | undefined;
  maxWorkers?: number | undefined;
  maxInProgress?: number | undefined;
  maxInProgressPerAssignee?: number | undefined;
  claimTtlSeconds?: number | undefined;
  staleTimeoutSeconds?: number | undefined;
  heartbeatMaxStaleSeconds?: number | undefined;
  crashGraceSeconds?: number | undefined;
  rateLimitCooldownSeconds?: number | undefined;
  allowWrites?: boolean | undefined;
  workspaceRoot?: string | undefined;
  attachmentsRoot?: string | undefined;
  logsRoot?: string | undefined;
  signal?: AbortSignal | undefined;
  onLog?: ((message: string) => void) | undefined;
}

function workerPrompt(claim: ClaimedTask): string {
  const { task } = claim.task;
  const instructions = [
    `You are the assigned Kanban worker for ${task.id}.`,
    "Call kanban_show first without a task_id. Work only on that task in the current workspace.",
    "Use kanban_heartbeat for long-running work. Record durable intermediate handoffs with kanban_comment.",
    "You must end exactly once by calling kanban_complete with verification evidence, or kanban_block with the concrete reason.",
    "Do not claim, create, reassign, unblock, or modify unrelated tasks.",
  ];
  if (task.skills.length > 0) {
    instructions.push(`Load and follow these task-specific skills before working: ${task.skills.join(", ")}.`);
  }
  return instructions.join(" ");
}

function mcpServerArgs(cliEntry: string, dbPath: string): string[] {
  return [cliEntry, "serve", "--db", dbPath];
}

function codexConfigString(value: string): string {
  return JSON.stringify(value);
}

export function buildRunnerCommand(
  claim: ClaimedTask,
  options: Pick<
    DispatcherOptions,
    "dbPath" | "cliEntry" | "allowWrites" | "workspaceRoot" | "attachmentsRoot" | "logsRoot"
  >,
): RunnerCommand {
  const task = claim.task.task;
  const cwd = task.workspace ? (isAbsolute(task.workspace) ? task.workspace : resolve(task.workspace)) : process.cwd();
  const serverArgs = mcpServerArgs(resolve(options.cliEntry), resolve(options.dbPath));
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    KANBAN_DB: resolve(options.dbPath),
    KANBAN_BOARD: task.board,
    KANBAN_TASK_ID: task.id,
    KANBAN_RUN_ID: claim.run.id,
    KANBAN_CLAIM_TOKEN: claim.claimToken,
    KANBAN_WORKER_ID: claim.run.workerId,
    KANBAN_TENANT: task.tenant ?? "",
    KANBAN_WORKSPACE: cwd,
    KANBAN_WORKSPACES_ROOT: options.workspaceRoot,
    KANBAN_ATTACHMENTS_ROOT: options.attachmentsRoot,
    KANBAN_LOGS_ROOT: options.logsRoot,
  };
  const prompt = workerPrompt(claim);

  if (task.runtime === "codex") {
    return {
      command: process.env.KANBAN_CODEX_BIN ?? "codex",
      cwd,
      env,
      args: [
        "exec",
        "--json",
        "--color",
        "never",
        "--skip-git-repo-check",
        "--sandbox",
        options.allowWrites ? "workspace-write" : "read-only",
        "-C",
        cwd,
        "-c",
        `mcp_servers.kanban.command=${codexConfigString(process.execPath)}`,
        "-c",
        `mcp_servers.kanban.args=${JSON.stringify(serverArgs)}`,
        "-c",
        "mcp_servers.kanban.required=true",
        prompt,
      ],
    };
  }

  if (task.runtime === "claude") {
    const mcpConfig = JSON.stringify({
      mcpServers: {
        kanban: { type: "stdio", command: process.execPath, args: serverArgs },
      },
    });
    const lifecycleTools = [
      "mcp__kanban__kanban_show",
      "mcp__kanban__kanban_comment",
      "mcp__kanban__kanban_heartbeat",
      "mcp__kanban__kanban_complete",
      "mcp__kanban__kanban_block",
    ];
    const builtInTools = options.allowWrites
      ? ["Read", "Edit", "Write", "Glob", "Grep", "Bash", "Skill"]
      : ["Read", "Glob", "Grep", "WebSearch", "WebFetch", "Skill"];
    return {
      command: process.env.KANBAN_CLAUDE_BIN ?? "claude",
      cwd,
      env,
      args: [
        "-p",
        prompt,
        "--output-format",
        "stream-json",
        "--verbose",
        "--strict-mcp-config",
        "--mcp-config",
        mcpConfig,
        "--permission-mode",
        options.allowWrites ? "acceptEdits" : "dontAsk",
        "--allowedTools",
        [...builtInTools, ...lifecycleTools].join(","),
      ],
    };
  }

  throw new Error(`Dispatcher cannot launch runtime: ${task.runtime satisfies Runtime}`);
}

function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolveDelay) => {
    if (signal?.aborted) return resolveDelay();
    const timer = setTimeout(resolveDelay, milliseconds);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolveDelay();
      },
      { once: true },
    );
  });
}

function pidAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function terminatePid(pid: number): boolean {
  if (pid === process.pid || !pidAlive(pid)) return false;
  try {
    process.kill(pid, "SIGTERM");
    return true;
  } catch {
    return false;
  }
}

function recoverAbandonedRuns(store: KanbanStore, board: string, options: DispatcherOptions): void {
  const timestamp = Date.now();
  const staleTimeoutMs = Math.max(60, options.staleTimeoutSeconds ?? 4 * 60 * 60) * 1_000;
  const heartbeatMaxStaleMs = Math.max(60, options.heartbeatMaxStaleSeconds ?? 60 * 60) * 1_000;
  const crashGraceMs = Math.max(0, options.crashGraceSeconds ?? 30) * 1_000;
  for (const active of store.listActiveRuns(board)) {
    const elapsed = timestamp - Date.parse(active.run.claimedAt);
    const heartbeatAge = timestamp - Date.parse(active.run.heartbeatAt);
    const expired = timestamp >= Date.parse(active.run.claimExpiresAt);
    const stale = elapsed >= staleTimeoutMs && heartbeatAge >= heartbeatMaxStaleMs;
    const timedOut = active.task.maxRuntimeSeconds !== null && elapsed >= active.task.maxRuntimeSeconds * 1_000;
    const alive = active.run.pid !== null && pidAlive(active.run.pid);
    const crashed = active.run.pid !== null && elapsed >= crashGraceMs && !alive;

    if (timedOut) {
      if (active.run.pid !== null) terminatePid(active.run.pid);
      store.recoverAbandonedRun(active.run.id, "timed_out", `Maximum runtime exceeded after ${Math.floor(elapsed / 1_000)} seconds`);
      options.onLog?.(`timed out ${active.task.id}`);
    } else if (crashed) {
      store.recoverAbandonedRun(active.run.id, "crashed", `Worker PID ${active.run.pid} is no longer alive`);
      options.onLog?.(`reclaimed crashed worker ${active.task.id}`);
    } else if (expired || stale) {
      if (alive && active.run.pid !== null && terminatePid(active.run.pid)) {
        store.deferReclaim(active.run.id, 120);
        options.onLog?.(`deferred reclaim while terminating PID ${active.run.pid} for ${active.task.id}`);
      } else {
        const reason = stale ? "Heartbeat became stale" : "Claim TTL expired";
        store.recoverAbandonedRun(active.run.id, "reclaimed", reason, false);
        options.onLog?.(`reclaimed ${active.task.id}: ${reason}`);
      }
    }
  }
}

async function runClaim(
  store: KanbanStore,
  claim: ClaimedTask,
  options: DispatcherOptions,
  children: Set<ChildProcess>,
  logsDir: string,
  workspaces: WorkspaceManager,
  workspaceRoot: string,
  attachmentsRoot: string,
): Promise<void> {
  const scope = { runId: claim.run.id, claimToken: claim.claimToken };
  let preparedClaim: ClaimedTask;
  try {
    preparedClaim = workspaces.prepare(store, claim);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    store.failRun(scope, `Workspace preparation failed: ${message}`);
    options.onLog?.(`workspace failure ${claim.task.task.id}: ${message}`);
    return;
  }
  const command = buildRunnerCommand(preparedClaim, {
    ...options,
    workspaceRoot,
    attachmentsRoot,
    logsRoot: logsDir,
  });
  mkdirSync(logsDir, { recursive: true });
  const logPath = join(logsDir, `${preparedClaim.task.task.id}-${preparedClaim.run.id}.log`);
  const logStream = createWriteStream(logPath, { flags: "a" });
  options.onLog?.(`launch ${preparedClaim.task.task.id} via ${preparedClaim.task.task.runtime}; log=${logPath}`);

  await new Promise<void>((resolveRun) => {
    const child = spawn(command.command, command.args, {
      cwd: command.cwd,
      env: command.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    children.add(child);
    if (child.pid !== undefined) store.recordSpawn(scope, child.pid, logPath);
    child.stdout?.pipe(logStream, { end: false });
    child.stderr?.pipe(logStream, { end: false });
    let spawnError: Error | null = null;
    let timedOut = false;
    let forceKillTimer: NodeJS.Timeout | undefined;
    const runtimeTimer = preparedClaim.task.task.maxRuntimeSeconds === null
      ? undefined
      : setTimeout(() => {
          timedOut = true;
          child.kill("SIGTERM");
          forceKillTimer = setTimeout(() => child.kill("SIGKILL"), 5_000);
        }, preparedClaim.task.task.maxRuntimeSeconds * 1_000);
    child.once("error", (error) => {
      spawnError = error;
    });
    child.once("close", (code, signal) => {
      if (runtimeTimer) clearTimeout(runtimeTimer);
      if (forceKillTimer) clearTimeout(forceKillTimer);
      children.delete(child);
      logStream.end();
      const current = store.getTask(preparedClaim.task.task.id).task;
      if (current.status === "running" && current.currentRunId === preparedClaim.run.id) {
        const detail = spawnError?.message ?? `Runner exited without a terminal Kanban call (code=${code}, signal=${signal ?? "none"})`;
        if (timedOut) {
          store.failRun(scope, detail, { outcome: "timed_out" });
        } else if (spawnError) {
          store.failRun(scope, detail, { outcome: "spawn_failed" });
        } else if (code === 75) {
          store.failRun(scope, detail, {
            outcome: "rate_limited",
            countFailure: false,
            cooldownSeconds: Math.max(0, options.rateLimitCooldownSeconds ?? 60),
          });
        } else if (code === 0) {
          store.failRun(scope, detail, { outcome: "protocol_violation" });
        } else {
          store.failRun(scope, detail);
        }
        options.onLog?.(`requeue/fail ${current.id}: ${detail}`);
      } else {
        options.onLog?.(`finish ${current.id}: ${current.status}`);
        if (current.status === "done") {
          try {
            workspaces.cleanup(current);
          } catch (error) {
            options.onLog?.(`workspace cleanup failed ${current.id}: ${error instanceof Error ? error.message : String(error)}`);
          }
        }
      }
      resolveRun();
    });
  });
}

export async function runDispatcher(options: DispatcherOptions): Promise<void> {
  const manager = new BoardManager(options.dbPath);
  const workspaces = new WorkspaceManager(manager);
  const active = new Set<Promise<void>>();
  const children = new Set<ChildProcess>();
  const maxWorkers = Math.max(1, options.maxWorkers ?? 2);
  const intervalMs = Math.max(250, options.intervalMs ?? 2_000);

  const stopChildren = (): void => {
    for (const child of children) child.kill("SIGTERM");
  };
  options.signal?.addEventListener("abort", stopChildren, { once: true });

  try {
    do {
      let launched = false;
      const boards = options.board
        ? [manager.resolve(options.board)]
        : manager.list().filter((board) => !board.archived).map((board) => board.slug);
      let foundInPass = true;
      while (!options.signal?.aborted && active.size < maxWorkers && foundInPass) {
        foundInPass = false;
        for (const board of boards) {
          if (options.signal?.aborted || active.size >= maxWorkers) break;
          const store = manager.openStore(board);
          store.promoteDueTasks(board);
          recoverAbandonedRuns(store, board, options);
          const claim = store.claimTask({
            board,
            workerId: `dispatcher-${process.pid}`,
            excludeManual: true,
            claimTtlSeconds: options.claimTtlSeconds,
            maxInProgress: options.maxInProgress,
            maxInProgressPerAssignee: options.maxInProgressPerAssignee,
          });
          if (!claim) {
            store.close();
            continue;
          }
          foundInPass = true;
          launched = true;
          let running: Promise<void>;
          running = runClaim(
            store,
            claim,
            options,
            children,
            manager.logsRoot(board),
            workspaces,
            manager.workspaceRoot(board),
            manager.attachmentsRoot(board),
          ).finally(() => {
            store.close();
            active.delete(running);
          });
          active.add(running);
          if (options.once) break;
        }
        if (options.once && launched) break;
      }

      if (options.once) {
        await Promise.all(active);
        break;
      }
      if (options.signal?.aborted) break;
      if (!launched && active.size > 0) {
        await Promise.race([...active, delay(intervalMs, options.signal)]);
      } else {
        await delay(intervalMs, options.signal);
      }
    } while (!options.signal?.aborted);
    await Promise.all(active);
  } finally {
    options.signal?.removeEventListener("abort", stopChildren);
  }
}
