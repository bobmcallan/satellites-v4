# Orchestrator-Driven Dynamic Configuration

Status: design note backing **story_64e32e42**, gating
`epic:orchestrator-driven-configuration`.

The current /config page reflects a v3 model where Configuration is a
stored, project-scope binding of contract refs, skill refs, and principle
refs (one row per project, picked from a static catalog; recently
reinforced by `c0d62fa` story_764726d3 which added the
`system_default` Configuration seed). With the orchestrator agent now in
place — `claude_orchestrator` (system-scope `type=agent`,
`config/seed/agents/claude_orchestrator.md`) — the orchestrator composes
a per-story plan dynamically from user intent, scope mandates, available
contracts/agents/skills, and active principles. The plan IS the
configuration. Persisting a separate Configuration row duplicates and
constrains it.

This note replaces the v3 stored-binding model with a dynamic-plan
model. It enumerates the implementation stories that deliver the
replacement (§7) so each downstream story files against a reviewable
artefact.

## 1. Workflow as scope-level mandate

A workflow is a markdown document, one per scope (system / workspace /
project / user), that declares the contracts mandatory at that scope.
The same source file is read by two consumers:

- **Orchestrator agent** reads the prose and any explanatory body to
  understand intent ("preplan exists to validate readiness before
  effort").
- **Reviewer** parses the structured mandate list out of the markdown
  frontmatter and gates `satellites_story_workflow_claim`. The current
  implementation is `internal/configseed/runner.go` (workflow phase) +
  `internal/mcpserver/contract_handlers.go` workflow-claim path; the
  enforcement signal already exists today as the
  `mandatory_slot_missing` error returned by the project workflow_spec
  gate (returned from `handleWorkflowClaim` when a proposed list omits
  a `required_slots` entry; see project workflow_spec retrieved via
  `satellites_project_workflow_spec_get`).

**Parser source:** the mandate list is parsed from frontmatter
`required_slots` — the same shape `config/seed/workflows/default.md`
uses today (`required_slots: [{ contract_name, required, min_count,
max_count }]`). Body text is for the orchestrator's prose reading; only
frontmatter is reviewer-enforced. One source file, two readers.

**Override resolution rule.** The override chain is **additive,
deduplicated by `contract_name`**:

```
effective_required_slots(scope) =
    system.required_slots
  ⊕ workspace.required_slots          // if a workspace workflow exists
  ⊕ project.required_slots            // if a project workflow exists
  ⊕ user.required_slots               // if a user workflow exists
where ⊕ unions by contract_name; min_count/max_count take the
maximum of min_count and the minimum of max_count across the chain
(stricter constraint wins); `required:false` at a parent is
upgradable to `required:true` at a child but never downgradable.
```

A project that wants to add a `compliance_review` slot does **not**
re-list system contracts — it ships a workflow markdown with one
`required_slots` entry (`compliance_review`) and the resolver folds it
into the system list.

**Worked example.** System ships `default.md` with the six current
slots (preplan, plan, develop, push, merge_to_main, story_close). A
project ships `compliance.md` with one entry: `{ contract_name:
compliance_review, required: true, min_count: 1, max_count: 1 }`. The
orchestrator's plan must include all seven slots; omitting any
triggers `mandatory_slot_missing` from the workflow-claim gate.

## 2. Configuration is emergent

There is no stored `type=configuration` document per story. The
orchestrator's per-story plan — which contracts run, which agents drive
each, which skills apply, which tasks land in the queue — is the
configuration. It lives in (a) the task queue, (b) one
`kind:plan` ledger row, (c) the `ContractInstance` rows materialised
from the workflow claim. The plan IS the configuration.

**Removal targets.** The implementation epic deletes the following:

| Surface | Path / symbol |
|---|---|
| Configuration struct + Marshal/Unmarshal | `internal/document/configuration.go:22-26` (Configuration), `:31-42` (Marshal), `:48-57` (Unmarshal); whole file removed |
| Document type constant | `internal/document/document.go:33` (`TypeConfiguration`) and the `validTypes` map entry at `:66`; Validate branch at `:156` |
| Story.ConfigurationID | `internal/story/story.go:34`; UpdateFields at `internal/story/store.go:39`; ListByConfigurationID at `internal/story/store.go:90`, `:278`; SurrealStore equivalents in `internal/story/surreal.go:176-196`; `ErrDanglingConfigurationID` at `internal/story/store.go:23-28` |
| Agent.DefaultConfigurationID | `internal/document/agent.go:13-34`; ListAgentsByDefaultConfigurationID at `internal/document/surreal.go:615`; `ErrDanglingAgentDefaultConfigurationID` at `internal/document/store.go:35-39`; validator at `internal/document/surreal.go:587-602` |
| configseed configuration phase | `internal/configseed/runner.go:91-100` (runConfigurationPhase + caller); `internal/configseed/parsers.go:140-185` (configurationToInput); `internal/configseed/loader.go:99` (the `"configurations"` subdir name) |
| Seed file | `config/seed/configurations/system_default.md` (the only file under that directory) |
| Portal /config "configuration" panel | `pages/templates/configuration.html:13-44` (selector dropdown + empty-state banner); `internal/portal/config_view.go` configComposite assembly (`:44-80` + `Configurations` slice / `SelectedID` / `Selected` / `:107` `docs.List(... TypeConfiguration ...)` call) |
| MCP CRUD surface | (no dedicated `configuration_*` MCP verbs exist today; Configuration documents are CRUD'd through the generic `document_*` verbs; nothing extra to remove) |
| Tests | `internal/document/configuration_test.go`; `internal/story/configuration_test.go`; `tests/integration/story_configuration_test.go`; `internal/portal/configuration_assignment_test.go`; relevant assertions in `internal/configseed/runner_test.go:270-470`; targeted assertions in `internal/portal/config_view_test.go`, `internal/portal/config_view_role_test.go`, `internal/portal/agents_view_test.go`, `internal/portal/document_detail_role_test.go`, `internal/portal/documents_view.go` references, `internal/portal/roles_view.go` references, `internal/document/skill_binding_migration*.go` |
| Document.Type validation | drop `TypeConfiguration` from `validTypes` (`internal/document/document.go:58-69`) and remove the type=configuration branch from `Document.Validate` (`document.go:152-167`) |

The orchestrator's emergent plan replaces every read path that today
asks "what's the Configuration for this story?". Reads that asked for
"the project's default Configuration" are deleted outright — there is
no project default; there is the resolved scope-mandate stack and the
orchestrator's plan.

## 3. Orchestrator-agent spec

The orchestrator agent is the entry point for story implementation. It
runs in the Claude Code session (per principle pr_f81f60ca,
satellites-agent is the worker; orchestration lives elsewhere).

### Inputs

| Input | Substrate origin |
|---|---|
| Story description + acceptance criteria | `satellites_story_get(id)` |
| User prompt comments / runtime intent | The current Claude session message stream (the `implement story_xxx` request and any clarifications) |
| Scope mandate stack | `type=workflow` documents at scope=system, scope=workspace (when bound), scope=project, scope=user; resolved per §1 override rule. The system-scope rows are seeded from `config/seed/workflows/*.md` by `internal/configseed/runner.go`. |
| Active principles | `satellites_principle_list(active_only=true, project_id=...)` returns the system+project active principle set; full body in `Principle.Description`. Principles are constraints, not options. |
| Contracts catalog | `type=contract` documents at scope=system + scope=project, listed via `satellites_contract_list` |
| Agents catalog | `type=agent` documents at scope=system + scope=project, listed via `satellites_internal_agent_list` |
| Skills catalog | `type=skill` documents at scope=system + scope=project (currently surfaced via the `skills` field on contract responses + portal `/config` skill panel) |

### Outputs

| Output | Substrate target |
|---|---|
| Per-story plan as ordered tasks | The `task` queue (per principle pr_75826278, tasks are the orchestration queue). Each task carries `{ contract_name, agent_ref, skill_refs?, sequence }`. |
| Plan ledger row | One `kind:plan` ledger row scoped to the story, structured payload mirrors the `proposed_contracts` list and the agent assignments. |
| Workflow claim | `satellites_story_workflow_claim(story_id, proposed_contracts=[...], claim_markdown=...)` — emits the `ContractInstance` rows and the `kind:workflow-claim` ledger row. |

### Constraint

The orchestrator MUST include every contract in the resolved scope
mandate stack from §1. The reviewer enforces the floor at workflow
claim time via `mandatory_slot_missing`. The orchestrator MAY add
optional middle slots (e.g. an extra `develop` for a multi-stage
implementation) and justify them via the `optional_contracts_requested`
field on preplan close.

### Trigger

When a user says "implement story_xxx" (or invokes a story-implement
verb), the Claude session boots the orchestrator role; the orchestrator
reads the inputs, composes the plan, writes the plan ledger row, and
invokes `workflow_claim`. The remainder of the run drives one contract
at a time per the materialised `ContractInstance` chain.

## 4. Agent = role, not contract shadow

An agent document defines a **role** — a bundle of permission patterns
+ skill refs + strategy text describing how an executor approaches a
class of work. A single agent can satisfy multiple contracts as long as
its execution shape (`permission_patterns`, `skill_refs`,
`agent_instruction`) covers what each contract needs.

**Current state — every contract has its own shadow agent.** Files
under `config/seed/agents/`:

| File | Role-shaped or shadow? |
|---|---|
| `claude_orchestrator.md` | Role-shaped (orchestration; not a contract shadow). |
| `preplan_agent.md` | 1-1 contract shadow (preplan only). |
| `plan_agent.md` | 1-1 contract shadow (plan only). |
| `develop_agent.md` | 1-1 contract shadow (develop only). |
| `push_agent.md` | 1-1 contract shadow (push only). |
| `merge_agent.md` | 1-1 contract shadow (merge_to_main only). |
| `story_close_agent.md` | 1-1 contract shadow (story_close only). |

Six of seven agent seeds are contract shadows.

**Role-vs-contract test (numeric criterion).** An agent is role-shaped
if it can drive **≥2 contracts cleanly** without the agent_instruction
text being substantially copy-paste of any single contract's
`agent_instruction` field. An agent that drives exactly one contract,
with text that repeats the contract's instruction, is a shadow.

**Proposed collapse target (the audit story will refine).**

| New agent | Contracts driven |
|---|---|
| `claude_orchestrator` | (orchestration; unchanged) |
| `developer_agent` | preplan + plan + develop |
| `releaser_agent` | push + merge_to_main |
| `story_close_agent` | story_close (kept separate **iff** the audit finds the close-evidence shape distinct enough to warrant role separation; otherwise folded into orchestrator) |

The audit story (S8) does the final classification and produces the
new agent seed files; the implementation epic ships the consolidation
in one cut.

## 5. Principles flow into every agent context

Active principles at the resolved scope are absolute constraints on
every agent's behaviour. They must be loaded into every agent's system
context, not only the orchestrator's.

**Composer.** The agent-context composer is
`internal/mcpserver/agent_compose.go` (whole file; package doc at
`agent_compose.go:1-5`). Today it composes the agent document body but
does **not** auto-inject principles — principles are merely *queryable*
via `satellites_principle_list`, which means the orchestrator might
pull them but downstream agents (developer, releaser, story_close) do
not see them in their system message unless an upstream caller pasted
them in.

**Injection point.** When an agent is composed for a story (the
`agent_compose` MCP verb path, registered in
`internal/mcpserver/wrappers.go`), the composer must:

1. Resolve the project + user scope of the story.
2. Call the equivalent of `satellites_principle_list(active_only=true,
   project_id=...)` to get the active principle set.
3. Render each principle's `Name` + `Description` as a system-message
   prelude on the composed agent document (or on the agent's runtime
   context payload — exact mechanism left to the implementation
   story to choose between *static doc-rewrite* and *runtime
   context-injection*).

The implementation story (S7) picks one of static or runtime injection;
the design doc treats both as acceptable provided every agent invocation
ends up with the principles in its context.

## 6. /config page revamp

| Change | Today | After |
|---|---|---|
| "configuration" panel (selector + bundle view) | Lines 13-44 of `pages/templates/configuration.html` (selector + the empty-state banner directing users to author Configurations) | **Removed.** No Configuration to select. |
| "system contracts" panel | `pages/templates/configuration.html:161` | **"contracts"** (drop `system` prefix; scope conveyed by table column). |
| "system workflows" panel | `pages/templates/configuration.html:233` | **"workflows"**, **above contracts** in the system section (currently below). |
| "system agents" panel | `pages/templates/configuration.html:293` | **"agents"** (drop `system` prefix). |
| **system principles panel** | **Missing — confirmed gap** (no panel today; `satellites_principle_list scope=system` returns 9 active rows but they are not listed on /config) | **Added.** Sibling table to system contracts/workflows/agents. |
| Read-only banner ("edit the seed file and reseed") | All system panels (`pages/templates/configuration.html:167, 239, 299`) | **Replaced** with create/update at the appropriate override scope (workspace / project / user). System rows stay read-only because system docs are seed-owned; lower-scope rows are user-writable. |
| `config/seed/principles/` directory + seed loader | **Missing** — system principles exist as `created_by:system` DB rows but have no source-of-truth file mirror | **Added.** Mirrors the existing `config/seed/{contracts,workflows,agents}/` pattern. The 9 current system principles get a markdown source file each. |

The /config page becomes a catalog view (contracts, workflows, agents,
principles, skills) plus an "active scope mandate" view that resolves
the workflow markdown stack per §1 and shows which contracts are
mandatory in this project's effective scope.

## 7. Implementation stories

These file under `epic:orchestrator-driven-configuration` after this
design closes; each cites the artefact ledger row of this design doc.

### S2. Drop `Phase` from contract instances

The `Phase` field on `internal/contract/contract.go:34` is set to the
slot name in two places (`internal/mcpserver/contract_handlers.go:221,
:476`) and read in **zero** non-test sites. It is a denormalised
duplicate of `contract_name`. Story removes the field, the two setters,
and any test references; updates JSON serialisation goldens. No
runtime behaviour change. **No dependencies.** Smallest cut; can land
first to flush the legacy field.

### S3. `config/seed/principles/` directory + seed loader

Add the missing principle seed source. Mirror
`config/seed/{contracts,workflows,agents}/` pattern: one markdown file
per principle with frontmatter (id, name, scope, tags) + body
(`description`). Wire `internal/configseed/runner.go` to load
principles before configurations (and after agents, so refs resolve).
Seed the 9 existing system principles as files. Closes the
`satellites_principle_list scope=system` source-of-truth gap. **No
dependencies; lands before /config changes that surface principles.**

### S4. Delete `type=configuration` end-to-end

Remove every Configuration surface enumerated in §2's removal table:
struct, type constant, story/agent FKs, configseed phase + parsers +
loader subdir, seed file, /config "configuration" panel, dependent
tests. Per pr_no_unrequested_compat, no aliases, no migration shims,
no `type=configuration_v2`. **Depends on:** S3 (principles seed exists
so /config doesn't need a "configuration" panel as the only place to
edit them).

### S5. Workflow-as-scope-mandate

Implement the additive override chain from §1 across system →
workspace → project → user workflow markdowns. Add a workspace-scope
and user-scope path for `type=workflow` documents (currently system
only). Implement the resolver + the `mandatory_slot_missing` extension
that walks the chain. Add a portal "active mandate stack" view.
**Depends on:** S4 (so configuration's now-removed selector doesn't
shadow the new mandate-stack view).

### S6. Orchestrator-agent dynamic plan composition

Wire the orchestrator role at the story-implement entry path. On
"implement story_xxx", the orchestrator: reads inputs per §3, composes
the per-story plan as ordered tasks + a `kind:plan` ledger row, and
calls `workflow_claim`. **Depends on:** S5 (mandate-stack resolver
exists), S7 (principles in agent context).

### S7. Principles → all-agent context

Wire principle injection per §5 into `internal/mcpserver/agent_compose.go`.
Every agent invocation receives the resolved active principle set in
its context. Pick static-rewrite or runtime-injection per
implementation judgement. **Depends on:** S3 (principles have a
source-of-truth file mirror).

### S8. Agent-role audit + collapse of contract-shadow agents

Per §4: audit `config/seed/agents/*`, classify each as role-shaped or
shadow, collapse shadows into role agents (`developer_agent`,
`releaser_agent`, possibly fold `story_close_agent` into orchestrator).
Update agent assignments in seeded contracts; update any tests pinning
agent file names. **Depends on:** S6 (orchestrator picks agents per
contract dynamically; the audit aligns the agent catalog with that
selection).

### S9. /config page revamp

Per §6: drop "configuration" panel; drop "system" prefix; add
**system principles** panel; reorder so workflows sit above contracts;
add create/update at the appropriate override scope for each catalog
table. **Depends on:** S3 (principles source mirror), S4 (Configuration
panel removed), S5 (mandate-stack view).

### Dependency order

```
S2 ──▶ (independent; smallest cut)

S3 ──▶ S4 ──▶ S5 ──▶ S6
       │              │
       └─▶ S7 ────────┘
                     │
        S6 ──▶ S8 ───┘
        S5, S4, S3 ──▶ S9
```

S2 lands first or in parallel. S3 unblocks the rest; S4 follows; S5
+ S7 in parallel; S6 + S8 + S9 last.

## Principle citations

This design rests on the following principles:

- **pr_a9ccecfb — Story is the unit of work; epics are tags, not
  primitives.** The implementation does not file parent/child stories
  for S2-S9; they are peer stories sharing the
  `epic:orchestrator-driven-configuration` tag.

- **pr_f81f60ca — Satellites-agent is the worker; orchestration lives
  elsewhere.** The orchestrator runs in the Claude session, not on the
  satellites-agent worker; tasks emit *to* the worker for execution.

- **pr_93835b29 — Documents share one schema, discriminated by type.**
  Removing `type=configuration` and consolidating agents around roles
  reduces the type discriminator surface — no new tables, no new
  primitive, just a smaller `validTypes` set.

- **pr_c25cc661 — Five primitives per project: documents, stories,
  tasks, ledger, repo.** The orchestrator's plan lives on tasks +
  ledger; no sixth primitive (no Configuration table) is introduced.

- **pr_10c48b6c — Documents drive feature stories.** Stories S2-S9 each
  cite this design's artefact ledger row when they file, satisfying
  the "feature stories trace to a document" rule.

- **pr_no_unrequested_compat — No unrequested abstractions or
  backwards-compat layers.** S4 deletes Configuration outright; no
  `type=configuration_v2`, no alias from `Configuration` to a
  hypothetical `Plan` struct, no flag-gated dual-read path. One commit
  per surface, fully migrated.

## Out of scope for this story

- Authoring the actual code changes — those are S2-S9.
- Migration of existing Configuration rows in production — addressed
  by S4's removal commit (one-pass migration: drop the type, drop the
  data; no live Configurations in pprod that would lose work).
- Workspace-tier UI for workflows beyond what S5 introduces.

## Cross-references

- Sibling design notes: `docs/architecture-contract-agent-separation.md`
  (story_b7bf3a5f), `docs/architecture.md` §2 Documents.
- Principle list as queried 2026-04-29:
  `satellites_principle_list scope=system` returned 9 active rows;
  none of those rows had a corresponding file under
  `config/seed/principles/` (directory does not exist) — confirms the
  S3 gap.
