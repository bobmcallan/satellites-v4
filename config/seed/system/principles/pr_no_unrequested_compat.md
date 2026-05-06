---
id: pr_no_unrequested_compat
name: No unrequested abstractions or backwards-compat layers
scope: system
tags:
  - scope
  - architecture
  - review
---
Aliases, shims, backwards-compat layers, deprecated wrappers, type aliases, feature flags, and migration adapters require an **explicit user or AC mention**. Default to `delete + migrate`, not `alias + defer`.

This principle exists because the previous epic's anchor (story_e008641a in epic:workflow-model) invented a full `Pipeline*` → `Workflow*` alias layer (type aliases, deprecated MCP tool registrations, legacy tag fallback) that was NOT requested in the story's acceptance criteria. The follow-up sweep (story_b2272163) AC 12 then had to schedule the alias removal — and a release later half the codebase still calls the deprecated names because once a compat layer ships, removing it becomes its own story. Extra surfaces accrete.

Concretely:

- **Type aliases** (e.g. `type OldName = NewName`) — only with explicit AC.
- **MCP tool aliases** (registering an old tool name that delegates to a new handler) — only with explicit AC.
- **Deprecated wrappers** (functions that call the new function with a deprecation comment) — only with explicit AC.
- **Feature flags** that gate behaviour the AC said to ship — only with explicit AC.
- **Shims** between layers that mask a rename — only with explicit AC.
- **Migration adapters** that translate legacy data shapes — only with explicit AC; if the AC requires a one-pass migration, do that and remove the adapter in the same change.

When in doubt, prefer the smaller change: rename + migrate callers in the same diff. If a migration is genuinely large, propose splitting the story; do NOT introduce an alias to defer half the work.

The reviewer cites this principle (`pr_no_unrequested_compat`) in the verdict rationale when the agent's evidence describes any compat surface the AC did not request, paired with the relevant rule (`rule_develop_no_unrequested_compat` or `rule_plan_no_unrequested_compat`).
