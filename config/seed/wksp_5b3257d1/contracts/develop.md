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
`./.satellites/worktree/<task_id>` on a private branch named
`client-{task_id}-from-{base_sha}`. The orchestrator (or operator)
creates the worktree before dispatch; the develop agent's commits
land on that branch. The substrate stores no copy of branch state —
git is the audit-of-record for code, the ledger is the
audit-of-record for evidence.

## Evidence required

- Branch name and the base SHA the worktree was cut from.
- Files-changed list with a one-line reason per file.
- Output of every gate the project requires (build, tests,
  formatters, linters, type checks — whatever the project supplies)
  as `kind:unit-test-run` ledger rows (see "Test evidence —
  unit vs integration" below).
- AC-by-AC mapping with file:line citations or command-output
  excerpts.
- `git diff --stat` summary.
- Commit SHA on the work branch and the commit subject.
- Template-field carrier ledger rows (see "Template field carrier"
  below) for each value this slice produces that the parent story's
  template will require at close time.

## Template field carrier

A develop task that produces a value the parent story's template
will require at close time MAY append a `kind:template-field:<name>`
ledger row, scoped to the parent story, with the value as `content`.
The `story_close` gate rolls carrier rows up last-write-wins into
`story.fields` BEFORE evaluating the category template's `done`
transition hook. The mechanism eliminates the manual
`story_update(fields={…})` step the operator used to invoke after a
develop close.

Authoring shape (one row per field, via `ledger_append`):

```
project_id: <project_id>
story_id:   <parent story_id>
type:       evidence
tags:       ["kind:template-field:<name>", "story_id:<parent>"]
content:    <value to write to story.fields.<name>>
```

Field names MUST match the parent story's template. For the
`improvement` template (covering the majority of substrate work),
slices that produce these values SHOULD carry them:

- `fix_commit` — the commit SHA the develop close created. Authored
  by the develop agent immediately after `git commit` lands.
- `regression_test_path` — `path/to/test_file.go:TestName` for the
  regression locking the new behaviour. Authored when the test
  exists.
- `before_after` — a concise behaviour-shape sketch. Authored on
  the develop close that materialises the user-visible change
  (often the last slice).

Carrier rows for field names the template does not declare are
recorded but not applied — no error, no gate. Last-write-wins is
per field name across the story's full carrier-row set (the
substrate iterates newest-first and uses the first occurrence per
name).

## Test evidence — unit vs integration

Two kinds of test-run rows live in the develop evidence packet:

- `kind:unit-test-run` — cheap per-task gates the develop contract
  always requires. Build, unit tests, linters, formatters,
  type-checks — whatever the project's toolchain supplies. Every
  develop close carries at least one `kind:unit-test-run` row.
- `kind:integration-test-run` — expensive cross-surface tests
  (testcontainers, end-to-end fixtures, pprod-shaped dogfood
  paths). Treated as a named checkpoint, not a per-task gate. The
  orchestrator authors integration evidence at meaningful
  boundaries — typically the last develop slice before commit, or
  when a substantial slice-set has landed.

Integration evidence is REQUIRED only when the parent develop task
is tagged `integration-boundary` (the orchestrator's choice when
authoring the task). Absent that tag, a develop close without
`kind:integration-test-run` is accepted. The orchestrator owns the
"when" — develop agents do not infer integration boundaries from
slice size.

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
