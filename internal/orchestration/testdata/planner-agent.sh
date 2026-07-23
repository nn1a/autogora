#!/bin/sh
set -eu

output_path=
schema_path=
model=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message)
      output_path=$2
      shift 2
      ;;
    --output-schema)
      schema_path=$2
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

if [ -z "$output_path" ] || [ -z "$schema_path" ] || [ "$model" != "gpt-test" ]; then
  exit 2
fi

if grep -q '"title"' "$schema_path"; then
  printf '%s' '{"title":"Planner-generated task specification","body":"Deliver the requested result. Acceptance: verification evidence is recorded."}' > "$output_path"
else
  printf '%s' '{"fanout":false,"rootTitle":"Planner-generated task specification","rootBody":"Deliver the requested result. Acceptance: verification evidence is recorded.","reason":"No fanout is needed.","tasks":[],"dependencies":[]}' > "$output_path"
fi
