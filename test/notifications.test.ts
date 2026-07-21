import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer, type IncomingHttpHeaders } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";
import { deliverNotifications, type NotificationPayload } from "../src/notifications.js";

interface ReceivedWebhook {
  body: string;
  headers: IncomingHttpHeaders;
  payload: NotificationPayload;
}

test("webhook notifications deliver terminal events and auto-remove completed subscriptions", async () => {
  const received: ReceivedWebhook[] = [];
  const server = createServer((request, response) => {
    const chunks: Buffer[] = [];
    request.on("data", (chunk: Buffer) => chunks.push(chunk));
    request.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8");
      received.push({ body, headers: request.headers, payload: JSON.parse(body) as NotificationPayload });
      response.writeHead(204).end();
    });
  });
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen(0, "127.0.0.1", resolveListen);
  });
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  const target = `http://127.0.0.1:${address.port}/kanban`;
  const directory = mkdtempSync(join(tmpdir(), "kanban-notify-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  try {
    const completedTask = store.createTask({ title: "notify on completion", assignee: "writer" });
    const subscription = store.subscribeTask({
      taskId: completedTask.task.id,
      platform: "webhook",
      chatId: target,
      threadId: "release",
      userId: "owner",
      secret: "shared-secret",
    });
    assert.equal(subscription.hasSecret, true);
    assert.equal(Object.hasOwn(subscription, "secret"), false);
    store.completeTask(completedTask.task.id, { summary: "published the release notes", result: "release shipped" });

    const completed = await deliverNotifications(store);
    assert.equal(completed.length, 1);
    assert.equal(completed[0]?.delivered, true);
    assert.equal(completed[0]?.eventKind, "completed");
    assert.equal(store.listNotificationSubscriptions(completedTask.task.id).length, 0);
    assert.equal(received[0]?.payload.task.id, completedTask.task.id);
    assert.match(received[0]?.payload.message ?? "", /published the release notes/);
    assert.equal(received[0]?.headers["x-kanban-event"], "completed");
    const signature = `sha256=${createHmac("sha256", "shared-secret").update(received[0]!.body).digest("hex")}`;
    assert.equal(received[0]?.headers["x-kanban-signature"], signature);

    const blockedTask = store.createTask({ title: "notify across retry", assignee: "reviewer" });
    store.subscribeTask({ taskId: blockedTask.task.id, platform: "webhook", chatId: target });
    store.blockTask(blockedTask.task.id, { reason: "approval required", kind: "needs_input" });
    const blocked = await deliverNotifications(store);
    assert.equal(blocked[0]?.eventKind, "blocked");
    assert.equal(store.listNotificationSubscriptions(blockedTask.task.id).length, 1);
    store.unblockTask(blockedTask.task.id);
    store.completeTask(blockedTask.task.id, { summary: "approval received" });
    const unblockedThenCompleted = await deliverNotifications(store);
    assert.equal(unblockedThenCompleted[0]?.eventKind, "completed");
    assert.equal(store.listNotificationSubscriptions(blockedTask.task.id).length, 0);
    assert.equal(received.length, 3);
  } finally {
    store.close();
    await new Promise<void>((resolveClose, rejectClose) => server.close((error) => error ? rejectClose(error) : resolveClose()));
    rmSync(directory, { recursive: true, force: true });
  }
});

test("notification delivery failures remain pending without exposing secrets", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-notify-retry-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  try {
    const task = store.createTask({ title: "retry notification" });
    store.subscribeTask({ taskId: task.task.id, platform: "test", chatId: "destination", secret: "hidden" });
    store.completeTask(task.task.id, { summary: "done" });
    const results = await deliverNotifications(store, {
      adapters: { test: async () => { throw new Error("temporary adapter failure"); } },
    });
    assert.equal(results[0]?.delivered, false);
    assert.match(results[0]?.error ?? "", /temporary adapter failure/);
    const subscriptions = store.listNotificationSubscriptions(task.task.id);
    assert.equal(subscriptions.length, 1);
    assert.equal(subscriptions[0]?.hasSecret, true);
    assert.equal(Object.hasOwn(subscriptions[0] ?? {}, "secret"), false);
    assert.deepEqual(store.claimNotificationDeliveries(), []);
    store.archiveTask(task.task.id);
    assert.deepEqual(store.listNotificationSubscriptions(task.task.id), []);
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
