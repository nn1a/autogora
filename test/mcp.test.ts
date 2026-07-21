import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport, getDefaultEnvironment } from "@modelcontextprotocol/sdk/client/stdio.js";

function textPayload(result: Awaited<ReturnType<Client["callTool"]>>): unknown {
  const block = result.content[0];
  assert.ok(block && block.type === "text");
  return JSON.parse(block.text) as unknown;
}

test("Claude/Codex-compatible stdio MCP transport exposes the Kanban workflow", async () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-mcp-"));
  const dbPath = join(directory, "kanban.db");
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
    assert.ok(tools.tools.some((tool) => tool.name === "kanban_complete"));

    const created = textPayload(
      await client.callTool({
        name: "kanban_create",
        arguments: {
          title: "MCP smoke task",
          assignee: "reviewer",
          runtime: "codex",
          workspace: process.cwd(),
        },
      }),
    ) as { task: { id: string; status: string } };
    assert.equal(created.task.status, "ready");

    const listed = textPayload(
      await client.callTool({ name: "kanban_list", arguments: { status: "ready" } }),
    ) as { id: string }[];
    assert.equal(listed[0]?.id, created.task.id);

    const shown = textPayload(
      await client.callTool({ name: "kanban_show", arguments: { task_id: created.task.id } }),
    ) as { task: { title: string } };
    assert.equal(shown.task.title, "MCP smoke task");

    const claim = textPayload(
      await client.callTool({ name: "kanban_claim", arguments: { task_id: created.task.id } }),
    ) as { run: { id: string }; claimToken: string };
    worker = new Client({ name: "scoped-worker", version: "1.0.0" });
    await worker.connect(
      new StdioClientTransport({
        command: process.execPath,
        args: [resolve("dist/cli.js"), "serve", "--db", dbPath],
        env: {
          ...getDefaultEnvironment(),
          KANBAN_TASK_ID: created.task.id,
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
      await client.callTool({ name: "kanban_show", arguments: { task_id: created.task.id } }),
    ) as { task: { status: string }; runs: { status: string }[] };
    assert.equal(completed.task.status, "done");
    assert.equal(completed.runs[0]?.status, "completed");
  } finally {
    await worker?.close();
    await client.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
