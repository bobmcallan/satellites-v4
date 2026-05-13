---
id: pr_substrate_model
name: substrate_model
scope: workspace
tags:
  - process
  - dispatch
  - context
  - configseed
  - lifecycle
---
# The substrate stores rows + exposes per-verb retrieval; operators author primitives lazily

The substrate stores typed documents per tier (agent, contract, principle, story, ledger evidence) and exposes per-verb retrieval. Dispatched agents collate the context they need by calling those verbs themselves; the substrate does NOT assemble or supply an agent's working context at dispatch time. There is no `dispatch_context` aggregate verb and the substrate is not growing one — per-verb retrieval keeps the substrate small and matches the configuration-over-code mandate.

## Dispatched-agent isolation

A dispatched agent runs in its own subprocess with its own `HOME` and its own `~/.claude` directory. It does NOT see the operator's memory directory at `~/.claude/projects/.../memory/`. It does NOT inherit the operator's open Claude Code conversation context.

`story_get` is the dispatch entry point — it returns the story body and the reference ids the orchestrator has pointed the agent at. From there the agent fetches what it needs:

- `agent_get(name=…)` — its own profile, voice, capability list, permission envelope.
- `contract_get(name=…)` — the rubric the action it is performing must satisfy.
- `principle_list(active_only=true, project_id=…)` — the system + project principles in force.
- `task_walk(story_id=…)` — the chain, including any `prior_task_id` and the verdict that triggered a retry.
- `ledger_*` — prior evidence rows the agent needs to read or extend.

The orchestrator's contribution at dispatch is `task_add(prompt=…)` — the rich instruction naming the story id, task id, agent role, and the explicit work. The dispatched bash subprocess carries only that thin pointer.

## What seeds at boot vs what operators author lazily

The substrate seeds **roles**, **agents**, **contracts**, **workflows**, **story_templates**, **principles**, **artifacts**, and **replicate_vocabulary** at boot via configseed. It does NOT seed `type=skill` or `type=reviewer` rows.

Skill documents (`type=skill`) bind to agents via `agent.skill_refs`. They appear lazily as a project surface grows — either through `agent_compose` minting a project-scope skill, or through an operator calling `skill_create` directly. Lifecycle agents ship with `skill_refs` empty by default; the substrate runs without any skill row in the system tier.

Reviewer dispatch lives on `type=agent` documents. The reviewer agents seeded today (`story_reviewer`, `development_reviewer`) are agent rows whose body IS the rubric prompt; `runReviewer` in `internal/mcpserver/close_handlers.go` dispatches them at close time based on contract name. `type=reviewer` is a writable type that no code path *reads* today — it exists for project-scope future expansion when an operator wants a reviewer identity decoupled from the dispatching agent.

A baseline skill / reviewer seed would invent rows nothing currently consumes (operator-maintained noise that drifts but doesn't drive behaviour) and blur the line between load-bearing system contracts and project assets. Operators add skills + reviewers when a real binding emerges.

## What this forbids

- **Relying on operator-side Claude Code memory to shape dispatched-agent behaviour.** Memory is orchestrator-only.
- **Treating the substrate as a context-assembly service at dispatch.** The split is: orchestrator's `task_add(prompt=…)` carries the thin pointer; the agent's per-verb MCP calls carry the rich context.
- **Hard-coding agent behaviour in Go.** Agent profiles live in `config/seed/agents/`. Changing how an agent works = editing that markdown, not changing the dispatch code.
- **Proposing a `dispatch_context` bundle verb.** Per-verb retrieval is the model.
- **Dispatching substrate work through Claude Code's `Agent` tool (or any subagent harness).** The two-surface model is MCP for authoring + `satellites-client task run` for execution; a third channel bypasses both.

## Worked failure mode — sty_4db0e025

`sty_4db0e025` (the per-noun convergence epic) dispatched its
develop slices via Claude Code's general-purpose `Agent` tool
rather than `satellites-client task run`. Both surfaces were
bypassed: no `task_add` per slice (no MCP audit row for "this
is what the orchestrator authored for the team member to do"),
and no `satellites-client task run` (no per-task permission
envelope, no `PreToolUse` hooks, no `client-{task_id}-…`
branch, no `<exe_dir>/worktree/` boundary). The work shipped
but the audit chain didn't exist — `task_walk(story_id)`
showed an empty chain on a story that had visibly produced
commits.

Root cause: operator-side Claude Code memory had sanctioned
the `Agent` tool as a dispatch path because the bootstrap
prose returned by `project_set` did not explicitly forbid it.
This principle's "What this forbids" list now names the third
channel directly. The `claude_orchestrator` agent doc and the
project intent now do too. If a future session is tempted to
shortcut through a subagent harness, the prose names the
exact failure mode it would reproduce.

Cite `pr_substrate_model` on any close whose evidence mentions
dispatch through the `Agent` tool, or whose `task_walk` chain
is missing the per-slice MCP authoring rows that should
accompany each delivered slice.

## When to revisit the no-baseline-skills/reviewers rule

Add a baseline if any of the following becomes true: a system-tier code path begins listing `type=skill` rows and failing-open is unacceptable; a system-tier code path begins reading `type=reviewer` rows; a generic skill becomes load-bearing for multiple agents and operators are each re-creating it project-by-project. Until then, the operator's first `skill_create` or `reviewer_create` is the right place for these to appear.
