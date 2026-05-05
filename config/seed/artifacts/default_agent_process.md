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

## Fetching context

Role-, project-, and task-specific context is fetched on demand
via the `mcp__satellites__*` verbs. The full catalogue — names,
descriptions, parameters, return shapes — is visible to your
session in the tool list and is authoritative there. Use those
verbs to resolve identifiers from the operator's prompt (story
id, task id, contract name, agent name) into substrate state.

## Operating principle

Read the documents that describe your role, your project, and
your task. Act on what they say. Write evidence to the ledger.
Prose is authoritative — fetch rules, do not infer them.
