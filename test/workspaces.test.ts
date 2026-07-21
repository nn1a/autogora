import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";
import { WorkspaceManager } from "../src/workspaces.js";

test("scratch workspaces are isolated and removed only after successful completion", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-scratch-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const workspaces = new WorkspaceManager(manager);
  const store = manager.openStore("default");
  try {
    const task = store.createTask({ title: "scratch", assignee: "worker", runtime: "codex" });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    const prepared = workspaces.prepare(store, claim);
    const path = prepared.task.task.workspace;
    assert.equal(prepared.task.task.workspaceKind, "scratch");
    assert.ok(path);
    assert.ok(existsSync(path));

    store.completeRun(
      { runId: claim.run.id, claimToken: claim.claimToken },
      { summary: "scratch work complete" },
    );
    assert.equal(workspaces.cleanup(store.getTask(task.task.id).task), true);
    assert.equal(existsSync(path), false);
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("git board defaults create preserved worktrees with optional branches", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-worktree-"));
  const repository = join(directory, "repository");
  mkdirSync(repository, { recursive: true });
  execFileSync("git", ["init", "-b", "main", repository], { stdio: "ignore" });
  writeFileSync(join(repository, "README.md"), "# Fixture\n", "utf8");
  execFileSync("git", ["-C", repository, "add", "README.md"]);
  execFileSync(
    "git",
    ["-C", repository, "-c", "user.name=Kanban Test", "-c", "user.email=kanban@example.com", "commit", "-m", "Initial fixture"],
    { stdio: "ignore" },
  );

  const manager = new BoardManager(join(directory, "kanban.db"));
  manager.create("project", { defaultWorkdir: repository });
  const workspaces = new WorkspaceManager(manager);
  const store = manager.openStore("project");
  try {
    const task = store.createTask({
      title: "worktree",
      assignee: "worker",
      runtime: "claude",
      branch: "kanban/worktree-test",
    });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    const prepared = workspaces.prepare(store, claim);
    const path = prepared.task.task.workspace;
    assert.ok(path);
    assert.equal(prepared.task.task.workspaceKind, "worktree");
    assert.ok(existsSync(join(path, "README.md")));
    assert.equal(
      execFileSync("git", ["-C", path, "branch", "--show-current"], { encoding: "utf8" }).trim(),
      "kanban/worktree-test",
    );

    store.completeRun(
      { runId: claim.run.id, claimToken: claim.claimToken },
      { summary: "worktree work complete" },
    );
    assert.equal(workspaces.cleanup(store.getTask(task.task.id).task), false);
    assert.ok(existsSync(path));
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("dir workspaces reject ambiguous relative paths", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-dir-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  try {
    const task = store.createTask({
      title: "unsafe dir",
      assignee: "worker",
      runtime: "codex",
      workspace: "dir:../ambiguous",
    });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    assert.throws(() => new WorkspaceManager(manager).prepare(store, claim), /must be an absolute path/);
    assert.equal(store.getTask(task.task.id).task.status, "running");
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
