---
name: autogora-orchestrator
description: Decompose a goal into durable Autogora tasks, assignees, runtimes, workspaces, and acyclic dependencies. Use for planning or routing multi-step work across Claude, Codex, Cline, and Gemini workers; do not use to implement claimed worker tasks.
---

# Autogora Orchestrator

Use Autogora MCP to turn a goal into a small dependency graph that workers can execute without hidden context.

## Workflow

1. Call `autogora_list` to avoid duplicating existing work.
2. For a rough card already in `triage`, prefer `autogora_specify` for one executable task or `autogora_decompose` for an atomic routed graph. Decomposition records every generated node as a subtask of the triage root and separately records prerequisite/dependent execution edges. Supply the known profile roster and an explicit fallback; review the returned routes.
3. For a new swarm-shaped goal, use `autogora_swarm` to create the completed blackboard, parallel workers, verifier, and synthesizer in one operation.
4. Otherwise split only where tasks can run independently or need an explicit handoff. Prefer a few bounded cards over many tiny cards.
5. Create each card with:
   - an outcome-oriented title;
   - a body containing scope, constraints, acceptance criteria, and expected evidence;
   - an explicit `assignee`, `runtime` (`claude`, `codex`, `cline`, or `gemini`), and workspace policy (`scratch`, `dir:<absolute-path>`, or `worktree`);
   - `parents` when the task consumes another card's result.
6. Use `autogora_link` only for dependencies discovered after creation (`parent_id` is the prerequisite, `child_id` is the dependent). Use `autogora_subtask_set` only for hierarchy ownership; hierarchy does not gate execution. Cycles are invalid in both relation types.
7. Call `autogora_graph` and `autogora_show` on the created cards. Check hierarchy membership, topological phases, root readiness, and that dependency-gated subtasks remain `todo` until every prerequisite handoff is satisfied.

If an administrative change must replace an active run, call `autogora_run_terminate` first. Do not move, complete, archive, or delete a running task while its worker can still modify the workspace.

Do not claim or implement cards while acting as orchestrator. The dispatcher owns claims and worker launch.

## Routing guidelines

- Route code modification and repository automation to a worker with a writable workspace.
- Keep every graph on one board; workers are pinned to that board and cross-board links are invalid.
- Keep research and review cards read-only when possible.
- A child must be able to continue from parent completion summaries and metadata; never rely on the orchestrator's private conversation.
- Use priority to order otherwise-ready work, not to bypass dependencies.
- If the requested assignee, runtime, or workspace is unknown, ask for it instead of inventing an unsafe target.
