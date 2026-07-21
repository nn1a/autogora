import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

import { KanbanStore, type RunScope } from "./store.js";
import { RUNTIMES, TASK_STATUSES, type Runtime, type TaskStatus } from "./types.js";

const runtimeSchema = z.enum(RUNTIMES);
const statusSchema = z.enum(TASK_STATUSES);

function result(value: unknown) {
  return {
    content: [{ type: "text" as const, text: JSON.stringify(value, null, 2) }],
  };
}

function scopedTaskId(requested?: string): string {
  const fromEnvironment = process.env.KANBAN_TASK_ID;
  if (fromEnvironment && requested && fromEnvironment !== requested) {
    throw new Error("This worker is scoped to a different task");
  }
  const taskId = fromEnvironment ?? requested;
  if (!taskId) throw new Error("task_id is required outside a dispatcher-scoped worker");
  return taskId;
}

function scopedRun(runId?: string, claimToken?: string): RunScope {
  const envRunId = process.env.KANBAN_RUN_ID;
  const envClaimToken = process.env.KANBAN_CLAIM_TOKEN;
  if (envRunId && runId && envRunId !== runId) throw new Error("This worker is scoped to a different run");
  if (envClaimToken && claimToken && envClaimToken !== claimToken) throw new Error("Claim token mismatch");
  const resolvedRunId = envRunId ?? runId;
  const resolvedClaimToken = envClaimToken ?? claimToken;
  if (!resolvedRunId || !resolvedClaimToken) {
    throw new Error("run_id and claim_token are required outside a dispatcher-scoped worker");
  }
  return { runId: resolvedRunId, claimToken: resolvedClaimToken };
}

function requireAdminSurface(): void {
  if (process.env.KANBAN_TASK_ID) {
    throw new Error("Dispatcher-scoped workers cannot plan, route, claim, or unblock board tasks");
  }
}

export function createKanbanServer(store: KanbanStore): McpServer {
  const server = new McpServer(
    { name: "hermes-kanban-mcp", version: "0.1.0" },
    {
      capabilities: { logging: {} },
      instructions:
        "Use this server as the canonical Kanban state. Workers must read their task first, heartbeat during long work, and terminate exactly once with kanban_complete or kanban_block. Orchestrators create and link tasks but do not implement them.",
    },
  );

  server.registerTool(
    "kanban_create",
    {
      title: "Create Kanban task",
      description: "Create a durable task, optionally assigned to a Claude or Codex worker and gated by parent tasks.",
      inputSchema: z.object({
        title: z.string().min(1),
        body: z.string().default(""),
        board: z.string().default("default"),
        assignee: z.string().nullable().optional(),
        runtime: runtimeSchema.default("manual"),
        priority: z.number().int().default(0),
        workspace: z.string().nullable().optional(),
        status: statusSchema.optional(),
        max_retries: z.number().int().min(1).max(20).default(2),
        parents: z.array(z.string()).default([]),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async (input) => {
      requireAdminSurface();
      return result(
        store.createTask({
          title: input.title,
          body: input.body,
          board: input.board,
          assignee: input.assignee,
          runtime: input.runtime as Runtime,
          priority: input.priority,
          workspace: input.workspace,
          status: input.status as TaskStatus | undefined,
          maxRetries: input.max_retries,
          parents: input.parents,
        }),
      );
    },
  );

  server.registerTool(
    "kanban_list",
    {
      title: "List Kanban tasks",
      description: "List board tasks with optional status, assignee, and runtime filters.",
      inputSchema: z.object({
        board: z.string().default("default"),
        status: statusSchema.optional(),
        assignee: z.string().optional(),
        runtime: runtimeSchema.optional(),
        include_archived: z.boolean().default(false),
        limit: z.number().int().min(1).max(500).default(100),
      }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (input) => {
      requireAdminSurface();
      return result(
        store.listTasks({
          board: input.board,
          status: input.status as TaskStatus | undefined,
          assignee: input.assignee,
          runtime: input.runtime as Runtime | undefined,
          includeArchived: input.include_archived,
          limit: input.limit,
        }),
      );
    },
  );

  server.registerTool(
    "kanban_show",
    {
      title: "Show Kanban task",
      description: "Read a task with dependencies, comments, run history, and events. Scoped workers may omit task_id.",
      inputSchema: z.object({ task_id: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id }) => result(store.getTask(scopedTaskId(task_id))),
  );

  server.registerTool(
    "kanban_update",
    {
      title: "Update Kanban task",
      description: "Update task metadata or perform an administrative status transition.",
      inputSchema: z.object({
        task_id: z.string(),
        title: z.string().min(1).optional(),
        body: z.string().optional(),
        assignee: z.string().nullable().optional(),
        runtime: runtimeSchema.optional(),
        priority: z.number().int().optional(),
        workspace: z.string().nullable().optional(),
        status: statusSchema.optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, ...updates }) => {
      requireAdminSurface();
      return result(
        store.updateTask(task_id, {
          ...updates,
          runtime: updates.runtime as Runtime | undefined,
          status: updates.status as TaskStatus | undefined,
        }),
      );
    },
  );

  server.registerTool(
    "kanban_comment",
    {
      title: "Comment on Kanban task",
      description: "Append a durable handoff or progress note to a task.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        author: z.string().default("agent"),
        body: z.string().min(1),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, author, body }) => result(store.addComment(scopedTaskId(task_id), author, body)),
  );

  server.registerTool(
    "kanban_link",
    {
      title: "Link Kanban dependency",
      description: "Create a parent-to-child dependency. Cycles and cross-board links are rejected.",
      inputSchema: z.object({ parent_id: z.string(), child_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_id, child_id }) => {
      requireAdminSurface();
      return result(store.linkTasks(parent_id, child_id));
    },
  );

  server.registerTool(
    "kanban_claim",
    {
      title: "Claim Kanban task",
      description: "Atomically claim one ready task and create a run lease. Normally used by the dispatcher.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().default("default"),
        runtime: runtimeSchema.optional(),
        worker_id: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, runtime, worker_id }) => {
      requireAdminSurface();
      return result(
        store.claimTask({
          taskId: task_id,
          board,
          runtime: runtime as Runtime | undefined,
          workerId: worker_id,
        }),
      );
    },
  );

  server.registerTool(
    "kanban_heartbeat",
    {
      title: "Heartbeat Kanban run",
      description: "Refresh the active run lease and optionally record a concise progress note.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        note: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, note }) => result(store.heartbeat(scopedRun(run_id, claim_token), note)),
  );

  server.registerTool(
    "kanban_complete",
    {
      title: "Complete Kanban run",
      description: "Complete the active run with a human summary and optional structured evidence.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        summary: z.string().min(1),
        metadata: z.record(z.string(), z.unknown()).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, summary, metadata }) =>
      result(store.completeRun(scopedRun(run_id, claim_token), summary, metadata)),
  );

  server.registerTool(
    "kanban_block",
    {
      title: "Block Kanban run",
      description: "Stop the active run because it needs human input, a capability, or an unresolved dependency.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        reason: z.string().min(1),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, reason }) =>
      result(store.blockRun(scopedRun(run_id, claim_token), reason)),
  );

  server.registerTool(
    "kanban_unblock",
    {
      title: "Unblock Kanban task",
      description: "Release a blocked task back to ready, or todo while a parent dependency remains open.",
      inputSchema: z.object({ task_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id }) => {
      requireAdminSurface();
      return result(store.unblockTask(task_id));
    },
  );

  return server;
}

export async function runStdioServer(dbPath: string): Promise<void> {
  const store = new KanbanStore(dbPath);
  const server = createKanbanServer(store);
  const transport = new StdioServerTransport();
  const shutdown = async (): Promise<void> => {
    await server.close();
    store.close();
  };
  process.once("SIGINT", () => void shutdown());
  process.once("SIGTERM", () => void shutdown());
  await server.connect(transport);
}
