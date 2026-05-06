---
id: pr_contract_separation
name: Lifecycle and project contract separation
scope: system
tags:
  - architecture
  - contracts
---
Satellites runs a two-tier scope model (story_b54df3f6): **system** for the review-only lifecycle shell, **project** for every project's own work contracts. There is no "global" tier — each project owns its development process, codified via `satellites_contract_create` / `satellites_skill_create` against its own project ID.

System-scope lifecycle contracts (plan, story_close) are review-only. They must not embed user/project operations like git, merge, push, commit, CI watch, or deploy. Their skills are also scope=system; both are managed through seed files, not MCP mutations.

Project-scope work contracts (develop, test, integration, commit, push, merge_to_main) perform the actual work. They must not embed story-review logic. They live in each project's DB rows, managed through MCP CRUD, never seeded at platform boot.

The two tiers never mix. A system-scope contract that performs a git operation is a violation. A project-scope contract that embeds story-review logic is a violation. If a shared operation belongs to both, it lives in a project-scope contract that lifecycle contracts observe via evidence.
