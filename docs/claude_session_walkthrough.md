# Claude session walkthrough — startup to plan-contract close

A concrete, annotated trace of a typical Claude Code session against
the satellites MCP server: session start, `implement sty_xxx`, and
the calls + documents involved up through the close of the **planning
contract**. develop / story_close / push / merge_to_main are out of
scope for this walkthrough.

Every response is labelled **seed** (the body comes from a markdown
file under `config/seed/`) or **runtime** (the body is a database
row written at request time).

The trace is illustrative — payloads are abbreviated and ids are
synthesised. The verb chain is real.

---

## Step 0 — what's loaded before the operator types anything

When the Claude Code harness opens its MCP connection to the
satellites server, the server returns an `instructions` block in
its `initialize` response. The harness surfaces that block into
the model's system context on every turn.

**Source of the block:** `config/seed/system/artifacts/default_agent_process.md`
(the seeded `default_agent_process` artifact). **seed.**

Content carried into the session: substrate identity
(configuration-over-code, multi-agent orchestration), the five
primitives (projects, stories, tasks, documents, ledger), the
bootstrap directive (`project_set` is the first call when the
prompt references substrate primitives), a fetch map naming the
verbs callers use to get more context (`story_get`,
`task_get`, `agent_get`, `contract_get`), and the operating
principle (act on prose, write evidence, fetch rules don't
infer them). ~30 lines total — universal to every reader
(orchestrator + every dispatched role + ad-hoc operator query).

Orchestrator-specific content — pre-flight rules, dispatch
loop, plan composition, routing rules — does **not** live in
this artifact. It lives in `config/seed/system/agents/claude_orchestrator.md`,
composed into the orchestrator session via `agent_get` plus the
substrate's session-start agent profile inheritance. Reading an
orchestrator concern in this walkthrough? Cite the orchestrator
agent doc, not the agent_process artifact.

> The session has the substrate fundamentals in context BEFORE
> any verb fires. Every later call is in service of those
> fundamentals — not a discovery of them.

---

## Step 1 — bootstrap into a project

The handshake's bootstrap directive: when the prompt references
substrate primitives, `project_set(repo_url=<git remote get-url
origin>)` is the first call.

`project_set` does three things in one roundtrip:

1. **Resolves** the git remote to a project row via
   `repos.GetByRemote(workspace, canonical)` →
   `repo.project_id` → `projects.GetByID(...)`. (The legacy
   `projects.git_remote` column is gone — sty_14dfd05b.)
2. **Registers the session** — auto-attaches the substrate
   session row keyed by the `Mcp-Session-Id` header. No separate
   `session_register({})` call needed; that verb exists for
   stdio / test callers that can't set the header.
3. **Returns the orientation bundle** — project row + intent
   prose + active principles in one payload.

### 1a. shell — `git remote get-url origin`

```
git@github.com:bobmcallan/satellites.git
```

Not an MCP call.

### 1b. `project_set({ repo_url })`

```json
// request
{ "repo_url": "git@github.com:bobmcallan/satellites.git" }

// response — mixed: runtime + seed
{
  "project_id": "proj_7a62aedb",
  "status": "resolved",
  "mcp_url": "https://satellites-pprod.fly.dev/mcp?project_id=proj_7a62aedb",
  "repo_url_canonical": "https://github.com/bobmcallan/satellites",
  "intent_body": "# Satellites — project intent …",
  "principles": [
    {"name": "Pipeline integrity", "scope": "project", "body": "…"},
    {"name": "Quality over speed", "scope": "project", "body": "…"},
    {"name": "Evidence must be verifiable", "scope": "project", "body": "…"}
  ]
}
```

`intent_body` carries the body of the `project_intent` artifact
seeded under `config/seed/<workspace_id>/<project_id>/artifacts/`
(per the path layout from sty_87e203c1); `principles[]` is every
active type=principle row scoped to the project plus any active
scope=system principles.

**Response: mixed seed + runtime.** Bodies are seed prose; the
membership / scope filtering is computed at request time. The
project row keys the rest of the session; subsequent
project-scoped verbs default to this id when omitted. The
bundle's intent + principles are the same content
`project_get()` returns on later turns when the agent needs
a refresh without re-resolving the repo URL.

If the repo isn't registered the response is
`{"status":"no_project_for_remote", "repo_url_canonical": "…"}` —
the orchestrator must ask the operator before calling
`project_create`.

---

## Step 2 — operator types `implement sty_a03449d1`

The first MCP call after this prompt is **always**
`story_get` (single-roundtrip orientation), or `story_get`
when the agent only needs the row and not the full bundle.

### 2a. `story_get({ id })`

Returns story row + project row + recent ledger evidence + the
resolved agent_process instruction markdown + category template.

```json
// request
{ "id": "sty_a03449d1" }

// response — mixed: runtime + seed
{
  "story": {                                   // runtime
    "id": "sty_a03449d1",
    "title": "portal: surface story task chain …",
    "description": "…",
    "acceptance_criteria": "…",
    "status": "backlog",
    "priority": "critical",
    "category": "bug",
    "tags": ["portal", "epic:operator-visibility", "order:07"],
    "fields": {}                              // template-driven, runtime values
  },
  "project": { "id": "proj_7a62aedb", … },     // runtime
  "agent_process":                              // seed: config/seed/system/artifacts/default_agent_process.md
    "# satellites · agent process\n\n## Primitives\n\n## Bootstrap\n\n…",
  "template": {                                 // seed: config/seed/system/story_templates/bug.md
    "category": "bug",
    "fields": [
      { "name": "repro",    "type": "text", "required": true,  … },
      { "name": "observed", "type": "text", "required": true,  … },
      { "name": "expected", "type": "text", "required": true,  … },
      { "name": "root_cause",            "required": false, … },
      { "name": "fix_commit",            "required": false, … },
      { "name": "regression_test_path",  "required": false, … },
      { "name": "post_deploy_check",     "required": false, … }
    ],
    "hooks": { … }
  }
}
```

**`story.*` and `project.*` are runtime.** **`agent_process`
is seed** (the `default_agent_process` artifact body). **`template`
is seed** (e.g. `config/seed/system/story_templates/bug.md` — the body
shipped with the substrate; the row's structured payload is
loaded from disk on boot).

### 2b. `task_walk({ story_id })`

```json
// request
{ "story_id": "sty_a03449d1" }

// response — runtime
{
  "story":          { "id":"sty_a03449d1", "title":"…", "status":"backlog" },
  "tasks":          [],
  "current_task_id": "",
  "action_summary": []
}
```

**Response: runtime.** Empty list = no plan submitted yet → the
session is the orchestrator and owns the plan composition.

---

## Step 3 — read contracts (pre-flight Rule 2)

> Before `task_submit(kind=plan)`, the orchestrator reads each
> contract document referenced by the planned actions, so the plan
> reflects the rubric the reviewer will enforce.

The orchestrator typically loads the three lifecycle floors
(plan / develop / story_close) plus any extras a story's shape
calls for.

### 3a. `document_get({ name: "contract:plan" })`

```json
// request
{ "name": "contract:plan" }

// response — seed: config/seed/system/contracts/plan.md
{
  "id":   "doc_…",
  "type": "contract",
  "name": "contract:plan",
  "scope": "system",
  "structured": {
    "name": "plan",
    "category": "plan",
    "delivers_by": "developer_agent",
    "reviewed_by": "story_reviewer",
    "evidence_required":
      "Two ledger artifacts recorded on the plan task …\n- plan.md\n- review-criteria.md\nPlus a submitted task list via task_submit(kind=plan, …)",
    "tags": ["v4","lifecycle","system"]
  },
  "body":
    "# Plan Contract\n\nDesigns the implementation strategy and decomposes the story into an ordered task list. Plan is the front-floor of every story …"
}
```

**Response: seed** — `config/seed/system/contracts/plan.md`.

### 3b. `document_get({ name: "contract:develop" })`

Returns `config/seed/system/contracts/develop.md`. **seed.**

The body's `evidence_required` block is the develop contract
rubric. Read here so the **plan** lays out an evidence model the
develop close will satisfy.

### 3c. `document_get({ name: "contract:story_close" })`

Returns `config/seed/system/contracts/story_close.md`. **seed.**

Required because every plan must end in `contract:story_close`
per `pr_mandate_reviewer_enforced`. The story_reviewer rejects
plans that omit this floor.

---

## Step 4 — read principles (constraints, not options)

### `principle_list({ project_id, active_only: true })`

```json
// request
{ "project_id": "proj_7a62aedb", "active_only": true }

// response — array of mixed rows
[
  {                                              // body = seed: config/seed/wksp_5b3257d1/proj_7a62aedb/principles/pr_evidence.md
    "id":     "pr_evidence",
    "type":   "principle",
    "name":   "pr_evidence",
    "status": "active",                          // runtime field
    "body":   "# Principle: evidence …"
  },
  {
    "id":   "pr_mandate_reviewer_enforced",      // body = seed
    "body": "# Principle: every story is reviewer-enforced …"
  },
  {
    "id":   "pr_no_unrequested_compat",          // body = seed
    "body": "# Principle: do not add backwards-compat layers …"
  },
  {
    "id":   "pr_root_cause",                     // body = seed
    "body": "# Principle: fix at the source …"
  },
  {
    "id":   "pr_reviewer_voice_authoritative",   // body = seed
    "body": "# Principle: reviewer rejection IS operator authority …"
  },
  {
    "id":   "pr_substrate_provides_context",     // body = seed
    "body": "# Principle: the substrate composes the agent's prompt …"
  }
  // …
]
```

**Each row's `body` is seed** (`config/seed/wksp_5b3257d1/proj_7a62aedb/principles/<name>.md`).
The wrapper (id, status, updated_at) is runtime — operators can
deactivate a principle without re-seeding.

---

## Step 5 — read agent capability + reviewer rubrics

The plan submission passes `agent_id` for each task, and the
substrate validates capability via the agent's `delivers:` /
`reviews:` lists at `task_add` time. To pick the right ids
the orchestrator reads the agent catalog.

### 5a. `agent_get({ name: "developer_agent" })` (or `agent_list`)

```json
// response — seed: config/seed/system/agents/developer_agent.md
{
  "id":   "agent_…",
  "type": "agent",
  "name": "developer_agent",
  "structured": {
    "delivers":            ["contract:plan", "contract:develop"],
    "permission_patterns": [ "Read:**", "Edit:**", … ],
    "instruction":         "Drive the read-and-author phases …",
    "tags":                ["v4","agents-roles","lifecycle","role-shaped"]
  },
  "body": "# Developer Agent\n\nRole-shaped agent covering …"
}
```

**Response: seed** — `config/seed/system/agents/developer_agent.md`.

The `delivers:` list confirms `developer_agent` is the right
`agent_id` for both `contract:plan` and `contract:develop` work
tasks.

### 5b. `agent_get({ name: "story_reviewer" })`

Returns `config/seed/system/agents/story_reviewer.md`. **seed.**

The body IS the reviewer rubric the autonomous reviewer service
runs against the plan / story_close close evidence. Reading it
before composing the plan tells the orchestrator what shape of
evidence will be accepted.

### 5c. `agent_get({ name: "story_close_agent" })`

Returns `config/seed/system/agents/story_close_agent.md`. **seed.**

`delivers: ["contract:story_close"]` — this is the agent for
the closing task pair.

---

## Step 6 — submit the plan

The orchestrator now has everything: the story AC, the contract
rubrics, the active principles, the agent capability map, and
the empty task chain. Time to compose + submit.

### `task_submit({ kind: "plan", story_id, plan_markdown, tasks })`

```json
// request
{
  "kind":      "plan",
  "story_id":  "sty_a03449d1",
  "plan_markdown":
    "# Plan: surface story task chain — sty_a03449d1\n\n## Scope\n…\n## Files to change\n…\n## Approach\n…\n## Test strategy\n…\n## AC mapping\n…\n## Rubric updates\nNo substrate-primitive changes in this story; pure portal/SSR + WS bridge.\n",
  "tasks": [
    { "kind":"work",   "action":"contract:plan",        "agent_id":"agent_developer",   "description":"Author plan.md + review-criteria.md and submit the chain." },
    { "kind":"review", "action":"contract:plan",        "description":"Story reviewer verifies the plan." },
    { "kind":"work",   "action":"contract:develop",     "agent_id":"agent_developer",   "description":"Edit story_view.go + template + WS bridge; bump .version; tests." },
    { "kind":"review", "action":"contract:develop",     "description":"Development reviewer verifies the develop close." },
    { "kind":"work",   "action":"contract:story_close", "agent_id":"agent_story_close", "description":"Closing-evidence row; transition story to done." },
    { "kind":"review", "action":"contract:story_close", "description":"Story reviewer verifies close evidence." }
  ]
}

// response — runtime
{
  "plan_ledger_id": "ldg_4f1a8c00",
  "task_ids": [
    "task_5ca2ef…",   // plan work
    "task_82c0ff…",   // plan review (status=planned, gated)
    "task_1220fe…",   // develop work (status=published, gated by predecessor close)
    "task_257080…",
    "task_5e3393…",
    "task_c1f598…"
  ]
}
```

**Response: runtime.** Validation runs server-side:

- `plan_first_task_must_be_plan` (tasks[0] action must be `contract:plan`),
- `missing_review_for:<action>` (every work has its sibling review),
- `agent_cannot_deliver:<id>` / `agent_cannot_review:<id>` (capability),
- `invalid_action_format` / `review_action_mismatch`.

On accept the substrate writes:

- one `kind:plan` ledger row (carrying `plan_markdown`),
- the six task rows (work tasks at `status=published`, review
  tasks at `status=planned` — the reviewer service holds them
  back until the sibling work closes).

The plan task itself is `tasks[0]`; submission **does not**
auto-close it. The orchestrator (or its dispatched plan agent)
still has to claim, do the read-and-author work, write the
two evidence rows, and close the plan task.

---

## Step 7 — claim the plan task

### `task_claim({ task_id })`

```json
// request
{ "task_id": "task_5ca2ef…" }

// response — runtime
{
  "id":          "task_5ca2ef…",
  "story_id":    "sty_a03449d1",
  "kind":        "work",
  "action":      "contract:plan",
  "status":      "claimed",
  "claimed_by":  "mcp-session-4b2b4406-…",
  "claimed_at":  "2026-05-05T08:21:51Z"
}
```

**Response: runtime.** Substrate enforces:

- predecessor gate (none for plan),
- agent capability (the claiming session's stamped agent must
  carry `contract:plan` in its `delivers:`),
- the task is still `published` (not already claimed).

---

## Step 8 — write plan-task evidence (two ledger rows)

The plan contract's `evidence_required` (read in step 4a) lists
two artifacts: `plan.md` and `review-criteria.md`, both ledger
rows tagged `task_id:<plan_task>`, `kind:evidence`.

### 8a. `ledger_append({ … plan.md … })`

```json
// request
{
  "project_id": "proj_7a62aedb",
  "story_id":   "sty_a03449d1",
  "type":       "evidence",
  "tags":       ["kind:evidence", "task_id:task_5ca2ef…", "phase:plan", "artifact:plan.md"],
  "content":
    "# plan.md — sty_a03449d1\n\n## Files to change\n- internal/portal/story_view.go — populate TaskChain from task store\n- pages/templates/_panel_stories.html — render task rows\n- …\n\n## AC mapping\n- View-model populates TaskChain → story_view.go:taskChainForStory.\n- Iter-2 retry exposes prior_task_id → template + test.\n- WS task.<status> patching → common.js _applyTaskEvent.\n…"
}

// response — runtime
{
  "id":          "ldg_a1b2c3d4",
  "type":        "evidence",
  "project_id":  "proj_7a62aedb",
  "story_id":    "sty_a03449d1",
  "tags":        ["kind:evidence","task_id:task_5ca2ef…","phase:plan","artifact:plan.md"],
  "durability":  "durable",
  "source_type": "agent",
  "status":      "active",
  "created_at":  "2026-05-05T08:22:14Z"
}
```

**Response: runtime.** The row is appended; the substrate emits
a `ledger.created` event the portal subscribes to.

### 8b. `ledger_append({ … review-criteria.md … })`

Same shape, different artifact:

```json
{
  "tags": ["kind:evidence","task_id:task_5ca2ef…","phase:plan","artifact:review-criteria.md"],
  "content":
    "# review-criteria.md — sty_a03449d1\n\nPer-AC verify / evidence / pass-fail boundary…\n\n## AC1: View-model populates TaskChain\n- Verify: `go test -run TestTaskChain_SixRowsRender ./internal/portal/`\n- Pass-fail: 6 chain rows render 6 <tr>; empty story renders 'No tasks'.\n…"
}
```

**Response: runtime.** Returns another `ldg_…` row id.

The two rows are now linked by tag to the plan task. The
reviewer service will dereference them by id when it grades
the plan close.

---

## Step 9 — close the plan task

### `task_submit({ kind: "close", task_id, outcome, evidence_ledger_ids })`

```json
// request
{
  "kind":     "close",
  "task_id":  "task_5ca2ef…",
  "outcome":  "success",
  "evidence_ledger_ids": ["ldg_a1b2c3d4", "ldg_e5f6g7h8"]
}

// response — runtime
{
  "task": {
    "id":           "task_5ca2ef…",
    "status":       "closed",
    "outcome":      "success",
    "completed_at": "2026-05-05T08:22:31Z"
  },
  "published_review_id": "task_82c0ff…"     // the sibling review task is now published
}
```

**Response: runtime.** The substrate:

1. closes the plan work task,
2. flips the paired `kind=review, action=contract:plan` task
   from `planned` → `published`,
3. emits `task.closed` and `task.published` events,
4. the autonomous reviewer service picks up the published
   review task on its own session, dereferences the two
   evidence rows + the plan-md row, runs the `story_reviewer`
   rubric (read in step 6b), and calls
   `contract_review_close` with verdict ∈ {accepted, rejected}.

On **accepted**: the review task closes with `outcome=success`
and the develop work task (next in the chain) is unblocked.

On **rejected**: the review task closes with `outcome=failure`,
the substrate spawns a successor `kind=work, action=contract:plan`
task carrying `prior_task_id=task_5ca2ef…` plus a paired
planned `kind=review`; the orchestrator's pre-flight Rule 3
kicks in (read the verdict ledger row, address the gaps,
submit the retry close on the iter-2 task).

The orchestrator session **never** invokes a reviewer verb
directly — there isn't one. The reviewer service runs in its
own runtime.

This is the floor of the planning contract. Subsequent steps
(develop / story_close + their reviews) repeat the
claim → evidence → close → reviewer-verdict pattern; their
walkthrough is out of scope for this doc.

---

## Summary — which responses are seed and which are runtime

| Step | Verb | Source |
|---|---|---|
| 0 | MCP `instructions` block (handshake) | **seed** — `config/seed/system/artifacts/default_agent_process.md` |
| 1a | `session_register` | **runtime** — session row |
| 2b | `project_set` | **runtime** — project row |
| 3a | `story_get` — `story` + `project` | **runtime** — story / project rows |
| 3a | `story_get` — `agent_process` | **seed** — `config/seed/system/artifacts/default_agent_process.md` |
| 3a | `story_get` — `template` | **seed** — `config/seed/system/story_templates/<category>.md` |
| 3b | `task_walk` | **runtime** — task rows |
| 4a | `document_get(contract:plan)` | **seed** — `config/seed/system/contracts/plan.md` |
| 4b | `document_get(contract:develop)` | **seed** — `config/seed/system/contracts/develop.md` |
| 4c | `document_get(contract:story_close)` | **seed** — `config/seed/system/contracts/story_close.md` |
| 5 | `principle_list` row bodies | **seed** — `config/seed/wksp_5b3257d1/proj_7a62aedb/principles/<name>.md` |
| 5 | `principle_list` row status / wrapper | **runtime** — principle metadata |
| 6a | `agent_get(developer_agent)` | **seed** — `config/seed/system/agents/developer_agent.md` |
| 6b | `agent_get(story_reviewer)` | **seed** — `config/seed/system/agents/story_reviewer.md` |
| 6c | `agent_get(story_close_agent)` | **seed** — `config/seed/system/agents/story_close_agent.md` |
| 7 | `task_submit(kind=plan)` | **runtime** — writes ledger row + task rows |
| 8 | `task_claim` | **runtime** — task row update |
| 9a/b | `ledger_append` | **runtime** — ledger row |
| 10 | `task_submit(kind=close)` | **runtime** — task row update + sibling publish |

### Rule of thumb

- **Anything the orchestrator reads to decide what to do** is
  seed (instructions, contracts, agents, principles, templates).
  Operators evolve substrate behaviour by editing markdown +
  reseeding — no rebuild.
- **Anything the orchestrator writes, claims, or transitions**
  is runtime (sessions, projects, stories, tasks, ledger rows).
  These are the audit trail.
- **Some responses are mixed**: `story_get` returns a runtime
  story row alongside the seeded agent_process artifact and
  category template in one roundtrip. `principle_list` rows
  carry a runtime status flag wrapped around a seeded body.

The boundary matters because operators edit *seed* to change
how the system reasons, and inspect *runtime* to see what the
system did.
