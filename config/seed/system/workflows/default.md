---
name: default
tags: [v4, system]
---
# Default System Workflow

The default lifecycle every story passes through. The workflow is
**prose-only context** for the orchestrator and reviewer agents —
the substrate enforces the structure of the submitted task list
(`task_submit(kind=plan)` validators) but not the specific
shape of the workflow itself.

## Shape

`plan → develop → push → merge_to_main → story_close`

- `plan` — implementation strategy + review criteria. The plan
  agent also assesses readiness (relevance, dependencies, prior
  delivery) and submits the full ordered task list via
  `task_submit(kind=plan, tasks=[…])`.
- `develop` — code edits + tests + commit. Multiple develop tasks
  are permitted when a story splits naturally (e.g. backend then
  frontend), but each is its own task pair (work + review) with
  its own evidence.
- `push` — ship to origin.
- `merge_to_main` — local sync.
- `story_close` — transition + reviewer verdict.

## How it's used

The orchestrator agent reads this prose when composing a per-story
plan and submits the plan via `task_submit(kind=plan, tasks=
[…])`. The substrate validates structural invariants (plan first,
every work task has a paired review sibling, agents have the right
capability) and rejects on violation.

The submitted plan list looks like:

```
tasks: [
  {kind: work,   action: contract:plan,          agent_id: developer_agent},
  {kind: review, action: contract:plan,          agent_id: story_reviewer},
  {kind: work,   action: contract:develop,       agent_id: developer_agent},
  {kind: review, action: contract:develop,       agent_id: development_reviewer},
  {kind: work,   action: contract:push,          agent_id: releaser_agent},
  {kind: review, action: contract:push,          agent_id: story_reviewer},
  {kind: work,   action: contract:merge_to_main, agent_id: releaser_agent},
  {kind: review, action: contract:merge_to_main, agent_id: story_reviewer},
  {kind: work,   action: contract:story_close,   agent_id: story_close_agent},
  {kind: review, action: contract:story_close,   agent_id: story_reviewer},
]
```

The orchestrator MAY add optional middle slots (e.g. an extra
`develop` pair for a multi-stage implementation) or drop steps
that don't apply to a particular story. The reviewer judges
whether the proposed shape is appropriate; the substrate accepts
whatever the reviewer approves.

## Floor

The mandate principle (`pr_mandate_reviewer_enforced`) requires
`contract:plan` at the front and `contract:story_close` at the
end of every story. Everything else is the orchestrator's
choice. Adding a new mandatory contract is an edit to the
principle text and the reviewer agent rubrics, not a Go change.
