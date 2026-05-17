---
name: default_lifecycle
tags: [v4, workspace, lifecycle]
---
# Default Lifecycle Workflow

The dispatch lifecycle for stories in the satellites workspace.
Prose-authoritative source for the chain shape: `plan → (develop →
review → iterate)+ → commit → push → close`. Substrate-level drift
detection cites this document by name; the `lifecycle_status` field
on `task_walk` is computed against the phase ordering enumerated
below.

This document is workspace-scoped (`wksp_5b3257d1`). Project-scope
overrides may shadow it by re-authoring `name: default_lifecycle`
at `scope=project`; the system-tier `default` workflow continues to
exist as the canonical pattern projects start with.

## Shape

`plan → (develop → review → iterate)+ → commit → push → close`

The chain is authored via MCP (`task_add` mints each task) and
EXECUTED via `satellites-client task run` — the authoring/execution
split per `sty_c8a488ae` applies uniformly. Each judgment-shape
phase (develop, story_close) carries a paired `kind=review`
sibling; execution-shape phases (commit, merge_to_main) do not.

## Phases

- **plan** — one work task, `contract:plan`. The plan agent assesses
  readiness, drafts the per-AC review criteria, and authors the
  ordered task-list for downstream phases. Author via
  `task_add(action=contract:plan, agent_id=developer_agent)`;
  execute via `task run`. Paired `kind=review` sibling
  (`story_reviewer`) reads the plan against `pr_plan` and writes
  `verdict:accepted | verdict:rejected`.

- **develop → review → iterate** — the loop body that ships small.
  Each develop close mints a paired `kind=review` task
  (`development_reviewer`). An accepted review allows either the
  next develop slice OR an exit to `commit`. A rejected review
  mints an iter-N+1 `contract:develop` work task via
  `task_add(prior_task_id=…)` per `pr_reviewer_voice_authoritative`.
  The loop body is the substrate's unit of independently valuable
  change.

- **commit** — one work task, `contract:commit`, releaser role.
  Publishes the develop work-branch commit(s) to origin under the
  `client-{task_id}-…` branch. Substrate-level publish action;
  this is not the release.

- **push** — one work task, `contract:merge_to_main`, releaser
  role. Fast-forward merges the work branch into local main,
  pushes main to origin, watches the GH Actions deploy workflow to
  completion, polls pprod's `satellites_info` until the reported
  commit matches the pushed SHA. Emits a `kind:release-evidence`
  ledger row carrying SHAs + GH run id + pprod converge.

- **close** — one work task, `contract:story_review`, story_reviewer
  role, plus the mechanical `story_close` MCP verb. The
  story_review task writes `kind:verdict, verdict:pass | verdict:fail`
  after walking the full chain; the `story_close` verb gate-checks
  the chain (open-task count, verdict row, template-field roll-up,
  deploy:behind) and on PASS appends `kind:close-evidence` and
  walks the story to `status=done`. The lifecycle-drift warning
  row (kind:lifecycle-drift) is authored here when the chain
  shape diverges from this document.

## Lifecycle status

`task_walk(story_id)` returns a `lifecycle_status` field with one
of:

- `on_shape` — closed-task sequence matches the phase ordering
  above (with `develop → review` loops allowed).
- `drifted:plan_absent` — no `contract:plan` work task on the
  chain.
- `drifted:review_skipped` — terminal story with a closed
  `contract:develop` work task but no paired closed
  `contract:develop` review.
- `drifted:close_before_push` — story status terminal but no
  `contract:merge_to_main` work task present.
- `drifted:phase_unknown:<action>` — a closed work task carries an
  action this workflow does not enumerate.

The signal is **initially advisory**. The `story_close` gate does
NOT refuse to close on drift; it appends one
`kind:lifecycle-drift` ledger row tagged
`reason:<drift_reason>` and proceeds. Idempotent — a repeated
close on the same drifted chain authors no duplicate row.

## Citations

- `pr_lifecycle_shape` — reviewers MAY cite this principle when a
  chain is `drifted:*` and the gap is reviewer-relevant. Authoring
  of the principle itself is a follow-up; this workflow registers
  the citation slot.
- `pr_substrate_model` — workflow doc is the new prose-authoritative
  source for the lifecycle. The Go-side `canonicalPhases` list in
  `internal/client/chain.go` continues to drive routing; collapsing
  it into this document is a follow-up once drift becomes
  load-bearing (gating, not advisory).

## How it's used

The orchestrator reads this prose when composing a per-story plan.
The substrate's `computeLifecycleStatus` helper
(`internal/client/lifecycle.go`) walks the closed task chain
against the enumerated phases and writes `lifecycle_status` onto
the `task_walk` payload. The reviewer reads `lifecycle_status`
when judging chain shape; on drift the reviewer's rubric MAY cite
`pr_lifecycle_shape`.
