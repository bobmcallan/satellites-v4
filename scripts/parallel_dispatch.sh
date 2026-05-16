#!/usr/bin/env bash
# parallel_dispatch.sh — sty_4fb2d985 multi-story chain dogfood (AC5
# evidence only; NOT a shipped artefact).
#
# Drives N sacrificial stories through their chains in parallel by
# running one `satellites-client chain run --story-id <id>` per
# story in the background, then `wait`-ing on all of them.
#
# The aim is to prove the substrate-side router handles concurrent
# chain advances without cross-talk. Each background invocation logs
# its heartbeat + terminal JSON to a per-story log file; the operator
# pastes the three terminal payloads + wall-clock into the merge_to_main
# task's ledger row tagged `kind:dogfood-evidence`.
#
# Usage:
#   ./scripts/parallel_dispatch.sh sty_aaa sty_bbb sty_ccc
#
# Requirements:
#   - The local serve daemon is running (`satellites-client serve start`).
#   - At least one valid bearer is loaded into the resolved client TOML.
#
# Exit code: non-zero if any chain run exits non-zero.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <story_id> [story_id ...]" >&2
  exit 2
fi

CLI="${SATELLITES_CLIENT:-./.satellites/satellites-client}"
LOGDIR="${LOGDIR:-./.satellites/logs/parallel_dispatch}"
mkdir -p "$LOGDIR"

START_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "=== parallel_dispatch starting at $START_TS for $# stories ==="

pids=()
for s in "$@"; do
  log="$LOGDIR/$s.log"
  echo "→ $s → $log"
  "$CLI" chain run --story-id "$s" > "$log" 2>&1 &
  pids+=("$!")
done

fail=0
for i in "${!pids[@]}"; do
  pid="${pids[$i]}"
  story="${@:i+1:1}"
  if wait "$pid"; then
    echo "✓ $story (pid $pid)"
  else
    echo "✗ $story (pid $pid) — see $LOGDIR/$story.log" >&2
    fail=1
  fi
done

END_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "=== parallel_dispatch finished at $END_TS (fail=$fail) ==="

# Print the final-line payload from each log so the evidence row has
# everything it needs in one paste.
for s in "$@"; do
  echo "--- terminal payload: $s ---"
  tail -1 "$LOGDIR/$s.log"
done

exit "$fail"
