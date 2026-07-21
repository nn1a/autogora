import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport, getDefaultEnvironment } from "@modelcontextprotocol/sdk/client/stdio.js";

import { BoardManager } from "../src/boards.js";

function textPayload(result: Awaited<ReturnType<Client["callTool"]>>): unknown {
  const block = result.content[0];
  assert.ok(block && block.type === "text");
  return JSON.parse(block.text) as unknown;
}

test("stdio MCP administration can route work across Claude, Codex, and Cline", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-mcp-"));
  const dbPath = join(directory, "kanban.db");
  new BoardManager(dbPath).create("project");
  const client = new Client({ name: "kanban-test-client", version: "1.0.0" });
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [resolve("dist/cli.js"), "serve", "--db", dbPath],
  });
  let worker: Client | undefined;
  try {
    await client.connect(transport);
    const tools = await client.listTools();
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_create"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_boards_create"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_complete"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_unlink"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_schedule"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_archive"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_delete"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_context"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_diagnostics"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_events"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_bulk"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_gc"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_notify_subscribe"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_notify_list"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_notify_unsubscribe"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_notify_deliver"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_specify"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_decompose"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_profile_describe_auto"));
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_swarm"));
    const boards = textPayload(
      await client.callTool({ name: "kanban_boards_list", arguments: {} }),
    ) as { slug: string }[];
    assert.ok(boards.some((board) => board.slug === "project"));
    const secondBoard = textPayload(
      await client.callTool({
        name: "kanban_boards_create",
        arguments: { slug: "project-two", name: "Project Two" },
      }),
    ) as { slug: string; name: string };
    assert.equal(secondBoard.slug, "project-two");
    assert.equal(secondBoard.name, "Project Two");

    const created = textPayload(
      await client.callTool({
        name: "kanban_create",
        arguments: {
          title: "MCP smoke task",
          board: "project",
          tenant: "engineering",
          idempotency_key: "mcp-smoke-once",
          assignee: "reviewer",
          runtime: "codex",
          workspace: process.cwd(),
          skills: ["github-code-review"],
          goal_mode: true,
          goal_max_turns: 7,
        },
      }),
    ) as { task: { id: string; status: string } };
    assert.equal(created.task.status, "ready");
    const briefPath = join(directory, "brief.txt");
    writeFileSync(briefPath, "MCP attachment", "utf8");
    const attached = textPayload(
      await client.callTool({
        name: "kanban_attach",
        arguments: { board: "project", task_id: created.task.id, path: briefPath },
      }),
    ) as { kind: string; name: string };
    assert.equal(attached.kind, "file");
    assert.equal(attached.name, "brief.txt");

    const listed = textPayload(
      await client.callTool({ name: "kanban_list", arguments: { board: "project", status: "ready" } }),
    ) as { id: string }[];
    assert.equal(listed[0]?.id, created.task.id);

    const shown = textPayload(
      await client.callTool({ name: "kanban_show", arguments: { board: "project", task_id: created.task.id } }),
    ) as {
      task: { title: string; tenant: string; skills: string[]; goalMode: boolean; goalMaxTurns: number };
      attachments: { name: string }[];
    };
    assert.equal(shown.task.title, "MCP smoke task");
    assert.equal(shown.task.tenant, "engineering");
    assert.deepEqual(shown.task.skills, ["github-code-review"]);
    assert.equal(shown.task.goalMode, true);
    assert.equal(shown.task.goalMaxTurns, 7);
    assert.equal(shown.attachments[0]?.name, "brief.txt");

    const context = textPayload(
      await client.callTool({ name: "kanban_context", arguments: { board: "project", task_id: created.task.id } }),
    );
    assert.equal(typeof context, "string");
    assert.match(context as string, /MCP smoke task/);
    const stats = textPayload(
      await client.callTool({ name: "kanban_stats", arguments: { board: "project" } }),
    ) as { total: number; byRuntime: Record<string, number> };
    assert.equal(stats.total, 1);
    assert.equal(stats.byRuntime.manual, 0);
    const events = textPayload(
      await client.callTool({ name: "kanban_events", arguments: { board: "project", task_id: created.task.id } }),
    ) as { taskId: string }[];
    assert.ok(events.length > 0);
    assert.ok(events.every((event) => event.taskId === created.task.id));

    const claim = textPayload(
      await client.callTool({ name: "kanban_claim", arguments: { board: "project", task_id: created.task.id } }),
    ) as { run: { id: string }; claimToken: string };
    worker = new Client({ name: "scoped-worker", version: "1.0.0" });
    await worker.connect(
      new StdioClientTransport({
        command: process.execPath,
        args: [resolve("dist/cli.js"), "serve", "--db", dbPath],
        env: {
          ...getDefaultEnvironment(),
          KANBAN_TASK_ID: created.task.id,
          KANBAN_BOARD: "project",
          KANBAN_RUN_ID: claim.run.id,
          KANBAN_CLAIM_TOKEN: claim.claimToken,
        },
      }),
    );
    const scoped = textPayload(
      await worker.callTool({ name: "kanban_show", arguments: {} }),
    ) as { task: { id: string; status: string } };
    assert.equal(scoped.task.id, created.task.id);
    assert.equal(scoped.task.status, "running");

    const forbidden = await worker.callTool({ name: "kanban_list", arguments: {} });
    assert.equal(forbidden.isError, true);

    await worker.callTool({
      name: "kanban_complete",
      arguments: { summary: "Scoped MCP worker completed the smoke task", metadata: { verification: ["mcp"] } },
    });
    const completed = textPayload(
      await client.callTool({ name: "kanban_show", arguments: { board: "project", task_id: created.task.id } }),
    ) as { task: { status: string }; runs: { status: string }[] };
    assert.equal(completed.task.status, "done");
    assert.equal(completed.runs[0]?.status, "completed");

    const parked = textPayload(
      await client.callTool({ name: "kanban_create", arguments: { title: "parked admin task" } }),
    ) as { task: { id: string } };
    const subscribed = textPayload(
      await client.callTool({
        name: "kanban_notify_subscribe",
        arguments: { task_id: parked.task.id, platform: "test", chat_id: "chat-1", secret: "not-returned" },
      }),
    ) as { id: string; hasSecret: boolean; secret?: string };
    assert.equal(subscribed.hasSecret, true);
    assert.equal(subscribed.secret, undefined);
    const subscriptions = textPayload(
      await client.callTool({ name: "kanban_notify_list", arguments: { task_id: parked.task.id } }),
    ) as { id: string }[];
    assert.equal(subscriptions[0]?.id, subscribed.id);
    const unsubscribed = textPayload(
      await client.callTool({
        name: "kanban_notify_unsubscribe",
        arguments: { task_id: parked.task.id, platform: "test", chat_id: "chat-1" },
      }),
    ) as { unsubscribed: boolean };
    assert.equal(unsubscribed.unsubscribed, true);
    const triage = textPayload(
      await client.callTool({ name: "kanban_create", arguments: { title: "rough MCP idea", status: "triage" } }),
    ) as { task: { id: string } };
    const specified = textPayload(
      await client.callTool({
        name: "kanban_specify",
        arguments: {
          task_id: triage.task.id,
          title: "Specified MCP task",
          body: "Deliver the MCP task. Acceptance: the deterministic tool call succeeds.",
        },
      }),
    ) as { task: { status: string } };
    assert.equal(specified.task.status, "todo");
    const swarm = textPayload(
      await client.callTool({
        name: "kanban_swarm",
        arguments: {
          goal: "Exercise MCP swarm topology",
          workers: [{ name: "researcher", runtime: "cline" }],
          verifier: { name: "reviewer", runtime: "claude" },
          synthesizer: { name: "writer", runtime: "claude" },
        },
      }),
    ) as { root: { task: { status: string } }; workerIds: string[] };
    assert.equal(swarm.root.task.status, "done");
    assert.equal(swarm.workerIds.length, 1);
    const clineWorker = textPayload(
      await client.callTool({ name: "kanban_show", arguments: { task_id: swarm.workerIds[0] } }),
    ) as { task: { runtime: string } };
    assert.equal(clineWorker.task.runtime, "cline");
    const scheduled = textPayload(
      await client.callTool({
        name: "kanban_schedule",
        arguments: { task_id: parked.task.id, reason: "wait for maintenance window" },
      }),
    ) as { task: { status: string } };
    assert.equal(scheduled.task.status, "scheduled");
    const promoted = textPayload(
      await client.callTool({ name: "kanban_promote", arguments: { task_id: parked.task.id } }),
    ) as { task: { status: string } };
    assert.equal(promoted.task.status, "todo");
    await client.callTool({ name: "kanban_archive", arguments: { task_id: parked.task.id } });
    const deleted = textPayload(
      await client.callTool({ name: "kanban_delete", arguments: { task_id: parked.task.id } }),
    ) as { id: string; deleted: boolean };
    assert.deepEqual(deleted, { id: parked.task.id, deleted: true });
  } finally {
    await worker?.close();
    await client.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
