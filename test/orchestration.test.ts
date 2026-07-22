import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { createCliPlanner, decomposeTriageTask, describeProfileRoute, specifyTriageTask } from "../src/orchestration.js";
import { KanbanStore } from "../src/store.js";

test("triage specification and decomposition use durable atomic task graphs", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-orchestrate-"));
  const store = new KanbanStore(join(directory, "kanban.db"));
  try {
    const rough = store.createTask({ title: "ship it", status: "triage", assignee: "writer", runtime: "codex" });
    const specified = await specifyTriageTask(store, rough.task.id, {
      specification: {
        title: "Publish the release notes",
        body: "Deliver release notes with cited changes. Acceptance: links resolve and the release checklist passes.",
      },
      author: "human",
    });
    assert.equal(specified.task.status, "todo");
    assert.match(specified.task.body, /Acceptance/);
    assert.ok(specified.events.some((event) => event.kind === "specified"));

    const root = store.createTask({ title: "research and report", body: "rough idea", status: "triage" });
    const result = await decomposeTriageTask(store, root.task.id, {
      profiles: [
        { name: "researcher", runtime: "codex", description: "finds primary sources" },
        { name: "writer", runtime: "claude", description: "synthesizes reports" },
      ],
      defaultProfile: { name: "fallback", runtime: "codex" },
      orchestratorProfile: { name: "orchestrator", runtime: "claude" },
      plan: {
        fanout: true,
        rootTitle: "Coordinate the verified market report",
        rootBody: "Judge the final report after all graph leaves finish.",
        reason: "Independent research can run in parallel.",
        tasks: [
          { key: "na", title: "Research North America", body: "Find primary sources.", assignee: "researcher", runtime: "codex", priority: 2, skills: [] },
          { key: "eu", title: "Research Europe", body: "Find primary sources.", assignee: "unknown", runtime: "claude", priority: 2, skills: [] },
          { key: "report", title: "Write verified report", body: "Synthesize both handoffs.", assignee: "writer", runtime: "claude", priority: 3, skills: ["editorial"] },
        ],
        dependencies: [
          { parent: "na", child: "report" },
          { parent: "eu", child: "report" },
        ],
      },
    });
    assert.equal(result.fanout, true);
    assert.ok(result.graph);
    const na = store.getTask(result.graph.tasksByKey.na!);
    const eu = store.getTask(result.graph.tasksByKey.eu!);
    const report = store.getTask(result.graph.tasksByKey.report!);
    assert.equal(na.task.status, "ready");
    assert.equal(eu.task.assignee, "fallback");
    assert.equal(eu.task.runtime, "codex");
    assert.equal(report.task.status, "todo");
    assert.deepEqual(new Set(report.parents.map((task) => task.id)), new Set([na.task.id, eu.task.id]));
    assert.deepEqual(result.task.parents.map((task) => task.id), [report.task.id]);
    assert.equal(result.task.task.assignee, "orchestrator");
    assert.deepEqual(new Set(result.task.subtasks.map((task) => task.id)), new Set([na.task.id, eu.task.id, report.task.id]));
    assert.equal(na.parentTask?.id, root.task.id);
    assert.equal(eu.parentTask?.id, root.task.id);
    assert.equal(report.parentTask?.id, root.task.id);
    assert.equal(result.graph.relationshipGraph.rootTaskId, root.task.id);
    assert.equal(result.graph.relationshipGraph.totalPhases, 3);
    assert.equal(result.graph.relationshipGraph.nodes.find((node) => node.task.id === report.task.id)?.phase, 1);
    assert.equal(result.graph.relationshipGraph.nodes.find((node) => node.task.id === root.task.id)?.phase, 2);

    store.completeTask(na.task.id, { summary: "NA complete" });
    store.completeTask(eu.task.id, { summary: "EU complete" });
    assert.equal(store.getTask(report.task.id).task.status, "ready");
    store.completeTask(report.task.id, { summary: "report complete" });
    assert.equal(store.getTask(root.task.id).task.status, "ready");

    const cyclicRoot = store.createTask({ title: "bad graph", status: "triage" });
    const taskCount = store.getStats().total;
    await assert.rejects(
      decomposeTriageTask(store, cyclicRoot.task.id, {
        profiles: [{ name: "worker", runtime: "codex" }],
        defaultProfile: { name: "worker", runtime: "codex" },
        plan: {
          fanout: true,
          rootTitle: "Bad graph",
          rootBody: "Must remain unchanged on cycle failure.",
          reason: "test",
          tasks: [
            { key: "a", title: "A", body: "A", assignee: "worker", runtime: "codex", priority: 0, skills: [] },
            { key: "b", title: "B", body: "B", assignee: "worker", runtime: "codex", priority: 0, skills: [] },
          ],
          dependencies: [{ parent: "a", child: "b" }, { parent: "b", child: "a" }],
        },
      }),
      /cycle/i,
    );
    assert.equal(store.getStats().total, taskCount);
    assert.equal(store.getTask(cyclicRoot.task.id).task.status, "triage");
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("swarm topology creates a completed blackboard, parallel workers, verifier, and synthesizer", () => {
  const store = new KanbanStore(":memory:");
  try {
    const swarm = store.createSwarm({
      goal: "Design a multi-region failover plan",
      workers: [
        { assignee: "researcher", runtime: "codex" },
        { assignee: "architect", runtime: "claude" },
        { assignee: "sre", runtime: "codex" },
      ],
      verifier: { assignee: "reviewer", runtime: "claude" },
      synthesizer: { assignee: "writer", runtime: "claude" },
      blackboard: { regions: ["us-east", "eu-west"] },
    });
    assert.equal(swarm.root.task.status, "done");
    assert.match(swarm.root.comments[0]?.body ?? "", /kanban_swarm_blackboard/);
    assert.equal(swarm.workerIds.length, 3);
    for (const workerId of swarm.workerIds) {
      const worker = store.getTask(workerId);
      assert.equal(worker.task.status, "ready");
      assert.deepEqual(worker.parents.map((task) => task.id), [swarm.root.task.id]);
      assert.equal(worker.parentTask?.id, swarm.root.task.id);
    }
    const verifier = store.getTask(swarm.verifierId);
    assert.equal(verifier.task.status, "todo");
    assert.deepEqual(new Set(verifier.parents.map((task) => task.id)), new Set(swarm.workerIds));
    assert.equal(verifier.parentTask?.id, swarm.root.task.id);
    const synthesizer = store.getTask(swarm.synthesizerId);
    assert.equal(synthesizer.task.status, "todo");
    assert.deepEqual(synthesizer.parents.map((task) => task.id), [swarm.verifierId]);
    assert.equal(synthesizer.parentTask?.id, swarm.root.task.id);
    assert.equal(store.getTask(swarm.root.task.id).subtasks.length, 5);
  } finally {
    store.close();
  }
});

test("decomposition can leave unblocked children in todo for manual review", async () => {
  const store = new KanbanStore(":memory:");
  try {
    const root = store.createTask({ title: "review graph first", status: "triage" });
    const result = await decomposeTriageTask(store, root.task.id, {
      profiles: [{ name: "worker", runtime: "codex" }],
      defaultProfile: { name: "worker", runtime: "codex" },
      autoPromoteChildren: false,
      plan: {
        fanout: true,
        rootTitle: "Review child graph",
        rootBody: "Promote children only after a manual routing review.",
        reason: "manual review requested",
        tasks: [
          { key: "one", title: "First child", body: "First deliverable", assignee: "worker", runtime: "codex", priority: 0, skills: [] },
          { key: "two", title: "Second child", body: "Second deliverable", assignee: "worker", runtime: "codex", priority: 0, skills: [] },
        ],
        dependencies: [],
      },
    });
    assert.ok(result.graph);
    assert.deepEqual(result.graph.childIds.map((id) => store.getTask(id).task.status), ["todo", "todo"]);
  } finally {
    store.close();
  }
});

test("Codex auxiliary planner uses a strict output schema and parsed last message", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-cli-planner-"));
  const fixture = resolve("test/fixtures/planner-agent.mjs");
  chmodSync(fixture, 0o755);
  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fixture;
  const store = new KanbanStore(join(directory, "kanban.db"));
  try {
    const task = store.createTask({ title: "rough planner input", status: "triage" });
    const result = await specifyTriageTask(store, task.task.id, {
      planner: createCliPlanner({ runtime: "codex", cwd: directory, timeoutMs: 5_000 }),
    });
    assert.equal(result.task.status, "todo");
    assert.equal(result.task.title, "Planner-generated task specification");
    assert.match(result.task.body, /verification evidence/);
  } finally {
    store.close();
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Cline auxiliary planner extracts and validates the final NDJSON response", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-cline-planner-"));
  const fixture = resolve("test/fixtures/planner-cline-agent.mjs");
  chmodSync(fixture, 0o755);
  const previous = process.env.KANBAN_CLINE_BIN;
  process.env.KANBAN_CLINE_BIN = fixture;
  const store = new KanbanStore(join(directory, "kanban.db"));
  try {
    const task = store.createTask({ title: "rough Cline planner input", status: "triage" });
    const result = await specifyTriageTask(store, task.task.id, {
      planner: createCliPlanner({ runtime: "cline", cwd: directory, timeoutMs: 5_000 }),
    });
    assert.equal(result.task.status, "todo");
    assert.equal(result.task.title, "Cline-generated task specification");
    assert.match(result.task.body, /CLI verification evidence/);
  } finally {
    store.close();
    if (previous === undefined) delete process.env.KANBAN_CLINE_BIN;
    else process.env.KANBAN_CLINE_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Gemini auxiliary planner unwraps a deny-all headless JSON response", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-gemini-planner-"));
  const fixture = resolve("test/fixtures/planner-gemini-agent.mjs");
  chmodSync(fixture, 0o755);
  const previous = process.env.KANBAN_GEMINI_BIN;
  process.env.KANBAN_GEMINI_BIN = fixture;
  const store = new KanbanStore(join(directory, "kanban.db"));
  try {
    const task = store.createTask({ title: "rough Gemini planner input", status: "triage" });
    const result = await specifyTriageTask(store, task.task.id, {
      planner: createCliPlanner({ runtime: "gemini", cwd: directory, timeoutMs: 5_000 }),
    });
    assert.equal(result.task.status, "todo");
    assert.equal(result.task.title, "Gemini-generated task specification");
    assert.match(result.task.body, /Gemini CLI verification evidence/);
  } finally {
    store.close();
    if (previous === undefined) delete process.env.KANBAN_GEMINI_BIN;
    else process.env.KANBAN_GEMINI_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("profile descriptions are generated through a constrained structured planner", async () => {
  let prompt = "";
  const described = await describeProfileRoute(
    { name: "security-reviewer", runtime: "codex" },
    [{ title: "Audit auth flow", body: "Review token validation and threat boundaries.", skills: ["security-audit"] }],
    async (request) => {
      prompt = request.prompt;
      assert.equal(request.kind, "profile_describe");
      return { description: "Reviews authentication code, threat boundaries, and security verification evidence." };
    },
  );
  assert.match(prompt, /Audit auth flow/);
  assert.match(described.description ?? "", /authentication code/);
});
