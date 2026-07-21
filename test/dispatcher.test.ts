import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";
import { buildRunnerCommand, runDispatcher } from "../src/dispatcher.js";
import { KanbanStore } from "../src/store.js";

for (const runtime of ["claude", "codex"] as const) {
  test(`builds a scoped ${runtime} runner without leaking the claim token into argv`, () => {
    const store = new KanbanStore(":memory:");
    try {
      const detail = store.createTask({
        title: `${runtime} task`,
        assignee: "worker",
        runtime,
        workspace: process.cwd(),
      });
      const claim = store.claimTask({ taskId: detail.task.id });
      assert.ok(claim);
      const command = buildRunnerCommand(claim, {
        dbPath: "/tmp/kanban-test.db",
        cliEntry: "/tmp/kanban-cli.js",
        allowWrites: false,
      });
      assert.equal(command.env.KANBAN_TASK_ID, detail.task.id);
      assert.equal(command.env.KANBAN_BOARD, "default");
      assert.equal(command.env.KANBAN_RUN_ID, claim.run.id);
      assert.equal(command.env.KANBAN_CLAIM_TOKEN, claim.claimToken);
      assert.equal(command.args.includes(claim.claimToken), false);
      assert.match(command.args.join(" "), /read-only|dontAsk/);
    } finally {
      store.close();
    }
  });
}

test("dispatcher claims, launches, and observes a terminal MCP lifecycle call", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-dispatch-"));
  const dbPath = join(directory, "kanban.db");
  const fakeAgent = resolve("test/fixtures/fake-agent.mjs");
  chmodSync(fakeAgent, 0o755);
  const manager = new BoardManager(dbPath);
  manager.create("project");
  let notificationCount = 0;
  const notificationServer = createServer((request, response) => {
    request.resume();
    request.on("end", () => {
      notificationCount += 1;
      response.writeHead(204).end();
    });
  });
  await new Promise<void>((resolveListen, rejectListen) => {
    notificationServer.once("error", rejectListen);
    notificationServer.listen(0, "127.0.0.1", resolveListen);
  });
  const notificationAddress = notificationServer.address();
  assert.ok(notificationAddress && typeof notificationAddress !== "string");
  const store = manager.openStore("project");
  const task = store.createTask({
    title: "dispatcher e2e",
    assignee: "fake",
    runtime: "codex",
    workspace: process.cwd(),
  });
  store.subscribeTask({
    taskId: task.task.id,
    platform: "webhook",
    chatId: `http://127.0.0.1:${notificationAddress.port}/kanban`,
  });
  store.close();

  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fakeAgent;
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      maxWorkers: 1,
      allowWrites: false,
    });
    const check = manager.openStore("project");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.summary, "fake worker completed");
      assert.equal(completed.runs[0]?.metadata?.verification?.[0], "dispatcher-e2e");
      assert.ok(completed.runs[0]?.pid);
      assert.ok(completed.runs[0]?.logPath);
      assert.equal(notificationCount, 1);
      assert.equal(check.listNotificationSubscriptions(task.task.id).length, 0);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    await new Promise<void>((resolveClose, rejectClose) =>
      notificationServer.close((error) => error ? rejectClose(error) : resolveClose()),
    );
    rmSync(directory, { recursive: true, force: true });
  }
});

test("dispatcher treats exit 75 as a retry-neutral rate limit", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-rate-limit-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/exit-75.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "rate limited worker",
    assignee: "worker",
    runtime: "codex",
    workspace: process.cwd(),
  });
  store.close();
  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fixture;
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      rateLimitCooldownSeconds: 0,
    });
    const check = manager.openStore("default");
    try {
      const detail = check.getTask(task.task.id);
      assert.equal(detail.task.status, "ready");
      assert.equal(detail.task.failureCount, 0);
      assert.equal(detail.runs[0]?.status, "rate_limited");
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("dispatcher terminates workers that exceed the task runtime limit", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-timeout-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/hanging-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "timed worker",
    assignee: "worker",
    runtime: "codex",
    workspace: process.cwd(),
    maxRuntimeSeconds: 1,
  });
  store.close();
  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fixture;
  try {
    await runDispatcher({ dbPath, cliEntry: resolve("dist/cli.js"), once: true });
    const check = manager.openStore("default");
    try {
      const detail = check.getTask(task.task.id);
      assert.equal(detail.task.status, "ready");
      assert.equal(detail.task.failureCount, 1);
      assert.equal(detail.runs[0]?.status, "timed_out");
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});
