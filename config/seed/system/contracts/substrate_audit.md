---
name: substrate_audit
category: substrate_audit
validation_mode: llm
evidence_required: |
  One ledger row tagged task_id:<this_audit_task>, kind:audit-report,
  phase:substrate_audit, verdict:audit:pass | verdict:audit:warn |
  verdict:audit:fail. Content is structured JSON:
  {
    "verdict": "audit:pass" | "audit:warn" | "audit:fail",
    "checks": [
      {"name": "non_drift", "status": "pass|fail|warn", "findings": [...], "recommended_fix": "..."},
      {"name": "agent_capability", ...},
      {"name": "canonical_chain_coverage", ...},
      {"name": "principle_citation_validity", ...},
      {"name": "story_template_field_validity", ...},
      {"name": "orphan_check", ...}
    ],
    "scope": "system" | "workspace:<wksp_id>" | "project:<proj_id>",
    "audited_at": "<RFC3339>"
  }
tags: [v4, lifecycle, system]
---
# Substrate Audit Contract

The continuous drift-detection rubric for the substrate. Reads the
seeded contract / agent / principle / story-template / workflow
documents and reports six structural checks against the substrate's
current shape. Read-only: the auditor never mutates the substrate;
the operator authors fix stories from the report.

## What it does

- Reads the substrate via the MCP read verbs (`agent_list`,
  `contract_list`, `principle_list`, `document_list`, `story_list`).
- Applies the six rubric checks below against the materialised
  document set.
- Writes one `kind:audit-report` ledger row carrying the structured
  findings plus a verdict tag the operator (or downstream automation)
  can filter on without parsing JSON.
- Closes its own task via `task_update(status=closed,
  outcome=success, evidence_ledger_ids=[<report_row_id>])`. The
  `outcome=success` field means the audit ran; the `verdict` tag on
  the report row is the structural conclusion.

## How

The audit task is operator-invoked. Three dispatch surfaces ship:

1. **On-demand CLI verb.** `satellites-client substrate audit`
   mints a `kind:work action=contract:substrate_audit` task naming
   the `substrate_auditor` agent. `satellites-client task run
   <task_id>` dispatches it; the report row lands on the project
   ledger.
2. **On-demand MCP verb.** `mcp__satellites__substrate_audit`
   provides the same mint behavior over the MCP transport. Params:
   `{project_id?: string, workspace_id?: string}`. Returns `{task_id,
   scope, agent_id}`.
3. **Scheduled via operator-side cron.** The operator wires a
   recurring caller (host cron, GitHub Actions schedule, the
   harness `CronCreate` primitive) that invokes the CLI verb on a
   cadence the project sets. The substrate exposes no cron primitive
   of its own — the recurring scheduler is the operator's
   responsibility, not the substrate's.

CI integration is **out of scope** for this contract; see the story
body's "CI integration (optional follow-up)" note.

## Rubric

### 1. Non-drift check (system docs project-agnostic)

Sweep `config/seed/system/` document bodies. Flag any of:

- `sty_[a-f0-9]{8}` references — system docs must not name
  project-specific story ids.
- `proj_[a-f0-9]{8}` references — system docs must not name
  project-specific project ids.
- `wksp_[a-f0-9]{8}` references — system docs must not name
  project-specific workspace ids.
- Project-name leaks. A name allowlist enumerated in this contract
  body identifies project-specific names that have leaked into
  system bodies (initial set: `satellites`, `magentus`). The
  allowlist itself is auditable surface — adding a name expands the
  rubric's vocabulary, never its exception list.

Each hit is a `findings[]` entry citing the document id + the matching
substring. `status: fail` when any hit is flagged.

### 2. Agent capability ↔ contract existence

For every active agent at system + workspace + project tier, each
entry in `delivers:` and `reviews:` must resolve to a contract
document that exists at one of those tiers. Match shape:
`contract:<name>` → `contract_get(name=<name>)` resolves via the
substrate's tier ladder. Unresolved capability entries are a
broken-reference fail; the agent will fail dispatch at task_add
time with `agent_cannot_deliver` / `agent_cannot_review`. Surface
those at audit time rather than at dispatch.

`status: fail` when any unresolved capability entry is found.

### 3. Canonical chain coverage

Read the substrate's canonical chain via
`chaincoverage.CanonicalContracts()` — the same primitive
`satellites_init` consumes for its `chain_coverage` payload field.
Resolve each canonical contract through the tier ladder (project →
workspace → system). Missing contracts surface as
`MissingContracts` on the chain-coverage Result.

`status: fail` when any canonical contract does not resolve.

### 4. Principle citation validity

Sweep all agent + contract + skill / artifact / story-template
document bodies for `pr_<name>` patterns. Each match must resolve
to a principle document with `status: active`. Citations to retired
or never-seeded principles are dangling references.

`status: fail` when any dangling principle citation is found.

### 5. Story-template field validity

Each `story_template` document's `required:` and `optional:` field
lists must enumerate fields the substrate's known-fields allowlist
recognises. The allowlist lives on the story store. When the
allowlist is not configured (early substrate boot, test fixtures),
this check is a no-op and recommends seeding the allowlist.

`status: warn` when the allowlist is missing; `status: fail` when
the allowlist exists and any template field is unknown.

### 6. Orphan check

Three orphan classes:

- **Orphan contracts.** Contracts not named by any agent's
  `delivers:` list. Maintained-but-unused noise.
- **Capability-less agents.** Agents whose `delivers:` AND
  `reviews:` lists are both empty. Either the agent was renamed
  and its replacement carries the capability now, or the agent
  is dead.
- **Uncited principles.** Active principles never cited by any
  agent / contract / artifact / story-template body. Either the
  principle is meant to be cited but hasn't landed in any rubric
  yet, or it has been retired in spirit but not in `status:`.

`status: warn` when any orphan is found (orphans are
noise, not breakage).

## Verdict mapping

The audit produces one of three verdicts:

- **`audit:pass`** — every check is `pass`.
- **`audit:warn`** — only checks 5 + 6 (story-template-field-
  validity, orphan-check) failed. Drift is informational, not
  structural breakage.
- **`audit:fail`** — any of checks 1-4 (non-drift, capability,
  chain-coverage, principle-citation) failed. The substrate has
  broken references or has drifted out of project-agnostic shape.

The verdict is stamped both on the row content's `verdict` field
and as a `verdict:<value>` tag on the row, so downstream automation
can filter the ledger without parsing content.

## Audit-report ledger schema

```json
{
  "verdict": "audit:pass" | "audit:warn" | "audit:fail",
  "checks": [
    {
      "name": "non_drift",
      "status": "pass" | "fail" | "warn",
      "findings": [
        {"document_id": "doc_…", "match": "sty_abcd1234", "tier": "system"}
      ],
      "recommended_fix": "Remove project-specific story id from system doc body."
    },
    {"name": "agent_capability", "status": "pass", "findings": [], "recommended_fix": ""},
    {"name": "canonical_chain_coverage", "status": "pass", "findings": [], "recommended_fix": ""},
    {"name": "principle_citation_validity", "status": "pass", "findings": [], "recommended_fix": ""},
    {"name": "story_template_field_validity", "status": "warn", "findings": [], "recommended_fix": "Allowlist not configured."},
    {"name": "orphan_check", "status": "warn", "findings": [], "recommended_fix": "..."}
  ],
  "scope": "system" | "workspace:<wksp_id>" | "project:<proj_id>",
  "audited_at": "<RFC3339>"
}
```

Row tags: `kind:audit-report, phase:substrate_audit,
task_id:<this_audit_task>, verdict:<audit:pass|audit:warn|audit:fail>`.

## Review policy

`substrate_audit` is base-case in the recursion: the report row IS
the operator's verdict surface. No `kind=review` sibling is minted;
the auditor is read-only and produces structured output the
operator reads directly. Adding a reviewer would recurse — who
audits the audit reviewer?

## Limitations

- Read-only. The auditor never edits a document, never appends to
  the document store, never calls a mutating MCP verb.
- The report enumerates drift; it does not fix it. The operator
  authors fix stories from the report's `findings[]` entries.
- No exception list. If a system document legitimately requires a
  project-specific reference (rare; the de-projection of the
  system tier in `sty_ab0b47d6` removed every such case), the
  rubric body itself names the enumerated exception — never a
  one-off comment in a document body. The allowlist is auditable
  surface.
- The substrate exposes no cron primitive. Scheduled audits are
  the operator's responsibility (host cron / harness CronCreate /
  CI schedule).
