---
id: pr_story_terminal_gate
name: Story terminal-transition gate
scope: project
tags:
  - process
  - structural-invariant
  - story
  - v4
---
A story cannot transition to `done` or `cancelled` while its task chain has open work — the substrate enforces this invariant in `internal/story/store.go`.

## What the invariant says

When the operator (or any caller of `story_update_status`) targets the terminal states `done` or `cancelled`, the substrate looks up tasks scoped to the story whose status is `published` or `planned`. If any exist, the transition is rejected with `ErrStoryHasOpenTasks` and the open task ids are returned in the error payload.

The reconciler path (`UpdateStatusDerived`) is exempt: derivation-driven flips are themselves the consequence of an observed terminal task event, so by construction the chain is consistent with the target.

## Why a substrate gate, not a reviewer rubric

This invariant is **structural**, not semantic. Reviewer rubrics check the quality of evidence; this gate checks the shape of state — same tier as `ValidTransition`'s terminal-immutability rule. Pushing it through reviewer evidence would couple the gate's enforcement to the reviewer service's availability, which is a different failure mode than "operator made a structural mistake".

A reviewer rubric also fires after the orchestrator has already submitted evidence; this gate fires earlier, on the first attempt, and tells the operator (in the response body) exactly which task ids to reconcile. Faster feedback, no rework.

## Why a Go gate, not a configuration document

The substrate has no "transition gate document" primitive today. Building one (a runtime that reads gate documents and applies them at transition time) is a substantive substrate evolution beyond a single rule. A small Go gate now is the minimum viable enforcement; if a second gate appears, that's the time to introduce the primitive.

## How to apply

- **Operator.** When a `story_update_status(done)` call returns 422 with `{"error": ..., "open_task_ids": [...]}`, do not retry the transition. Reconcile the listed tasks first — close them via `task_submit(kind=close)`, cancel them, or cancel the story instead. Then retry.
- **Orchestrator.** Same as the operator. The pre-flight rule "do not bypass the chain by transitioning to done while open tasks remain" (cited from `pr_reviewer_voice_authoritative`) is now enforced; the rule is no longer honour-system.
- **Substrate authors.** Internal flows that legitimately need to bypass the gate (the storystatus reconciler) call `UpdateStatusDerived`, which skips both `ValidTransition` and this gate. Adding a new internal flow goes through the same path.

## Citation

Backs the third slice of `epic:operator-visibility`. See `sty_0233fabd` for the implementation. Closes the bypass observed during `sty_01f75142`'s zombie-state reconciliation.
