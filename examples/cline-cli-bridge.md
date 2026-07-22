# MCP-disabled Cline CLI bridge

The dispatcher does not configure or call Cline MCP. A compatible modified
Cline executable must:

- accept `--json`, `--cwd <path>`, and `--auto-approve <boolean>`;
- accept the worker prompt as the final positional argument;
- expose a shell tool that inherits the `TASKCIRCUIT_*` environment variables;
- return exit code `0` after a successful turn and emit NDJSON on stdout;
- support Cline's desktop tool-approval file protocol for guarded read-only
  runs, or be used only with the dispatcher's explicit `--allow-writes` mode.

Configure and run it with:

```bash
export TASKCIRCUIT_CLINE_BIN=/absolute/path/to/modified-cline
taskcircuit create "Verify the modified Cline bridge" \
  --assignee cline-worker \
  --runtime cline \
  --workspace "$PWD"
taskcircuit dispatch --once
```

The prompt contains absolute, shell-quoted lifecycle commands equivalent to:

```bash
"$TASKCIRCUIT_CLI" show "$TASKCIRCUIT_TASK_ID"
"$TASKCIRCUIT_CLI" heartbeat "$TASKCIRCUIT_TASK_ID" --note "progress"
"$TASKCIRCUIT_CLI" comment "$TASKCIRCUIT_TASK_ID" "durable handoff"
"$TASKCIRCUIT_CLI" complete "$TASKCIRCUIT_TASK_ID" --summary "verified result"
"$TASKCIRCUIT_CLI" block "$TASKCIRCUIT_TASK_ID" "missing decision" --kind needs_input
```

Do not supply claim tokens on the command line. The dispatcher injects them in
the child environment, and the CLI validates them against the active run.

To use Cline as the auxiliary planner:

```bash
taskcircuit specify <triage-task-id> --planner-runtime cline
taskcircuit decompose <triage-task-id> \
  --planner-runtime cline \
  --profile "worker:cline:implements and verifies scoped tasks"
```

Cline has no native output-schema flag. The planner prompt contains the schema;
the final NDJSON `run_result.text` or `agent_event` done text must be one JSON
object. TaskCircuit parses and validates it before mutating the board.
