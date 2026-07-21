import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BoardManager } from "../src/boards.js";

test("file, URL, and completion artifact attachments remain board scoped and durable", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-attachments-"));
  const workspace = join(directory, "workspace");
  const manager = new BoardManager(join(directory, "kanban.db"));
  manager.create("project");
  const store = manager.openStore("project");
  try {
    const source = join(directory, "requirements.md");
    writeFileSync(source, "# Requirements\n", "utf8");
    const task = store.createTask({
      title: "attachment task",
      assignee: "worker",
      runtime: "codex",
      workspace,
    });

    const file = store.attachFile(task.task.id, source, "requirements.md");
    assert.equal(file.kind, "file");
    assert.ok(file.path?.startsWith(manager.attachmentsRoot("project")));
    assert.equal(readFileSync(file.path!, "utf8"), "# Requirements\n");
    assert.equal(file.sha256?.length, 64);

    const url = store.attachUrl(task.task.id, "https://example.com/design.pdf", "design reference");
    assert.equal(url.kind, "url");
    assert.equal(url.url, "https://example.com/design.pdf");
    assert.throws(() => store.attachUrl(task.task.id, "file:///etc/passwd"), /http or https/);

    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    rmSync(workspace, { recursive: true, force: true });
    writeFileSync(join(directory, "placeholder"), "", "utf8");
    // The worker workspace is normally created by the dispatcher. Build it here
    // to exercise relative artifact capture at the kernel boundary.
    mkdirSync(workspace, { recursive: true });
    writeFileSync(join(workspace, "report.txt"), "verified output", "utf8");
    const completed = store.completeRun(
      { runId: claim.run.id, claimToken: claim.claimToken },
      { summary: "report produced", artifacts: ["report.txt"] },
    );
    assert.equal(completed.task.status, "done");
    assert.equal(completed.attachments.length, 3);
    assert.equal(completed.attachments.find((attachment) => attachment.name === "report.txt")?.size, 15);
    assert.equal((completed.runs[0]?.metadata?.artifacts as { name: string }[])[0]?.name, "report.txt");

    assert.deepEqual(store.removeAttachment(task.task.id, file.id), { id: file.id, removed: true });
    assert.equal(existsSync(file.path!), false);

    const taskAttachmentDir = join(manager.attachmentsRoot("project"), task.task.id);
    assert.ok(existsSync(taskAttachmentDir));
    store.deleteTask(task.task.id);
    assert.equal(existsSync(taskAttachmentDir), false);
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});

test("missing declared artifacts leave the claimed task running for correction", () => {
  const directory = mkdtempSync(join(tmpdir(), "kanban-artifact-missing-"));
  const manager = new BoardManager(join(directory, "kanban.db"));
  const store = manager.openStore("default");
  try {
    const task = store.createTask({
      title: "missing artifact",
      assignee: "worker",
      runtime: "claude",
      workspace: directory,
    });
    const claim = store.claimTask({ taskId: task.task.id });
    assert.ok(claim);
    assert.throws(
      () => store.completeRun(
        { runId: claim.run.id, claimToken: claim.claimToken },
        { summary: "done", artifacts: ["does-not-exist.txt"] },
      ),
      /Attachment file not found/,
    );
    assert.equal(store.getTask(task.task.id).task.status, "running");
  } finally {
    store.close();
    rmSync(directory, { recursive: true, force: true });
  }
});
