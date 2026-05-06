---
id: pr_skills_reviewers_ad_hoc
name: Skills and reviewers are ad-hoc, not baseline
scope: system
tags:
  - configseed
  - lifecycle
---
The substrate seeds **roles**, **agents**, **contracts**, **workflows**,
**story_templates**, **principles**, **artifacts**, and
**replicate_vocabulary** at boot via configseed. It does **not** seed
`type=skill` or `type=reviewer` rows. This is intentional.

## Skills

Skill documents (`type=skill`) bind to agents via `agent.skill_refs`
(story_b1108d4a). They appear lazily as a project surface grows —
either through `agent_compose` minting a project-scope skill that an
agent will reference, or through an operator calling `skill_create`
directly. Lifecycle agents (`developer_agent`, `releaser_agent`,
`story_close_agent`) ship with `skill_refs` empty by default; the
substrate runs without any skill row in the system tier.

## Reviewers

Reviewer dispatch lives on `type=agent` documents. The two reviewer
agents seeded today — `story_reviewer` and `development_reviewer` —
are `type=agent` rows whose body IS the rubric prompt. The substrate
dispatches them at contract-close time via `runReviewer`
(internal/mcpserver/close_handlers.go) based on contract name.

`type=reviewer` is a writable type (the `reviewer_create` /
`reviewer_update` MCP verbs accept it, and `contract_binding` is
required at validate time per internal/document/document.go) but no
code path *reads* an existing `type=reviewer` row. It exists for
project-scope future expansion when an operator wants a reviewer
identity that is decoupled from the agent doing the dispatching.

## Why no baseline seed

A baseline seed would:

1. invent rows nothing currently consumes, creating noise an operator
   has to maintain (the row's text drifts but no behaviour depends on
   it);
2. blur the line between "system contract" (the agent_process /
   role / contract / workflow seeds, which are load-bearing) and
   "project asset" (skills + reviewers, which the operator authors as
   project needs become concrete).

Operators add skills + reviewers when a real binding emerges. The
substrate does not pre-stage them.

## When to revisit

Add a baseline if any of the following becomes true:

- a system-tier code path begins listing `type=skill` rows and
  failing-open is unacceptable (today
  `skill_binding_migration.go:40` lists for migration only — empty
  list is a no-op);
- a system-tier code path begins reading `type=reviewer` rows
  (today none do);
- a generic skill (e.g. "summarise-evidence", "diagnose-failed-CI")
  becomes load-bearing for multiple agents and operators are each
  re-creating it project-by-project.

At that point, add `KindSkill` / `KindReviewer` to configseed and
ship the seed files. Until then, the operator's first
`skill_create` is the right place for a skill to appear.
