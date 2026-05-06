---
name: plan
category: plan
delivers_by: developer_agent
reviewed_by: story_reviewer
evidence_required: |
  Two ledger artifacts recorded on the plan task (tagged
  task_id:<plan_task>):
  - plan.md  (scope, files-to-change, approach, test-strategy, AC mapping)
  - review-criteria.md  (per-AC verify / evidence / pass-fail boundary)

  Plus a submitted task list via task_submit(kind=plan,
  tasks=[…]) covering the downstream actions (develop / push /
  merge_to_main / story_close) each paired with its kind=review
  sibling. The plan task itself is tasks[0] (kind=work,
  action=contract:plan); its review sibling is tasks[1].
tags: [v4, lifecycle, system]
---
# Plan Contract

Designs the implementation strategy and decomposes the story into
an ordered task list. Plan is the front-floor of every story — the
orchestrator role owns process definition here, including the
readiness assessment.

## What it does

- **Readiness assessment** — relevance, dependencies, prior delivery.
  The plan agent confirms the story is required, blockers are met,
  and the work has not already shipped under a sibling story before
  designing the implementation.
- `plan.md` — the implementation strategy: scope, files-to-change,
  approach, test strategy, AC mapping. Written as a ledger row
  tagged `task_id:<plan_task>`, `kind:evidence`.
- `review-criteria.md` — the per-AC success conditions, written
  before the implementing agent begins so the criteria are
  independent of the implementing agent's choices. Same tagging.
- **Task list** — the plan agent submits the full ordered task
  list via `task_submit(kind=plan, tasks=[…])`. Each
  downstream work task is paired with its kind=review sibling; the
  substrate validates structure (plan first, every work has a
  review, agents match capability) and rejects on violation.

## How

Read-only investigation plus ledger writes plus the
`task_submit(kind=plan)` call. The plan agent inspects the
codebase and reasons about the change shape; it never edits a file
or runs a build.

## Limitations

- Plan binds develop. Mid-flight scope changes require submitting a
  fresh plan via `task_submit(kind=plan)` against the same
  story (the substrate is idempotent on first submission and
  rejects subsequent submissions when tasks already exist — agents
  amend through the orchestrator's task-spawn flow, not by
  re-submitting plan).
- Plan cannot file follow-up stories during planning — that belongs
  to the user's decision space. Plan can _propose_ splits in
  `plan.md` but does not act on them.
- Plan close requires the submitted task list to cover the story's
  ACs; a plan that designs no work is not a plan.
