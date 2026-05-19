---
name: review
category: review
validation_mode: llm
review_required: false
tags: [v4, lifecycle, workspace]
---
# Review Contract

Reads the work a prior contract delivered and returns a verdict on
whether the evidence satisfies the story's review-criteria plus the
active principles. Review is the chain's judgment surface — the
sibling that turns a delivered work task into an accepted (or
rejected) outcome.

## What it does

- Reads the delivered branch and the prior task's evidence ledger
  rows.
- Reasons about whether the evidence satisfies the story's
  `review-criteria.md` plus the active principles.
- Writes one ledger row tagged `task_id:<this_review_task>`,
  `kind:verdict` carrying the structured rationale: cited
  principle ids, named gaps (each tied to an AC or principle), and
  the accept/reject decision.

## How

Read-only across the codebase, the git tree, and the ledger. No
file edits, no commits, no pushes. The review task never touches
the worktree.

## Evidence required

- The verdict ledger row id.
- The principle ids cited in the rationale.
- The prior task id and the commit SHA the verdict applies to.
- On rejection, the successor task id minted by this contract.

## Review policy

Review is a base case. There is no reviewer of reviewer. The
recursion terminates here. Acceptance is rendered as the absence
of a successor on the chain; rejection is rendered as a fresh
successor task minted by this contract.

## On rejection

Mint a successor work task carrying the operator-voiced gap:
`task_add(action=contract:<reviewed_action>, prior_task_id=<rejected_task>,
prompt=<feedback citing the gap>)`. The successor's prompt carries
the verdict's gap text verbatim, so the next attempt addresses the
specific cited gap (per `pr_pipeline_authority`).

## Closure

The review task closes with `status=closed` regardless of verdict.
`outcome=success` means the review happened (a verdict row exists);
it does NOT mean acceptance. Acceptance is rendered as the absence
of a successor on the chain — the orchestrator reads the chain
shape via `task_walk(story_id)`, not the review task's outcome
field.

## Limitations

- No edits, no commits, no pushes, no merges — review is
  read-only.
- No substrate-level auto-pairing — the review task exists only
  because the orchestrator's plan step minted it under a
  judgment-shape contract.
- The review task cannot close without writing a verdict ledger
  row.
