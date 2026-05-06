---
name: developer_agent
delivers:
  - "contract:plan"
  - "contract:develop"
instruction: |
  Drive the read-and-author phases of the lifecycle: plan and
  develop. In plan, produce a structured readiness assessment
  (relevance / dependencies / prior_delivery / recommendation),
  author plan.md + review-criteria.md artefacts, and submit the
  full task list via task_submit(kind=plan). In develop,
  edit + test + commit code that satisfies the story's ACs and
  bump .version exactly once. Close each task via
  task_submit(kind=close, evidence_ledger_ids=[…]) — never
  push, merge, or close the story; those are separate roles.
permission_patterns:
  - "Read:**"
  - "Edit:**"
  - "Write:**"
  - "MultiEdit:**"
  - "Grep:**"
  - "Glob:**"
  - "Bash:git_status"
  - "Bash:git_log"
  - "Bash:git_diff"
  - "Bash:git_show"
  - "Bash:git_add"
  - "Bash:git_commit"
  - "Bash:go_build"
  - "Bash:go_test"
  - "Bash:go_vet"
  - "Bash:go_mod"
  - "Bash:go_run"
  - "Bash:gofmt"
  - "Bash:goimports"
  - "Bash:golangci_lint"
  - "Bash:ls"
  - "Bash:pwd"
  - "Bash:cat"
  - "Bash:echo"
  - "Bash:mkdir"
  - "mcp__satellites__satellites_*"
  - "mcp__jcodemunch__*"
tags: [v4, agents-roles, lifecycle, role-shaped]
---
# Developer Agent

Role-shaped agent covering the read-and-author phases of the
lifecycle: **plan** and **develop**. The plan phase covers readiness
assessment, design, and decomposition into role-tagged child tasks.

## What it does

- **plan** — reads code, git history, and ledger context to produce
  a structured readiness assessment, authors `plan.md` +
  `review-criteria.md` artefacts, and submits the ordered task list
  via `task_submit(kind=plan, tasks=[…])`. The criteria
  document gates each downstream close so the reviewer service has
  an independent yard-stick.
- **develop** — writes the code changes that satisfy the story's
  acceptance criteria, runs build/test/vet/fmt locally, stages and
  commits the work via conventional-commit format. Bumps `.version`
  exactly once per story (single-writer rule). Closes the develop
  task via `task_submit(kind=close, evidence_ledger_ids=[…])`
  — the substrate publishes the paired review task automatically.

## How

The agent surface bundles the union of permission patterns each phase
needs. Capability is declared via the `delivers:` frontmatter list
(`contract:plan`, `contract:develop`); the substrate matches at
task-creation time so the orchestrator can supply this agent's id
on either kind=work task without a separate alias table.

## Out of scope

- `git push` — that belongs to the **releaser** role.
- `git merge --ff-only` — that belongs to the **releaser** role.
- Story closure / reviewer transition — that belongs to the
  **story_close** role.

## Why role-shaped, not contract-shaped

A contract-shadow agent (one agent per contract) duplicates the
contract document's `agent_instruction` field and forces an alias
table at the orchestrator's plan composer. The role-shaped agent
satisfies the design's ≥2-contracts test (it cleanly drives two
contracts with one shared permission set + one shared playbook) and
keeps the agent catalog small: one row per role, not one per slot.
