---
name: develop
category: develop
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

## Evidence required

- Files-changed list with a one-line reason per file.
- Output of every gate the project requires (build, tests,
  formatters, linters, type checks — whatever the project supplies).
- AC-by-AC mapping with file:line citations or command-output
  excerpts.
- `git diff --stat` summary.
- Commit SHA on the work branch.

## Limitations

- No `git push` — push is a separate contract.
- No history rewrites (`--amend`, force, rebase) on shared
  branches.
- No skipping pre-commit hooks (`--no-verify`).
- No new abstractions, shims, or backwards-compat layers the AC
  did not request (principle pr_no_unrequested_compat).
