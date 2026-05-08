---
id: pr_reviewer_voice_authoritative
name: reviewer_voice_authoritative
scope: workspace
tags:
  - process
  - mandate
  - reviewer
  - v4
---
Reviewer rejections are the operator's voice; the orchestrator's response is to address the cited gaps, not to bypass the chain.

## What this means

The autonomous reviewer service runs the rubric the operator (via the seed) has codified. When it rejects a work-task close, the rejection carries the operator's standard for "done" — it is not noise to be routed around. The substrate is append-only: a rejection is rendered as a fresh `kind=work` task minted by the reviewer's contract prose via `task_add(prior_task_id=…)`, and the chain on `task_walk(story_id)` is the audit-of-record. The orchestrator's job is to dispatch that fresh attempt and address each gap the verdict cited.

## What it forbids

- Closing a story while its task chain has open work tasks. The story's chain reflects the substrate's notion of in-flight work; transitioning to `done` while work is unclosed is a bypass of the reviewer's authority.
- Treating a reviewer rationale as advisory. The verdict's text — including the principle ids it cites and the specific gaps it names — is the close-criteria checklist for the iter-2 retry.
- Re-submitting the same evidence package on a retry without addressing the gaps. The reviewer will reject again with the same rationale; the loop converges only when the orchestrator changes what it submits.

## Citation

This principle backs the orchestrator pre-flight rules in the `claude_orchestrator` agent doc and the `default_agent_process` artifact, which the substrate surfaces to every Claude session via `story_get.agent_process`.
