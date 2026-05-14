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

`plan → develop → develop_review → story_review → push → merge_to_main → push_main → story_close [verb]`

- `plan` — implementation strategy + review criteria. The plan
  agent also assesses readiness (relevance, dependencies, prior
  delivery) and submits the full ordered task list via
  `task_submit(kind=plan, tasks=[…])`.
- `develop` — code edits + tests + commit. Multiple develop tasks
  are permitted when a story splits naturally (e.g. backend then
  frontend), but each is its own task pair (work + review) with
  its own evidence.
- `develop_review` — `development_reviewer` reads the develop
  task's evidence and writes `verdict:accepted | verdict:rejected`.
  On rejection the reviewer's contract prose mints a fresh
  `kind=work contract:develop task_add(prior_task_id=…)` and
  the orchestrator dispatches that retry.
- `story_review` — `story_reviewer` reads the full chain +
  evidence ledger rows + the story template fields and writes
  `verdict:pass | verdict:fail` to one `kind:verdict` ledger row.
  On `verdict:fail` the orchestrator mints a fresh
  `kind=work contract:develop task_add(prior_task_id=…)` carrying
  the cited gap verbatim (same iteration shape as develop_review;
  cite `pr_pipeline_authority`).
- `push` — ship the develop branch to its upstream remote.
- `merge_to_main` — local fast-forward main to the develop branch.
- `push_main` — ship the merged main to origin.
- `story_close [verb]` — the mechanical MCP verb (no agent
  dispatch). Gate-checks chain shape + the `story_review` verdict
  + story template fields; on PASS appends a
  `kind:close-evidence` ledger row and walks the story to
  `status=done` via `UpdateStatusDerived`.

## How it's used

The orchestrator agent reads this prose when composing a per-story
plan and submits the plan via `task_submit(kind=plan, tasks=
[…])`. The substrate validates structural invariants (plan first,
every work task has a paired review sibling where the contract
requires one, agents have the right capability) and rejects on
violation.

The submitted plan list looks like:

```
tasks: [
  {kind: work,   action: contract:plan,          agent_id: developer_agent},
  {kind: review, action: contract:plan,          agent_id: story_reviewer},
  {kind: work,   action: contract:develop,       agent_id: developer_agent},
  {kind: review, action: contract:develop,       agent_id: development_reviewer},
  {kind: work,   action: contract:story_review,  agent_id: story_reviewer},
  {kind: work,   action: contract:push,          agent_id: releaser_agent},
  {kind: review, action: contract:push,          agent_id: story_reviewer},
  {kind: work,   action: contract:merge_to_main, agent_id: releaser_agent},
  {kind: review, action: contract:merge_to_main, agent_id: story_reviewer},
  {kind: work,   action: contract:push,          agent_id: releaser_agent}, // push_main
  {kind: review, action: contract:push,          agent_id: story_reviewer},
]
// The terminal `story_close` step is the mechanical MCP verb
// (no task — orchestrator calls `mcp__satellites__story_close(story_id=…)`
// directly after the last `task_walk` confirms the chain is clean.)
```

The orchestrator MAY add optional middle slots (e.g. an extra
`develop` pair for a multi-stage implementation) or drop steps
that don't apply to a particular story. The reviewer judges
whether the proposed shape is appropriate; the substrate accepts
whatever the reviewer approves.
