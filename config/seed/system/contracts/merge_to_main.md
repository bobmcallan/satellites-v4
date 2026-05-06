---
name: merge_to_main
category: merge_to_main
delivers_by: releaser_agent
reviewed_by: story_reviewer
evidence_required: |
  Ledger rows tagged task_id:<merge_to_main_task>, kind:evidence:
  1. Source branch (or "direct on main").
  2. Pre-merge log + post-merge SHA.
  3. `git status -uno` clean.
  4. Confirmation: fast-forward only.
tags: [v4, lifecycle, system]
---
# Merge to Main Contract

Aligns local main with origin after push. The v4 trunk-based flow
does not produce merge commits — merges are fast-forward only. When
work landed directly on main (the common case), this contract is a
no-op confirmation.

## What it does

- Confirms current branch + sync state.
- Runs `git merge --ff-only` to advance local main.
- Records SHA + ff-only confirmation on close evidence.

## How

Read-only git inspection plus checkout/branch/merge with the
`--ff-only` constraint.

## Limitations

- Fast-forward only. If a merge commit would be required, the
  contract aborts and the operator files a follow-up.
- Cannot delete branches.
- Cannot push.
