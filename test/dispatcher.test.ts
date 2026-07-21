import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

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
  const store = new KanbanStore(dbPath);
  const task = store.createTask({
    title: "dispatcher e2e",
    assignee: "fake",
    runtime: "codex",
    workspace: process.cwd(),
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
    const check = new KanbanStore(dbPath);
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.summary, "fake worker completed");
      assert.equal(completed.runs[0]?.metadata?.verification?.[0], "dispatcher-e2e");
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});
