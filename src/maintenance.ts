import { existsSync, readdirSync, rmSync, statSync, unlinkSync } from "node:fs";
import { join } from "node:path";

import { BoardManager } from "./boards.js";

export interface GarbageCollectionOptions {
  eventRetentionDays?: number | undefined;
  logRetentionDays?: number | undefined;
  workspaceRetentionDays?: number | undefined;
}

export interface GarbageCollectionResult {
  board: string;
  eventsDeleted: number;
  logsDeleted: string[];
  workspacesDeleted: string[];
}

function cutoff(days: number): number {
  return Date.now() - Math.max(0, days) * 86_400_000;
}

export function garbageCollect(
  manager: BoardManager,
  board: string,
  options: GarbageCollectionOptions = {},
): GarbageCollectionResult {
  const store = manager.openStore(board);
  const logsDeleted: string[] = [];
  const workspacesDeleted: string[] = [];
  try {
    const eventsDeleted = store.garbageCollectEvents(options.eventRetentionDays ?? 30);
    const logsRoot = manager.logsRoot(board);
    const logCutoff = cutoff(options.logRetentionDays ?? 30);
    if (existsSync(logsRoot)) {
      for (const entry of readdirSync(logsRoot, { withFileTypes: true })) {
        if (!entry.isFile()) continue;
        const path = join(logsRoot, entry.name);
        if (statSync(path).mtimeMs < logCutoff) {
          unlinkSync(path);
          logsDeleted.push(path);
        }
      }
    }

    const workspacesRoot = manager.workspaceRoot(board);
    const workspaceCutoff = cutoff(options.workspaceRetentionDays ?? 7);
    if (existsSync(workspacesRoot)) {
      for (const entry of readdirSync(workspacesRoot, { withFileTypes: true })) {
        if (!entry.isDirectory()) continue;
        const path = join(workspacesRoot, entry.name);
        if (statSync(path).mtimeMs >= workspaceCutoff) continue;
        try {
          const task = store.getTask(entry.name).task;
          if (task.workspaceKind !== "scratch" || !["done", "archived"].includes(task.status)) continue;
        } catch {
          // Without task metadata we cannot distinguish scratch from a
          // deliberately preserved worktree, so leave it alone.
          continue;
        }
        rmSync(path, { recursive: true, force: true });
        workspacesDeleted.push(path);
      }
    }
    return { board, eventsDeleted, logsDeleted, workspacesDeleted };
  } finally {
    store.close();
  }
}
