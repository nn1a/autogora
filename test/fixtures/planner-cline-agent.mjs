#!/usr/bin/env node

const prompt = process.argv.at(-1) ?? "";
if (!process.argv.includes("--json") || !prompt.includes("must conform to this schema")) {
  throw new Error("Cline planner did not receive JSON mode and schema guidance");
}

process.stdout.write(`${JSON.stringify({ type: "agent_event", event: {
  type: "done",
  text: JSON.stringify({
    title: "Cline-generated task specification",
    body: "Implement the requested change. Acceptance: record CLI verification evidence.",
  }),
} })}\n`);
process.stdout.write(`${JSON.stringify({ type: "run_result", text: JSON.stringify({
  title: "Cline-generated task specification",
  body: "Implement the requested change. Acceptance: record CLI verification evidence.",
}) })}\n`);
