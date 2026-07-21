---
name: kanban-orchestrator
description: Decompose a goal into durable Kanban MCP tasks, assignees, runtimes, workspaces, and acyclic dependencies. Use for planning or routing multi-step work across Claude/Codex workers; do not use to implement claimed worker tasks.
---

# Kanban Orchestrator

Use Kanban MCP to turn a goal into a small dependency graph that workers can execute without hidden context.

## Workflow

1. Call `kanban_list` to avoid duplicating existing work.
2. For a rough card already in `triage`, prefer `kanban_specify` for one executable task or `kanban_decompose` for an atomic routed graph. Supply the known profile roster and an explicit fallback; review the returned routes.
3. For a new swarm-shaped goal, use `kanban_swarm` to create the completed blackboard, parallel workers, verifier, and synthesizer in one operation.
4. Otherwise split only where tasks can run independently or need an explicit handoff. Prefer a few bounded cards over many tiny cards.
5. Create each card with:
   - an outcome-oriented title;
   - a body containing scope, constraints, acceptance criteria, and expected evidence;
   - an explicit `assignee`, `runtime` (`claude` or `codex`), and workspace policy (`scratch`, `dir:<absolute-path>`, or `worktree`);
   - `parents` when the task consumes another card's result.
6. Use `kanban_link` only for dependencies discovered after creation. Cycles are invalid.
7. Call `kanban_show` on the created cards and check that roots are `ready` and gated children are `todo`.

Do not claim or implement cards while acting as orchestrator. The dispatcher owns claims and worker launch.

## Routing guidelines

- Route code modification and repository automation to a worker with a writable workspace.
- Keep every graph on one board; workers are pinned to that board and cross-board links are invalid.
- Keep research and review cards read-only when possible.
- A child must be able to continue from parent completion summaries and metadata; never rely on the orchestrator's private conversation.
- Use priority to order otherwise-ready work, not to bypass dependencies.
- If the requested assignee, runtime, or workspace is unknown, ask for it instead of inventing an unsafe target.
