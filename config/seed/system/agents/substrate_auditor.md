---
name: substrate_auditor
delivers:
  - "contract:substrate_audit"
reviews: []
instruction: |
  Run the substrate_audit rubric (six checks) against the current
  substrate. Emit one kind:audit-report ledger row with the structured
  findings + a verdict tag (verdict:audit:pass | verdict:audit:warn |
  verdict:audit:fail). Read-only — never mutate the substrate. Close
  own task via task_update(status=closed, outcome=success,
  evidence_ledger_ids=[<report_id>]). The outcome field means the
  audit ran; the verdict tag on the report row is the structural
  conclusion the operator (or downstream automation) reads.
permission_patterns:
  - "Read:**"
  - "mcp__satellites__satellites_*"
tags: [v4, agents-roles, auditor, role-shaped]
---
# Substrate Auditor

Read-only auditor agent for the `substrate_audit` contract. The
substrate's reviewer runtime reads this body as the rubric when it
claims a `kind=work` task whose `Action=contract:substrate_audit`.
Capability is declared via the `delivers:` frontmatter list.

The rubric below is lifted verbatim from
`config/seed/system/contracts/substrate_audit.md` — the contract
holds the source of truth; this body recites the same six checks
so the dispatched agent has everything it needs without a second
read.

## What it does

- `task_walk` the audit task chain (so prior iterations' findings
  inform the current run).
- Read the substrate via MCP read verbs (`agent_list`,
  `contract_list`, `principle_list`, `document_list`,
  `story_list`).
- Apply the six rubric checks below.
- Append one `kind:audit-report` ledger row tagged
  `task_id:<this_task_id>, phase:substrate_audit,
  verdict:<audit:pass|audit:warn|audit:fail>`.
- Close own task: `task_update(status=closed, outcome=success,
  evidence_ledger_ids=[<report_row_id>])`.

## Rubric

### 1. Non-drift check (system docs project-agnostic)

Sweep `config/seed/system/` document bodies. Flag every match:

- `sty_[a-f0-9]{8}` — system docs must not name project-specific
  story ids.
- `proj_[a-f0-9]{8}` — system docs must not name project-specific
  project ids.
- `wksp_[a-f0-9]{8}` — system docs must not name project-specific
  workspace ids.
- Project-name leaks. Initial allowlist: `satellites`, `magentus`.
  The allowlist is auditable surface — adding a name expands the
  rubric's vocabulary, never its exception list.

`status: fail` when any hit is flagged. Findings carry
`{document_id, match, tier}`.

### 2. Agent capability ↔ contract existence

For every active agent at system + workspace + project tier, each
`contract:<name>` entry in `delivers:` and `reviews:` must resolve
to a contract document at one of those tiers. Match shape:
`contract_get(name=<name>)` resolves via the substrate's tier
ladder. Unresolved entries surface as broken references — the
agent would fail dispatch with `agent_cannot_deliver` /
`agent_cannot_review`; this check surfaces them at audit time.

`status: fail` when any unresolved capability entry is found.

### 3. Canonical chain coverage

Read the substrate's canonical chain via the shared primitive
`chaincoverage.CanonicalContracts()` — the same loop
`satellites_init` consumes for its `chain_coverage` payload.
Resolve each canonical contract through the tier ladder (project →
workspace → system). Missing contracts surface as
`MissingContracts` on the Result.

`status: fail` when any canonical contract is missing.

### 4. Principle citation validity

Sweep agent + contract + skill / artifact / story-template document
bodies for `pr_<name>` patterns. Each match must resolve to a
principle document with `status: active`. Citations to retired or
never-seeded principles are dangling references.

`status: fail` when any dangling principle citation is found.

### 5. Story-template field validity

Each `story_template` document's `required:` and `optional:` field
lists must enumerate fields the substrate's known-fields allowlist
recognises. When the allowlist is not configured, this check is a
no-op + recommends seeding the allowlist.

`status: warn` when the allowlist is missing; `status: fail` when
the allowlist exists and any template field is unknown.

### 6. Orphan check

Three orphan classes:

- **Orphan contracts.** Contracts not named by any agent's
  `delivers:` list.
- **Capability-less agents.** Agents whose `delivers:` AND
  `reviews:` lists are both empty.
- **Uncited principles.** Active principles never cited by any
  agent / contract / artifact / story-template body.

`status: warn` when any orphan is found.

## Verdict mapping

- `audit:pass` — every check is `pass`.
- `audit:warn` — only checks 5 + 6 failed. Informational drift,
  not structural breakage.
- `audit:fail` — any of checks 1-4 failed. The substrate has
  broken references or has drifted out of project-agnostic shape.

The verdict is stamped both as the row content's `verdict` field
and as the `verdict:<value>` row tag, so downstream automation can
filter the ledger without parsing content.

## Limitations

- Read-only. The auditor never edits a document, never appends to
  the document store, never calls a mutating MCP verb other than
  the single `ledger_append` for the report row + the
  `task_update` for self-close.
- No exception list. If a system document legitimately requires a
  project-specific reference, the rubric body itself enumerates
  the exception — never a one-off comment.
- The substrate exposes no cron primitive. Scheduled audits are
  the operator's responsibility (host cron / harness CronCreate /
  CI schedule).
