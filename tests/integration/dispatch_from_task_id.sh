#!/usr/bin/env bash
# dispatch_from_task_id.sh
#
# Repro scaffold for sty_38bec58f AC3 + AC4 — proves a fresh claude
# subprocess, dispatched on a freshly-minted task with only its task
# id, can drive the work end-to-end via existing _get verbs (no
# operator memory inheritance).
#
# This script is the captured invocation shape; it is not run in CI.
# CI does not have operator OAuth credentials. The substrate-resident
# regression evidence is the kind:integration-evidence ledger row on
# sty_38bec58f (ldg_<8hex>); see story_get(id=sty_38bec58f).
#
# Usage:
#   1. Mint a test story (`story_add`) and a `task_add(agent_id=
#      developer_agent, action=contract:develop, prompt=<canonical>)`
#      task on it. Capture <task_id>.
#   2. Create a per-task worktree at `.satellites-agents/<task_id>`
#      on `agent-<task_id>-from-<base_sha>`.
#   3. Export TEST_TASK=<task_id>; run this script from the satellites
#      repo root.
#
# What it does:
#   - Materialises a temp HOME with only auth artefacts (credentials +
#     settings; no `~/.claude/projects/` to inherit memory from).
#   - Captures the cleansed HOME listing as evidence.
#   - Invokes `claude -p` non-interactively, relying on the project-
#     local `.mcp.json` for satellites MCP server registration.
#   - Tees the transcript for the integration-evidence row.
#
# AC4 note:
#   The story's literal AC4.3 names `--bare`, but `--bare` rejects
#   OAuth (operator's only Anthropic auth path) — see
#   `claude --help` for the strict-API-key requirement. Memory
#   non-inheritance is instead proven by:
#     (a) the cleansed HOME has no `~/.claude/projects/` at dispatch
#         time (see /tmp/dispatch-home-listing.txt);
#     (b) settings.json carries no `hooks` key, so no operator hooks
#         fire;
#     (c) the empirical grep below — operator memory titles drawn
#         from MEMORY.md must produce zero hits in the transcript.

set -euo pipefail

if [[ -z "${TEST_TASK:-}" ]]; then
  echo "TEST_TASK env var required (e.g. TEST_TASK=task_5aa413ef)" >&2
  exit 2
fi

REPO_ROOT="${REPO_ROOT:-$(git rev-parse --show-toplevel)}"
WT="${REPO_ROOT}/.satellites-agents/${TEST_TASK}"

if [[ ! -d "${WT}" ]]; then
  echo "Worktree ${WT} does not exist; create it first via:" >&2
  echo "  git worktree add ${WT} -b agent-${TEST_TASK}-from-\$(git rev-parse --short HEAD)" >&2
  exit 2
fi

TMPHOME="$(mktemp -d -t dispatch-home-XXXXXX)"
mkdir -p "${TMPHOME}/.claude"
cp ~/.claude/.credentials.json "${TMPHOME}/.claude/"
cp ~/.claude/settings.json     "${TMPHOME}/.claude/"

LISTING="/tmp/dispatch-home-listing-${TEST_TASK}.txt"
{
  echo "=== HOME=${TMPHOME} ==="
  echo "--- ls -la \$HOME ---"
  ls -la "${TMPHOME}"
  echo "--- ls -la \$HOME/.claude ---"
  ls -la "${TMPHOME}/.claude"
  echo "--- projects/ check ---"
  if [[ -d "${TMPHOME}/.claude/projects" ]]; then
    echo "PROJECTS_DIR=present"
  else
    echo "PROJECTS_DIR=absent"
  fi
} > "${LISTING}"
cat "${LISTING}"

LOG="/tmp/dispatched-${TEST_TASK}.log"
cd "${WT}"

env HOME="${TMPHOME}" claude \
  --permission-mode bypassPermissions \
  -p "You are dispatched on satellites task ${TEST_TASK}. Read your full instructions via task_get(id='${TEST_TASK}'), then execute them in the listed order using only the verbs the body lists. Working directory is your cwd (${WT}). Print HOME=\$HOME and pwd at the start so the transcript shows them. Begin." \
  2>&1 | tee "${LOG}"

echo "---"
echo "Memory-titles grep (must produce no output):"
grep -Ef <(awk -F'[][]' '/^- \[/{print $2}' \
   ~/.claude/projects/-home-bobmc-development-satellites/memory/MEMORY.md) \
   "${LOG}" \
  || echo "OK: no operator memory titles in transcript"

echo "---"
echo "Transcript:    ${LOG}"
echo "Home listing:  ${LISTING}"
echo "Cleansed HOME: ${TMPHOME}"
