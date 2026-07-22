#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const cliEntry = fileURLToPath(new URL("../../dist/cli.js", import.meta.url));
const taskId = process.env.KANBAN_TASK_ID;
const prompt = process.argv.at(-1) ?? "";

if (!process.argv.includes("--json") || process.argv.some((arg) => arg.includes("mcpServers"))) {
  throw new Error("Cline runner must use JSON mode without injected MCP configuration");
}
if (!prompt.includes("scoped TaskCircuit CLI bridge")) throw new Error("Cline runner prompt is missing the CLI bridge");

const shellQuote = (value) => `'${value.replaceAll("'", `'"'"'`)}'`;
const requestApproval = async (id, toolName, input) => {
  const directory = process.env.CLINE_TOOL_APPROVAL_DIR;
  const requestPath = join(directory, `fixture-session.request.${id}.json`);
  const decisionPath = join(directory, `fixture-session.decision.${id}.json`);
  await writeFile(requestPath, `${JSON.stringify({ toolCallId: id, toolName, input })}\n`, "utf8");
  const deadline = Date.now() + 5_000;
  while (Date.now() < deadline) {
    try {
      return JSON.parse(await readFile(decisionPath, "utf8"));
    } catch {
      await new Promise((resolveWait) => setTimeout(resolveWait, 25));
    }
  }
  throw new Error(`Timed out waiting for approval decision ${id}`);
};

const bridge = `${shellQuote(process.execPath)} ${shellQuote(cliEntry)}`;
const allowed = await requestApproval("allowed", "run_commands", {
  commands: [`${bridge} show "$KANBAN_TASK_ID"`],
});
if (allowed.approved !== true) throw new Error("Scoped TaskCircuit CLI command was not approved");
const denied = await requestApproval("denied", "run_commands", { commands: ["touch /tmp/not-allowed"] });
if (denied.approved !== false) throw new Error("Unrelated read-only shell command was not denied");

const run = (...args) => execFileSync(process.execPath, [cliEntry, ...args], {
  env: process.env,
  encoding: "utf8",
});

run("show", taskId);
run("heartbeat", taskId, "--note", "fake Cline worker running");
run("comment", taskId, "Cline communicated through the scoped CLI bridge.", "--author", "cline");
run(
  "complete",
  taskId,
  "--summary",
  "fake Cline worker completed through CLI",
  "--metadata",
  JSON.stringify({ verification: ["cline-cli-bridge-e2e"] }),
);

process.stdout.write(`${JSON.stringify({
  type: "run_result",
  finishReason: "completed",
  text: "Completed through the scoped TaskCircuit CLI bridge.",
})}\n`);
