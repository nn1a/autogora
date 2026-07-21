import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import test from "node:test";

import { KanbanStore } from "../src/store.js";

test("dependency-gated tasks promote after a verified parent completion", () => {
  const store = new KanbanStore(":memory:");
  try {
    const parent = store.createTask({ title: "parent", assignee: "worker-a", runtime: "codex" });
    const child = store.createTask({
      title: "child",
      assignee: "worker-b",
      runtime: "claude",
      parents: [parent.task.id],
    });

    assert.equal(parent.task.status, "ready");
    assert.equal(child.task.status, "todo");
    assert.equal(store.claimTask({ taskId: child.task.id }), null);

    const claim = store.claimTask({ taskId: parent.task.id, workerId: "test-worker" });
    assert.ok(claim);
    assert.equal(store.claimTask({ taskId: parent.task.id }), null);
    assert.throws(
      () => store.heartbeat({ runId: claim.run.id, claimToken: "wrong" }),
      /Invalid claim token/,
    );

    store.heartbeat({ runId: claim.run.id, claimToken: claim.claimToken }, "tests running");
    store.completeRun(
      { runId: claim.run.id, claimToken: claim.claimToken },
      "parent complete",
      { verification: ["npm test"] },
    );

    assert.equal(store.getTask(parent.task.id).task.status, "done");
    assert.equal(store.getTask(child.task.id).task.status, "ready");
    assert.equal(store.getTask(parent.task.id).runs[0]?.metadata?.verification?.[0], "npm test");

    const childClaim = store.claimTask({ taskId: child.task.id });
    assert.ok(childClaim);
    store.addComment(child.task.id, "worker-b", "Need a product decision");
    store.blockRun({ runId: childClaim.run.id, claimToken: childClaim.claimToken }, "Choose option A or B");
    assert.equal(store.getTask(child.task.id).task.status, "blocked");
    assert.equal(store.unblockTask(child.task.id).task.status, "ready");
  } finally {
    store.close();
  }
});

test("dependency cycles are rejected", () => {
  const store = new KanbanStore(":memory:");
  try {
    const first = store.createTask({ title: "first" });
    const second = store.createTask({ title: "second", parents: [first.task.id] });
    assert.throws(() => store.linkTasks(second.task.id, first.task.id), /Dependency cycle rejected/);
  } finally {
    store.close();
  }
});

test("failed runs requeue until the retry budget is exhausted", () => {
  const store = new KanbanStore(":memory:");
  try {
    const task = store.createTask({
      title: "flaky",
      assignee: "worker",
      runtime: "codex",
      maxRetries: 2,
    });
    const first = store.claimTask({ taskId: task.task.id });
    assert.ok(first);
    const firstFailure = store.failRun(
      { runId: first.run.id, claimToken: first.claimToken },
      "exit 1",
    );
    assert.equal(firstFailure.task.status, "ready");
    assert.equal(firstFailure.task.failureCount, 1);

    const second = store.claimTask({ taskId: task.task.id });
    assert.ok(second);
    const exhausted = store.failRun(
      { runId: second.run.id, claimToken: second.claimToken },
      "exit 1 again",
    );
    assert.equal(exhausted.task.status, "blocked");
    assert.equal(exhausted.task.failureCount, 2);
    assert.deepEqual(
      exhausted.events.filter((event) => event.kind === "requeued" || event.kind === "gave_up").map((event) => event.kind),
      ["requeued", "gave_up"],
    );
  } finally {
    store.close();
  }
});

test("idempotent scheduled tasks preserve extended execution metadata", () => {
  const store = new KanbanStore(":memory:");
  try {
    const scheduledAt = new Date(Date.now() + 60_000).toISOString();
    const first = store.createTask({
      title: "nightly audit",
      tenant: "acme",
      idempotencyKey: "nightly-2026-07-22",
      assignee: "ops",
      runtime: "codex",
      scheduledAt,
      maxRuntimeSeconds: 1_800,
      skills: ["security-audit", "security-audit", " reporting "],
      goalMode: true,
      goalMaxTurns: 12,
      workspace: "worktree:/tmp/audit",
      branch: "kanban/audit",
    });
    const duplicate = store.createTask({
      title: "duplicate webhook delivery",
      board: "default",
      idempotencyKey: "nightly-2026-07-22",
    });

    assert.equal(duplicate.task.id, first.task.id);
    assert.equal(first.task.status, "scheduled");
    assert.equal(first.task.tenant, "acme");
    assert.equal(first.task.workspaceKind, "worktree");
    assert.equal(first.task.maxRuntimeSeconds, 1_800);
    assert.deepEqual(first.task.skills, ["security-audit", "reporting"]);
    assert.equal(first.task.goalMode, true);
    assert.equal(first.task.goalMaxTurns, 12);

    assert.equal(store.promoteDueTasks("default", new Date(Date.now() + 120_000).toISOString()), 1);
    assert.equal(store.getTask(first.task.id).task.status, "ready");
    assert.deepEqual(store.listTasks({ tenant: "acme", search: "audit" }).map((task) => task.id), [first.task.id]);
  } finally {
    store.close();
  }
});

test("legacy MVP databases migrate without losing tasks, runs, links, comments, or events", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-legacy-"));
  const dbPath = join(directory, "kanban.db");
  const legacy = new DatabaseSync(dbPath);
  legacy.exec(`
    PRAGMA foreign_keys = ON;
    CREATE TABLE tasks (
      id TEXT PRIMARY KEY, board TEXT NOT NULL DEFAULT 'default', title TEXT NOT NULL,
      body TEXT NOT NULL DEFAULT '', assignee TEXT, runtime TEXT NOT NULL,
      status TEXT NOT NULL CHECK (status IN ('triage','todo','ready','running','blocked','done','archived')),
      priority INTEGER NOT NULL DEFAULT 0, workspace TEXT, current_run_id TEXT,
      failure_count INTEGER NOT NULL DEFAULT 0, max_retries INTEGER NOT NULL DEFAULT 2,
      created_at TEXT NOT NULL, updated_at TEXT NOT NULL
    );
    CREATE TABLE task_links (
      parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
      child_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
      PRIMARY KEY(parent_id, child_id)
    );
    CREATE TABLE task_comments (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
      author TEXT NOT NULL, body TEXT NOT NULL, created_at TEXT NOT NULL
    );
    CREATE TABLE task_runs (
      id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
      worker_id TEXT NOT NULL, runtime TEXT NOT NULL, status TEXT NOT NULL,
      claim_token TEXT NOT NULL, claimed_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL,
      ended_at TEXT, summary TEXT, metadata_json TEXT, error TEXT
    );
    CREATE TABLE task_events (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
      run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
      kind TEXT NOT NULL, payload_json TEXT, created_at TEXT NOT NULL
    );
    INSERT INTO tasks VALUES (
      't_parent','default','legacy parent','body','worker','codex','done',1,'/tmp/work',NULL,0,2,
      '2026-07-20T00:00:00.000Z','2026-07-20T00:01:00.000Z'
    );
    INSERT INTO tasks VALUES (
      't_child','default','legacy child','','worker','claude','todo',0,NULL,NULL,0,2,
      '2026-07-20T00:02:00.000Z','2026-07-20T00:02:00.000Z'
    );
    INSERT INTO task_links VALUES ('t_parent','t_child');
    INSERT INTO task_comments(task_id,author,body,created_at) VALUES ('t_child','human','keep this','2026-07-20T00:03:00.000Z');
    INSERT INTO task_runs VALUES (
      'r_old','t_parent','worker','codex','completed','secret','2026-07-20T00:00:00.000Z',
      '2026-07-20T00:01:00.000Z','2026-07-20T00:01:00.000Z','done','{}',NULL
    );
    INSERT INTO task_events(task_id,run_id,kind,payload_json,created_at) VALUES (
      't_parent','r_old','completed','{}','2026-07-20T00:01:00.000Z'
    );
  `);
  legacy.close();

  try {
    const store = new KanbanStore(dbPath);
    try {
      const parent = store.getTask("t_parent");
      const child = store.getTask("t_child");
      assert.equal(parent.task.workspaceKind, "dir");
      assert.equal(parent.runs[0]?.id, "r_old");
      assert.equal(parent.events[0]?.runId, "r_old");
      assert.equal(child.parents[0]?.id, "t_parent");
      assert.equal(child.comments[0]?.body, "keep this");
      assert.equal(store.createTask({ title: "review lane", status: "review" }).task.status, "review");
    } finally {
      store.close();
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("runtime schema migration adds Cline without losing modern related records", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-runtime-migration-"));
  const dbPath = join(directory, "kanban.db");
  let taskId = "";
  {
    const store = new KanbanStore(dbPath);
    const task = store.createTask({ title: "preserve modern data", assignee: "worker", runtime: "codex" });
    taskId = task.task.id;
    store.addComment(taskId, "human", "keep the comment");
    store.attachUrl(taskId, "https://example.com/evidence", "evidence");
    store.subscribeTask({ taskId, platform: "webhook", chatId: "https://example.com/hook" });
    const claim = store.claimTask({ taskId });
    assert.ok(claim);
    store.completeRun({ runId: claim.run.id, claimToken: claim.claimToken }, "keep the completed run");
    store.close();
  }

  const oldSchema = new DatabaseSync(dbPath);
  oldSchema.enableDefensive(false);
  oldSchema.exec("PRAGMA writable_schema = ON");
  oldSchema.prepare(`
    UPDATE sqlite_master
    SET sql = replace(sql, '''claude'', ''codex'', ''cline'', ''manual''', '''claude'', ''codex'', ''manual''')
    WHERE type = 'table' AND name IN ('tasks', 'task_runs')
  `).run();
  oldSchema.exec("PRAGMA writable_schema = OFF; PRAGMA user_version = 4");
  oldSchema.close();

  try {
    const migrated = new KanbanStore(dbPath);
    try {
      const preserved = migrated.getTask(taskId);
      assert.equal(preserved.comments[0]?.body, "keep the comment");
      assert.equal(preserved.attachments[0]?.url, "https://example.com/evidence");
      assert.equal(preserved.runs[0]?.summary, "keep the completed run");
      assert.ok(preserved.events.some((event) => event.kind === "completed"));
      assert.equal(migrated.listNotificationSubscriptions(taskId).length, 1);

      const cline = migrated.createTask({ title: "new Cline task", assignee: "cline-worker", runtime: "cline" });
      const claim = migrated.claimTask({ taskId: cline.task.id });
      assert.ok(claim);
      assert.equal(claim.run.runtime, "cline");
    } finally {
      migrated.close();
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("typed dependency blocks wait in todo and repeated human blockers escalate to triage", () => {
  const store = new KanbanStore(":memory:");
  try {
    const parent = store.createTask({ title: "dependency", assignee: "parent", runtime: "codex" });
    const task = store.createTask({ title: "blocked work", assignee: "worker", runtime: "claude" });
    const dependencyRun = store.claimTask({ taskId: task.task.id });
    assert.ok(dependencyRun);
    const waiting = store.blockRun(
      { runId: dependencyRun.run.id, claimToken: dependencyRun.claimToken },
      "waiting for parent output",
      "dependency",
    );
    assert.equal(waiting.task.status, "todo");
    assert.equal(waiting.task.blockKind, "dependency");

    store.linkTasks(parent.task.id, task.task.id);
    const parentRun = store.claimTask({ taskId: parent.task.id });
    assert.ok(parentRun);
    store.completeRun({ runId: parentRun.run.id, claimToken: parentRun.claimToken }, "dependency complete");
    assert.equal(store.getTask(task.task.id).task.status, "ready");

    const first = store.claimTask({ taskId: task.task.id });
    assert.ok(first);
    const firstBlock = store.blockRun(
      { runId: first.run.id, claimToken: first.claimToken },
      "choose a data retention policy",
      "needs_input",
    );
    assert.equal(firstBlock.task.status, "blocked");
    assert.equal(firstBlock.task.blockRecurrences, 1);
    store.unblockTask(task.task.id);

    const second = store.claimTask({ taskId: task.task.id });
    assert.ok(second);
    const escalated = store.blockRun(
      { runId: second.run.id, claimToken: second.claimToken },
      "choose a data retention policy",
      "needs_input",
    );
    assert.equal(escalated.task.status, "triage");
    assert.equal(escalated.task.blockRecurrences, 2);
    assert.ok(escalated.events.some((event) => event.kind === "block_loop_detected"));
  } finally {
    store.close();
  }
});

test("human lifecycle actions synthesize handoff runs and administrative moves reclaim active runs", () => {
  const store = new KanbanStore(":memory:");
  try {
    const manual = store.createTask({ title: "human task" });
    const completed = store.completeTask(manual.task.id, {
      summary: "verified manually",
      result: "approved",
      metadata: { verification: ["human review"] },
    });
    assert.equal(completed.task.status, "done");
    assert.equal(completed.task.result, "approved");
    assert.equal(completed.runs[0]?.status, "completed");
    assert.equal(completed.runs[0]?.workerId, "human");

    const running = store.createTask({ title: "running", assignee: "worker", runtime: "codex" });
    const claim = store.claimTask({ taskId: running.task.id });
    assert.ok(claim);
    const archived = store.archiveTask(running.task.id);
    assert.equal(archived.task.status, "archived");
    assert.equal(archived.task.currentRunId, null);
    assert.equal(archived.runs[0]?.status, "reclaimed");

    const parent = store.createTask({ title: "unlink parent" });
    const child = store.createTask({ title: "unlink child", parents: [parent.task.id] });
    assert.equal(store.unlinkTasks(parent.task.id, child.task.id).parents.length, 0);
    assert.deepEqual(store.deleteTask(child.task.id), { id: child.task.id, deleted: true });
    assert.throws(() => store.getTask(child.task.id), /Task not found/);
  } finally {
    store.close();
  }
});

test("running status can only be entered through an atomic claim", () => {
  const store = new KanbanStore(":memory:");
  try {
    assert.throws(
      () => store.createTask({ title: "invalid running task", status: "running" }),
      /atomic claim/,
    );
    const task = store.createTask({ title: "claimable task", assignee: "worker", runtime: "codex" });
    assert.throws(() => store.updateTask(task.task.id, { status: "running" }), /atomic claim/);
    assert.ok(store.claimTask({ taskId: task.task.id }));
    assert.equal(store.getTask(task.task.id).task.status, "running");
  } finally {
    store.close();
  }
});
