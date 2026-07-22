import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

import { BoardManager, type BoardUpdate } from "./boards.js";
import { garbageCollect } from "./maintenance.js";
import { deliverNotifications } from "./notifications.js";
import {
  createCliPlanner,
  decomposeTriageTask,
  describeProfileRoute,
  specifyTriageTask,
  type DecompositionPlan,
} from "./orchestration.js";
import { KanbanStore, type RunScope } from "./store.js";
import {
  BLOCK_KINDS,
  PLANNER_RUNTIMES,
  RUNTIMES,
  TASK_STATUSES,
  WORKER_RUNTIMES,
  type BlockKind,
  type Runtime,
  type TaskStatus,
} from "./types.js";

const runtimeSchema = z.enum(RUNTIMES);
const statusSchema = z.enum(TASK_STATUSES);
const workerRuntimeSchema = z.enum(WORKER_RUNTIMES);
const plannerRuntimeSchema = z.enum(PLANNER_RUNTIMES);
const profileRouteSchema = z.object({
  name: z.string().min(1),
  runtime: workerRuntimeSchema,
  description: z.string().optional(),
});
const decompositionPlanSchema = z.object({
  fanout: z.boolean(),
  rootTitle: z.string().min(1),
  rootBody: z.string().min(1),
  reason: z.string(),
  tasks: z.array(z.object({
    key: z.string().min(1),
    title: z.string().min(1),
    body: z.string().min(1),
    assignee: z.string().min(1),
    runtime: workerRuntimeSchema,
    priority: z.number().int(),
    skills: z.array(z.string()),
  })).max(100),
  dependencies: z.array(z.object({ parent: z.string().min(1), child: z.string().min(1) })),
});
const boardOrchestrationSchema = z.object({
  autoDecompose: z.boolean().optional(),
  autoDecomposePerTick: z.number().int().min(1).max(100).optional(),
  autoPromoteChildren: z.boolean().optional(),
  plannerRuntime: plannerRuntimeSchema.optional(),
  defaultProfile: z.string().nullable().optional(),
  orchestratorProfile: z.string().nullable().optional(),
  profiles: z.array(profileRouteSchema.extend({ description: z.string().default("") })).max(200).optional(),
});

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

function selectedBoard(manager: BoardManager, requested?: string): string {
  const pinned = process.env.KANBAN_BOARD?.trim();
  if (pinned && requested && pinned !== requested.trim().toLowerCase()) {
    throw new Error("This worker is scoped to a different board");
  }
  return manager.resolve(pinned || requested);
}

function usingStore<T>(
  manager: BoardManager,
  requested: string | undefined,
  fn: (store: KanbanStore, board: string) => T,
): T {
  const board = selectedBoard(manager, requested);
  const store = manager.openStore(board);
  try {
    return fn(store, board);
  } finally {
    store.close();
  }
}

export function createKanbanServer(manager: BoardManager): McpServer {
  const server = new McpServer(
    { name: "taskcircuit", version: "0.1.0" },
    {
      capabilities: { logging: {} },
      instructions:
        "Use TaskCircuit as the canonical task state. Workers must read their task first and heartbeat during long work. Ordinary workers terminate exactly once with kanban_complete or kanban_block; goal-mode workers may leave a non-terminal progress handoff so the dispatcher can judge and resume the session. Orchestrators route work but do not implement it.",
    },
  );

  server.registerTool(
    "kanban_boards_list",
    {
      title: "List Kanban boards",
      description: "List isolated boards with metadata, paths, and per-status task counts.",
      inputSchema: z.object({ include_archived: z.boolean().default(false) }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ include_archived }) => {
      requireAdminSurface();
      return result(manager.list(include_archived));
    },
  );

  server.registerTool(
    "kanban_boards_create",
    {
      title: "Create Kanban board",
      description: "Create an isolated board with its own database, workspaces, attachments, and logs.",
      inputSchema: z.object({
        slug: z.string().min(1),
        name: z.string().optional(),
        description: z.string().optional(),
        icon: z.string().optional(),
        color: z.string().optional(),
        default_workdir: z.string().nullable().optional(),
        orchestration: boardOrchestrationSchema.optional(),
        switch: z.boolean().default(false),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (input) => {
      requireAdminSurface();
      const metadata = manager.create(input.slug, {
        name: input.name,
        description: input.description,
        icon: input.icon,
        color: input.color,
        defaultWorkdir: input.default_workdir,
        orchestration: input.orchestration as BoardUpdate["orchestration"],
      });
      if (input.switch) manager.switch(metadata.slug);
      return result(metadata);
    },
  );

  server.registerTool(
    "kanban_boards_update",
    {
      title: "Update Kanban board",
      description: "Update board presentation metadata and its default project directory.",
      inputSchema: z.object({
        slug: z.string().min(1),
        name: z.string().optional(),
        description: z.string().optional(),
        icon: z.string().optional(),
        color: z.string().optional(),
        default_workdir: z.string().nullable().optional(),
        orchestration: boardOrchestrationSchema.optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ slug, default_workdir, ...update }) => {
      requireAdminSurface();
      return result(manager.update(slug, {
        ...update,
        orchestration: update.orchestration as BoardUpdate["orchestration"],
        defaultWorkdir: default_workdir,
      }));
    },
  );

  server.registerTool(
    "kanban_boards_switch",
    {
      title: "Switch current Kanban board",
      description: "Persist the current board used when an explicit board is omitted.",
      inputSchema: z.object({ slug: z.string().min(1) }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ slug }) => {
      requireAdminSurface();
      return result(manager.switch(slug));
    },
  );

  server.registerTool(
    "kanban_boards_remove",
    {
      title: "Remove Kanban board",
      description: "Archive a named board by default, or permanently delete it when hard_delete is true.",
      inputSchema: z.object({ slug: z.string().min(1), hard_delete: z.boolean().default(false) }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ slug, hard_delete }) => {
      requireAdminSurface();
      return result(manager.remove(slug, hard_delete));
    },
  );

  server.registerTool(
    "kanban_create",
    {
      title: "Create Kanban task",
      description: "Create a durable task, optionally assigned to a Claude, Codex, Cline, or Gemini worker and gated by parent tasks.",
      inputSchema: z.object({
        title: z.string().min(1),
        body: z.string().default(""),
        board: z.string().optional(),
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
        workflow_template_id: z.string().nullable().optional(),
        current_step_key: z.string().nullable().optional(),
        max_retries: z.number().int().min(1).max(20).default(2),
        parents: z.array(z.string()).default([]),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async (input) => {
      requireAdminSurface();
      return result(usingStore(manager, input.board, (store, board) =>
        store.createTask({
          title: input.title,
          body: input.body,
          board,
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
          workflowTemplateId: input.workflow_template_id,
          currentStepKey: input.current_step_key,
          maxRetries: input.max_retries,
          parents: input.parents,
        }),
      ));
    },
  );

  server.registerTool(
    "kanban_list",
    {
      title: "List Kanban tasks",
      description: "List board tasks with optional status, assignee, and runtime filters.",
      inputSchema: z.object({
        board: z.string().optional(),
        status: statusSchema.optional(),
        tenant: z.string().optional(),
        assignee: z.string().optional(),
        runtime: runtimeSchema.optional(),
        workflow_template_id: z.string().optional(),
        current_step_key: z.string().optional(),
        include_archived: z.boolean().default(false),
        search: z.string().optional(),
        sort: z.enum(["created", "created-desc", "priority", "priority-desc", "status", "assignee", "title", "updated"]).default("priority-desc"),
        limit: z.number().int().min(1).max(500).default(100),
      }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async (input) => {
      requireAdminSurface();
      return result(usingStore(manager, input.board, (store, board) =>
        store.listTasks({
          board,
          status: input.status as TaskStatus | undefined,
          tenant: input.tenant,
          assignee: input.assignee,
          runtime: input.runtime as Runtime | undefined,
          workflowTemplateId: input.workflow_template_id,
          currentStepKey: input.current_step_key,
          includeArchived: input.include_archived,
          search: input.search,
          sort: input.sort,
          limit: input.limit,
        }),
      ));
    },
  );

  server.registerTool(
    "kanban_show",
    {
      title: "Show Kanban task",
      description: "Read a task with dependencies, comments, run history, and events. Scoped workers may omit task_id.",
      inputSchema: z.object({ task_id: z.string().optional(), board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) => result(usingStore(manager, board, (store) => {
      const resolvedTaskId = scopedTaskId(task_id);
      return {
        ...store.getTask(resolvedTaskId),
        relationshipGraph: store.getRelationshipGraph(resolvedTaskId),
        workerContext: store.buildWorkerContext(resolvedTaskId),
      };
    })),
  );

  server.registerTool(
    "kanban_context",
    {
      title: "Build Kanban worker context",
      description: "Return the bounded task body, hierarchy root, dependency phases, prerequisite handoffs, downstream tasks, attachments, prior attempts, and comments seen by a worker.",
      inputSchema: z.object({ task_id: z.string().optional(), board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) =>
      result(usingStore(manager, board, (store) => store.buildWorkerContext(scopedTaskId(task_id)))),
  );

  server.registerTool(
    "kanban_graph",
    {
      title: "Show TaskCircuit relationship graph",
      description: "Show the connected parent/subtask hierarchy and prerequisite/dependent DAG with enforced execution phases.",
      inputSchema: z.object({ task_id: z.string().optional(), board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) =>
      result(usingStore(manager, board, (store) => store.getRelationshipGraph(scopedTaskId(task_id)))),
  );

  server.registerTool(
    "kanban_stats",
    {
      title: "Get Kanban statistics",
      description: "Count board tasks by status, assignee, runtime, and tenant.",
      inputSchema: z.object({ board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store, resolvedBoard) => store.getStats(resolvedBoard)));
    },
  );

  server.registerTool(
    "kanban_diagnostics",
    {
      title: "Diagnose Kanban board",
      description: "Inspect task/run invariants, stranded ready tasks, promotion lag, and active workers.",
      inputSchema: z.object({ board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store, resolvedBoard) => store.diagnose(resolvedBoard)));
    },
  );

  server.registerTool(
    "kanban_events",
    {
      title: "Read Kanban events",
      description: "Read the append-only board event stream by cursor, task, and event kind.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_id: z.string().optional(),
        since_id: z.number().int().min(0).optional(),
        kinds: z.array(z.string()).default([]),
        limit: z.number().int().min(1).max(2_000).default(500),
      }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ board, task_id, since_id, kinds, limit }) => result(usingStore(manager, board, (store) => {
      const resolvedTaskId = process.env.KANBAN_TASK_ID ? scopedTaskId(task_id) : task_id;
      return store.listEvents({ taskId: resolvedTaskId, sinceId: since_id, kinds, limit });
    })),
  );

  server.registerTool(
    "kanban_runs",
    {
      title: "List Kanban runs",
      description: "Read full attempt history for one task.",
      inputSchema: z.object({ task_id: z.string().optional(), board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) =>
      result(usingStore(manager, board, (store) => store.getTask(scopedTaskId(task_id)).runs)),
  );

  server.registerTool(
    "kanban_log",
    {
      title: "Read Kanban worker log",
      description: "Read up to 1 MB from the tail of a task run log.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        run_id: z.string().optional(),
        tail_bytes: z.number().int().min(1).max(1024 * 1024).default(64 * 1024),
      }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board, run_id, tail_bytes }) =>
      result(usingStore(manager, board, (store) => store.readRunLog(scopedTaskId(task_id), tail_bytes, run_id))),
  );

  server.registerTool(
    "kanban_bulk",
    {
      title: "Bulk mutate Kanban tasks",
      description: "Apply one status, assignee, priority, archive, or delete mutation with per-task error reporting.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_ids: z.array(z.string()).min(1).max(500),
        status: statusSchema.optional(),
        assignee: z.string().nullable().optional(),
        priority: z.number().int().optional(),
        archive: z.boolean().optional(),
        delete: z.boolean().optional(),
      }).refine(
        ({ status, assignee, priority, archive, delete: hardDelete }) =>
          status !== undefined || assignee !== undefined || priority !== undefined || archive === true || hardDelete === true,
        { message: "At least one mutation is required" },
      ),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ board, task_ids, status, assignee, priority, archive, delete: hardDelete }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.bulkMutate(task_ids, {
        status: status as TaskStatus | undefined,
        assignee,
        priority,
        archive,
        delete: hardDelete,
      })));
    },
  );

  server.registerTool(
    "kanban_gc",
    {
      title: "Garbage collect Kanban data",
      description: "Delete expired events, worker logs, and terminal scratch workspaces from one board.",
      inputSchema: z.object({
        board: z.string().optional(),
        event_retention_days: z.number().int().min(0).default(30),
        log_retention_days: z.number().int().min(0).default(30),
        workspace_retention_days: z.number().int().min(0).default(7),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ board, event_retention_days, log_retention_days, workspace_retention_days }) => {
      requireAdminSurface();
      const resolvedBoard = selectedBoard(manager, board);
      return result(garbageCollect(manager, resolvedBoard, {
        eventRetentionDays: event_retention_days,
        logRetentionDays: log_retention_days,
        workspaceRetentionDays: workspace_retention_days,
      }));
    },
  );

  server.registerTool(
    "kanban_notify_subscribe",
    {
      title: "Subscribe to Kanban task notifications",
      description: "Subscribe a platform destination to future terminal events for one task. Use platform=webhook and an HTTP(S) URL as chat_id for the bundled adapter.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_id: z.string(),
        platform: z.string().min(1),
        chat_id: z.string().min(1),
        thread_id: z.string().nullable().optional(),
        user_id: z.string().nullable().optional(),
        event_kinds: z.array(z.string().min(1)).min(1).optional(),
        secret: z.string().nullable().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ board, task_id, platform, chat_id, thread_id, user_id, event_kinds, secret }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.subscribeTask({
        taskId: task_id,
        platform,
        chatId: chat_id,
        threadId: thread_id,
        userId: user_id,
        eventKinds: event_kinds,
        secret,
      })));
    },
  );

  server.registerTool(
    "kanban_notify_list",
    {
      title: "List Kanban notification subscriptions",
      description: "List board notification subscriptions, optionally for one task. Stored secrets are never returned.",
      inputSchema: z.object({ board: z.string().optional(), task_id: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ board, task_id }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.listNotificationSubscriptions(task_id)));
    },
  );

  server.registerTool(
    "kanban_notify_unsubscribe",
    {
      title: "Unsubscribe from Kanban task notifications",
      description: "Remove a task notification destination and its pending deliveries.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_id: z.string(),
        platform: z.string().min(1),
        chat_id: z.string().min(1),
        thread_id: z.string().nullable().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ board, task_id, platform, chat_id, thread_id }) => {
      requireAdminSurface();
      const unsubscribed = usingStore(manager, board, (store) => store.unsubscribeTask({
        taskId: task_id,
        platform,
        chatId: chat_id,
        threadId: thread_id,
      }));
      return result({ taskId: task_id, unsubscribed });
    },
  );

  server.registerTool(
    "kanban_notify_deliver",
    {
      title: "Deliver pending Kanban notifications",
      description: "Claim and deliver pending terminal events. The bundled webhook adapter performs external HTTP requests.",
      inputSchema: z.object({
        board: z.string().optional(),
        limit: z.number().int().min(1).max(500).default(25),
        timeout_ms: z.number().int().min(100).max(120_000).default(10_000),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async ({ board, limit, timeout_ms }) => {
      requireAdminSurface();
      const resolvedBoard = selectedBoard(manager, board);
      const store = manager.openStore(resolvedBoard);
      try {
        return result(await deliverNotifications(store, { limit, timeoutMs: timeout_ms }));
      } finally {
        store.close();
      }
    },
  );

  server.registerTool(
    "kanban_specify",
    {
      title: "Specify a Kanban triage task",
      description: "Rewrite a rough triage card into an executable specification and move it to todo. Provide title/body directly or use a Claude, Codex, Cline, or Gemini auxiliary planner.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_id: z.string(),
        title: z.string().min(1).optional(),
        body: z.string().min(1).optional(),
        author: z.string().optional(),
        planner_runtime: plannerRuntimeSchema.default("codex"),
        planner_timeout_ms: z.number().int().min(1_000).max(600_000).default(120_000),
      }).refine(({ title, body }) => (title === undefined) === (body === undefined), {
        message: "title and body must be provided together",
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async ({ board, task_id, title, body, author, planner_runtime, planner_timeout_ms }) => {
      requireAdminSurface();
      const resolvedBoard = selectedBoard(manager, board);
      const store = manager.openStore(resolvedBoard);
      try {
        const value = await specifyTriageTask(store, task_id, {
          specification: title && body ? { title, body } : undefined,
          planner: createCliPlanner({ runtime: planner_runtime, cwd: process.cwd(), timeoutMs: planner_timeout_ms }),
          author,
        });
        return result(value);
      } finally {
        store.close();
      }
    },
  );

  server.registerTool(
    "kanban_decompose",
    {
      title: "Decompose a Kanban triage task",
      description: "Use an explicit or agent-generated plan to atomically create and route a child task graph. Unknown assignees fall back to default_profile.",
      inputSchema: z.object({
        board: z.string().optional(),
        task_id: z.string(),
        profiles: z.array(profileRouteSchema).default([]),
        default_profile: profileRouteSchema,
        orchestrator_profile: profileRouteSchema.optional(),
        auto_promote_children: z.boolean().optional(),
        plan: decompositionPlanSchema.optional(),
        planner_runtime: plannerRuntimeSchema.default("codex"),
        planner_timeout_ms: z.number().int().min(1_000).max(600_000).default(120_000),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async ({
      board,
      task_id,
      profiles,
      default_profile,
      orchestrator_profile,
      auto_promote_children,
      plan,
      planner_runtime,
      planner_timeout_ms,
    }) => {
      requireAdminSurface();
      const resolvedBoard = selectedBoard(manager, board);
      const store = manager.openStore(resolvedBoard);
      try {
        const value = await decomposeTriageTask(store, task_id, {
          profiles,
          defaultProfile: default_profile,
          orchestratorProfile: orchestrator_profile,
          autoPromoteChildren: auto_promote_children ?? manager.read(resolvedBoard).orchestration.autoPromoteChildren,
          plan: plan as DecompositionPlan | undefined,
          planner: createCliPlanner({ runtime: planner_runtime, cwd: process.cwd(), timeoutMs: planner_timeout_ms }),
        });
        return result(value);
      } finally {
        store.close();
      }
    },
  );

  server.registerTool(
    "kanban_profile_describe_auto",
    {
      title: "Describe a Kanban routing profile",
      description: "Generate and persist a concise board routing description from the profile name and its prior task evidence.",
      inputSchema: z.object({
        board: z.string().optional(),
        name: z.string().min(1),
        runtime: workerRuntimeSchema,
        planner_runtime: plannerRuntimeSchema.optional(),
        planner_timeout_ms: z.number().int().min(1_000).max(600_000).default(120_000),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async ({ board, name, runtime, planner_runtime, planner_timeout_ms }) => {
      requireAdminSurface();
      const resolvedBoard = selectedBoard(manager, board);
      const metadata = manager.read(resolvedBoard);
      const store = manager.openStore(resolvedBoard);
      try {
        const existing = metadata.orchestration.profiles.find((profile) => profile.name === name);
        const evidence = store.listTasks({ assignee: name, includeArchived: true, limit: 50 })
          .map((task) => ({ title: task.title, body: task.body, skills: task.skills }));
        const described = await describeProfileRoute(
          { name, runtime, description: existing?.description },
          evidence,
          createCliPlanner({
            runtime: planner_runtime ?? metadata.orchestration.plannerRuntime,
            cwd: process.cwd(),
            timeoutMs: planner_timeout_ms,
          }),
        );
        const profiles = metadata.orchestration.profiles.filter((profile) => profile.name !== name);
        profiles.push({ name, runtime, description: described.description ?? "" });
        manager.update(resolvedBoard, { orchestration: { profiles } });
        return result(described);
      } finally {
        store.close();
      }
    },
  );

  server.registerTool(
    "kanban_swarm",
    {
      title: "Create a Kanban swarm",
      description: "Atomically create a completed blackboard, parallel workers, a gated verifier, and a gated synthesizer.",
      inputSchema: z.object({
        board: z.string().optional(),
        goal: z.string().min(1),
        workers: z.array(profileRouteSchema).min(1).max(50),
        verifier: profileRouteSchema,
        synthesizer: profileRouteSchema,
        tenant: z.string().nullable().optional(),
        workspace: z.string().nullable().optional(),
        workspace_kind: z.enum(["scratch", "dir", "worktree"]).optional(),
        blackboard: z.record(z.string(), z.unknown()).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ board, goal, workers, verifier, synthesizer, tenant, workspace, workspace_kind, blackboard }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.createSwarm({
        goal,
        workers: workers.map((profile) => ({ assignee: profile.name, runtime: profile.runtime })),
        verifier: { assignee: verifier.name, runtime: verifier.runtime },
        synthesizer: { assignee: synthesizer.name, runtime: synthesizer.runtime },
        tenant,
        workspace,
        workspaceKind: workspace_kind,
        blackboard,
      })));
    },
  );

  server.registerTool(
    "kanban_update",
    {
      title: "Update Kanban task",
      description: "Update task metadata or perform an administrative status transition.",
      inputSchema: z.object({
        task_id: z.string(),
        board: z.string().optional(),
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
        workflow_template_id: z.string().nullable().optional(),
        current_step_key: z.string().nullable().optional(),
        status: statusSchema.optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board, ...updates }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) =>
        store.updateTask(task_id, {
          ...updates,
          workspaceKind: updates.workspace_kind,
          scheduledAt: updates.scheduled_at,
          maxRuntimeSeconds: updates.max_runtime_seconds,
          goalMode: updates.goal_mode,
          goalMaxTurns: updates.goal_max_turns,
          workflowTemplateId: updates.workflow_template_id,
          currentStepKey: updates.current_step_key,
          runtime: updates.runtime as Runtime | undefined,
          status: updates.status as TaskStatus | undefined,
        }),
      ));
    },
  );

  server.registerTool(
    "kanban_comment",
    {
      title: "Comment on Kanban task",
      description: "Append a durable handoff or progress note to a task.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        author: z.string().default("agent"),
        body: z.string().min(1),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, author, body }) =>
      result(usingStore(manager, board, (store) => store.addComment(scopedTaskId(task_id), author, body))),
  );

  server.registerTool(
    "kanban_link",
    {
      title: "Link Kanban dependency",
      description: "Create an execution dependency: parent_id is the prerequisite and child_id is the dependent. Cycles and cross-board links are rejected.",
      inputSchema: z.object({ parent_id: z.string(), child_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_id, child_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.linkTasks(parent_id, child_id)));
    },
  );

  server.registerTool(
    "kanban_unlink",
    {
      title: "Unlink Kanban dependency",
      description: "Remove a prerequisite-to-dependent execution edge and recompute whether the dependent is ready.",
      inputSchema: z.object({ parent_id: z.string(), child_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_id, child_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.unlinkTasks(parent_id, child_id)));
    },
  );

  server.registerTool(
    "kanban_subtask_set",
    {
      title: "Set TaskCircuit subtask parent",
      description: "Place a task under one hierarchy parent without changing execution dependencies. Reparents an existing subtask atomically.",
      inputSchema: z.object({
        parent_task_id: z.string(),
        subtask_id: z.string(),
        position: z.number().int().min(0).optional(),
        board: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_task_id, subtask_id, position, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => ({
        detail: store.setSubtaskParent(parent_task_id, subtask_id, position),
        graph: store.getRelationshipGraph(subtask_id),
      })));
    },
  );

  server.registerTool(
    "kanban_subtask_remove",
    {
      title: "Remove TaskCircuit subtask parent",
      description: "Remove a parent/subtask hierarchy edge without changing execution dependencies.",
      inputSchema: z.object({
        parent_task_id: z.string(),
        subtask_id: z.string(),
        board: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ parent_task_id, subtask_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => ({
        detail: store.removeSubtask(parent_task_id, subtask_id),
        graph: store.getRelationshipGraph(subtask_id),
      })));
    },
  );

  server.registerTool(
    "kanban_promote",
    {
      title: "Promote Kanban task",
      description: "Move a parked task into the executable todo/ready pipeline, respecting dependencies.",
      inputSchema: z.object({ task_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.promoteTask(task_id)));
    },
  );

  server.registerTool(
    "kanban_schedule",
    {
      title: "Schedule Kanban task",
      description: "Park a task until an optional ISO-8601 start time or a manual promotion.",
      inputSchema: z.object({
        task_id: z.string(),
        board: z.string().optional(),
        scheduled_at: z.string().datetime({ offset: true }).nullable().default(null),
        reason: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board, scheduled_at, reason }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.scheduleTask(task_id, scheduled_at, reason)));
    },
  );

  server.registerTool(
    "kanban_archive",
    {
      title: "Archive Kanban task",
      description: "Archive a task and reclaim any active run.",
      inputSchema: z.object({ task_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.archiveTask(task_id)));
    },
  );

  server.registerTool(
    "kanban_delete",
    {
      title: "Delete Kanban task",
      description: "Permanently delete a task and its links, comments, runs, and events.",
      inputSchema: z.object({ task_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.deleteTask(task_id)));
    },
  );

  server.registerTool(
    "kanban_claim",
    {
      title: "Claim Kanban task",
      description: "Atomically claim one ready task and create a run lease. Normally used by the dispatcher.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        runtime: runtimeSchema.optional(),
        worker_id: z.string().optional(),
        ttl_seconds: z.number().int().min(1).max(86_400).default(900),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, runtime, worker_id, ttl_seconds }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store, resolvedBoard) =>
        store.claimTask({
          taskId: task_id,
          board: resolvedBoard,
          runtime: runtime as Runtime | undefined,
          workerId: worker_id,
          claimTtlSeconds: ttl_seconds,
        }),
      ));
    },
  );

  server.registerTool(
    "kanban_attach",
    {
      title: "Attach file to Kanban task",
      description: "Copy a local file up to 25 MB into durable board-scoped attachment storage.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        path: z.string().min(1),
        name: z.string().min(1).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, path, name }) =>
      result(usingStore(manager, board, (store) => store.attachFile(scopedTaskId(task_id), path, name))),
  );

  server.registerTool(
    "kanban_attach_url",
    {
      title: "Attach URL to Kanban task",
      description: "Add an http(s) reference to the durable task attachment list.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        url: z.string().url(),
        name: z.string().min(1).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: true },
    },
    async ({ task_id, board, url, name }) =>
      result(usingStore(manager, board, (store) => store.attachUrl(scopedTaskId(task_id), url, name))),
  );

  server.registerTool(
    "kanban_attachments",
    {
      title: "List Kanban attachments",
      description: "List durable file paths and URL references attached to a task.",
      inputSchema: z.object({ task_id: z.string().optional(), board: z.string().optional() }),
      annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    },
    async ({ task_id, board }) =>
      result(usingStore(manager, board, (store) => store.listAttachments(scopedTaskId(task_id)))),
  );

  server.registerTool(
    "kanban_attachment_remove",
    {
      title: "Remove Kanban attachment",
      description: "Remove attachment metadata and its stored file, when applicable.",
      inputSchema: z.object({
        task_id: z.string().optional(),
        board: z.string().optional(),
        attachment_id: z.string(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board, attachment_id }) =>
      result(usingStore(manager, board, (store) => store.removeAttachment(scopedTaskId(task_id), attachment_id))),
  );

  server.registerTool(
    "kanban_heartbeat",
    {
      title: "Heartbeat Kanban run",
      description: "Refresh the active run lease and optionally record a concise progress note.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        board: z.string().optional(),
        note: z.string().optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, board, note }) =>
      result(usingStore(manager, board, (store) => store.heartbeat(scopedRun(run_id, claim_token), note))),
  );

  server.registerTool(
    "kanban_complete",
    {
      title: "Complete Kanban run",
      description: "Complete the active run with a human summary and optional structured evidence.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        board: z.string().optional(),
        summary: z.string().min(1).optional(),
        result: z.string().min(1).optional(),
        metadata: z.record(z.string(), z.unknown()).optional(),
        artifacts: z.array(z.string().min(1)).default([]),
      }).refine((input) => Boolean(input.summary || input.result), "summary or result is required"),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, board, summary, result: taskResult, metadata, artifacts }) =>
      result(usingStore(manager, board, (store) =>
        store.completeRun(scopedRun(run_id, claim_token), { summary, result: taskResult, metadata, artifacts })
      )),
  );

  server.registerTool(
    "kanban_block",
    {
      title: "Block Kanban run",
      description: "Stop the active run because it needs human input, a capability, or an unresolved dependency.",
      inputSchema: z.object({
        run_id: z.string().optional(),
        claim_token: z.string().optional(),
        board: z.string().optional(),
        reason: z.string().min(1),
        kind: z.enum(BLOCK_KINDS).optional(),
      }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ run_id, claim_token, board, reason, kind }) =>
      result(usingStore(manager, board, (store) =>
        store.blockRun(scopedRun(run_id, claim_token), reason, kind as BlockKind | undefined)
      )),
  );

  server.registerTool(
    "kanban_unblock",
    {
      title: "Unblock Kanban task",
      description: "Release a blocked task back to ready, or todo while a parent dependency remains open.",
      inputSchema: z.object({ task_id: z.string(), board: z.string().optional() }),
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
    },
    async ({ task_id, board }) => {
      requireAdminSurface();
      return result(usingStore(manager, board, (store) => store.unblockTask(task_id)));
    },
  );

  return server;
}

export async function runStdioServer(dbPath: string): Promise<void> {
  const manager = new BoardManager(dbPath);
  const server = createKanbanServer(manager);
  const transport = new StdioServerTransport();
  const shutdown = async (): Promise<void> => {
    await server.close();
  };
  process.once("SIGINT", () => void shutdown());
  process.once("SIGTERM", () => void shutdown());
  await server.connect(transport);
}
