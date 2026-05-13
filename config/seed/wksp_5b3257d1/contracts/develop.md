---
name: develop
category: develop
dispatch_class: heavy
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Develop Contract

Writes the change that satisfies the story's acceptance criteria.
Develop is the file-write floor of every work-bearing story — the
only contract licensed to edit, create, or delete files.

## What it does

- Edits, creates, deletes files per the accepted plan.
- Runs the project's quality gates locally and iterates until they
  are green. The contract names no specific gate — the project's
  toolchain and skills supply the inventory.
- Stages the delivered files and creates one commit per story,
  following the project's commit conventions.

## How

Code-edit + read-only inspection + `git add` + `git commit`. The
green gate state is achieved before the commit is recorded, not
after.

## Worktree + branch shape

Develop work runs in a per-task git worktree at
`./satellites/worktree/<task_id>` on a private branch named
`client-{task_id}-from-{base_sha}`. The orchestrator (or operator)
creates the worktree before dispatch; the develop agent's commits
land on that branch. The substrate stores no copy of branch state —
git is the audit-of-record for code, the ledger is the
audit-of-record for evidence.

## Evidence required

- Branch name and the base SHA the worktree was cut from.
- Files-changed list with a one-line reason per file.
- Output of every gate the project requires (build, tests,
  formatters, linters, type checks — whatever the project supplies).
- AC-by-AC mapping with file:line citations or command-output
  excerpts.
- `git diff --stat` summary.
- Commit SHA on the work branch and the commit subject.

## Review policy

Develop dispatches its reviewer asynchronously. After commit and
close, the orchestrator's next plan step is `task_add(kind=review,
action=contract:develop, parent_task_id=<develop_task>)`. The
develop task closes immediately on commit; the kind=review task
carries the review work. Develop does not dispatch a sub-reviewer
in-process — the orchestrator-driven async model keeps dispatch in
one place and develop's permission envelope free of subagent
spawning.

## Limitations

- No `git push` — push is a separate contract.
- No history rewrites (`--amend`, force, rebase) on shared
  branches.
- No skipping pre-commit hooks (`--no-verify`).
- No new abstractions, shims, or backwards-compat layers the AC
  did not request (principle pr_no_unrequested_compat).
- No reviewer dispatch from inside the develop task — the
  orchestrator's plan step mints the kind=review successor.
