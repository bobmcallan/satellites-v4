---
name: merge_to_main
category: merge_to_main
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Merge to Main Contract

Advances local main to incorporate a finished work branch. The
merge must be fast-forward — any state requiring a merge commit is
a structural drift the contract refuses to paper over.

## What it does

- Confirms the source branch is ahead of local main with no
  divergence.
- Runs `git merge --ff-only` to advance main to the source ref.

## How

Read-only inspection + `git merge --ff-only`. No commit
authoring; the merge is an updated ref pointer, not new history.

## Pre-merge gate

Before running the merge, call `task_walk(story_id=<story>)` and
verify the chain shape:

- (a) the develop task whose branch is being merged closed with
  `outcome=success`;
- (b) every `kind=review` successor of that develop task closed;
- (c) no open `kind=work` task on the chain has
  `prior_task_id=<develop_task_id>` (no in-flight retry develop).

On any of these failing, the contract aborts and the operator
reconciles before re-attempting. The substrate verb is
`task_walk(story_id)`; no new helper is required — the chain
shape is already present in the returned `tasks[]` and
`action_summary`.

## Evidence required

- The source ref being merged (branch name or SHA).
- The pre-merge SHA of local main.
- The post-merge SHA of local main.
- `git status` clean after the merge.
- Explicit confirmation that the merge resolved fast-forward.
- Chain-shape attestation: a ledger row capturing the
  `task_walk(story_id)` excerpt with `review_total`,
  `review_open`, and the per-task `prior_task_id` / `status` rows
  for the develop task and any successors.

## Review policy

Merge is execution-shape. No reviewer is dispatched. The
chain-shape gate IS the structural integrity check; reviewer
judgment was applied earlier in the chain.

## Limitations

- Fast-forward only. If the merge would require a non-ff resolve,
  the contract aborts; the operator reconciles divergence in a
  follow-up before re-attempting.
- No force, no merge commits, no rebases.
- No `git push` — pushing main is a separate concern.
- No branch deletion as part of this contract.
