#!/usr/bin/env node

import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport, getDefaultEnvironment } from "@modelcontextprotocol/sdk/client/stdio.js";

const marker = join(process.env.KANBAN_WORKSPACE, `.goal-turn-${process.env.KANBAN_TASK_ID}`);
const cliEntry = fileURLToPath(new URL("../../dist/cli.js", import.meta.url));
const firstTurn = !existsSync(marker);
const client = new Client({ name: "fake-goal-agent", version: "1.0.0" });
const transport = new StdioClientTransport({
  command: process.execPath,
  args: [cliEntry, "serve", "--db", process.env.KANBAN_DB],
  env: {
    ...getDefaultEnvironment(),
    KANBAN_TASK_ID: process.env.KANBAN_TASK_ID,
    KANBAN_BOARD: process.env.KANBAN_BOARD,
    KANBAN_RUN_ID: process.env.KANBAN_RUN_ID,
    KANBAN_CLAIM_TOKEN: process.env.KANBAN_CLAIM_TOKEN,
  },
});

try {
  await client.connect(transport);
  await client.callTool({ name: "kanban_show", arguments: {} });
  if (firstTurn) {
    writeFileSync(marker, "first turn complete", "utf8");
    await client.callTool({
      name: "kanban_comment",
      arguments: { body: "First goal turn produced partial progress." },
    });
    process.stdout.write(`${JSON.stringify({ type: "thread.started", thread_id: "11111111-1111-4111-8111-111111111111" })}\n`);
    process.stdout.write(`${JSON.stringify({ type: "item.completed", text: "Partial progress; one acceptance gap remains." })}\n`);
  } else {
    await client.callTool({
      name: "kanban_complete",
      arguments: { summary: "goal worker satisfied every acceptance criterion", metadata: { turns: 2 } },
    });
  }
} finally {
  await client.close();
}
