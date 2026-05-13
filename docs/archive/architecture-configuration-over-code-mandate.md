# Configuration-over-Code Mandate

Status: design note backing **story_4362afb7**, anchoring
`epic:configuration-over-code-mandate`.

This note supersedes the workflow-enforcement portion of
`docs/architecture-orchestrator-driven-configuration.md` (story_64e32e42).
That earlier doc introduced orchestrator-driven plan composition while
keeping a substrate-side workflow-mandate gate (`required_slots` in
markdown frontmatter, parsed and enforced at `workflow_claim` time via
`mandatory_slot_missing`). After landing the orchestrator-composition
work, the gate became redundant and contradictory: the orchestrator's
job is to choose the contract sequence per story, while the gate
pre-decides it from a configuration tier the orchestrator does not
control. The two enforcement paths can — and at runtime will —
disagree.

This doc rebases the mandate onto reviewer agents and a system principle.
Substrate code stops enforcing workflow shape; the reviewer (Gemini) does,
using its prompt context. The orchestrator composes a plan; the reviewer
approves the plan and each contract close; both loops are bounded.

## Overview

The change is conceptual, not architectural: the substrate keeps the
same primitives (documents, stories, tasks, ledger, repo) and the same
contract pipeline. What moves is *where the mandate lives*:

- **Before:** the mandate is a Go-coded slot list, parsed from
  `type=workflow` document frontmatter, merged across system / workspace
  / project / user tiers, and enforced by the `workflow_claim` handler.
- **After:** the mandate is a system-scope **principle** ("every story
  must flow `preplan → plan → orchestrator-composed → story_close`") and
  the **reviewer agents** (`story_reviewer` and `development_reviewer`)
  cite it when reviewing the plan and each contract close. No Go gate
  enforces shape.

Two new loops carry the enforcement: a **plan-approval loop** at the
front of the lifecycle (orchestrator submits plan → reviewer approves
or asks for revision), and the existing **per-contract close loop**
(`Verdict.Outcome == needs_more` → `kind:review-question` →
`contract_respond` → re-close), now backed by a real Gemini reviewer
rather than the silent `AcceptAll` default.

## Rejected: substrate-side workflow gates

Substrate-side enforcement of the contract sequence has six surfaces in
the codebase as of HEAD. All are removed by `story_af79cf95`:

| Surface | Path |
|---|---|
| 4-tier resolver | `internal/mcpserver/workflow_resolver.go` (`loadResolvedWorkflowSpec`) |
| Slot algebra | `contract.MergeSlots`, `contract.LayerSlots`, `contract.WorkflowSpec`, `contract.DefaultWorkflowSpec`, `contract.SlotsFromWorkflowDocStructured` |
| Workflow-claim gate | `mandatory_slot_missing` error returned by `handleWorkflowClaim` |
| Per-project KV fallback | `s.loadWorkflowSpec` reading `key:workflow_spec` |
| Per-project MCP verbs | `project_workflow_spec_get` / `_set` (`mcp.go:374-385`) |
| `/config` UI | "active mandate stack" panel + four vestigial `<th>phase</th>` columns in `pages/templates/configuration.html` |

The rejection rests on three observations:

1. **The orchestrator already composes the sequence.** After story_66d4249f
   (S6 of `epic:orchestrator-driven-configuration`), the orchestrator
   reads the contracts catalog, the agents catalog, the principles, and
   the workflow markdown's prose body, then writes a `kind:plan` ledger
   row + `ContractInstance` rows. A second enforcement layer is duplicate
   work.

2. **The two layers can disagree.** A project-tier `required_slots`
   override that the orchestrator did not anticipate causes
   `mandatory_slot_missing` after the plan is already written. There is
   no recovery path that does not bypass one of the two layers.

3. **"Configuration over code".** Adding a new mandated contract should
   be a configuration edit (the principle's text, the reviewer agent's
   rubric body), not a Go change to the slot algebra. The current shape
   forces both.

## Accepted: reviewer-driven mandate

The mandate moves into two configuration-only places:

- A new **system-scope principle**, seeded as
  `config/seed/principles/pr_mandate_reviewer_enforced.md` (story_e0833aea).
  Body states explicitly that every story must flow
  `preplan → plan → orchestrator-composed → story_close`; the reviewer
  enforces this; the substrate does not.

- Two new **system-scope reviewer agents** seeded under
  `config/seed/agents/` (story_6d259b99):
  - `story_reviewer` — reviews the plan, every non-develop contract
    close (preplan, plan, push, merge_to_main), and the final
    `story_close`. Body cites the mandate principle and the
    AC-coverage / evidence-completeness rubric.
  - `development_reviewer` — reviews `develop` CIs only. Body has the
    code-quality / tests-pass / commit-discipline rubric specific to
    code review.

Reviewer dispatch is per-contract: `runReviewer` (in
`internal/mcpserver/close_handlers.go`) selects the rubric body based
on the contract name — `develop` → `development_reviewer`; everything
else → `story_reviewer` (story_b4d1107c).

## Plan-approval loop

The new front-of-lifecycle loop catches the case the substrate gate
caught before (a missing `preplan` or `plan` slot) plus everything the
gate could not catch (semantic problems with the proposed sequence, AC
mismatches, missing optional contracts the story actually needs).

```
implement story_xxx
  ↓
orchestrator composes plan (kind:plan ledger row)
  ↓
orchestrator calls satellites_orchestrator_submit_plan
  ↓
  story_reviewer (Gemini) evaluates plan against principles + ACs
  ↓
  accepted? ── no ──→ orchestrator revises plan ──┐
                                                   │
  yes                                              │
  ↓ ←──────────────────────────────────────────────┘
workflow_claim (no slot enforcement — reviewer already approved)
  ↓
per contract:
  agent does work → contract_close
    ↓
    reviewer (story_reviewer or development_reviewer) evaluates evidence
    ↓
    needs_more? ── yes ──→ kind:review-question → contract_respond ──┐
                                                                      │
    accepted                                                          │
    ↓ ←───────────────────────────────────────────────────────────────┘
  next contract
  ↓
story_close → story_reviewer final sign-off
```

`workflow_claim` requires a `kind:plan-approved` ledger row to exist for
the story; without it, the claim rejects with `plan_not_approved`. The
old `mandatory_slot_missing` gate is removed in story_af79cf95.

## Iteration cap via KV

The plan-approval loop is bounded by a KV-configurable cap. The cap is
read via `kv_get_resolved` for the key `plan_review_max_iterations` —
the resolver walks the system → workspace → project → user tier chain
the existing `kv_*` verbs already implement. Default value is `5`; a
workspace or project may override.

When the orchestrator submits a plan with `iteration` exceeding the
resolved cap, `satellites_orchestrator_submit_plan` returns an error
tagged `plan_review_iteration_cap_exceeded`. The orchestrator surfaces
the failure to the user, who decides whether to raise the cap, narrow
the story, or cancel.

The per-contract close loop inherits the existing iteration semantics
(no hard cap; reviewer can keep returning `needs_more` indefinitely).
Raising or capping that loop is a follow-up if it surfaces as a
problem.

## Gemini wiring

`internal/reviewer/reviewer.go` already defines the `Reviewer` interface
and a default `AcceptAll` implementation. `cmd/satellites-server/main.go`
constructs `mcpserver.Deps` without a `Reviewer:` field today, so the
server runs with `AcceptAll` in production — every `validation_mode=llm`
contract close is silently auto-accepted.

Story_b4d1107c lands a Gemini-backed `reviewer.Reviewer` at
`internal/reviewer/gemini.go`. Wiring in `cmd/satellites-server/main.go`
constructs the Gemini reviewer when `GEMINI_API_KEY` is present and
falls back to `AcceptAll` with a warning log when not. Tests retain
`AcceptAll` via the existing test injection — no test breakage.

`runReviewer` selects the rubric body by contract name: `develop` →
`development_reviewer.Body`; everything else → `story_reviewer.Body`.
The selected body is passed in `reviewer.Request.ReviewerRubric` and
becomes part of the Gemini prompt.

## Reference: Archon's loop primitive

The plan-approval loop is the substrate equivalent of Archon's
[`loop.until` primitive](https://github.com/coleam00/archon). Archon
encodes development workflows as YAML DAGs with three node kinds (AI,
deterministic, loop). A canonical workflow uses `loop.until: APPROVED`
with `loop.fresh_context: true` to iterate until the reviewer accepts;
the same primitive carries `loop.until: ALL_TASKS_COMPLETE` for
implementation loops.

The decisive idea is **the loop is the configuration**: adding a new
review step is a YAML edit, not a substrate change. Satellites adopts
the same shape with two differences worth noting:

1. The loop primitive lives in **reviewer agent rubrics + the principle
   that names them**, not a YAML DAG. The loop's iteration cap lives in
   KV. The dispatch rule (which reviewer reviews which contract) lives
   in the orchestrator agent body and the Gemini reviewer dispatch in
   `runReviewer`.

2. Archon isolates parallel workflow runs via per-run **git worktrees**.
   Satellites runs sequentially inside one Claude session today; the
   worktree / branch / remote-agent execution mode is a separate
   configuration knob for a follow-up epic (see "Deferred" below).

## Deferred: execution mode

The execution mode — whether a story runs in the user's working
directory, in an isolated worktree, on a feature branch, or via a
remote agent process — is a configuration concern that does not block
this epic. Satellites v3 had worktree-based execution; v4 has not yet
ported it. The v4 design will support all three (worktree, branch,
remote agent) as configurable per-project or per-story options.

Out of scope here:

- The execution-mode configuration schema.
- The dispatcher changes to honour the chosen mode.
- The portal UI for selecting the mode.

These belong to a future `epic:execution-mode` (or similar). This epic
keeps the existing single-session execution model untouched.

## Supersession

This doc supersedes the following sections of
`docs/architecture-orchestrator-driven-configuration.md` (story_64e32e42):

- **§1 "Workflow as scope-level mandate"** — the additive override chain
  across system / workspace / project / user is removed in
  story_af79cf95. Workflow markdowns at scope=system remain as
  orchestrator/reviewer prose context (with `required_slots` frontmatter
  stripped); workspace / project / user `type=workflow` documents stop
  being enforced.
- **§3 "Constraint" (last paragraph)** — the orchestrator no longer needs
  to "include every contract in the resolved scope mandate stack" because
  the resolved stack is removed. The new constraint is "include `preplan`
  + `plan` at the front and `story_close` at the end; everything else is
  the orchestrator's choice; the reviewer enforces".
- **§5 dependency note "S5"** — the workflow-as-scope-mandate story is
  reverted by this epic's story_af79cf95.
- **§6 "active mandate stack" panel addition** — removed in
  story_af79cf95.

The orchestrator-composition thrust of the prior doc (§2 "Configuration
is emergent", §3 "Orchestrator-agent spec" except the constraint
paragraph, §4 "Agent = role, not contract shadow", §5 "Principles flow
into every agent context") is unchanged and remains canonical.

## Story sequence

The epic ships in seven peer stories (story is the unit of work, epics
are tags — pr_a9ccecfb). Each cites the artefact ledger row of this
design doc.

- **story_4362afb7** — design: configuration-over-code mandate
  architecture (this doc).
- **story_e0833aea** — principle: mandate enforced by reviewer, not
  substrate. Adds the system principle that codifies the mandate.
- **story_6d259b99** — seed system reviewer agents — `story_reviewer` +
  `development_reviewer`. Adds the rubric bodies the Gemini reviewer
  dispatches to.
- **story_b4d1107c** — wire Gemini reviewer + per-contract reviewer-agent
  dispatch. Implements `internal/reviewer/gemini.go` and rubric-body
  selection in `runReviewer`.
- **story_a5826137** — plan-approval loop — `orchestrator_submit_plan`
  with KV-capped iteration. Adds the front-of-lifecycle loop and the
  `plan_approved` precondition on `workflow_claim`.
- **story_af79cf95** — rip out substrate workflow enforcement. Deletes
  the resolver, slot algebra, claim gate, per-project KV verbs, and the
  `/config` panel + `phase` columns.
- **story_0932c700** — rewrite `claude_orchestrator` agent body — drop
  scope-mandate references; document the plan-submit step and the
  reviewer-mapping rule.

### Dependency order

```
story_4362afb7 ──▶ story_e0833aea ──▶ story_6d259b99 ──▶ story_b4d1107c ──▶ story_a5826137 ──▶ story_af79cf95 ──▶ story_0932c700
```

Stories 1–4 add the new model. Story 5 deletes the old once stories 1–4
cover the gap. Story 6 pins the orchestrator body into the new shape.

## Principle citations

- **pr_a9ccecfb — Story is the unit of work; epics are tags.** The
  epic ships as seven peer stories sharing the
  `epic:configuration-over-code-mandate` tag.
- **pr_no_unrequested_compat — No unrequested abstractions or
  backwards-compat layers.** story_af79cf95 deletes the workflow
  resolver, slot algebra, claim gate, and per-project KV verbs in one
  cut — no aliases, no flag-gated dual paths.
- **pr_evidence — Evidence must be verifiable.** The Gemini reviewer
  produces structured verdict rows with principle citations; the
  plan-approval loop writes a `kind:plan-approved` row that
  `workflow_claim` can verify.
- **pr_process_is_trust — Process is trust.** The substrate stops
  enforcing the contract sequence; the reviewer enforces it. Audit
  evidence comes from `kind:plan` + `kind:plan-approved` + per-CI
  verdict rows, not from a server-side reject path.
- **pr_10c48b6c — Documents drive feature stories.** Stories
  story_e0833aea through story_0932c700 each cite this design's
  artefact ledger row.

## Out of scope for this story

- Authoring the actual code changes — those are the six follow-up
  stories above.
- Migrating per-project `key:workflow_spec` KV rows in production —
  story_af79cf95 deletes the verb and the read path; existing rows
  become inert ledger entries (the audit chain is preserved).
- Worktree / branch / remote-agent execution mode — deferred to a
  future epic.
