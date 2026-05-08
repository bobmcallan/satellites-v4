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
`config/seed/system/artifacts/default_agent_process.md`.

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

Model invokes verbs against the satellites MCP server. The
agent process artifact (Phase 3) directs the agent: when the
prompt references substrate primitives, **`project_set(repo_url=<git remote get-url origin>)` is the first call**. It binds the
session to the project, auto-registers the session row keyed by
the `Mcp-Session-Id` header, and returns the orientation bundle
in one roundtrip:

```
{ project_id, status: "resolved", mcp_url,
  intent_body, principles[] }
```

That single call replaces what used to be three separate fetches
(project row + intent + principles). On later turns,
`project_get(id=<project_id>)` returns the same bundle without
re-resolving the repo URL — useful when intent or principles
need a refresh.

Beyond the bootstrap, role-, story-, and task-specific context
is fetched on demand:

- `story_get(id)` — story + project + recent ledger +
  resolved agent_process artifact + category template, in one
  roundtrip.
- `task_get(id)` / `task_walk(story_id)` — task chain plus
  prior verdicts.
- `agent_get(name)` / `contract_get(name)` /
  `principle_list(project_id)` — fetched only when the
  orientation bundle alone doesn't carry enough context for
  the work at hand.

For prompts unrelated to substrate primitives ("what's in this
file", a quick code question), the bootstrap is unnecessary —
the artifact in the system context already orients the model.

**Sessions — protocol vs registry.** Two layers are easy to
conflate:

- The **MCP protocol session** is auto-established at Phase 3.
  The `initialize` handshake mints a session ID; the harness
  echoes it on every subsequent call. Already present, no
  explicit step.
- The **satellites session registry** is a substrate-level
  binding of user/agent identity to that protocol session.
  Auto-attached by `project_set` (the bootstrap call) — no
  separate `session_register` roundtrip needed for the typical
  flow.

`session_register` and `session_whoami` exist for callers that
skip the bootstrap (a worker that takes a `task_id` directly,
or a stdio test harness that can't set the session header).
The handshake's instructions don't direct the agent to call
them speculatively.

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
| `default_agent_process` artifact | 3 | `config/seed/system/artifacts/default_agent_process.md` | Returned verbatim as the satellites MCP server's `instructions` field; surfaced as a `<system-reminder>` block, repeated every turn |

That is the entire startup payload. One markdown file. Project
intent, active principles, and per-story/task context are not
loaded at startup — they're fetched after Phase 6 fires. The
first post-prompt call (`project_set` for substrate prompts)
returns the orientation bundle that brings intent + principles
into the model's working context.

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
  `project_get`, `story_get`, `task_get`,
  `agent_get`, `contract_get`, `principle_list`. Tool
  descriptions registered in `internal/mcpserver/` are
  themselves instruction prose read by the model on every turn,
  rendered into the portal's MCP verb map page for operator
  inspection.
- The orientation bundle's contents — `project_set` /
  `project_get` return whatever scope=project artifact
  named `project_intent` exists for the bound project, plus
  every active scope=system + scope=project type=principle row
  (filtered by workspace at the row layer). Editing the seed
  files reshapes what every agent sees on Phase 6.3.

Anything role-specific, project-specific, or task-specific
beyond intent + principles must be fetched after the first
prompt arrives. The startup blob is one size for all readers —
orchestrator, dispatched subprocess, reviewer, ad-hoc operator
query.

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
catalogue (Phase 6.2's mapping work). For substrate prompts the
bootstrap `project_set(repo_url)` typically runs first, then
the prompt-specific verb:

| Operator prompt | Identifier extracted | Verb after bootstrap |
|---|---|---|
| `implement sty_a03449d1` | story id `sty_a03449d1` | `story_get(id)` |
| `run sty_cf8ff98b` | story id `sty_cf8ff98b` | `story_get(id)` |
| `do task_b3a91e` | task id `task_b3a91e` | `task_get(id)` |
| `what's in this repo` | (resolves repo url via `git remote get-url origin`) | `project_set(repo_url)` returns intent + principles directly — no follow-up needed |
| `list stories in this project` | project id (from bound session) | `story_list(project_id)` |
| `show me contract:develop` | contract name `develop` | `contract_get(name)` |
| `which agent reviews story_close work?` | agent name (looked up by capability) | `agent_get(name)` |
| `which principles apply here` | project id (from bound session) | The bootstrap bundle's `principles[]` covers it; `principle_list(project_id)` only if more detail is needed |

The handshake doesn't carry a verb table — Claude already has
the full catalogue from the MCP `tools/list` response, with
authoritative descriptions and parameter schemas. The handshake
just orients the agent on what kinds of identifiers it might
parse and what primitives those identifiers point at; the
catalogue answers "which verb."

## Concrete example

A trace of one operator prompt as it flows through the
satellites MCP surface. Calls to other MCP servers in the
session are out of scope and excluded.

| Step | Phase | Owner | Call / action |
|---|---|---|---|
| 1 | 6 | Operator | Typed: *"validate sty_14dfd05b"* |
| 2 | 6.1 | Claude | Parsed prompt — story id `sty_14dfd05b` extracted |
| 3 | 6.3 | Satellites MCP | `project_set(repo_url="git@github.com:bobmcallan/satellites.git")` → `{project_id, status:resolved, intent_body, principles[]}` — bootstrap done in one roundtrip, session auto-registered |
| 4 | 6.3 | Satellites MCP | `story_get(id=sty_14dfd05b)` → story + project + recent ledger + agent_process + category template |
| 5 | 6.4 | Claude | Synthesised + acted (verified ACs against on-disk code, reported result) |

Two roundtrips for a substrate-prompt — bootstrap (which
carries intent + principles) and the prompt-specific bundle.
The orientation bundle and `story_get` collapse the
stitching into the substrate so the agent doesn't have to
fetch project, intent, principles, story, ledger, and agent
process as separate calls.
