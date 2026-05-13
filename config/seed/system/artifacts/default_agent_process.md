---
name: default_agent_process
tags: [kind:agent-process, v4]
---
# satellites · agent process

Satellites is a **customisable mapped business process for the
implementation of user stories**. It uses a configuration-over-code
substrate for multi-agent orchestration: agents, contracts,
principles, and workflows are markdown documents.
New behaviour comes from new prose and a re-seed, not from
editing code.

## Primitives

- **projects** — top-level work surface; carry intent + active principles.
- **stories** — units of deliverable work (user stories) scoped to a project.
- **tasks** — the dispatch unit; what a single agent acts on.
- **documents** — typed markdown: agents, contracts, principles, skills, workflows, help.
- **ledger** — append-only audit log; evidence and verdicts.

## Bootstrap

When a user prompt references substrate primitives (story id,
task id, contract name, agent name) or asks about the project,
make `project_set(repo_url=<git remote get-url origin>)` your
first call. It binds your session to the project, returns the
project's intent prose and active principles in one roundtrip,
and lets every subsequent project-scoped verb default to the
bound project.

For unrelated prompts (a quick question, a non-substrate task)
the bootstrap is unnecessary. The handshake orients — it does
not demand a roundtrip on every turn.

`project_get(id=<project_id>)` returns the same orientation
bundle without re-resolving the repo URL. Use it on later turns
when the intent or principles need a refresh.

## Fetching context

The default substrate surface is the `satellites-client` CLI
invoked via Bash — grouped by noun (`task get <id>`,
`ledger append --type evidence ...`, `story update-status ...`).
The binary lives at `./satellites/satellites-client` colocated
under the consumer project root. Auto-JSON when stdout is not a
tty; pipe to `jq`. Auth + server URL resolve from the loader's
config chain (flag > env > satellites/ > bin/ > XDG).

The `mcp__satellites__*` verbs in your tool list are the
equivalent shape, 1:1 with the CLI verbs. They're the fallback
when no `satellites-client` binary resolves on PATH — for
example, in MCP-only clients that don't shell out. Names and
parameters in either form are authoritative. The `satellites_init`
MCP verb returns the install payload when the binary is missing
or outdated (`install_required` / `update_available` /
`up_to_date`).

## Operating principle

Read the documents that describe your role, your project, and
your task. Act on what they say. Write evidence to the ledger.
Prose is authoritative — fetch rules, do not infer them.
