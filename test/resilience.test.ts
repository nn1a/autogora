import assert from "node:assert/strict";
import test from "node:test";

import { KanbanStore } from "../src/store.js";

test("rate limits requeue without consuming retries and cooldown through scheduled", () => {
  const store = new KanbanStore(":memory:");
  try {
    const task = store.createTask({ title: "rate limited", assignee: "worker", runtime: "codex" });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    const limited = store.failRun(
      { runId: claim.run.id, claimToken: claim.claimToken },
      "provider returned 429 rate limit",
      { outcome: "rate_limited", countFailure: false, cooldownSeconds: 60 },
    );
    assert.equal(limited.task.status, "scheduled");
    assert.equal(limited.task.failureCount, 0);
    assert.equal(limited.runs[0]?.status, "rate_limited");
    assert.equal(store.promoteDueTasks("default", new Date(Date.now() + 120_000).toISOString()), 1);
    assert.equal(store.getTask(task.task.id).task.status, "ready");
  } finally {
    store.close();
  }
});

test("claim concurrency caps apply board-wide and per assignee", () => {
  const store = new KanbanStore(":memory:");
  try {
    const first = store.createTask({ title: "first", assignee: "shared", runtime: "claude" });
    const second = store.createTask({ title: "second", assignee: "shared", runtime: "codex" });
    const third = store.createTask({ title: "third", assignee: "other", runtime: "codex" });
    assert.ok(store.claimTask({ taskId: first.task.id, maxInProgress: 2, maxInProgressPerAssignee: 1 }));
    assert.equal(
      store.claimTask({ taskId: second.task.id, maxInProgress: 2, maxInProgressPerAssignee: 1 }),
      null,
    );
    assert.ok(store.claimTask({ taskId: third.task.id, maxInProgress: 2, maxInProgressPerAssignee: 1 }));
    assert.equal(store.claimTask({ maxInProgress: 2 }), null);
  } finally {
    store.close();
  }
});

test("abandoned claims can defer for a live PID or recover with a classified outcome", () => {
  const store = new KanbanStore(":memory:");
  try {
    const task = store.createTask({ title: "abandoned", assignee: "worker", runtime: "codex", maxRetries: 2 });
    const claim = store.claimTask({ taskId: task.task.id, claimTtlSeconds: 1 });
    assert.ok(claim);
    const originalExpiry = claim.run.claimExpiresAt;
    store.recordSpawn({ runId: claim.run.id, claimToken: claim.claimToken }, 999_999, "/tmp/run.log");
    const deferred = store.deferReclaim(claim.run.id, 120);
    assert.ok(Date.parse(deferred.claimExpiresAt) > Date.parse(originalExpiry));
    assert.equal(store.listActiveRuns().length, 1);

    const recovered = store.recoverAbandonedRun(claim.run.id, "crashed", "worker PID disappeared");
    assert.equal(recovered.task.status, "ready");
    assert.equal(recovered.task.failureCount, 1);
    assert.equal(recovered.task.currentRunId, null);
    assert.equal(recovered.runs[0]?.status, "crashed");
    assert.equal(store.listActiveRuns().length, 0);
  } finally {
    store.close();
  }
});

test("recent successful work guards an administratively re-opened task from immediate respawn", () => {
  const store = new KanbanStore(":memory:");
  try {
    const task = store.createTask({ title: "guarded", assignee: "worker", runtime: "claude" });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    store.completeRun({ runId: claim.run.id, claimToken: claim.claimToken }, "completed recently");
    store.updateTask(task.task.id, { status: "ready" });
    assert.equal(store.claimTask({ taskId: task.task.id }), null);
    assert.ok(store.getTask(task.task.id).events.some((event) =>
      event.kind === "respawn_guarded" && event.payload?.reason === "recent_success"
    ));
  } finally {
    store.close();
  }
});
