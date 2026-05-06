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

`project_context()` returns the same orientation bundle without
re-resolving the repo URL. Use it on later turns when the
intent or principles need a refresh.

## Fetching context

Beyond the bootstrap, role-, story-, and task-specific context
is fetched on demand via the `mcp__satellites__*` verbs. The
full catalogue — names, descriptions, parameters, return shapes
— is visible to your session in the tool list and is
authoritative there. Resolve identifiers from the operator's
prompt against it: a story id maps to `story_get` /
`story_context`, a task id maps to `task_get`, an agent or
contract name maps to `agent_get` / `contract_get`.

## Operating principle

Read the documents that describe your role, your project, and
your task. Act on what they say. Write evidence to the ledger.
Prose is authoritative — fetch rules, do not infer them.
