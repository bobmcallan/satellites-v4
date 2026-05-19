---
id: pr_role_grid
name: role_grid
scope: workspace
tags:
  - role
  - authority
  - mcp
  - configseed
  - substrate
---
# Three roles bound to verbs: orchestration, execution, review

Roles today are honour-system. The orchestrator's prompt can override
a contract; the executor inherits the orchestrator's project-scoped
MCP key; reviewers and executors share the same envelope. This
principle names the three roles, grids them against the MCP verb
surface, and pins one role to every seeded agent doc. Subsequent
stories under `epic:role-boundaries` (Story 2 enforcement onwards)
read this grid as the source of truth.

## The grid

| Verb (or family) | orchestration | execution | review |
|---|---|---|---|
| story_add / story_update | ✓ | ✗ | ✗ |
| story_update(status=done) | gated by pr_story_terminal_gate + pr_review_required_gate | ✗ | ✗ |
| task_add | ✓ | ✗ | ✗ |
| task_update(self → closed) | ✓ | ✓ | ✓ |
| task_update(other) | ✓ | ✗ | ✗ |
| ledger_append (own task tag) | ✓ | ✓ | ✓ |
| ledger_append (cross-task) | ✓ | ✗ | ✗ |
| story_get / task_walk / ledger_list (own story) | ✓ | inlined by orchestrator | ✓ |
| ledger_search / story_list / document_list (project) | ✓ | ✗ | ✓ |
| document_get / principle_list / agent_list / contract_list / skill_list | ✓ | inlined | ✓ |
| repo_search / repo_get_file / repo_get_outline | ✓ | ✗ initially | ✓ |
| kv_get / kv_set | ✓ | own-task namespace only | ✗ |

"inlined" = the orchestrator pre-renders the relevant document into
the task prompt; the executor never calls the verb itself.

## Per-agent `role:` field rule

Every seeded `type=agent` doc carries `role:` in its frontmatter,
with a value in `{orchestration, execution, review}`. The configseed
lint at `internal/configseed/role_field_test.go` enforces presence +
the allowed-values set + the seven-agent grid mapping below:

- `claude_orchestrator` → `orchestration`
- `developer_agent` → `execution`
- `releaser_agent` → `execution`
- `substrate_auditor` → `execution`
- `development_reviewer` → `review`
- `story_reviewer` → `review`
- `gemini_reviewer` → `review`

`role:` rides the existing frontmatter merge path
(`mergeFrontmatterIntoJSON`) into the agent row's `structured` JSON
— no parser change is required, no transitional shim, no
`role: legacy` value. Citing `pr_no_unrequested_compat`.

## What this forbids

- A fourth role value. The allowed set is closed at three; new
  authority shapes are a separate story that updates this grid
  before introducing a value.
- A per-task `role` override. Authority is pinned at the agent doc;
  tasks do not carry escalation knobs.
- A transitional `role: legacy` value for un-migrated agents. All
  seven seeded agents land on the grid in the same diff.

## Citations

- `pr_substrate_model` — configuration-over-code mandate: the role
  decision lives in the agent doc, not in Go branching.
- `pr_pipeline_authority` — the review gate (later stories) reads
  `role: review` to admit a task into the verdict-authoring path.
- `pr_naming_convention` — the seed frontmatter `name:` is the
  plain slug `role_grid`; the display title `pr_role_grid` lives in
  the body and citations only.
