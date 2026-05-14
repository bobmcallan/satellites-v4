---
title: Contracts
slug: contracts
order: 30
tags: [help, contracts]
---
# Contracts

A **contract** is one phase in a story's lifecycle. Every contract
defines:

- which **agent** delivers it (`delivers_by:` frontmatter naming
  the agent doc whose `delivers:` list contains
  `contract:<name>`),
- which **agent reviews** it (`reviewed_by:` frontmatter naming
  the agent whose `reviews:` list contains `contract:<name>`),
- the **evidence shape** the delivering agent must record on close
  (`evidence_required:` frontmatter — typically a list of ledger
  rows tagged `task_id:<id>`).

## System contracts

The default lifecycle ships four system contracts:

| Contract | Phase | Delivers | Reviews |
|---|---|---|---|
| `plan` | readiness assessment, design, task list submission | `developer_agent` | `story_reviewer` |
| `develop` | implementation | `developer_agent` | `development_reviewer` |
| `push` | ship to origin | `releaser_agent` | `story_reviewer` |
| `merge_to_main` | local sync | `releaser_agent` | `story_reviewer` |

## Configuration

Each contract's markdown lives at
`config/seed/contracts/<name>.md`. Frontmatter carries the
structured payload (`delivers_by`, `reviewed_by`,
`evidence_required`); body is the human description.

## Lifecycle

Contracts surface at runtime as **task actions**. The orchestrator
submits a plan via `task_submit(kind=plan, tasks=[…])` where
each entry's `action` is the canonical `contract:<name>` form.
Each contract becomes a paired (kind=work, kind=review) task in
the chain. The work task is delivered by the agent whose
`delivers:` list covers the action; the paired review is graded by
the autonomous reviewer service against the agent whose `reviews:`
list covers the same action.

## Limitations

- Contract order is enforced by the reviewer, not the substrate.
  The mandate principle requires `contract:plan` at the front and
  `contract:story_close` at the end; the reviewer rejects plans
  that skip the floor.
- The full task list is submitted up front via
  `task_submit(kind=plan)`; the substrate validates structure
  and rejects malformed plans (missing review siblings, agent
  capability mismatches, etc.). Mid-flight scope changes happen by
  spawning successor task pairs, not by amending the original plan.
