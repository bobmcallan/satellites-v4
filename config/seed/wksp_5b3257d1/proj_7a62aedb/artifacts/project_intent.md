---
name: project_intent
tags: [kind:project-intent]
---
# Satellites — project intent

Satellites is a configuration-over-code substrate where orchestration
agents drive work via tasks. This project — satellites itself —
treats the running substrate as its data source: stories, principles,
agents, contracts, and ledger evidence all live in satellites and are
fetched via MCP.

Stories carry intent. Tasks dispatch one agent. Agents read their
own doc, the project intent, active principles, and the task body
via MCP — no operator-side context injection. Prose is authoritative;
new behaviour comes from new prose and a re-seed, not from a code
change.

## Authoring vs execution — two surfaces, one separation

The substrate exposes two distinct surfaces and the orchestrator
must use them for distinct purposes. Conflating them — or routing
work through a third channel — is the failure mode this section
exists to forbid.

- **MCP = authoring surface.** The orchestrator uses MCP write
  verbs to AUTHOR substrate primitives: `story_add`, `task_add`,
  `ledger_append`, `story_update`, document and principle
  mutations, contract + workflow edits, verdict rows. All
  planning, framing, observation, evidence-writing, and
  verdict-writing flow over MCP. Read-only MCP verbs
  (`project_set`, `story_get`, `task_walk`, `ledger_list`,
  `principle_list`, …) carry the orientation reads the
  orchestrator needs to plan.

- **satellites-client = execution surface.** The orchestrator
  dispatches the team-member agent via
  `satellites-client task run <task_id>`. The dispatched team-
  member subprocess — running under its agent's permission
  envelope in a per-task worktree on a `client-{task_id}-…`
  branch — performs the implementation work: file edits, builds,
  tests, commits, pushes. The orchestrator does NOT execute that
  work itself.

The boundary is the **productive tension**. The orchestrator
cannot shortcut "I'll just write the code myself" because
development work lives behind `task run`. That forces explicit
authoring (via MCP), explicit dispatch (via the CLI), explicit
review (orchestrator reads team-member evidence), and explicit
iteration (reviewer rejection → fresh MCP `task_add` → another
`task run`). Same separation a human dev lead has with their team:
the lead authors + reviews; the team builds.

### First-run install

The `satellites-client` binary installs as
`./satellites/satellites-client` colocated under the consumer
project root (sibling `sty_796b8fe1`). When the binary is not
present on PATH or in the colocated location, call the
`satellites_init` MCP verb — it returns the install payload
(`install_required` / `update_available` / `up_to_date`) that
bootstraps the execution surface for the project.

### Lifecycle reference

The phases `plan → (develop → review → iterate) → commit → push
→ close` live in a workflow document (sibling `sty_e0c3d615`).
The orchestrator AUTHORS each phase's task via MCP; the team
member EXECUTES it via `satellites-client task run`; the
orchestrator REVIEWS the resulting evidence and either advances
to the next phase or iterates on a fresh work task.

### Forbidden alternatives

These bypass one or both surfaces and break the audit chain.
They are not allowed regardless of how convenient they look:

1. **Claude Code's `Agent` tool (or any subagent harness) for
   substrate work.** Dispatching a slice through a general-purpose
   subagent leaves no `task_add` row in MCP and no
   `task run` envelope in `satellites-client`. Both surfaces are
   bypassed. Worked failure mode: `sty_4db0e025` shipped develop
   slices via Claude Code's `Agent` tool — no per-slice MCP
   authoring trail, no per-task permission envelope, no branch
   template, no worktree-root. Cite `pr_substrate_model`.
2. **Direct file edits, builds, tests, commits, or pushes by the
   orchestrator.** Those are execution-surface activities owned
   by the dispatched team member under `satellites-client task
   run`. The orchestrator running `git commit` directly skips
   the team-member identity, the worktree boundary, and the
   reviewer dispatch the contract requires.
3. **Operator-side Claude Code memory as a source of substrate
   context.** Project context lives in the substrate
   (`project_set`, `story_get`, `principle_list`, `agent_get`,
   `contract_get`, `ledger_*`). Memory at
   `~/.claude/projects/.../memory/` is orchestrator-only and
   does NOT flow into a dispatched team-member subprocess. Cite
   `pr_substrate_model`.
