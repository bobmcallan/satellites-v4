---
name: merge_to_main
category: merge_to_main
dispatch_class: hot
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Merge to Main Contract

The **atomic release operation**. Fast-forwards local main to the
finished work branch, publishes main to origin, watches the
GitHub Actions deploy workflow to completion, and polls pprod's
`satellites_info` until its reported running `commit` matches
the pushed SHA. Failure at any step stops the release without
appending a `kind:release-evidence` row — the operator
reconciles before re-attempting.

## What it does

- Confirms the source branch is ahead of local main with no
  divergence (`git merge-base --is-ancestor main <branch>`).
- Runs `git merge --ff-only` to advance main to the source ref.
- Runs `git push origin main` (non-force) to publish main to
  origin.
- Captures the GitHub Actions workflow run id triggered by the
  main-push (`gh run list --branch main --limit 1`) and watches
  it to completion (`gh run watch <run_id>`) — configurable GH
  timeout (default 600s).
- Polls `mcp__satellites__satellites_info` against the pprod
  server URL until reported `commit` matches the pushed SHA —
  configurable converge timeout (default 300s).

## How

Read-only inspection + `git merge --ff-only` + `git push origin
main` + `gh run watch` + HTTP poll of pprod's `satellites_info`.
No commit authoring; the merge is an updated ref pointer, not new
history.

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

A single `kind:release-evidence` ledger row tagged
`phase:merge_to_main, kind:release-evidence` carrying:

- The source ref being merged (branch name).
- The pre-merge SHA of local main.
- The post-merge SHA of local main (the pushed SHA).
- `git status` clean after the merge.
- Explicit confirmation that the merge resolved fast-forward.
- Chain-shape attestation: a ledger row capturing the
  `task_walk(story_id)` excerpt with `review_total`,
  `review_open`, and the per-task `prior_task_id` / `status` rows
  for the develop task and any successors.
- The `git push origin main` literal output (the
  `X..Y main -> main` line confirming non-force update).
- The GitHub workflow run id captured from
  `gh run list --branch main --limit 1`.
- The `gh run watch <run_id>` exit with `conclusion=success`.
- The pprod converge polling: each `satellites_info` response's
  `commit` field + timestamp, ending with the row whose `commit`
  matches the pushed SHA.

## Review policy

Merge is execution-shape. No reviewer is dispatched. The
chain-shape gate IS the structural integrity check; reviewer
judgment was applied earlier in the chain.

## Limitations

- Fast-forward only. If the merge would require a non-ff resolve,
  the contract aborts; the operator reconciles divergence in a
  follow-up before re-attempting.
- No force, no merge commits, no rebases.
- No tag pushes — only main is published; the work branch is
  already on origin via the prior `commit` contract.
- No branch deletion as part of this contract.
- On GH-watch failure (timeout or `conclusion=failure`), or on
  pprod-converge timeout (pprod's reported `commit` does not
  match the pushed SHA within the converge timeout), the release
  stops and no `kind:release-evidence` row is appended — the
  operator reconciles before re-attempting.
