#!/usr/bin/env node

import { resolve } from "node:path";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport, getDefaultEnvironment } from "@modelcontextprotocol/sdk/client/stdio.js";

const client = new Client({ name: "fake-kanban-agent", version: "1.0.0" });
const transport = new StdioClientTransport({
  command: process.execPath,
  args: [resolve("dist/cli.js"), "serve", "--db", process.env.KANBAN_DB],
  env: {
    ...getDefaultEnvironment(),
    KANBAN_TASK_ID: process.env.KANBAN_TASK_ID,
    KANBAN_RUN_ID: process.env.KANBAN_RUN_ID,
    KANBAN_CLAIM_TOKEN: process.env.KANBAN_CLAIM_TOKEN,
  },
});

try {
  await client.connect(transport);
  await client.callTool({ name: "kanban_show", arguments: {} });
  await client.callTool({ name: "kanban_heartbeat", arguments: { note: "fake worker running" } });
  await client.callTool({
    name: "kanban_complete",
    arguments: { summary: "fake worker completed", metadata: { verification: ["dispatcher-e2e"] } },
  });
} finally {
  await client.close();
}
