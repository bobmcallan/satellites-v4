---
name: plan
category: plan
dispatch_class: heavy
evidence_required: |
  Two ledger artifacts recorded on the plan task (tagged
  task_id:<plan_task>):
  - plan.md  (scope, files-to-change, approach, test-strategy, AC mapping)
  - review-criteria.md  (per-AC verify / evidence / pass-fail boundary)

  Plus an ordered downstream task list authored by the plan agent
  covering each contract the story will execute. Judgment-shape
  contracts (develop, story_close) carry a kind=review sibling on
  the chain; execution-shape contracts (push, merge_to_main) do
  not. plan and review are base cases (no recursion).
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
- **Task list** — the plan agent authors the ordered downstream
  task list. Judgment-shape contracts (develop, story_close) carry
  a `kind=review` sibling on the chain; execution-shape contracts
  (push, merge_to_main) do not. plan and review are base cases
  (no recursion).

## How

Read-only investigation plus ledger writes plus the task-list
authoring call. The plan agent inspects the codebase and reasons
about the change shape; it never edits a file or runs a build.

## Review policy

Plan has no reviewer dispatch. The plan agent's output (plan.md +
review-criteria.md) is the review yardstick for every downstream
contract; meta-review of the plan happens implicitly when the
develop reviewer cites plan-induced gaps in its verdict.

## Limitations

- Plan binds develop. Mid-flight scope changes go through the
  orchestrator's task-spawn flow against the same story; the plan
  task itself is one-shot per story.
- Plan cannot file follow-up stories during planning — that
  belongs to the user's decision space. Plan can _propose_ splits
  in `plan.md` but does not act on them.
- Plan close requires the authored task list to cover the story's
  ACs; a plan that designs no work is not a plan.
