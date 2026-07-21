import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

function cli(args: string[], env: NodeJS.ProcessEnv = process.env): { status: number | null; stdout: string; stderr: string } {
  const result = spawnSync(process.execPath, [resolve("dist/cli.js"), ...args], {
    encoding: "utf8",
    env,
  });
  return { status: result.status, stdout: result.stdout, stderr: result.stderr };
}

function successfulJson<T>(args: string[], env?: NodeJS.ProcessEnv): T {
  const result = cli(args, env);
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout) as T;
}

test("CLI parity verbs share atomic claims, heartbeats, routing fields, and bulk assignment", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-cli-"));
  const dbPath = join(directory, "kanban.db");
  try {
    const initialized = cli(["init", "--db", dbPath]);
    assert.equal(initialized.status, 0, initialized.stderr);

    const created = successfulJson<any>([
      "create", "CLI task",
      "--db", dbPath,
      "--assignee", "worker",
      "--runtime", "codex",
      "--max-runtime", "30m",
      "--workflow-template-id", "release",
      "--current-step-key", "build",
    ]);
    assert.equal(created.task.maxRuntimeSeconds, 1_800);
    assert.equal(created.task.status, "ready");

    const routed = successfulJson<any[]>([
      "list", "--db", dbPath,
      "--workflow-template-id", "release",
      "--current-step-key", "build",
      "--sort", "created",
    ]);
    assert.deepEqual(routed.map((task) => task.id), [created.task.id]);

    const claim = successfulJson<any>(["claim", created.task.id, "--db", dbPath, "--ttl", "120"]);
    assert.equal(claim.task.task.status, "running");
    assert.ok(claim.task.task.workspace);
    assert.ok(claim.claimToken);
    const heartbeat = successfulJson<any>([
      "heartbeat", created.task.id, "--db", dbPath, "--note", "CLI integration test",
    ]);
    assert.equal(heartbeat.id, claim.run.id);
    assert.equal(heartbeat.status, "running");
    successfulJson<any[]>(["complete", created.task.id, "--db", dbPath, "--summary", "CLI flow complete"]);

    const first = successfulJson<any>(["create", "First", "--db", dbPath]);
    const second = successfulJson<any>(["create", "Second", "--db", dbPath]);
    const reassigned = successfulJson<any>([
      "reassign", first.task.id, second.task.id, "reviewer", "--db", dbPath,
    ]);
    assert.equal(reassigned.ok.length, 2);
    assert.equal(reassigned.errors.length, 0);

    const blocked = successfulJson<any[]>([
      "block", first.task.id, "batch review required", "--ids", second.task.id, "--db", dbPath,
    ]);
    assert.deepEqual(blocked.map((detail) => detail.task.status), ["blocked", "blocked"]);

    const mine = successfulJson<any[]>(["list", "--db", dbPath, "--mine"], {
      ...process.env,
      HERMES_PROFILE: "reviewer",
    });
    assert.deepEqual(new Set(mine.map((task) => task.id)), new Set([first.task.id, second.task.id]));

    const triage = successfulJson<any>(["create", "Rough idea", "--db", dbPath, "--triage"]);
    assert.equal(triage.task.status, "triage");

    const dispatchable = successfulJson<any>([
      "create", "Dry run candidate", "--db", dbPath, "--assignee", "worker", "--runtime", "codex",
    ]);
    const dryRun = successfulJson<any>(["dispatch", "--db", dbPath, "--dry-run", "--max", "1"]);
    assert.equal(dryRun.dryRun, true);
    assert.deepEqual(dryRun.candidates.map((task: { id: string }) => task.id), [dispatchable.task.id]);

    const daemon = cli(["daemon", "--db", dbPath]);
    assert.equal(daemon.status, 1);
    assert.match(daemon.stderr, /requires --force/);

    const invalidSort = cli(["list", "--db", dbPath, "--sort", "unsupported"]);
    assert.equal(invalidSort.status, 1);
    assert.match(invalidSort.stderr, /Invalid sort/);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
