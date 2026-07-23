#!/bin/sh
set -eu

json_mode=false
prompt=
model=
provider=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --json)
      json_mode=true
      shift
      ;;
    --model)
      model=$2
      shift 2
      ;;
    --provider)
      provider=$2
      shift 2
      ;;
    *)
      prompt=$1
      shift
      ;;
  esac
done

if [ "$json_mode" != true ] || [ "$model" != "cline-test" ] || [ "$provider" != "openrouter" ]; then
  echo "Cline planner did not receive JSON mode, model, and provider" >&2
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
