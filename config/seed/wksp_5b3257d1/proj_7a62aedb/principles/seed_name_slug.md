---
id: seed_name_slug
name: seed_name_slug
scope: project
tags:
  - configseed
  - convention
  - naming
---
# Seed-document `name:` is the plain slug

A seed document's `name:` frontmatter field stores the **plain slug**:
no typed prefix (no `agent_`, `pr_`, `contract_`), no display title.
The verb namespace already carries the type — `agent_get(name=…)`,
`principle_get(name=…)`, `contract_get(name=…)` — so the name field
must not duplicate it.

## Rule

For every file under `config/seed/`:

- `frontmatter.name` is a plain slug matching the regex
  `^[a-z][a-z0-9_]*$`.
- `frontmatter.name` equals `basename(file)` minus `.md`. The file
  basename and the stored name are interchangeable identifiers; the
  configseed lint at `internal/configseed/name_convention_test.go`
  enforces the equality on every boot of the test suite.
- The body's `# Display Title` H1 carries the human-readable phrase.
  Display titles live in the body, never in `name:`.

The `id:` frontmatter field is documentation-only — the substrate
upserts on `(workspace_id, type, name)` and never reads `id:`. The
plain-slug rule scopes to `name:` exclusively.

## Why

`name` is the substrate's identity key. When it carries a typed
prefix, every `_get(name=…)` call has to remember whether the slug
was prefixed by the seed author — `agent_get(name="claude_orchestrator")`
vs `agent_get(name="agent_claude_orchestrator")` — and the
substrate's hierarchical lookup chain pays the cost of that
ambiguity. Plain slugs make the verb namespace self-describing.

Display titles in `name:` are worse: they encode whitespace and
capitalisation that no caller can predict from the verb. The body H1
is the right surface for the human-readable title.

## Sibling

`mcp_naming_convention` codifies the same "namespace already carries
the type" rationale at the verb surface (`<object>_<action>` shape,
no action-first verbs). The pair covers both surfaces — the verbs
the substrate exposes, and the names the seed authors stamp on the
documents those verbs return.

## Citation

Backed by `sty_94c54229`. The lint at
`internal/configseed/name_convention_test.go` is the regression test
that catches a re-introduction.
