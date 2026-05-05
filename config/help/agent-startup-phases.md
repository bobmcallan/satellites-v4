---
title: Agent Startup Phases
slug: agent-startup-phases
order: 25
tags: [help, agents, startup, context]
---
# Agent Startup Phases

What loads into a Claude session before the first user prompt
fires. This is the map of pre-prompt context — what the model
already knows before the operator types anything — and which
part of it satellites controls.

## Phase summary

| # | Phase | Source | Satellites? |
|---|---|---|---|
| 1 | Harness boot | Claude Code CLI built-ins, model card, environment block (CWD, OS, git status), permissions, hooks | No |
| 2 | Operator-side context | `~/.claude/CLAUDE.md`, project-tree `CLAUDE.md` files, auto-memory at `~/.claude/projects/<cwd>/memory/MEMORY.md` | No |
| 3 | MCP initialize handshakes | Each configured MCP server returns `serverInfo` + `capabilities` + `instructions` on `initialize`; tool catalogues on `tools/list` | **Yes — one blob** |
| 4 | Tool + skill registration | Harness built-in tools + MCP server tool catalogues + plugin skills surfaced into the tool list and skills index | No |
| 5 | System reminder assembly | Harness composes everything above into the pre-prompt block — repeated on every turn | No |
| 6 | First user prompt | The operator's first message arrives. Now the model can act — call `session_register`, `project_set`, fetch a story, etc. | — |

## Where satellites' contribution sits

```
Phase 1   harness boot                        ──┐
Phase 2   operator CLAUDE.md + auto-memory    ──┤  not satellites'
Phase 3   MCP initialize handshakes           ──┤
            └─ satellites: default_agent_process artifact  ◄── THE ONE THING
Phase 4   tool + skill registration           ──┤  not satellites'
Phase 5   system reminder assembly            ──┤
                                                ┘
Phase 6   first user prompt
```

## Satellites documents loaded at startup

| Document | Phase | Seed source | How it arrives in the model |
|---|---|---|---|
| `default_agent_process` artifact | 3 | `config/seed/artifacts/default_agent_process.md` | Returned verbatim as the satellites MCP server's `instructions` field; surfaced as a `<system-reminder>` block, repeated every turn |

That is the entire list. One markdown file.

## What satellites cannot influence at startup

- The harness's system prompt, tool schemas, or environment block.
- The operator's `CLAUDE.md` hierarchy.
- Auto-memory contents (operator-private, project-scoped under
  `~/.claude/projects/...`).
- Other MCP servers' `instructions` blobs.
- Phase ordering.

## What satellites can shape

- The contents of the `default_agent_process` artifact (the Phase
  3 contribution).
- The verbs that artifact directs callers to use *after* Phase 6
  fires — `project_set`, `story_context`, `task_context`,
  `agent_get`, `principle_list`, `document_get`.

Anything role-specific, project-specific, or task-specific must be
fetched after the first prompt arrives. The startup blob is one
size for all readers — orchestrator, dispatched subprocess,
reviewer, ad-hoc operator query.

## Implication for design

Because the same blob serves every reader, it must be:

- **universal** — applies to every role
- **minimal** — what doesn't apply has to be filtered mentally
  by every reader, every turn
- **a fetch map** — names the verbs each reader calls to get its
  role-specific / project-specific / task-specific context on
  demand

## After Phase 6 — what happens today

The handshake is loaded. The session is idle, waiting for the
operator. A prompt fires. The session then alternates between
two kinds of work:

- **Claude work** — model + Claude Code harness. Parses the
  prompt, chooses the next call, reads responses, synthesises,
  invokes harness tools (Read, Write, Bash, Edit, Grep). No
  satellites involvement.
- **Satellites MCP call** — the model invokes a verb against the
  satellites MCP server (`mcp__satellites__*`); the server
  returns a response payload; the model reads it as context for
  the next step.

A typical run, starting from the operator's prompt:

| Step | Kind | Detail |
|---|---|---|
| 1 | Operator | Types prompt — e.g. `implement sty_a03449d1`, `what's in this repo`, `show me contract:develop`. |
| 2 | Claude | Parse identifiers from the prompt (`sty_xxx`, repo url, contract name, agent name, task id). |
| 3 | Claude | Map each identifier to a satellites verb — guided by the tool catalogue's descriptions (which the harness surfaces from the MCP server's `tools/list` response). |
| 4 | Satellites MCP | `session_register({})` / `session_whoami({})` — returns session row. |
| 5 | Satellites MCP | `project_set(repo_url)` or `project_list({})` — returns project row(s). |
| 6 | Satellites MCP | `story_get` / `story_list` / `story_context` / `task_context` (target) — returns story or task + (sometimes) project + recent ledger. |
| 7 | Satellites MCP | `agent_get(name)` + `principle_list(project_id)` + `document_get(name="contract:…")` — each separately. |
| 8 | Claude | Synthesise the responses + act. |

There is no single context-bundle satellites verb today. Steps
4–7 are sequential and the model stitches the responses on its
own.

### Prompt examples

How common operator prompts map onto the satellites verb
catalogue:

| Operator prompt | Identifier extracted | Verb |
|---|---|---|
| `implement sty_a03449d1` | story id `sty_a03449d1` | `story_context(id)` |
| `run sty_cf8ff98b` | story id `sty_cf8ff98b` | `story_context(id)` |
| `do task_b3a91e` | task id `task_b3a91e` | `task_context(id)` (target — `sty_38bec58f`) |
| `what's in this repo` | (resolves repo url via `git remote get-url origin`) | `project_set(repo_url)` |
| `list stories in this project` | project id (from session) | `story_list(project_id)` |
| `show me contract:develop` | contract name `develop` | `contract_get(name)` |
| `which agent reviews story_close work?` | agent name (looked up by capability) | `agent_get(name)` |
| `which principles apply here` | project id (from session) | `principle_list(project_id)` |

The handshake doesn't carry a verb table — Claude already has
the full catalogue from the MCP `tools/list` response, with
authoritative descriptions and parameter schemas. The handshake
just orients the agent on what kinds of identifiers it might
parse and what primitives those identifiers point at; the
catalogue answers "which verb."

### Target verb shape — what has to land

Even though the handshake no longer carries a verb table, the
epic's stories shape **the catalogue itself** — adding verbs
that don't exist today and tightening the return payloads of
ones that do. Each row below is the target shape and the story
that lands it.

| Verb / behaviour | Today | Closes via |
|---|---|---|
| `project_set(repo_url)` returns project + intent | Returns project only; no intent prose | `sty_31d51494` layer 2 |
| `project_get(id)` returns project + intent + principles | Returns project only | Same |
| `story_context(id)` includes project intent in the bundle | Returns story + project + ledger + template — no intent | Same |
| `task_context(id)` exists | **Does not exist** | `sty_38bec58f` |
| `agent_get` / `contract_get` / `principle_get` enforce type on read | Forwards to generic handler — no type filter | `sty_7cfe5e29` |
| Project-scoped seed loader | System-only seeds today | `sty_8868eaf4` |

Sequencing matters: seed the slim handshake only after the
catalogue verbs it implicitly relies on (`task_context`, type-
safe `_get`) have landed — otherwise prompts of the shapes in
the table above resolve to the wrong verbs or silently
mistype.

### Concrete example

| Step | Kind | Call / action |
|---|---|---|
| 1 | Operator | Typed: *"review existing stories. Is there one which removes the changelog…"* |
| 2 | Claude | Parsed prompt — no identifiers, intent = "find story matching X" |
| 3 | jcodemunch MCP | `list_repos` (different server — indexing check) |
| 4 | Satellites MCP | `session_whoami({})` → `session_not_registered` |
| 5 | Satellites MCP | `project_list({})` — 4 projects returned |
| 6 | Satellites MCP | `story_list(project_id=…)` — 643KB overflow, fell to file |
| 7 | Claude / harness | `bash` + `jq` on the file (overflow workaround) |
| 8 | Satellites MCP | `story_get(id=sty_cf8ff98b)` — returned the candidate |
| 9 | Claude | Synthesised + answered |

Nine steps from prompt to answer. Three were satellites
context-stitching that the target shape (`story_context` with
project intent + `task_context` for task work) would deliver in
one or two roundtrips. Two more steps (3, 7) only fired because
of the substrate's overflow handling on `story_list` —
unrelated to the fetch flow itself.
