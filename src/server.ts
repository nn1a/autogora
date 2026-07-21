import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

import { KanbanStore, type RunScope } from "./store.js";
import { BLOCK_KINDS, RUNTIMES, TASK_STATUSES, type BlockKind, type Runtime, type TaskStatus } from "./types.js";

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
        tenant: z.string().nullable().optional(),
        idempotency_key: z.string().nullable().optional(),
        assignee: z.string().nullable().optional(),
        runtime: runtimeSchema.default("manual"),
        priority: z.number().int().default(0),
        workspace: z.string().nullable().optional(),
        workspace_kind: z.enum(["scratch", "dir", "worktree"]).optional(),
        branch: z.string().nullable().optional(),
        status: statusSchema.optional(),
        scheduled_at: z.string().datetime({ offset: true }).nullable().optional(),
        max_runtime_seconds: z.number().int().min(1).nullable().optional(),
        skills: z.array(z.string().min(1)).default([]),
        goal_mode: z.boolean().default(false),
        goal_max_turns: z.number().int().min(1).max(100).default(20),
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
          tenant: input.tenant,
          idempotencyKey: input.idempotency_key,
          assignee: input.assignee,
          runtime: input.runtime as Runtime,
          priority: input.priority,
          workspace: input.workspace,
          workspaceKind: input.workspace_kind,
          branch: input.branch,
          status: input.status as TaskStatus | undefined,
          scheduledAt: input.scheduled_at,
          maxRuntimeSeconds: input.max_runtime_seconds,
          skills: input.skills,
          goalMode: input.goal_mode,
          goalMaxTurns: input.goal_max_turns,
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
        tenant: z.string().optional(),
        assignee: z.string().optional(),
        runtime: runtimeSchema.optional(),
        include_archived: z.boolean().default(false),
        search: z.string().optional(),
        sort: z.enum(["created", "created-desc", "priority", "priority-desc", "status", "assignee", "title", "updated"]).default("priority-desc"),
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
          tenant: input.tenant,
          assignee: input.assignee,
          runtime: input.runtime as Runtime | undefined,
          includeArchived: input.include_archived,
          search: input.search,
          sort: input.sort,
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
        tenant: z.string().nullable().optional(),
        assignee: z.string().nullable().optional(),
        runtime: runtimeSchema.optional(),
        priority: z.number().int().optional(),
        workspace: z.string().nullable().optional(),
        workspace_kind: z.enum(["scratch", "dir", "worktree"]).optional(),
        branch: z.string().nullable().optional(),
        scheduled_at: z.string().datetime({ offset: true }).nullable().optional(),
        max_runtime_seconds: z.number().int().min(1).nullable().optional(),
        skills: z.array(z.string().min(1)).optional(),
        goal_mode: z.boolean().optional(),
        goal_max_turns: z.number().int().min(1).max(100).optional(),
        status: statusSchema.optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, ...updates }) => {
      requireAdminSurface();
      return result(
        store.updateTask(task_id, {
          ...updates,
          workspaceKind: updates.workspace_kind,
          scheduledAt: updates.scheduled_at,
          maxRuntimeSeconds: updates.max_runtime_seconds,
          goalMode: updates.goal_mode,
          goalMaxTurns: updates.goal_max_turns,
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
    "kanban_unlink",
    {
      title: "Unlink Kanban dependency",
      description: "Remove a parent-to-child dependency and recompute whether the child is ready.",
      inputSchema: z.object({ parent_id: z.string(), child_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_id, child_id }) => {
      requireAdminSurface();
      return result(store.unlinkTasks(parent_id, child_id));
    },
  );

  server.registerTool(
    "kanban_promote",
    {
      title: "Promote Kanban task",
      description: "Move a parked task into the executable todo/ready pipeline, respecting dependencies.",
      inputSchema: z.object({ task_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id }) => {
      requireAdminSurface();
      return result(store.promoteTask(task_id));
    },
  );

  server.registerTool(
    "kanban_schedule",
    {
      title: "Schedule Kanban task",
      description: "Park a task until an optional ISO-8601 start time or a manual promotion.",
      inputSchema: z.object({
        task_id: z.string(),
        scheduled_at: z.string().datetime({ offset: true }).nullable().default(null),
        reason: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, scheduled_at, reason }) => {
      requireAdminSurface();
      return result(store.scheduleTask(task_id, scheduled_at, reason));
    },
  );

  server.registerTool(
    "kanban_archive",
    {
      title: "Archive Kanban task",
      description: "Archive a task and reclaim any active run.",
      inputSchema: z.object({ task_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id }) => {
      requireAdminSurface();
      return result(store.archiveTask(task_id));
    },
  );

  server.registerTool(
    "kanban_delete",
    {
      title: "Delete Kanban task",
      description: "Permanently delete a task and its links, comments, runs, and events.",
      inputSchema: z.object({ task_id: z.string() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id }) => {
      requireAdminSurface();
      return result(store.deleteTask(task_id));
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
        ttl_seconds: z.number().int().min(1).max(86_400).default(900),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, runtime, worker_id, ttl_seconds }) => {
      requireAdminSurface();
      return result(
        store.claimTask({
          taskId: task_id,
          board,
          runtime: runtime as Runtime | undefined,
          workerId: worker_id,
          claimTtlSeconds: ttl_seconds,
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
        summary: z.string().min(1).optional(),
        result: z.string().min(1).optional(),
        metadata: z.record(z.string(), z.unknown()).optional(),
      }).refine((input) => Boolean(input.summary || input.result), "summary or result is required"),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, summary, result: taskResult, metadata }) =>
      result(store.completeRun(scopedRun(run_id, claim_token), { summary, result: taskResult, metadata })),
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
        kind: z.enum(BLOCK_KINDS).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, reason, kind }) =>
      result(store.blockRun(scopedRun(run_id, claim_token), reason, kind as BlockKind | undefined)),
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
