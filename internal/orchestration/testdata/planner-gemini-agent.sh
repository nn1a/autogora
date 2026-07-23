#!/bin/sh
set -eu

output_format=
approval_mode=
policy_path=
prompt=
model=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-format)
      output_format=$2
      shift 2
      ;;
    --approval-mode)
      approval_mode=$2
      shift 2
      ;;
    --policy)
      policy_path=$2
      shift 2
      ;;
    -p)
      prompt=$2
      shift 2
      ;;
    --model)
      model=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ "$output_format" != json ] || [ "$approval_mode" != default ] || [ "$model" != "gemini-test" ]; then
  echo "Gemini planner must use constrained JSON headless mode" >&2
  exit 2
fi
if [ -z "$policy_path" ] || ! grep -Fq 'toolName = "*"' "$policy_path"; then
  echo "Gemini planner must receive a deny-all tool policy" >&2
  exit 2
fi
case "$prompt" in
  *"must conform to this schema"*) ;;
  *)
    echo "Gemini planner did not receive schema guidance" >&2
    exit 2
    ;;
esac

printf '%s\n' '{"response":"{\"title\":\"Gemini-generated task specification\",\"body\":\"Implement the requested change. Acceptance: record Gemini CLI verification evidence.\"}","stats":{}}'
