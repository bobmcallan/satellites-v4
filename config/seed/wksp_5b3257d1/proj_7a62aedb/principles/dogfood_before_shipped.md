---
id: pr_dogfood_before_shipped
name: dogfood_before_shipped
scope: project
tags:
  - quality
  - dogfood
  - testing
---
# Dogfood verbs against the live canonical project before declaring shipped

Tests written by an agent verify the contract that agent designed.
They use fresh fixtures populated the way the new code expects. They
do not encounter the **actual existing state** in the live system —
legacy rows, alternate registration paths, partially-migrated data,
the historical accidents that accumulate in any long-running
environment. Passing tests + green CI prove the happy path the author
reasoned about works. They do not prove the feature works.

This principle exists because of `sty_31d51494` Layer 2: the
`project_set` bootstrap verb shipped with green CI, full unit and
integration coverage, and a clear acceptance criterion ("project
intent reachable via project_set"). Tests passed because they used
fresh fixtures with `project.git_remote` populated. The live
canonical project — the satellites project itself — stored its remote
in a different table (`repos`), which the lookup didn't query. The
verb did not work for the project that ships the substrate. The bug
was visible only when called against live data, never against test
data.

## What this means in practice

When the change touches a primitive that has more than one
registration path (e.g. legacy column + new table; multiple seed
sources; a migration window), enumerate every path that currently
writes to the field and verify the lookup hits all of them — not
just the path the new code uses. Tests can't enumerate paths the
author hasn't seen.

When you ship a substrate verb whose purpose is to resolve / bind /
operate on a primitive, **call it against the canonical demo
instance on the live deployment** before claiming the story
satisfied. For satellites itself, that means against `proj_7a62aedb`.
Capture the response in the story's evidence — a ledger row with
the literal output, not a prose summary. That's the verification
bar; passing tests is necessary but not sufficient.

## Forbidden

- Treating "the test passes" as equivalent to "the feature works in
  the live environment".
- Closing a story whose verb is reachable through MCP without having
  actually called that verb against the live deployment.
- Defending insufficient test coverage by adding another assertion
  in the same blind spot. The right move is to harden the
  verification model, not to bolt on more assertions in the same
  shape that already missed the bug.

## How to apply

1. Before status → `done` on a story whose deliverable is an MCP
   verb, call the verb against the live canonical project on pprod
   (or the operator-designated dogfood instance).
2. Append the literal request + response to the story's ledger as
   `kind:dogfood-evidence`.
3. If the live call fails or returns an unexpected shape, the story
   is not done — irrespective of CI status. Surface the gap, file
   the follow-up, and reconcile before transitioning.

## Citation

Backed by `sty_14dfd05b`, the cleanup story that removed the legacy
`projects.git_remote` column whose existence had silently broken
`project_set` for the canonical demo project. Cite this principle
(`pr_dogfood_before_shipped`) on any close where the deliverable is
a verb but only test-suite evidence is in the ledger — that's the
shape that just failed us.
