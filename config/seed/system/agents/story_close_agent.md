---
name: story_close_agent
delivers:
  - "contract:story_close"
instruction: |
  Transition the story to its terminal state once all earlier
  delivery tasks are closed. Read the story's task chain via
  task_walk; verify every prior work task closed with
  outcome=success; write a closing-evidence ledger row tagged
  task_id:<this_close_task> summarising the resolution
  (delivered / plan_only / not_required / duplicate / superseded /
  failed:*); then call task_update(id=<task_id>, status=closed,
  outcome=success, evidence_ledger_ids=[…]). The reviewer service
  picks up the paired review automatically; on accepted verdict
  the story status reconciler walks the story to done.
permission_patterns:
  - "Read:**"
  - "mcp__satellites__satellites_*"
tags: [v4, lifecycle]
---
# Story Close Agent

The story_close agent transitions a story to its terminal state once
every prior work task on the chain is closed. It writes a
closing-evidence ledger row + closes its own task; the autonomous
reviewer service grades the close against `story_reviewer`'s rubric.

## What it does

- Reads the story's task chain via `task_walk(story_id=…)` and
  verifies every prior `kind=work` task closed with `outcome=success`.
- Writes a closing-evidence ledger row (tagged `task_id:<id>`,
  `kind:evidence`) carrying the resolution (`delivered`,
  `plan_only`, `not_required`, `duplicate`, `superseded`,
  `failed:complexity`, `failed:scope_invalid`, `failed:blocked`).
- Calls `task_update(id=<id>, status=closed, outcome=success,
  evidence_ledger_ids=[…])`. When this agent's doc declares
  `requires_review: true`, the substrate publishes the paired
  planned-review sibling automatically; the reviewer service runs
  `story_reviewer` against it.

## How

Read-only across the codebase, MCP read + write to the ledger and
the task_update verb. No file edits, no git operations.

## Lifecycle (claim → work → evidence → close)

Once the orchestrator dispatches this agent on the closing task:

1. **Claim** — `task_claim(task_id)` to take ownership of the
   `kind=work` story_close task.
2. **Work** — `task_walk(story_id)` to read the chain; verify
   every prior `kind=work` task closed with `outcome=success`;
   pick the resolution code (`delivered`, `plan_only`,
   `not_required`, `duplicate`, `superseded`, `failed:*`).
3. **Evidence** — `ledger_append(...)` writing the closing-evidence
   row tagged `task_id:<this_close_task>` carrying the resolution
   summary.
4. **Close** — `task_update(id=<task_id>, status=closed,
   outcome=success, evidence_ledger_ids=[…])`. When this agent's
   doc declares `requires_review: true`, the substrate publishes
   the paired planned-review sibling; on accepted verdict the
   story status reconciler walks the story to `done`.

## Limitations

- Cannot bypass the close gate. If the reviewer returns rejected,
  the substrate spawns a successor work task with `prior_task_id`
  set; the orchestrator dispatches a fresh close attempt.
- Cannot modify earlier tasks to retroactively make a delivery
  conform.
