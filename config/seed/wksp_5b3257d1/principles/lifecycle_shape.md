---
id: pr_lifecycle_shape
name: lifecycle_shape
scope: workspace
tags:
  - process
  - lifecycle
  - reviewer
  - drift
  - v4
---
# Lifecycle shape — citation surface for task_walk drift signals

The `default_lifecycle` workflow document enumerates the canonical
chain shape `plan → (develop → review → iterate)+ → commit → push →
deploy → close`. `task_walk(story_id)` returns a `lifecycle_status`
field computed against that shape by `computeLifecycleStatus`
(`internal/client/lifecycle.go`). When the chain diverges, the value
is one of five `drifted:<reason>` tokens. Reviewers cite this
principle when reading those tokens against an in-flight or
just-closed chain.

## When to cite

Cite `pr_lifecycle_shape` when `task_walk.lifecycle_status` returns
any of the following:

- `drifted:plan_absent` — the chain skipped readiness assessment
  (no `contract:plan` work task on the chain).
- `drifted:review_skipped` — a terminal story has a closed
  `contract:develop` work task but no paired closed
  `contract:develop` review task.
- `drifted:close_before_push` — the story status is terminal but
  no `contract:merge_to_main` work task is present on the chain.
- `drifted:deploy_skipped` — the story status is terminal AND
  a closed `contract:merge_to_main` work task is present, but
  no `contract:deploy` work task is on the chain (regardless
  of status). The chain pushed main without attesting that
  pprod converged on the pushed SHA.
- `drifted:phase_unknown:<action>` — a closed work task carries
  an action this workflow does not enumerate.

## Reviewer-relevance heuristic

Reviewers MAY cite this principle when the drift blocks them
from judging the story shipped clean. The bare presence of a
`drifted:*` status is NOT itself a reject trigger; the reviewer
must articulate the specific gap the drift creates. Worked
examples:

- `drifted:review_skipped` on a chain whose develop slice
  touched a substrate primitive — reject, because the missing
  paired review means no one verified the rubric-updates list
  named by `pr_mandate_configuration_over_code` was authored
  alongside.
- `drifted:plan_absent` on a one-line typo fix that opened and
  closed inside an hour — typically benign; cite only if the
  one-line change carries downstream impact the reviewer cannot
  judge without the plan-md scope statement.
- `drifted:phase_unknown:<action>` — treat as a prompt to ask
  the orchestrator whether the new action belongs in
  `default_lifecycle`. The unknown action may be legitimate
  (the workflow doc has not caught up) or it may be a dispatch
  bug.
- `drifted:close_before_push` — reject when the story claims a
  pprod deploy that no `contract:merge_to_main` task can
  attest, accept when the story explicitly carries no deploy
  surface (docs-only change, internal tooling).
- `drifted:deploy_skipped` — reject when the story body claims
  a behaviour exercised against the pprod-deployed binary
  (any AC requiring the dispatched agent to exercise the
  running server, any dogfood AC against a real-world MCP
  surface, any task render-prompt / chain run / task run
  invocation that requires the just-merged code path) —
  closing the story without `contract:deploy` means the
  binary serving pprod requests does NOT yet carry the
  merged change. Accept when the story scope is
  configseed-only (markdown seeds, prose updates, principle
  authoring) or otherwise carries no pprod surface — the
  chain shipped intent but the deployed binary is unchanged,
  which is fine. The discriminator is whether the AC body
  references pprod behaviour (reject) or only on-disk seed
  state (accept).

## Why advisory now (not gating)

`default_lifecycle` frames the signal as **initially advisory**.
The `story_close` gate at `internal/client/story_close.go` does
NOT refuse to close on drift — it appends one
`kind:lifecycle-drift` ledger row tagged `reason:<drift_reason>`
and proceeds. The row is idempotent: re-closing the same drifted
chain authors no duplicate row.

This principle's surface is reviewer citation, not substrate
enforcement. Promoting any `drifted:*` reason to a hard close-time
gate is a separate follow-up story, authored only after the drift
signal has run long enough in practice to identify which reasons
are actually load-bearing (i.e. which classes of drift consistently
indicate a story that should NOT be closed). Citing this principle
in a reviewer verdict is the live observation that feeds the
eventual gating decision.

## Citation

Backed by `sty_43911f1e`. Registered as the citation slot in
`default_lifecycle` (sty_e0c3d615). The drift computation lives
at `internal/client/lifecycle.go` (function
`computeLifecycleStatus`); the advisory ledger-row write lives at
`internal/client/story_close.go`.
