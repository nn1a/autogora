import { randomBytes, timingSafeEqual } from "node:crypto";
import {
  createReadStream,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { dirname, extname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { WebSocketServer, WebSocket } from "ws";

import { BoardManager, type BoardUpdate } from "./boards.js";
import { runDispatcher } from "./dispatcher.js";
import { garbageCollect } from "./maintenance.js";
import { deliverNotifications } from "./notifications.js";
import {
  createCliPlanner,
  decomposeTriageTask,
  describeProfileRoute,
  specifyTriageTask,
  type DecompositionPlan,
  type ProfileRoute,
} from "./orchestration.js";
import { ATTACHMENT_MAX_BYTES, type BulkMutation, type UpdateTaskInput } from "./store.js";
import {
  RUNTIMES,
  TASK_STATUSES,
  type ListTaskFilter,
  type Runtime,
  type TaskStatus,
} from "./types.js";
import { WorkspaceManager } from "./workspaces.js";

export interface DashboardServerOptions {
  dbPath: string;
  cliEntry: string;
  host?: string | undefined;
  port?: number | undefined;
  token?: string | undefined;
  webRoot?: string | undefined;
  onLog?: ((message: string) => void) | undefined;
}

export interface DashboardServerHandle {
  server: Server;
  token: string;
  url: string;
  close: () => Promise<void>;
}

type JsonObject = Record<string, unknown>;

const STATIC_CONTENT_TYPES: Record<string, string> = {
  ".css": "text/css; charset=utf-8",
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".svg": "image/svg+xml",
};

const TASK_SORTS = [
  "created",
  "created-desc",
  "priority",
  "priority-desc",
  "status",
  "assignee",
  "title",
  "updated",
] as const satisfies readonly NonNullable<ListTaskFilter["sort"]>[];

function secureEqual(left: string | undefined, right: string): boolean {
  if (!left) return false;
  const leftBytes = Buffer.from(left);
  const rightBytes = Buffer.from(right);
  return leftBytes.length === rightBytes.length && timingSafeEqual(leftBytes, rightBytes);
}

function cookieValue(request: IncomingMessage, name: string): string | undefined {
  for (const part of (request.headers.cookie ?? "").split(";")) {
    const [key, ...value] = part.trim().split("=");
    if (key === name) return decodeURIComponent(value.join("="));
  }
  return undefined;
}

function requestToken(request: IncomingMessage, url: URL): string | undefined {
  const authorization = request.headers.authorization;
  if (authorization?.startsWith("Bearer ")) return authorization.slice("Bearer ".length);
  return cookieValue(request, "kanban_session") ?? url.searchParams.get("token") ?? undefined;
}

function securityHeaders(response: ServerResponse): void {
  response.setHeader("x-content-type-options", "nosniff");
  response.setHeader("x-frame-options", "DENY");
  response.setHeader("referrer-policy", "no-referrer");
  response.setHeader(
    "content-security-policy",
    "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'none'",
  );
}

function sendJson(response: ServerResponse, status: number, value: unknown): void {
  securityHeaders(response);
  const body = JSON.stringify(value, null, 2);
  response.writeHead(status, { "content-type": "application/json; charset=utf-8", "content-length": Buffer.byteLength(body) });
  response.end(body);
}

function sendError(response: ServerResponse, error: unknown): void {
  const message = error instanceof Error ? error.message : String(error);
  const status = error instanceof SyntaxError || /requires|invalid|must|cannot be empty/i.test(message)
    ? 400
    : /exceeds \d+ bytes/i.test(message)
      ? 413
      : /not found/i.test(message)
        ? 404
        : /already|cannot|only|cycle|scoped|active|terminal/i.test(message)
          ? 409
          : 500;
  sendJson(response, status, { error: message });
}

async function readBody(request: IncomingMessage, limit: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    bytes += buffer.length;
    if (bytes > limit) throw new Error(`Request body exceeds ${limit} bytes`);
    chunks.push(buffer);
  }
  return Buffer.concat(chunks);
}

async function readJson(request: IncomingMessage, limit = 1024 * 1024): Promise<JsonObject> {
  const body = await readBody(request, limit);
  if (body.length === 0) return {};
  const value = JSON.parse(body.toString("utf8")) as unknown;
  if (!value || Array.isArray(value) || typeof value !== "object") throw new Error("JSON body must be an object");
  return value as JsonObject;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function nullableString(value: unknown): string | null | undefined {
  return value === null ? null : stringValue(value);
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function booleanValue(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function stringArray(value: unknown): string[] | undefined {
  return Array.isArray(value) && value.every((item) => typeof item === "string") ? value : undefined;
}

function runtimeValue(value: unknown): Runtime | undefined {
  return typeof value === "string" && RUNTIMES.includes(value as Runtime) ? value as Runtime : undefined;
}

function statusValue(value: unknown): TaskStatus | undefined {
  return typeof value === "string" && TASK_STATUSES.includes(value as TaskStatus) ? value as TaskStatus : undefined;
}

function sortValue(value: string | null): ListTaskFilter["sort"] {
  if (value === null) return "priority-desc";
  if (!TASK_SORTS.includes(value as (typeof TASK_SORTS)[number])) throw new Error(`Invalid task sort: ${value}`);
  return value as ListTaskFilter["sort"];
}

function profileRoute(value: unknown): ProfileRoute {
  if (!value || Array.isArray(value) || typeof value !== "object") throw new Error("Profile route must be an object");
  const route = value as JsonObject;
  const name = stringValue(route.name)?.trim();
  const runtime = runtimeValue(route.runtime);
  if (!name || !runtime || runtime === "manual") throw new Error("Profile route requires name and a worker runtime");
  return { name, runtime, description: stringValue(route.description) };
}

function boardFrom(manager: BoardManager, url: URL): string {
  return manager.resolve(url.searchParams.get("board") ?? undefined);
}

async function withStore<T>(manager: BoardManager, board: string, fn: (store: ReturnType<BoardManager["openStore"]>) => T | Promise<T>): Promise<T> {
  const store = manager.openStore(board);
  try {
    return await fn(store);
  } finally {
    store.close();
  }
}

function taskUpdate(body: JsonObject): UpdateTaskInput {
  return {
    title: stringValue(body.title),
    body: stringValue(body.body),
    assignee: nullableString(body.assignee),
    tenant: nullableString(body.tenant),
    runtime: runtimeValue(body.runtime),
    priority: numberValue(body.priority),
    workspace: nullableString(body.workspace),
    workspaceKind: stringValue(body.workspaceKind) as "scratch" | "dir" | "worktree" | undefined,
    branch: nullableString(body.branch),
    scheduledAt: nullableString(body.scheduledAt),
    maxRuntimeSeconds: body.maxRuntimeSeconds === null ? null : numberValue(body.maxRuntimeSeconds),
    skills: stringArray(body.skills),
    goalMode: booleanValue(body.goalMode),
    goalMaxTurns: numberValue(body.goalMaxTurns),
    status: statusValue(body.status),
  };
}

function terminatePid(pid: number | null): boolean {
  if (pid === null || pid === process.pid) return false;
  try {
    process.kill(pid, "SIGTERM");
    return true;
  } catch {
    return false;
  }
}

export async function startDashboardServer(options: DashboardServerOptions): Promise<DashboardServerHandle> {
  const manager = new BoardManager(resolve(options.dbPath));
  manager.create("default");
  const token = options.token ?? randomBytes(32).toString("base64url");
  if (token.length < 16) throw new Error("Dashboard token must contain at least 16 characters");
  const webRoot = resolve(options.webRoot ?? join(dirname(fileURLToPath(import.meta.url)), "../web"));
  const wss = new WebSocketServer({ noServer: true, maxPayload: 1024 * 1024 });

  const server = createServer(async (request, response) => {
    const url = new URL(request.url ?? "/", "http://localhost");
    securityHeaders(response);

    if (url.pathname === "/" && url.searchParams.has("token")) {
      if (!secureEqual(url.searchParams.get("token") ?? undefined, token)) {
        sendJson(response, 401, { error: "Unauthorized" });
        return;
      }
      response.writeHead(302, {
        location: "/",
        "set-cookie": `kanban_session=${encodeURIComponent(token)}; HttpOnly; SameSite=Strict; Path=/`,
        "cache-control": "no-store",
      });
      response.end();
      return;
    }

    if (!secureEqual(requestToken(request, url), token)) {
      sendJson(response, 401, { error: "Unauthorized" });
      return;
    }

    try {
      if (!url.pathname.startsWith("/api/")) {
        const staticFiles: Record<string, string> = { "/": "index.html", "/app.js": "app.js", "/styles.css": "styles.css" };
        const filename = staticFiles[url.pathname];
        if (!filename) {
          sendJson(response, 404, { error: "Not found" });
          return;
        }
        const path = join(webRoot, filename);
        if (!existsSync(path) || !statSync(path).isFile()) throw new Error(`Dashboard asset not found: ${filename}`);
        const content = readFileSync(path);
        response.writeHead(200, {
          "content-type": STATIC_CONTENT_TYPES[extname(path)] ?? "application/octet-stream",
          "content-length": content.length,
          "cache-control": filename === "index.html" ? "no-store" : "public, max-age=300",
        });
        response.end(content);
        return;
      }

      const segments = url.pathname.split("/").filter(Boolean).map(decodeURIComponent);
      const method = request.method ?? "GET";

      if (segments[1] === "boards") {
        if (segments.length === 2 && method === "GET") {
          sendJson(response, 200, { current: manager.getCurrent(), boards: manager.list(url.searchParams.get("archived") === "true") });
          return;
        }
        if (segments.length === 2 && method === "POST") {
          const body = await readJson(request);
          const metadata = manager.create(stringValue(body.slug) ?? "", {
            name: stringValue(body.name),
            description: stringValue(body.description),
            icon: stringValue(body.icon),
            color: stringValue(body.color),
            defaultWorkdir: nullableString(body.defaultWorkdir),
            orchestration: body.orchestration as BoardUpdate["orchestration"],
          });
          if (body.switch === true) manager.switch(metadata.slug);
          sendJson(response, 201, metadata);
          return;
        }
        const slug = segments[2];
        if (slug && method === "PATCH") {
          const body = await readJson(request);
          sendJson(response, 200, manager.update(slug, {
            name: stringValue(body.name),
            description: stringValue(body.description),
            icon: stringValue(body.icon),
            color: stringValue(body.color),
            defaultWorkdir: nullableString(body.defaultWorkdir),
            orchestration: body.orchestration as BoardUpdate["orchestration"],
          }));
          return;
        }
        if (slug && segments[3] === "switch" && method === "POST") {
          sendJson(response, 200, manager.switch(slug));
          return;
        }
        if (slug && method === "DELETE") {
          sendJson(response, 200, manager.remove(slug, url.searchParams.get("hard") === "true"));
          return;
        }
      }

      const board = boardFrom(manager, url);

      if (segments[1] === "board" && method === "GET") {
        const payload = await withStore(manager, board, (store) => {
          const tasks = store.listTasks({ includeArchived: url.searchParams.get("includeArchived") === "true", limit: 500 })
            .map((task) => {
              const detail = store.getTask(task.id);
              return {
                ...task,
                childrenDone: detail.children.filter((child) => child.status === "done").length,
                childrenTotal: detail.children.length,
                commentsCount: detail.comments.length,
                linksCount: detail.parents.length + detail.children.length,
              };
            });
          return { board: manager.read(board), tasks, stats: store.getStats(board), diagnostics: store.diagnose(board) };
        });
        sendJson(response, 200, payload);
        return;
      }

      if (segments[1] === "tasks" && segments.length === 2) {
        if (method === "GET") {
          const tasks = await withStore(manager, board, (store) => store.listTasks({
            status: statusValue(url.searchParams.get("status")),
            tenant: url.searchParams.get("tenant") ?? undefined,
            assignee: url.searchParams.get("assignee") ?? undefined,
            runtime: runtimeValue(url.searchParams.get("runtime")),
            includeArchived: url.searchParams.get("includeArchived") === "true",
            search: url.searchParams.get("search") ?? undefined,
            sort: sortValue(url.searchParams.get("sort")),
            limit: Number.parseInt(url.searchParams.get("limit") ?? "500", 10),
          }));
          sendJson(response, 200, tasks);
          return;
        }
        if (method === "POST") {
          const body = await readJson(request);
          const created = await withStore(manager, board, (store) => store.createTask({
            title: stringValue(body.title) ?? "",
            body: stringValue(body.body),
            board,
            tenant: nullableString(body.tenant),
            idempotencyKey: nullableString(body.idempotencyKey),
            assignee: nullableString(body.assignee),
            runtime: runtimeValue(body.runtime),
            priority: numberValue(body.priority),
            workspace: nullableString(body.workspace),
            workspaceKind: stringValue(body.workspaceKind) as "scratch" | "dir" | "worktree" | undefined,
            branch: nullableString(body.branch),
            status: statusValue(body.status),
            scheduledAt: nullableString(body.scheduledAt),
            maxRuntimeSeconds: body.maxRuntimeSeconds === null ? null : numberValue(body.maxRuntimeSeconds),
            skills: stringArray(body.skills),
            goalMode: booleanValue(body.goalMode),
            goalMaxTurns: numberValue(body.goalMaxTurns),
            maxRetries: numberValue(body.maxRetries),
            parents: stringArray(body.parents),
          }));
          sendJson(response, 201, created);
          return;
        }
      }

      if (segments[1] === "tasks" && segments[2] === "bulk" && method === "POST") {
        const body = await readJson(request);
        const ids = stringArray(body.ids);
        if (!ids?.length) throw new Error("Bulk mutation requires ids");
        const mutationBody = (body.mutation && typeof body.mutation === "object" ? body.mutation : body) as JsonObject;
        const mutation: BulkMutation = {
          status: statusValue(mutationBody.status),
          assignee: nullableString(mutationBody.assignee),
          priority: numberValue(mutationBody.priority),
          archive: booleanValue(mutationBody.archive),
          delete: booleanValue(mutationBody.delete),
        };
        sendJson(response, 200, await withStore(manager, board, (store) => store.bulkMutate(ids, mutation)));
        return;
      }

      if (segments[1] === "links" && (method === "POST" || method === "DELETE")) {
        const body = method === "POST" ? await readJson(request) : {};
        const parentId = stringValue(body.parentId) ?? url.searchParams.get("parentId") ?? "";
        const childId = stringValue(body.childId) ?? url.searchParams.get("childId") ?? "";
        const value = await withStore(manager, board, (store) =>
          method === "POST" ? store.linkTasks(parentId, childId) : store.unlinkTasks(parentId, childId));
        sendJson(response, 200, value);
        return;
      }

      const taskId = segments[1] === "tasks" ? segments[2] : undefined;
      if (taskId) {
        if (segments.length === 3 && method === "GET") {
          sendJson(response, 200, await withStore(manager, board, (store) => ({
            ...store.getTask(taskId),
            workerContext: store.buildWorkerContext(taskId),
          })));
          return;
        }
        if (segments.length === 3 && method === "PATCH") {
          const body = await readJson(request);
          const value = await withStore(manager, board, (store) => {
            const status = statusValue(body.status);
            if (status === "done") return store.completeTask(taskId, {
              summary: stringValue(body.summary),
              result: stringValue(body.result),
              metadata: body.metadata as Record<string, unknown> | undefined,
            });
            if (status === "blocked") return store.blockTask(taskId, {
              reason: stringValue(body.reason) ?? "Blocked from dashboard",
              kind: stringValue(body.kind) as "dependency" | "needs_input" | "capability" | "transient" | undefined,
            });
            if (status === "archived") return store.archiveTask(taskId);
            return store.updateTask(taskId, taskUpdate(body));
          });
          sendJson(response, 200, value);
          return;
        }
        if (segments.length === 3 && method === "DELETE") {
          sendJson(response, 200, await withStore(manager, board, (store) => store.deleteTask(taskId)));
          return;
        }
        const action = segments[3];
        if (action === "claim" && method === "POST") {
          const body = await readJson(request);
          const value = await withStore(manager, board, (store) => {
            const claim = store.claimTask({
              taskId,
              claimTtlSeconds: numberValue(body.ttlSeconds) ?? 900,
              workerId: stringValue(body.workerId) ?? `dashboard-${process.pid}`,
            });
            if (!claim) throw new Error(`Task is not claimable: ${taskId}`);
            try {
              return new WorkspaceManager(manager).prepare(store, claim);
            } catch (error) {
              const message = error instanceof Error ? error.message : String(error);
              store.failRun(
                { runId: claim.run.id, claimToken: claim.claimToken },
                `Workspace preparation failed: ${message}`,
              );
              throw error;
            }
          });
          sendJson(response, 200, value);
          return;
        }
        if (action === "comments" && method === "POST") {
          const body = await readJson(request);
          sendJson(response, 201, await withStore(manager, board, (store) =>
            store.addComment(taskId, stringValue(body.author) ?? "human", stringValue(body.body) ?? "")));
          return;
        }
        if (["complete", "block", "unblock", "promote", "schedule", "archive"].includes(action ?? "") && method === "POST") {
          const body = await readJson(request);
          const value = await withStore(manager, board, (store) => {
            if (action === "complete") return store.completeTask(taskId, {
              summary: stringValue(body.summary), result: stringValue(body.result), metadata: body.metadata as Record<string, unknown> | undefined,
            });
            if (action === "block") return store.blockTask(taskId, {
              reason: stringValue(body.reason) ?? "Blocked from dashboard",
              kind: stringValue(body.kind) as "dependency" | "needs_input" | "capability" | "transient" | undefined,
            });
            if (action === "unblock") return store.unblockTask(taskId);
            if (action === "promote") return store.promoteTask(taskId);
            if (action === "schedule") return store.scheduleTask(taskId, nullableString(body.at) ?? null, stringValue(body.reason));
            return store.archiveTask(taskId);
          });
          sendJson(response, 200, value);
          return;
        }
        if (action === "attachments" && method === "POST") {
          const contentType = request.headers["content-type"] ?? "application/octet-stream";
          if (contentType.includes("application/json")) {
            const body = await readJson(request);
            const value = await withStore(manager, board, (store) => {
              if (stringValue(body.url)) return store.attachUrl(taskId, stringValue(body.url)!, stringValue(body.name));
              if (stringValue(body.path)) return store.attachFile(taskId, stringValue(body.path)!, stringValue(body.name));
              throw new Error("Attachment JSON requires url or path");
            });
            sendJson(response, 201, value);
            return;
          }
          const body = await readBody(request, ATTACHMENT_MAX_BYTES);
          const directory = mkdtempSync(join(tmpdir(), "kanban-upload-"));
          const name = url.searchParams.get("name") ?? "upload.bin";
          const temporaryPath = join(directory, "upload");
          try {
            writeFileSync(temporaryPath, body);
            sendJson(response, 201, await withStore(manager, board, (store) => store.attachFile(taskId, temporaryPath, name)));
          } finally {
            rmSync(directory, { recursive: true, force: true });
          }
          return;
        }
        if (action === "attachments" && segments[4] && method === "DELETE") {
          sendJson(response, 200, await withStore(manager, board, (store) => store.removeAttachment(taskId, segments[4]!)));
          return;
        }
        if (action === "log" && method === "GET") {
          sendJson(response, 200, await withStore(manager, board, (store) => store.readRunLog(
            taskId,
            Number.parseInt(url.searchParams.get("tailBytes") ?? "65536", 10),
            url.searchParams.get("runId") ?? undefined,
          )));
          return;
        }
        if (action === "specify" && method === "POST") {
          const body = await readJson(request);
          const settings = manager.read(board).orchestration;
          const value = await withStore(manager, board, (store) => specifyTriageTask(store, taskId, {
            specification: stringValue(body.title) && stringValue(body.body)
              ? { title: stringValue(body.title)!, body: stringValue(body.body)! }
              : undefined,
            planner: createCliPlanner({ runtime: settings.plannerRuntime, cwd: process.cwd() }),
            author: stringValue(body.author),
          }));
          sendJson(response, 200, value);
          return;
        }
        if (action === "decompose" && method === "POST") {
          const body = await readJson(request);
          const settings = manager.read(board).orchestration;
          const profiles = settings.profiles.map((profile) => ({ ...profile } satisfies ProfileRoute));
          const defaultProfile = profiles.find((profile) => profile.name === settings.defaultProfile) ??
            profiles[0] ?? { name: `${settings.plannerRuntime}-worker`, runtime: settings.plannerRuntime };
          const orchestratorProfile = profiles.find((profile) => profile.name === settings.orchestratorProfile) ?? defaultProfile;
          const value = await withStore(manager, board, (store) => decomposeTriageTask(store, taskId, {
            profiles,
            defaultProfile,
            orchestratorProfile,
            autoPromoteChildren: settings.autoPromoteChildren,
            plan: body.plan as DecompositionPlan | undefined,
            planner: createCliPlanner({ runtime: settings.plannerRuntime, cwd: process.cwd() }),
          }));
          sendJson(response, 200, value);
          return;
        }
      }

      if (segments[1] === "attachments" && segments[2] && segments[3] === "download" && method === "GET") {
        const task = url.searchParams.get("taskId");
        if (!task) throw new Error("Attachment download requires taskId");
        const attachment = await withStore(manager, board, (store) =>
          store.getTask(task).attachments.find((item) => item.id === segments[2]));
        if (!attachment?.path || !existsSync(attachment.path)) throw new Error("Attachment file not found");
        response.writeHead(200, {
          "content-type": attachment.mediaType ?? "application/octet-stream",
          "content-disposition": `attachment; filename*=UTF-8''${encodeURIComponent(attachment.name)}`,
          "content-length": statSync(attachment.path).size,
        });
        createReadStream(attachment.path).pipe(response);
        return;
      }

      if (segments[1] === "events" && method === "GET") {
        const events = await withStore(manager, board, (store) => store.listEvents({
          taskId: url.searchParams.get("taskId") ?? undefined,
          sinceId: Number.parseInt(url.searchParams.get("since") ?? "0", 10),
          kinds: (url.searchParams.get("kinds") ?? "").split(",").filter(Boolean),
          limit: Number.parseInt(url.searchParams.get("limit") ?? "500", 10),
        }));
        sendJson(response, 200, events);
        return;
      }

      if (segments[1] === "stats" && method === "GET") {
        sendJson(response, 200, await withStore(manager, board, (store) => store.getStats(board)));
        return;
      }
      if (segments[1] === "diagnostics" && method === "GET") {
        sendJson(response, 200, await withStore(manager, board, (store) => store.diagnose(board)));
        return;
      }
      if (segments[1] === "workers" && segments[2] === "active" && method === "GET") {
        sendJson(response, 200, await withStore(manager, board, (store) => store.listActiveRuns(board)));
        return;
      }
      if (segments[1] === "runs" && segments[2]) {
        if (segments.length === 3 && method === "GET") {
          sendJson(response, 200, await withStore(manager, board, (store) => store.getRun(segments[2]!)));
          return;
        }
        if (segments[3] === "terminate" && method === "POST") {
          const body = await readJson(request);
          const value = await withStore(manager, board, (store) => {
            const inspection = store.getRun(segments[2]!);
            if (inspection.run.status !== "running") throw new Error("Run is already terminal");
            const signaled = terminatePid(inspection.run.pid);
            const task = store.recoverAbandonedRun(
              inspection.run.id,
              "reclaimed",
              stringValue(body.reason) ?? "Run terminated from dashboard",
              false,
            );
            return { signaled, task };
          });
          sendJson(response, 200, value);
          return;
        }
      }

      if (segments[1] === "inspect" && method === "GET") {
        const value = await withStore(manager, board, (store) => ({
          diagnostics: store.diagnose(board),
          recentEvents: store.listEvents({ limit: 100 }),
        }));
        sendJson(response, 200, value);
        return;
      }

      if (segments[1] === "gc" && method === "POST") {
        const body = await readJson(request);
        sendJson(response, 200, garbageCollect(manager, board, {
          eventRetentionDays: numberValue(body.eventRetentionDays),
          logRetentionDays: numberValue(body.logRetentionDays),
          workspaceRetentionDays: numberValue(body.workspaceRetentionDays),
        }));
        return;
      }

      if (segments[1] === "notifications") {
        if (method === "GET") {
          sendJson(response, 200, await withStore(manager, board, (store) =>
            store.listNotificationSubscriptions(url.searchParams.get("taskId") ?? undefined)));
          return;
        }
        if (segments[2] === "deliver" && method === "POST") {
          sendJson(response, 200, await withStore(manager, board, (store) => deliverNotifications(store)));
          return;
        }
        if (method === "POST") {
          const body = await readJson(request);
          sendJson(response, 201, await withStore(manager, board, (store) => store.subscribeTask({
            taskId: stringValue(body.taskId) ?? "",
            platform: stringValue(body.platform) ?? "",
            chatId: stringValue(body.chatId) ?? "",
            threadId: nullableString(body.threadId),
            userId: nullableString(body.userId),
            eventKinds: stringArray(body.eventKinds),
            secret: nullableString(body.secret),
          })));
          return;
        }
        if (method === "DELETE") {
          const body = await readJson(request);
          sendJson(response, 200, { unsubscribed: await withStore(manager, board, (store) => store.unsubscribeTask({
            taskId: stringValue(body.taskId) ?? "",
            platform: stringValue(body.platform) ?? "",
            chatId: stringValue(body.chatId) ?? "",
            threadId: nullableString(body.threadId),
          })) });
          return;
        }
      }

      if (segments[1] === "profiles" && method === "GET") {
        const metadata = manager.read(board);
        const discovered = await withStore(manager, board, (store) => store.listTasks({ includeArchived: true, limit: 500 })
          .filter((task) => task.assignee && task.runtime !== "manual")
          .map((task) => ({ name: task.assignee!, runtime: task.runtime })));
        sendJson(response, 200, [...new Map([...discovered, ...metadata.orchestration.profiles].map((item) => [item.name, item])).values()]);
        return;
      }
      if (segments[1] === "profiles" && segments[2] && segments[3] === "describe-auto" && method === "POST") {
        const body = await readJson(request);
        const metadata = manager.read(board);
        const existing = metadata.orchestration.profiles.find((profile) => profile.name === segments[2]);
        const runtime = existing?.runtime ?? runtimeValue(body.runtime);
        if (!runtime || runtime === "manual") throw new Error("Profile auto-description requires a worker runtime");
        const evidence = await withStore(manager, board, (store) => store.listTasks({
          assignee: segments[2], includeArchived: true, limit: 50,
        }).map((task) => ({ title: task.title, body: task.body, skills: task.skills })));
        const described = await describeProfileRoute(
          { name: segments[2], runtime, description: existing?.description },
          evidence,
          createCliPlanner({ runtime: metadata.orchestration.plannerRuntime, cwd: process.cwd() }),
        );
        const profiles = metadata.orchestration.profiles.filter((profile) => profile.name !== described.name);
        profiles.push({ name: described.name, runtime: described.runtime, description: described.description ?? "" });
        manager.update(board, { orchestration: { profiles } });
        sendJson(response, 200, described);
        return;
      }

      if (segments[1] === "orchestration") {
        if (method === "GET") {
          sendJson(response, 200, manager.read(board).orchestration);
          return;
        }
        if (method === "PUT") {
          const body = await readJson(request);
          sendJson(response, 200, manager.update(board, { orchestration: body as BoardUpdate["orchestration"] }).orchestration);
          return;
        }
        if (segments[2] === "swarm" && method === "POST") {
          const body = await readJson(request);
          const workers = Array.isArray(body.workers) ? body.workers.map(profileRoute) : [];
          const verifier = profileRoute(body.verifier);
          const synthesizer = profileRoute(body.synthesizer);
          sendJson(response, 201, await withStore(manager, board, (store) => store.createSwarm({
            goal: stringValue(body.goal) ?? "",
            workers: workers.map((profile) => ({ assignee: profile.name, runtime: profile.runtime })),
            verifier: { assignee: verifier.name, runtime: verifier.runtime },
            synthesizer: { assignee: synthesizer.name, runtime: synthesizer.runtime },
            tenant: nullableString(body.tenant),
            workspace: nullableString(body.workspace),
            workspaceKind: stringValue(body.workspaceKind) as "scratch" | "dir" | "worktree" | undefined,
            blackboard: body.blackboard as Record<string, unknown> | undefined,
          })));
          return;
        }
      }

      if (segments[1] === "dispatch" && method === "POST") {
        const body = await readJson(request);
        void runDispatcher({
          dbPath: manager.defaultDbPath,
          cliEntry: resolve(options.cliEntry),
          board,
          once: true,
          maxWorkers: numberValue(body.maxWorkers) ?? 2,
          allowWrites: booleanValue(body.allowWrites) ?? false,
          onLog: options.onLog,
        }).catch((error) => options.onLog?.(`dashboard dispatch failed: ${error instanceof Error ? error.message : String(error)}`));
        sendJson(response, 202, { accepted: true, board });
        return;
      }

      sendJson(response, 404, { error: "Not found" });
    } catch (error) {
      sendError(response, error);
    }
  });

  server.on("upgrade", (request, socket, head) => {
    try {
      const url = new URL(request.url ?? "/", "http://localhost");
      if (url.pathname !== "/api/events/ws" || !secureEqual(requestToken(request, url), token)) {
        socket.write("HTTP/1.1 401 Unauthorized\r\nConnection: close\r\n\r\n");
        socket.destroy();
        return;
      }
      const board = boardFrom(manager, url);
      let cursor = Number.parseInt(url.searchParams.get("since") ?? "0", 10) || 0;
      wss.handleUpgrade(request, socket, head, (websocket) => {
        const pump = (): void => {
          if (websocket.readyState !== WebSocket.OPEN) return;
          void withStore(manager, board, (store) => store.listEvents({ sinceId: cursor, limit: 500 }))
            .then((events) => {
              if (events.length === 0 || websocket.readyState !== WebSocket.OPEN) return;
              cursor = events.at(-1)!.id;
              websocket.send(JSON.stringify({ type: "events", board, cursor, events }));
            })
            .catch((error) => {
              if (websocket.readyState === WebSocket.OPEN) {
                websocket.send(JSON.stringify({ type: "error", error: error instanceof Error ? error.message : String(error) }));
              }
            });
        };
        pump();
        const timer = setInterval(pump, 500);
        websocket.once("close", () => clearInterval(timer));
        websocket.once("error", () => clearInterval(timer));
      });
    } catch {
      socket.destroy();
    }
  });

  await new Promise<void>((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen(options.port ?? 0, options.host ?? "127.0.0.1", resolveListen);
  });
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Dashboard server did not bind a TCP port");
  const host = address.address.includes(":") ? `[${address.address}]` : address.address;
  const url = `http://${host}:${address.port}`;
  return {
    server,
    token,
    url,
    close: async () => {
      for (const client of wss.clients) client.terminate();
      await new Promise<void>((resolveClose) => wss.close(() => resolveClose()));
      await new Promise<void>((resolveClose, rejectClose) =>
        server.close((error) => error ? rejectClose(error) : resolveClose()),
      );
    },
  };
}
