import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";

test("boards isolate databases and manage metadata, current selection, archive, and deletion", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-boards-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const previousBoard = process.env.KANBAN_BOARD;
  delete process.env.KANBAN_BOARD;
  try {
    const initial = manager.list();
    assert.equal(initial.length, 1);
    assert.equal(initial[0]?.slug, "default");

    const project = manager.create("Project_API", {
      name: "Project API",
      description: "Backend delivery",
      icon: "api",
      color: "#4f46e5",
      defaultWorkdir: "/work/api",
    });
    assert.equal(project.slug, "project_api");
    assert.equal(project.defaultWorkdir, "/work/api");
    assert.ok(existsSync(project.dbPath));
    assert.ok(existsSync(project.workspaceRoot));
    assert.ok(existsSync(project.attachmentsRoot));
    assert.ok(existsSync(project.logsRoot));

    const defaultStore = manager.openStore("default");
    const projectStore = manager.openStore("project_api");
    try {
      defaultStore.createTask({ title: "default task" });
      projectStore.createTask({ title: "project task" });
      assert.deepEqual(defaultStore.listTasks().map((task) => task.title), ["default task"]);
      assert.deepEqual(projectStore.listTasks().map((task) => task.title), ["project task"]);
      assert.equal(projectStore.listTasks()[0]?.board, "project_api");
    } finally {
      defaultStore.close();
      projectStore.close();
    }

    manager.switch("project_api");
    assert.equal(manager.getCurrent(), "project_api");
    const listed = manager.list();
    assert.equal(listed.find((board) => board.slug === "project_api")?.counts?.todo, 1);
    assert.throws(() => manager.create("../escape"), /Invalid board slug/);
    assert.throws(() => manager.remove("default"), /cannot be removed/);

    const archived = manager.remove("project_api");
    assert.equal(archived.archived, true);
    assert.ok(existsSync(archived.path));
    assert.equal(manager.getCurrent(), "default");
    assert.equal(manager.list().some((board) => board.slug === "project_api"), false);
    assert.equal(manager.list(true).some((board) => board.slug === "project_api" && board.archived), true);

    manager.create("throwaway");
    const removed = manager.remove("throwaway", true);
    assert.equal(removed.archived, false);
    assert.equal(existsSync(removed.path), false);
  } finally {
    if (previousBoard === undefined) delete process.env.KANBAN_BOARD;
    else process.env.KANBAN_BOARD = previousBoard;
    rmSync(directory, { recursive: true, force: true });
  }
});
