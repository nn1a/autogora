# Hermes-style Kanban MCP MVP

A local, durable Kanban control plane that Claude Code and Codex can share through MCP. It provides SQLite-backed tasks, dependencies, comments, atomic claims, scoped claim tokens, heartbeat, completion/blocking, bounded retries, and an optional CLI dispatcher.

## Requirements

- Node.js 24 or newer
- Claude Code and/or Codex CLI only when using the dispatcher

## Set up

```bash
npm install
npm run build
node dist/cli.js init
```

Connect Claude Code:

```bash
claude mcp add --scope local kanban -- \
  node "$PWD/dist/cli.js" serve --db "$PWD/data/kanban.db"
```

Connect Codex:

```bash
codex mcp add kanban -- \
  node "$PWD/dist/cli.js" serve --db "$PWD/data/kanban.db"
```

The equivalent checked-in examples are [examples/claude.mcp.json](examples/claude.mcp.json) and [examples/codex.config.toml](examples/codex.config.toml).

## Try a task

Create one from the shell:

```bash
node dist/cli.js create "Inspect the authentication module" \
  --body "Document the flow and verify it against existing tests." \
  --assignee reviewer \
  --runtime codex \
  --workspace "$PWD"
```

Run one worker in read-only mode:

```bash
node dist/cli.js dispatch --once
```

For a trusted coding workspace, explicitly allow writes:

```bash
node dist/cli.js dispatch --once --allow-writes
```

Run a persistent local dispatcher with up to two workers:

```bash
node dist/cli.js dispatch --watch --max-workers 2 --allow-writes
```

Long-running dispatchers persist claim TTLs, heartbeats, worker PIDs, and task
runtime limits. They recover dead or stale workers, terminate tasks that exceed
`max_runtime_seconds`, and treat exit code 75 as a retry-neutral provider rate
limit. Optional `--max-in-progress` and `--max-per-assignee` caps coordinate
multiple dispatcher processes through the database.

Worker output is stored next to the database under `data/logs/`.

Automation-friendly task fields are available through both the CLI and MCP:

```bash
node dist/cli.js create "Nightly security audit" \
  --tenant acme \
  --idempotency-key "audit-2026-07-22" \
  --scheduled-at "2026-07-22T23:00:00+09:00" \
  --max-runtime-seconds 1800 \
  --skill security-audit \
  --goal --goal-max-turns 12 \
  --assignee reviewer --runtime codex
```

Scheduled cards are promoted when they become due. Repeating the same non-empty
idempotency key on a board returns the existing non-archived task.

## Multiple boards

Named boards isolate their database, workspaces, attachments, and logs. The
`default` board retains `data/kanban.db`; named boards live under
`data/boards/<slug>/`.

```bash
node dist/cli.js boards create project-api \
  --name "Project API" --default-workdir "$PWD" --switch
node dist/cli.js boards list
node dist/cli.js boards show
node dist/cli.js boards rename project-api "Project API v2"
node dist/cli.js boards rm project-api       # recoverable archive
```

Use `boards rm <slug> --delete` only when permanent removal is intended. The
`default` board cannot be removed.

## Workspaces

- `scratch` (default): isolated under the board workspace root and removed only
  after successful completion and artifact capture.
- `dir:/absolute/path`: uses and preserves an existing trusted directory.
- `worktree`: creates and preserves a Git worktree under the board workspace
  root. Set a board `default-workdir` to the source repository and optionally
  pass `--branch` on task creation.
- `worktree:/absolute/target`: pins the worktree destination explicitly.

Relative `dir:` and explicit worktree paths are rejected. Dispatcher runs record
their resolved workspace, worker PID, and log path in attempt history.

## Attachments and artifacts

Files are copied into the active board's durable attachment root and limited to
25 MB each. HTTP(S) references are stored without downloading them.

```bash
node dist/cli.js attach <task-id> ./requirements.pdf
node dist/cli.js attach-url <task-id> https://example.com/design --name "Design"
node dist/cli.js attachments <task-id>
```

Workers can declare relative deliverables in `kanban_complete` through its
`artifacts` array. Every path must exist before the task can become `done`; the
server copies valid artifacts into durable storage and records them in run
metadata.

## Observe and maintain a board

The CLI and MCP expose the same bounded worker context, attempt history, event
cursor, counts, and health diagnostics used by the dispatcher:

```bash
node dist/cli.js context <task-id>
node dist/cli.js runs <task-id>
node dist/cli.js log <task-id> --tail-bytes 65536
node dist/cli.js stats
node dist/cli.js diagnostics
node dist/cli.js watch --since 0
node dist/cli.js tail <task-id> --follow
```

Bulk mutations always report success or failure for each task instead of
aborting the whole batch. Garbage collection is board-scoped and only removes
expired events, log files, and old scratch directories that still map to a
terminal task; preserved directories and worktrees are left untouched.

```bash
node dist/cli.js bulk <task-a> <task-b> --assignee reviewer --priority 10
node dist/cli.js gc --event-retention-days 30 --log-retention-days 30
```

## MCP tools

- Planning: `kanban_create`, `kanban_list`, `kanban_show`, `kanban_update`, `kanban_comment`, `kanban_link`, `kanban_unlink`
- Boards: `kanban_boards_list`, `kanban_boards_create`, `kanban_boards_update`, `kanban_boards_switch`, `kanban_boards_remove`
- Dispatch: `kanban_claim`
- Worker lifecycle: `kanban_heartbeat`, `kanban_complete`, `kanban_block`
- Attachments: `kanban_attach`, `kanban_attach_url`, `kanban_attachments`, `kanban_attachment_remove`
- Observability: `kanban_context`, `kanban_stats`, `kanban_diagnostics`, `kanban_events`, `kanban_runs`, `kanban_log`
- Administration: `kanban_bulk`, `kanban_gc`
- Human recovery: `kanban_unblock`, `kanban_promote`, `kanban_schedule`, `kanban_archive`, `kanban_delete`

Dispatcher-launched workers receive board, task, run, and claim-token scope
through environment variables. Their lifecycle tools can omit those identifiers,
and the server rejects attempts to operate on another scoped board, task, or
run. Without `--board`, the dispatcher sweeps all active boards while preserving
the global worker limit.

## Skills

The portable Agent Skills are under `skills/`:

- `kanban-worker`: execute and close one claimed task
- `kanban-orchestrator`: create an executable dependency graph

Install them into the client you use:

```bash
cp -R skills/kanban-worker skills/kanban-orchestrator ~/.agents/skills/
cp -R skills/kanban-worker skills/kanban-orchestrator ~/.claude/skills/
```

Restart the client if it does not detect the new skills.

## Safety and MVP limits

- `--allow-writes` grants a spawned coding worker workspace edits and shell access. Use only in repositories you trust.
- The server is local stdio only; there is no remote authentication or multi-user isolation.
- SQLite and PID recovery assume one host; cross-host dispatch is intentionally unsupported.
- The dashboard, notification delivery, triage decomposition, goal continuation, and review automation are still in progress. See `docs/HERMES_PARITY.md` for the audited checklist.
