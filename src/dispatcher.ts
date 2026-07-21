import { spawn, type ChildProcess } from "node:child_process";
import { createWriteStream, mkdirSync } from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";

import { KanbanStore } from "./store.js";
import type { ClaimedTask, Runtime } from "./types.js";

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
  allowWrites?: boolean | undefined;
  signal?: AbortSignal | undefined;
  onLog?: ((message: string) => void) | undefined;
}

function workerPrompt(claim: ClaimedTask): string {
  const { task } = claim.task;
  return [
    `You are the assigned Kanban worker for ${task.id}.`,
    "Call kanban_show first without a task_id. Work only on that task in the current workspace.",
    "Use kanban_heartbeat for long-running work. Record durable intermediate handoffs with kanban_comment.",
    "You must end exactly once by calling kanban_complete with verification evidence, or kanban_block with the concrete reason.",
    "Do not claim, create, reassign, unblock, or modify unrelated tasks.",
  ].join(" ");
}

function mcpServerArgs(cliEntry: string, dbPath: string): string[] {
  return [cliEntry, "serve", "--db", dbPath];
}

function codexConfigString(value: string): string {
  return JSON.stringify(value);
}

export function buildRunnerCommand(
  claim: ClaimedTask,
  options: Pick<DispatcherOptions, "dbPath" | "cliEntry" | "allowWrites">,
): RunnerCommand {
  const task = claim.task.task;
  const cwd = task.workspace ? (isAbsolute(task.workspace) ? task.workspace : resolve(task.workspace)) : process.cwd();
  const serverArgs = mcpServerArgs(resolve(options.cliEntry), resolve(options.dbPath));
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    KANBAN_DB: resolve(options.dbPath),
    KANBAN_TASK_ID: task.id,
    KANBAN_RUN_ID: claim.run.id,
    KANBAN_CLAIM_TOKEN: claim.claimToken,
    KANBAN_WORKER_ID: claim.run.workerId,
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
      ? ["Read", "Edit", "Write", "Glob", "Grep", "Bash"]
      : ["Read", "Glob", "Grep", "WebSearch", "WebFetch"];
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

async function runClaim(
  store: KanbanStore,
  claim: ClaimedTask,
  options: DispatcherOptions,
  children: Set<ChildProcess>,
): Promise<void> {
  const command = buildRunnerCommand(claim, options);
  const logsDir = join(dirname(resolve(options.dbPath)), "logs");
  mkdirSync(logsDir, { recursive: true });
  const logPath = join(logsDir, `${claim.task.task.id}-${claim.run.id}.log`);
  const logStream = createWriteStream(logPath, { flags: "a" });
  options.onLog?.(`launch ${claim.task.task.id} via ${claim.task.task.runtime}; log=${logPath}`);

  await new Promise<void>((resolveRun) => {
    const child = spawn(command.command, command.args, {
      cwd: command.cwd,
      env: command.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    children.add(child);
    child.stdout?.pipe(logStream, { end: false });
    child.stderr?.pipe(logStream, { end: false });
    let spawnError: Error | null = null;
    child.once("error", (error) => {
      spawnError = error;
    });
    child.once("close", (code, signal) => {
      children.delete(child);
      logStream.end();
      const current = store.getTask(claim.task.task.id).task;
      if (current.status === "running" && current.currentRunId === claim.run.id) {
        const detail = spawnError?.message ?? `Runner exited without a terminal Kanban call (code=${code}, signal=${signal ?? "none"})`;
        store.failRun({ runId: claim.run.id, claimToken: claim.claimToken }, detail);
        options.onLog?.(`requeue/fail ${current.id}: ${detail}`);
      } else {
        options.onLog?.(`finish ${current.id}: ${current.status}`);
      }
      resolveRun();
    });
  });
}

export async function runDispatcher(options: DispatcherOptions): Promise<void> {
  const store = new KanbanStore(options.dbPath);
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
      while (!options.signal?.aborted && active.size < maxWorkers) {
        const claim = store.claimTask({
          board: options.board ?? "default",
          workerId: `dispatcher-${process.pid}`,
          excludeManual: true,
        });
        if (!claim) break;
        launched = true;
        const running = runClaim(store, claim, options, children).finally(() => active.delete(running));
        active.add(running);
        if (options.once) break;
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
    store.close();
  }
}
