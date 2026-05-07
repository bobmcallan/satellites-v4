---
name: story_close
category: story-close
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
  After this task closes, the orchestrator's next plan step (per
  this contract's review policy) is `task_add(kind=review,
  action=contract:story_close, parent_task_id=<story_close_task>)`
  for `story_reviewer`.
- Closes its own task via `task_update(id=<story_close_task>,
  status=closed, outcome=success, evidence_ledger_ids=[…])`. On
  accepted reviewer verdict the story status reconciler walks the
  story to `done`.

## How

Read-only across the codebase, MCP read on `task_walk` and ledger
verbs, and the `task_update` call that closes the story-close
task itself.

## Review policy

Story close dispatches a reviewer (the QA gate). The reviewer
reads the story's task chain via `task_walk(story_id)` and the
evidence ledger rows tagged on each prior task, then writes a
verdict on whether the chain delivered the ACs. The story status
reconciler walks the story to `done` only after the reviewer
verdict is accepted — see the `Story terminal-transition gate`
principle.

## Limitations

- Cannot bypass the close gate. On rejected verdict the
  reviewer's contract prose mints a successor work task via
  `task_add(prior_task_id=…)`; the orchestrator dispatches a
  fresh close attempt.
- Cannot retroactively edit prior tasks to make the close pass.
- One terminal transition per story; once `done` or `cancelled`,
  the story is immutable.
- No bypass of the QA reviewer — story status remains `published`
  until the reviewer verdict is accepted.
