---
name: gemini_reviewer
role: review
tier: flash
tool_ceiling:
  - "task_get"
  - "task_list"
  - "ledger_get"
  - "ledger_list"
  - "ledger_append"
  - "document_get"
  - "document_list"
  - "session_whoami"
provider_chain:
  - provider: gemini
    model: gemini-2.5-flash
    tier: flash
tags: [v4, agents-roles, reviewer]
---
# Gemini Reviewer Agent

Configuration shape for a Gemini-backed reviewer agent.
provider_chain=gemini/2.5-flash. `tool_ceiling` covers the verbs
the agent exercises: `task_*` reads to fetch the parent work task,
`ledger_*` reads to dereference evidence rows, `ledger_append` to
write the verdict / review-question / kind:evidence rows, and
`document_*` reads to resolve the contract doc + reviewer rubric.

When dispatched on a `kind=review` task the agent claims the task,
runs the rubric, writes a `kind:verdict` ledger row tagged
`task_id:<id>`, and closes the task with success/failure. On
rejection the reviewer's contract prose mints a successor
`kind=work` task via `task_add(prior_task_id=…)`; pair-spawn is
not the substrate's behaviour.
