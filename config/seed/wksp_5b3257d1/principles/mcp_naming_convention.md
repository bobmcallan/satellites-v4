---
id: pr_mcp_naming_convention
name: mcp_naming_convention
scope: workspace
tags:
  - mcp
  - api
  - convention
---
MCP write verbs follow CRUD-shaped naming: `<object>_add`, `<object>_update`, `<object>_delete`.

`_add` (not `_create`, not `_submit`) for the verb that introduces a new row of `<object>`. `_update` for partial mutation. `_delete` for removal. Read shapes are `<object>_get`, `<object>_list`, `<object>_search`.

The verb's first token is the object; the second token is the action. Action-first verbs (`create_X`, `add_X`) are not allowed.

The convention exists so an agent reading the tool catalogue can predict the verb shape from the object name without memorising per-verb idioms. New verbs follow the convention from day one; existing non-conforming verbs are renamed in their own stories.
