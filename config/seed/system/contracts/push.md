---
name: push
category: push
delivers_by: releaser_agent
reviewed_by: story_reviewer
evidence_required: |
  Ledger rows tagged task_id:<push_task>, kind:evidence:
  1. Commit SHA + subject being pushed.
  2. `git push` output verbatim (or the "X..Y main -> main" line).
  3. Confirmation .version was NOT re-bumped (develop's bump stands).
  4. Confirmation no force / destructive operations were used.
tags: [v4, lifecycle, system]
---
# Push Contract

Ships the develop commit to origin. Push is a thin contract — it
does no work beyond the actual `git push` and its evidence.

## What it does

- `git fetch` for pre-push sanity.
- `git push` (non-force) to the current branch's upstream.
- Records the pushed SHA + remote response.

## How

Minimal command surface: read-only inspection + fetch + push.

## Limitations

- No force push, no branch deletion, no tag pushes without explicit
  story scope.
- Cannot edit files, run tests, or amend the develop commit.
- Cannot re-bump `.version` (single-writer rule: develop is the only
  bumper).
- On rejection (non-fast-forward, hook failure), surface the error
  and stop — no retry, no force.
