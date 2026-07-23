#!/bin/sh
set -eu

model=
schema=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --model)
      model=$2
      shift 2
      ;;
    --json-schema)
      schema=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ "$model" != "claude-test" ] || [ -z "$schema" ]; then
  echo "Claude planner did not receive model and schema" >&2
  exit 2
fi

printf '%s\n' '{"structured_output":{"title":"Claude-generated task specification","body":"Implement the requested change and record verification evidence."}}'
