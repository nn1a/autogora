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

## MCP tools

- Planning: `kanban_create`, `kanban_list`, `kanban_show`, `kanban_update`, `kanban_comment`, `kanban_link`
- Dispatch: `kanban_claim`
- Worker lifecycle: `kanban_heartbeat`, `kanban_complete`, `kanban_block`
- Human recovery: `kanban_unblock`

Dispatcher-launched workers receive task, run, and claim-token scope through environment variables. Their lifecycle tools can omit those identifiers, and the server rejects attempts to operate on another scoped task or run.

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
- SQLite assumes one host. Atomic claims allow multiple local dispatcher processes, but this MVP does not yet reclaim a run after the dispatcher host itself crashes.
- There is no dashboard, attachment storage, scheduler, notification gateway, automatic decomposition, or PR review gate yet.
