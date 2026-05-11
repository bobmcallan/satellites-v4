---
id: pr_naming_convention
name: naming_convention
scope: workspace
tags:
  - configseed
  - convention
  - naming
  - mcp
  - api
---
# Namespace already carries the type — both at the verb surface and on seed names

The verb namespace already carries the object type (`agent_get`, `principle_get`, `contract_get`) and the action shape (`<object>_get` / `<object>_add` / `<object>_update`). Names and verbs that re-encode the same information force callers to remember per-row idioms instead of predicting the shape.

## Seed-document `name:` is the plain slug

For every file under `config/seed/`:

- `frontmatter.name` is a plain slug matching `^[a-z][a-z0-9_]*$`. No typed prefix (no `agent_`, `pr_`, `contract_`), no display title.
- `frontmatter.name` equals `basename(file)` minus `.md`. File basename and stored name are interchangeable identifiers; the configseed lint at `internal/configseed/name_convention_test.go` enforces equality on every boot of the test suite.
- The body's `# Display Title` H1 carries the human-readable phrase. Display titles live in the body, never in `name:`.

The `id:` frontmatter field is documentation-only — the substrate upserts on `(workspace_id, type, name)` and never reads `id:`. The plain-slug rule scopes to `name:` exclusively.

When `name` carries a typed prefix, every `_get(name=…)` call has to remember whether the slug was prefixed — `agent_get(name="claude_orchestrator")` vs `agent_get(name="agent_claude_orchestrator")` — and the hierarchical lookup chain pays the cost of that ambiguity. Display titles in `name:` are worse: they encode whitespace and capitalisation no caller can predict from the verb.

## MCP write verbs follow CRUD-shaped naming

`<object>_add`, `<object>_update`, `<object>_delete`.

`_add` (not `_create`, not `_submit`) introduces a new row of `<object>`. `_update` for partial mutation. `_delete` for removal. Read shapes: `<object>_get`, `<object>_list`, `<object>_search`.

The verb's first token is the object; the second token is the action. Action-first verbs (`create_X`, `add_X`) are NOT allowed.

An agent reading the tool catalogue can predict the verb shape from the object name without memorising per-verb idioms. New verbs follow the convention from day one; existing non-conforming verbs are renamed in their own stories.

## Citation

Backed by the configseed lint at `internal/configseed/name_convention_test.go` (regression test that catches a re-introduction of typed prefixes).
