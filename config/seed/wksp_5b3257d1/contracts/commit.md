---
name: commit
category: commit
dispatch_class: hot
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Commit Contract

Publishes the developer's already-committed work to its upstream
remote under the work branch. This is the **substrate-level
publish action**, not a release — release is the
`merge_to_main` contract's job. The commit contract exists to
record that the developer's commit(s) have left the local
environment and to surface the remote's response.

## What it does

- Fetches the upstream for pre-push sanity.
- Pushes the current work branch to its tracked upstream as a
  non-force, fast-forward update.

## How

Read-only inspection + `git fetch` + `git push`. No edits, no
amends, no new commits — the developer is the single writer for
source files and version metadata.

## Evidence required

- The commit SHA and subject being published.
- The verbatim `git push` output (or the `X..Y branch -> branch`
  line confirming a non-force update).
- Confirmation that no force, branch deletion, or unrelated tag
  push occurred.

## Review policy

Commit is execution-shape. No reviewer is dispatched. The remote's
accept/reject (non-fast-forward, hook failure, refusal) is the
only judgment that runs.

## Limitations

- No force push under any circumstance.
- No branch deletion.
- No tag pushes outside the explicit story scope.
- No file edits, no amends, no rebases — commit performs no work
  beyond publishing the existing commits.
- No release semantics — release is the `merge_to_main`
  contract's atomic operation.
- On rejection (non-fast-forward, hook failure, remote refusal),
  surface the error and stop. Recovery belongs in a fresh story,
  not a retry-with-force.
