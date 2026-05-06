> **[ARCHIVED]** This document describes a substrate state where
> the role tier was a first-class primitive. The role tier has
> been deleted (in principle by sty_c67ad430 + sty_0f7cea24, with
> code-residue cleanup tracked by sty_92218a87). For the current
> process, see `agent_process_v3.md`. Do not follow the role-grant
> ceremony described below — it no longer exists.

## Agent process v2 — the actual process today

This is the post-implementation snapshot of the verb chain a Claude
session runs to take a story from `backlog` to `done`. It supersedes
the "Today" / "Proposed Solution" split in `agent_process.md`: the
proposals listed there have all landed (sty_31975268, sty_a4074d21,
sty_cef068fe, sty_3cc804cd, sty_224621bd, sty_e1ab884d), and the
configseed work (epic:setup-as-data-v1) closed the duplicate-write
gap in the boot path.

If the older doc says "Today X / Proposed Y", **Y is current**.

---

## What changed vs `agent_process.md`

| Area | Before | Now |
|---|---|---|
| `session_id` source | harness/CLI passed the id | server mints UUIDv4 and returns it via the `Mcp-Session-Id` response header on `initialize`; spec-compliant clients echo it on every subsequent request (sty_31975268). Body-arg `session_id` accepted as override for stdio / test only. |
| Session role at start | `session_register` auto-granted orchestrator | no auto-grant; session has no role until `agent_role_claim` (sty_a4074d21). |
| Session resume | none — a CLI restart orphaned the prior session | `session_register({project_id})` resumes the most recent non-stale session for `(user, project)` and reuses its stamped grant (sty_cef068fe). Stale rows (`SATELLITES_SESSION_STALENESS`) skipped. |
| Roles per session | parallel possible | substrate-enforced sequential — at most one active grant per session (sty_3cc804cd). Same-role re-claim returns existing grant (`reused: true`). |
| Reviewer | run inside the orchestrator session | runs in an internal Gemini runtime (`SATELLITES_REVIEWER_SERVICE=embedded`) that holds its own `role_reviewer` grant and claims `kind:review` tasks from the queue (sty_224621bd). The orchestrator never claims `reviewer`. |
| `needs_more` from reviewer | undefined | coerced to `rejected`; each review question becomes a `kind:review-question` ledger row the orchestrator addresses via `contract_respond` (sty_d8d6928 follow-on). |
| `required_role` gate | name vs id mismatch failed claims | gate resolves contract `required_role` name → role doc id before compare (sty_a4074d21). configseed writes name-form, gate accepts either. |
| Handshake instructions | Go string constant `agentprocess.SystemDefaultBody` + `SeedSystemDefault()` | seeded by configseed from `config/seed/artifacts/default_agent_process.md` via `KindArtifact` (sty_6c3f8091). Operator edits = file edit + reseed, no rebuild. |
| Roles + agents at boot | written twice — once by in-Go `seedOrchestratorDocs` / `seedReviewerDocs` / `seedLifecycleAgents`, once by configseed | configseed is the single writer (sty_db196ff4 + sty_a1a77518). `config/seed/{roles,agents,artifacts}/*.md` is the source of truth. `system_seed_run` after boot is a no-op. |
| Skills + reviewers baseline | undocumented | ad-hoc-only by principle (`pr_skills_reviewers_ad_hoc`, sty_051222a5). The substrate seeds none; operators mint them when a binding emerges. |

---

## 1. Session bootstrap

1. **`session_register({})`** — under Streamable HTTP, the server
   mints a UUIDv4 and returns it via the `Mcp-Session-Id` response
   header on the `initialize` call. Spec-compliant clients echo it
   on every subsequent request automatically. Body-arg `session_id`
   accepted as an override for stdio / test callers only. Session
   row carries `{session_id, user_id, last_seen_at}` — **no role,
   no grant**.
2. **`session_register({project_id})`** — resume semantics. When
   `project_id` is supplied AND no explicit `session_id` was
   carried, the handler returns the caller's most recent
   non-stale session bound to that project (`resumed=true`). Stale
   rows (`SATELLITES_SESSION_STALENESS`) are skipped; a fresh id is
   minted instead. Restored session inherits its previously
   stamped orchestrator grant if any.
3. (Optional) **`session_whoami({})`** — smoke check. Returns
   `effective_verbs` derived from the granted agent's
   `tool_ceiling` once a role is claimed.

The MCP server's `instructions` block (the agent-process preamble)
is sourced live from the seeded `default_agent_process` artifact —
the harness surfaces it into the model's system context on every
turn. Operators tightening principles or routing rules edit
`config/seed/artifacts/default_agent_process.md` and reseed.

---

## 2. Project context

1. `git remote get-url origin` (shell).
2. **`project_set({repo_url})`** — binds the session to the project
   that owns the canonicalised remote. Returns
   `{project_id, status: "resolved"}` on a hit, or
   `{status: "no_project_for_remote", repo_url_canonical}` on a
   miss.
3. **On miss** ask the user before creating. If approved,
   **`project_create({name})`** then **`repo_add({git_remote})`** to
   register the remote on the project. The project↔remote binding
   lives on the repo row; `project_create` no longer takes a
   `git_remote` arg (sty_14dfd05b).
4. (Optional) **`project_list()`** to find an existing project by
   name when `project_set` fails to canonical-match.

Subsequent project-scoped verbs default to the bound project when
`project_id` is omitted.

---

## 3. Inspect the story

1. **`story_get({id})`** — full story body, ACs, status, template
   (`improvement` / `bug` / `feature` / `infrastructure` /
   `documentation`), including the `done` and `in_progress`
   field-present hooks.
2. (Optional) **`principle_list({project_id, active_only: true})`**
   — read the active principles before composing a plan.
   Principles are constraints, not options.
3. (Optional) **`contract_list({scope: "system"})`** /
   **`agent_list({scope: "system"})`** — see what's available to
   the orchestrator. Both surfaces are configseed-seeded.

---

## 4. Compose the workflow

The first call materialises a plan and creates one
`contract_instance` per slot. Idempotent — re-running returns the
existing CIs.

1. **`orchestrator_compose_plan({story_id, agent_overrides?})`** —
   - writes a `kind:plan` ledger row,
   - writes a `kind:plan-approved` row (auto-approval shortcut for
     the legacy single-shot path),
   - calls `workflow_claim` internally to write the
     `kind:workflow-claim` row and create one CI per slot
     (default sequence: `plan → develop → push → merge_to_main →
     story_close`),
   - enqueues one task per slot (origin=`story_stage`),
   - returns `{contract_instances, plan_ledger_id,
     workflow_claim_ledger_id, task_ids, agent_assignments}`.

   *Reviewer-loop alternative (preferred for new stories):*
   **`orchestrator_submit_plan({story_id, plan_markdown,
   proposed_contracts, iteration})`** — calls the `story_reviewer`
   agent for verdict; loop on `needs_more` until accepted, then
   `workflow_claim` accepts the story.

2. **`contract_next({story_id})`** — returns the lowest-sequence
   CI with `status=ready` plus any skill docs whose
   `contract_binding` matches the contract id. Read-only.

---

## 5. Role-per-CI ceremony

The session holds **at most one active role grant** at any moment
— substrate-enforced. Switching roles is *release → claim*, never
parallel. A re-claim of the same role with the same agent returns
the existing grant (`reused: true`).

Before claiming the contract instance (§6a) the agent must hold a
grant whose role matches the CI's `required_role`.

1. Look up the next CI's `required_role` (read it from the
   contract doc, or derive it from the `agent_assignments` map
   returned by `orchestrator_compose_plan`).
2. **If the session already holds the right role** (the previous
   CI used the same role) — skip to §6.
3. **If the session holds a different role** — release it:
   **`agent_role_release({grant_id})`** writes a
   `kind:role-grant, event:released` ledger row, flips the grant
   status, and clears the session row's stamped grant id.
4. **Allocate the agent doc** — the permission-patterns /
   skill-refs bundle the role will execute under. Either:
   - Use the `agent_assignments` map returned by
     `orchestrator_compose_plan` (system-scope role agent loaded
     from `config/seed/agents/`), OR
   - **`agent_compose({name, story_id, ephemeral: true,
     permission_patterns, skill_refs?, reason})`** — mints a
     story-scoped ephemeral agent doc with explicit permission
     patterns. Writes a `kind:agent-compose` ledger row. The
     response payload includes `principles_context` — load these
     into the working context.
5. **`agent_role_claim({workspace_id, role_id, grantee_kind:
   "session", grantee_id: session_id, agent_id, project_id?})`** —
   binds the role to the session. The substrate validates
   `role.allowed_mcp_verbs ⊆ agent.tool_ceiling` before issuing
   the grant.

The `reviewer` role is **never claimed by the orchestrator
session**. Reviewer work runs in the internal Gemini runtime
(`SATELLITES_REVIEWER_SERVICE=embedded`) on its own session +
`role_reviewer` grant. The orchestrator only files the close
(§6c) and waits.

---

## 6. Claim → work → close (one CI at a time)

Repeat for each CI returned by `contract_next` until none remain.

### 6a. Claim
**`contract_claim({contract_instance_id, agent_id, plan_markdown,
skills_used?})`** —
- runs the predecessor gate (predecessors must be
  `passed`/`skipped`),
- runs the `required_role` gate (caller's grant role must match
  the contract's `required_role`; gate resolves names to ids
  before comparing),
- writes a `kind:action-claim` row,
- writes a `kind:plan` row (if `plan_markdown` non-empty),
- flips the CI to `claimed`.

`session_id` is header-derived; not a body arg under Streamable
HTTP. Same-session re-claim is treated as an amend: prior
action-claim + plan rows are dereferenced and replaced.

### 6b. Do the work

File edits, shell commands, etc. — bounded by the agent's
`permission_patterns`. Write durable evidence as you go:

- **`ledger_append({project_id, story_id?, contract_id?, type,
  content, structured?, tags?})`** — for `kind:evidence`,
  `kind:artifact`, `kind:decision` rows that carry verifiable
  proof (file:line refs, command output, test results, grep
  matches). Reviewers read these.

### 6c. Close

1. **`contract_close({contract_instance_id, close_markdown,
   evidence_markdown?, evidence_ledger_ids?})`** —
   - writes a `kind:close-request` row,
   - writes an inline `kind:evidence` row when
     `evidence_markdown` is non-empty,
   - if `validation_mode=task` (production default), enqueues a
     `kind:review` task (`required_role=reviewer`) and flips the
     CI to `pending_review`; otherwise the inline reviewer
     dispatches and the CI flips directly to `passed` / `failed`.
2. **The internal Gemini reviewer runtime** claims the queued
   `kind:review` task on its own session +
   `role_reviewer` grant, runs the rubric against the close
   evidence, and calls `contract_review_close` with
   `verdict ∈ {accepted, rejected}`. CI flips:
   - `pending_review → passed` on `accepted`
   - `pending_review → failed` on `rejected`
   - `needs_more` is coerced to `rejected`; each review question
     is posted as its own `kind:review-question` ledger row.
3. **On `failed`** the substrate appends a fresh CI of the same
   contract type with `PriorCIID` set. Read the latest
   `kind:review-question` row, then either:
   - **`contract_respond({contract_instance_id,
     response_markdown})`** to write a `kind:review-response` row
     addressing the question, then close the new CI to re-trigger
     the reviewer, OR
   - re-claim the failed CI (amend) with revised plan and re-close.

### 6d. Move on

**`contract_next({story_id})`** — returns the next ready CI; loop
back to 6a until empty.

---

## 7. Story-level evidence

Some contracts (`develop`, `push`, `merge_to_main`) require
artefacts beyond the contract close. File them as you produce
them so the closing reviewer can cite them:

- **`ledger_append`** with `type=artifact` and tags
  `kind:artifact, phase:<contract_name>` — for build outputs,
  diff stats, deploy logs.
- **`changelog_add({project_id, …})`** — system-level changelog
  only.
- **`document_create` / `document_update`** — when the work
  produces new principle, contract, agent, or skill rows.

---

## 8. Story close

The last CI in the default sequence is `story_close`. Its
close-evidence rubric demands the story-template `done` hooks are
satisfied (e.g. `before_after`, `fix_commit`,
`regression_test_path` for the `improvement` template).

1. Set the template fields with **`story_field_set({id, field,
   value})`** for each hook field the template lists.
2. **`contract_claim`** the `story_close` CI (per §6a).
3. Build the close evidence — link the merge commit, regression
   test path, and a brief before/after.
4. **`contract_close`** with the close evidence.
5. Reviewer flips the CI to `passed`; the substrate flips the
   story status to `done` (or `partially_delivered` if the
   orchestrator amended the AC scope mid-flight via
   **`plan_amend`**).
6. (Optional) **`story_update_status({id, status})`** to flip
   manually when the close path doesn't auto-advance.

Note: `story_update_status` itself enforces the category
template's `in_progress` and `done` field-present hooks, so
**`story_field_set`** must be called *before* a status transition
that the template gates.

---

## 9. Session housekeeping

- **`session_register`** is touched again whenever the session
  resumes — the staleness check rejects sessions older than
  `SATELLITES_SESSION_STALENESS` (default a few hours).
- **`agent_role_release`** is called between contracts whose
  `required_role` differs. For a run of contracts that share a
  role (e.g. `plan` + `develop` both want `developer`), the
  release/claim is skipped and the same grant is reused.
- Ephemeral agents composed in §5 are archived by the
  project-status sweeper after
  `SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS` once the story
  reaches a terminal state — no manual cleanup required.

---

## 10. Where the substrate's setup data lives

After epic:setup-as-data-v1 (sty_a86cde6c) configseed is the
single writer for every system-tier doc the substrate boots with.
The on-disk source of truth is `config/seed/`:

| Subdir | Document type | What lives here |
|---|---|---|
| `roles/` | `role` | `role_orchestrator`, `role_reviewer` — `allowed_mcp_verbs`, `required_hooks`, `default_context_policy` |
| `agents/` | `agent` | `agent_claude_orchestrator`, `agent_gemini_reviewer`, `developer_agent`, `releaser_agent`, `story_close_agent`, `story_reviewer`, `development_reviewer` — `permitted_roles`, `tool_ceiling`, `permission_patterns` |
| `contracts/` | `contract` | per-slot contract definitions — `category`, `required_role`, `evidence_required`, `validation_mode` |
| `workflows/` | `workflow` | prose workflow descriptions (slot algebra retired in story_af79cf95) |
| `artifacts/` | `artifact` | `default_agent_process` — the handshake markdown the MCP server returns as its `instructions` block |
| `principles/` | `principle` | the substrate's enforced rules (`pr_evidence`, `pr_root_cause`, `pr_no_unrequested_compat`, `pr_skills_reviewers_ad_hoc`, …) |
| `story_templates/` | `story_template` | per-category required + done-hook fields |
| `replicate_vocabulary/` | `replicate_vocabulary` | natural-language → `portal_replicate` action mapping |

Skills + reviewers (`type=skill`, `type=reviewer`) are **not**
seeded — they are ad-hoc by principle (`pr_skills_reviewers_ad_hoc`).

A boot of a fresh DB creates each row exactly once. A re-boot
against an existing DB converges to the seed-file shape via
`document.Upsert` (keyed by `(workspace_id, project_id, name)`).
`system_seed_run` is idempotent.

To change any of the above on a deployed substrate: edit the
markdown, redeploy, optionally call `system_seed_run` to apply
without a restart.

---

## Quick reference: the verb chain for one story

```
session_register                              (server mints id; header carries it)
  └─ project_set                              (or project_create on miss)
       └─ story_get
            └─ orchestrator_compose_plan
                 └─ for each CI in sequence:
                      ├─ agent_role_release   (only if current role ≠ required role)
                      ├─ agent_compose        (or use compose's assignment)
                      ├─ agent_role_claim     (only if role changed)
                      ├─ contract_claim       (no session_id param)
                      ├─ ledger_append        (evidence; repeat as needed)
                      └─ contract_close       (no session_id param)
                           ↳ kind:review task → internal Gemini reviewer →
                              contract_review_close → CI flips passed / failed
                           ↳ on rejected/needs_more: contract_respond,
                              then close the appended fresh CI
                 └─ story_field_set           (template hooks)
                 └─ (story_close CI flips story to done)
```
