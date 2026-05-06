---
name: develop
category: develop
delivers_by: developer_agent
reviewed_by: development_reviewer
evidence_required: |
  Ledger rows tagged task_id:<develop_task>, kind:evidence:
  1. Files changed list with one-line why per file.
  2. Build / test / vet / fmt / lint outputs.
  3. AC-by-AC mapping with file:line or command-output citations.
  4. git diff --stat summary.
  5. Commit SHA (no AI attribution).
tags: [v4, lifecycle, system]
---
# Develop Contract

Writes the code that satisfies the story's acceptance criteria.
Develop is the only contract with file-write authority and the
only contract that bumps `.version`.

## What it does

- Edits, creates, deletes files per `plan.md`.
- Runs `go build`, `go test`, `go vet`, `gofmt`, `goimports`,
  `golangci-lint` locally; iterates until green.
- Bumps `.version` patch segment exactly once.
- Stages the delivered files and creates one conventional-commit
  per story (no AI attribution).
- Records evidence: files changed, command outputs, AC-by-AC
  mapping, commit SHA.

## How

Full code-edit + go-toolchain + read-only git inspection +
`git add` + `git commit`. Develop iterates locally; the green
state happens before the commit, not after.

## Limitations

- No `git push` — push is a separate contract.
- No history rewrites (`--amend`, force, rebase).
- No skipping pre-commit hooks (`--no-verify`).
- No new abstractions, shims, or backwards-compat layers the AC did
  not request (principle pr_no_unrequested_compat).
- Cannot bump `.version` minor or major segment unilaterally.
