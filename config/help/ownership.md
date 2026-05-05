---
title: Ownership Tiers
slug: ownership
order: 70
tags: [help, ownership, architecture, configuration-over-code]
---
# Ownership Tiers

Satellites separates *who can author what* into four strict tiers. Each
tier owns one surface; mixing them is a category error that the
reviewer is asked to flag.

```
┌─────────────────────────────────────────────────────────────┐
│ INFRA TEAM                                                  │
│   ENV vars + scripts/satellites.toml                        │
│   Sole purpose: get the substrate running.                  │
│   Does NOT tune behaviour.                                  │
├─────────────────────────────────────────────────────────────┤
│ PROJECT / SATELLITES TEAM (the maintainers of this repo)    │
│   ./config/seed/* and ./config/help/* (markdown)            │
│   Authors every system-tier default — agents, contracts,    │
│   principles, artifacts, workflows, help docs.              │
│   Tuning behaviour = editing seed markdown, then running    │
│   `system_seed_run` (or restarting).                        │
├─────────────────────────────────────────────────────────────┤
│ WORKSPACE / PROJECT OWNERS                                  │
│   Per-project document overrides on top of seed.            │
│   Future surface — not implemented yet. Seed is the only    │
│   authoring tier today.                                     │
├─────────────────────────────────────────────────────────────┤
│ ORCHESTRATOR SESSION (Claude Code, the operator's session)  │
│   Executes seed-prescribed prose; fetches project content   │
│   via MCP at runtime. Decides domain matters within that    │
│   frame — does NOT decide policy or routing.                │
└─────────────────────────────────────────────────────────────┘
```

## Why this matters

Two satellites principles converge here, and both are violated when
the tiers blur:

- **Configuration over code** (`docs/architecture-configuration-over-code-mandate.md`).
  Behaviour is declared in seed markdown rows — not in branching Go
  code, not in runtime KV switches the agent must consciously
  consult. If you find yourself writing `if cfg.X == "bash" { ... }`
  in Go to steer agent behaviour, the right home is seed prose: the
  agent reads the prose and follows it.
- **Substrate provides context** (`pr_substrate_provides_context`).
  The agent's operating instructions arrive in the dispatched
  handshake. The agent doesn't pull configuration switches at
  runtime to decide *what to do*; the handshake already told it.

Mixing tiers — e.g. exposing dispatch behaviour as a TOML field the
infra team must wire, or as a KV row the orchestrator must `kv_get`
to consult — re-introduces the code-shaped runtime branching that
configuration-over-code exists to eliminate.

## Tier 1: Infra team — ENV / TOML

The infra team's surface is *deployment*. Their job is to get the
substrate running on its target environment (Fly.io, Docker,
bare metal). Examples of fields they own:

- `SATELLITES_DB_DSN` — where the SurrealDB lives.
- `SATELLITES_PUBLIC_URL` — externally-reachable base URL.
- `SATELLITES_OAUTH_*` — OAuth issuer / TTLs / secrets.
- `SATELLITES_GEMINI_API_KEY` — third-party LLM key (used by the
  `/healthz` liveness pinger today).
- `SATELLITES_DOCS_DIR`, `SATELLITES_SEED_DIR`, `SATELLITES_HELP_DIR`
  — filesystem paths to the seed material.
- `SATELLITES_TASKS_RETENTION_DAYS` — operational retention window.

Full list lives in `internal/config/config.go`'s `FieldDefinitions`
table. None of these affect *agent behaviour* — they wire the box
to the network and the disk.

**What the infra team does NOT own:** anything that changes how an
agent dispatches, what permissions it carries, what evidence it
produces, what the reviewer accepts. All of that is project-team
seed authoring.

## Tier 2: Project / satellites team — `./config/seed/*` + `./config/help/*`

This is where the maintainers of *this repo* (the satellites project
team, including you when you author here) declare every system-tier
default. The seed loader (`internal/configseed`) reads these
directories at boot and upserts each markdown file into the document
store as the system-scope row of its type.

| Directory | Document type | Purpose |
|---|---|---|
| `config/seed/agents/` | `agent` | Per-role profile, voice, `delivers:` / `reviews:` lists, `permission_patterns` |
| `config/seed/contracts/` | `contract` | The lifecycle steps (plan, develop, push, merge_to_main, story_close) — what each does, what evidence it requires |
| `config/seed/principles/` | `principle` | The behavioural invariants the reviewer cites |
| `config/seed/artifacts/` | `artifact` | The handshake content — primarily `default_agent_process.md`, the body the substrate emits as MCP server-instructions |
| `config/seed/roles/` | `role` | Reserved (today thin) |
| `config/seed/workflows/` | `workflow` | Reserved for non-default contract sequences |
| `config/help/` | `help` | The pages this section renders |

Tuning agent behaviour means **editing seed markdown**, then either
restarting the substrate or calling `system_seed_run` for a hot
re-seed. There is no other path. If you reach for a Go function or
a KV switch to change behaviour, the right home was seed.

## Tier 3: Workspace / project owners — override layer (future)

The intended future shape: a workspace or project owner can override
seed-tier defaults with a document of the same name at their scope
(`scope=workspace` or `scope=project`). The resolver walks
project → workspace → system, returning the lowest-tier match.

This layer is **not implemented yet** for behaviour-bearing
documents. Today, seed is the only authoring tier. The hooks for
the override (workspace-scoped + project-scoped document storage)
exist; the resolver chains do not.

## Tier 4: Orchestrator session — execution + domain decisions

The orchestrator is your active Claude Code session connected to
the satellites MCP server. Its operating instructions arrive in
two parts:

1. **The handshake** delivers the seed-prescribed how:
   - The `default_agent_process` artifact body (the dispatch
     mechanism, the routing rules, the fundamentals).
   - The agent doc body for the role the session is acting as
     (orchestrator, developer, reviewer).
   - Active principles cited by the agent's rubric.
   - Contract document body for the action being performed.
2. **MCP calls** fetch the project content at execution time:
   - `story_get` / `story_context` — the story body, AC, fields.
   - `task_walk` — where the chain currently is.
   - `ledger_recall` / `ledger_list` — prior evidence, verdicts.
   - `repo_get_file` / `repo_search_text` — code under review.
   - `document_get` — any other system or project document.

**What the orchestrator decides:** the story design, the task plan
(which contracts to invoke in what order, which agent gets each
work task), the implementation approach, the code itself. These
are domain decisions the orchestrator is paid to make.

**What the orchestrator does NOT decide:**
- Which contract sequence is canonical (seed-prescribed: plan →
  develop → push → merge_to_main → story_close).
- Which agent reviews which contract (each agent doc declares its
  `reviews:` list; the orchestrator matches but doesn't invent).
- The dispatch mechanism (the `default_agent_process` body
  prescribes how to spawn dispatched agents — `claude -p` with
  per-task worktrees, permission_patterns enforced via
  `--allowedTools`).
- The reviewer rubric (the reviewer agent doc body IS the rubric;
  the orchestrator can't argue with it).

## Worked example: how to change the dispatch timeout

You want dispatched agents to run for 1200 seconds instead of 600.

**Wrong path** (mixes tiers):
- Add a TOML field `agent_dispatch_timeout = 1200` to
  `internal/config/config.go` and have the orchestrator read it via
  `kv_get` or hard-code it. Treats deployment config as a behaviour
  knob → ENV/TOML in the infra-team tier, but the change is a
  *behaviour* change.

**Right path** (respects tiers):
- Edit `config/seed/agents/developer_agent.md` (or wherever the
  timeout currently lives in agent doc frontmatter) and bump the
  value.
- Run `system_seed_run` (or restart). The orchestrator's next
  dispatch reads the updated agent doc as part of its handshake
  context and applies the new timeout.

Same outcome, but the change lives where the architecture says it
belongs: in the project-team seed surface, declared, auditable,
loadable by configseed, and overridable per workspace/project when
that layer ships.

## Why no `agent.dispatch.*` KV rows

An earlier draft of `sty_51571015` proposed four KV rows
(`agent.dispatch.mode`, `agent.dispatch.bash.claude_path`, etc.)
that the orchestrator would consult at dispatch time. That shape
was retired before landing because:

- It would require the orchestrator to make a `kv_get` call to
  decide *what to do* — a code-shaped runtime branch dressed up as
  configuration.
- It split the dispatch instructions across two surfaces (agent
  doc body + KV rows) so neither was the source of truth.
- It blurred the infra/project ownership: who owns a KV row?
  Neither tier did, cleanly.

Replacement: the dispatch mechanism is fully declared in
`config/seed/artifacts/default_agent_process.md` (the handshake
artifact). The orchestrator reads the prose and follows it. Per-
agent timeouts / permissions live in the agent doc frontmatter.
Project-team authoring; orchestrator execution; no third surface.
