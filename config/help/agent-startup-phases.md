---
title: Agent Startup Phases
slug: agent-startup-phases
order: 25
tags: [help, agents, startup, context]
---
# Agent Startup Phases

What loads into a Claude session before the first user prompt
fires, and what runs once a prompt arrives. This is the map of
pre-prompt context — what the model already knows before the
operator types anything — and the phases that follow.

Phases 1–5 are **startup**: no user interaction, the session is
preparing itself. Phase 6 is the **user prompt** — the first
operator message. Phases 6.1+ are post-prompt processing.

Each phase is labelled **Claude** (model + Claude Code harness),
**Satellites** (the satellites MCP server), or **Operator**.

---

## Startup — no user interaction

### Phase 1 — Harness boot

**Owner:** Claude Code harness.

Claude Code CLI starts and reads its built-ins: model card,
environment block (CWD, OS, shell, git status, registered
platforms), permissions, hooks. User settings
(`~/.claude/settings.json`) and project settings
(`<project>/.claude/settings.json` plus `.local.json`) are
loaded.

### Phase 2 — Operator-side context

**Owner:** Claude Code harness (reads operator files).

The harness reads `~/.claude/CLAUDE.md` (user global), each
`<project>/CLAUDE.md` recursively up the tree (project
instructions), and any auto-memory at
`~/.claude/projects/<sanitized-cwd>/memory/MEMORY.md` (capped at
~200 lines). All surfaced into the model's system context.

### Phase 3 — MCP initialize handshakes

**Owner:** Claude initiates; each MCP server responds. Includes Satellites.

For each configured MCP server, the harness opens a connection
and runs:

- `initialize` — server returns `serverInfo`, `capabilities`,
  and an `instructions` field (free-form prose surfaced into
  the model's system context as a `<system-reminder>` block on
  every turn).
- `resources/list`, `prompts/list` (when the server advertises
  these capabilities).

**Satellites contribution this phase:** the
`default_agent_process` artifact, returned as the `instructions`
field. Sourced from
`config/seed/artifacts/default_agent_process.md`.

### Phase 4 — Tool + skill registration

**Owner:** Claude initiates; each MCP server returns its catalogue.

Each MCP server's `tools/list` response is registered into the
harness's tool surface. Some tools are top-of-prompt (full
schemas always loaded — Read, Edit, Bash, etc.); others are
deferred (names announced in the deferred-tool list, schemas
fetched on demand via `ToolSearch` when the model is about to
call). Plugin skills are surfaced into the skills index.

### Phase 5 — System reminder assembly

**Owner:** Claude Code harness.

The harness composes the final pre-prompt context block:

1. Built-in Anthropic system prompt.
2. Environment block.
3. Top-of-prompt tool schemas.
4. `<system-reminder>` blocks: deferred-tool list, skills, each
   MCP server's `instructions` field.
5. claudeMd block (CLAUDE.md hierarchy, auto-memory, current
   date).

Reassembled on every turn — the same context is rebuilt each
time the model is invoked.

---

## Prompt — interactive

### Phase 6 — User prompt arrives

**Owner:** Operator.

Operator types the first message. Examples:
`implement sty_a03449d1`, `what's in this repo`,
`show me contract:develop`. The session, idle until now, begins
its first turn.

### Phase 6.1 — Parse identifiers

**Owner:** Claude.

Model parses the prompt for identifiers it recognises from the
handshake's primitives: story id (`sty_<hex>`), task id
(`task_<hex>`), repo url, project id, contract name, agent
name.

### Phase 6.2 — Map identifiers to verbs

**Owner:** Claude.

Each identifier maps to a satellites verb. The model picks the
verb by reading the tool catalogue (loaded at Phase 4) — verb
names plus descriptions plus parameter schemas. The handshake's
"Operating principle" reminds the agent to fetch from prose,
not infer from training.

### Phase 6.3 — Satellites MCP fetch

**Owner:** Satellites MCP (per call) + Claude (deciding which call to make).

Model invokes verbs against the satellites MCP server. Each
call returns a response payload; the model reads it as context
for the next step. Today there is no single context-bundle
verb, so a typical session stitches:

- `project_set(repo_url)` or `project_list({})` — project
  row(s).
- `story_get` / `story_list` / `story_context` /
  `task_context` (target — see "Target verb shape") — story or
  task plus (sometimes) project plus recent ledger.
- `agent_get(name)` + `principle_list(project_id)` +
  `document_get(name="contract:…")` — each separately.

Each is a separate roundtrip the model orchestrates.

**Sessions — protocol vs registry.** Two layers are easy to
conflate:

- The **MCP protocol session** is auto-established at Phase 3.
  The `initialize` handshake mints a session ID; the harness
  echoes it on every subsequent call. Already present, no
  explicit step.
- The **satellites session registry** is a substrate-level
  binding of user/agent identity to that protocol session.
  Opt-in. `session_register({})` adds the binding;
  `session_whoami({})` queries it.

Most verbs are project-scoped or take explicit IDs and run
fine without a session-registry binding. `session_register` is
**conditional** — called only when:

- A verb returns the structured `session_not_registered` error
  (the error names the recovery action).
- The agent is about to do role-gated work (claim a task,
  submit a plan, close a contract).

A speculative `session_whoami({})` early in a session is
common but generally unnecessary — a wasted roundtrip unless
identity binding is actually required for the work.

### Phase 6.4 — Synthesise and act

**Owner:** Claude.

Model assembles the responses, decides what to do, and acts:
write code, mint tasks, append ledger rows, answer the
operator, or invoke harness tools (Read, Write, Bash, Edit,
Grep) for local work that doesn't involve satellites.

---

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
6.1–6.4   parse → map → fetch → act           ◄── interactive work begins
```

## Satellites documents loaded at startup

| Document | Phase | Seed source | How it arrives in the model |
|---|---|---|---|
| `default_agent_process` artifact | 3 | `config/seed/artifacts/default_agent_process.md` | Returned verbatim as the satellites MCP server's `instructions` field; surfaced as a `<system-reminder>` block, repeated every turn |

That is the entire list. One markdown file.

## What satellites cannot influence at startup

- The harness's system prompt, tool schemas, or environment
  block.
- The operator's `CLAUDE.md` hierarchy.
- Auto-memory contents (operator-private, project-scoped under
  `~/.claude/projects/...`).
- Other MCP servers' `instructions` blobs.
- Phase ordering.

## What satellites can shape

- The contents of the `default_agent_process` artifact (the
  Phase 3 contribution).
- The verbs (and their descriptions) the artifact directs
  callers to use after Phase 6 fires — `project_set`,
  `story_context`, `task_context`, `agent_get`,
  `principle_list`, `document_get`. Tool descriptions
  registered in `internal/mcpserver/` are themselves
  instruction prose read by the model on every turn — see
  `sty_cd8b89c6` for the operator view onto that surface.

Anything role-specific, project-specific, or task-specific must
be fetched after the first prompt arrives. The startup blob is
one size for all readers — orchestrator, dispatched subprocess,
reviewer, ad-hoc operator query.

## Implication for design

Because the same blob serves every reader, it must be:

- **universal** — applies to every role
- **minimal** — what doesn't apply has to be filtered mentally
  by every reader, every turn
- **a fetch trigger** — directs each reader to fetch its
  role-specific / project-specific / task-specific context on
  demand via the tool catalogue

## Prompt examples

How common operator prompts map onto the satellites verb
catalogue (Phase 6.2's mapping work):

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

## Target verb shape — what has to land

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
catalogue verbs it implicitly relies on (`task_context`,
type-safe `_get`) have landed — otherwise prompts of the shapes
in the table above resolve to the wrong verbs or silently
mistype.

## Concrete example

A trace of one operator prompt as it flows through the satellites
MCP surface. Calls to other MCP servers in the session are out of
scope and excluded — this walkthrough covers the satellites
contribution only.

| Step | Phase | Owner | Call / action |
|---|---|---|---|
| 1 | 6 | Operator | Typed: *"review existing stories. Is there one which removes the changelog…"* |
| 2 | 6.1 | Claude | Parsed prompt — no identifiers, intent = "find story matching X" |
| 3 | 6.3 | Satellites MCP | `session_whoami({})` → `session_not_registered` |
| 4 | 6.3 | Satellites MCP | `project_list({})` — 4 projects returned |
| 5 | 6.3 | Satellites MCP | `story_list(project_id=…)` — 643KB overflow, fell to file |
| 6 | 6.3 | Satellites MCP | `story_get(id=sty_cf8ff98b)` — returned the candidate |
| 7 | 6.4 | Claude | Synthesised + answered |

Of these, three (steps 3–5) are satellites context-stitching that
the target shape (`story_context` with project intent +
`task_context` for task work) would deliver in one or two
roundtrips.
