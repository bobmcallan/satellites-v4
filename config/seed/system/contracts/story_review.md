---
name: story_review
category: story-review
dispatch_class: heavy
validation_mode: llm
evidence_required: |
  Ledger row tagged task_id:<story_review_task>, kind:verdict carrying
  verdict:pass | verdict:fail in row.Tags, plus structured rationale:
  cited principle ids, named gaps (each tied to an AC or principle),
  the prior task id, the commit SHA the verdict applies to, and on
  pass the resolution slot the story_close verb will record.
tags: [v4, lifecycle, system]
---
# Story Review Contract

The AC-coverage judgment surface for a story. Story review is the
single LLM gate between the develop+integration work and the
mechanical `story_close` transition: it reads the story's task chain
and evidence ledger rows, applies the rubric, and writes one
`verdict:pass` or `verdict:fail` ledger row. The mechanical
`story_close` MCP verb consults that verdict structurally; reasoning
lives here.

## What it does

- Reads the story's task chain via `task_walk(story_id)` — the
  structural truth from the chain shape, not from agent prose.
- Reads the evidence rows on each prior task via `ledger_list` /
  `ledger_get`; cited `ldg_…` ids are first-class evidence and must
  be dereferenced (per `pr_evidence_audit`).
- Maps each acceptance criterion in the story to specific work-task
  evidence (file:line, command output, ledger row id, commit SHA).
- Writes one `kind:verdict` ledger row tagged
  `task_id:<this_review_task>`, `verdict:pass` or `verdict:fail`,
  carrying the structured rationale.
- Closes its own task via
  `task_update(id=<review_task>, status=closed,
  outcome=success, evidence_ledger_ids=[<verdict_row_id>])`.

`outcome=success` means the review happened; it does NOT mean
acceptance. Acceptance is `verdict:pass` on the verdict row; the
mechanical `story_close` reads the tag, not the outcome field.

## How

Read-only across the codebase, the git tree, and the substrate.
The review task never edits files, never commits, never pushes,
never claims a worktree.

## Rubric

### 1. Read structural state via `task_walk`

Verify the task chain by calling `task_walk({story_id})` before
demanding prose recital. The substrate exposes the ordered chain
and per-action summary; structural truth comes from that sequence
rather than from the implementing agent repeating it in prose.
Only when `task_walk` returns no chain (no tasks submitted yet for
the story) is prose recital relevant.

### 2. AC coverage

Every acceptance criterion in the story must map to a specific
work-task action in the plan. On task close, every AC the closing
task claims to satisfy must cite verifiable evidence (file:line,
command output, ledger row id, commit SHA). Declarative claims
("AC satisfied", "tests pass") without citation are rejected.

### 3. Evidence completeness

Cite `pr_evidence_audit`. The evidence markdown must be
reproducible: every claim should be re-runnable by a third party
from the ledger row alone. Missing command output, missing file
references, or evidence that points to ephemeral state ("I ran the
test locally and it passed") is a `verdict:fail`.

`evidence_ledger_ids` are first-class evidence. When a close
references prior ledger rows by id, dereference each id via
`ledger_get` and read the row's content as part of the evidence
packet. Do NOT reject for missing inline duplication when the
cited rows contain the content the rubric requires — content
reachability + traceability is the bar `pr_evidence_audit` sets,
not duplication. The exception: when a cited row's content does
NOT actually satisfy the rubric (e.g. plan-md missing the AC
mapping table the reviewer asked for), reject for the missing
CONTENT, not for the citation form.

### 4. Substrate evolution and rubric updates

Cite `pr_substrate_model` and `pr_no_unrequested_compat`. When a
plan describes a substrate-primitive change (verb add/remove/
rename, schema field change, contract category change, MCP
signature change, agent doc body change, or contract doc body
change), the plan MUST contain a "rubric updates" checklist
enumerating which rubric files are updated in the SAME commit as
the substrate change. Without that checklist, the verdict is
`fail` with the cited gap:
*"Plan touches substrate primitive X but no rubric-updates list."*

Pure markdown / docs / test changes that do NOT touch substrate
primitives are exempt from this gate.

### 5. Principle citation on rejection

Every `verdict:fail` row must cite the specific principle id the
rejection rests on (e.g. `pr_evidence_audit`,
`pr_no_unrequested_compat`, `pr_root_cause`). The orchestrator
reading the verdict knows which class of fix to make.

## Verdict format

- `verdict:pass` — rationale cites the ACs satisfied and any
  principles honoured. The mechanical `story_close` verb reads
  this tag structurally to permit the `done` transition.
- `verdict:fail` — rationale cites the failing principle + the AC
  or evidence gap. The orchestrator (per `pr_pipeline_authority`)
  reads the verdict and mints the iter-N+1 work task via
  `task_add(prior_task_id=…)`, then dispatches that fresh attempt.

Verdict vocabulary on the row tags is `verdict:pass | verdict:fail`
(distinct from `development_reviewer`'s
`verdict:accepted | verdict:rejected`). Vocab harmonisation across
all reviewers is filed as a separate follow-up per the
`sty_b97dda00` story body.

## Verdict-handling

The orchestrator's contract-prescribed next action when this review
task closes:

- **`verdict:pass`** — invoke the mechanical `story_close` verb via
  `mcp__satellites__story_close(story_id=<…>)`. The verb runs the
  structural gate documented in the `story_close` contract,
  appends a `kind:close-evidence` ledger row, and walks the story
  to `done` via `UpdateStatusDerived` in the same call. The
  orchestrator does NOT call `story_update_status(done)` directly
  on the close path — that would bypass the gate and violate
  `pr_story_terminal_gate`.
- **`verdict:fail`** — author a fresh iter-N+1 work task via
  `task_add(action=contract:develop, prior_task_id=<rejected>,
  prompt=<addresses cited gaps>)` per `pr_pipeline_authority`. The
  verdict text — including principle ids it cites and gaps it names
  — IS the close-criteria checklist for the iter-N+1 retry. Do NOT
  invoke `story_close` after a `verdict:fail` row.

This step is named in this contract so the orchestrator executes it
from prose rather than from implicit workflow ordering. Cite
`pr_mandate_configuration_over_code` (the framing alias for
`pr_substrate_model`'s configuration-over-code mandate).

## Review policy

`story_review` is a base case. There is no reviewer of the
story_reviewer. The recursion terminates here. The reviewer's
verdict row IS the acceptance surface; the mechanical `story_close`
verb is the structural enforcement.

## Limitations

- No edits, no commits, no pushes — story_review is read-only.
- The review task cannot close without writing a `kind:verdict`
  ledger row carrying `verdict:pass` or `verdict:fail` on the
  row's tags.
- The mechanical `story_close` verb refuses to walk a story to
  `done` while the review task is absent, open, or carries
  `verdict:fail` — see the `story_close` contract for the gate's
  named steps; the structural enforcement lives at
  `internal/client/story_close.go`.
