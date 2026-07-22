#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const cliEntry = fileURLToPath(new URL("../../dist/cli.js", import.meta.url));
const taskId = process.env.KANBAN_TASK_ID;
const promptIndex = process.argv.indexOf("-p");
const prompt = promptIndex >= 0 ? process.argv[promptIndex + 1] ?? "" : "";
const policyIndex = process.argv.indexOf("--policy");
const policy = policyIndex >= 0 ? readFileSync(process.argv[policyIndex + 1], "utf8") : "";

if (process.argv[process.argv.indexOf("--output-format") + 1] !== "stream-json") {
  throw new Error("Gemini worker must use stream-json output");
}
if (process.argv[process.argv.indexOf("--approval-mode") + 1] !== "default") {
  throw new Error("Read-only Gemini worker must use default approval mode");
}
if (!process.argv.includes("--skip-trust") || !process.argv.includes("none")) {
  throw new Error("Gemini worker must isolate extensions and trust the dispatcher workspace");
}
if (!policy.includes("commandPrefix") || !policy.includes('toolName = "mcp_*"')) {
  throw new Error("Gemini worker must narrowly allow the bridge and deny MCP tools");
}
if (!prompt.includes("scoped TaskCircuit CLI bridge")) throw new Error("Gemini worker prompt is missing the CLI bridge");

const run = (...args) => execFileSync(process.execPath, [cliEntry, ...args], {
  env: process.env,
  encoding: "utf8",
});

run("show", taskId);
run("heartbeat", taskId, "--note", "fake Gemini worker running");
run("comment", taskId, "Gemini communicated through the scoped CLI bridge.", "--author", "gemini");
run(
  "complete",
  taskId,
  "--summary",
  "fake Gemini worker completed through CLI",
  "--metadata",
  JSON.stringify({ verification: ["gemini-cli-bridge-e2e"] }),
);

process.stdout.write(`${JSON.stringify({
  type: "init",
  timestamp: new Date().toISOString(),
  session_id: "22222222-2222-4222-8222-222222222222",
  model: "fixture-gemini",
})}\n`);
process.stdout.write(`${JSON.stringify({ type: "result", status: "success", stats: {} })}\n`);
