# Spec — `task_context(task_id)`

Single-roundtrip orientation bundle for a dispatched agent that has
only a `task_id`. Mirrors the shape of `story_context(story_id)` but
scoped to one task.

**Status:** specification only — landed by `sty_31d51494` Layer 4 as
the contract for `sty_38bec58f`'s implementation. The verb does not
exist yet. Until it ships, callers stitch `task_get` + `story_context`
+ `agent_get` + `contract_get` + `principle_list` themselves.

## Why

A dispatched Claude subprocess invoked as `bash claude -p 'implement
task_xxx'` reads its task, agent doc, project intent, principles,
and contract via MCP using only the task id (per
`pr_substrate_provides_context`). Today that's five to seven
roundtrips. The bundle collapses them into one.

The orientation bundle's design constraint: every layer the
substrate composes for a dispatched agent (per
`pr_substrate_provides_context`) is reachable by `task_id` alone,
through this verb.

## Verb signature

```
task_context(task_id: string) → TaskContext
```

`task_id` is required. Cross-workspace access returns not-found.

## Return shape

```
type TaskContext = {
  task: Task                  // the task row itself
  story: Story | null         // owning story, when one exists
  project: Project            // owning project, with workspace_id
  intent_body: string         // project_intent artifact body
  principles: PrincipleEntry[] // active principles for the project
  agent: Agent | null         // dispatched agent doc (resolved by task.agent_id)
  contract: Contract | null   // contract for task.action, when one exists
  recent_evidence: LedgerEntry[] // recent ledger rows scoped to the task or its parent task
  prior_task: Task | null     // task.prior_task_id resolved, when set (retry chain)
  prior_verdict: LedgerEntry | null // verdict ledger row from prior_task's review, when set
  agent_process: string       // default_agent_process artifact body
}
```

### Field-by-field composition rules

- **`task`** — the row identified by `task_id`. Mandatory; if
  missing, the verb returns not-found.
- **`story`** — `story_get(task.story_id)` when `task.story_id` is
  set. Some maintenance tasks (e.g. retention sweeps) carry no
  story; the field is null in that case.
- **`project`** — resolved via `task.project_id` →
  `project_get(...)`. Mandatory; tasks always carry a project.
- **`intent_body`** — the `project_intent` artifact for
  `task.project_id`. The same body `project_set` /
  `project_context` return. Empty string when the project has no
  intent artifact.
- **`principles`** — every active type=principle document scoped to
  the task's project, plus active scope=system principles. Same
  composition rule as `orientation.buildOrientation`.
  Workspace-tier principles roll up if `sty_7f5585e9` ships first;
  otherwise system + project only.
- **`agent`** — `agent_get(name=task.agent_id)` when
  `task.agent_id` is set. Null for unassigned tasks.
- **`contract`** — `contract_get(name=task.action)` when the action
  resolves to a known contract. Null otherwise (orchestrator-only
  actions like `dispatch` are not contracts).
- **`recent_evidence`** — last N ledger rows where `task_id ==
  task.id` OR `task_id == task.prior_task_id`. Default N=20. Lets
  the dispatched agent see the verdict that triggered the retry +
  any partial evidence already in the chain.
- **`prior_task`** — `task_get(id=task.prior_task_id)` when set
  (retry chain). Null otherwise.
- **`prior_verdict`** — when `prior_task` is non-null, the most
  recent type=verdict ledger row scoped to `prior_task.id`. Null
  otherwise. Carries the rationale text the orchestrator must
  address.
- **`agent_process`** — body of the `default_agent_process`
  artifact. Same body Phase 3 of every Claude session sees as the
  MCP `instructions` field; included in this bundle so a dispatched
  subprocess that boots with a stripped MCP catalogue still has
  the substrate fundamentals.

## Auth

Workspace-scoped. Caller must be a member of the task's
workspace. Cross-workspace task_id returns the standard
`document: not found` shape — does NOT leak existence.

## Error shape

| Condition | Response |
|---|---|
| `task_id` empty | 400, `{"error": "task_id required"}` |
| Task not found / cross-workspace | 404, `{"error": "task: not found"}` |
| Project FK broken | 500, structured error referencing the broken FK |

Per-field resolution failures (missing principles, missing agent
doc) are NOT errors — the bundle returns the empty/null shape
documented above. Hard error only on structural problems.

## Compose-vs-fetch trade-offs

The bundle is "everything the dispatched agent needs to start
working." Anything reachable from those primitives by following
ids stays out of the bundle:

- **In:** task, story, project + intent + principles, agent doc,
  contract, prior_task + prior_verdict, recent evidence,
  agent_process.
- **Out:** the broader workflow document, the full agent catalogue
  (the agent fetches `agent_get` separately if it needs to look at
  a sibling), task siblings on the same story (use `task_walk` for
  that), the full ledger history (use `ledger_search`).

Rule of thumb: include something in the bundle when **every**
dispatched agent on **every** task needs it. Include it
on-demand when only some do.

## Idempotence

Read-only. Repeated calls return the same shape (modulo
recent_evidence, which advances as new rows append).

## Caller pattern

```
ctx = task_context(task_id)
# ctx.task, ctx.story, ctx.project, ctx.intent_body,
# ctx.principles, ctx.agent, ctx.contract, ctx.recent_evidence
# are all populated in one roundtrip.
# The agent claims, works, writes evidence, closes:
task_claim(ctx.task.id)
# ... role-specific work, fetching extras via agent_get, contract_get
# only when the bundle's contract/agent fields are insufficient ...
ledger_append(task_id=ctx.task.id, type=evidence, ...)
task_submit(kind=close, task_id=ctx.task.id, evidence_ledger_ids=[...])
```

## Implementation notes (for `sty_38bec58f`)

- Reuse `orientation.buildOrientation` for the
  intent_body + principles fields — same composition rule.
- Resolve `agent_get` / `contract_get` via the typed wrappers
  once `sty_7cfe5e29` lands; until then, fall back to the
  generic `document_get` and check the type field.
- Recent evidence list: query the ledger store for `task_id IN
  (task.id, task.prior_task_id)` ordered desc, limit 20.
- Prior verdict: same query scoped to `task.prior_task_id` AND
  `type=verdict`, limit 1.
- Workspace check: `task_get` already enforces this; reuse its
  membership filter.

## Open question for `sty_38bec58f`

Should `task_context` accept an optional `include` parameter to
opt into / out of expensive sub-queries (e.g.
`include=["agent_process"]` only)? Today's recommendation:
return the full bundle every time — the substrate's job is to
deliver enough context, not to negotiate it. Revisit if a verb
caller surfaces with a measured cost concern.
