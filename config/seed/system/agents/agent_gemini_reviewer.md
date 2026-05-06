---
name: agent_gemini_reviewer
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

The autonomous reviewer service's delivery-agent configuration.
provider_chain=gemini/2.5-flash. `tool_ceiling` covers the verbs
the service exercises: `task_*` reads to fetch the parent work
task, `ledger_*` reads to dereference evidence rows, `ledger_append`
to write the verdict / review-question / kind:evidence rows, and
`document_*` reads to resolve the contract doc + reviewer rubric.

The service runs in-process alongside the satellites server when
the system-tier KV row `reviewer.service.mode` resolves to
`embedded` (the default). It subscribes to the task store's
listener bus, filters for `kind:review` tasks at claimable status,
claims via `task.ClaimByID`, runs the rubric, writes a
`kind:verdict` ledger row tagged `task_id:<id>`, closes the task
with success/failure, and on rejection spawns a successor
`kind=work` + paired planned-`kind=review` task pair with
`prior_task_id` set on the work task.

There is no MCP verb the reviewer service calls to commit verdicts —
it writes the ledger row + closes the task directly via the store
APIs. Operators flip the mode via `kv_set` at scope=system; the
next boot picks it up.
