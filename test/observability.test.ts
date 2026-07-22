import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";
import { garbageCollect } from "../src/maintenance.js";

test("worker context, stats, diagnostics, event cursors, and bulk results share one kernel", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-observe-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  try {
    const parent = store.createTask({ title: "research", assignee: "researcher", runtime: "codex" });
    const parentClaim = store.claimTask({ taskId: parent.task.id });
    assert.ok(parentClaim);
    store.completeRun(
      { runId: parentClaim.run.id, claimToken: parentClaim.claimToken },
      "research handoff",
      { sources: ["primary"] },
    );
    const child = store.createTask({
      title: "write report",
      body: "Use the verified research and produce a concise report.",
      assignee: "writer",
      runtime: "claude",
      tenant: "acme",
      parents: [parent.task.id],
    });
    store.addComment(child.task.id, "human", "Use the 2026 figures.");

    const context = store.buildWorkerContext(child.task.id);
    assert.match(context, /Relationship and execution order/);
    assert.match(context, /Prerequisite handoffs/);
    assert.match(context, /research handoff/);
    assert.match(context, /Use the 2026 figures/);

    const stats = store.getStats();
    assert.equal(stats.total, 2);
    assert.equal(stats.byStatus.done, 1);
    assert.equal(stats.byTenant.acme, 1);

    const stranded = store.createTask({ title: "stranded", status: "ready" });
    const lagging = store.createTask({ title: "lagging", assignee: "worker", runtime: "codex", status: "todo" });
    const archivedPrerequisite = store.createTask({ title: "abandoned prerequisite", assignee: "owner", runtime: "codex" });
    const terminallyBlocked = store.createTask({
      title: "blocked by archive",
      assignee: "worker",
      runtime: "codex",
      parents: [archivedPrerequisite.task.id],
    });
    store.archiveTask(archivedPrerequisite.task.id);
    const diagnostics = store.diagnose();
    assert.equal(diagnostics.healthy, false);
    assert.ok(diagnostics.issues.some((issue) => issue.kind === "stranded_in_ready" && issue.taskId === stranded.task.id));
    assert.ok(diagnostics.issues.some((issue) => issue.kind === "promotion_lag" && issue.taskId === lagging.task.id));
    assert.ok(diagnostics.issues.some((issue) =>
      issue.kind === "terminal_prerequisite" && issue.taskId === terminallyBlocked.task.id
    ));

    const firstPage = store.listEvents({ limit: 2 });
    assert.equal(firstPage.length, 2);
    const secondPage = store.listEvents({ sinceId: firstPage[1]!.id, limit: 100 });
    assert.ok(secondPage.every((event) => event.id > firstPage[1]!.id));

    const bulk = store.bulkMutate([child.task.id, "t_missing"], { assignee: "editor", priority: 9 });
    assert.equal(bulk.ok.length, 1);
    assert.equal(bulk.errors.length, 1);
    assert.equal(store.getTask(child.task.id).task.assignee, "editor");
    assert.equal(store.getTask(child.task.id).task.priority, 9);
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("garbage collection removes only expired logs, events, and known terminal scratch workspaces", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-gc-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  let taskId = "";
  try {
    const task = store.createTask({ title: "terminal scratch" });
    taskId = task.task.id;
    store.completeTask(taskId, { summary: "done" });
  } finally {
    store.close();
  }

  const old = new Date(Date.now() - 10 * 86_400_000);
  const workspace = join(manager.workspaceRoot("default"), taskId);
  mkdirSync(workspace, { recursive: true });
  writeFileSync(join(workspace, "temporary.txt"), "temporary", "utf8");
  utimesSync(workspace, old, old);
  const oldLog = join(manager.logsRoot("default"), "old.log");
  const newLog = join(manager.logsRoot("default"), "new.log");
  mkdirSync(manager.logsRoot("default"), { recursive: true });
  writeFileSync(oldLog, "old", "utf8");
  writeFileSync(newLog, "new", "utf8");
  utimesSync(oldLog, old, old);

  try {
    const collected = garbageCollect(manager, "default", {
      eventRetentionDays: 0,
      logRetentionDays: 7,
      workspaceRetentionDays: 7,
    });
    assert.ok(collected.eventsDeleted > 0);
    assert.deepEqual(collected.logsDeleted, [oldLog]);
    assert.deepEqual(collected.workspacesDeleted, [workspace]);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
