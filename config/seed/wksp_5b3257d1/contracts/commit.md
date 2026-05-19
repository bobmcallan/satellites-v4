---
name: commit
category: commit
validation_mode: llm
required_role: role_orchestrator
tags: [v4, lifecycle, workspace]
---
# Commit Contract

Publishes the developer's already-committed work to its upstream
remote under the work branch. This is the **substrate-level
publish action**, not a release — release is the
`merge_to_main` contract's job. The commit contract exists to
record that the developer's commit(s) have left the local
environment and to surface the remote's response.

## What it does

- Fetches the upstream for pre-push sanity.
- Pushes the current work branch to its tracked upstream as a
  non-force, fast-forward update.

## How

Read-only inspection + `git fetch` + `git push`. No edits, no
amends, no new commits — the developer is the single writer for
source files and version metadata.

## Evidence required

- The commit SHA and subject being published.
- The verbatim `git push` output (or the `X..Y branch -> branch`
  line confirming a non-force update).
- Confirmation that no force, branch deletion, or unrelated tag
  push occurred.
- Per-binary attestation: for every commit in the push range,
  the diff against the parent shows a `.version` bump in the
  section matching each touched binary (see `## Version bump
  policy` below). Capture `git diff HEAD~N..HEAD -- .version`
  output in the evidence row.

## Version bump policy

Every commit on the branch in the push range MUST bump
`[satellites-server]`, `[satellites-client]`, or
`[satellites-agent]` in `.version` for whichever binary the commit
touches. The reviewer rejects evidence whose diff against
`HEAD~N..HEAD` shows no `.version` bump for a touched binary.

Touched-binary resolution (changed-files heuristic):

- `cmd/satellites-server/**` → `[satellites-server]`
- `cmd/satellites-client/**` → `[satellites-client]`
- `cmd/satellites-agent/**` → `[satellites-agent]`
- `internal/**`, `config/**`, `docs/**`, root files: default to
  the binary indicated by the commit title's conventional-commit
  scope (`feat(satellites-client): …`). When the title is
  scope-agnostic, bump every binary whose `cmd/<binary>/**` tree
  consumes the touched code path (when in doubt, bump all three).

Multi-binary commits bump every touched binary in the same diff.

## Review policy

Commit is execution-shape. No reviewer is dispatched. The remote's
accept/reject (non-fast-forward, hook failure, refusal) is the
only judgment that runs.

## Limitations

- No force push under any circumstance.
- No branch deletion.
- No tag pushes outside the explicit story scope.
- No file edits, no amends, no rebases — commit performs no work
  beyond publishing the existing commits.
- No release semantics — release is the `merge_to_main`
  contract's atomic operation.
- On rejection (non-fast-forward, hook failure, remote refusal),
  surface the error and stop. Recovery belongs in a fresh story,
  not a retry-with-force.
- No-bump push is a reject: if any commit in the push range
  touches a binary's code paths (per the heuristic above) and the
  same commit's diff against its parent does not bump that
  binary's `.version` section, the reviewer rejects the close
  evidence and the operator amends or follows up with a bump
  commit before re-attempting.
