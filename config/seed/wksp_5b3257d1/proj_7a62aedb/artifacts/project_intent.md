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
