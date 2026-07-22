#!/bin/sh
set -eu

json_mode=false
prompt=
for argument in "$@"; do
  if [ "$argument" = "--json" ]; then
    json_mode=true
  fi
  prompt=$argument
done

if [ "$json_mode" != true ]; then
  echo "Cline planner did not receive JSON mode" >&2
  exit 2
fi
case "$prompt" in
  *"must conform to this schema"*) ;;
  *)
    echo "Cline planner did not receive schema guidance" >&2
    exit 2
    ;;
esac

printf '%s\n' '{"type":"agent_event","event":{"type":"done","text":"{\"title\":\"Cline-generated task specification\",\"body\":\"Implement the requested change. Acceptance: record CLI verification evidence.\"}"}}'
printf '%s\n' '{"type":"run_result","text":"{\"title\":\"Cline-generated task specification\",\"body\":\"Implement the requested change. Acceptance: record CLI verification evidence.\"}"}'
