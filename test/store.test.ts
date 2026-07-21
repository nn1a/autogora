import assert from "node:assert/strict";
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
