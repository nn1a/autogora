import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";
import { buildRunnerCommand, runDispatcher } from "../src/dispatcher.js";
import { KanbanStore } from "../src/store.js";

for (const runtime of ["claude", "codex", "cline", "gemini"] as const) {
  test(`builds a scoped ${runtime} runner without leaking the claim token into argv`, () => {
    const directory = mkdtempSync(join(tmpdir(), "kanban-runner-command-"));
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
        clineApprovalDir: directory,
      });
      assert.equal(command.env.KANBAN_TASK_ID, detail.task.id);
      assert.equal(command.env.KANBAN_BOARD, "default");
      assert.equal(command.env.KANBAN_RUN_ID, claim.run.id);
      assert.equal(command.env.KANBAN_CLAIM_TOKEN, claim.claimToken);
      assert.equal(command.args.includes(claim.claimToken), false);
      if (runtime === "cline") {
        assert.equal(command.env.CLINE_TOOL_APPROVAL_MODE, "desktop");
        assert.match(command.args.join(" "), /--auto-approve false/);
        assert.equal(command.args.some((arg) => arg.includes("mcpServers")), false);
        assert.match(command.args.at(-1) ?? "", /scoped Kanban CLI bridge/);
      } else if (runtime === "gemini") {
        assert.match(command.args.join(" "), /--approval-mode default/);
        assert.match(command.args.join(" "), /--policy/);
        assert.match(command.policyFile?.content ?? "", /commandPrefix/);
        assert.match(command.policyFile?.content ?? "", /toolName = "mcp_\*"/);
        assert.equal(command.args.some((arg) => arg.includes("mcpServers")), false);
        assert.match(command.args.at(-1) ?? "", /scoped Kanban CLI bridge/);
      } else {
        assert.match(command.args.join(" "), /read-only|dontAsk/);
      }
    } finally {
      store.close();
      rmSync(directory, { recursive: true, force: true });
    }
  });
}

test("dispatcher runs a Cline worker through the scoped CLI bridge without MCP", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-cline-dispatch-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/fake-cline-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "Cline CLI bridge e2e",
    assignee: "cline-worker",
    runtime: "cline",
    workspace: directory,
  });
  store.close();
  const previous = process.env.KANBAN_CLINE_BIN;
  process.env.KANBAN_CLINE_BIN = fixture;
  try {
    await runDispatcher({ dbPath, cliEntry: resolve("dist/cli.js"), once: true, allowWrites: false });
    const check = manager.openStore("default");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.runtime, "cline");
      assert.equal(completed.runs[0]?.summary, "fake Cline worker completed through CLI");
      assert.equal(completed.runs[0]?.metadata?.verification?.[0], "cline-cli-bridge-e2e");
      assert.match(completed.comments[0]?.body ?? "", /scoped CLI bridge/);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CLINE_BIN;
    else process.env.KANBAN_CLINE_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("dispatcher runs a Gemini worker through the scoped CLI bridge", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-gemini-dispatch-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/fake-gemini-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "Gemini CLI bridge e2e",
    assignee: "gemini-worker",
    runtime: "gemini",
    workspace: directory,
  });
  store.close();
  const previous = process.env.KANBAN_GEMINI_BIN;
  process.env.KANBAN_GEMINI_BIN = fixture;
  try {
    await runDispatcher({ dbPath, cliEntry: resolve("dist/cli.js"), once: true, allowWrites: false });
    const check = manager.openStore("default");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.runtime, "gemini");
      assert.equal(completed.runs[0]?.summary, "fake Gemini worker completed through CLI");
      assert.equal(completed.runs[0]?.metadata?.verification?.[0], "gemini-cli-bridge-e2e");
      assert.match(completed.comments[0]?.body ?? "", /scoped CLI bridge/);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_GEMINI_BIN;
    else process.env.KANBAN_GEMINI_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

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

test("dispatcher can automatically specify triage cards through the decomposition planner", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-auto-decompose-"));
  const dbPath = join(directory, "kanban.db");
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({ title: "rough operational idea", status: "triage" });
  store.close();
  const plannerKinds: string[] = [];
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      autoDecompose: true,
      decompositionPlanner: async ({ kind }) => {
        plannerKinds.push(kind);
        return {
          fanout: false,
          rootTitle: "Audit the production backup process",
          rootBody: "Verify restoration from the latest backup. Acceptance: record timestamps and checksum evidence.",
          reason: "One specialist can execute this safely.",
          tasks: [],
          dependencies: [],
        };
      },
    });
    const check = manager.openStore("default");
    try {
      const specified = check.getTask(task.task.id);
      assert.equal(specified.task.status, "todo");
      assert.match(specified.task.body, /Acceptance/);
      assert.deepEqual(plannerKinds, ["decompose"]);
    } finally {
      check.close();
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("goal mode resumes one worker session until completion", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-goal-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/goal-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "finish a multi-turn goal",
    body: "Acceptance: the second turn records a verified completion.",
    assignee: "worker",
    runtime: "codex",
    workspace: directory,
    goalMode: true,
    goalMaxTurns: 3,
  });
  store.close();
  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fixture;
  const judgedTurns: number[] = [];
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      goalJudge: async ({ turn }) => {
        judgedTurns.push(turn);
        return { complete: false, reason: "one acceptance gap remains", nextPrompt: "Finish and verify the remaining gap." };
      },
    });
    const check = manager.openStore("default");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.status, "completed");
      assert.equal(completed.runs[0]?.metadata?.turns, 2);
      assert.deepEqual(judgedTurns, [1]);
      assert.equal(completed.events.filter((event) => event.kind === "goal_judged").length, 1);
      assert.equal(completed.events.filter((event) => event.kind === "spawned").length, 2);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CODEX_BIN;
    else process.env.KANBAN_CODEX_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Cline goal mode continues through fresh CLI turns when headless resume is unavailable", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-cline-goal-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/goal-cline-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "finish a Cline multi-turn goal",
    body: "Acceptance: the second fresh turn records completion.",
    assignee: "cline-worker",
    runtime: "cline",
    workspace: directory,
    goalMode: true,
    goalMaxTurns: 3,
  });
  store.close();
  const previous = process.env.KANBAN_CLINE_BIN;
  process.env.KANBAN_CLINE_BIN = fixture;
  const judgedTurns: number[] = [];
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      goalJudge: async ({ turn }) => {
        judgedTurns.push(turn);
        return { complete: false, reason: "one gap remains", nextPrompt: "Finish the remaining gap." };
      },
    });
    const check = manager.openStore("default");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.runtime, "cline");
      assert.deepEqual(judgedTurns, [1]);
      assert.equal(completed.events.filter((event) => event.kind === "spawned").length, 2);
      assert.match(completed.comments[0]?.body ?? "", /durable handoff/);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_CLINE_BIN;
    else process.env.KANBAN_CLINE_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Gemini goal mode resumes the stream-json session until completion", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-gemini-goal-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/goal-gemini-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "finish a Gemini multi-turn goal",
    body: "Acceptance: the resumed turn records completion.",
    assignee: "gemini-worker",
    runtime: "gemini",
    workspace: directory,
    goalMode: true,
    goalMaxTurns: 3,
  });
  store.close();
  const previous = process.env.KANBAN_GEMINI_BIN;
  process.env.KANBAN_GEMINI_BIN = fixture;
  const judgedTurns: number[] = [];
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      goalJudge: async ({ turn }) => {
        judgedTurns.push(turn);
        return { complete: false, reason: "one gap remains", nextPrompt: "Finish the remaining gap." };
      },
    });
    const check = manager.openStore("default");
    try {
      const completed = check.getTask(task.task.id);
      assert.equal(completed.task.status, "done");
      assert.equal(completed.runs[0]?.runtime, "gemini");
      assert.deepEqual(judgedTurns, [1]);
      assert.equal(completed.events.filter((event) => event.kind === "spawned").length, 2);
      assert.match(completed.comments[0]?.body ?? "", /durable handoff/);
    } finally {
      check.close();
    }
  } finally {
    if (previous === undefined) delete process.env.KANBAN_GEMINI_BIN;
    else process.env.KANBAN_GEMINI_BIN = previous;
    rmSync(directory, { recursive: true, force: true });
  }
});

test("goal mode blocks for review when its turn budget is exhausted", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-goal-budget-"));
  const dbPath = join(directory, "kanban.db");
  const fixture = resolve("test/fixtures/goal-agent.mjs");
  chmodSync(fixture, 0o755);
  const manager = new BoardManager(dbPath);
  const store = manager.openStore("default");
  const task = store.createTask({
    title: "bounded goal",
    body: "Acceptance: evidence is required.",
    assignee: "worker",
    runtime: "codex",
    workspace: directory,
    goalMode: true,
    goalMaxTurns: 1,
  });
  store.close();
  const previous = process.env.KANBAN_CODEX_BIN;
  process.env.KANBAN_CODEX_BIN = fixture;
  try {
    await runDispatcher({
      dbPath,
      cliEntry: resolve("dist/cli.js"),
      once: true,
      goalJudge: async () => ({ complete: false, reason: "verification is missing", nextPrompt: "verify" }),
    });
    const check = manager.openStore("default");
    try {
      const blocked = check.getTask(task.task.id);
      assert.equal(blocked.task.status, "blocked");
      assert.equal(blocked.task.blockKind, "needs_input");
      assert.match(blocked.task.blockReason ?? "", /budget exhausted/i);
      assert.equal(blocked.runs[0]?.status, "blocked");
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
