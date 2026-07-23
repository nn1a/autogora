# Autogora

A local, durable agent work control plane for Claude Code, Codex, Cline, and
Gemini CLI. Claude and Codex use dispatcher-injected MCP; MCP-disabled Cline
builds and isolated Gemini workers use a scoped CLI bridge. Autogora combines
SQLite-backed task orchestration, atomic worker ownership, a terminal board,
and an authenticated Web UI in one executable.

Korean documentation: [installation and upgrades](docs/INSTALL_KO.md) · [practical workflow from Triage to Done](docs/WORKFLOW_KO.md)

## Install

Autogora is currently pre-release. Build it from source with Go 1.25 or later:

```bash
git clone https://github.com/nn1a/autogora.git
cd autogora
make build
./bin/autogora version
sudo install -m 0755 ./bin/autogora /usr/local/bin/autogora
autogora version
autogora init
```

The resulting native executable embeds the TUI, Web UI, and SQLite engine, so
it needs no separate database server or Web asset installation. Claude Code,
Codex, Cline, and Gemini CLI are needed only for the worker, planner, or judge
routes you select.

Future tagged versions are expected to publish archives and `checksums.txt` on
[GitHub Releases](https://github.com/nn1a/autogora/releases). The release build
script already produces Linux, explicitly labelled Linux/musl, macOS, and
Windows archives. Linux artifacts use `CGO_ENABLED=0` and do not dynamically
depend on glibc or musl. Until a release exists, use the source build above.
See the [Korean install guide](docs/INSTALL_KO.md) for verification, data
locations, and the release build procedure.

Release builds strip paths, symbols, and the Go build ID; selectively suppress
inlining in the internal, Charmbracelet, MCP, and JSON Schema package trees;
enforce a 16 MiB binary budget; and use `gzip -9n` so gzip headers contain no
source name or timestamp. They do not use UPX or global inlining suppression.

## Project data location

Autogora keeps mutable project state outside the Git worktree by default. A
readable project name plus a hash of Git's common directory gives clones
separate state while all linked worktrees of one clone share it. Run this from
any directory in the project to inspect the exact paths without creating them:

```bash
autogora paths
```

The native roots are `$XDG_DATA_HOME/autogora` (or
`~/.local/share/autogora`) on Linux,
`~/Library/Application Support/autogora` on macOS, and
`%LOCALAPPDATA%\autogora` on Windows. `AUTOGORA_DATA_HOME` can replace the
app-data root with an absolute path.

To deliberately keep state in the repository directory, initialize an ignored
hidden directory:

```bash
autogora init --data-dir .autogora
```

Autogora writes `.autogora/.gitignore` so the database, WAL files, logs,
attachments, and scratch workspaces do not affect Git. It rejects `.git`
internal paths. `autogora init --reset-data-dir` selects and initializes the
native default again; changing locations never moves or deletes existing data.
An explicit `--db` or `AUTOGORA_DB` remains the highest-priority per-command
override. Moving the repository itself produces a new path-based project ID;
reconnect the old state with
`autogora init --data-dir /absolute/previous/dataRoot` when that is intended.

## Configure coding agents and automatic orchestration

The dashboard opens the Agents dialog on the first visit when no global agent
configuration exists. Its detection is intentionally narrow: it resolves the
supported CLI names through `PATH` and runs only `--version`. It does not send
a prompt, contact a paid model, verify login state, or check subscription and
quota availability. Confirm those conditions in each coding-agent CLI before
enabling unattended work.

The dialog and the `agents` subcommand write the same global `config.json`.
Use `autogora agents path` to locate it. The file contains routing metadata,
not credentials: agent ID, runtime, executable, model, provider, worker/planner/
judge roles, fallback order, per-agent concurrency, preferred role order, and
supervisor settings.

```bash
# Inspect first; --save adds detected CLIs to the registry.
autogora agents detect
autogora agents detect --save

autogora agents set claude-backup \
  --runtime claude --model <model-id> \
  --roles worker,planner,judge
autogora agents set codex-primary \
  --runtime codex --model <model-id> \
  --roles worker,planner,judge \
  --fallbacks claude-backup --max-concurrent 2
autogora agents defaults \
  --worker codex-primary,claude-backup \
  --planner codex-primary,claude-backup \
  --judge claude-backup,codex-primary
autogora agents supervisor \
  --auto-start=true --max-workers 2 --allow-writes=true
```

With `auto-start` enabled, `autogora dashboard` and `autogora tui` start the
in-process supervisor. A separate `dispatch --watch` process is unnecessary.
The dashboard can also start or stop its supervisor from the Agents dialog.
Use `dispatch --once`, `dispatch --watch`, or `dispatch --dry-run` when you need
a separate CLI-managed runner.

The dashboard's **Run now** action follows the saved **Allow workspace
changes** policy (`allowWrites`). **Automation & activity** shows the 20 most
recent Web UI dispatch operations. The server retains every in-flight operation
and up to 100 terminal results until that dashboard process stops.

Board profiles refine the global registry for one board. A matching board
profile can pin a different model or provider, describe and prioritize the
route, choose fallbacks, disable it, or lower its concurrency. It cannot change
the globally registered runtime or command, enable a globally disabled agent,
or raise the global concurrency cap. A board may also add a board-only route.

## Set up an agent client

Autogora embeds its worker and coordinator Skills and can register its stdio
MCP server through each client's native CLI. Preview both changes, then apply
them from the project that will own the board:

```bash
autogora setup --client codex --dry-run
autogora setup --client codex
```

Use `claude`, `gemini`, repeated `--client` options, or `--client all` as
needed. Skills default to project scope. MCP uses each client's safe native
default: Codex user, Claude local, and Gemini project. Inspect either half with
`autogora skills status --client codex` or
`autogora mcp status --client codex`. Run `autogora help setup`,
`autogora help skills`, or `autogora help mcp` for scope and recovery options.

For an MCP-disabled Cline build, skip `setup`: the dispatcher uses the scoped
CLI bridge described below.

### Manual MCP registration

Resolve the installed executable once so the client receives an absolute path:

```bash
AUTOGORA_BIN=$(command -v autogora)
autogora paths  # copy the printed "database" value below
AUTOGORA_DB=/absolute/path/printed/by/autogora/paths
```

Connect Claude Code:

```bash
claude mcp add --scope local autogora -- \
  "$AUTOGORA_BIN" serve --db "$AUTOGORA_DB"
```

Connect Codex:

```bash
codex mcp add autogora -- \
  "$AUTOGORA_BIN" serve --db "$AUTOGORA_DB"
```

Connect Gemini CLI for interactive MCP use:

```bash
gemini mcp add --scope project autogora "$AUTOGORA_BIN" serve -- \
  --db "$AUTOGORA_DB"
```

The equivalent checked-in examples are [examples/claude.mcp.json](examples/claude.mcp.json), [examples/codex.config.toml](examples/codex.config.toml), the [MCP-disabled Cline CLI bridge contract](examples/cline-cli-bridge.md), and the [Gemini CLI runtime guide](examples/gemini-cli.md).

For an MCP-disabled Cline build, no Cline MCP configuration is required. Point
the dispatcher at the executable when it is not named `cline`:

```bash
export AUTOGORA_CLINE_BIN=/absolute/path/to/modified-cline
autogora create "Inspect the Cline integration" \
  --assignee cline-worker --runtime cline --workspace "$PWD"
autogora dispatch --once
```

The dispatcher launches Cline with `--json`, `--cwd`, and `--auto-approve` and
puts the exact scoped Autogora CLI commands in the worker prompt. The child
inherits `AUTOGORA_TASK_ID`, `AUTOGORA_RUN_ID`,
`AUTOGORA_CLAIM_TOKEN`, `AUTOGORA_BOARD`, and `AUTOGORA_DB`;
lifecycle commands validate that scope before changing state. The Cline build
therefore needs shell-command support, but it does not need an MCP client.
Profiles may also pin Cline's model and provider. A modified build must accept
the standard `--model` and `--provider` arguments before it can use those
fields.

Gemini dispatcher runs do not modify `.gemini/settings.json`. Set a custom
binary when necessary, create a routed task, and dispatch it normally:

```bash
export AUTOGORA_GEMINI_BIN=/absolute/path/to/gemini
autogora create "Inspect the Gemini integration" \
  --assignee gemini-worker --runtime gemini --workspace "$PWD"
autogora dispatch --once
```

The dispatcher uses Gemini headless `stream-json` output and a temporary,
run-scoped policy. Read-only runs allow Gemini's normal read/search tools, deny
MCP tools and all shell commands except the exact Autogora lifecycle bridge.
`--allow-writes` is the explicit opt-in to Gemini `yolo` approval mode.

## Terminal board

Open the current board without starting a web server:

```bash
autogora tui
# or select one explicitly
autogora tui --board product-web
```

The full-screen TUI adapts its visible columns and detail panel to the terminal
width. It refreshes every two seconds while preserving the selected task.
Use the arrow keys or `h/j/k/l` to navigate, `/` to search, `f` to filter by
tenant, assignee, or runtime, `tab` to switch between overview, relationships,
and activity, and `a` to include archived tasks. Press `?` for the complete key
map. When the global supervisor has `autoStart` enabled, opening the TUI also
starts orchestration with its configured worker and write limits.

The create and edit form has Task, Agent, and Execution sections. Task creation
includes Status and Run after; editing keeps status and scheduling in the action
palette. Both forms cover title, description, priority, assignee, runtime,
tenant, workspace kind/path, branch, skills, maximum runtime and retries, and
goal-mode turn limit. The profile list uses the same effective global and board
routes as the Web API; choosing one fills assignee and runtime and shows its
pinned model or `CLI default`. Use `tab` and `shift+tab` between fields,
`ctrl+left/right` between sections, and the up/down arrows to change selection
fields. Press `space` to toggle Goal mode and `ctrl+s` to validate and save. The
focused selection field repeats the relevant key hint.

Press `space` for the searchable task action palette. It includes status moves,
Specify, Decompose, Promote, Unblock, targeted dispatcher runs, active-run termination,
scheduling, completion and blocking, hierarchy and dependency edits, comments,
file/URL attachments, archive, and delete. These actions use the same in-process
task service as the dashboard. Destructive and planner actions remain pinned to
the displayed task ID while the board refreshes and require confirmation. The
TUI does not bypass active-run ownership, dependency, claim, or lifecycle rules.

## Web dashboard and HTTP API

Start the embedded local dashboard:

```bash
autogora dashboard
```

The command binds to `127.0.0.1:8420` and prints a bootstrap URL containing a
random 256-bit token. Opening it once exchanges the query token for an
HTTP-only session cookie with `SameSite=Strict`, then redirects to a clean URL.
Every static asset, REST request, attachment download, and event-stream
connection requires that cookie or an `Authorization: Bearer <token>` header.
Use `--host`, `--port`, or `--token` to override the defaults; do not expose a
non-loopback bind without an external TLS/reverse-proxy boundary.

The dashboard includes:

- responsive light/dark presentation with consistent controls and explicit
  task status, owner, runtime, and board-health cues;
- Planning and Execution stages that each show four equal lifecycle columns in
  one overview without page-level horizontal scrolling, then adapt to two or
  one column on narrower screens;
- search, tenant/assignee filters, archived visibility, and optional
  per-profile Running lanes;
- bounded 500-task board snapshots with a returned/total warning and a filtered
  task-list API for larger boards;
- first-run coding-agent setup, safe PATH/`--version` detection, effective
  profile and health views, and supervisor start/stop controls;
- GitHub and GitHub Enterprise issue preview/import through the authenticated
  `gh` CLI, with duplicate protection and partial-result reporting;
- an Automation & activity view for supervisor state, agent cooldowns, active-run heartbeat
  and lease age, one-shot dispatch operations, diagnostics, and durable events;
- create/edit drawers, safe Markdown rendering, dependencies, comments, run
  history and termination, attachments, recent events, complete execution
  settings, and optimistic conflict detection for edits and lifecycle actions;
- progress/comment/link badges, drag/drop transitions, targeted dispatcher runs,
  a guarded trash target, and version-checked, partial-failure bulk move, assign,
  archive, and delete actions;
- isolated board switching/creation/settings, persisted profile routing and
  opt-in auxiliary profile descriptions, automatic decomposition settings,
  manual specify/decompose, swarm creation, and dispatcher nudging;
- multi-file attachment upload and persisted archived/profile-lane preferences;
- a cursor-based Server-Sent Events stream with reconnect and debounced board/drawer
  refresh.

The JSON API lives under `/api/` and mirrors the same board kernel used by MCP
and the CLI. For example:

```bash
curl -H "Authorization: Bearer $AUTOGORA_DASHBOARD_TOKEN" \
  "http://127.0.0.1:8420/api/board?board=default"
```

## Try a task

Create one from the shell:

```bash
autogora create "Inspect the authentication module" \
  --body "Document the flow and verify it against existing tests." \
  --assignee reviewer \
  --runtime codex \
  --workspace "$PWD"
```

Run one worker in read-only mode when the supervisor is not enabled:

```bash
autogora dispatch --once
```

Import open GitHub issues directly into Triage through the authenticated `gh`
CLI. Repeating an import returns the existing active task instead of creating a
duplicate; the source URL is retained in both the task body and attachments.
Imported titles and bodies remain untrusted external input and are never
automatically decomposed. Review each card, then choose Specify, Decompose, or
Promote. The dashboard's **Import issues** dialog provides the same preview and
import flow.

```bash
autogora github import --repo nn1a/autogora --label bug --limit 20
autogora github import --repo nn1a/autogora --issue 42 --dry-run
```

GitHub Enterprise Server uses the same command with an explicit host. Autogora
passes the fully qualified repository to `gh`, so credentials, custom TLS, and
host selection remain under GitHub CLI configuration.

```bash
gh auth login --hostname github.corp.example
autogora github import \
  --host github.corp.example \
  --repo platform/control \
  --tenant platform
```

For a trusted coding workspace, explicitly allow writes:

```bash
autogora dispatch --once --allow-writes
```

Run a persistent local dispatcher with up to two workers when you want an
explicit process instead of the Web/TUI supervisor:

```bash
autogora dispatch --watch --max-workers 2 --allow-writes
```

Long-running dispatchers persist claim TTLs, heartbeats, worker PIDs, and task
runtime limits. They recover dead or stale workers, terminate tasks that exceed
`max_runtime_seconds`, and treat exit code 75 as a retry-neutral provider rate
limit. Optional `--max-in-progress` and `--max-per-assignee` caps coordinate
multiple dispatcher processes through the database. A registered agent's
`maxConcurrent` cap is shared by workers, planners, and judges on every board,
and by every Autogora process using the same data root. A capacity-full worker
is requeued without consuming a retry; a planner or judge tries its next
configured fallback. This prevents separate dashboards and dispatchers from
overcommitting one subscription or CLI.

When one dispatcher watches multiple boards, worker-claim and planning passes
rotate their starting board. A backlog on the default board therefore cannot
starve another board.

Worker launch and output can mark a profile `missing`, `auth_required`, or
`rate_limited`. The dispatcher skips an unavailable profile and
follows its explicit fallback chain, recording the selected source and
`fallbackFrom` in run history. Rate limits use a cooldown and do not consume a
task retry. Detection itself never asserts these health states; they come from
real execution outcomes. If an unsuccessful worker has already changed or
committed files, Autogora does not retry or start a fallback on top of that
work. It blocks the task with the preserved workspace path for human review.

Worker output is stored under the resolved board `logsRoot` shown by
`autogora paths`.

Automation-friendly task fields are available through both the CLI and MCP:

```bash
autogora create "Nightly security audit" \
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
continue in a fresh turn using the same workspace and durable Autogora handoff.
Acceptance completes the task; exhausting `goal_max_turns` blocks it for human
review.

## Multiple boards

Named boards isolate their database, workspaces, attachments, and logs. The
default board uses `<data-root>/autogora.db`; named boards live under
`<data-root>/boards/<slug>/`. `autogora paths --board <slug>` shows the exact
locations.

```bash
autogora boards create project-api \
  --name "Project API" --default-workdir "$PWD" --switch
autogora boards list
autogora boards show
autogora boards rename project-api "Project API v2"
autogora boards rm project-api       # recoverable archive
```

Use `boards rm <slug> --delete` only when permanent removal is intended. The
`default` board cannot be removed. Archive and hard delete both refuse a board
that still owns a running task, agent slot, or local/global workspace lease.
Close any TUI or other client that still has the board store open before
retrying. Removal barriers prevent a concurrent claim or write during the
filesystem move. An archived slug keeps a coordination tombstone until a new
board with that slug has been fully created.

## Workspaces

- `scratch` (default): isolated per run under the board workspace root and removed only
  after successful completion and artifact capture.
- `dir:/absolute/path`: uses and preserves an existing trusted directory.
  Writable runs take an exclusive SQLite lease on its canonical path, or on the
  containing Git repository, until the run ends. This lease is shared across
  every board that uses the same Autogora data root. A competing run is
  rescheduled without consuming a retry; read-only runs remain concurrent.
- `worktree`: creates and preserves a detached Git worktree per run under the
  board workspace root. Set a board `default-workdir` to the source repository.
  `--branch` selects an existing starting ref when available and remains the
  task's integration target; workers never move the shared branch directly.
- `worktree:/absolute/target`: pins the worktree destination explicitly.

Relative `dir:` and explicit worktree paths are rejected. Dispatcher runs record
their resolved path, repository, base commit, worker PID, and log path without
overwriting the task's workspace configuration.

Before finalizing a successful managed worktree completion, the dispatcher
snapshots its final tree with a temporary Git index. It does not stage the
worker index or move the user checkout. The snapshot is retained at
`refs/autogora/runs/<run-id>` and recorded in task details under `changeSets`
with base/head commits and changed files. A managed worktree cannot become Done
until this durable handoff exists.

Each satisfied prerequisite edge pins the exact completion run and change set
that the dependent must consume. Before starting a dependent in an isolated
worktree, Autogora validates those durable refs and merges all direct
prerequisite heads in deterministic order. It advances the dependent's
effective base only after the whole fan-in succeeds, so the dependent change
set reports its own files rather than repeating inherited parent changes.

A merge conflict, changed or foreign durable ref, or dropped prerequisite
history blocks the dependent for review without starting its worker or
increasing `failureCount`. Autogora aborts the merge and preserves the run
workspace. A shared `dir` workspace is accepted only when its current Git HEAD
already contains every required prerequisite head; use a worktree when
automatic fan-in is needed.

## Attachments and artifacts

Files are copied into the active board's durable attachment root and limited to
25 MB each. HTTP(S) references are stored without downloading them.

```bash
autogora attach <task-id> ./requirements.pdf
autogora attach-url <task-id> https://example.com/design --name "Design"
autogora attachments <task-id>
```

Workers can declare relative deliverables in `autogora_complete` through its
`artifacts` array. Every path must exist before the task can become `done`; the
server copies valid artifacts into durable storage and records them in run
metadata.

## Observe and maintain a board

The CLI and MCP expose the same bounded worker context, attempt history, event
cursor, counts, and health diagnostics used by the dispatcher:

```bash
autogora context <task-id>
autogora runs <task-id>
autogora log <task-id> --tail-bytes 65536
autogora stats
autogora diagnostics
autogora watch --since 0
autogora tail <task-id> --follow
```

Scripts can also claim a ready task and keep its lease alive through the same
atomic run kernel used by the dispatcher:

```bash
autogora claim <task-id> --ttl 900
# Copy task.id, run.id, and claimToken from the claim response.
export AUTOGORA_TASK_ID=<task-id>
export AUTOGORA_RUN_ID=<run-id>
export AUTOGORA_CLAIM_TOKEN=<claim-token>
autogora heartbeat "$AUTOGORA_TASK_ID" --note "verification in progress"
autogora complete "$AUTOGORA_TASK_ID" --summary "verified and delivered"
```

`claim` prepares and prints the resolved workspace plus the scoped claim token.
For a manually claimed worktree, CLI and MCP completion first capture and pin
the final change set, then finalize synchronously. If capture fails, Autogora
blocks the task and preserves the workspace instead of declaring it Done.
Dispatcher-managed runs use the two-phase rule described below.
Use `reassign <id>... <profile>` for partial-failure bulk routing, and
`list --mine` with `AUTOGORA_PROFILE` or `AUTOGORA_WORKER_ID` for
profile-local views.

Bulk mutations always report success or failure for each task instead of
aborting the whole batch. Garbage collection is board-scoped and only removes
expired events, log files, and old scratch directories that still map to a
terminal task; preserved directories and worktrees are left untouched.

```bash
autogora bulk <task-a> <task-b> --assignee reviewer --priority 10
autogora block <task-a> "needs review" --ids <task-b> --ids <task-c>
autogora dispatch --dry-run --max 3
autogora gc --event-retention-days 30 --log-retention-days 30
```

## Notifications

Task-scoped subscriptions use a platform/chat/thread destination model and
default to `completed`, `blocked`, `gave_up`, `crashed`, and `timed_out`
events. The dispatcher polls and delivers them in the background; a one-shot
delivery pass is also available for cron and diagnostics.

The standalone bundled adapter uses `platform=webhook` with the endpoint URL in
`--chat-id`. An optional secret signs the exact JSON body with HMAC-SHA256 in
`X-Autogora-Signature`. Delivery leases prevent concurrent dispatchers from
claiming the same event, failures use bounded backoff, and a completion or
archive removes the subscription automatically.

```bash
autogora notify-subscribe <task-id> \
  --platform webhook --chat-id https://example.com/hooks/autogora \
  --thread-id release --secret "$AUTOGORA_WEBHOOK_SECRET"
autogora notify-list <task-id>
autogora notify-deliver
autogora notify-unsubscribe <task-id> \
  --platform webhook --chat-id https://example.com/hooks/autogora \
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

The global agent registry is authoritative for executable availability,
runtime, command, role eligibility, preferred worker/planner/judge order, and
maximum concurrency. Per-board settings may add a board-only worker profile or
specialize a matching global profile as described above. A board-pinned planner
model/provider takes precedence; otherwise Autogora tries the preferred global
planner order and each configured fallback. It skips unhealthy or capacity-full
agents and records the selected route. Goal mode applies the same behavior to
the preferred global judges, then uses the resolved planner configuration when
no judge is configured.

Leaving a model blank is an intentional `CLI default (unpinned)` choice. The
dispatcher snapshots the resolved profile, runtime, requested model, provider,
configuration source, and `fallbackFrom` for every run, so later settings edits
do not rewrite execution history. The same values are available to workers as
`AUTOGORA_AGENT_PROFILE`, `AUTOGORA_MODEL`, and `AUTOGORA_PROVIDER`.

```bash
autogora specify <triage-id> --planner-runtime codex --planner-model <model-id>
autogora specify <triage-id> --planner-runtime cline
autogora specify <triage-id> --planner-runtime gemini
autogora decompose <triage-id> \
  --profile "researcher:codex:finds primary sources" \
  --profile "writer:claude:synthesizes verified reports" \
  --profile "reviewer:gemini:checks the implementation through Gemini CLI" \
  --default-profile researcher:codex \
  --finalizer-profile writer:claude
```

For deterministic automation, `specify` accepts `--title` plus `--body`, and
`decompose` accepts a validated `--plan-json`. New boards enable bounded,
asynchronous triage processing by default, capped at three cards per dispatcher
tick. Planning runs outside the worker lifecycle loop, so a slow planner does
not stop maintenance or Ready claims. Failures emit `auto_decompose_failed` and
use per-task exponential backoff from five seconds to five minutes. Imported
GitHub issues are excluded until a person explicitly reviews them. Review-gated
or cooling-down cards do not consume the planning quota, and paginated scans
continue to eligible cards behind a large import backlog. A planner that ignores
cancellation receives a bounded shutdown grace period instead of holding the
dispatcher open indefinitely. Change the policy in the board's dashboard
settings; command-line dispatcher overrides include `--auto-decompose` and
`--auto-decompose-per-tick`.
Boards can also disable automatic child promotion so every newly decomposed
leaf remains in `todo` for a human routing review.

`AUTOGORA_CODEX_MODEL`, `AUTOGORA_CLAUDE_MODEL`,
`AUTOGORA_CLINE_MODEL`, and `AUTOGORA_GEMINI_MODEL` provide process-level
defaults when a matching board profile does not pin a model. Cline also reads
`AUTOGORA_CLINE_PROVIDER`. A disabled profile is excluded from automatic
decomposition and dispatch; its queued cards remain visible in `Ready` until
the profile is enabled or the cards are reassigned.

The swarm helper creates a completed structured blackboard, parallel worker
cards, a verifier gated on every worker, and a synthesizer gated on the
verifier:

```bash
autogora swarm "Design a multi-region failover plan" \
  --workers researcher:codex,architect:claude,sre:gemini \
  --verifier reviewer:claude --synthesizer writer:claude
```

## MCP tools

The product, package, CLI, and MCP registration name are `Autogora`/`autogora`.
All MCP tools and worker environment variables use the `autogora_*` and
`AUTOGORA_*` prefixes respectively.

- Planning: `autogora_create`, `autogora_list`, `autogora_show`, `autogora_update`, `autogora_comment`, `autogora_link`, `autogora_unlink`
- Relationships: `autogora_graph`, `autogora_subtask_set`, `autogora_subtask_remove`
- Boards: `autogora_boards_list`, `autogora_boards_create`, `autogora_boards_update`, `autogora_boards_switch`, `autogora_boards_remove`
- Dispatch: `autogora_claim`
- Worker lifecycle: `autogora_heartbeat`, `autogora_complete`, `autogora_block`
- Attachments: `autogora_attach`, `autogora_attach_url`, `autogora_attachments`, `autogora_attachment_remove`
- Observability: `autogora_context`, `autogora_stats`, `autogora_diagnostics`, `autogora_events`, `autogora_runs`, `autogora_log`
- Administration: `autogora_run_terminate`, `autogora_bulk`, `autogora_gc`
- Notifications: `autogora_notify_subscribe`, `autogora_notify_list`, `autogora_notify_unsubscribe`, `autogora_notify_deliver`
- Orchestration: `autogora_specify`, `autogora_decompose`, `autogora_profile_describe_auto`, `autogora_swarm`
- Human recovery: `autogora_unblock`, `autogora_promote`, `autogora_schedule`, `autogora_archive`, `autogora_delete`

Dispatcher-launched workers receive board, task, run, and claim-token scope
through environment variables. Their lifecycle tools can omit those identifiers,
and the server rejects attempts to operate on another scoped board, task, or
run. Without `--board`, the dispatcher sweeps all active boards while preserving
the global worker limit.

For dispatcher-managed runs, `complete` and `block` record a terminal request
but keep the task and workspace lease in `running`. The dispatcher waits for the
worker process to exit successfully, captures the final artifacts, and only then
finalizes the run and unlocks dependents in one transaction. Pending and
finalized requests are visible in task details as `terminalRequests`. Goal-mode
completion requests are discarded until the independent judge accepts the goal.
Terminal writes retry transient SQLite lock failures and propagate any remaining
error instead of reporting success. Recovery writes the terminal run outcome and
the task's `blocked` state in one transaction, avoiding a briefly claimable task
after lease release. If a failed, timed-out, or canceled run already changed or
committed files, Autogora preserves its workspace for review and does not start
a retry or fallback on top of the partial work.

Autogora keeps two relation types separate:

- parent task/subtask hierarchy records which goal owns a unit of work;
- prerequisite/dependent links form the acyclic execution DAG and gate claims.

Dependency completion is stored on each edge as a durable handoff that pins the
exact completion run and, when present, its change set. A later rerun of the
prerequisite cannot silently replace what the dependent consumes. Archiving or
reopening a completed prerequisite does not retroactively invalidate the
handoff; unlink and relink the dependency to require a fresh completion. The
prerequisite set of a running dependent is immutable, and a prerequisite cannot
be deleted while such a run is active.

`decompose` atomically records every generated task under the triage root, applies
the dependency DAG, and makes the root depend on all terminal subtasks. Use
`autogora graph <task-id>` or `autogora_graph` to inspect the combined topology
and topological phases. A worker receives the root goal, current node, completed
direct prerequisite handoffs, direct dependents, and a metadata-only phase map.
Each handoff contains its pinned summary, metadata, and change-set provenance.
Bodies, workspaces, attachments, and unfinished results from other nodes are
not copied into worker context. The dispatcher still rechecks the dependency
gate inside the same transaction that claims a task.

Relationship responses remain bounded at 500 nodes. Larger connected graphs no
longer fail worker startup: Autogora returns the exact total node and phase
counts, keeps the focus/root/direct neighborhood, and marks the response as
`truncated` with an `omittedNodeCount`.

Administrative completion, blocking, archiving, deletion, and ownership or
workspace moves reject a task with an active run. Use
`autogora terminate <task-id>` or `autogora_run_terminate` first. Autogora
persists the request and signals a PID only when its OS process-start identity
still matches the worker recorded at spawn. A managed run returns `pending:
true` and remains `running` until the dispatcher observes process exit and
checks its workspace, including when the process is already missing. This
prevents overlap and preserves partial changes. A direct, unmanaged claim with
no live process can be reclaimed immediately. Execution settings are locked
during a run; only priority remains editable. Add durable
clarifications as comments, or terminate the run before changing its task spec.

## Skills

The portable Agent Skills are under `skills/`:

- `autogora-worker`: execute and close one claimed task
- `autogora-coordinator`: create and recover an executable dependency graph

Install both embedded Skills into the project (the default) and inspect them:

```bash
autogora skills install --client codex
autogora skills status --client codex
```

Codex and Gemini share `.agents/skills`; Claude uses `.claude/skills`. Add
`--scope user` for a user-wide installation. Autogora records hashes in each
installed Skill and refuses to overwrite locally modified or unmanaged files
unless `--force` is explicit. Restart the client if it does not detect the new
skills.

## Safety and scope

- `--allow-writes` grants a spawned coding worker workspace edits and shell access. Use only in repositories you trust. Read-only Cline runs use the dispatcher approval broker; read-only Gemini runs use a temporary policy. Both permit only their normal read/search tools and the scoped Autogora CLI lifecycle bridge.
- The MCP server is local stdio only; there is no multi-user isolation.
- The optional dashboard is authenticated but remains a trusted-local-user,
  single-tenant surface; its bearer token is not a substitute for TLS on an
  untrusted network.
- SQLite and PID recovery assume one host; cross-host dispatch is intentionally unsupported.
- Messaging-platform slash commands remain adapter concerns; every board action
  they need is available through the shared MCP, CLI, or authenticated HTTP
  kernel.
