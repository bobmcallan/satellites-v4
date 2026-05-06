---
id: pr_reviewer_voice_authoritative
name: Reviewer rejections are the operator's voice
scope: system
tags:
  - process
  - mandate
  - reviewer
  - v4
---
Reviewer rejections are the operator's voice; the orchestrator's response is to address the cited gaps, not to bypass the chain.

## What this means

The autonomous reviewer service runs the rubric the operator (via the seed) has codified. When it rejects a work-task close, the rejection carries the operator's standard for "done" — it is not noise to be routed around. The substrate's rejection-append loop spawns a successor `kind=work` + paired planned-`kind=review` pair carrying `prior_task_id`; the orchestrator's job is to dispatch a fresh attempt that addresses each gap the verdict cited.

## What it forbids

- Closing a story while its task chain has open work tasks. The story's chain reflects the substrate's notion of in-flight work; transitioning to `done` while work is unclosed is a bypass of the reviewer's authority.
- Treating a reviewer rationale as advisory. The verdict's text — including the principle ids it cites and the specific gaps it names — is the close-criteria checklist for the iter-2 retry.
- Re-submitting the same evidence package on a retry without addressing the gaps. The reviewer will reject again with the same rationale; the loop converges only when the orchestrator changes what it submits.

## Citation

This principle backs the orchestrator pre-flight rules in `config/seed/agents/claude_orchestrator.md` and the artifact `config/seed/artifacts/default_agent_process.md` that the substrate surfaces to every Claude session via `story_context.agent_process`. It is paired with `pr_mandate_reviewer_enforced`: that principle covers the floor (`plan` first, `story_close` last); this principle covers the reception of rejections within the floor.
