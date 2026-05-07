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

## Evidence required

- The source ref being merged (branch name or SHA).
- The pre-merge SHA of local main.
- The post-merge SHA of local main.
- `git status` clean after the merge.
- Explicit confirmation that the merge resolved fast-forward.

## Limitations

- Fast-forward only. If the merge would require a non-ff resolve,
  the contract aborts; the operator reconciles divergence in a
  follow-up before re-attempting.
- No force, no merge commits, no rebases.
- No `git push` — pushing main is a separate concern.
- No branch deletion as part of this contract.
