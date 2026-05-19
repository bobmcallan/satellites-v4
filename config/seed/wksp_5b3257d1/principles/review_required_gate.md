---
id: pr_review_required_gate
name: review_required_gate
scope: workspace
tags:
  - process
  - structural-invariant
  - story
  - v4
---
A story cannot transition to `done` or `cancelled` while any closed work task on the chain runs a contract that declares `review_required: true` and lacks a paired review with a `verdict:pass` ledger row — the substrate enforces this invariant in `internal/story/store.go`.

## What the invariant says

`pr_story_terminal_gate` already blocks `story_update(status=done|cancelled)` while OPEN tasks remain. This gate is the structural sibling: it blocks the same transitions while a CLOSED work task on the chain ran a `review_required: true` contract without acquiring the matching `verdict:pass` review row.

When the operator (or any caller of `story_update` with a `status` argument targeting `done` or `cancelled`) attempts the transition, the substrate walks the story's task chain. For each row whose `Kind == work`, `Status == closed`, and whose `Action == contract:<name>` resolves to a contract document carrying `review_required: true` in its structured payload, the substrate requires:

- A sibling task `R` on the same chain with `R.ParentTaskID == work.ID`, `R.Kind == review`, `R.Status == closed`, and `R.Outcome == success`.
- A ledger row scoped to the story tagged `kind:verdict`, `task_id:<R>`, and `verdict:pass`.

Any missing piece → 422 with `{"error":"review_required_gate","work_tasks_missing_pass":["task_<id>",...]}` listing the offending work task ids. The orchestrator's response is to dispatch the missing review tasks; on `verdict:fail`, mint a fresh `task_add(action=contract:<name>, prior_task_id=<rejected_work>)` per `pr_pipeline_authority`.

## Contract opt-in

Each seeded contract carries `review_required: true | false` in its frontmatter. The configseed parser rejects a contract missing the key (or carrying a non-bool value) with the literal error `contract %q: review_required required (true|false)` so the choice is explicit at author time.

The matrix on the seeded contracts today:

- `develop` — `review_required: true`. Develop is judgement-shape work; the development_reviewer's verdict is the close-license.
- `story_review` — `review_required: true`. Story review's own pass/fail itself receives a meta-review row on `kind:verdict` scoped to the review task.
- `plan`, `story_close`, `substrate_audit`, `review`, `commit`, `merge_to_main` — `review_required: false`. Plan and review are base cases (no recursion); commit / merge_to_main are execution-shape (mechanical, no reviewer dispatch); story_close + substrate_audit are structural gates.

When a new contract joins the seed set, the author MUST add the field; the substrate refuses to seed a contract missing it.

## Worked failure mode

The session that surfaced this gap closed a work task with `outcome=success` but skipped the development_reviewer dispatch entirely — there was no `kind=review` sibling on the chain, no `kind:verdict` row, and the story would have shipped without a pass row anywhere. `pr_pipeline_authority` already declared the orchestrator's discipline ("a reviewer rejection is operator authority"), and `pr_role_grid` declared the role narrowing — but until this gate, both were honour-system. The substrate now rejects the close attempt structurally with the offending work task ids in the response body, the same shape as `pr_story_terminal_gate`'s envelope.

## Why a substrate gate, not a reviewer rubric

This invariant is **structural**, not semantic — same tier as `pr_story_terminal_gate`'s open-tasks check. A reviewer rubric fires after the orchestrator has already submitted evidence; this gate fires earlier, on the first attempt, and tells the operator (in the response body) exactly which work task ids are missing their verdict row. Faster feedback, no rework. The reviewer rubric's job is to evaluate the *quality* of evidence; this gate's job is to enforce the *shape* of state.

## Reconciler carve-out

The reconciler path (`UpdateStatusDerived`) is exempt — derivation-driven flips are themselves the consequence of an observed terminal task event, so by construction the chain is consistent with the target. Manual operator transitions are guarded; the storystatus reconciler is not.

## How to apply

- **Operator.** When a `story_update(status=done)` call returns 422 with `{"error":"review_required_gate","work_tasks_missing_pass":[…]}`, do not retry the transition. For each listed work task, dispatch the missing review via `task_add(kind=review, action=contract:<name>, parent_task_id=<work>)`, await its verdict, and retry the close only once every listed work task carries `verdict:pass`.
- **Orchestrator.** Same as the operator. The Pre-flight rule "the substrate refuses story done while any `review_required` contract closed without `verdict:pass`" is now structurally enforced rather than honour-system; see `[[claude_orchestrator]]`.
- **Substrate authors.** No operator-callable bypass exists (`pr_no_unrequested_compat`). Internal flows that legitimately need to skip the gate (the storystatus reconciler) call `UpdateStatusDerived`, which skips both `ValidTransition` and this gate. Adding a new internal flow goes through the same path.

## Citation

Companion to `pr_story_terminal_gate` (sty_0233fabd) — together the two gates structurally enforce the close discipline that `pr_pipeline_authority` declared. See sty_f49c378d for the implementation. Cites `pr_no_unrequested_compat`, `pr_pipeline_authority`, `pr_role_grid`.
