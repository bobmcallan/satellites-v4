#!/usr/bin/env bash
# Dispatch a satellites task to a fresh `claude -p` subprocess in a
# per-task git worktree with a cleansed HOME. Replaces hand-running
# the same shape per task while sty_a6250f92 (Client.Execute) is still
# in flight.
#
# Usage:
#   scripts/dispatch-task.sh <task_id> [base_sha]
#
# - <task_id>  required.  satellites task id (task_<8hex>).
# - <base_sha> optional.  Worktree base; defaults to current HEAD.
#
# Side effects:
#   - Creates .satellites-agents/<task_id>/ git worktree on a new
#     branch agent-<task_id>-from-<short_sha>.
#   - Creates a tmp HOME under /tmp with selective copy of
#     .credentials.json + settings.json (no projects/, no sessions/).
#   - Runs claude --permission-mode bypassPermissions -p '...' in the
#     worktree with HOME=<tmphome>.
#   - Streams stdout+stderr to .satellites-agents/<task_id>/.satellites-agent.log.
#   - Synchronous: blocks until the dispatched subprocess exits.
#
# The dispatched prompt is a thin pointer per pr_substrate_provides_context.
# The agent reads its task body via task_get(id=<task_id>) and fetches
# the rest of its context (story, agent, contract, principles) itself.

set -eu

TASK_ID="${1:-}"
if [[ -z "${TASK_ID}" ]]; then
    echo "usage: $0 <task_id> [base_sha]" >&2
    exit 2
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"
BASE_SHA="${2:-$(git -C "${REPO_ROOT}" rev-parse HEAD)}"
SHORT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short "${BASE_SHA}")"
WORKTREE="${REPO_ROOT}/.satellites-agents/${TASK_ID}"
BRANCH="agent-${TASK_ID}-from-${SHORT_SHA}"

if [[ ! -d "${WORKTREE}" ]]; then
    git -C "${REPO_ROOT}" worktree add "${WORKTREE}" -b "${BRANCH}" "${BASE_SHA}"
fi

TMPHOME="$(mktemp -d -p /tmp "dispatch-home-${TASK_ID}-XXXXXX")"
mkdir -p "${TMPHOME}/.claude"
cp ~/.claude/.credentials.json "${TMPHOME}/.claude/.credentials.json"
cp ~/.claude/settings.json "${TMPHOME}/.claude/settings.json"

LOG="${WORKTREE}/.satellites-agent.log"
PROMPT="implement ${TASK_ID} — call task_get(id=\"${TASK_ID}\") to read your full instructions; fetch project + agent + contract context via per-verb retrieval, then execute as described in the task body."

cd "${WORKTREE}"
HOME="${TMPHOME}" claude --permission-mode bypassPermissions -p "${PROMPT}" >"${LOG}" 2>&1
RC=$?

echo "task=${TASK_ID} branch=${BRANCH} tmphome=${TMPHOME} log=${LOG} exit=${RC}"
exit "${RC}"
