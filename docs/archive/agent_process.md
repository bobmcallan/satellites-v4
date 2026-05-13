# Agent process: Claude session start → `implement sty_xxx` → story close

> **[ARCHIVED]** This document references the role-grant ceremony
> (`agent_role_claim`, `role_orchestrator`, `required_role`) that has
> been deleted from the substrate. For the current process see
> `docs/agent_process_v3.md`. This file is preserved for the contract
> definitions in §6 and the historical narrative around the
> "Today / Proposed" split. Do not follow the role-grant flow.

The end-to-end MCP call sequence an agent runs to take a story from
`backlog` to `done`. Each step lists the satellites MCP verbs in the
order they're invoked. Steps 1–3 happen once per session; steps
5–8 repeat per contract instance in the story's workflow.

---

## 1. Session bootstrap

### Today

1. **`session_register({})`** — under Streamable HTTP, the server mints a UUIDv4 and returns it via the `Mcp-Session-Id` response header on the `initialize` call (sty_31975268); spec-compliant clients echo it on every subsequent request automatically. The body argument `session_id` is still accepted as an override for stdio / test callers. Per sty_a4074d21 the session has no role at registration; roles are claimed explicitly later (§5).
2. **`session_register({ project_id })`** — same verb, resume semantics (sty_cef068fe). When `project_id` is supplied AND no explicit `session_id` was carried, the handler returns the caller's most recent non-stale session bound to that project (`resumed=true`). Stale rows (`SATELLITES_SESSION_STALENESS`) are skipped; a fresh id is minted instead. Allows a CLI restart to recover the prior session row + its orchestrator grant without orphaning in-flight CIs.
3. **`agent_role_claim({ workspace_id, role_id, agent_id, grantee_kind: "session", grantee_id: session_id })`** — mints the orchestrator grant and stamps it on the session row so the `contract_claim` `required_role` gate finds it. Required before any §6 work.
4. (Optional) **`session_whoami({})`** — smoke check; resolves session id from the Mcp-Session-Id header. Returns `effective_verbs` derived from the granted agent's `tool_ceiling` once a role is claimed.

### Q&A

- **How does the agent know to perform these actions?**
  When the satellites MCP server connects, the harness surfaces the server's `instructions` block (the `agent process` preamble) into the model's system context on every turn. That preamble carries the substrate fundamentals + the routing rules (project context first, story routing on `implement <story_id>`). This document expands those rules into the explicit per-call chain.
- **How is the `session_id` used through the rest of the process?**
  As the audit-trail join key. It is passed to `agent_role_claim` (as `grantee_id`), `contract_claim`, `contract_close`, `contract_respond`. Every claim/close re-verifies the session is registered, not stale, and carries a matching grant. The id appears in ledger tags on grant + claim rows (`session:<id>`, `grant_id:<id>`).


---

## 2. Project context

1. `git remote get-url origin` (shell).
2. **`project_set({ repo_url })`** — binds the session to the project
   that owns the canonicalised remote. Returns
   `{project_id, status: "resolved"}` on a hit, or
   `{status: "no_project_for_remote", repo_url_canonical}` on a miss.
3. **On miss**: ask the user before creating. If approved,
   **`project_create({ name, repo_url, … })`**.
4. (Optional) **`project_list()`** to find an existing project by
   name when `project_set` fails to canonical-match.

---

## 3. Inspect the story

1. **`story_get({ id })`** — full story body, ACs, status, template
   (improvement / bug / feature) including the `done`/`in_progress`
   field-present hooks.
2. (Optional) **`principle_list({ project_id, active_only: true })`**
   — read the active principles before composing a plan. Principles
   are constraints, not options.
3. (Optional) **`contract_list({ scope: "system" })`** /
   **`agent_list({ scope: "system" })`** — see what's available to
   the orchestrator.

---

## 4. Compose the workflow

The first call materialises a plan and creates one
`contract_instance` per slot. Idempotent — re-running returns the
existing CIs.

1. **`orchestrator_compose_plan({ story_id, agent_overrides? })`** —
   - writes a `kind:plan` ledger row,
   - writes a `kind:plan-approved` row (auto-approval shortcut for
     the legacy single-shot path),
   - calls `workflow_claim` internally to write the
     `kind:workflow-claim` row and create one CI per slot
     (default sequence: `plan → develop → push → merge_to_main → story_close`),
   - enqueues one task per slot (origin=`story_stage`).
   - Returns `{contract_instances, plan_ledger_id, workflow_claim_ledger_id, task_ids, agent_assignments}`.

   *Alternative reviewer-loop path* (preferred for new stories):
   **`orchestrator_submit_plan({ story_id, plan_markdown, proposed_contracts, iteration })`**
   — calls the `story_reviewer` agent for verdict; loop on
   `needs_more` until accepted, then `workflow_claim` accepts the
   story.

2. **`contract_next({ story_id })`** — returns the lowest-sequence
   CI with `status=ready` plus any skill docs whose
   `contract_binding` matches the contract id. Read-only.

---

## 5. Claim the role for the next CI

Per Option 1, the session holds **at most one role at a time** —
substrate-enforced (sty_3cc804cd). The session row carries the live
grant id (`orchestrator_grant_id`); `agent_role_claim` rejects with
`role_already_held` when the session already holds an active grant
for a different role, and `agent_role_release` clears the field
atomically with the grant status flip. A re-claim of the *same*
role is treated as a no-op and returns the existing grant
(`reused: true`).

Before claiming the contract instance (§6a) the agent must hold a
grant whose role matches the CI's `required_role`.

1. Look up the next CI's `required_role` (read it from the
   contract doc, or derive it from the `agent_assignments` map
   returned by `orchestrator_compose_plan`).
2. **If the session already holds the right role** (the previous
   CI used the same role) — skip to §6. (A `agent_role_claim` with
   the same role + agent returns `reused: true` and is harmless.)
3. **If the session holds a different role** — release it:
   **`agent_role_release({ grant_id })`** writes a
   `kind:role-grant, event:released` ledger row, flips the grant's
   status, and clears the session row's stamped grant id.
4. **Allocate the agent doc** (the permission-patterns / skill-refs
   bundle the role will execute under). Either:
   - Use the `agent_assignments` map returned by
     `orchestrator_compose_plan` (system-scope role agent), OR
   - **`agent_compose({ name, story_id, ephemeral: true, permission_patterns, skill_refs?, reason })`**
     — mints a story-scoped ephemeral agent doc with explicit
     permission patterns. Writes a `kind:agent-compose` ledger row.
     The response payload includes `principles_context` — load
     these into the working context.
5. **`agent_role_claim({ workspace_id, role_id, grantee_kind: "session", grantee_id: session_id, agent_id, project_id? })`** —
   binds the role to the session. The substrate validates
   `role.allowed_mcp_verbs ⊆ agent.tool_ceiling` before issuing
   the grant.

The `reviewer` role is **never** claimed by the orchestrator
session — reviewer work runs in the internal Gemini runtime that
claims `kind:review` tasks from the queue. The orchestrator only
files the close (§6c) and waits.

---

## 6. Claim → work → close (one CI at a time)

Repeat for each CI returned by `contract_next` until none remain.

### 6a. Claim
1. **`contract_claim({ contract_instance_id, session_id, agent_id, plan_markdown, skills_used? })`** —
   - runs the predecessor gate (predecessors must be `passed`/`skipped`),
   - runs the `required_role` gate (caller's grant role must match
     the contract's `required_role`),
   - writes a `kind:action-claim` row,
   - writes a `kind:plan` row (if `plan_markdown` non-empty),
   - flips the CI to `claimed`.
   - Same-session re-claim is treated as an amend: prior
     action-claim + plan rows are dereferenced and replaced.

### 6b. Do the work
File edits, shell commands, etc. — bounded by the agent's
`permission_patterns`. Write durable evidence as you go:

- **`ledger_append({ project_id, story_id?, contract_id?, type, content, structured?, tags? })`** —
  for `kind:evidence`, `kind:artifact`, `kind:decision` rows that
  carry verifiable proof (file:line refs, command output,
  test results, grep matches). Reviewers read these.

### 6c. Close
1. **`contract_close({ contract_instance_id, close_markdown, evidence_markdown?, evidence_ledger_ids? })`** —
   - writes a `kind:close-request` row,
   - writes an inline `kind:evidence` row when `evidence_markdown`
     is non-empty,
   - if the contract's `validation_mode = task` (the production
     default), enqueues a `kind:review` task (required_role=`reviewer`)
     and flips the CI to `pending_review`; otherwise the inline
     reviewer dispatches and the CI flips directly to `passed` /
     `failed`.
2. **The internal Gemini reviewer runtime** (sty_224621bd, run as an
   in-server goroutine when the system-tier KV row
   `reviewer.service.mode` resolves to `embedded` — the default)
   then claims the queued `kind:review` task on its own session,
   runs the rubric against the close evidence, and calls
   `contract_review_close` with `verdict ∈ {accepted, rejected}`. The
   CI flips through `pending_review → passed` (accepted) or
   `pending_review → failed` (rejected). `needs_more` reviewer
   outputs are coerced to `rejected`, and each review question is
   posted as its own `kind:review-question` ledger row so the
   orchestrator can address it via `contract_respond`.
3. **On `failed`**: the substrate appends a fresh CI of the same
   contract type with `PriorCIID` set (sty_bbe732af). Read the latest
   `kind:review-question` row, then:
   - **`contract_respond({ contract_instance_id, response_markdown })`**
     to write a `kind:review-response` row addressing the question,
     then close the new CI to re-trigger the reviewer, OR
   - re-claim the failed CI (amend) with revised plan and re-close.

### 6d. Move on
1. **`contract_next({ story_id })`** — returns the next ready CI;
   loop back to 6a until empty.

---

## 7. Story-level evidence (when needed)

Some contracts (`develop`, `push`, `merge_to_main`) require
artefacts beyond the contract close. File them as you produce
them so the closing reviewer can cite them:

- **`ledger_append`** with `type=artifact` and tags
  `kind:artifact, phase:<contract_name>` — for build outputs,
  diff stats, deploy logs.
- **`changelog_add({ project_id, … })`** — for the system-level
  changelog only; not a per-project surface.
- **`document_create` / `document_update`** — when the work
  produces new principle, contract, agent, or skill rows.

---

## 8. Story close

The last CI in the default sequence is `story_close`. Its
close-evidence rubric demands the story-template `done` hooks are
satisfied (e.g. `before_after`, `fix_commit`, `regression_test_path`
for the `improvement` template).

1. Set the template fields with
   **`story_field_set({ story_id, name, value })`** for each hook
   field the template lists.
2. **`contract_claim`** the `story_close` CI (per §6a).
3. Build the close evidence — link the merge commit, regression
   test path, and a brief before/after.
4. **`contract_close`** with the close evidence.
5. The reviewer flips the CI to `passed`; the substrate flips the
   story status to `done` (or to `partially_delivered` if the
   orchestrator amended the AC scope mid-flight via
   **`plan_amend`**).
6. (Optional) **`story_update_status({ id, status })`** to flip
   manually when the close path doesn't auto-advance the story.

---

## 9. Session housekeeping

- **`session_register`** is touched again whenever the session
  resumes — the staleness check rejects sessions older than
  `SATELLITES_SESSION_STALENESS` (default a few hours).
- **`agent_role_release`** is called between contracts whose
  `required_role` differs (per Option 1 — the session holds at
  most one role at a time). For a run of contracts that share a
  role (e.g. the lifecycle's plan + develop both want
  `developer`), the release/claim is skipped and the same grant
  is reused.
- Ephemeral agents composed in §5 are archived by the
  project-status sweeper after
  `SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS` once the story
  reaches a terminal state — no manual cleanup required.

---

## Quick reference: the verb chain for one story

```
session_register
  └─ project_set                  (or project_create on miss)
       └─ story_get
            └─ orchestrator_compose_plan
                 └─ for each CI in sequence:
                      ├─ agent_role_release           (only if current role ≠ required role)
                      ├─ agent_compose                (or use compose's assignment)
                      ├─ agent_role_claim             (only if role changed)
                      ├─ contract_claim
                      ├─ ledger_append (evidence)     (repeat as needed)
                      └─ contract_close
                           └─ contract_respond        (only on needs_more)
                 └─ story_field_set                   (template hooks)
                 └─ (story_close CI flips story to done)
```

---

---

## Proposed Solution

The chosen direction. The current process documented in §§1–9
reflects what the substrate does today; this section is the target
state we are working toward.

### Requirements

- **Session = status carrier, no role at start.** The session row
  holds `{session_id, user_id, current_role_grant?, current_ci?,
  last_seen_at}`. Roles are claimed explicitly later via
  `agent_role_claim`. The auto-grant in `handleSessionRegister`
  is removed.
- **One role at a time per session (Option 1, sequential).** The
  agent holds at most one active role grant at any moment. Switching
  roles is *release → claim*, never parallel. Enforces the
  *session = one role* rule from the MCP preamble at the substrate
  level rather than by convention.
- **Stable `session_id` across the conversation.** Same id reused
  for every call so the audit trail joins. Preferred source:
  server-minted UUID returned from `session_register`,
  round-tripped automatically via the MCP `Mcp-Session-Id` header
  (Streamable HTTP transport spec). Harness-supplied id (Claude
  CLI / Gemini) accepted when available as a convenience; never
  required.
- **Session resume after cancel/fail.** Delivered: `session_register({project_id})` — see §1 step 2 (sty_cef068fe). A new MCP connection from the same `(user, project)` resolves to the most recent non-stale session row, reusing the same `session_id` (and its stamped orchestrator grant). CIs left in `claimed` state by the prior connection are reclaimable via the same-grant amend path (`claim_handlers.go:132`).
- **Internal reviewer in place from day one.** Satellites publishes
  a task list; a satellites-internal Gemini runtime claims and
  processes `kind:review` tasks. The orchestrator session never
  reviews its own work.

### Limitations

- **Sequential agents are a mild pain for the agent.** Every
  contract whose agent differs from the prior CI's
  forces a release + claim round-trip. Accepted as the trade for
  substrate-enforced *one role per session*. Mitigation: a run of
  contracts that share a role (e.g. `plan` + `develop` both want
  `developer`) skips the release/claim and reuses the same grant.
- **Sub-agent sessions can't be cleanly separated without harness
  cooperation.** A subagent spawned by Claude's Task tool inherits
  the parent's MCP connection and therefore the parent's
  `Mcp-Session-Id`. Claude Code teams
  (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`) is the only clean
  path; without it, a child session id has to be passed in the
  subagent's prompt and used explicitly on every call.
- **`session_resume` requires a known scope key.** Recovery is by
  `(user, project)`. Two concurrent sessions in the same project
  collapse to the same row unless we add a third dimension (e.g.
  role) — at which point each `(user, project, role)` resumes
  independently. Cross-project sessions are naturally distinct.
- **Stdio MCP transport has no headers.** The `Mcp-Session-Id`
  round-trip works only over Streamable HTTP. Stdio clients would
  need to fall back to passing `session_id` as a verb parameter.
  Not a concern for satellites today (HTTP transport per
  `.mcp.json`), but a future limit if a stdio client is added.

### Pre-reqs

- **Drop the auto-grant** from `handleSessionRegister`
  (`internal/mcpserver/grant_handlers.go:387`).
- **Fix the `required_role` gate** at `grant_handlers.go:492`
  (resolve name → id before compare, or shift the resolution to
  seed-load time in `internal/configseed/parsers.go:65`).
- **Server-mint `session_id`** when `session_register` is called
  with no id; return the new id in the response. Round-trip via
  `Mcp-Session-Id` header on Streamable HTTP.
- **Add `session_resume`** (or extend `session_register` to act as
  resume when the caller's `(user, project)` has an active row).
- **Decide harness story.** SessionStart hook is optional under
  this model — drop it from the proposed config. The server is
  the source of truth for session id.
- **Internal Gemini reviewer runtime** stood up against the
  `reviewer` role and the task queue (`task_list({ required_role:
  "reviewer" })` → claim → run → close).
- **Substrate redeployed** to `satellites-pprod.fly.dev` after
  each of the above.

### Process (new)

Same shape as §§1–9 with the following deltas:

1. **§1 Session bootstrap** — `session_register({})` with no args;
   server mints + returns `session_id`; client stores it as
   `Mcp-Session-Id` header for the rest of the connection. No
   grant minted. No `session_id` parameter on subsequent verbs
   that the header covers.
2. **§4 Compose** — unchanged.
3. **§5 Role claim** — release the prior role (if any) with
   `agent_role_release`; claim the next role with `agent_role_claim`
   bound to the chosen agent doc. Skip when consecutive CIs share
   a role.
4. **§6a Contract claim** — `contract_claim` no longer takes
   `session_id` (header-derived); the gate runs identically.
5. **§6c Contract close** — close fires; the close-request becomes
   a `kind:review` task (when `validation_mode=task`); the
   internal Gemini reviewer claims the task on its own session,
   runs the rubric, calls `contract_review_close` to flip the CI
   to `passed`/`failed`. Orchestrator polls for the verdict (or
   subscribes to `admin_events_stream`) and resumes.
6. **§9 Housekeeping** — `agent_role_release` is the inter-CI
   ceremony when roles differ. `session_resume({})` recovers a
   prior session after a CLI restart; the substrate returns the
   same `Mcp-Session-Id`.

Refreshed quick-reference tree under the proposed model:

```
session_register                               (server mints id; header carries it)
  └─ project_set                               (or project_create on miss)
       └─ story_get
            └─ orchestrator_compose_plan
                 └─ for each CI in sequence:
                      ├─ agent_role_release    (only if current role ≠ required role)
                      ├─ agent_compose         (or use compose's assignment)
                      ├─ agent_role_claim      (only if role changed)
                      ├─ contract_claim        (no session_id param)
                      ├─ ledger_append         (evidence; repeat as needed)
                      └─ contract_close        (no session_id param)
                           ↳ kind:review task → internal Gemini reviewer →
                              contract_review_close → CI flips passed / failed
                 └─ story_field_set            (template hooks)
                 └─ (story_close CI flips story to done)
```
