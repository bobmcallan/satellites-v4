---
name: deploy
category: deploy
validation_mode: llm
required_role: role_orchestrator
review_required: false
tags: [v4, lifecycle, workspace]
---
# Deploy Contract

The **post-merge converge attestation**. Waits for pprod to report
running the commit that `contract:merge_to_main` pushed to origin
before the chain is allowed to advance to `story_close`. Until this
contract closes `outcome=success`, the story is not done — even if
main carries the merged SHA. Cite `pr_substrate_model`,
`pr_lifecycle_shape`.

The contract exists because today's `merge_to_main` skill stops at
`git push origin main`; the CI build, release, and pprod
auto-update happen asynchronously outside the substrate's audit
chain. Without this phase, a story closes as "shipped" while the
deployed binary serving pprod requests is still the previous
release — the gap captured live at `ldg_40eee079` (sty_72e36256
iter-1 AC7 dogfood).

## What it does

- Fetches the merged-HEAD SHA from `origin/main` (the SHA that
  `contract:merge_to_main` published in its prior task).
- Resolves the pprod base URL (default
  `https://satellites-pprod.fly.dev`; override via
  `SATELLITES_PPROD_URL` env knob — no CLI flag, no in-prompt
  override).
- Polls `${PPROD_URL}/api/v1/system/version` on a 30s cadence
  (override via `WAIT_INTERVAL_SEC`); compares the response's
  `.commit` field against the merged SHA via prefix match.
- On match: emits one `kind:deploy-evidence` ledger row and
  closes `outcome=success`.
- On bounded-wait exhaustion (default 30min; override via
  `WAIT_TIMEOUT_SEC`): emits the same ledger row carrying the
  literal `TIMEOUT …` line and closes `outcome=failure`.

The full bash recipe lives in `skill:wait_for_pprod_deploy`. The
contract body is the rubric the reviewer / `task_walk` chain-shape
gate enforces; the skill body is the recipe the dispatched
executor runs one command at a time.

## How

Read-only inspection + `git rev-parse origin/main` + `curl` to
the pprod `/api/v1/system/version` route + `jq` to parse the
response. No git writes, no file edits, no commits, no pushes,
no tag operations. The executor reads the skill via
`satellites-client skill get --name wait_for_pprod_deploy` and
runs each step one command at a time, capturing stdout + stderr
verbatim per `pr_evidence_audit`.

## Pre-deploy gate

Before invoking the skill, call `task_walk(story_id=<story>)`
and verify:

- (a) a closed `contract:merge_to_main` work task is on the
  chain (deploy without a merge has nothing to wait for);
- (b) no open `contract:deploy` work task with a later
  `created_at` precedes this one (the substrate's
  auto-supersession protects against double-dispatch).

On either failing, the contract aborts and the operator
reconciles before re-attempting.

## Evidence required

A single `kind:deploy-evidence` ledger row tagged
`phase:deploy, kind:deploy-evidence, task_id:<deploy_task>`
carrying:

- The literal `MATCH …` line (success) or `TIMEOUT …` line
  (failure) emitted by the skill recipe — the audit-of-record.
- `merged_sha` — the SHA `git rev-parse origin/main` reported.
- `pprod_sha` — the SHA `/api/v1/system/version`'s `.commit`
  field reported on the matching poll (or the last-observed
  value on TIMEOUT).
- `poll_count` — total number of poll attempts.
- `total_elapsed_seconds` — wall-clock between first and final
  poll.
- `timestamp` — RFC3339 UTC string at close time.
- The full poll trail (every intermediate `poll=N …` line) in
  the row content so a future reader can reconstruct the
  converge progression.

## Review policy

Deploy is execution-shape. `review_required: false`. No
reviewer is dispatched; the pprod endpoint's response is the
only judgment that runs (the prefix-match against the merged
SHA either succeeds or the bounded wait exhausts). This is the
same shape as `contract:commit` and `contract:merge_to_main`,
and is consistent with the `pr_review_required_gate` opt-in
matrix.

## Limitations

- Bounded wait only. On TIMEOUT the contract closes
  `outcome=failure`; the operator's recourse is to investigate
  the deploy pipeline (CI build, release publish, pprod
  auto-update) — NOT to extend the deadline and retry against
  a still-broken deploy. No "skip the wait" flag, no
  "fast-path close success on TIMEOUT" branch. Cite
  `pr_no_unrequested_compat`.
- The contract polls one environment (pprod). Multi-environment
  staging / canary deploys are a separate concern; this
  contract attests the production target only.
- The contract does NOT republish, redeploy, or roll back. It
  is a wait-and-attest phase; remediation belongs in a fresh
  story.
- A sustained endpoint-error (DNS / TLS / 5xx) propagates
  through to the TIMEOUT path — the bounded wait continues
  polling so a transient hiccup does not fail the deploy
  permanently, but a deadline-long outage still terminates
  the contract.
- No tag pushes, no merge-commit authoring, no branch
  deletion — those are owned by other contracts.
