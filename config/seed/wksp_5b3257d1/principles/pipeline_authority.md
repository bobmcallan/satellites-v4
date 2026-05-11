---
id: pr_pipeline_authority
name: pipeline_authority
scope: workspace
tags:
  - process
  - pipeline
  - reviewer
  - mandate
---
# The contract pipeline is mandatory; reviewer rejections are the operator's voice

The contract pipeline is mandatory. When a contract is claimed, all work flows through the contracted process — no commits, pushes, or deliveries outside it.

A process failure (reviewer bug, serialization error, tool malfunction) is not the orchestrator's problem to solve by circumventing the process. Report it and wait.

## Reviewer rejections carry the operator's standard for "done"

The autonomous reviewer service runs the rubric the operator codified in the seed. When it rejects a work-task close, the rejection is the operator's voice — it is not noise to be routed around. The substrate is append-only: a rejection renders as a fresh `kind=work` task minted by the reviewer's contract prose via `task_add(prior_task_id=…)`, and the chain on `task_walk(story_id)` is the audit-of-record.

The orchestrator's job is to dispatch the fresh attempt and address each gap the verdict cited. The verdict text — including principle ids it cites and gaps it names — IS the close-criteria checklist for the iter-2 retry.

## What this forbids

- **Closing a story while its task chain has open work tasks.** The chain reflects the substrate's notion of in-flight work; transitioning to `done` while work is unclosed is a bypass of the reviewer's authority. (Substrate-enforced gate at `internal/story/store.go`; see also `pr_story_terminal_gate`.)
- **Treating reviewer rationale as advisory.** Cited gaps are blocking; cited principle ids are load-bearing.
- **Re-submitting the same evidence package on a retry without addressing the gaps.** The reviewer will reject again with the same rationale; the loop converges only when the orchestrator changes what it submits.

## Dispute path

If a contract rejects evidence and the orchestrator believes the evidence is correct, dispute it through the contracted channel. If the dispute is rejected, stop and report to the user. Do not improvise around the verdict.
