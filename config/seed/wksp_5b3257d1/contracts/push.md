---
name: push
category: push
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Push Contract

Ships an already-committed branch to its upstream remote. Push is
the thinnest contract in the lifecycle — it exists to record that
the work has left the local environment and to surface the remote's
response.

## What it does

- Fetches the upstream for pre-push sanity.
- Pushes the current branch to its tracked upstream as a
  non-force, fast-forward update.

## How

Read-only inspection + `git fetch` + `git push`. No edits, no
amends, no new commits.

## Evidence required

- The commit SHA and subject being pushed.
- The verbatim `git push` output (or the `X..Y branch -> branch`
  line confirming a non-force update).
- Confirmation that no force, branch deletion, or unrelated tag
  push occurred.

## Review policy

Push is execution-shape. No reviewer is dispatched. The remote's
accept/reject (non-fast-forward, hook failure, refusal) is the
only judgment that runs.

## Limitations

- No force push under any circumstance.
- No branch deletion.
- No tag pushes outside the explicit story scope.
- No file edits, no amends, no rebases — push performs no work
  beyond shipping the existing commits.
- On rejection (non-fast-forward, hook failure, remote refusal),
  surface the error and stop. Recovery belongs in a fresh story,
  not a retry-with-force.
