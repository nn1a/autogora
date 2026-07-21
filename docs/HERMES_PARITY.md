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
- [x] Maximum-runtime enforcement
- [x] Goal-mode same-session continuation, independent judgment, and turn budget
- [x] Typed blockers and repeated unblock/re-block loop breaker
- [x] Synthetic human handoff runs and reclaimed-run invariant on administrative moves
- [x] Unlink, archive, delete, promote, scheduling, and configurable sorting
- [x] Bulk mutation with per-task failure reporting

### Multi-board isolation

- [x] Board metadata: slug, display name, description, icon, color, default workdir
- [x] Separate database, workspace, attachment, and log roots per board
- [x] Current-board resolution with validated, traversal-safe slugs
- [x] MCP/dispatcher worker board pinning and all-board dispatcher sweep
- [x] Create, list, switch, rename, archive, and delete board operations

### Workspaces and artifacts

- [x] `scratch`, `dir:<path>`, `worktree`, and `worktree:<path>` workspaces
- [x] Optional git branch and preserved worktree lifecycle
- [x] Durable file and URL attachments with a 25 MB upload limit
- [x] Completion artifact validation and durable capture
- [x] Successful scratch cleanup with preserved dir/worktree workspaces
- [x] Garbage collection for scratch workspaces, old events, and logs

### Dispatcher resilience

- [x] Claude/Codex process launch, bounded parallelism, logs, terminal-call guard
- [x] Claim TTL and safe stale-claim reclaim with live-PID termination deferral
- [x] Worker PID tracking, crash detection, and heartbeat-stale detection
- [x] Maximum-runtime termination and rate-limit-neutral cooldown/requeue
- [x] Board-wide and per-assignee concurrency limits
- [x] Spawn/protocol failure classification and respawn guards
- [x] Active-worker, backlog, and diagnostics snapshots
- [x] Explicit active-run inspection and termination control

### Agent and human surfaces

- [x] Core MCP planning and worker lifecycle tools
- [x] Scoped worker isolation and portable worker/orchestrator Skills
- [x] Attachment MCP tools and attachment-aware task detail
- [x] Bounded, preformatted worker context with parent handoffs and prior attempts
- [x] CLI verbs for boards, tasks, claim/heartbeat, runs, events, logs, stats, diagnostics, and dry-run dispatch
- [x] Terminal event watch/tail and machine-readable output
- [x] Notification subscriptions and leased terminal-event delivery adapters

### Orchestration

- [x] Manual and bounded automatic triage specification
- [x] Atomic task-graph decomposition with profile/runtime routing and fallback
- [x] Configurable automatic promotion of unblocked decomposition children
- [x] Kanban Swarm blackboard/worker/verifier/synthesizer topology helper
- [x] Per-task skill guidance injected into Claude/Codex workers
- [x] Goal-mode continuation and completion judgment

### Dashboard

- [x] Local token-authenticated HTTP API over the shared board kernel
- [x] Kanban columns, search/filter, archived toggle, and Running profile lanes
- [x] Create/edit drawer, dependencies, comments, runs, attachments, and events
- [x] Drag/drop and bulk status/assignee/archive/delete operations
- [x] Atomic dashboard manual start, card progress badges, and guarded trash drop
- [x] Board switcher/settings and specify/decompose/swarm controls
- [x] WebSocket event stream with reconnect/cursor support

## Explicit upstream boundary

Like Hermes Kanban, this project is single-host. Cross-host SQLite sharing is
not supported. Messaging-platform slash commands are integration adapters, not
board-kernel behavior; this standalone implementation exposes the same actions
through MCP, CLI, and its HTTP API so a platform adapter can call them without
duplicating business logic.
