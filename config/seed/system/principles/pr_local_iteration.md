---
id: pr_local_iteration
name: Iterate locally, deploy once
scope: system
tags:
  - delivery
  - iteration
  - deploy
---
Iterate against a local docker/testcontainers environment before pushing. Each push to main should deploy a story that is already green on a local satellites server — diagnostic round-trips through PPROD deploys waste cycles and mix diagnostic commits into main-line history.

## Why

Story_75501520 is the first-person case study: three deploys for a single substrate change because the local unit test only asserted frontmatter parsing but never exercised the seed-loader's apply-to-store path. Every diagnostic round-trip cost a full CI run + Fly deploy + propagation wait. A ten-line integration test against `tests/common/containers` would have caught both bugs locally and compressed the three commits into one.

## How to apply

- For any change that touches seed loading, DB shape, server boot paths, pipeline resolution, or MCP tool registration — write an integration test that boots a local satellites server via `tests/common/containers` and exercises the change against it.
- Run `go test ./tests/mcp ./internal/seeds ./...` locally before pushing. Green locally means the push is for "ship", not "diagnose".
- The project-scoped `test` contract (story_36641cae) formalises this as a pipeline phase: every iteration of the loop is a ledger row tagged `kind:test-run, iteration:<n>`, producing an auditable trail of what was tried before green was reached.

## When the rule loosens

Purely-documentation changes, content-only seed edits (e.g. rewording a principle), and changes that have no runtime surface in the server can skip the local-server boot path. `go test ./...` without testcontainers is still mandatory — the rule is about not iterating on PPROD, not about ceremony for ceremony's sake.
