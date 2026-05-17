---
name: story_close
category: story-close
dispatch_class: mechanical
validation_mode: structural
evidence_required: |
  Ledger row tagged kind:close-evidence carrying:
   - resolution:<code>  (default "delivered" when caller did not supply)
   - JSON payload with review_task_id, verdict_row_id, chain_size,
     contract_id (the document id of the story_close contract the
     gate resolved at close time; "" when neither a project-scope
     nor a system-scope contract is present).

  Plus a story.status_change ledger row authored in the same call,
  walking the story to done via UpdateStatusDerived.
tags: [v4, lifecycle, system]
---
# Story Close Contract

The mechanical close gate of a story. story_close is the only path
that transitions a story to `done`. It is structural-only: no LLM
call, no shell-out, no agent dispatch — same tier as
`pr_story_terminal_gate`'s in-Go enforcement at
`internal/story/store.go`.

The implementation lives at `internal/client/story_close.go`. This
contract body is the authoritative prose surface that documents the
gate's named steps; the Go code is the structural enforcement. Both
are kept in lock-step — a substrate-primitive change touching the
gate (adding/removing a step, changing evidence shape, altering the
contract-resolution semantics) updates this contract body in the
same commit per `pr_substrate_model` and the story_review rubric §4.

## What it does

The gate runs five named steps, in order. Any step that detects a
gap appends a `StoryCloseGap` and the gate refuses without mutating.
On a gap-free chain the gate writes one `kind:close-evidence` ledger
row, then walks the story to `done` via `UpdateStatusDerived`.

1. **Open-task check** — every `kind=work` task on the chain whose
   status is in {planned, published, enqueued, claimed, in_flight}
   surfaces a `chain:open_work` gap citing the open task ids. Closing
   a story with open work is a substrate invariant (cite
   `pr_story_terminal_gate`).

2. **Story-review verdict consumption** — the latest
   `contract:story_review` task on the chain must be closed and must
   have authored a `kind:verdict` ledger row tagged `verdict:pass`.
   Absent / open / failing review surface
   `story_review:absent | story_review:open | story_review:fail` gaps.
   The verdict row IS the acceptance surface; the orchestrator's
   contract-prescribed action on `verdict:pass` is to invoke
   `story_close` (see `story_review.md` Verdict-handling).

3. **Template-field roll-up** — the resolved story_template for the
   story's category declares the fields gated at the `done`
   transition. The gate rolls `kind:template-field:<name>` ledger
   rows up onto `story.fields` (last-write-wins, newest-first) BEFORE
   running `EvaluateTransition`, so the gate-check reads the freshly
   populated map. Carrier rows for field names the resolved template
   does NOT declare are silently ignored. Missing fields surface
   `template:<name>:missing` gaps.

4. **Deploy-converge check** — defense-in-depth that catches pprod
   regressions between release and close. The gate finds the latest
   `kind:release-evidence` row, reads `pushed_sha:` from its tags,
   and resolves the project's pprod commit via
   `c.resolvePprodCommit` (caller override → satellites-self
   fallback → `release-evidence:no-deploy-endpoint`). On mismatch,
   walks project-scoped release-evidence rows newest-first to find
   an ancestor row whose pushed_sha prefix-matches the pprod commit;
   accepts when the story's release row is older than the ancestor.
   Surfaces `release-evidence:absent`, `deploy:behind`, or
   `release-evidence:no-deploy-endpoint` as appropriate.
   `skip_deploy_check=true` suppresses the per-commit comparison
   (release-evidence:absent still fires when no row exists).

5. **Close-evidence + status walk** — on a gap-free chain, the gate
   appends one `kind:close-evidence` ledger row carrying the
   resolution slot + JSON payload (review_task_id, verdict_row_id,
   chain_size, contract_id), then walks the story to `done` via
   `UpdateStatusDerived`. Both writes happen inside the same MCP
   call; the orchestrator does NOT call `story_update_status(done)`
   directly anywhere on the close path (would bypass this gate;
   cite `pr_story_terminal_gate`).

## How

Structural only. No LLM call, no shell-out, no agent dispatch. The
verb runs in the same process that handled the MCP request and
writes one ledger row + one story status transition. Same tier as
`pr_story_terminal_gate`'s in-Go enforcement at
`internal/story/store.go`.

## Project-scope override

A project may author a `name=story_close, scope=project,
project_id=<…>` contract document via `contract_add`. The Go gate
resolves the contract via `Documents.ResolveByName` which walks
project → workspace → system and returns the first hit. The resolved
contract's document id lands on the `kind:close-evidence` payload's
`contract_id` field, so the operator and the reviewer can see which
contract prose the gate cited from the ledger row alone.

The override does NOT change the gate's behaviour — the five named
steps remain structural Go invariants. The override changes which
contract prose the operator and reviewer read at close time, and
which document id is recorded on the close-evidence row. Adding a
new project-scope close contract therefore requires only authoring
a new contract doc; no Go change. Cite
`pr_mandate_configuration_over_code` (the framing alias for
`pr_substrate_model`'s configuration-over-code mandate).

## Verdict-handling — what the orchestrator does next

This contract is invoked by the orchestrator as the verdict-handling
step the `story_review` contract names. On `verdict:pass`, the
orchestrator calls `story_close(story_id=<…>)`; the verb walks the
story to done mechanically. On `verdict:fail`, the orchestrator does
NOT invoke `story_close` — instead it mints
`task_add(action=contract:develop, prior_task_id=<rejected>)` per
`pr_pipeline_authority` and dispatches the iter-N+1 attempt.

The orchestrator never calls `story_update_status(done)` directly on
the close path. The only sanctioned path to `done` is this verb;
direct status updates bypass the gate and violate
`pr_story_terminal_gate`.

## Limitations

- No edits, no commits, no pushes — story_close is a read + one
  ledger row + one story status transition.
- The gate refuses to walk a story already at `done` (returns
  `story:already_done` gap, no mutation).
- The gate cannot close a story whose chain has an absent / open /
  failing `contract:story_review` task — see step 2 above.

## Citations

- `pr_substrate_model` — configuration-over-code mandate.
  Contract-resolution and close-evidence carry the contract id so
  the operator authoring a project-scope override sees the override
  taking effect from the ledger row alone, without a code change.
- `pr_story_terminal_gate` — the substrate's structural invariant
  that a story cannot transition to `done` while its task chain has
  open work. Same tier as this gate.
- `pr_evidence_audit` — the `kind:close-evidence` row + the
  `story.status_change` row are the audit-of-record for the close.
  Every gap the gate surfaces cites file:line / row id / task id
  detail so the operator can reconcile the chain.
- `pr_pipeline_authority` — `verdict:fail` is the operator's voice;
  the orchestrator addresses cited gaps via the iter-N+1 develop
  task, not by re-invoking `story_close`.
