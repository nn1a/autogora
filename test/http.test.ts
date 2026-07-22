import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { WebSocket } from "ws";

import { BoardManager } from "../src/boards.js";
import { startDashboardServer } from "../src/http.js";

test("authenticated HTTP API and WebSocket stream share the board kernel", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-http-"));
  const dbPath = join(directory, "kanban.db");
  const token = "test-dashboard-token-32-characters";
  const dashboard = await startDashboardServer({
    dbPath,
    cliEntry: resolve("dist/cli.js"),
    token,
    port: 0,
  });
  const headers = { authorization: `Bearer ${token}`, "content-type": "application/json" };
  const request = async (path: string, init: RequestInit = {}): Promise<{ response: Response; value: any }> => {
    const response = await fetch(`${dashboard.url}${path}`, {
      ...init,
      headers: { ...headers, ...(init.headers ?? {}) },
    });
    const text = await response.text();
    return { response, value: text ? JSON.parse(text) : null };
  };
  let socket: WebSocket | undefined;
  try {
    const unauthorized = await fetch(`${dashboard.url}/api/boards`);
    assert.equal(unauthorized.status, 401);
    const bootstrap = await fetch(`${dashboard.url}/?token=${encodeURIComponent(token)}`, { redirect: "manual" });
    assert.equal(bootstrap.status, 302);
    const cookie = bootstrap.headers.get("set-cookie");
    assert.match(cookie ?? "", /kanban_session=/);
    const sessionHeaders = { cookie: cookie!.split(";", 1)[0]! };
    const html = await fetch(`${dashboard.url}/`, { headers: sessionHeaders });
    assert.equal(html.status, 200);
    const htmlText = await html.text();
    assert.match(htmlText, /<option>gemini<\/option>/);
    assert.match(htmlText, /id="theme-toggle"/);
    assert.match(htmlText, /role="dialog" aria-modal="true" aria-label="Task details"/);
    const app = await fetch(`${dashboard.url}/app.js`, { headers: sessionHeaders });
    assert.equal(app.status, 200);
    const appText = await app.text();
    assert.match(appText, /"gemini"/);
    assert.match(appText, /kanban\.theme/);
    assert.match(appText, /status-badge/);
    assert.match(appText, /task-context/);
    const styles = await fetch(`${dashboard.url}/styles.css`, { headers: sessionHeaders });
    assert.equal(styles.status, 200);
    const stylesText = await styles.text();
    assert.match(stylesText, /:root\[data-theme="light"\]/);
    assert.match(stylesText, /min-height: 40px/);
    assert.match(stylesText, /grid-auto-columns: calc\(100vw - 24px\)/);

    const orchestration = await request("/api/boards/default", {
      method: "PATCH",
      body: JSON.stringify({
        name: "Default Project",
        orchestration: {
          autoDecompose: true,
          autoDecomposePerTick: 2,
          autoPromoteChildren: false,
          plannerRuntime: "gemini",
          defaultProfile: "worker",
          profiles: [{ name: "worker", runtime: "gemini", description: "general work" }],
        },
      }),
    });
    assert.equal(orchestration.response.status, 200);
    assert.equal(orchestration.value.orchestration.autoDecompose, true);
    assert.equal(orchestration.value.orchestration.autoPromoteChildren, false);
    assert.equal(orchestration.value.orchestration.plannerRuntime, "gemini");
    assert.equal(orchestration.value.orchestration.profiles[0]?.runtime, "gemini");
    assert.equal(new BoardManager(dbPath).read("default").orchestration.profiles[0]?.name, "worker");

    const invalidSort = await request("/api/tasks?board=default&sort=drop-table");
    assert.equal(invalidSort.response.status, 400);
    const invalidJson = await fetch(`${dashboard.url}/api/tasks?board=default`, {
      method: "POST",
      headers,
      body: "{",
    });
    assert.equal(invalidJson.status, 400);
    const invalidRunning = await request("/api/tasks?board=default", {
      method: "POST",
      body: JSON.stringify({ title: "invalid running task", status: "running" }),
    });
    assert.equal(invalidRunning.response.status, 409);

    const created = await request("/api/tasks?board=default", {
      method: "POST",
      body: JSON.stringify({ title: "HTTP task", body: "exercise the API", status: "triage" }),
    });
    assert.equal(created.response.status, 201);
    const taskId = created.value.task.id as string;
    const specified = await request(`/api/tasks/${taskId}?board=default`, {
      method: "PATCH",
      body: JSON.stringify({ title: "Updated HTTP task", body: "Acceptance: API state is durable.", status: "todo" }),
    });
    assert.equal(specified.value.task.title, "Updated HTTP task");
    const commented = await request(`/api/tasks/${taskId}/comments?board=default`, {
      method: "POST",
      body: JSON.stringify({ author: "test", body: "durable comment" }),
    });
    assert.equal(commented.response.status, 201);

    const upload = await fetch(`${dashboard.url}/api/tasks/${taskId}/attachments?board=default&name=brief.txt`, {
      method: "POST",
      headers: { authorization: `Bearer ${token}`, "content-type": "text/plain" },
      body: "attachment body",
    });
    assert.equal(upload.status, 201);
    const attachment = await upload.json() as { id: string };
    const download = await fetch(
      `${dashboard.url}/api/attachments/${attachment.id}/download?board=default&taskId=${taskId}`,
      { headers: { authorization: `Bearer ${token}` } },
    );
    assert.equal(download.status, 200);
    assert.equal(await download.text(), "attachment body");

    const existingEvents = await request("/api/events?board=default&since=0");
    const cursor = existingEvents.value.at(-1)?.id ?? 0;
    const websocketUrl = dashboard.url.replace(/^http/, "ws") +
      `/api/events/ws?board=default&since=${cursor}&token=${encodeURIComponent(token)}`;
    socket = new WebSocket(websocketUrl);
    await new Promise<void>((resolveOpen, rejectOpen) => {
      socket!.once("open", resolveOpen);
      socket!.once("error", rejectOpen);
    });
    const nextEvents = new Promise<any>((resolveMessage, rejectMessage) => {
      const timer = setTimeout(() => rejectMessage(new Error("WebSocket event timeout")), 5_000);
      socket!.once("message", (data) => {
        clearTimeout(timer);
        resolveMessage(JSON.parse(data.toString()));
      });
    });
    const streamed = await request("/api/tasks?board=default", {
      method: "POST",
      body: JSON.stringify({ title: "streamed task" }),
    });
    const eventMessage = await nextEvents;
    assert.equal(eventMessage.type, "events");
    assert.ok(eventMessage.events.some((event: { taskId: string }) => event.taskId === streamed.value.task.id));

    const manager = new BoardManager(dbPath);
    const store = manager.openStore("default");
    const activeTask = store.createTask({ title: "active API run", assignee: "worker", runtime: "codex" });
    store.close();
    const claimed = await request(`/api/tasks/${activeTask.task.id}/claim?board=default`, {
      method: "POST",
      body: "{}",
    });
    assert.equal(claimed.response.status, 200);
    assert.equal(claimed.value.task.task.status, "running");
    assert.ok(claimed.value.task.task.workspace);
    const runId = claimed.value.run.id as string;
    const terminated = await request(`/api/runs/${runId}/terminate?board=default`, {
      method: "POST",
      body: JSON.stringify({ reason: "HTTP test termination" }),
    });
    assert.equal(terminated.response.status, 200);
    assert.equal(terminated.value.task.task.status, "ready");
    const run = await request(`/api/runs/${runId}?board=default`);
    assert.equal(run.value.run.status, "reclaimed");

    const bulk = await request("/api/tasks/bulk?board=default", {
      method: "POST",
      body: JSON.stringify({ ids: [taskId, "missing"], mutation: { priority: 7 } }),
    });
    assert.equal(bulk.value.ok.length, 1);
    assert.equal(bulk.value.errors.length, 1);
    const board = await request("/api/board?board=default&includeArchived=true");
    assert.ok(board.value.tasks.some((task: { id: string; priority: number }) => task.id === taskId && task.priority === 7));
    const taskCard = board.value.tasks.find((task: { id: string }) => task.id === taskId);
    assert.equal(taskCard.commentsCount, 1);
  } finally {
    socket?.close();
    if (socket && socket.readyState !== WebSocket.CLOSED) {
      await new Promise<void>((resolveClose) => socket!.once("close", () => resolveClose()));
    }
    await dashboard.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
