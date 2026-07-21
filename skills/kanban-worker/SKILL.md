---
name: kanban-worker
description: Execute a dispatcher-claimed task through the Kanban MCP lifecycle. Use when a worker session has KANBAN_TASK_ID/KANBAN_RUN_ID scope or the user explicitly asks to work an assigned Kanban card; do not use for planning or routing unrelated cards.
---

# Kanban Worker

Treat Kanban MCP as the canonical task state. Finish the assigned work and leave a durable, verifiable handoff.

## Workflow

1. Call `kanban_show` without `task_id`. Read the body, parent results, comments, prior runs, and workspace constraints.
2. Work only on that task in the current workspace. Do not claim, create, reassign, link, unblock, or update unrelated cards.
3. For long work, call `kanban_heartbeat` after meaningful checkpoints. Use `kanban_comment` for intermediate findings another run must retain.
4. Verify the acceptance criteria. For code, run focused tests and inspect the final diff.
5. Terminate exactly once:
   - Call `kanban_complete` only when the result is usable and verified.
   - Call `kanban_block` when human input, a missing capability, or an unresolved external dependency prevents completion.

Do not finish with prose alone. A dispatcher treats exit without `kanban_complete` or `kanban_block` as a failed run.

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
