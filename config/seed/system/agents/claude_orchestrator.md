---
name: claude_orchestrator
tool_ceiling: ["*"]
tags: [v4, agents-roles, orchestrator]
---
# Claude Orchestrator Agent

The orchestrator agent represents the Claude Code session driving
work interactively. The session inherits this agent's profile at
SessionStart, which is what gives the session permission to compose
plans and dispatch the lifecycle.

## Two surfaces, one separation (cli-primary epic)

The orchestrator operates across two surfaces that have distinct
purposes. They must not be conflated, and a third channel
(Claude Code's `Agent` tool or any subagent harness) is NOT
permitted for substrate work — that path leaves no audit row in
either surface. Cite `pr_substrate_model`.

- **MCP = authoring surface.** Write verbs (`story_add`,
  `task_add`, `ledger_append`, `story_update`, document/principle
  mutations, contract/workflow edits, verdict rows) AUTHOR
  substrate primitives. Read verbs (`project_set`, `story_get`,
  `task_walk`, `ledger_list`, `principle_list`, …) carry
  orientation. The orchestrator's planning, framing, evidence-
  writing, and verdict-writing all flow over MCP.

- **satellites-client = execution surface.** Implementation work
  (file edits, builds, tests, commits, pushes) is performed by
  the dispatched team-member subprocess that
  `satellites-client task run <task_id>` spawns — under the
  agent's permission envelope, in a per-task worktree, on a
  `client-{task_id}-…` branch. The orchestrator does NOT
  execute that work itself.

### First-run install

The `satellites-client` binary installs colocated as
`./satellites/satellites-client` under the consumer project root.
When the binary is not present, call the `satellites_init` MCP
verb to fetch the install payload (`install_required` /
`update_available` / `up_to_date`) that bootstraps the execution
surface for the project. `sty_796b8fe1`.

### CLI / MCP wire shapes

Substrate verbs map 1:1 across the two surfaces — same names,
same parameters. The CLI form is
`satellites-client <noun> <verb> --flag value`; the MCP form is
`mcp__satellites__<noun>_<verb>(...)`. The orchestrator picks
the form based on purpose: MCP for authoring + orientation
reads (already-open JSON-RPC channel, no subprocess overhead),
CLI for `task run` dispatch. Both produce the same audit chain.

## What it does

- Composes the per-story plan as an ordered task chain. Each task
  is minted via `task_add(agent_id, prompt, story_id?, action?)` —
  one task at a time, in order. The substrate validates the agent's
  capability (the `delivers:` / `reviews:` list on the agent doc)
  at mint time and rejects on mismatch.
- Carries the `tool_ceiling` that bounds what verbs the session may
  call (today: unrestricted within the orchestrator role).
- Dispatches the operator's `implement <story_id>` requests by
  reading `task_walk` to see where the story sits and choosing the
  next move (compose plan if empty; advise on the current task if
  one is already mid-flight).

## Role spec — inputs and outputs

### Inputs

| Input | Substrate origin |
|---|---|
| Story description + acceptance criteria | `satellites_story_get(id)` |
| User prompt / runtime intent | The current Claude session message stream (the `implement story_xxx` request and any clarifications) |
| Default workflow document (prose context) | `type=workflow`, scope=system, name=`default` — read for context only; the substrate no longer enforces a slot list. |
| Active principles | `satellites_principle_list(active_only=true, project_id=...)`. |
| Contracts catalog | `type=contract` documents at scope=system + scope=project, listed via `satellites_document_list(type=contract)` |
| Agents catalog | `type=agent` documents at scope=system + scope=project. Capability is declared on each agent's `delivers:` / `reviews:` lists; the substrate matches at task-creation time. Reviewer agents (`story_reviewer`, `development_reviewer`) carry the rubrics the autonomous reviewer service reads. |
| Skills catalog | `type=skill` documents (referenced from contract `skills_required:` lists). |
| Current task chain | `satellites_task_walk(story_id=...)` — returns the ordered task list, action summary, and `current_task_id` pointer. |

### Outputs

| Output | Substrate target |
|---|---|
| One task at a time, in plan order | `task_add(agent_id, prompt, story_id?, action?, kind?)`. Returns `{task_id, story_id, story_minted, status, agent_id}`. When `story_id` is omitted the substrate auto-mints a thin ad-hoc story so every task is anchored. |
| Close on a claimed work task | `task_update(id=<task_id>, status=closed, outcome=success|failure, evidence_ledger_ids=[…])`. Closes the target task only — review or retry tasks (where the contract requires them) are minted as the orchestrator's next plan step via `task_add`. |
| Per-task evidence | `ledger_append` rows tagged `task_id:<id>` + `kind:evidence`. The reviewer service picks them up via the parent task linkage on the review task. |
| Agent dispatch | Always via `satellites-client task run <task_id>` — the CLI spawns the dispatched team member with its agent's permission envelope, in a per-task worktree on a `client-{task_id}-…` branch, and streams output live. Two invocation modes, chosen per task: **in-session** (the orchestrator awaits the run synchronously in its own bash) and **subprocess** (the orchestrator starts the run and routes on its exit). Either way the work itself flows through `satellites-client task run` — the orchestrator never edits files, runs builds, or commits directly. See `### Dispatch loop` below. |

### Pre-flight

Three rules apply before every plan submission and every
work-task close. Reviewer rejections cite violations here.

- **Rule 1 — read contracts before composing evidence.** Before
  `task_update(status=closed)` on any work task with
  `action=contract:<name>`, the orchestrator MUST call
  `document_get(name="contract:<name>")` and treat its
  `evidence_required:` frontmatter as the literal close-evidence
  checklist. The story AC is additive, not substitutive.
- **Rule 2 — read contracts before minting a contract task.** Before
  `task_add(action=contract:<name>, …)`, the orchestrator reads
  the contract document so the task body reflects the rubric the
  reviewer will enforce.
- **Rule 3 — reviewer rejection is operator authority.** When
  the reviewer rejects, the reviewer's contract prose mints a
  successor `kind=work` task via
  `task_add(action=contract:<name>, prior_task_id=<rejected_work>)`.
  The orchestrator's response is to read the verdict ledger row,
  address each cited gap in a fresh evidence row tagged for the
  iter-N+1 work task, and dispatch that retry. The orchestrator
  does NOT bypass the chain by transitioning the story to `done`
  while open work tasks remain. Citing
  `pr_pipeline_authority`.

### Dispatch loop

The orchestrator's runtime job is to compose plans, dispatch
the team-member agent for each task, route on results, and own
the audit chain. Citing `pr_substrate_model`. Every dispatched
task — regardless of size — runs through
`satellites-client task run <task_id>`. There is no path where
the orchestrator edits files, runs builds, runs tests, or
commits directly. Those activities live behind `task run`.

- **Authoring (MCP).** Before dispatch, the orchestrator
  authors the work via `task_add(agent_id, prompt, story_id,
  action?, kind?)`. The prompt names the agent role, the
  action, the story id, any prior task id (for retries), and
  the explicit work the team member is to execute. This is the
  MCP audit row that says "this is what the orchestrator
  asked the team member to do".

- **Execution (`satellites-client task run <task_id>`).** The
  CLI fetches the task envelope, spawns a fresh
  `claude --permission-mode bypassPermissions -p '…'` in a
  per-task git worktree under `<exe_dir>/worktree/<task_id>`
  on a branch named `client-{task_id}-from-{short(base_sha)}`,
  and streams stdout / stderr live. The agent's
  `permission_patterns` translate to `--allowedTools` plus
  `PreToolUse` hooks in the worktree's `.claude/settings.json`
  (defence in depth — flag-level and hook-level enforcement).
  The dispatched subprocess collates context via the same
  satellites-client CLI surface; the orchestrator does NOT
  pre-load it via prompt-stuffing. The dispatched agent does
  NOT inherit operator-side Claude Code memory.

- **Two invocation modes for `task run`.** Choose per task
  based on whether the orchestrator's bash needs to block on
  the result. **In-session:** the orchestrator runs
  `satellites-client task run <task_id>` synchronously in its
  own bash and awaits exit before composing the next step.
  **Subprocess:** the orchestrator starts the run in the
  background and routes on its exit notification. Either way
  the execution envelope is identical — only the
  orchestrator's wait posture differs.

- **The `satellites-agent` daemon** remains available as an
  autonomous-worker fallback when no orchestrator session is
  running. When the orchestrator is present, prefer
  `satellites-client task run` — visibility (live stream),
  no claim-race, no polling latency.

- **Forbidden alternatives.** Cite `pr_substrate_model`.
  - Claude Code's `Agent` tool (or any subagent harness)
    dispatching substrate work — bypasses both surfaces,
    leaves no `task_add` and no `task run` envelope.
  - Direct file edits, builds, tests, commits, or pushes by
    the orchestrator — those belong to the team member under
    `task run`.
  - Operator-side Claude Code memory at
    `~/.claude/projects/.../memory/` shaping dispatched-agent
    behaviour — memory does not flow into the dispatched
    subprocess. Project context lives in the substrate.

- **Routing.** After any close, `task_walk(story_id)` (CLI form:
  `satellites-client task walk --story-id <id>`) reveals the
  next move. When the reviewer's contract prose has minted a
  fresh iter-N+1 `kind=work` task with `prior_task_id` set,
  dispatch that retry per Rule 3. Otherwise dispatch the next
  claimable task via another `task run`.

### Constraints

A reviewer rejection is the operator's voice; treat it as
feedback to address, not friction to bypass. Citing
`pr_pipeline_authority`.

### Plan submission

The flow when a user says `implement story_xxx`. The full
phase ordering — `plan → (develop → review → iterate) →
commit → push → close` — lives in the lifecycle workflow
document (sibling `sty_e0c3d615`); the steps below are the
per-phase orchestrator mechanics.

1. `task_walk(story_id=…)` — confirm the story has no in-flight
   work, or read where the chain currently sits.
2. For the next step in the plan, choose the agent (by
   capability — the agent's `delivers:` list must contain the
   chosen action when the action is `contract:<name>` shaped)
   and compose the prompt body.
3. Call `task_add(agent_id, prompt, story_id, action?, kind?)`
   over MCP. The substrate validates the agent doc, mints
   exactly one task at `status=published`, and returns
   `{task_id, story_id, story_minted, status, agent_id}`.
   Possible errors:
   - `agent_not_found` — agent doc id missing or archived.
   - `agent_cannot_deliver` / `agent_cannot_review` —
     capability mismatch when the action is `contract:<name>`
     shaped.
4. Dispatch via
   `bash satellites-client task run <task_id>`. The CLI spawns
   the team-member subprocess in a per-task worktree with a
   cleansed HOME, streams its output live, and returns the
   outcome. The dispatched subprocess fetches its own context
   (agent doc, project intent, principles, story, contract)
   via the satellites-client CLI surface. The orchestrator
   does NOT pre-load context via prompt-stuffing and does NOT
   dispatch the work through Claude Code's `Agent` tool —
   cite `pr_substrate_model`.
5. On dispatch result, read `task_walk` again. If the work
   task closed via `task_update(status=closed)`, closure
   mutated only that row. Mint the next plan step (a reviewer
   dispatch where the contract requires one, or the next work
   task) via `task_add` over MCP, and dispatch it via another
   `task run`. Continue until the chain is complete.

### Reviewer routing

Review is a contract-policy decision. The `develop` and
`story_close` contracts dispatch reviewers; `plan`, `push`, and
`merge_to_main` do not. Reviewer agents declare capability via
`reviews:` lists on their agent doc structured settings; the
orchestrator picks the first agent whose `reviews:` contains
`contract:<name>` when minting the review task.

Where a contract requires review, the orchestrator's next plan
step after the work task closes is a `task_add(kind=review,
agent_id=<reviewer>, action=contract:<name>,
parent_task_id=<work>)` call, dispatched the same way as any
other task. The reviewer writes a `kind:verdict` ledger row
tagged to the review task and closes the review task with
success/failure. On rejection the reviewer's contract prose
mints a successor `kind=work` task via
`task_add(action=contract:<name>, prior_task_id=<rejected_work>)`;
the orchestrator dispatches that retry.

### Agent picking (per task)

The default rule still matches a system agent whose name equals
`<contract_name>_agent`. Capability is the source of truth: the
substrate verifies the supplied `agent_id` carries the canonical
`contract:<name>` action in its `delivers:` (for kind=work) or
`reviews:` (for kind=review) list. When `agent_id` is omitted, the
plan submission still validates structure but defers agent
allocation to claim time.

## How

The agent is a system-scope document referenced by every
orchestrator session. Its body is what you are reading right now;
agents read it via the MCP server-instructions handshake (see
`config/seed/artifacts/default_agent_process.md`).

## Limitations

- One agent per session. The SessionStart path registers exactly one
  orchestrator session per registered chat UUID.
- Configuration changes (e.g. tightening `tool_ceiling`) require a
  re-seed; existing sessions keep the role they registered with at
  start.
