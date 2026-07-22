#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const sessionId = "33333333-3333-4333-8333-333333333333";
const cliEntry = fileURLToPath(new URL("../../dist/cli.js", import.meta.url));
const taskId = process.env.KANBAN_TASK_ID;
const marker = join(process.env.KANBAN_WORKSPACE, `.gemini-goal-turn-${taskId}`);
const resumeIndex = process.argv.indexOf("--resume");
const run = (...args) => execFileSync(process.execPath, [cliEntry, ...args], {
  env: process.env,
  encoding: "utf8",
});

run("show", taskId);
if (!existsSync(marker)) {
  if (resumeIndex >= 0) throw new Error("Initial Gemini goal turn must not resume a session");
  writeFileSync(marker, "first turn", "utf8");
  run("comment", taskId, "First Gemini goal turn left a durable handoff.", "--author", "gemini");
  process.stdout.write(`${JSON.stringify({ type: "init", session_id: sessionId, model: "fixture-gemini" })}\n`);
  process.stdout.write(`${JSON.stringify({ type: "message", role: "assistant", content: "One acceptance gap remains." })}\n`);
  process.stdout.write(`${JSON.stringify({ type: "result", status: "success", stats: {} })}\n`);
} else {
  if (resumeIndex < 0 || process.argv[resumeIndex + 1] !== sessionId) {
    throw new Error("Gemini continuation must resume the stream-json session");
  }
  const promptIndex = process.argv.indexOf("-p");
  if (promptIndex < 0 || !process.argv[promptIndex + 1]?.includes("Finish the remaining gap")) {
    throw new Error("Gemini continuation is missing the judge prompt");
  }
  run("complete", taskId, "--summary", "Gemini completed the goal in the resumed session");
  process.stdout.write(`${JSON.stringify({ type: "init", session_id: sessionId, model: "fixture-gemini" })}\n`);
  process.stdout.write(`${JSON.stringify({ type: "result", status: "success", stats: {} })}\n`);
}
