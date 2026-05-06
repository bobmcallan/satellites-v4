---
name: story_close
category: story-close
delivers_by: story_close_agent
reviewed_by: story_reviewer
evidence_required: |
  Ledger row tagged task_id:<story_close_task>, kind:evidence
  capturing the resolution (delivered / plan_only / not_required /
  duplicate / superseded / failed:complexity / failed:scope_invalid
  / failed:blocked) and citing the prior task chain via
  task_walk(story_id).
tags: [v4, lifecycle, system]
---
# Story Close Contract

The end-floor of every story. Story close transitions the story to
its terminal state once every prior `kind=work` task on the chain
closed with `outcome=success`.

## What it does

- Reads `task_walk(story_id=…)` to confirm every prior work task
  closed successfully.
- Writes a closing-evidence ledger row capturing the resolution.
- Closes its own task via `task_submit(kind=close,
  outcome=success, evidence_ledger_ids=[…])`. The substrate
  publishes the paired review task; the autonomous reviewer service
  grades the close against `story_reviewer`'s rubric. On accepted
  verdict the story status reconciler walks the story to done.

## How

Read-only across the codebase, MCP read + write to the ledger and
`task_submit` verbs.

## Limitations

- Cannot bypass the close gate. On rejected verdict the substrate
  spawns a successor work task with `prior_task_id` set; the
  orchestrator dispatches a fresh close attempt.
- Cannot retroactively edit prior tasks to make the close pass.
- One terminal transition per story; once `done` or `cancelled`,
  the story is immutable.
