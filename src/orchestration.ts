import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import {
  KanbanStore,
  type TaskGraphResult,
} from "./store.js";
import { RUNTIMES, type Runtime, type TaskDetail } from "./types.js";

export interface ProfileRoute {
  name: string;
  description?: string | undefined;
  runtime: Exclude<Runtime, "manual">;
}

export interface SpecificationPlan {
  title: string;
  body: string;
}

export interface DecompositionPlan {
  fanout: boolean;
  rootTitle: string;
  rootBody: string;
  reason: string;
  tasks: Array<{
    key: string;
    title: string;
    body: string;
    assignee: string;
    runtime: Exclude<Runtime, "manual">;
    priority: number;
    skills: string[];
  }>;
  dependencies: Array<{ parent: string; child: string }>;
}

export interface GoalJudgment {
  complete: boolean;
  reason: string;
  nextPrompt: string;
}

export interface StructuredPlannerRequest {
  kind: "specify" | "decompose" | "goal_judge";
  prompt: string;
  schema: Record<string, unknown>;
}

export type StructuredPlanner = (request: StructuredPlannerRequest) => Promise<unknown>;

const SPECIFICATION_SCHEMA: Record<string, unknown> = {
  type: "object",
  additionalProperties: false,
  properties: {
    title: { type: "string", minLength: 1 },
    body: { type: "string", minLength: 1 },
  },
  required: ["title", "body"],
};

const DECOMPOSITION_SCHEMA: Record<string, unknown> = {
  type: "object",
  additionalProperties: false,
  properties: {
    fanout: { type: "boolean" },
    rootTitle: { type: "string" },
    rootBody: { type: "string" },
    reason: { type: "string" },
    tasks: {
      type: "array",
      maxItems: 100,
      items: {
        type: "object",
        additionalProperties: false,
        properties: {
          key: { type: "string", minLength: 1 },
          title: { type: "string", minLength: 1 },
          body: { type: "string", minLength: 1 },
          assignee: { type: "string", minLength: 1 },
          runtime: { type: "string", enum: ["claude", "codex"] },
          priority: { type: "integer" },
          skills: { type: "array", items: { type: "string" } },
        },
        required: ["key", "title", "body", "assignee", "runtime", "priority", "skills"],
      },
    },
    dependencies: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        properties: {
          parent: { type: "string", minLength: 1 },
          child: { type: "string", minLength: 1 },
        },
        required: ["parent", "child"],
      },
    },
  },
  required: ["fanout", "rootTitle", "rootBody", "reason", "tasks", "dependencies"],
};

const GOAL_JUDGE_SCHEMA: Record<string, unknown> = {
  type: "object",
  additionalProperties: false,
  properties: {
    complete: { type: "boolean" },
    reason: { type: "string", minLength: 1 },
    nextPrompt: { type: "string" },
  },
  required: ["complete", "reason", "nextPrompt"],
};

function record(value: unknown, label: string): Record<string, unknown> {
  if (!value || Array.isArray(value) || typeof value !== "object") throw new Error(`${label} must be a JSON object`);
  return value as Record<string, unknown>;
}

function requiredString(value: unknown, label: string): string {
  if (typeof value !== "string" || !value.trim()) throw new Error(`${label} must be a non-empty string`);
  return value.trim();
}

function parseSpecification(value: unknown): SpecificationPlan {
  const plan = record(value, "Specification plan");
  return {
    title: requiredString(plan.title, "Specification title"),
    body: requiredString(plan.body, "Specification body"),
  };
}

function parseDecomposition(value: unknown): DecompositionPlan {
  const plan = record(value, "Decomposition plan");
  if (typeof plan.fanout !== "boolean") throw new Error("Decomposition fanout must be boolean");
  if (!Array.isArray(plan.tasks) || !Array.isArray(plan.dependencies)) {
    throw new Error("Decomposition tasks and dependencies must be arrays");
  }
  const tasks = plan.tasks.map((rawTask, index) => {
    const task = record(rawTask, `Decomposition task ${index + 1}`);
    const runtime = requiredString(task.runtime, `Task ${index + 1} runtime`) as Runtime;
    if (!RUNTIMES.includes(runtime) || runtime === "manual") throw new Error(`Invalid task runtime: ${runtime}`);
    if (!Array.isArray(task.skills) || task.skills.some((skill) => typeof skill !== "string")) {
      throw new Error(`Task ${index + 1} skills must be strings`);
    }
    if (!Number.isInteger(task.priority)) throw new Error(`Task ${index + 1} priority must be an integer`);
    return {
      key: requiredString(task.key, `Task ${index + 1} key`),
      title: requiredString(task.title, `Task ${index + 1} title`),
      body: requiredString(task.body, `Task ${index + 1} body`),
      assignee: requiredString(task.assignee, `Task ${index + 1} assignee`),
      runtime: runtime as Exclude<Runtime, "manual">,
      priority: task.priority as number,
      skills: [...new Set(task.skills.map((skill) => skill.trim()).filter(Boolean))],
    };
  });
  const dependencies = plan.dependencies.map((rawDependency, index) => {
    const dependency = record(rawDependency, `Dependency ${index + 1}`);
    return {
      parent: requiredString(dependency.parent, `Dependency ${index + 1} parent`),
      child: requiredString(dependency.child, `Dependency ${index + 1} child`),
    };
  });
  if (plan.fanout && tasks.length === 0) throw new Error("A fanout decomposition must include tasks");
  return {
    fanout: plan.fanout,
    rootTitle: requiredString(plan.rootTitle, "Decomposition root title"),
    rootBody: requiredString(plan.rootBody, "Decomposition root body"),
    reason: typeof plan.reason === "string" ? plan.reason.trim() : "",
    tasks,
    dependencies,
  };
}

function parseGoalJudgment(value: unknown): GoalJudgment {
  const judgment = record(value, "Goal judgment");
  if (typeof judgment.complete !== "boolean") throw new Error("Goal judgment complete must be boolean");
  return {
    complete: judgment.complete,
    reason: requiredString(judgment.reason, "Goal judgment reason"),
    nextPrompt: typeof judgment.nextPrompt === "string" ? judgment.nextPrompt.trim() : "",
  };
}

function plannerPromptForSpecification(task: TaskDetail): string {
  return [
    "You are a Kanban triage specifier.",
    "Rewrite the rough idea into a precise, executable task without inventing external facts.",
    "The body must include scope, concrete deliverables, acceptance criteria, constraints, and verification.",
    "Return only the requested structured object.",
    "",
    `Task id: ${task.task.id}`,
    `Title: ${task.task.title}`,
    `Body: ${task.task.body || "(empty)"}`,
    `Tenant: ${task.task.tenant ?? "(none)"}`,
  ].join("\n");
}

function plannerPromptForDecomposition(task: TaskDetail, profiles: ProfileRoute[]): string {
  const roster = profiles.map((profile) =>
    `- ${profile.name} [${profile.runtime}]: ${profile.description?.trim() || "no description"}`,
  ).join("\n");
  return [
    "You are a Kanban graph decomposer.",
    "Decide whether this triage idea benefits from independent parallel or sequential specialist tasks.",
    "If not, set fanout=false and return an improved rootTitle/rootBody with empty tasks and dependencies.",
    "If yes, produce a small acyclic graph. Dependency parent means prerequisite; child waits for parent.",
    "Use only assignee names from the profile roster. Every task needs a complete handoff-ready body.",
    "Return only the requested structured object.",
    "",
    `Task id: ${task.task.id}`,
    `Title: ${task.task.title}`,
    `Body: ${task.task.body || "(empty)"}`,
    `Tenant: ${task.task.tenant ?? "(none)"}`,
    "",
    "Profile roster:",
    roster || "(empty)",
  ].join("\n");
}

function unwrapPlannerOutput(value: unknown): unknown {
  if (!value || Array.isArray(value) || typeof value !== "object") return value;
  const envelope = value as Record<string, unknown>;
  if (envelope.structured_output !== undefined) return envelope.structured_output;
  if (envelope.structuredOutput !== undefined) return envelope.structuredOutput;
  if (typeof envelope.result === "string") {
    try {
      return JSON.parse(envelope.result) as unknown;
    } catch {
      return envelope.result;
    }
  }
  if (envelope.result && typeof envelope.result === "object") return envelope.result;
  return value;
}

async function runProcess(
  command: string,
  args: string[],
  cwd: string,
  timeoutMs: number,
): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolveProcess, rejectProcess) => {
    const child = spawn(command, args, { cwd, env: process.env, stdio: ["ignore", "pipe", "pipe"] });
    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];
    let stdoutBytes = 0;
    let stderrBytes = 0;
    const outputLimit = 2 * 1_024 * 1_024;
    child.stdout.on("data", (chunk: Buffer) => {
      if (stdoutBytes >= outputLimit) return;
      const captured = chunk.subarray(0, outputLimit - stdoutBytes);
      stdout.push(captured);
      stdoutBytes += captured.length;
    });
    child.stderr.on("data", (chunk: Buffer) => {
      if (stderrBytes >= outputLimit) return;
      const captured = chunk.subarray(0, outputLimit - stderrBytes);
      stderr.push(captured);
      stderrBytes += captured.length;
    });
    let timedOut = false;
    let forceKillTimer: NodeJS.Timeout | undefined;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGTERM");
      forceKillTimer = setTimeout(() => child.kill("SIGKILL"), 5_000);
    }, Math.max(1_000, timeoutMs));
    child.once("error", (error) => {
      clearTimeout(timer);
      if (forceKillTimer) clearTimeout(forceKillTimer);
      rejectProcess(error);
    });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      if (forceKillTimer) clearTimeout(forceKillTimer);
      const output = { stdout: Buffer.concat(stdout).toString("utf8"), stderr: Buffer.concat(stderr).toString("utf8") };
      if (code === 0 && !timedOut) resolveProcess(output);
      else {
        rejectProcess(new Error(
          timedOut
            ? `Planner timed out after ${timeoutMs} ms`
            : `Planner exited with code=${code}, signal=${signal ?? "none"}: ${output.stderr.slice(-2_000)}`,
        ));
      }
    });
  });
}

export function createCliPlanner(options: {
  runtime: Exclude<Runtime, "manual">;
  cwd?: string | undefined;
  timeoutMs?: number | undefined;
}): StructuredPlanner {
  const cwd = resolve(options.cwd ?? process.cwd());
  const timeoutMs = options.timeoutMs ?? 120_000;
  return async ({ prompt, schema }) => {
    const directory = mkdtempSync(join(tmpdir(), "kanban-planner-"));
    const schemaPath = join(directory, "schema.json");
    const outputPath = join(directory, "output.json");
    writeFileSync(schemaPath, JSON.stringify(schema), "utf8");
    try {
      if (options.runtime === "codex") {
        await runProcess(process.env.KANBAN_CODEX_BIN ?? "codex", [
          "exec",
          "--ephemeral",
          "--color", "never",
          "--sandbox", "read-only",
          "--skip-git-repo-check",
          "-C", cwd,
          "--output-schema", schemaPath,
          "--output-last-message", outputPath,
          prompt,
        ], cwd, timeoutMs);
        return unwrapPlannerOutput(JSON.parse(readFileSync(outputPath, "utf8")) as unknown);
      }
      const output = await runProcess(process.env.KANBAN_CLAUDE_BIN ?? "claude", [
        "-p", prompt,
        "--output-format", "json",
        "--json-schema", JSON.stringify(schema),
        "--permission-mode", "dontAsk",
        "--tools", "",
        "--no-session-persistence",
      ], cwd, timeoutMs);
      return unwrapPlannerOutput(JSON.parse(output.stdout) as unknown);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  };
}

export async function specifyTriageTask(
  store: KanbanStore,
  taskId: string,
  options: {
    planner?: StructuredPlanner | undefined;
    specification?: SpecificationPlan | undefined;
    author?: string | undefined;
  } = {},
): Promise<TaskDetail> {
  const task = store.getTask(taskId);
  if (task.task.status !== "triage") throw new Error(`Task is not in triage: ${taskId}`);
  let specification: SpecificationPlan;
  if (options.specification) specification = parseSpecification(options.specification);
  else {
    if (!options.planner) throw new Error("A planner or explicit specification is required");
    specification = parseSpecification(await options.planner({
      kind: "specify",
      prompt: plannerPromptForSpecification(task),
      schema: SPECIFICATION_SCHEMA,
    }));
  }
  return store.specifyTask(taskId, { ...specification, author: options.author });
}

export async function decomposeTriageTask(
  store: KanbanStore,
  taskId: string,
  options: {
    profiles: ProfileRoute[];
    defaultProfile: ProfileRoute;
    orchestratorProfile?: ProfileRoute | undefined;
    planner?: StructuredPlanner | undefined;
    plan?: DecompositionPlan | undefined;
  },
): Promise<{ fanout: boolean; reason: string; task: TaskDetail; graph?: TaskGraphResult | undefined }> {
  const task = store.getTask(taskId);
  if (task.task.status !== "triage") throw new Error(`Task is not in triage: ${taskId}`);
  const profiles = [...new Map([...options.profiles, options.defaultProfile].map((profile) => [profile.name, profile])).values()];
  const rawPlan = options.plan ?? await options.planner?.({
    kind: "decompose",
    prompt: plannerPromptForDecomposition(task, profiles),
    schema: DECOMPOSITION_SCHEMA,
  });
  if (!rawPlan) throw new Error("A planner or explicit decomposition plan is required");
  const plan = options.plan ? parseDecomposition(options.plan) : parseDecomposition(rawPlan);
  if (!plan.fanout) {
    const specified = store.specifyTask(taskId, { title: plan.rootTitle, body: plan.rootBody, author: "decomposer" });
    return { fanout: false, reason: plan.reason, task: specified };
  }

  const profilesByName = new Map(profiles.map((profile) => [profile.name, profile]));
  const nodes = plan.tasks.map((planned) => {
    const profile = profilesByName.get(planned.assignee) ?? options.defaultProfile;
    return {
      ...planned,
      assignee: profile.name,
      runtime: profile.runtime,
    };
  });
  const orchestrator = options.orchestratorProfile ?? options.defaultProfile;
  const graph = store.applyTaskGraph({
    rootTaskId: taskId,
    rootTitle: plan.rootTitle,
    rootBody: plan.rootBody,
    orchestratorAssignee: orchestrator.name,
    orchestratorRuntime: orchestrator.runtime,
    nodes,
    dependencies: plan.dependencies,
  });
  return { fanout: true, reason: plan.reason, task: graph.root, graph };
}

export async function judgeGoalProgress(
  task: TaskDetail,
  turn: number,
  workerOutput: string,
  planner: StructuredPlanner,
): Promise<GoalJudgment> {
  const output = workerOutput.length > 32 * 1_024
    ? workerOutput.slice(workerOutput.length - 32 * 1_024)
    : workerOutput;
  const prompt = [
    "You are the independent completion judge for a goal-mode Kanban worker.",
    "Compare the worker's latest output and durable task state against every acceptance criterion.",
    "Set complete=true only when the goal is demonstrably satisfied. Otherwise give one concrete next-turn instruction.",
    "Do not treat confidence, effort, or a promise to finish as evidence.",
    "Return only the requested structured object.",
    "",
    `Turn: ${turn} of ${task.task.goalMaxTurns}`,
    `Task: ${task.task.title}`,
    `Acceptance body:\n${task.task.body || "(empty)"}`,
    `Current status: ${task.task.status}`,
    `Current result: ${task.task.result ?? "(none)"}`,
    "",
    `Latest worker output:\n${output || "(empty)"}`,
  ].join("\n");
  return parseGoalJudgment(await planner({ kind: "goal_judge", prompt, schema: GOAL_JUDGE_SCHEMA }));
}
