#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const cliEntry = fileURLToPath(new URL("../../dist/cli.js", import.meta.url));
const taskId = process.env.KANBAN_TASK_ID;
const marker = join(process.env.KANBAN_WORKSPACE, `.cline-goal-turn-${taskId}`);
const run = (...args) => execFileSync(process.execPath, [cliEntry, ...args], {
  env: process.env,
  encoding: "utf8",
});

run("show", taskId);
if (!existsSync(marker)) {
  writeFileSync(marker, "first turn", "utf8");
  run("comment", taskId, "First Cline goal turn left a durable handoff.", "--author", "cline");
  process.stdout.write(`${JSON.stringify({ type: "run_result", text: "One acceptance gap remains." })}\n`);
} else {
  run("complete", taskId, "--summary", "Cline completed the goal in a fresh continuation turn");
  process.stdout.write(`${JSON.stringify({ type: "run_result", text: "All acceptance criteria are complete." })}\n`);
}
