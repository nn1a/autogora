# TaskCircuit

A local, durable agent work control plane for Claude Code, Codex, Cline, and Gemini CLI. Claude and Codex use dispatcher-injected MCP; MCP-disabled Cline builds and isolated Gemini worker runs use a scoped CLI bridge. TaskCircuit provides SQLite-backed tasks, dependencies, comments, atomic claims, scoped claim tokens, heartbeat, completion/blocking, bounded retries, planning, a dispatcher, and an authenticated Web UI.

한국어 안내는 [설치 및 업그레이드](docs/INSTALL_KO.md)와 [Triage에서 Done까지의 실전 워크플로 가이드](docs/WORKFLOW_KO.md)를 참고하세요. Web UI 화면과 간단한 기능 구현, 코드 분석 후 문서화, 분석 → 구현 → 리뷰 예제를 포함합니다.

## Install

TaskCircuit is distributed as one native executable. It does not require Node.js,
npm, Bun, Go, a separate SQLite library, or a Web UI installation at runtime.
Download the archive for your OS and architecture from
[GitHub Releases](https://github.com/nn1a/kanban/releases), verify it with
`checksums.txt`, extract it, and place `taskcircuit` on `PATH`.

Linux and macOS example:

```bash
tar -xzf taskcircuit_<version>_<platform>_<architecture>.tar.gz
sudo install -m 0755 taskcircuit_<version>_<platform>_<architecture>/taskcircuit /usr/local/bin/taskcircuit
taskcircuit version
taskcircuit init
```

Use the `linux_musl_amd64` or `linux_musl_arm64` archive when an explicitly
labelled Alpine/musl artifact is preferred. Linux release binaries are built
with `CGO_ENABLED=0`, are statically linked, and have no glibc or musl runtime
dependency. See the [Korean install guide](docs/INSTALL_KO.md) for Windows,
upgrades, source builds, and data locations.

Claude Code, Codex, Cline, and Gemini CLI are needed only for the worker or
planner runtimes you actually select.

## Connect an MCP client

Resolve the installed executable once so the client receives an absolute path:

```bash
TASKCIRCUIT_BIN=$(command -v taskcircuit)
```

Connect Claude Code:

```bash
claude mcp add --scope local taskcircuit -- \
  "$TASKCIRCUIT_BIN" serve --db "$PWD/data/taskcircuit.db"
```

Connect Codex:

```bash
codex mcp add taskcircuit -- \
  "$TASKCIRCUIT_BIN" serve --db "$PWD/data/taskcircuit.db"
```

Connect Gemini CLI for interactive MCP use:

```bash
gemini mcp add --scope project taskcircuit "$TASKCIRCUIT_BIN" serve -- \
  --db "$PWD/data/taskcircuit.db"
```

The equivalent checked-in examples are [examples/claude.mcp.json](examples/claude.mcp.json), [examples/codex.config.toml](examples/codex.config.toml), the [MCP-disabled Cline CLI bridge contract](examples/cline-cli-bridge.md), and the [Gemini CLI runtime guide](examples/gemini-cli.md).

For an MCP-disabled Cline build, no Cline MCP configuration is required. Point
the dispatcher at the executable when it is not named `cline`:

```bash
export TASKCIRCUIT_CLINE_BIN=/absolute/path/to/modified-cline
taskcircuit create "Inspect the Cline integration" \
  --assignee cline-worker --runtime cline --workspace "$PWD"
taskcircuit dispatch --once
```

The dispatcher launches Cline with `--json`, `--cwd`, and `--auto-approve` and
puts the exact scoped TaskCircuit CLI commands in the worker prompt. The child
inherits `TASKCIRCUIT_TASK_ID`, `TASKCIRCUIT_RUN_ID`,
`TASKCIRCUIT_CLAIM_TOKEN`, `TASKCIRCUIT_BOARD`, and `TASKCIRCUIT_DB`;
lifecycle commands validate that scope before changing state. The Cline build
therefore needs shell-command support, but it does not need an MCP client.

Gemini dispatcher runs do not modify `.gemini/settings.json`. Set a custom
binary when necessary, create a routed task, and dispatch it normally:

```bash
export TASKCIRCUIT_GEMINI_BIN=/absolute/path/to/gemini
taskcircuit create "Inspect the Gemini integration" \
  --assignee gemini-worker --runtime gemini --workspace "$PWD"
taskcircuit dispatch --once
```

The dispatcher uses Gemini headless `stream-json` output and a temporary,
run-scoped policy. Read-only runs allow Gemini's normal read/search tools, deny
MCP tools and all shell commands except the exact TaskCircuit lifecycle bridge.
`--allow-writes` is the explicit opt-in to Gemini `yolo` approval mode.

## Web dashboard and HTTP API

Start the embedded local dashboard:

```bash
taskcircuit dashboard
```

The command binds to `127.0.0.1:8420` and prints a bootstrap URL containing a
random 256-bit token. Opening it once exchanges the query token for a strict,
HTTP-only session cookie and redirects to a clean URL. Every static asset, REST
request, attachment download, and event-stream connection requires that cookie or an
`Authorization: Bearer <token>` header. Use `--host`, `--port`, or `--token` to
override the defaults; do not expose a non-loopback bind without an external
TLS/reverse-proxy boundary.

The dashboard includes:

- responsive light/dark presentation with consistent controls and explicit
  task status, owner, runtime, and board-health cues;
- all lifecycle columns, search, tenant/assignee filters, archived visibility,
  and optional per-profile Running lanes;
- create/edit drawers, safe Markdown rendering, dependencies, comments, run
  history and termination, attachments, and recent events;
- progress/comment/link badges, drag/drop transitions, atomic manual starts,
  a guarded trash target, and partial-failure bulk move, assign, archive, and
  delete actions;
- isolated board switching/creation/settings, persisted profile routing and
  opt-in auxiliary profile descriptions, automatic decomposition settings,
  manual specify/decompose, swarm creation, and dispatcher nudging;
- multi-file attachment upload and persisted archived/profile-lane preferences;
- a cursor-based Server-Sent Events stream with reconnect and debounced board/drawer
  refresh.

The JSON API lives under `/api/` and mirrors the same board kernel used by MCP
and the CLI. For example:

```bash
curl -H "Authorization: Bearer $TASKCIRCUIT_DASHBOARD_TOKEN" \
  "http://127.0.0.1:8420/api/board?board=default"
```

## Try a task

Create one from the shell:

```bash
taskcircuit create "Inspect the authentication module" \
  --body "Document the flow and verify it against existing tests." \
  --assignee reviewer \
  --runtime codex \
  --workspace "$PWD"
```

Run one worker in read-only mode:

```bash
taskcircuit dispatch --once
```

For a trusted coding workspace, explicitly allow writes:

```bash
taskcircuit dispatch --once --allow-writes
```

Run a persistent local dispatcher with up to two workers:

```bash
taskcircuit dispatch --watch --max-workers 2 --allow-writes
```

Long-running dispatchers persist claim TTLs, heartbeats, worker PIDs, and task
runtime limits. They recover dead or stale workers, terminate tasks that exceed
`max_runtime_seconds`, and treat exit code 75 as a retry-neutral provider rate
limit. Optional `--max-in-progress` and `--max-per-assignee` caps coordinate
multiple dispatcher processes through the database.

Worker output is stored next to the database under `data/logs/`.

Automation-friendly task fields are available through both the CLI and MCP:

```bash
taskcircuit create "Nightly security audit" \
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

Goal-mode cards run differently from ordinary one-shot work. After a worker
turn exits without a terminal lifecycle call, an independent structured-output judge
checks the card's title/body acceptance criteria. An incomplete card resumes
the same Claude/Codex/Gemini session with the judge's next instruction. Stock Cline's
headless JSON mode does not support prompt-based `--id` resume, so Cline goals
continue in a fresh turn using the same workspace and durable TaskCircuit handoff.
Acceptance completes the task; exhausting `goal_max_turns` blocks it for human
review.

## Multiple boards

Named boards isolate their database, workspaces, attachments, and logs. The
`default` board retains `data/taskcircuit.db`; named boards live under
`data/boards/<slug>/`.

```bash
taskcircuit boards create project-api \
  --name "Project API" --default-workdir "$PWD" --switch
taskcircuit boards list
taskcircuit boards show
taskcircuit boards rename project-api "Project API v2"
taskcircuit boards rm project-api       # recoverable archive
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
taskcircuit attach <task-id> ./requirements.pdf
taskcircuit attach-url <task-id> https://example.com/design --name "Design"
taskcircuit attachments <task-id>
```

Workers can declare relative deliverables in `taskcircuit_complete` through its
`artifacts` array. Every path must exist before the task can become `done`; the
server copies valid artifacts into durable storage and records them in run
metadata.

## Observe and maintain a board

The CLI and MCP expose the same bounded worker context, attempt history, event
cursor, counts, and health diagnostics used by the dispatcher:

```bash
taskcircuit context <task-id>
taskcircuit runs <task-id>
taskcircuit log <task-id> --tail-bytes 65536
taskcircuit stats
taskcircuit diagnostics
taskcircuit watch --since 0
taskcircuit tail <task-id> --follow
```

Scripts can also claim a ready task and keep its lease alive through the same
atomic run kernel used by the dispatcher:

```bash
taskcircuit claim <task-id> --ttl 900
taskcircuit heartbeat <task-id> --note "verification in progress"
taskcircuit complete <task-id> --summary "verified and delivered"
```

`claim` prepares and prints the resolved workspace plus the scoped claim token.
Use `reassign <id>... <profile>` for partial-failure bulk routing, and
`list --mine` with `TASKCIRCUIT_PROFILE` or `TASKCIRCUIT_WORKER_ID` for
profile-local views.

Bulk mutations always report success or failure for each task instead of
aborting the whole batch. Garbage collection is board-scoped and only removes
expired events, log files, and old scratch directories that still map to a
terminal task; preserved directories and worktrees are left untouched.

```bash
taskcircuit bulk <task-a> <task-b> --assignee reviewer --priority 10
taskcircuit block <task-a> "needs review" --ids <task-b> --ids <task-c>
taskcircuit dispatch --dry-run --max 3
taskcircuit gc --event-retention-days 30 --log-retention-days 30
```

## Notifications

Task-scoped subscriptions follow the Hermes platform/chat/thread model and
default to `completed`, `blocked`, `gave_up`, `crashed`, and `timed_out`
events. The dispatcher polls and delivers them in the background; a one-shot
delivery pass is also available for cron and diagnostics.

The standalone bundled adapter uses `platform=webhook` with the endpoint URL in
`--chat-id`. An optional secret signs the exact JSON body with HMAC-SHA256 in
`X-TaskCircuit-Signature`. Delivery leases prevent concurrent dispatchers from
claiming the same event, failures use bounded backoff, and a completion or
archive removes the subscription automatically.

```bash
taskcircuit notify-subscribe <task-id> \
  --platform webhook --chat-id https://example.com/hooks/kanban \
  --thread-id release --secret "$TASKCIRCUIT_WEBHOOK_SECRET"
taskcircuit notify-list <task-id>
taskcircuit notify-deliver
taskcircuit notify-unsubscribe <task-id> \
  --platform webhook --chat-id https://example.com/hooks/kanban \
  --thread-id release
```

Additional messaging platforms can register the exported notification adapter
interface without changing the board kernel. Stored secrets are never returned
by CLI or MCP reads.

## Triage and orchestration

`specify` turns a rough `triage` card into a scoped task with deliverables,
acceptance criteria, constraints, and verification. `decompose` asks Claude,
Codex, Cline, or Gemini for a structured acyclic graph, validates every route, substitutes a
configured fallback for unknown assignees, and applies all children, links, and
root changes in one SQLite transaction. If fan-out adds no value, decomposition
falls back to specification.

Codex and Claude receive their native structured-output schema flags. Cline and
Gemini receive the schema in the planner prompt. The dispatcher extracts Cline's
final `run_result`/`done` NDJSON text or Gemini's headless JSON `response`, parses
it as JSON, and applies the same domain validation before any board mutation.
Gemini planners also receive a temporary deny-all tool policy.

```bash
taskcircuit specify <triage-id> --planner-runtime codex
taskcircuit specify <triage-id> --planner-runtime cline
taskcircuit specify <triage-id> --planner-runtime gemini
taskcircuit decompose <triage-id> \
  --profile "researcher:codex:finds primary sources" \
  --profile "writer:claude:synthesizes verified reports" \
  --profile "reviewer:gemini:checks the implementation through Gemini CLI" \
  --default-profile researcher:codex \
  --orchestrator-profile writer:claude
```

For deterministic automation, `specify` accepts `--title` plus `--body`, and
`decompose` accepts a validated `--plan-json`. New boards enable bounded
automatic triage processing by default, capped at three cards per dispatcher
tick. Change it in the board's dashboard settings; command-line dispatcher
overrides include `--auto-decompose` and `--auto-decompose-per-tick`.
Boards can also disable automatic child promotion so every newly decomposed
leaf remains in `todo` for a human routing review.

The swarm helper creates a completed structured blackboard, parallel worker
cards, a verifier gated on every worker, and a synthesizer gated on the
verifier:

```bash
taskcircuit swarm "Design a multi-region failover plan" \
  --workers researcher:codex,architect:claude,sre:gemini \
  --verifier reviewer:claude --synthesizer writer:claude
```

## MCP tools

The product, package, CLI, and MCP registration name are `TaskCircuit`/`taskcircuit`.
All MCP tools and worker environment variables use the `taskcircuit_*` and
`TASKCIRCUIT_*` prefixes respectively.

- Planning: `taskcircuit_create`, `taskcircuit_list`, `taskcircuit_show`, `taskcircuit_update`, `taskcircuit_comment`, `taskcircuit_link`, `taskcircuit_unlink`
- Relationships: `taskcircuit_graph`, `taskcircuit_subtask_set`, `taskcircuit_subtask_remove`
- Boards: `taskcircuit_boards_list`, `taskcircuit_boards_create`, `taskcircuit_boards_update`, `taskcircuit_boards_switch`, `taskcircuit_boards_remove`
- Dispatch: `taskcircuit_claim`
- Worker lifecycle: `taskcircuit_heartbeat`, `taskcircuit_complete`, `taskcircuit_block`
- Attachments: `taskcircuit_attach`, `taskcircuit_attach_url`, `taskcircuit_attachments`, `taskcircuit_attachment_remove`
- Observability: `taskcircuit_context`, `taskcircuit_stats`, `taskcircuit_diagnostics`, `taskcircuit_events`, `taskcircuit_runs`, `taskcircuit_log`
- Administration: `taskcircuit_run_terminate`, `taskcircuit_bulk`, `taskcircuit_gc`
- Notifications: `taskcircuit_notify_subscribe`, `taskcircuit_notify_list`, `taskcircuit_notify_unsubscribe`, `taskcircuit_notify_deliver`
- Orchestration: `taskcircuit_specify`, `taskcircuit_decompose`, `taskcircuit_profile_describe_auto`, `taskcircuit_swarm`
- Human recovery: `taskcircuit_unblock`, `taskcircuit_promote`, `taskcircuit_schedule`, `taskcircuit_archive`, `taskcircuit_delete`

Dispatcher-launched workers receive board, task, run, and claim-token scope
through environment variables. Their lifecycle tools can omit those identifiers,
and the server rejects attempts to operate on another scoped board, task, or
run. Without `--board`, the dispatcher sweeps all active boards while preserving
the global worker limit.

TaskCircuit keeps two relation types separate:

- parent task/subtask hierarchy records which goal owns a unit of work;
- prerequisite/dependent links form the acyclic execution DAG and gate claims.

Dependency completion is stored on each edge as a durable handoff. Archiving or
reopening a completed prerequisite does not retroactively invalidate work that
already consumed that handoff. To require a fresh completion, unlink and relink
the dependency. An unfinished prerequisite cannot be attached to a task that is
already running; completed prerequisites may be attached without interrupting it.

`decompose` atomically records every generated task under the triage root, applies
the dependency DAG, and makes the root depend on all terminal subtasks. Use
`taskcircuit graph <task-id>` or `taskcircuit_graph` to inspect the combined topology
and topological phases. A worker receives the root goal, current node, completed
prerequisite handoffs, direct dependents, and a metadata-only phase map. Bodies,
workspaces, attachments, and unfinished results from other nodes are not copied
into worker context. The dispatcher still rechecks the dependency gate inside
the same transaction that claims a task.

Relationship responses remain bounded at 500 nodes. Larger connected graphs no
longer fail worker startup: TaskCircuit returns the exact total node and phase
counts, keeps the focus/root/direct neighborhood, and marks the response as
`truncated` with an `omittedNodeCount`.

Administrative completion, blocking, archiving, deletion, and ownership or
workspace moves reject a task with an active run. Use
`taskcircuit terminate <task-id>` or `taskcircuit_run_terminate` first; this signals the recorded worker PID
and reclaims the run. A live PID returns `pending: true` and remains `running`
until the dispatcher observes process exit, preventing an old and a replacement
worker from overlapping. A missing or already-dead PID is reclaimed immediately.
Title, body, and priority clarifications remain editable during a run, while
workspace and branch identity stay fixed.

## Skills

The portable Agent Skills are under `skills/`:

- `taskcircuit-worker`: execute and close one claimed task
- `taskcircuit-orchestrator`: create an executable dependency graph

Install them into the client you use:

```bash
cp -R skills/taskcircuit-worker skills/taskcircuit-orchestrator ~/.agents/skills/
cp -R skills/taskcircuit-worker skills/taskcircuit-orchestrator ~/.claude/skills/
```

Restart the client if it does not detect the new skills.

## Safety and scope

- `--allow-writes` grants a spawned coding worker workspace edits and shell access. Use only in repositories you trust. Read-only Cline runs use the dispatcher approval broker; read-only Gemini runs use a temporary policy. Both permit only their normal read/search tools and the scoped TaskCircuit CLI lifecycle bridge.
- The MCP server is local stdio only; there is no multi-user isolation.
- The optional dashboard is authenticated but remains a trusted-local-user,
  single-tenant surface; its bearer token is not a substitute for TLS on an
  untrusted network.
- SQLite and PID recovery assume one host; cross-host dispatch is intentionally unsupported.
- Messaging-platform slash commands remain adapter concerns; every board action
  they need is available through the shared MCP, CLI, or authenticated HTTP
  kernel. See `docs/HERMES_PARITY.md` for the audited checklist.
