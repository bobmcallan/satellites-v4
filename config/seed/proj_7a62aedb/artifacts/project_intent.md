---
name: project_intent
tags: [kind:project-intent]
---
# Satellites — project intent

Satellites is a **customisable mapped business process for the
implementation of user stories**, built as a configuration-over-code
substrate for multi-agent orchestration. Behaviour is specified as
markdown documents — agents, contracts, principles, workflows,
artifacts — not as branches in code. New behaviour comes from new
prose and a re-seed, not from a code change.

## Non-negotiables

- **Prose is the configuration.** Substrate behaviour is shaped by
  document bodies, not Go logic. If a rule needs to land, it lands as
  markdown that the seed loader picks up and the runtime reads.
- **Story is the unit of work.** Every change ties to a story; there is
  no work outside a story.
- **Configuration over code.** Adding a verb, a contract, a principle
  is a documentation task with code support, not a feature task with
  documentation as an afterthought.
- **Seed prose must be self-sufficient.** A reader given only a
  document's body should be able to act on it. Repo-internal
  references (`docs/...`, `config/...`, `internal/...`) leak the file
  layout into the prose surface and break dispatched-agent contexts
  that don't carry the repo.

## Status

Placeholder body. The full project-intent prose lands via
`sty_31d51494` layer 2 — this file exists today to exercise the
project-tier seed loader's end-to-end path (`sty_8868eaf4`).
