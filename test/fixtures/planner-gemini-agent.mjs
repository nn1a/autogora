#!/usr/bin/env node

import { readFileSync } from "node:fs";

const valueAfter = (flag) => {
  const index = process.argv.indexOf(flag);
  return index >= 0 ? process.argv[index + 1] : undefined;
};
const prompt = valueAfter("-p") ?? "";
const policyPath = valueAfter("--policy");

if (valueAfter("--output-format") !== "json" || valueAfter("--approval-mode") !== "default") {
  throw new Error("Gemini planner must use constrained JSON headless mode");
}
if (!policyPath || !readFileSync(policyPath, "utf8").includes('toolName = "*"')) {
  throw new Error("Gemini planner must receive a deny-all tool policy");
}
if (!prompt.includes("must conform to this schema")) {
  throw new Error("Gemini planner did not receive schema guidance");
}

process.stdout.write(`${JSON.stringify({
  response: JSON.stringify({
    title: "Gemini-generated task specification",
    body: "Implement the requested change. Acceptance: record Gemini CLI verification evidence.",
  }),
  stats: {},
})}\n`);
