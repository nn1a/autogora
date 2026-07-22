# MCP-disabled Cline CLI bridge

The dispatcher does not configure or call Cline MCP. A compatible modified
Cline executable must:

- accept `--json`, `--cwd <path>`, and `--auto-approve <boolean>`;
- accept the worker prompt as the final positional argument;
- expose a shell tool that inherits the `AUTOGORA_*` environment variables;
- return exit code `0` after a successful turn and emit NDJSON on stdout;
- support Cline's desktop tool-approval file protocol for guarded read-only
  runs, or be used only with the dispatcher's explicit `--allow-writes` mode.

Configure and run it with:

```bash
export AUTOGORA_CLINE_BIN=/absolute/path/to/modified-cline
autogora create "Verify the modified Cline bridge" \
  --assignee cline-worker \
  --runtime cline \
  --workspace "$PWD"
autogora dispatch --once
```

The prompt contains absolute, shell-quoted lifecycle commands equivalent to:

```bash
"$AUTOGORA_CLI" show "$AUTOGORA_TASK_ID"
"$AUTOGORA_CLI" heartbeat "$AUTOGORA_TASK_ID" --note "progress"
"$AUTOGORA_CLI" comment "$AUTOGORA_TASK_ID" "durable handoff"
"$AUTOGORA_CLI" complete "$AUTOGORA_TASK_ID" --summary "verified result"
"$AUTOGORA_CLI" block "$AUTOGORA_TASK_ID" "missing decision" --kind needs_input
```

Do not supply claim tokens on the command line. The dispatcher injects them in
the child environment, and the CLI validates them against the active run.

To use Cline as the auxiliary planner:

```bash
autogora specify <triage-task-id> --planner-runtime cline
autogora decompose <triage-task-id> \
  --planner-runtime cline \
  --profile "worker:cline:implements and verifies scoped tasks"
```

Cline has no native output-schema flag. The planner prompt contains the schema;
the final NDJSON `run_result.text` or `agent_event` done text must be one JSON
object. Autogora parses and validates it before mutating the board.
