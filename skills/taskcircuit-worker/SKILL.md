---
name: taskcircuit-worker
description: Execute a dispatcher-claimed task through TaskCircuit MCP or its scoped CLI lifecycle. Use when a worker session has KANBAN_TASK_ID/KANBAN_RUN_ID scope or the user explicitly asks to work an assigned TaskCircuit card; do not use for planning or routing unrelated cards.
---

# TaskCircuit Worker

Treat TaskCircuit as the canonical task state. Finish the assigned work and leave a durable, verifiable handoff.

Use the MCP tools when they are available. In an MCP-disabled Cline worker or an isolated Gemini dispatcher run, use the exact scoped TaskCircuit CLI bridge commands included in the dispatcher prompt; the environment already carries board, task, run, database, and claim-token scope.

## Workflow

1. Call `kanban_show` without `task_id`. Read the body, `relationshipGraph`, `workerContext`, prerequisite results, comments, prior runs, and workspace constraints. The hierarchy identifies the parent goal and sibling subtasks; dependency phases identify enforced execution order.
2. Work only on that task under `$KANBAN_WORKSPACE`. Read task attachments from the absolute paths returned by `kanban_show`. Do not claim, create, reassign, link, unblock, or update unrelated cards.
3. Work only on the current graph node. Do not implement sibling or downstream nodes; completing this node unlocks the dependents listed in the context. For long work, call `kanban_heartbeat` after meaningful checkpoints. Use `kanban_comment` for intermediate findings another run must retain.
4. Verify the acceptance criteria. For code, run focused tests and inspect the final diff.
5. For an ordinary card, terminate exactly once:
   - Call `kanban_complete` only when the result is usable and verified. List deliverable paths in `artifacts`; every relative path is resolved inside `$KANBAN_WORKSPACE` and must exist.
   - Call `kanban_block` with `kind=dependency`, `needs_input`, `capability`, or `transient` when work cannot continue.
   For a goal-mode card whose acceptance criteria are not yet met, leave the run active and end the turn with a concise progress handoff. The dispatcher records an independent judgment and continues until acceptance or the turn budget is exhausted. Claude, Codex, and Gemini resume the session; Cline may start a fresh turn from the durable handoff.

Do not finish an ordinary card with prose alone. A dispatcher treats that as a failed run.

## Completion evidence

Use a short human-readable summary. Put reusable evidence in `metadata`, for example:

```json
{
  "changed_files": ["src/example.ts"],
  "verification": ["npm test"],
  "residual_risk": []
}
```

Never include tokens, credentials, raw secret-bearing logs, or unrelated transcript content.

## Blocking

State the exact missing decision or capability and the smallest action that would unblock the card. Do not block merely because work is difficult or incomplete while safe progress remains.
