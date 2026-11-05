#!/usr/bin/env bash
# Shared helper, sourced by the other scripts/<tool>.sh wrappers: runs a
# command, sends its full output to a log file under .dev/logs/, and
# prints only a short pass/fail result instead of the raw output.
# Not meant to be run directly.

run_logged() {
  local name="$1"
  shift
  mkdir -p "$ROOT/.dev/logs"
  local log="$ROOT/.dev/logs/$name.log"
  "$@" >"$log" 2>&1
  local status=$?
  if [ "$status" -eq 0 ]; then
    echo "$name: OK (log: $log)"
  else
    echo "$name: FAILED (exit $status) - last 40 lines of $log:" >&2
    tail -n 40 "$log" >&2
  fi
  return "$status"
}
