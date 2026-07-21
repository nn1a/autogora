# Hermes Kanban parity contract

This project targets the shipped, single-host Hermes Kanban feature set while
using Claude Code and Codex as worker runtimes. Proposed Hermes v2 workflow
templates are not part of the v1 parity contract; their routing columns remain
reserved for forward compatibility.

Authoritative references:

- [Hermes Kanban user guide](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/features/kanban.md)
- [Hermes CLI reference](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/reference/cli-commands.md#hermes-kanban)
- [Hermes repository contributor guide](https://github.com/NousResearch/hermes-agent/blob/main/AGENTS.md#kanban-multi-agent-work-queue)

## Parity checklist

### Durable board kernel

- [x] SQLite WAL storage, transactional writes, dependency-cycle rejection
- [x] Task, dependency, comment, run, and append-only event records
- [x] Statuses: `triage`, `todo`, `ready`, `running`, `blocked`, `done`, `archived`
- [x] Atomic claim tokens, heartbeat, structured completion handoff, retry budget
- [x] Statuses: `scheduled` and `review`
- [x] Tenant namespace and idempotency keys
- [x] Scheduled-start promotion and persisted runtime/skill/goal settings
- [ ] Maximum-runtime enforcement and goal-mode continuation engine
- [x] Typed blockers and repeated unblock/re-block loop breaker
- [x] Synthetic human handoff runs and reclaimed-run invariant on administrative moves
- [x] Unlink, archive, delete, promote, scheduling, and configurable sorting
- [ ] Bulk mutation with per-task failure reporting

### Multi-board isolation

- [x] Board metadata: slug, display name, description, icon, color, default workdir
- [x] Separate database, workspace, attachment, and log roots per board
- [x] Current-board resolution with validated, traversal-safe slugs
- [x] MCP/dispatcher worker board pinning and all-board dispatcher sweep
- [x] Create, list, switch, rename, archive, and delete board operations

### Workspaces and artifacts

- [ ] `scratch`, `dir:<path>`, `worktree`, and `worktree:<path>` workspaces
- [ ] Optional git branch and preserved worktree lifecycle
- [ ] Durable file and URL attachments with a 25 MB upload limit
- [ ] Completion artifact capture before scratch cleanup
- [ ] Garbage collection for scratch workspaces, old events, and logs

### Dispatcher resilience

- [x] Claude/Codex process launch, bounded parallelism, logs, terminal-call guard
- [ ] Claim TTL and safe stale-claim reclaim
- [ ] Worker PID tracking, crash detection, heartbeat-stale detection
- [ ] Maximum-runtime termination and rate-limit-neutral requeue
- [ ] Board-wide and per-runtime/profile concurrency limits
- [ ] Spawn/protocol failure classification and respawn guards
- [ ] Active-worker, run-control, backlog, and diagnostics snapshots

### Agent and human surfaces

- [x] Core MCP planning and worker lifecycle tools
- [x] Scoped worker isolation and portable worker/orchestrator Skills
- [ ] Attachment MCP tools and bounded, preformatted worker context
- [ ] Full CLI verbs for boards, tasks, runs, events, logs, stats, and diagnostics
- [ ] Terminal event watch/tail and machine-readable output
- [ ] Notification subscriptions and terminal-event delivery adapters

### Orchestration

- [ ] Manual and automatic triage specification
- [ ] Task-graph decomposition with profile/runtime routing
- [ ] Kanban Swarm topology helper
- [x] Per-task skill guidance injected into Claude/Codex workers
- [ ] Goal-mode continuation and completion judgment

### Dashboard

- [ ] Local authenticated HTTP API for every kernel operation
- [ ] Kanban columns, search/filter, archived toggle, profile/runtime lanes
- [ ] Create/edit drawer, dependencies, comments, runs, attachments, events
- [ ] Drag/drop and bulk status/assignee/archive/delete operations
- [ ] Board switcher/settings and orchestration controls
- [ ] Live event stream with reconnect/cursor support

## Explicit upstream boundary

Like Hermes Kanban, this project is single-host. Cross-host SQLite sharing is
not supported. Messaging-platform slash commands are integration adapters, not
board-kernel behavior; this standalone implementation exposes the same actions
through MCP, CLI, and its HTTP API so a platform adapter can call them without
duplicating business logic.
