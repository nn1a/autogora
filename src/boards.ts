import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";

import { KanbanStore } from "./store.js";
import { TASK_STATUSES, type Runtime, type TaskStatus } from "./types.js";

export interface BoardProfile {
  name: string;
  runtime: Exclude<Runtime, "manual">;
  description: string;
}

export interface BoardOrchestrationSettings {
  autoDecompose: boolean;
  autoDecomposePerTick: number;
  plannerRuntime: Exclude<Runtime, "manual">;
  defaultProfile: string | null;
  orchestratorProfile: string | null;
  profiles: BoardProfile[];
}

export interface BoardMetadata {
  slug: string;
  name: string;
  description: string;
  icon: string;
  color: string;
  defaultWorkdir: string | null;
  createdAt: string | null;
  archived: boolean;
  dbPath: string;
  workspaceRoot: string;
  attachmentsRoot: string;
  logsRoot: string;
  orchestration: BoardOrchestrationSettings;
  counts?: Record<TaskStatus, number> | undefined;
}

export interface BoardUpdate {
  name?: string | undefined;
  description?: string | undefined;
  icon?: string | undefined;
  color?: string | undefined;
  defaultWorkdir?: string | null | undefined;
  orchestration?: Partial<BoardOrchestrationSettings> | undefined;
}

const BOARD_SLUG = /^[a-z0-9][a-z0-9_-]{0,63}$/;

export function normalizeBoardSlug(value: string): string {
  const slug = value.trim().toLowerCase();
  if (!BOARD_SLUG.test(slug)) {
    throw new Error(
      `Invalid board slug ${JSON.stringify(value)}: use 1-64 lowercase alphanumerics, hyphens, or underscores`,
    );
  }
  return slug;
}

function defaultBoardName(slug: string): string {
  return slug
    .replaceAll("_", "-")
    .split("-")
    .filter(Boolean)
    .map((part) => part[0]?.toUpperCase() + part.slice(1))
    .join(" ");
}

function safeMetadata(path: string): Partial<BoardMetadata> {
  if (!existsSync(path)) return {};
  try {
    const value = JSON.parse(readFileSync(path, "utf8")) as unknown;
    return value && !Array.isArray(value) && typeof value === "object" ? value as Partial<BoardMetadata> : {};
  } catch {
    return {};
  }
}

function orchestrationSettings(raw: Partial<BoardOrchestrationSettings> | undefined): BoardOrchestrationSettings {
  const profiles = Array.isArray(raw?.profiles)
    ? raw.profiles.filter((profile): profile is BoardProfile =>
        Boolean(profile && typeof profile.name === "string" &&
          (profile.runtime === "claude" || profile.runtime === "codex")),
      ).map((profile) => ({
        name: profile.name.trim(),
        runtime: profile.runtime,
        description: typeof profile.description === "string" ? profile.description : "",
      })).filter((profile) => profile.name)
    : [];
  return {
    autoDecompose: raw?.autoDecompose !== false,
    autoDecomposePerTick: Number.isInteger(raw?.autoDecomposePerTick) && (raw?.autoDecomposePerTick ?? 0) > 0
      ? raw!.autoDecomposePerTick!
      : 3,
    plannerRuntime: raw?.plannerRuntime === "claude" ? "claude" : "codex",
    defaultProfile: typeof raw?.defaultProfile === "string" && raw.defaultProfile.trim()
      ? raw.defaultProfile.trim()
      : null,
    orchestratorProfile: typeof raw?.orchestratorProfile === "string" && raw.orchestratorProfile.trim()
      ? raw.orchestratorProfile.trim()
      : null,
    profiles,
  };
}

export class BoardManager {
  readonly defaultDbPath: string;
  readonly home: string;
  readonly boardsRoot: string;
  readonly currentPath: string;

  constructor(defaultDbPath: string) {
    this.defaultDbPath = resolve(defaultDbPath);
    this.home = dirname(this.defaultDbPath);
    this.boardsRoot = join(this.home, "boards");
    this.currentPath = join(this.home, "current");
  }

  boardDir(board: string): string {
    return join(this.boardsRoot, normalizeBoardSlug(board));
  }

  dbPath(board = "default"): string {
    const slug = normalizeBoardSlug(board);
    return slug === "default" ? this.defaultDbPath : join(this.boardDir(slug), "kanban.db");
  }

  workspaceRoot(board = "default"): string {
    const slug = normalizeBoardSlug(board);
    return slug === "default" ? join(this.home, "workspaces") : join(this.boardDir(slug), "workspaces");
  }

  attachmentsRoot(board = "default"): string {
    const slug = normalizeBoardSlug(board);
    return slug === "default" ? join(this.home, "attachments") : join(this.boardDir(slug), "attachments");
  }

  logsRoot(board = "default"): string {
    const slug = normalizeBoardSlug(board);
    return slug === "default" ? join(this.home, "logs") : join(this.boardDir(slug), "logs");
  }

  metadataPath(board = "default"): string {
    return join(this.boardDir(board), "board.json");
  }

  exists(board: string): boolean {
    const slug = normalizeBoardSlug(board);
    return slug === "default" || existsSync(this.dbPath(slug)) || existsSync(this.metadataPath(slug));
  }

  read(board = "default"): BoardMetadata {
    const slug = normalizeBoardSlug(board);
    const raw = safeMetadata(this.metadataPath(slug));
    return {
      slug,
      name: typeof raw.name === "string" && raw.name.trim() ? raw.name : defaultBoardName(slug),
      description: typeof raw.description === "string" ? raw.description : "",
      icon: typeof raw.icon === "string" ? raw.icon : "",
      color: typeof raw.color === "string" ? raw.color : "",
      defaultWorkdir: typeof raw.defaultWorkdir === "string" && raw.defaultWorkdir ? raw.defaultWorkdir : null,
      createdAt: typeof raw.createdAt === "string" ? raw.createdAt : null,
      archived: raw.archived === true,
      dbPath: this.dbPath(slug),
      workspaceRoot: this.workspaceRoot(slug),
      attachmentsRoot: this.attachmentsRoot(slug),
      logsRoot: this.logsRoot(slug),
      orchestration: orchestrationSettings(raw.orchestration),
    };
  }

  private write(board: string, update: BoardUpdate & { archived?: boolean }): BoardMetadata {
    const slug = normalizeBoardSlug(board);
    const existing = this.read(slug);
    const metadata: BoardMetadata = {
      ...existing,
      name: update.name?.trim() || existing.name,
      description: update.description ?? existing.description,
      icon: update.icon ?? existing.icon,
      color: update.color ?? existing.color,
      defaultWorkdir: update.defaultWorkdir === undefined ? existing.defaultWorkdir : update.defaultWorkdir,
      archived: update.archived ?? existing.archived,
      orchestration: orchestrationSettings({ ...existing.orchestration, ...(update.orchestration ?? {}) }),
      createdAt: existing.createdAt ?? new Date().toISOString(),
    };
    const path = this.metadataPath(slug);
    mkdirSync(dirname(path), { recursive: true });
    const persisted = {
      slug: metadata.slug,
      name: metadata.name,
      description: metadata.description,
      icon: metadata.icon,
      color: metadata.color,
      defaultWorkdir: metadata.defaultWorkdir,
      createdAt: metadata.createdAt,
      archived: metadata.archived,
      orchestration: metadata.orchestration,
    };
    writeFileSync(path, `${JSON.stringify(persisted, null, 2)}\n`, "utf8");
    return metadata;
  }

  create(board: string, update: BoardUpdate = {}): BoardMetadata {
    const slug = normalizeBoardSlug(board);
    const metadata = this.write(slug, update);
    const store = this.openStore(slug);
    store.close();
    mkdirSync(this.workspaceRoot(slug), { recursive: true });
    mkdirSync(this.attachmentsRoot(slug), { recursive: true });
    mkdirSync(this.logsRoot(slug), { recursive: true });
    return metadata;
  }

  update(board: string, update: BoardUpdate): BoardMetadata {
    const slug = normalizeBoardSlug(board);
    if (!this.exists(slug)) throw new Error(`Board not found: ${slug}`);
    return this.write(slug, update);
  }

  list(includeArchived = false): BoardMetadata[] {
    const slugs = new Set<string>(["default"]);
    if (existsSync(this.boardsRoot)) {
      for (const entry of readdirSync(this.boardsRoot, { withFileTypes: true })) {
        if (entry.isDirectory() && entry.name !== "_archived" && BOARD_SLUG.test(entry.name)) slugs.add(entry.name);
      }
    }
    const active = [...slugs]
      .sort((left, right) => left === "default" ? -1 : right === "default" ? 1 : left.localeCompare(right))
      .map((slug) => {
        const metadata = this.read(slug);
        const store = this.openStore(slug);
        try {
          return { ...metadata, counts: store.countTasksByStatus() };
        } finally {
          store.close();
        }
      });
    if (!includeArchived) return active;

    const archivedRoot = join(this.boardsRoot, "_archived");
    if (!existsSync(archivedRoot)) return active;
    const archived = readdirSync(archivedRoot, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => {
        const path = join(archivedRoot, entry.name, "board.json");
        const raw = safeMetadata(path);
        const slug = typeof raw.slug === "string" ? raw.slug : entry.name.replace(/-\d+$/, "");
        return {
          ...this.read(slug),
          ...raw,
          slug,
          archived: true,
          dbPath: join(archivedRoot, entry.name, "kanban.db"),
          workspaceRoot: join(archivedRoot, entry.name, "workspaces"),
          attachmentsRoot: join(archivedRoot, entry.name, "attachments"),
          logsRoot: join(archivedRoot, entry.name, "logs"),
        } as BoardMetadata;
      });
    return [...active, ...archived];
  }

  getCurrent(): string {
    const environment = process.env.KANBAN_BOARD?.trim();
    if (environment) {
      const slug = normalizeBoardSlug(environment);
      if (this.exists(slug)) return slug;
    }
    if (existsSync(this.currentPath)) {
      try {
        const slug = normalizeBoardSlug(readFileSync(this.currentPath, "utf8"));
        if (this.exists(slug)) return slug;
      } catch {
        // A malformed or stale pointer falls back to the default board.
      }
    }
    return "default";
  }

  resolve(explicit?: string): string {
    const slug = explicit ? normalizeBoardSlug(explicit) : this.getCurrent();
    if (!this.exists(slug)) throw new Error(`Board not found: ${slug}`);
    return slug;
  }

  switch(board: string): BoardMetadata {
    const slug = this.resolve(board);
    mkdirSync(dirname(this.currentPath), { recursive: true });
    writeFileSync(this.currentPath, `${slug}\n`, "utf8");
    return this.read(slug);
  }

  remove(board: string, hardDelete = false): { slug: string; archived: boolean; path: string } {
    const slug = normalizeBoardSlug(board);
    if (slug === "default") throw new Error("The default board cannot be removed");
    if (!this.exists(slug)) throw new Error(`Board not found: ${slug}`);
    const source = this.boardDir(slug);
    const wasCurrent = this.getCurrent() === slug;
    if (hardDelete) {
      rmSync(source, { recursive: true, force: false });
      if (wasCurrent) this.switch("default");
      return { slug, archived: false, path: source };
    }
    this.write(slug, { archived: true });
    const archivedRoot = join(this.boardsRoot, "_archived");
    mkdirSync(archivedRoot, { recursive: true });
    const target = join(archivedRoot, `${slug}-${Date.now()}`);
    renameSync(source, target);
    if (wasCurrent) this.switch("default");
    return { slug, archived: true, path: target };
  }

  openStore(board?: string): KanbanStore {
    const slug = board ? normalizeBoardSlug(board) : this.getCurrent();
    if (slug !== "default" && !this.exists(slug)) throw new Error(`Board not found: ${slug}`);
    return new KanbanStore(this.dbPath(slug), slug, this.attachmentsRoot(slug));
  }
}

export function emptyStatusCounts(): Record<TaskStatus, number> {
  return Object.fromEntries(TASK_STATUSES.map((status) => [status, 0])) as Record<TaskStatus, number>;
}
