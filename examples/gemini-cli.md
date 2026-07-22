# Gemini CLI runtime

Gemini CLI can use this project in two independent ways:

1. Interactive sessions can register the TaskCircuit stdio MCP server:

   ```bash
   TASKCIRCUIT_BIN=$(command -v taskcircuit)
   gemini mcp add --scope project taskcircuit "$TASKCIRCUIT_BIN" serve -- \
     --db "$PWD/data/kanban.db"
   ```

2. The dispatcher can launch Gemini as a worker or auxiliary planner without
   changing user or project Gemini settings:

   ```bash
   export TASKCIRCUIT_GEMINI_BIN=/absolute/path/to/gemini # optional
   taskcircuit create "Implement and verify the change" \
     --assignee gemini-worker --runtime gemini --workspace "$PWD"
   taskcircuit dispatch --once --allow-writes
   ```

Worker runs use `--output-format stream-json`, capture the `init.session_id`,
and use `--resume <session-id>` for later goal turns. The worker communicates
task state through the scoped CLI commands in its prompt. Every lifecycle call
must match `TASKCIRCUIT_DB`, `TASKCIRCUIT_BOARD`, `TASKCIRCUIT_TASK_ID`,
`TASKCIRCUIT_RUN_ID`, and `TASKCIRCUIT_CLAIM_TOKEN` from the child environment.

Read-only dispatch is the default. A temporary Gemini policy denies MCP tools
and denies `run_shell_command` except when its command begins with the exact
TaskCircuit CLI bridge prefix. The policy file is created immediately before the
process starts and deleted when it exits. Extensions are disabled for the run.
Use `--allow-writes` only for a trusted workspace; it selects Gemini's `yolo`
approval mode, whose sandbox behavior remains controlled by the installed
Gemini CLI and local configuration.

Gemini can also specify or decompose triage work:

```bash
taskcircuit specify <triage-task-id> --planner-runtime gemini
taskcircuit decompose <triage-task-id> \
  --planner-runtime gemini \
  --profile "worker:gemini:implements and verifies scoped tasks"
```

Planner runs use headless JSON output, unwrap the `response` text, and validate
it against the same domain schema used by the other planners. A temporary
deny-all tool policy keeps planning read-only and side-effect free.
