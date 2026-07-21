#!/usr/bin/env node

import { readFileSync, writeFileSync } from "node:fs";

const outputIndex = process.argv.indexOf("--output-last-message");
const schemaIndex = process.argv.indexOf("--output-schema");
if (outputIndex < 0 || schemaIndex < 0) process.exit(2);
const outputPath = process.argv[outputIndex + 1];
const schemaPath = process.argv[schemaIndex + 1];
const schema = JSON.parse(readFileSync(schemaPath, "utf8"));
if (schema.properties?.title) {
  writeFileSync(outputPath, JSON.stringify({
    title: "Planner-generated task specification",
    body: "Deliver the requested result. Acceptance: verification evidence is recorded.",
  }), "utf8");
} else {
  writeFileSync(outputPath, JSON.stringify({
    fanout: false,
    rootTitle: "Planner-generated task specification",
    rootBody: "Deliver the requested result. Acceptance: verification evidence is recorded.",
    reason: "No fanout is needed.",
    tasks: [],
    dependencies: [],
  }), "utf8");
}
