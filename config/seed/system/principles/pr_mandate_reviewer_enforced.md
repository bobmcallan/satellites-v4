---
id: pr_mandate_reviewer_enforced
name: Mandate enforced by reviewer, not substrate
scope: system
tags:
  - process
  - mandate
  - reviewer
  - v4
---
Every story must flow `plan → orchestrator-composed contracts → story_close`. `plan` is mandatory at the front; `story_close` is mandatory at the end; the orchestrator chooses the contracts that run in between. The reviewer agent enforces this floor; the substrate does not.

## Enforcement

The mandate is enforced in two places, both reviewer-driven:

- **Plan submission.** When the orchestrator submits its proposed plan via `task_submit(kind=plan, tasks=[…])`, the substrate validates structural invariants (plan task first, every work paired with a review, agent capability matches) and rejects on violation. The `story_reviewer` agent then runs against the published plan task and checks that the action list begins with `contract:plan` and ends with `contract:story_close`. A plan missing the floor is rejected; the substrate spawns a successor work + paired planned-review pair so the orchestrator can revise and resubmit.

- **Per-task close loop.** When each work task closes via `task_submit(kind=close)`, the substrate publishes the paired review task. The autonomous reviewer service (`internal/reviewer/service`) claims it, looks up the rubric by capability match (`reviews:` list on agent docs), reads the evidence against the contract's rubric, and either accepts or rejects. The mandate is implicit at this layer — the reviewer cannot accept a `story_close` for a story that never had a `plan` because there is no evidence chain to point at.

The substrate has no `mandatory_slot_missing` gate, no `required_slots` resolver, no per-tier workflow merge. Those surfaces are gone in favour of this principle and the reviewer agents that cite it.

## Why configuration, not code

A Go-coded mandate (resolved across system / workspace / project / user tiers and enforced at `workflow_claim` time) duplicates the orchestrator's job. The orchestrator's purpose is to compose the contract sequence per story; a substrate gate that pre-decides part of the sequence can — and at runtime did — disagree with the orchestrator's choice, with no recovery path that does not bypass one of the two layers.

Moving the mandate into a principle the reviewer cites means: adding a new mandatory contract is a configuration edit (this principle's text + the reviewer agent's rubric), not a Go change to a slot-algebra package.

## How to apply

- **Orchestrator.** When composing a plan, ensure `contract:plan` leads the task list and `contract:story_close` ends it. Other contracts (`contract:develop`, `contract:push`, `contract:merge_to_main`, project-defined actions) are the orchestrator's choice based on the story's shape. Submit the plan via `task_submit(kind=plan, tasks=[…])`; on rejection from a downstream review, the substrate spawns a successor task pair — dispatch a fresh attempt.

- **Reviewer agents.** Cite this principle (`pr_mandate_reviewer_enforced`) when rejecting a plan that omits the floor. On per-task closes, treat the mandate as a precondition — the chain of accepted task closes is what makes the closing review valid.

- **Authors of new contracts.** A new mandatory contract is added by editing this principle and the reviewer agent rubrics, not by changing Go code. A new optional contract needs no principle change — the orchestrator can include it whenever the story warrants.

## Citation

This principle backs `epic:configuration-over-code-mandate`. See `docs/architecture-configuration-over-code-mandate.md` for the full design. The task-chain orchestration model (sty_c6d76a5b) replaced the prior CI-instance lifecycle; this principle's enforcement surface moved with it.
