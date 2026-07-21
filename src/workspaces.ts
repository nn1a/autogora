import { existsSync, mkdirSync, readdirSync, rmSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, join, resolve, sep } from "node:path";
import { spawnSync } from "node:child_process";

import { BoardManager } from "./boards.js";
import { KanbanStore } from "./store.js";
import type { ClaimedTask, Task } from "./types.js";

function expandHome(path: string): string {
  return path === "~" ? homedir() : path.startsWith(`~${sep}`) ? join(homedir(), path.slice(2)) : path;
}

function requireAbsolute(path: string, label: string): string {
  const expanded = expandHome(path);
  if (!isAbsolute(expanded)) throw new Error(`${label} must be an absolute path: ${path}`);
  return resolve(expanded);
}

function isNonEmptyDirectory(path: string): boolean {
  return existsSync(path) && statSync(path).isDirectory() && readdirSync(path).length > 0;
}

function gitRoot(path: string): string | null {
  const result = spawnSync("git", ["-C", path, "rev-parse", "--show-toplevel"], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "ignore"],
  });
  return result.status === 0 ? resolve(result.stdout.trim()) : null;
}

function addWorktree(repository: string, target: string, branch: string | null): void {
  if (isNonEmptyDirectory(target)) return;
  mkdirSync(dirname(target), { recursive: true });
  const args = ["-C", repository, "worktree", "add"];
  if (branch) {
    const branchExists = spawnSync("git", ["-C", repository, "show-ref", "--verify", `refs/heads/${branch}`], {
      stdio: "ignore",
    }).status === 0;
    if (branchExists) args.push(target, branch);
    else args.push("-b", branch, target, "HEAD");
  } else {
    args.push("--detach", target, "HEAD");
  }
  const result = spawnSync("git", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  if (result.status !== 0) {
    throw new Error(`Unable to create git worktree: ${(result.stderr || result.stdout).trim()}`);
  }
}

export class WorkspaceManager {
  constructor(private readonly boards: BoardManager) {}

  prepare(store: KanbanStore, claim: ClaimedTask): ClaimedTask {
    const task = claim.task.task;
    const metadata = this.boards.read(task.board);
    const scope = { runId: claim.run.id, claimToken: claim.claimToken };
    let kind: "scratch" | "dir" | "worktree";
    let path: string;

    if (task.workspace && task.workspace !== "scratch") {
      if (task.workspace === "worktree" || task.workspace.startsWith("worktree:")) {
        kind = "worktree";
        const requestedTarget = task.workspace.startsWith("worktree:") ? task.workspace.slice("worktree:".length) : "";
        path = requestedTarget
          ? requireAbsolute(requestedTarget, "worktree target")
          : join(this.boards.workspaceRoot(task.board), task.id);
        const source = metadata.defaultWorkdir ? requireAbsolute(metadata.defaultWorkdir, "board default workdir") : process.cwd();
        const repository = gitRoot(source);
        if (!repository) throw new Error(`Worktree workspace requires a git repository: ${source}`);
        addWorktree(repository, path, task.branch);
      } else {
        kind = "dir";
        const rawPath = task.workspace.startsWith("dir:") ? task.workspace.slice("dir:".length) : task.workspace;
        path = requireAbsolute(rawPath, "dir workspace");
        if (!existsSync(path) || !statSync(path).isDirectory()) {
          throw new Error(`dir workspace does not exist: ${path}`);
        }
      }
    } else if (task.workspaceKind === "worktree" || (metadata.defaultWorkdir && gitRoot(expandHome(metadata.defaultWorkdir)))) {
      kind = "worktree";
      const source = metadata.defaultWorkdir ? requireAbsolute(metadata.defaultWorkdir, "board default workdir") : process.cwd();
      const repository = gitRoot(source);
      if (!repository) throw new Error(`Worktree workspace requires a git repository: ${source}`);
      path = join(this.boards.workspaceRoot(task.board), task.id);
      addWorktree(repository, path, task.branch);
    } else if (metadata.defaultWorkdir) {
      kind = "dir";
      path = requireAbsolute(metadata.defaultWorkdir, "board default workdir");
      if (!existsSync(path) || !statSync(path).isDirectory()) {
        throw new Error(`Board default workdir does not exist: ${path}`);
      }
    } else {
      kind = "scratch";
      path = join(this.boards.workspaceRoot(task.board), task.id);
      mkdirSync(path, { recursive: true });
    }

    const detail = store.bindRunWorkspace(scope, path, kind);
    return { ...claim, task: detail };
  }

  cleanup(task: Task): boolean {
    if (task.workspaceKind !== "scratch" || !task.workspace) return false;
    const root = resolve(this.boards.workspaceRoot(task.board));
    const target = resolve(task.workspace);
    if (target !== join(root, task.id) || !target.startsWith(`${root}${sep}`)) {
      throw new Error(`Refusing to clean an untrusted scratch path: ${target}`);
    }
    if (!existsSync(target)) return false;
    rmSync(target, { recursive: true, force: true });
    return true;
  }
}
