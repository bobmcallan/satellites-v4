---
title: Agents
slug: agents
order: 20
tags: [help, agents]
---
# Agents

An **agent** is a configured executor — a bundle of permission
patterns + capability declarations that the substrate matches at
task-creation time.

## System agents

Seeded from `config/seed/agents/*.md`, system agents power the
lifecycle:

- `developer_agent` — delivers `contract:plan` and
  `contract:develop`. Reads code + git history; writes code +
  commits; submits the plan task list via `task_submit`.
- `releaser_agent` — delivers `contract:push` and
  `contract:merge_to_main`.
- `story_reviewer` — reviews `contract:plan`, `contract:push`,
  `contract:merge_to_main`. Read-only;
  the autonomous reviewer service uses this body as its rubric.
- `development_reviewer` — reviews `contract:develop`. Read-only.
- `agent_gemini_reviewer` — provider-chain config for the
  reviewer service (gemini-2.5-flash).
- `agent_claude_orchestrator` — anchors the orchestrator role
  inherited by interactive Claude sessions.

## How agents are configured

Each agent's markdown frontmatter declares:

- `delivers:` — list of canonical `contract:<name>` actions the
  agent can execute as the kind=work side.
- `reviews:` — list of canonical `contract:<name>` actions the
  agent can grade as the kind=review side.
- `permission_patterns:` — tool-call patterns the enforce hook
  admits when the agent is bound to a task.
- `instruction:` — short imperative summary the substrate may
  surface in admin views.

The body is the human description (what it does, how, what it
explicitly cannot do). For reviewer agents, the body IS the rubric
the reviewer service evaluates closes against.

## Capability lookup

The substrate matches at task-creation time:

- `task_submit(kind=plan)` validates that any supplied
  `agent_id` carries `contract:<action>` in its `delivers:` (for
  kind=work) or `reviews:` (for kind=review). Mismatches reject
  with `agent_cannot_deliver` / `agent_cannot_review`.
- The autonomous reviewer service resolves the rubric for a
  kind=review task by scanning system-scope agents and picking the
  first one whose `reviews:` list covers the task's action.

## Limitations

- Agents do not choose their next task. Orchestration lives in the
  Claude session (interactive) via `task_walk` + `task_submit`.
- Re-seeding (`/admin/system-config`) updates the document body and
  structured payload but does not interrupt running tasks; in-flight
  tasks keep the agent stamp they were created with.
