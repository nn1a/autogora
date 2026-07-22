import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import {
  createWriteStream,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";

import { BoardManager } from "./boards.js";
import { deliverNotifications } from "./notifications.js";
import {
  createCliPlanner,
  decomposeTriageTask,
  judgeGoalProgress,
  type GoalJudgment,
  type ProfileRoute,
  type StructuredPlanner,
} from "./orchestration.js";
import { KanbanStore } from "./store.js";
import type { ClaimedTask, PlannerRuntime, WorkerRuntime } from "./types.js";
import { WorkspaceManager } from "./workspaces.js";

export interface RunnerCommand {
  command: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  toolApproval?: {
    directory: string;
    commandPrefix: string;
  } | undefined;
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
  failureLimit?: number | undefined;
  notificationLimit?: number | undefined;
  notificationTimeoutMs?: number | undefined;
  goalJudge?: ((input: {
    task: ClaimedTask["task"];
    turn: number;
    workerOutput: string;
  }) => Promise<GoalJudgment>) | undefined;
  autoDecompose?: boolean | undefined;
  autoDecomposePerTick?: number | undefined;
  decompositionProfiles?: ProfileRoute[] | undefined;
  defaultDecompositionProfile?: ProfileRoute | undefined;
  orchestratorProfile?: ProfileRoute | undefined;
  plannerRuntime?: PlannerRuntime | undefined;
  plannerTimeoutMs?: number | undefined;
  decompositionPlanner?: StructuredPlanner | undefined;
  allowWrites?: boolean | undefined;
  workspaceRoot?: string | undefined;
  attachmentsRoot?: string | undefined;
  logsRoot?: string | undefined;
  clineApprovalDir?: string | undefined;
  signal?: AbortSignal | undefined;
  onLog?: ((message: string) => void) | undefined;
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

function workerPrompt(claim: ClaimedTask, cliEntry: string): string {
  const { task } = claim.task;
  const instructions = [`You are the assigned Kanban worker for ${task.id}.`];
  if (task.runtime === "cline" || task.runtime === "gemini") {
    const bridge = `${shellQuote(process.execPath)} ${shellQuote(resolve(cliEntry))}`;
    instructions.push(
      task.runtime === "cline"
        ? "MCP is unavailable in this Cline build. Use only the scoped Kanban CLI bridge for task lifecycle communication."
        : "Use only the scoped Kanban CLI bridge for task lifecycle communication; do not change Gemini user or project MCP settings.",
      `First run ${bridge} show "$KANBAN_TASK_ID". For long work run ${bridge} heartbeat "$KANBAN_TASK_ID" --note "progress".`,
      `Record handoffs with ${bridge} comment "$KANBAN_TASK_ID" "message".`,
      `Finish exactly once with ${bridge} complete "$KANBAN_TASK_ID" --summary "summary" or ${bridge} block "$KANBAN_TASK_ID" "reason" --kind needs_input.`,
      "The dispatcher scopes these commands to the active task and claim. Do not claim, create, reassign, unblock, or modify unrelated tasks.",
    );
  } else {
    instructions.push(
      "Call kanban_show first without a task_id. Work only on that task in the current workspace.",
      "Use kanban_heartbeat for long-running work. Record durable intermediate handoffs with kanban_comment.",
      "Do not claim, create, reassign, unblock, or modify unrelated tasks.",
    );
  }
  if (task.goalMode) {
    instructions.push(
      "This card is in goal mode. Call kanban_complete only when every acceptance criterion is demonstrably satisfied, or kanban_block for a real blocker.",
      task.runtime === "cline"
        ? "If meaningful work remains after this turn, leave the task running and end with a concise progress handoff; an independent judge may continue the goal in a fresh Cline turn."
        : "If meaningful work remains after this turn, leave the task running and end your response with a concise progress handoff; an independent judge will continue this same session.",
    );
  } else {
    instructions.push("You must end exactly once by calling kanban_complete with verification evidence, or kanban_block with the concrete reason.");
  }
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
    "dbPath" | "cliEntry" | "allowWrites" | "workspaceRoot" | "attachmentsRoot" | "logsRoot" | "clineApprovalDir"
  >,
  sessionId?: string,
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
  const prompt = workerPrompt(claim, options.cliEntry);

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
        ...(sessionId ? ["--session-id", sessionId] : []),
      ],
    };
  }

  if (task.runtime === "cline") {
    const allowWrites = options.allowWrites === true;
    if (!allowWrites && !options.clineApprovalDir) {
      throw new Error("Read-only Cline execution requires a scoped tool approval directory");
    }
    const commandPrefix = `${shellQuote(process.execPath)} ${shellQuote(resolve(options.cliEntry))}`;
    const clineEnv = allowWrites
      ? env
      : {
          ...env,
          CLINE_TOOL_APPROVAL_MODE: "desktop",
          CLINE_TOOL_APPROVAL_DIR: options.clineApprovalDir,
        };
    return {
      command: process.env.KANBAN_CLINE_BIN ?? "cline",
      cwd,
      env: clineEnv,
      args: [
        "--json",
        "--auto-approve", allowWrites ? "true" : "false",
        "--cwd", cwd,
        prompt,
      ],
      toolApproval: allowWrites
        ? undefined
        : { directory: options.clineApprovalDir!, commandPrefix },
    };
  }

  if (task.runtime === "gemini") {
    const allowWrites = options.allowWrites === true;
    const commandPrefix = `${shellQuote(process.execPath)} ${shellQuote(resolve(options.cliEntry))}`;
    const readOnlyTools = [
      "read_file",
      "read_many_files",
      "list_directory",
      "glob",
      "grep_search",
      "google_web_search",
      "web_fetch",
      `ShellTool(${commandPrefix})`,
    ];
    return {
      command: process.env.KANBAN_GEMINI_BIN ?? "gemini",
      cwd,
      env,
      args: [
        "--output-format", "stream-json",
        "--approval-mode", allowWrites ? "yolo" : "default",
        "--skip-trust",
        "-e", "none",
        ...(!allowWrites ? ["--allowed-tools", readOnlyTools.join(",")] : []),
        "-p", prompt,
      ],
    };
  }

  throw new Error(`Dispatcher cannot launch runtime: ${task.runtime}`);
}

function buildGoalContinuationCommand(
  claim: ClaimedTask,
  options: Pick<
    DispatcherOptions,
    "dbPath" | "cliEntry" | "allowWrites" | "workspaceRoot" | "attachmentsRoot" | "logsRoot" | "clineApprovalDir"
  >,
  sessionId: string | null,
  prompt: string,
): RunnerCommand {
  const task = claim.task.task;
  const initial = buildRunnerCommand(claim, options);
  if (task.runtime === "codex") {
    if (!sessionId) throw new Error("Codex goal continuation requires a session id");
    return {
      ...initial,
      args: [
        "exec",
        "resume",
        "--json",
        "--skip-git-repo-check",
        sessionId,
        prompt,
      ],
    };
  }
  if (task.runtime === "claude") {
    if (!sessionId) throw new Error("Claude goal continuation requires a session id");
    const promptIndex = initial.args.indexOf(workerPrompt(claim, options.cliEntry));
    const args = [...initial.args];
    if (promptIndex >= 0) args[promptIndex] = prompt;
    args.push("--resume", sessionId);
    return { ...initial, args };
  }
  if (task.runtime === "cline") {
    const initialPrompt = workerPrompt(claim, options.cliEntry);
    const promptIndex = initial.args.indexOf(initialPrompt);
    const args = [...initial.args];
    if (promptIndex >= 0) args[promptIndex] = `${initialPrompt}\nContinuation focus: ${prompt}`;
    return { ...initial, args };
  }
  if (task.runtime === "gemini") {
    if (!sessionId) throw new Error("Gemini goal continuation requires a session id");
    const initialPrompt = workerPrompt(claim, options.cliEntry);
    const promptIndex = initial.args.indexOf(initialPrompt);
    const args = [...initial.args];
    if (promptIndex >= 0) args[promptIndex] = prompt;
    args.push("--resume", sessionId);
    return { ...initial, args };
  }
  throw new Error(`Goal continuation cannot launch runtime: ${task.runtime}`);
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

async function deliverBoardNotifications(
  manager: BoardManager,
  boards: string[],
  options: DispatcherOptions,
): Promise<void> {
  await Promise.all(boards.map(async (board) => {
    const store = manager.openStore(board);
    try {
      const results = await deliverNotifications(store, {
        limit: options.notificationLimit ?? 25,
        timeoutMs: options.notificationTimeoutMs ?? 5_000,
      });
      for (const delivery of results) {
        if (delivery.delivered) options.onLog?.(`notified ${delivery.taskId}: ${delivery.eventKind}`);
        else options.onLog?.(`notification failed ${delivery.taskId}: ${delivery.error ?? "unknown error"}`);
      }
    } catch (error) {
      options.onLog?.(`notification sweep failed for ${board}: ${error instanceof Error ? error.message : String(error)}`);
    } finally {
      store.close();
    }
  }));
}

async function decomposeBoardTriage(
  manager: BoardManager,
  boards: string[],
  options: DispatcherOptions,
): Promise<void> {
  let remaining = Math.max(1, options.autoDecomposePerTick ?? 500);
  for (const board of boards) {
    if (remaining <= 0) break;
    const settings = manager.read(board).orchestration;
    if (!(options.autoDecompose ?? settings.autoDecompose)) continue;
    let boardRemaining = Math.min(remaining, options.autoDecomposePerTick ?? settings.autoDecomposePerTick);
    const plannerRuntime = options.plannerRuntime ?? settings.plannerRuntime;
    const planner = options.decompositionPlanner ?? createCliPlanner({
      runtime: plannerRuntime,
      cwd: process.cwd(),
      timeoutMs: options.plannerTimeoutMs ?? 120_000,
    });
    const store = manager.openStore(board);
    try {
      const triage = store.listTasks({ status: "triage", limit: boardRemaining });
      const discovered = store.listTasks({ includeArchived: true, limit: 500 })
        .filter((task) => task.assignee && task.runtime !== "manual")
        .map((task) => ({
          name: task.assignee!,
          runtime: task.runtime as WorkerRuntime,
        } satisfies ProfileRoute));
      const configuredProfiles = options.decompositionProfiles ?? settings.profiles;
      const profiles = [...new Map(
        [...discovered, ...configuredProfiles].map((profile) => [profile.name, profile]),
      ).values()];
      for (const task of triage) {
        const configuredDefault = settings.defaultProfile
          ? profiles.find((profile) => profile.name === settings.defaultProfile)
          : undefined;
        const fallback = options.defaultDecompositionProfile ?? configuredDefault ??
          (task.assignee && task.runtime !== "manual"
            ? { name: task.assignee, runtime: task.runtime as WorkerRuntime }
            : profiles[0] ?? { name: `${plannerRuntime}-worker`, runtime: plannerRuntime });
        const configuredOrchestrator = settings.orchestratorProfile
          ? profiles.find((profile) => profile.name === settings.orchestratorProfile)
          : undefined;
        try {
          const result = await decomposeTriageTask(store, task.id, {
            profiles,
            defaultProfile: fallback,
            orchestratorProfile: options.orchestratorProfile ?? configuredOrchestrator ?? fallback,
            autoPromoteChildren: settings.autoPromoteChildren,
            planner,
          });
          options.onLog?.(`auto-${result.fanout ? "decomposed" : "specified"} ${task.id}: ${result.reason}`);
        } catch (error) {
          options.onLog?.(`auto-decompose failed ${task.id}: ${error instanceof Error ? error.message : String(error)}`);
        }
        remaining -= 1;
        boardRemaining -= 1;
        if (remaining <= 0 || boardRemaining <= 0) break;
      }
    } finally {
      store.close();
    }
  }
}

interface TurnExecution {
  code: number | null;
  signal: NodeJS.Signals | null;
  spawnError: Error | null;
  timedOut: boolean;
  output: string;
  sessionId: string | null;
}

function sessionIdFromOutput(output: string): string | null {
  for (const line of output.split(/\r?\n/)) {
    if (!line.trim().startsWith("{")) continue;
    try {
      const event = JSON.parse(line) as Record<string, unknown>;
      const nested = event.event && typeof event.event === "object" ? event.event as Record<string, unknown> : null;
      const sessionId = event.thread_id ?? event.session_id ?? event.sessionId ?? nested?.session_id ?? nested?.sessionId;
      if (typeof sessionId === "string" && sessionId) return sessionId;
    } catch {
      // Non-JSON lines are still useful to the goal judge.
    }
  }
  return null;
}

const CLINE_READ_ONLY_TOOLS = new Set([
  "read_files",
  "read_file",
  "list_files",
  "list_code_definition_names",
  "search_codebase",
  "search_files",
  "fetch_web_content",
  "skills",
]);

function approvalCommands(input: unknown): string[] {
  if (typeof input === "string") return [input];
  if (Array.isArray(input)) return input.flatMap(approvalCommands);
  if (!input || typeof input !== "object") return [];
  const record = input as Record<string, unknown>;
  if (record.commands !== undefined) return approvalCommands(record.commands);
  if (typeof record.command === "string" && Array.isArray(record.args) && record.args.every((arg) => typeof arg === "string")) {
    return [`${shellQuote(record.command)} ${record.args.map((arg) => shellQuote(arg as string)).join(" ")}`];
  }
  if (typeof record.command === "string") return [record.command];
  if (typeof record.cmd === "string") return [record.cmd];
  return [];
}

function isScopedBridgeCommand(command: string, commandPrefix: string): boolean {
  const normalized = command.trim();
  if (!normalized.startsWith(`${commandPrefix} `)) return false;
  if (/[\n\r;|&<>`]/.test(normalized) || normalized.includes("$(")) return false;
  const subcommand = normalized.slice(commandPrefix.length).trimStart().split(/\s+/, 1)[0];
  return ["show", "context", "heartbeat", "comment", "complete", "block"].includes(subcommand ?? "");
}

function startToolApprovalBroker(policy: NonNullable<RunnerCommand["toolApproval"]>): () => void {
  mkdirSync(policy.directory, { recursive: true });
  const handled = new Set<string>();
  const sweep = (): void => {
    let names: string[];
    try {
      names = readdirSync(policy.directory);
    } catch {
      return;
    }
    for (const name of names) {
      if (handled.has(name) || !name.includes(".request.") || !name.endsWith(".json")) continue;
      try {
        const request = JSON.parse(readFileSync(join(policy.directory, name), "utf8")) as Record<string, unknown>;
        const toolName = typeof request.toolName === "string" ? request.toolName : "";
        const commands = approvalCommands(request.input);
        const approved = CLINE_READ_ONLY_TOOLS.has(toolName) ||
          (["run_commands", "execute_command"].includes(toolName) && commands.length > 0 &&
            commands.every((command) => isScopedBridgeCommand(command, policy.commandPrefix)));
        const decisionName = name.replace(".request.", ".decision.");
        writeFileSync(join(policy.directory, decisionName), `${JSON.stringify({
          approved,
          reason: approved ? "Approved by the scoped Kanban read-only policy" : "Denied by the scoped Kanban read-only policy",
        })}\n`, "utf8");
        handled.add(name);
      } catch {
        // The Cline process may still be finishing its atomic request write; retry next sweep.
      }
    }
  };
  const timer = setInterval(sweep, 50);
  timer.unref();
  return () => {
    clearInterval(timer);
    sweep();
  };
}

async function executeTurn(
  command: RunnerCommand,
  store: KanbanStore,
  scope: { runId: string; claimToken: string },
  children: Set<ChildProcess>,
  logPath: string,
  runtimeLimitMs: number | null,
): Promise<TurnExecution> {
  return new Promise((resolveTurn) => {
    const logStream = createWriteStream(logPath, { flags: "a" });
    const child = spawn(command.command, command.args, {
      cwd: command.cwd,
      env: command.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    const stopApprovalBroker = command.toolApproval ? startToolApprovalBroker(command.toolApproval) : undefined;
    children.add(child);
    if (child.pid !== undefined) store.recordSpawn(scope, child.pid, logPath);
    let output = "";
    const capture = (chunk: Buffer): void => {
      logStream.write(chunk);
      output = `${output}${chunk.toString("utf8")}`.slice(-128 * 1_024);
    };
    child.stdout?.on("data", capture);
    child.stderr?.on("data", capture);
    let spawnError: Error | null = null;
    let timedOut = false;
    let forceKillTimer: NodeJS.Timeout | undefined;
    const runtimeTimer = runtimeLimitMs === null
      ? undefined
      : setTimeout(() => {
          timedOut = true;
          child.kill("SIGTERM");
          forceKillTimer = setTimeout(() => child.kill("SIGKILL"), 5_000);
        }, Math.max(1, runtimeLimitMs));
    child.once("error", (error) => {
      spawnError = error;
    });
    child.once("close", (code, signal) => {
      if (runtimeTimer) clearTimeout(runtimeTimer);
      if (forceKillTimer) clearTimeout(forceKillTimer);
      children.delete(child);
      stopApprovalBroker?.();
      logStream.end();
      resolveTurn({ code, signal, spawnError, timedOut, output, sessionId: sessionIdFromOutput(output) });
    });
  });
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
    store.failRun(scope, `Workspace preparation failed: ${message}`, { failureLimit: options.failureLimit });
    options.onLog?.(`workspace failure ${claim.task.task.id}: ${message}`);
    return;
  }

  mkdirSync(logsDir, { recursive: true });
  const taskId = preparedClaim.task.task.id;
  const logPath = join(logsDir, `${taskId}-${preparedClaim.run.id}.log`);
  const runnerOptions = {
    ...options,
    workspaceRoot,
    attachmentsRoot,
    logsRoot: logsDir,
  };
  const goalMode = preparedClaim.task.task.goalMode;
  let sessionId: string | null = goalMode && preparedClaim.task.task.runtime === "claude" ? randomUUID() : null;
  let continuationPrompt: string | null = null;
  let turn = 1;
  const runStartedAt = Date.parse(preparedClaim.run.claimedAt);
  const defaultGoalPlanner = goalMode && !options.goalJudge
      ? createCliPlanner({
        runtime: preparedClaim.task.task.runtime as PlannerRuntime,
        cwd: preparedClaim.task.task.workspace ?? process.cwd(),
        timeoutMs: options.plannerTimeoutMs ?? 120_000,
      })
    : null;

  const cleanupIfDone = (): void => {
    const current = store.getTask(taskId).task;
    options.onLog?.(`finish ${current.id}: ${current.status}`);
    if (current.status !== "done") return;
    try {
      workspaces.cleanup(current);
    } catch (error) {
      options.onLog?.(`workspace cleanup failed ${current.id}: ${error instanceof Error ? error.message : String(error)}`);
    }
  };

  while (true) {
    const command = continuationPrompt && (sessionId || preparedClaim.task.task.runtime === "cline")
      ? buildGoalContinuationCommand(preparedClaim, runnerOptions, sessionId, continuationPrompt)
      : buildRunnerCommand(preparedClaim, runnerOptions, sessionId ?? undefined);
    const maxRuntimeMs = preparedClaim.task.task.maxRuntimeSeconds === null
      ? null
      : preparedClaim.task.task.maxRuntimeSeconds * 1_000 - (Date.now() - runStartedAt);
    options.onLog?.(
      `launch ${taskId} via ${preparedClaim.task.task.runtime}${goalMode ? ` goal turn ${turn}` : ""}; log=${logPath}`,
    );
    const execution = await executeTurn(command, store, scope, children, logPath, maxRuntimeMs);
    const currentDetail = store.getTask(taskId);
    const current = currentDetail.task;
    if (current.status !== "running" || current.currentRunId !== preparedClaim.run.id) {
      cleanupIfDone();
      return;
    }

    const latestEvent = currentDetail.events.at(-1);
    if (latestEvent?.kind === "reclaim_deferred" && latestEvent.runId === preparedClaim.run.id) {
      store.recoverAbandonedRun(preparedClaim.run.id, "reclaimed", "Claim reclaimed after worker termination", false);
      options.onLog?.(`reclaimed ${taskId} after deferred termination`);
      return;
    }

    const detail = execution.spawnError?.message ??
      `Runner exited without a terminal Kanban call (code=${execution.code}, signal=${execution.signal ?? "none"})`;
    if (execution.timedOut || (maxRuntimeMs !== null && maxRuntimeMs <= 0)) {
      store.failRun(scope, detail, { outcome: "timed_out", failureLimit: options.failureLimit });
      options.onLog?.(`requeue/fail ${taskId}: ${detail}`);
      return;
    }
    if (execution.spawnError) {
      store.failRun(scope, detail, { outcome: "spawn_failed", failureLimit: options.failureLimit });
      options.onLog?.(`requeue/fail ${taskId}: ${detail}`);
      return;
    }
    if (execution.code === 75) {
      store.failRun(scope, detail, {
        outcome: "rate_limited",
        countFailure: false,
        cooldownSeconds: Math.max(0, options.rateLimitCooldownSeconds ?? 60),
        failureLimit: options.failureLimit,
      });
      options.onLog?.(`requeue/fail ${taskId}: ${detail}`);
      return;
    }
    if (execution.code !== 0) {
      store.failRun(scope, detail, { failureLimit: options.failureLimit });
      options.onLog?.(`requeue/fail ${taskId}: ${detail}`);
      return;
    }
    if (!goalMode) {
      store.failRun(scope, detail, { outcome: "protocol_violation", failureLimit: options.failureLimit });
      options.onLog?.(`requeue/fail ${taskId}: ${detail}`);
      return;
    }

    store.pauseGoalRun(scope, turn);
    sessionId = sessionId ?? execution.sessionId;
    if (!sessionId && preparedClaim.task.task.runtime !== "cline") {
      store.failRun(scope, "Goal-mode runner did not report a resumable session id", {
        outcome: "protocol_violation",
        failureLimit: options.failureLimit,
      });
      return;
    }
    let judgment: GoalJudgment;
    try {
      judgment = options.goalJudge
        ? await options.goalJudge({ task: currentDetail, turn, workerOutput: execution.output })
        : await judgeGoalProgress(currentDetail, turn, execution.output, defaultGoalPlanner!);
    } catch (error) {
      const reason = `Goal judge failed: ${error instanceof Error ? error.message : String(error)}`;
      store.blockRun(scope, reason, "transient");
      options.onLog?.(`blocked ${taskId}: ${reason}`);
      return;
    }
    store.recordGoalJudgment(scope, { turn, ...judgment });
    if (judgment.complete) {
      store.completeRun(scope, {
        summary: `Goal accepted after ${turn} turn${turn === 1 ? "" : "s"}: ${judgment.reason}`,
        metadata: { goalMode: true, turns: turn, judgeReason: judgment.reason },
      });
      cleanupIfDone();
      return;
    }
    if (turn >= preparedClaim.task.task.goalMaxTurns) {
      store.blockRun(
        scope,
        `Goal turn budget exhausted after ${turn} turns: ${judgment.reason}`,
        "needs_input",
      );
      options.onLog?.(`goal budget exhausted ${taskId}`);
      return;
    }
    turn += 1;
    continuationPrompt = judgment.nextPrompt || `Continue toward the task acceptance criteria. Address this gap: ${judgment.reason}`;
  }
}

export async function runDispatcher(options: DispatcherOptions): Promise<void> {
  const manager = new BoardManager(options.dbPath);
  const workspaces = new WorkspaceManager(manager);
  const active = new Set<Promise<void>>();
  const children = new Set<ChildProcess>();
  let generatedClineApprovalDir: string | undefined;
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
      await deliverBoardNotifications(manager, boards, options);
      await decomposeBoardTriage(manager, boards, options);
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
          const claimOptions = claim.task.task.runtime === "cline" && !options.clineApprovalDir
            ? {
                ...options,
                clineApprovalDir: generatedClineApprovalDir ??= mkdtempSync(join(tmpdir(), "kanban-cline-approvals-")),
              }
            : options;
          let running: Promise<void>;
          running = runClaim(
            store,
            claim,
            claimOptions,
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
        await deliverBoardNotifications(manager, boards, options);
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
    if (generatedClineApprovalDir) rmSync(generatedClineApprovalDir, { recursive: true, force: true });
  }
}
